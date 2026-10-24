package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// PendingInputsInTx exposes ordered input for a scheduler's atomic claim. It
// never consumes input; use ConsumeInputsInTx in the same guarded transaction.
func PendingInputsInTx(tx storage.Tx, work domain.ID) ([]QueuedInput, error) {
	return pendingInputs(tx, work)
}

// Complete settles a successful attempt under its current lease. Unrouted
// accepted input forces a retry; queued or unconfirmed injected input keeps Work
// ready. No execution transport acknowledgement is inferred from an intent.
func (r *Router) Complete(ctx context.Context, ref domain.LeaseRef) error {
	return r.transact(ctx, &ref, func(tx storage.Tx) error {
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		events, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		for _, a := range events {
			if a.Routed {
				continue
			}
			target, e := potentiallyTargets(tx, a.Event, w)
			if e != nil {
				return e
			}
			if !target {
				continue
			}
			owner, e := routeOwner(tx, a.Event)
			if e != nil && !errors.Is(e, storage.ErrNotFound) {
				return e
			}
			if owner.ID == w.ID {
				return storage.ErrConflict
			}
		}
		x.State = domain.ExecutionCompleted
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeCompleted}
		if err = tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		pending, err := pendingInputs(tx, w.ID)
		if err != nil {
			return err
		}
		w.State = domain.WorkCompleted
		if len(pending) > 0 {
			w.State = domain.WorkReady
		}
		return tx.PutWork(w, w.Revision)
	})
}

// Cancel fences all attempts and removes eligibility in the same transaction.
// Physical process cleanup is performed by later scheduler/executor dispatch.
// Historical accepted inputs and control intents remain available for audit.
func (r *Router) Cancel(ctx context.Context, id domain.ID) error {
	return r.transact(ctx, nil, func(tx storage.Tx) error {
		w, err := tx.Work(id)
		if err != nil {
			return err
		}
		if w.State.Terminal() {
			return nil
		}
		waits, err := tx.ListWaits(id)
		if err != nil {
			return err
		}
		for _, wait := range waits {
			if wait.State == domain.WaitActive {
				wait.State = domain.WaitCancelled
				if err = disableWaitSubscriptionsInTx(tx, wait.ID); err != nil {
					return err
				}
				if err = tx.PutWait(wait, wait.Revision); err != nil {
					return err
				}
			}
		}
		xs, err := tx.ListExecutions(id)
		if err != nil {
			return err
		}
		for _, x := range xs {
			if !x.State.Terminal() {
				x.State = domain.ExecutionCancelled
				x.Outcome = nil
				if err = tx.PutExecution(x, x.Revision); err != nil {
					return err
				}
			}
		}
		timers, err := tx.ListTimers()
		if err != nil {
			return err
		}
		for _, timer := range timers {
			target, e := potentiallyTargets(tx, timer.Event, w)
			if e != nil {
				return e
			}
			for _, wait := range waits {
				if wait.TimeoutTimerID == timer.ID {
					target = true
				}
			}
			if !target {
				continue
			}
			if err = tx.DeleteTimer(timer.ID, timer.Revision); err != nil {
				return err
			}
			if err = tx.Delete(timerNamespace, timerKey(timer.ID)); err != nil {
				return err
			}
		}
		entries, err := tx.List(queueNamespace, identity(string(id))+"/")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err = tx.Delete(queueNamespace, entry.Key); err != nil {
				return err
			}
		}
		if err = tx.Delete(wakeReadyNamespace, identity(string(id))); err != nil {
			return err
		}
		// These namespaces are reserved for the scheduler's retry policy.
		if err = tx.Delete("core.scheduler.eligibility", identity(string(id))); err != nil {
			return err
		}
		w.State = domain.WorkCancelled
		return tx.PutWork(w, w.Revision)
	})
}

type ChildRequest struct {
	ID      domain.ID
	Kind    string
	Keys    []domain.CorrelationKey
	Context domain.Payload
	EventID domain.ID
}

