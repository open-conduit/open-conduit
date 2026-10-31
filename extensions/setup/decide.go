package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Change set action IDs: approve_<change>, reject_<change>,
// approve_all_<revision> and validate.
const (
	ActionValidate   = "validate"
	approvePrefix    = "approve_"
	rejectPrefix     = "reject_"
	approveAllPrefix = "approve_all_"
)

// PanelChanges is the change set's panel on a session's Work.
const PanelChanges = "changes"

// sessionChanges is the session's change set and each change's state, as
// it stands (not refreshed: actions read, they never write).
func sessionChanges(tx storage.Tx, w domain.Work) (changeSet, map[string]string, error) {
	cs, _, err := loadChangeSet(tx, w.ID)
	if err != nil {
		return cs, nil, err
	}
	st, err := states(tx, w, cs)
	return cs, st, err
}

// changeActions are the change set's actions on an open session: approve
// and reject for each open change (reject also for an approved one, until
// it is applied), Approve all while more than one is open, and validate
// once one is approved.
func changeActions(cs changeSet, st map[string]string) []extension.Action {
	var out []extension.Action
	pending := open(cs, st)
	if len(pending) > 1 {
		out = append(out, extension.Action{ID: approveAllPrefix + revision(pending), Label: fmt.Sprintf("Approve all %d", len(pending))})
	}
	approved := 0
	for _, c := range cs.Changes {
		switch st[c.ID] {
		case StateProposed:
			out = append(out, extension.Action{ID: approvePrefix + c.ID, Label: "Approve " + c.ID},
				extension.Action{ID: rejectPrefix + c.ID, Label: "Reject " + c.ID, Prompt: "why (optional)"})
		case StateApproved:
			approved++
			out = append(out, extension.Action{ID: rejectPrefix + c.ID, Label: "Reject " + c.ID, Prompt: "why (optional)"})
		}
	}
	if approved > 0 {
		out = append(out, extension.Action{ID: ActionValidate, Label: "Validate the approved changes"})
	}
	return out
}

// decide plans a change set action: one setup.decision on the session's
// key, which the route hands the agent.
func (a Actions) decide(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error, bool) {
	w := *c.Target.Work
	if c.Action != ActionValidate && !strings.HasPrefix(c.Action, approvePrefix) && !strings.HasPrefix(c.Action, rejectPrefix) {
		return extension.ActionPlan{}, nil, false
	}
	var cs changeSet
	var st map[string]string
	if err := a.transact(ctx, func(tx storage.Tx) error {
		var e error
		cs, st, e = sessionChanges(tx, w)
		return e
	}); err != nil {
		return extension.ActionPlan{}, err, true
	}
	key, _ := sessionKey(w)
	plan := func(d Decision, message string) (extension.ActionPlan, error, bool) {
		return extension.ActionPlan{Event: domain.Event{Type: TypeDecision, CorrelationKey: &key, Data: payload(SchemaDecision, d)}, Message: message}, nil, true
	}
	switch {
	case c.Action == ActionValidate:
		if !slices.ContainsFunc(cs.Changes, func(x change) bool { return st[x.ID] == StateApproved }) {
			p, err := refuseAction("nothing_approved", "approve a change before validating")
			return p, err, true
		}
		plan, err := a.validate(ctx, w)
		return plan, err, true
	case strings.HasPrefix(c.Action, approveAllPrefix):
		pending := open(cs, st)
		if len(pending) < 2 || c.Action != approveAllPrefix+revision(pending) {
			p, err := refuseAction("change_set_moved", "the proposals changed since the page loaded; reload and review them")
			return p, err, true
		}
		ids := make([]string, len(pending))
		for i, x := range pending {
			ids[i] = x.ID
		}
		return plan(Decision{Changes: ids, Decision: StateApproved}, "approved "+strings.Join(ids, ", "))
	case strings.HasPrefix(c.Action, approvePrefix):
		id := strings.TrimPrefix(c.Action, approvePrefix)
		if st[id] != StateProposed {
			p, err := refuseAction("not_open", "no open change "+id)
			return p, err, true
		}
		return plan(Decision{Changes: []string{id}, Decision: StateApproved}, "approved "+id)
	default:
		id := strings.TrimPrefix(c.Action, rejectPrefix)
		if s := st[id]; s != StateProposed && s != StateApproved {
			p, err := refuseAction("not_open", "no open change "+id)
			return p, err, true
		}
		reason := strings.TrimSpace(c.Input)
		if len(reason) > maxContext {
			p, err := refuseAction("invalid_input", "give a reason of at most 4000 bytes")
			return p, err, true
		}
		return plan(Decision{Changes: []string{id}, Decision: StateRejected, Reason: reason}, "rejected "+id)
	}
}

