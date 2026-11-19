package human

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

func TestWantsFillsTheGateLabelElseTheOperations(t *testing.T) {
	in := json.RawMessage(`{"host":"github.com","path":"acme/backend","protocol":"https"}`)
	for _, tc := range []struct{ label, op, want string }{
		{"Sign in to git for {path}", "push to {host}", "sign in to git for acme/backend"},
		{"", "sign in to git for {path}", "sign in to git for acme/backend"},
		// A field the label names is absent: the operation's sentence.
		{"Deploy {service}", "sign in to git on {host}", "sign in to git on github.com"},
		{"Deploy {service}", "", "call github.git.credential"},
		{"", "", "call github.git.credential"},
	} {
		if got := wants(tc.label, tc.op, "github.git.credential", in); got != tc.want {
			t.Errorf("wants(%q, %q) = %q, want %q", tc.label, tc.op, got, tc.want)
		}
	}
	// A value can't break the line it is shown on.
	if got := wants("deploy {s}", "", "x.y", json.RawMessage(`{"s":"a\nb"}`)); got != "deploy a b" {
		t.Errorf("control characters: %q", got)
	}
}

func TestInputWordingNamesTopLevelValues(t *testing.T) {
	fields, whole := inputWording(json.RawMessage(`{"host":"github.com","pull_number":12,"draft":false,"note":null}`), nil)
	want := []extension.WordingField{{Name: "Host", Value: "github.com"}, {Name: "Pull number", Value: "12"}, {Name: "Draft", Value: "no"}}
	if !slices.Equal(fields, want) || whole != "" {
		t.Fatalf("fields: %+v %q", fields, whole)
	}
	// The gate's fields pick and name them.
	fields, _ = inputWording(json.RawMessage(`{"host":"github.com","path":"acme/backend","protocol":"https"}`), map[string]string{"path": "Repository", "host": ""})
	if want := []extension.WordingField{{Name: "Host", Value: "github.com"}, {Name: "Repository", Value: "acme/backend"}}; !slices.Equal(fields, want) {
		t.Fatalf("named fields: %+v", fields)
	}
	// A nested value is shown with the whole input.
	fields, whole = inputWording(json.RawMessage(`{"env":"prod","targets":["a","b"]}`), nil)
	if len(fields) != 1 || !strings.Contains(whole, `"targets": [`) {
		t.Fatalf("nested: %+v %q", fields, whole)
	}
	// With fields listed, the input shown holds only those, even when one
	// of them is nested.
	fields, whole = inputWording(json.RawMessage(`{"env":"prod","targets":["a"],"token":"s3cret"}`), map[string]string{"env": "", "targets": "Targets"})
	if len(fields) != 1 || !strings.Contains(whole, `"targets": [`) || strings.Contains(whole, "s3cret") {
		t.Fatalf("listed and nested: %+v %q", fields, whole)
	}
	if fields, whole := inputWording(nil, nil); fields != nil || whole != "" {
		t.Fatalf("no input: %+v %q", fields, whole)
	}
}

func TestWordingOfAnApprovalAndAQuestion(t *testing.T) {
	now := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	approval := Ask{Gate: "deploy.run", Wants: "deploy to prod", Fields: []extension.WordingField{{Name: "Env", Value: "prod"}}, Confirm: OptionApprove, State: StateOpen}
	w := wording(approval, now)
	if w.Wants != "deploy to prod" || w.Question != "" || len(w.Fields) != 1 {
		t.Fatalf("approval: %+v", w)
	}
	approve, refuse := w.ChoiceWording(OptionApprove), w.ChoiceWording(OptionRefuse)
	if approve.Label != "Approve" || approve.Tone != extension.ToneAffirm || !strings.Contains(approve.Confirm, "deploy to prod") ||
		refuse.Label != "Refuse" || refuse.Tone != extension.ToneDanger || refuse.Confirm != "" {
		t.Fatalf("choices: %+v %+v", approve, refuse)
	}
	question := wording(Ask{Question: "Which region?", Context: "We have two.", State: StateOpen}, now)
	if question.Wants != "" || question.Question != "Which region?" || question.Note != "We have two." || question.ChoiceWording("eu").Label != "eu" {
		t.Fatalf("question: %+v", question)
	}
	// Escalated, it says so, without naming who it now goes to.
	approval.EscalateAt, approval.EscalateTo, approval.Deadline = now.Add(-time.Minute), []string{"operator:dana"}, now.Add(time.Hour)
	if w := wording(approval, now); !strings.Contains(w.Note, "more people are asked") || strings.Contains(w.Note, "dana") {
		t.Fatalf("escalated: %q", w.Note)
	}
}

func TestParseGateWording(t *testing.T) {
	c, err := ParseConfig(json.RawMessage(`{"gates":[{"operations":["github.git.credential"],"label":"Sign in to git for {path}","fields":{"path":"Repository"},"confirm":"approve"}]}`))
	if err != nil || c.Gates[0].Label != "Sign in to git for {path}" || c.Gates[0].Fields["path"] != "Repository" || c.Gates[0].Confirm != OptionApprove {
		t.Fatalf("parsed: %+v %v", c, err)
	}
	for _, bad := range []string{
		`{"gates":[{"operations":["x.y"],"confirm":"maybe"}]}`,
		`{"gates":[{"operations":["x.y"],"label":"two\nlines"}]}`,
		`{"gates":[{"kinds":["github.issue"],"fields":{"a":"A"}}]}`,
		`{"gates":[{"operations":["x.y"],"fields":{"":"A"}}]}`,
	} {
		if _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
