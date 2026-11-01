package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// The Setup page (issue #242) is core's generic extension page: setup only
// declares it, and draws a session as panels. The conversation panel is
// the session's Events as messages; the session panel is what the operator
// chose at the start. Core reviews the change set from ConfigCandidate.

// PageID is the Setup page's ID, and its path under /pages/.
const PageID = "setup"

// Panels on a session's Work.
const (
	PanelConversation = "conversation"
	PanelSession      = "session"
)

// Pages declares the Setup page.
func (*Extension) Pages() []extension.Page {
	return []extension.Page{{
		ID:       PageID,
		Title:    "Setup",
		Summary:  "A Claude Code session running Open Conduit's setup playbook looks at this machine, explains what it finds and asks what you want. It builds on what's there and never removes anything you didn't ask it to; nothing changes until you apply the changes you approve.",
		Entry:    Type,
		WorkKind: WorkKind,
		Start:    ActionStart,
		Compose:  []string{ActionAnswer, ActionMessage},
		Route:    Type,
	}}
}

// Panels shows a session: the conversation, and what the operator turned
// on for it.
func (a Actions) Panels(ctx context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	if t.Work == nil || t.Work.Kind != WorkKind {
		return nil, nil
	}
	w := *t.Work
	var events []domain.AcceptedEvent
	if err := a.transact(ctx, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, e := range all {
			if e.Event.WorkID == w.ID || e.RoutedWorkID == w.ID {
				events = append(events, e)
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	var s Start
	if w.Context.Schema == SchemaContext {
		_ = json.Unmarshal(w.Context.Data, &s)
	}
	labels := grantLabels(s.Grants, Grants(t.Scope))
	lead := "It has no shell: it only talks and asks here, and nothing it says changes your configuration."
	if len(labels) > 0 {
		lead = "You gave it more than talking for this session: " + strings.Join(labels, "; ") + "."
	}
	convo := extension.Panel{ID: PanelConversation, Title: "Conversation", Sections: []extension.PanelSection{{Text: lead, Messages: Conversation(events)}}}
	access := "Talking only"
	if len(labels) > 0 {
		access = strings.Join(labels, "; ")
	}
	session := extension.Panel{ID: PanelSession, Title: "Model and access", Sections: []extension.PanelSection{{Fields: []extension.PanelField{{Name: "Access", Value: cut(access)}}}}}
	if s.Model != "" {
		session.Sections[0].Fields = append([]extension.PanelField{{Name: "Model", Value: cut(s.Model)}}, session.Sections[0].Fields...)
	}
	return []extension.Panel{convo, session}, nil
}

// grantLabels are the grants turned on, by label where the binding still
// offers them.
func grantLabels(names []string, offered []domain.BindingGrant) []string {
	var out []string
	for _, name := range names {
		label := name
		if i := slices.IndexFunc(offered, func(g domain.BindingGrant) bool { return g.Name == name }); i >= 0 && offered[i].Label != "" {
			label = offered[i].Label
		}
		out = append(out, label)
	}
	return out
}

// Conversation is a session's Events as messages, in the order they were
// accepted: what each side said, the agent's questions with the answers
// given, and a note for each probe and change set Event. Only the latest
// extension.MaxPanelMessages are kept.
func Conversation(events []domain.AcceptedEvent) []extension.PanelMessage {
	slices.SortStableFunc(events, func(a, b domain.AcceptedEvent) int {
		switch {
		case a.Sequence < b.Sequence:
			return -1
		case a.Sequence > b.Sequence:
			return 1
		}
		return 0
	})
	var out []extension.PanelMessage
	asked := map[string]int{}
	for _, ae := range events {
		e := ae.Event
		m := extension.PanelMessage{At: e.OccurredAt}
		switch e.Type {
		case TypeMessage:
			var x Message
			if json.Unmarshal(e.Data.Data, &x) != nil || strings.TrimSpace(x.Text) == "" {
				continue
			}
			m.From, m.Text = extension.MessageAgent, x.Text
			m.Label = "Assistant"
			if x.Role == RoleOperator {
				m.From, m.Label = extension.MessageOperator, ""
			}
		case TypeQuestion:
			var q Question
			if json.Unmarshal(e.Data.Data, &q) != nil || strings.TrimSpace(q.Text) == "" {
				continue
			}
			m.From, m.Label, m.Text, m.Choices = extension.MessageAgent, "Assistant asks", q.Text, q.Choices
			if q.Context != "" {
				m.Text += "\n\n" + q.Context
			}
			asked[q.ID] = len(out)
		case TypeAnswer:
			var x Answer
			if json.Unmarshal(e.Data.Data, &x) != nil || strings.TrimSpace(x.Answer) == "" {
				continue
			}
			if i, ok := asked[x.Question]; ok {
				out[i].Chosen = extension.Clip(x.Answer, extension.MaxPanelTitle)
			}
			m.From, m.Text = extension.MessageOperator, x.Answer
		default:
			line, ok := note(e)
			if !ok {
				continue
			}
			m.From, m.Label, m.Text = extension.MessageNote, "Change set", line
			if e.Type == TypeProbe {
				m.Label = "Probe"
			}
		}
		m.Text = clipText(m.Text)
		for i, c := range m.Choices {
			m.Choices[i] = extension.Clip(c, extension.MaxPanelTitle)
		}
		if len(m.Choices) > extension.MaxPanelChoices {
			m.Choices = m.Choices[:extension.MaxPanelChoices]
		}
		out = append(out, m)
	}
	if n := len(out) - extension.MaxPanelMessages; n > 0 {
		out = out[n:]
	}
	return out
}

// clipText keeps a message within a panel's text limit.
func clipText(s string) string {
	if len(s) <= extension.MaxPanelText {
		return s
	}
	keep := extension.MaxPanelText - 40
	for keep > 0 && !utf8Start(s[keep]) {
		keep--
	}
	return s[:keep] + fmt.Sprintf(" … (%d more bytes)", len(s)-keep)
}

// note is a probe or change set Event as a short line: what the agent ran,
// proposed or withdrew, and what the operator decided, validated or
// applied.
func note(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeProposal:
		var p Proposal
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Change == "" {
			return "", false
		}
		line := "Proposed " + p.Change + ": " + p.Op + " " + p.Pointer
		if n := len(p.Diagnostics); n > 0 {
			line += " (" + plural(n, "problem") + ")"
		}
		return line, true
	case TypeDecision:
		var d Decision
		if json.Unmarshal(e.Data.Data, &d) != nil || len(d.Changes) == 0 || d.Decision == "" {
			return "", false
		}
		who, verb := "You", d.Decision
		if d.Decision == StateWithdrawn {
			who, verb = "The assistant", "withdrew"
		}
		line := who + " " + verb + " " + strings.Join(d.Changes, ", ")
		if d.Reason != "" {
			line += ": " + d.Reason
		}
		return line, true
	case TypeChecks:
		var c Checks
		if json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		who := "The assistant"
		if c.Source == RoleOperator {
			who = "You"
		}
		if c.Valid {
			return fmt.Sprintf("%s validated the approved changes: %s, valid", who, plural(c.Approved, "change")), true
		}
		return fmt.Sprintf("%s validated the approved changes: %s", who, plural(len(c.Diagnostics), "problem")), true
	case TypeProbe:
		var p ProbeEvent
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Probe == "" {
			return "", false
		}
		what := p.Command
		if what == "" {
			what = p.Probe
		}
		line := "The assistant ran " + what
		if p.Why != "" {
			line += ": " + p.Why
		}
		switch {
		case p.Error != "":
			line += " (failed: " + p.Error + ")"
		case p.Redacted:
			line += " (output withheld: it looked like it held a secret)"
		}
		return line, true
	case TypeApplied:
		var a Applied
		if json.Unmarshal(e.Data.Data, &a) != nil || a.Generation == "" {
			return "", false
		}
		return "Applied " + strings.Join(a.Changes, ", ") + " as generation " + a.Generation, true
	}
	return "", false
}

// plural is n and the noun, plural unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