// CreateChild derives ancestry from the leased execution's immutable trigger;
// request data cannot choose a parent Scope or causal root.
func (r *Router) CreateChild(ctx context.Context, ref domain.LeaseRef, request ChildRequest) (out domain.Work, err error) {
	var rejected error
	err = r.transact(ctx, &ref, func(tx storage.Tx) error {
		x, e := tx.Execution(ref.ExecutionID)
		if e != nil {
			return e
		}
		parent, e := tx.Work(x.WorkID)
		if e != nil {
			return e
		}
		if parent.State.Terminal() {
			return storage.ErrInvalid
		}
		trigger, e := executionTriggerInTx(tx, x)
		if e != nil {
			return e
		}
		out, e = r.createLinked(tx, parent.ScopeID, parent.ID, "", trigger, request, &rejected)
		return e
	})
	if err != nil {
		return domain.Work{}, err
	}
	if rejected != nil {
		return domain.Work{}, rejected
	}
	return
}

// CreateSuccessor is trusted control-plane policy, normally called by a route
// policy or authorized management action. It preserves terminal ownership and
// requires at least one distinct generation-qualified external identity. The
// caller supplies the accepted late/replay trigger, never a synthetic new root.
func (r *Router) CreateSuccessor(ctx context.Context, predecessor, trigger domain.ID, request ChildRequest) (out domain.Work, err error) {
	var rejected error
	err = r.transact(ctx, nil, func(tx storage.Tx) error {
		old, e := tx.Work(predecessor)
		if e != nil {
			return e
		}
		if !old.State.Terminal() || len(request.Keys) == 0 {
			return storage.ErrInvalid
		}
		a, e := tx.Event(trigger)
		if e != nil {
			return e
		}
		acceptedScope, e := scopeForEvent(tx, a)
		if e != nil {
			return e
		}
		if acceptedScope != old.ScopeID {
			return storage.ErrConflict
		}
		owner, e := routeOwner(tx, a.Event)
		if e != nil {
			return e
		}
		if owner.ID != old.ID {
			return storage.ErrInvalid
		}
		for _, key := range request.Keys {
			if containsKey(old, key) {
				return storage.ErrInvalid
			}
		}
		out, e = r.createLinked(tx, old.ScopeID, "", old.ID, trigger, request, &rejected)
		return e
	})
	if err != nil {
		return domain.Work{}, err
	}
	if rejected != nil {
		return domain.Work{}, rejected
	}
	return
}
func (r *Router) createLinked(tx storage.Tx, scope, parent, predecessor, trigger domain.ID, request ChildRequest, rejected *error) (domain.Work, error) {
	if request.ID == "" || request.Kind == "" {
		return domain.Work{}, storage.ErrInvalid
	}
	eventID := request.EventID
	if eventID == "" {
		eventID = domain.ID("work-created/" + identity(string(request.ID)))
	}
	data, err := json.Marshal(struct{ WorkID, ParentID, PredecessorID domain.ID }{request.ID, parent, predecessor})
	if err != nil {
		return domain.Work{}, err
	}
	now := r.now()
	event := domain.Event{ID: eventID, Type: "core.work.created", Source: "core.work", DeliveryID: "created/" + string(request.ID), OccurredAt: now, ReceivedAt: now, CausationID: trigger, WorkID: request.ID, Data: domain.Payload{Schema: "core.work.created.v1", Data: data}}
	if err = CheckDerivedInTx(tx, now, r.options.CausalLimits, trigger, event); err != nil {
		if errors.Is(err, ErrCausalRejected) {
			*rejected = err
			return domain.Work{}, nil
		}
		return domain.Work{}, err
	}
	old, e := tx.Work(request.ID)
	if e == nil {
		if old.ScopeID != scope || old.ParentID != parent || old.Kind != request.Kind {
			return domain.Work{}, storage.ErrConflict
		}
		// Require the creation envelope to prove this is the same linked operation.
		prior, e := tx.Event(eventID)
		if e != nil {
			return domain.Work{}, storage.ErrConflict
		}
		if prior.Event.CausationID != trigger || prior.Event.WorkID != old.ID || string(prior.Event.Data.Data) != string(data) {
			return domain.Work{}, storage.ErrConflict
		}
		return old, nil
	}
	if !errors.Is(e, storage.ErrNotFound) {
		return domain.Work{}, e
	}
	work := domain.Work{ID: request.ID, Kind: request.Kind, ScopeID: scope, ParentID: parent, State: domain.WorkReady, CorrelationKeys: append([]domain.CorrelationKey(nil), request.Keys...), Context: request.Context}
	if err = tx.PutWork(work, 0); err != nil {
		return domain.Work{}, err
	}
	if _, _, err = AcceptDerivedInTx(tx, now, r.options.CausalLimits, scope, trigger, event); err != nil {
		return domain.Work{}, err
	}
	return tx.Work(work.ID)
}
