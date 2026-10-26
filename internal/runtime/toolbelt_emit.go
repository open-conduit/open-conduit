package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// translatedEmit applies an extension emit translation (03-08). The handler
// runs outside storage after the fenced authority check; its Event is then
// accepted under the fence exactly like the builtin emit: a family the same
// registration declared and the token allows, trusted execution source and
// trigger causation, and AcceptDerivedInTx's chain-depth and per-chain rate
// limits. The registration's own records join that transaction. Nothing in
// the plan can select a root, another Work's identity or a namespace the
// registration does not own; a plan may cite one of the registration's own
// Events in the scope as its cause (05-03), which the trigger still floors.
func (t *Toolbelt) translatedEmit(ctx context.Context, digest, owner string, op extension.Operation, a extension.Authority, input domain.Payload, replayKey, hash string) (out extension.ToolResult, err error) {
	plan, err := op.Emit(ctx, a, input)
	if err != nil {
		return out, err
	}
	ev := plan.Event
	t.mu.RLock()
	family := t.events[ev.Type]
	t.mu.RUnlock()
	if family == "" || family != owner || !contains(a.EventTypes, ev.Type) {
		return out, extension.ErrUndeclared
	}
	if ev.ID != "" || ev.Source != "" || ev.CausationID != "" || ev.WorkID != "" || ev.ExecutionID != "" || !ev.ReceivedAt.IsZero() {
		return out, extension.ErrUnauthorized
	}
	if ev.DeliveryID == "" || len(ev.DeliveryID) > 256 || strings.HasPrefix(ev.Type, "core.") || plan.Commit == nil {
		return out, extension.ErrInvalid
	}
	if k := ev.CorrelationKey; k != nil && (k.Source == "" || k.ExternalID == "") {
		return out, extension.ErrInvalid
	}
	ref := a.Lease
	now := t.now()
	var refusal error
	err = t.store.Transact(ctx, storage.Guard{Now: now, Lease: &ref}, func(tx storage.Tx) error {
		_, x, w, _, e := t.authorizedInTx(tx, digest)
		if e != nil {
			return e
		}
		replay, e := t.replayInTx(tx, replayKey, hash)
		if e != nil || replay != nil {
			if replay != nil {
				out = replay.Result
			}
			return e
		}
		trigger, e := executionTriggerInTx(tx, x)
		if e != nil {
			return e
		}
		e2 := ev
		if ev.CorrelationKey != nil {
			k := *ev.CorrelationKey
			e2.CorrelationKey = &k
		}
		if e2.CorrelationKey == nil || containsKey(w, *e2.CorrelationKey) {
			e2.WorkID, e2.ExecutionID = w.ID, x.ID
		} else if !ownedNamespace(e2.CorrelationKey.Source, owner, "/") {
			return extension.ErrUnauthorized
		}
		e2.Source = "execution/" + string(x.ID)
		e2.ID = domain.ID("emit/" + identity(string(x.ID), e2.DeliveryID))
		e2.ReceivedAt = now
		if e2.OccurredAt.IsZero() {
			e2.OccurredAt = now
		}
		// A cited cause (05-03) must be one of the registration's own Events
		// in this scope; the trigger then floors the chain position.
		parent, floor := trigger, domain.ID("")
		if plan.Cause != "" && plan.Cause != trigger {
			cause, er := tx.Event(plan.Cause)
			if errors.Is(er, storage.ErrNotFound) {
				return extension.ErrUnauthorized
			}
			if er != nil {
				return er
			}
			scope, er := scopeForEvent(tx, cause)
			if er != nil {
				return er
			}
			if k := cause.Event.CorrelationKey; scope != w.ScopeID || k == nil || !ownedNamespace(k.Source, owner, "/") {
				return extension.ErrUnauthorized
			}
			parent, floor = plan.Cause, trigger
		}
		accepted, added, er := acceptCausalInTx(tx, now, t.options.CausalLimits, w.ScopeID, parent, floor, e2)
		if errors.Is(er, ErrCausalRejected) {
			refusal = er
			return nil
		}
		if er != nil {
			return er
		}
		var info CausalInfo
		if er = getJSON(tx, causalNamespace, causalKey(accepted.Event.ID), &info); er != nil && !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		commit, er := plan.Commit(extension.Emitted{Event: accepted, Added: added, Root: info.Root, Depth: info.Depth})
		if er != nil {
			return er
		}
		for _, r := range commit.Records {
			if !ownedNamespace(r.Namespace, owner, ".") || r.Key == "" || len(r.Key) > 512 {
				return extension.ErrUnauthorized
			}
		}
		if added {
			for _, r := range commit.Records {
				if er = tx.Put(r.Namespace, r.Key, r.Value); er != nil {
					return er
				}
			}
		}
		out.Event, out.Output = &accepted, commit.Output
		if replayKey != "" {
			return putJSON(tx, requestNamespace, replayKey, toolbeltReplay{Hash: hash, Result: out})
		}
		return nil
	})
	if err == nil && refusal != nil {
		// The refusal diagnostic committed without any associated effects.
		return extension.ToolResult{}, refusal
	}
	if err != nil {
		return extension.ToolResult{}, err
	}
	return out, nil
}

// ownedNamespace reports whether name is the registration's own name or lies
// beneath it after sep.
func ownedNamespace(name, owner, sep string) bool {
	return name == owner || strings.HasPrefix(name, owner+sep) && len(name) > len(owner)+len(sep)
}