// validate is the operator's validation of the candidate, recorded as
// setup.checks on the session.
func (a Actions) validate(ctx context.Context, w domain.Work) (extension.ActionPlan, error) {
	if a.Config == nil {
		return refuseAction("unavailable", "the configuration can't be read now")
	}
	checks, err := check(ctx, a.Store, a.Config, a.now, w, RoleOperator)
	if err != nil {
		return extension.ActionPlan{}, err
	}
	key, _ := sessionKey(w)
	message := "the approved changes are valid"
	if !checks.Valid {
		message = strconv.Itoa(len(checks.Diagnostics)) + " problems in the approved changes"
	}
	return extension.ActionPlan{Event: domain.Event{Type: TypeChecks, CorrelationKey: &key, Data: payload(SchemaChecks, checks)}, Message: message}, nil
}

// Panels shows a session's change set: every change with its state, and
// each open one with its before and after, its reason and its diagnostics,
// reference text included. Panels are an operator-class read, so this is
// the one view where the operator sees the references they approve.
func (a Actions) Panels(ctx context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	if t.Work == nil || t.Work.Kind != WorkKind {
		return nil, nil
	}
	var cs changeSet
	var st map[string]string
	if err := a.transact(ctx, func(tx storage.Tx) error {
		var e error
		cs, st, e = sessionChanges(tx, *t.Work)
		return e
	}); err != nil {
		return nil, err
	}
	if len(cs.Changes) == 0 {
		return nil, nil
	}
	p := extension.Panel{ID: PanelChanges, Title: "Proposed changes"}
	summary := extension.PanelSection{Text: fmt.Sprintf("Based on generation %s. Nothing changes until you apply the approved changes.", cs.BaseGeneration)}
	pending := open(cs, st)
	approved := 0
	table := &extension.PanelTable{Columns: []string{"Change", "Op", "Pointer", "State"}}
	for _, c := range cs.Changes {
		if st[c.ID] == StateApproved {
			approved++
		}
		if len(table.Rows) < extension.MaxPanelRows {
			table.Rows = append(table.Rows, []string{c.ID, c.Op, cut(c.Pointer), st[c.ID]})
		}
	}
	if len(pending) > 1 {
		summary.Actions = append(summary.Actions, approveAllPrefix+revision(pending))
	}
	if approved > 0 {
		summary.Actions = append(summary.Actions, ActionValidate)
	}
	p.Sections = append(p.Sections, summary, extension.PanelSection{Title: "All changes", Table: table})
	room := extension.MaxPanelSections - len(p.Sections)
	for _, c := range cs.Changes {
		if s := st[c.ID]; s != StateProposed && s != StateApproved {
			continue
		}
		if room == 0 {
			p.Sections[0].Text += " More changes are open than fit here: decide these first."
			break
		}
		room--
		sec := extension.PanelSection{Title: c.ID + ": " + c.Op + " " + cut(c.Pointer), Fields: []extension.PanelField{
			{Name: "State", Value: st[c.ID]}, {Name: "Why", Value: cut(c.Why)}}}
		if len(c.Before) > 0 {
			sec.Fields = append(sec.Fields, extension.PanelField{Name: "Before", Value: cut(string(c.Before))})
		}
		if c.Op != OpRemove {
			sec.Fields = append(sec.Fields, extension.PanelField{Name: "After", Value: cut(string(c.Value))})
		}
		for i, d := range c.Diagnostics {
			if i == 4 {
				sec.Fields = append(sec.Fields, extension.PanelField{Name: "Problem", Value: fmt.Sprintf("and %d more", len(c.Diagnostics)-i)})
				break
			}
			sec.Fields = append(sec.Fields, extension.PanelField{Name: "Problem", Value: cut(d.Path + ": " + d.Code + ": " + d.Message)})
		}
		if st[c.ID] == StateProposed {
			sec.Actions = append(sec.Actions, approvePrefix+c.ID)
		}
		sec.Actions = append(sec.Actions, rejectPrefix+c.ID)
		p.Sections = append(p.Sections, sec)
	}
	return []extension.Panel{p}, nil
}

