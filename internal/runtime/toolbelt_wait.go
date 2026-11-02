package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// declareTranslatedWait applies an extension wait translation. The handler
// runs outside storage after the fenced authority check; its plan is then
// declared under the fence exactly like the builtin wait, so it releases
// authority immediately and registers only when the trusted host observes the
// segment finish or suspend. Dependencies become subscription requests on the
// planned Wait, installed after registration with their own backscan.
func (t *Toolbelt) declareTranslatedWait(ctx context.Context, record tokenRecord, digest, owner string, op extension.Operation, a extension.Authority, input domain.Payload) (out extension.ToolResult, err error) {
	plan, err := op.Wait(ctx, a, input)
	if err != nil {
		return out, err
	}
	return t.declarePlan(ctx, record, digest, owner, a, plan)
}

// declarePlan declares a wait plan owner made (a translated wait, or a
// gate parking a call) under the fence.
func (t *Toolbelt) declarePlan(ctx context.Context, record tokenRecord, digest, owner string, a extension.Authority, plan extension.WaitPlan) (out extension.ToolResult, err error) {
	seen := map[string]bool{}
	for _, d := range plan.Dependencies {
		if d.ID == "" || len(d.ID) > 256 || seen[d.ID] || d.SourceKey.Source == "" || d.SourceKey.ExternalID == "" || !validType(d.AliasType) || strings.HasPrefix(d.AliasType, "core.") || len(d.Label) > 200 {
			return out, extension.ErrInvalid
		}
		seen[d.ID] = true
		for _, typ := range d.Types {
			if !validType(typ) {
				return out, extension.ErrInvalid
			}
		}
		if d.SourceScopeID != "" && d.SourceScopeID != a.ScopeID {
			if t.options.AuthorizeAlias == nil {
				return out, extension.ErrUnauthorized
			}
			if e := t.options.AuthorizeAlias(ctx, AliasAuthorization{SourceScopeID: d.SourceScopeID, TargetScopeID: a.ScopeID, Generation: record.Generation}); e != nil {
				return out, extension.ErrUnauthorized
			}
		}
	}
	ref := a.Lease
	now := t.now()
	err = t.store.Transact(ctx, storage.Guard{Now: now, Lease: &ref}, func(tx storage.Tx) error {
		_, x, w, state, e := t.authorizedInTx(tx, digest)
		if e != nil {
			return e
		}
		trigger, e := executionTriggerInTx(tx, x)
		if e != nil {
			return e
		}
		request := plan.Wait
		if len(plan.Dependencies) > 0 && len(request.Match.Keys) == 0 {
			// Aliases arrive on one dependent-owned key; waiting on every key
			// the Work owns keeps own-key input (comments, checks) eligible too.
			request.Match.Keys = append([]domain.CorrelationKey(nil), w.CorrelationKeys...)
		}
		decl, e := t.declareWaitInTx(tx, x, w, state, trigger, request, now)
		if e != nil {
			return e
		}
		// The declaring operation's extension owns the Wait: it alone may
		// say how the wait is going (SetWaitProgressInTx).
		decl.Wait.Owner = owner
		latest, er := tx.LatestSequence()
		if er != nil {
			return er
		}
		for _, d := range plan.Dependencies {
			if len(decl.Wait.Match.Types) > 0 && !contains(decl.Wait.Match.Types, d.AliasType) {
				return extension.ErrInvalid
			}
			scope := d.SourceScopeID
			if scope == "" {
				scope = w.ScopeID
			}
			sub := SubscriptionRequest{Namespace: owner, ID: string(decl.Wait.ID) + "/" + d.ID, WaitID: decl.Wait.ID, SourceScopeID: scope,
				SourceKey: d.SourceKey, Types: append([]string(nil), d.Types...), TargetKey: decl.Wait.Match.Keys[0], AliasType: d.AliasType,
				Label: d.Label}
			if d.Filter.Schema != "" || len(d.Filter.Data) > 0 {
				filter := d.Filter
				sub.Filter = &filter
			}
			if d.FromNow {
				sub.After = latest
			}
			decl.Subscriptions = append(decl.Subscriptions, sub)
		}
		state.Declaration = decl
		if e = putAttemptState(tx, state); e != nil {
			return e
		}
		planned := *decl.Wait
		out.Wait = &planned
		return nil
	})
	if err != nil {
		return extension.ToolResult{}, err
	}
	out.Output = plan.Output
	return out, nil
}

// gate asks every registered gate but the operation owner's, in
// registration-name order, whether the call may proceed. It returns the
// first gate's plan to park the caller, with the gate's name, or its
// refusal wrapped in extension.GateRefusal. A gate that panics refuses.
func (t *Toolbelt) gate(ctx context.Context, owner, operation string, a extension.Authority, input domain.Payload) (*extension.WaitPlan, string, error) {
	t.mu.RLock()
	names := make([]string, 0, len(t.gates))
	gates := make(map[string]extension.Gate, len(t.gates))
	for name, g := range t.gates {
		if name != owner {
			names = append(names, name)
			gates[name] = g
		}
	}
	t.mu.RUnlock()
	sort.Strings(names)
	for _, name := range names {
		plan, err := callGate(ctx, gates[name], a, extension.GateCall{Operation: operation, Owner: owner, Input: input})
		if err != nil {
			return nil, name, &extension.GateRefusal{Gate: name, Err: err}
		}
		if plan != nil {
			return plan, name, nil
		}
	}
	return nil, "", nil
}

func callGate(ctx context.Context, g extension.Gate, a extension.Authority, call extension.GateCall) (plan *extension.WaitPlan, err error) {
	defer func() {
		if recover() != nil {
			plan, err = nil, &extension.OperationError{Code: "gate_failed", Message: "the host could not decide whether this call may proceed, so it was refused"}
		}
	}()
	a.Operations = append([]string(nil), a.Operations...)
	a.EventTypes = append([]string(nil), a.EventTypes...)
	return g(ctx, a, call)
}
