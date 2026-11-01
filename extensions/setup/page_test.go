package setup

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
)

func TestThePageValidates(t *testing.T) {
	for _, p := range (&Extension{}).Pages() {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

// The conversation is the session's Events in acceptance order: what each
// side said, the question with the answer given, and a note for each probe
// and change set Event. Other Events are not part of it.
func TestConversation(t *testing.T) {
	ev := func(seq uint64, typ, schema string, v any) domain.AcceptedEvent {
		return domain.AcceptedEvent{Sequence: seq, Event: domain.Event{Type: typ, Data: payload(schema, v)}}
	}
	msgs := Conversation([]domain.AcceptedEvent{
		ev(5, TypeAnswer, SchemaAnswer, Answer{Question: "q1", Answer: "dotfiles"}),
		ev(1, TypeMessage, SchemaMessage, Message{Role: RoleAgent, Text: "Claude Code is **signed in**."}),
		ev(2, TypeMessage, SchemaMessage, Message{Role: RoleOperator, Text: "hello"}),
		ev(3, TypeProbe, SchemaProbe, ProbeEvent{Probe: "github.accounts", Command: "gh auth status", Why: "which logins you have"}),
		ev(4, TypeQuestion, SchemaQuestion, Question{ID: "q1", Text: "Which repository?", Context: "Both are checked out.", Choices: []string{"open-conduit", "dotfiles"}}),
		ev(6, TypeProposal, SchemaProposal, Proposal{Change: "c1", Op: OpReplace, Pointer: "/scopes/setup/name", Diagnostics: []inproc.ConfigDiagnostic{{Path: "/x"}}}),
		ev(7, TypeDecision, SchemaDecision, Decision{Changes: []string{"c4"}, Decision: StateRejected, Reason: "not yet"}),
		ev(8, TypeDecision, SchemaDecision, Decision{Changes: []string{"c3"}, Decision: StateWithdrawn}),
		ev(9, TypeChecks, SchemaChecks, Checks{Source: RoleOperator, Valid: true, Approved: 2}),
		ev(10, TypeApplied, SchemaApplied, Applied{Generation: "2", Changes: []string{"c1", "c2"}}),
		ev(11, TypeStart, SchemaStart, Start{Model: "sonnet"}),
		ev(12, TypeMessage, SchemaMessage, Message{Role: RoleAgent}),
	})
	want := []struct{ from, label, text string }{
		{extension.MessageAgent, "Assistant", "Claude Code is **signed in**."},
		{extension.MessageOperator, "", "hello"},
		{extension.MessageNote, "Probe", "The assistant ran gh auth status: which logins you have"},
		{extension.MessageAgent, "Assistant asks", "Which repository?\n\nBoth are checked out."},
		{extension.MessageOperator, "", "dotfiles"},
		{extension.MessageNote, "Change set", "Proposed c1: replace /scopes/setup/name (1 problem)"},
		{extension.MessageNote, "Change set", "You rejected c4: not yet"},
		{extension.MessageNote, "Change set", "The assistant withdrew c3"},
		{extension.MessageNote, "Change set", "You validated the approved changes: 2 changes, valid"},
		{extension.MessageNote, "Change set", "Applied c1, c2 as generation 2"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages: %+v", msgs)
	}
	for i, w := range want {
		if m := msgs[i]; m.From != w.from || m.Label != w.label || m.Text != w.text {
			t.Errorf("message %d: %+v, want %+v", i, m, w)
		}
	}
	if q := msgs[3]; strings.Join(q.Choices, ",") != "open-conduit,dotfiles" || q.Chosen != "dotfiles" {
		t.Errorf("question: %+v", q)
	}
	p := extension.Panel{ID: PanelConversation, Title: "Conversation", Sections: []extension.PanelSection{{Messages: msgs}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	// A long session keeps its latest messages, and a long one is clipped
	// to what a panel holds.
	var many []domain.AcceptedEvent
	for i := range extension.MaxPanelMessages + 5 {
		many = append(many, ev(uint64(i+1), TypeMessage, SchemaMessage, Message{Role: RoleAgent, Text: strings.Repeat("x", extension.MaxPanelText+i)}))
	}
	if msgs := Conversation(many); len(msgs) != extension.MaxPanelMessages || len(msgs[0].Text) > extension.MaxPanelText || !strings.Contains(msgs[0].Text, "(45 more bytes)") {
		t.Fatalf("%d messages, the first %d bytes", len(msgs), len(msgs[0].Text))
	}
}

// The review lists every change, and offers decisions only on an open
// session: approve and reject while proposed, reject while approved,
// Approve all over more than one open change, and validate once one is
// approved.
func TestReview(t *testing.T) {
	cs := changeSet{BaseGeneration: "1", Changes: []change{
		{ID: "c1", Op: OpReplace, Pointer: "/scopes/s/name", Before: []byte(`"S"`), Value: []byte(`"T"`), Why: "clearer"},
		{ID: "c2", Op: OpAdd, Pointer: "/settings/x", Value: []byte(`1`)},
		{ID: "c3", Op: OpRemove, Pointer: "/scopes/old", Before: []byte(`{}`), Diagnostics: []inproc.ConfigDiagnostic{{Path: "/scopes/old", Code: "in_use", Message: "a route uses it"}}},
		{ID: "c4", Op: OpAdd, Pointer: "/settings/y", Value: []byte(`2`)},
	}}
	st := map[string]string{"c1": StateProposed, "c2": StateApproved, "c3": StateProposed, "c4": StateRejected}
	r := review(cs, st, true)
	if r.BaseGeneration != "1" || len(r.Changes) != 4 || !strings.HasPrefix(r.ApproveAll, approveAllPrefix) || r.Validate != ActionValidate {
		t.Fatalf("review: %+v", r)
	}
	c1, c2, c3, c4 := r.Changes[0], r.Changes[1], r.Changes[2], r.Changes[3]
	if c1.Approve != "approve_c1" || c1.Reject != "reject_c1" || string(c1.Before) != `"S"` || string(c1.After) != `"T"` || c1.Why != "clearer" {
		t.Errorf("c1: %+v", c1)
	}
	if c2.Approve != "" || c2.Reject != "reject_c2" || c2.State != StateApproved {
		t.Errorf("c2: %+v", c2)
	}
	if c3.After != nil || len(c3.Problems) != 1 || c3.Problems[0] != "/scopes/old: in_use: a route uses it" {
		t.Errorf("c3: %+v", c3)
	}
	if c4.Approve != "" || c4.Reject != "" {
		t.Errorf("c4: %+v", c4)
	}
	if ended := review(cs, st, false); ended.ApproveAll != "" || ended.Validate != "" || ended.Changes[0].Approve != "" || ended.Changes[1].Reject != "" {
		t.Errorf("an ended session offers decisions: %+v", ended)
	}
}
