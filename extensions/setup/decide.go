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

// ConfigCandidate is the session's change set for core's review: every
// change with its state, its before and after (reference text included:
// the candidate is an operator-class read, the one view where the operator
// sees the references they approve) and the actions that decide it. Once a
// change is approved and the approved changes validate, it is also the
// candidate to apply: the base text plus the approved changes, its base
// generation and hash. A session that has proposed nothing has none.
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
	var cs changeSet
	var st map[string]string
	err = x.host.Store.Transact(ctx, storage.Guard{Now: x.host.Now()}, func(tx storage.Tx) error {
		var ok bool
		var err error
		cs, ok, err = loadChangeSet(tx, w.ID)
		if err != nil || !ok || len(cs.Changes) == 0 {
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
		if st, err = states(tx, w, cs); err != nil {
			return err
		}
		cand, ids, _ = candidate(ctx, x.host.Config, cs, st)
		return nil
	})
	if err != nil {
		return extension.Candidate{}, err
	}
	out := review(cs, st, !w.State.Terminal())
	if len(ids) == 0 {
		return out, nil
	}
	hash, diags, err := x.host.Config.Validate(ctx, cand.Handle)
	if err != nil || len(diags) > 0 {
		// Nothing to apply until the approved changes validate; validating
		// them in the session says why.
		return out, nil
	}
	out.Handle, out.Hash = cand.Handle, hash
	return out, nil
}

// review is the change set as core reviews it: every change in proposal
// order, and while the session is open the actions that decide them.
func review(cs changeSet, st map[string]string, live bool) extension.Candidate {
	out := extension.Candidate{BaseGeneration: cs.BaseGeneration}
	pending, approved := 0, 0
	for _, c := range cs.Changes {
		x := extension.CandidateChange{ID: c.ID, Op: c.Op, Pointer: c.Pointer, State: st[c.ID], Why: c.Why, Before: c.Before}
		if c.Op != OpRemove {
			x.After = c.Value
		}
		for _, d := range c.Diagnostics {
			x.Problems = append(x.Problems, d.Path+": "+d.Code+": "+d.Message)
		}
		switch x.State {
		case StateProposed:
			pending++
			if live {
				x.Approve, x.Reject = approvePrefix+c.ID, rejectPrefix+c.ID
			}
		case StateApproved:
			approved++
			if live {
				x.Reject = rejectPrefix + c.ID
			}
		}
		out.Changes = append(out.Changes, x)
	}
	if live && pending > 1 {
		out.ApproveAll = approveAllPrefix + revision(open(cs, st))
	}
	if live && approved > 0 {
		out.Validate = ActionValidate
	}
	return out
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
