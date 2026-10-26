package reference

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Toolbelt is a volatile authority double over the public storage contract.
// It is not a management server or an implementation of scheduling/wake policy.
type Toolbelt struct {
	mu            sync.Mutex
	Store         storage.Store
	Now           func() time.Time
	tokens        map[string]extension.Authority
	registrations map[string]bool
	operations    map[string]extension.Operation
	events        map[string]bool
}

func NewToolbelt(s storage.Store, now func() time.Time) *Toolbelt {
	return &Toolbelt{Store: s, Now: now, tokens: map[string]extension.Authority{}, registrations: map[string]bool{}, operations: map[string]extension.Operation{}, events: map[string]bool{}}
}
func builtin(n string) bool {
	return n == extension.Emit || n == extension.CreateWork || n == extension.Wait || n == extension.DeclareOutcome || n == extension.ReadContext
}
func (t *Toolbelt) Register(r extension.Registration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.Name == "" || r.Version != extension.ContractVersion {
		return extension.ErrInvalid
	}
	if t.registrations[r.Name] {
		return extension.ErrConflict
	}
	names, events := map[string]bool{}, map[string]bool{}
	for _, op := range r.Operations {
		if op.Name == "" || op.Handler == nil {
			return extension.ErrInvalid
		}
		if builtin(op.Name) || t.operations[op.Name].Handler != nil || names[op.Name] {
			return extension.ErrConflict
		}
		names[op.Name] = true
	}
	for _, n := range r.EventTypes {
		if n == "" {
			return extension.ErrInvalid
		}
		if events[n] || t.events[n] {
			return extension.ErrConflict
		}
		events[n] = true
	}
	t.registrations[r.Name] = true
	for _, op := range r.Operations {
		t.operations[op.Name] = op
	}
	for _, n := range r.EventTypes {
		t.events[n] = true
	}
	return nil
}
func (t *Toolbelt) Issue(_ context.Context, a extension.Authority) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a.WorkID == "" || a.ScopeID == "" || a.ExecutionID == "" || a.Lease.ExecutionID != a.ExecutionID || !a.ExpiresAt.After(t.Now()) {
		return "", extension.ErrUnauthorized
	}
	for _, op := range a.Operations {
		if !builtin(op) && t.operations[op].Handler == nil {
			return "", extension.ErrUndeclared
		}
	}
	for _, n := range a.EventTypes {
		if !t.events[n] {
			return "", extension.ErrUndeclared
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	a.Operations = append([]string(nil), a.Operations...)
	a.EventTypes = append([]string(nil), a.EventTypes...)
	t.tokens[token] = a
	return token, nil
}
func (t *Toolbelt) Revoke(_ context.Context, token string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tokens, token)
	return nil
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func (t *Toolbelt) Call(ctx context.Context, token string, c extension.ToolCall) (extension.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.tokens[token]
	if !ok || !a.ExpiresAt.After(t.Now()) || c.ScopeID != "" && c.ScopeID != a.ScopeID || c.WorkID != "" && c.WorkID != a.WorkID || c.ExecutionID != "" && c.ExecutionID != a.ExecutionID {
		return extension.ToolResult{}, extension.ErrUnauthorized
	}
	if !builtin(c.Operation) && t.operations[c.Operation].Handler == nil {
		return extension.ToolResult{}, extension.ErrUndeclared
	}
	if !contains(a.Operations, c.Operation) {
		return extension.ToolResult{}, extension.ErrUnauthorized
	}
	var out extension.ToolResult
	err := t.Store.Transact(ctx, storage.Guard{Now: t.Now(), Lease: &a.Lease}, func(tx storage.Tx) error {
		w, err := tx.Work(a.WorkID)
		if err != nil {
			return err
		}
		x, err := tx.Execution(a.ExecutionID)
		if err != nil {
			return err
		}
		if w.ScopeID != a.ScopeID || x.WorkID != a.WorkID || x.TriggerID != a.TriggerID {
			return extension.ErrUnauthorized
		}
		switch c.Operation {
		case extension.ReadContext:
			out.Context = w.Context
		case extension.Emit:
			if c.Event == nil {
				return extension.ErrInvalid
			}
			e := *c.Event
			if !t.events[e.Type] || !contains(a.EventTypes, e.Type) {
				return extension.ErrUndeclared
			}
			if e.CausationID != "" && e.CausationID != a.TriggerID || e.WorkID != "" && e.WorkID != a.WorkID || e.ExecutionID != "" && e.ExecutionID != a.ExecutionID {
				return extension.ErrUnauthorized
			}
			e.WorkID, e.ExecutionID, e.CausationID = a.WorkID, a.ExecutionID, a.TriggerID
			e.Source, e.ReceivedAt = "execution/"+string(a.ExecutionID), t.Now()
			if e.CorrelationKey != nil {
				owned := false
				for _, k := range w.CorrelationKeys {
					if k == *e.CorrelationKey {
						owned = true
					}
				}
				if !owned {
					return extension.ErrUnauthorized
				}
			}
			accepted, _, err := tx.AcceptEvent(e)
			if err != nil {
				return err
			}
			out.Event = &accepted
		case extension.CreateWork:
			if c.Child == nil {
				return extension.ErrInvalid
			}
			child := *c.Child
			if child.ScopeID != "" && child.ScopeID != a.ScopeID || child.ParentID != "" && child.ParentID != a.WorkID {
				return extension.ErrUnauthorized
			}
			child.ScopeID, child.ParentID, child.State = a.ScopeID, a.WorkID, domain.WorkReady
			if err := tx.PutWork(child, 0); err != nil {
				return err
			}
			saved, err := tx.Work(child.ID)
			if err != nil {
				return err
			}
			out.Work = &saved
			created := domain.Event{ID: domain.ID("child-created/" + string(child.ID)), Type: "core.work.created", Source: "execution/" + string(a.ExecutionID), DeliveryID: "child-created/" + string(child.ID), CausationID: a.TriggerID, WorkID: a.WorkID, ExecutionID: a.ExecutionID, OccurredAt: t.Now(), ReceivedAt: t.Now()}
			accepted, _, err := tx.AcceptEvent(created)
			if err != nil {
				return err
			}
			out.Event = &accepted
		case extension.DeclareOutcome:
			if c.Outcome == nil {
				return extension.ErrInvalid
			}
			if err := extension.ValidateResult(extension.Result{Outcome: *c.Outcome}); err != nil {
				return err
			}
			// Waiting must register its predicate atomically using wait, not this call.
			if c.Outcome.Kind == domain.OutcomeWaiting {
				return extension.ErrInvalid
			}
			x.Outcome = c.Outcome
			x.State = domain.ExecutionState(c.Outcome.Kind)
			return tx.PutExecution(x, x.Revision)
		case extension.Wait:
			if c.Wait == nil {
				return extension.ErrInvalid
			}
			wait := domain.Wait{ID: domain.ID("wait/" + string(a.ExecutionID)), WorkID: a.WorkID, ExecutionID: a.ExecutionID, State: domain.WaitActive, Match: c.Wait.Match, AfterSequence: c.Wait.AfterSequence}
			if c.Wait.Timeout != nil {
				timerID := domain.ID("timeout/" + string(a.ExecutionID))
				wait.TimeoutTimerID = timerID
				e := domain.Event{ID: timerID, Type: "core.wait.timeout", Source: "core.timer", DeliveryID: string(timerID), CausationID: a.TriggerID, WorkID: a.WorkID, ExecutionID: a.ExecutionID}
				if err := tx.PutTimer(domain.Timer{ID: timerID, FireAt: *c.Wait.Timeout, Event: e}, 0); err != nil {
					return err
				}
			}
			x.Outcome = &domain.Outcome{Kind: domain.OutcomeWaiting}
			x.State = domain.ExecutionWaiting
			if err := tx.PutExecution(x, x.Revision); err != nil {
				return err
			}
			w.State = domain.WorkWaiting
			if err := tx.PutWork(w, w.Revision); err != nil {
				return err
			}
			if err := tx.PutWait(wait, 0); err != nil {
				return err
			}
			saved, err := tx.Wait(wait.ID)
			if err != nil {
				return err
			}
			out.Wait = &saved
		default:
			// This fixture permits pure handlers only. External writes must run outside
			// coordination transactions in real toolbelt implementations.
			a.Operations = append([]string(nil), a.Operations...)
			a.EventTypes = append([]string(nil), a.EventTypes...)
			out.Output, err = t.operations[c.Operation].Handler(ctx, a, c.Input)
			return err
		}
		return nil
	})
	if errors.Is(err, storage.ErrLeaseLost) {
		err = extension.ErrUnauthorized
	}
	if err != nil {
		return extension.ToolResult{}, err
	}
	return out, nil
}
