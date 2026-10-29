package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// The setup extension's change set, as the Setup page draws it. The
// session's `changes` panel carries every change and each open one's
// before, after, reason and problems, with the decision actions; the page
// lays it out as proposals to review rather than a generic panel, and adds
// Apply, which is not an action: the page reads the session's candidate
// and applies it with its expected generation as the operator.
const (
	setupPanelChanges  = "changes"
	setupTypeProposal  = "setup.proposal"
	setupTypeDecision  = "setup.decision"
	setupTypeChecks    = "setup.checks"
	setupTypeApplied   = "setup.applied"
	setupTypeProbe     = "setup.probe"
	setupValidate      = "validate"
	setupApprove       = "approve_"
	setupReject        = "reject_"
	setupApproveAll    = "approve_all_"
	setupStateApproved = "approved"
	setupStateProposed = "proposed"
	setupCodeConflict  = "generation_conflict"
	setupFieldState    = "State"
	setupFieldWhy      = "Why"
	setupFieldBefore   = "Before"
	setupFieldAfter    = "After"
	setupFieldProblem  = "Problem"
	setupApplyCall     = "POST /v1/config/apply"
)

// setupChanges is a session's change set: a lead line, every change with
// its state, the changes still open for a decision, and the controls that
// act on all of them.
type setupChanges struct {
	Lead string
	All  []setupChangeRow
	Open []setupChange
	// ApproveAll and Validate are offered when the session lists them;
	// Apply when a change is approved.
	ApproveAll, Validate *actionView
	Approved             int
	Apply                bool
}

// setupChangeRow is one change in the full list.
type setupChangeRow struct {
	ID, Op, Pointer, State string
}

// setupChange is a change waiting on the operator, or approved and not yet
// applied: its before and after side by side, its reason and problems.
type setupChange struct {
	ID, Op, Pointer, State, Why string
	Before, After               string
	HasBefore, HasAfter         bool
	Problems                    []string
	Approve, Reject             *actionView
}

// Approved says the change is approved and waiting to be applied.
func (c setupChange) Approved() bool { return c.State == setupStateApproved }

// setupChangeSet reads the change set from the session's panels and the
// actions it lists now. A panel from another provider, or without the
// layout the extension writes, is not a change set.
func setupChangeSet(panels []client.Panel, actions []client.Action, entry string, av func(client.Action) *actionView) *setupChanges {
	var p *client.Panel
	for i := range panels {
		if panels[i].ID == setupPanelChanges && providerName(panels[i].Provider) == entry {
			p = &panels[i]
		}
	}
	if p == nil || len(p.Sections) == 0 {
		return nil
	}
	act := func(id string) *actionView {
		if a, ok := setupAction(actions, entry, id); ok {
			return av(a)
		}
		return nil
	}
	cs := &setupChanges{Lead: p.Sections[0].Text}
	for _, ref := range p.Sections[0].Actions {
		switch {
		case strings.HasPrefix(ref.ID, setupApproveAll):
			cs.ApproveAll = act(ref.ID)
		case ref.ID == setupValidate:
			cs.Validate = act(ref.ID)
		}
	}
	rows := map[string]setupChangeRow{}
	for _, sec := range p.Sections[1:] {
		if t := sec.Table; t != nil {
			for _, r := range t.Rows {
				if len(r) < 4 {
					continue
				}
				row := setupChangeRow{ID: r[0], Op: r[1], Pointer: r[2], State: r[3]}
				rows[row.ID] = row
				cs.All = append(cs.All, row)
				if row.State == setupStateApproved {
					cs.Approved++
				}
			}
			continue
		}
		id, _, ok := strings.Cut(sec.Title, ": ")
		row, known := rows[id]
		if !ok || !known {
			continue
		}
		c := setupChange{ID: id, Op: row.Op, Pointer: row.Pointer, State: row.State}
		for _, f := range sec.Fields {
			switch f.Name {
			case setupFieldState:
				c.State = f.Value
			case setupFieldWhy:
				c.Why = f.Value
			case setupFieldBefore:
				c.Before, c.HasBefore = f.Value, true
			case setupFieldAfter:
				c.After, c.HasAfter = f.Value, true
			case setupFieldProblem:
				c.Problems = append(c.Problems, f.Value)
			}
		}
		c.Approve, c.Reject = act(setupApprove+id), act(setupReject+id)
		cs.Open = append(cs.Open, c)
	}
	if len(cs.All) == 0 {
		return nil
	}
	cs.Apply = cs.Approved > 0
	return cs
}