// cut keeps a panel value within its limit, saying how much it left out.
func cut(s string) string {
	if len(s) <= extension.MaxPanelValue {
		return s
	}
	keep := extension.MaxPanelValue - 40
	for keep > 0 && !utf8Start(s[keep]) {
		keep--
	}
	return s[:keep] + fmt.Sprintf(" … (%d more bytes)", len(s)-keep)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ConfigCandidate is the session's candidate for the operator to apply:
// the base text plus the approved changes, its base generation and hash.
// A session with nothing approved, or whose candidate doesn't validate,
// has none to apply.
func (x *Extension) ConfigCandidate(ctx context.Context, w domain.Work) (extension.Candidate, error) {
	if w.Kind != WorkKind || x.host.Config == nil {
		return extension.Candidate{}, storage.ErrNotFound
	}
	base, err := x.host.Config.Base(ctx)
	if err != nil {
		return extension.Candidate{}, refuse("unavailable", "the configuration can't be read now")
	}
	var cand built
	var ids []string
	err = x.host.Store.Transact(ctx, storage.Guard{Now: x.host.Now()}, func(tx storage.Tx) error {
		cs, ok, err := loadChangeSet(tx, w.ID)
		if err != nil || !ok {
			return errors.Join(err, storage.ErrNotFound)
		}
		latest, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		if moved := refresh(ctx, cs, base, x.host.Config, latest); moved.BaseGeneration != cs.BaseGeneration || moved.BaseHandle != cs.BaseHandle {
			if err = saveChangeSet(tx, w.ID, moved); err != nil {
				return err
			}
			cs = moved
		}
		st, err := states(tx, w, cs)
		if err != nil {
			return err
		}
		cand, ids, _ = candidate(ctx, x.host.Config, cs, st)
		return nil
	})
	if errors.Is(err, storage.ErrNotFound) {
		return extension.Candidate{}, storage.ErrNotFound
	}
	if err != nil {
		return extension.Candidate{}, err
	}
	if len(ids) == 0 {
		return extension.Candidate{}, refuse("nothing_approved", "approve a change before applying")
	}
	hash, diags, err := x.host.Config.Validate(ctx, cand.Handle)
	if err != nil {
		return extension.Candidate{}, refuse("unavailable", "the configuration can't be validated now")
	}
	if len(diags) > 0 {
		return extension.Candidate{}, refuse("invalid_candidate", "the approved changes don't validate: validate them in the session to see why")
	}
	return extension.Candidate{Handle: cand.Handle, BaseGeneration: base.Generation, Hash: hash}, nil
}

// decisionRender renders the operator's decision for the agent.
func decisionRender(e domain.Event) (string, bool) {
	var d Decision
	if e.Data.Schema != SchemaDecision || json.Unmarshal(e.Data.Data, &d) != nil {
		return "", false
	}
	out := "### The operator " + d.Decision + " " + strings.Join(d.Changes, ", ")
	if d.Reason != "" {
		out += "\n" + d.Reason
	}
	if d.Decision == StateRejected {
		out += "\nDon't propose the same change again; ask what they want instead."
	}
	return out, true
}
