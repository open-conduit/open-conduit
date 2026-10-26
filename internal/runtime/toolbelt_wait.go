package runtime

import (
	"context"
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
		var latest uint64
		if all, er := tx.Events(0, 0); er != nil {
			return er
		} else if len(all) > 0 {
			latest = all[len(all)-1].Sequence
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