// setupNote is one of the session's change set Events as a short line of
// the conversation: a probe, a proposal, a decision, a validation or an
// apply.
func setupNote(e client.Event) (string, bool) {
	switch e.Type {
	case setupTypeProposal:
		var p struct {
			Change, Op, Pointer, Why string
			Diagnostics              []json.RawMessage
		}
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Change == "" {
			return "", false
		}
		line := "Proposed " + p.Change + ": " + p.Op + " " + p.Pointer
		if n := len(p.Diagnostics); n > 0 {
			line += " (" + plural(n, "problem") + ")"
		}
		return line, true
	case setupTypeDecision:
		var d struct {
			Changes  []string
			Decision string
			Reason   string
		}
		if json.Unmarshal(e.Data.Data, &d) != nil || len(d.Changes) == 0 || d.Decision == "" {
			return "", false
		}
		who, verb := "You", d.Decision
		if d.Decision == "withdrawn" {
			who, verb = "The assistant", "withdrew"
		}
		line := who + " " + verb + " " + strings.Join(d.Changes, ", ")
		if d.Reason != "" {
			line += ": " + d.Reason
		}
		return line, true
	case setupTypeChecks:
		var c struct {
			Source      string
			Valid       bool
			Approved    int
			Diagnostics []json.RawMessage
		}
		if json.Unmarshal(e.Data.Data, &c) != nil {
			return "", false
		}
		who := "The assistant"
		if c.Source == setupRoleOperator {
			who = "You"
		}
		if c.Valid {
			return fmt.Sprintf("%s validated the approved changes: %s, valid", who, plural(c.Approved, "change")), true
		}
		return fmt.Sprintf("%s validated the approved changes: %s", who, plural(len(c.Diagnostics), "problem")), true
	case setupTypeProbe:
		var p struct {
			Probe, Command, Why, Error string
			Redacted                   bool
		}
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
	case setupTypeApplied:
		var a struct {
			Generation string
			Changes    []string
		}
		if json.Unmarshal(e.Data.Data, &a) != nil || a.Generation == "" {
			return "", false
		}
		return "Applied " + strings.Join(a.Changes, ", ") + " as generation " + a.Generation, true
	}
	return "", false
}

// setupApply applies the session's approved changes as the operator: it
// reads the session's candidate (the base text plus the approved changes,
// with the generation it is based on and its hash) and applies it with that
// expected generation, so a configuration that moved meanwhile is refused
// rather than overwritten.
func (s *Server) setupApply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	work := r.PostFormValue("work")
	if work == "" {
		http.Error(w, "apply needs a session", http.StatusBadRequest)
		return
	}
	rc := receipt{Summary: "Apply the approved changes", Call: setupApplyCall, WorkID: work}
	applied, err := s.applyCandidate(ctx, work)
	if err != nil {
		s.record(w, r, "", rc, errors.New(setupApplyErr(err)))
		return
	}
	s.record(w, r, "Applied: generation "+applied.Generation+" is active. The assistant hears it next.", rc, nil)
}

func (s *Server) applyCandidate(ctx context.Context, work string) (client.ConfigApplied, error) {
	c, err := s.src.WorkConfigCandidate(ctx, work)
	if err != nil {
		return client.ConfigApplied{}, err
	}
	return s.src.ApplyConfigText(ctx, []byte(c.ConfigText), c.CandidateHash, c.BaseGeneration, s.opts.NewKey())
}

// setupApplyErr says why apply was refused.
func setupApplyErr(err error) string {
	var e *client.Error
	switch {
	case errors.As(err, &e) && e.Code == setupCodeConflict:
		return "The configuration changed since these changes were based on it, so nothing was applied. Reload, review the changes again and apply."
	case errors.As(err, &e) && e.Status == http.StatusNotFound:
		return "This session has nothing to apply."
	case errors.As(err, &e) && e.Message != "":
		return "Not applied: " + e.Message + " (" + e.Code + ")"
	}
	return "Not applied: " + errText(err)
}

// setupBack is where a change on the session returns to.
func setupBack(work string) string { return "/setup?" + url.Values{"session": {work}}.Encode() }
