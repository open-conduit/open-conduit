package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// ErrRevision reports a stale expected revision on an operator mutation.
var ErrRevision = errors.New("revision conflict")

// Control exposes trusted operator actions. Every action commits through the
// normal accepted-event path; none launches executors or forces wake state.
type Control struct {
	store     storage.Store
	now       func() time.Time
	router    *Router
	scheduler *Scheduler
	limits    CausalLimits
}

func NewControl(store storage.Store, now func() time.Time, router *Router, scheduler *Scheduler, limits CausalLimits) (*Control, error) {
	if store == nil || now == nil || router == nil || scheduler == nil {
		return nil, storage.ErrInvalid
	}
	limits, err := normalizeCausalLimits(limits)
	if err != nil {
		return nil, err
	}
	return &Control{store, now, router, scheduler, limits}, nil
}

// ManualEvent is an operator-submitted event. Source, IDs and times are
// assigned by core; a CausationID makes it derived from a readable accepted
// parent, otherwise it is an explicit operator root.
type ManualEvent struct {
	ScopeID, WorkID, CausationID domain.ID
	Type, DeliveryID             string
	Data                         domain.Payload
	CorrelationKey               *domain.CorrelationKey
}

const ManualSource = "core.manual"

func manualEventID(delivery string) domain.ID {
	return domain.ID("manual/" + identity(delivery))
}
func (c *Control) transact(ctx context.Context, fn func(storage.Tx) error) error {
	return c.store.Transact(ctx, storage.Guard{Now: c.now()}, fn)
}
func validManualType(t string) bool { return validType(t) && !strings.HasPrefix(t, "core.") }
func validDelivery(d string) bool   { return d != "" && len(d) <= 256 && !strings.ContainsAny(d, "\r\n") }

// prior returns an already accepted manual control/event delivery.
func prior(tx storage.Tx, delivery string) (domain.AcceptedEvent, bool, error) {
	return deliveryDuplicate(tx, domain.Event{Source: ManualSource, DeliveryID: delivery})
}

func (c *Control) acceptInTx(tx storage.Tx, scope domain.ID, m ManualEvent, typ string) (domain.AcceptedEvent, bool, error) {
	now := c.now()
	e := domain.Event{ID: manualEventID(m.DeliveryID), Type: typ, Source: ManualSource, DeliveryID: m.DeliveryID, OccurredAt: now, ReceivedAt: now, CorrelationKey: m.CorrelationKey, WorkID: m.WorkID, Data: m.Data}
	if m.CausationID != "" {
		parent, err := tx.Event(m.CausationID)
		if err != nil {
			return domain.AcceptedEvent{}, false, err
		}
		parentScope, err := scopeForEvent(tx, parent)
		if err != nil {
			return domain.AcceptedEvent{}, false, err
		}
		if parentScope != scope {
			return domain.AcceptedEvent{}, false, storage.ErrNotFound
		}
		return AcceptDerivedInTx(tx, now, c.limits, scope, m.CausationID, e)
	}
	return AcceptInTx(tx, scope, e)
}

// SubmitEvent accepts an operator event; duplicates return the original.
func (c *Control) SubmitEvent(ctx context.Context, m ManualEvent) (accepted domain.AcceptedEvent, added bool, err error) {
	if m.ScopeID == "" || !validManualType(m.Type) || !validDelivery(m.DeliveryID) {
		return accepted, false, storage.ErrInvalid
	}
	// Operator deliveries share core.manual with control actions; namespace them.
	m.DeliveryID = "event/" + m.DeliveryID
	var refusal error
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, m.DeliveryID); e != nil || dup {
			accepted = old
			if e == nil {
				if s, er := scopeForEvent(tx, old); er != nil || s != m.ScopeID || old.Event.Type != m.Type {
					return storage.ErrConflict
				}
			}
			return e
		}
		if _, e := tx.Scope(m.ScopeID); e != nil {
			return e
		}
		if m.WorkID != "" {
			w, e := tx.Work(m.WorkID)
			if e != nil {
				return e
			}
			if w.ScopeID != m.ScopeID {
				return storage.ErrNotFound
			}
		}
		var e error
		accepted, added, e = c.acceptInTx(tx, m.ScopeID, m, m.Type)
		if errors.Is(e, ErrCausalRejected) {
			refusal = e
			return nil
		}
		return e
	})
	if err == nil && refusal != nil {
		return domain.AcceptedEvent{}, false, refusal
	}
	if err == nil && added {
		c.routeBestEffort(ctx, accepted.Event.ID)
		_ = c.transact(ctx, func(tx storage.Tx) error {
			a, e := tx.Event(accepted.Event.ID)
			if e == nil {
				accepted = a
			}
			return e
		})
	}
	return
}

// routeBestEffort routes immediately after commit; a failure leaves the event
// pending for the host's normal routing discovery.
func (c *Control) routeBestEffort(ctx context.Context, id domain.ID) {
	_ = c.router.Route(ctx, id)
}

type ManualWork struct {
	ScopeID, ParentID domain.ID
	Kind, Binding     string
	DeliveryID        string
	Context           domain.Payload
	Keys              []domain.CorrelationKey
}

// CreateWork atomically creates ready Work, its accepted core.work.created
// event and the explicitly bound queued input. It never launches directly.
func (c *Control) CreateWork(ctx context.Context, m ManualWork) (work domain.Work, created domain.AcceptedEvent, err error) {
	if m.ScopeID == "" || m.Kind == "" || m.Binding == "" || !validDelivery(m.DeliveryID) {
		return work, created, storage.ErrInvalid
	}
	id := domain.ID("manual/" + identity("work", m.DeliveryID))
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, "work/"+m.DeliveryID); e != nil || dup {
			if e != nil {
				return e
			}
			w, e := tx.Work(id)
			if e != nil {
				return e
			}
			if w.ScopeID != m.ScopeID || w.Kind != m.Kind || w.ParentID != m.ParentID {
				return storage.ErrConflict
			}
			work, created = w, old
			return nil
		}
		if _, e := effectiveBinding(tx, m.ScopeID, m.Binding); e != nil {
			return e
		}
		if m.ParentID != "" {
			parent, e := tx.Work(m.ParentID)
			if e != nil {
				return e
			}
			if parent.ScopeID != m.ScopeID {
				return storage.ErrNotFound
			}
		}
		w := domain.Work{ID: id, Kind: m.Kind, ScopeID: m.ScopeID, ParentID: m.ParentID, State: domain.WorkReady, CorrelationKeys: append([]domain.CorrelationKey(nil), m.Keys...), Context: m.Context}
		if e := tx.PutWork(w, 0); e != nil {
			return e
		}
		data, e := json.Marshal(struct{ WorkID, ParentID domain.ID }{id, m.ParentID})
		if e != nil {
			return e
		}
		created, _, e = c.acceptInTx(tx, m.ScopeID, ManualEvent{WorkID: id, DeliveryID: "work/" + m.DeliveryID, Data: domain.Payload{Schema: "core.work.created.v1", Data: data}}, "core.work.created")
		if e != nil {
			return e
		}
		if e = putJSON(tx, queueNamespace, queueKey(id, created.Sequence), QueuedInput{EventID: created.Event.ID, Sequence: created.Sequence, Binding: m.Binding}); e != nil {
			return e
		}
		if e = tx.MarkRouted(created.Event.ID, id, "manual"); e != nil {
			return e
		}
		if created, e = tx.Event(created.Event.ID); e != nil {
			return e
		}
		work, e = tx.Work(id)
		return e
	})
	return
}

type controlPayload struct {
	Reason      string          `json:"reason"`
	ExecutionID domain.ID       `json:"execution_id,omitempty"`
	Revision    domain.Revision `json:"expected_revision"`
}

func controlData(schema, reason string, execution domain.ID, rev domain.Revision) domain.Payload {
	b, _ := json.Marshal(controlPayload{Reason: reason, ExecutionID: execution, Revision: rev})
	return domain.Payload{Schema: schema, Data: b}
}

// CancelWork terminalizes Work, fencing attempts, Waits, timers and queued
// input with an accepted core.manual.cancel control event in one transaction.
// Physical process stop is dispatched by the host outside the transaction.
func (c *Control) CancelWork(ctx context.Context, workID domain.ID, expected domain.Revision, reason, key string) (event domain.ID, err error) {
	if !validDelivery(key) {
		return "", storage.ErrInvalid
	}
	delivery := "cancel/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			if e == nil && old.Event.WorkID != workID {
				return storage.ErrConflict
			}
			event = old.Event.ID
			return e
		}
		w, e := tx.Work(workID)
		if e != nil {
			return e
		}
		if w.Revision != expected {
			return ErrRevision
		}
		if w.State.Terminal() {
			return storage.ErrConflict
		}
		// Control checkpoints keep Work in the envelope but use an empty routed
		// association so earlier runnable input cannot delay fencing.
		accepted, _, e := c.acceptInTx(tx, w.ScopeID, ManualEvent{WorkID: w.ID, DeliveryID: delivery, Data: controlData("core.manual.cancel.v1", reason, "", expected)}, "core.manual.cancel")
		if e != nil {
			return e
		}
		event = accepted.Event.ID
		if e = tx.MarkRouted(event, "", "control"); e != nil {
			return e
		}
		return cancelWorkInTx(tx, w.ID)
	})
	return
}

// CancelExecution fences one attempt and holds scheduling for its Work until
// an explicit manual retry. Operator cancellation is never retried automatically.
func (c *Control) CancelExecution(ctx context.Context, executionID domain.ID, expected domain.Revision, reason, key string) (event, work domain.ID, err error) {
	if !validDelivery(key) {
		return "", "", storage.ErrInvalid
	}
	delivery := "cancel-execution/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			event, work = old.Event.ID, old.Event.WorkID
			return e
		}
		x, e := tx.Execution(executionID)
		if e != nil {
			return e
		}
		if x.Revision != expected {
			return ErrRevision
		}
		if x.State.Terminal() {
			return storage.ErrConflict
		}
		w, e := tx.Work(x.WorkID)
		if e != nil {
			return e
		}
		accepted, _, e := c.acceptInTx(tx, w.ScopeID, ManualEvent{WorkID: w.ID, DeliveryID: delivery, Data: controlData("core.manual.cancel.v1", reason, x.ID, expected)}, "core.manual.cancel")
		if e != nil {
			return e
		}
		event, work = accepted.Event.ID, w.ID
		if e = tx.MarkRouted(event, "", "control"); e != nil {
			return e
		}
		waits, e := tx.ListWaits(w.ID)
		if e != nil {
			return e
		}
		for _, wait := range waits {
			if wait.State != domain.WaitActive || wait.ExecutionID != x.ID {
				continue
			}
			wait.State = domain.WaitCancelled
			if e = disableWaitSubscriptionsInTx(tx, wait.ID); e != nil {
				return e
			}
			if e = tx.PutWait(wait, wait.Revision); e != nil {
				return e
			}
			if wait.TimeoutTimerID != "" {
				if timer, er := tx.Timer(wait.TimeoutTimerID); er == nil {
					if e = tx.DeleteTimer(timer.ID, timer.Revision); e != nil {
						return e
					}
					if e = tx.Delete(timerNamespace, timerKey(timer.ID)); e != nil {
						return e
					}
				} else if !errors.Is(er, storage.ErrNotFound) {
					return er
				}
			}
		}
		if e = CancelExecutionDeadlineInTx(tx, x.ID); e != nil {
			return e
		}
		x.State, x.Outcome = domain.ExecutionCancelled, nil
		if e = tx.PutExecution(x, x.Revision); e != nil {
			return e
		}
		if state, er := attemptState(tx, x.ID); er == nil {
			// Physical stop is unproven until trusted ConfirmStopped evidence.
			state.LaunchState, state.Declaration = "cancelled", nil
			if e = putAttemptState(tx, state); e != nil {
				return e
			}
		} else if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		if w, e = tx.Work(w.ID); e != nil {
			return e
		}
		if w.State == domain.WorkRunning || w.State == domain.WorkWaiting {
			w.State = domain.WorkReady
			if e = tx.PutWork(w, w.Revision); e != nil {
				return e
			}
		}
		state, er := retryState(tx, w.ID)
		if er != nil && !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		state.WorkID, state.FailedExecutionID, state.Binding = w.ID, x.ID, x.ExecutorBinding
		state.FailureSequence, state.Blocked, state.BlockedReason = accepted.Sequence, true, "operator_cancelled"
		return putJSON(tx, retryNamespace, identity(string(w.ID)), state)
	})
	return
}

// RetryWork accepts core.manual.retry for ready Work with no live or parked
// attempt after a failure/exhaustion/operator cancellation, then lets retry
// policy queue it. Replaying the same key completes an interrupted request.
func (c *Control) RetryWork(ctx context.Context, workID domain.ID, expected domain.Revision, reason, key string) (event domain.ID, err error) {
	if !validDelivery(key) {
		return "", storage.ErrInvalid
	}
	delivery := "retry/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			if e == nil && old.Event.WorkID != workID {
				return storage.ErrConflict
			}
			event = old.Event.ID
			return e
		}
		w, e := tx.Work(workID)
		if e != nil {
			return e
		}
		if w.Revision != expected {
			return ErrRevision
		}
		if w.State != domain.WorkReady {
			return storage.ErrConflict
		}
		if _, e = retryState(tx, w.ID); e != nil {
			if errors.Is(e, storage.ErrNotFound) {
				return ErrNotEligible
			}
			return e
		}
		xs, e := tx.ListExecutions(w.ID)
		if e != nil {
			return e
		}
		for _, x := range xs {
			if !x.State.Terminal() {
				return ErrNotEligible
			}
		}
		pending, e := PendingInputsInTx(tx, w.ID)
		if e != nil {
			return e
		}
		for _, p := range pending {
			a, er := tx.Event(p.EventID)
			if er != nil {
				return er
			}
			if a.Event.Type == "core.manual.retry" {
				return storage.ErrConflict
			}
		}
		accepted, _, e := c.acceptInTx(tx, w.ScopeID, ManualEvent{WorkID: w.ID, DeliveryID: delivery, Data: controlData("core.manual.retry.v1", reason, "", expected)}, "core.manual.retry")
		event = accepted.Event.ID
		return e
	})
	if err != nil {
		return event, err
	}
	return event, c.scheduler.ProcessRetry(ctx, event)
}

// Wake accepts an ordinary declared event addressed to waiting Work. It reports
// whether that event woke the active Wait; it never forces state.
func (c *Control) Wake(ctx context.Context, workID domain.ID, expected domain.Revision, m ManualEvent) (accepted domain.AcceptedEvent, matched bool, err error) {
	if !validManualType(m.Type) || !validDelivery(m.DeliveryID) || m.CorrelationKey == nil {
		return accepted, false, storage.ErrInvalid
	}
	m.DeliveryID = "event/" + m.DeliveryID
	var waitID domain.ID
	err = c.transact(ctx, func(tx storage.Tx) error {
		w, e := tx.Work(workID)
		if e != nil {
			return e
		}
		waits, e := tx.ListWaits(w.ID)
		if e != nil {
			return e
		}
		for _, wait := range waits {
			if wait.State == domain.WaitActive || wait.WakeEventID == manualEventID(m.DeliveryID) {
				waitID = wait.ID
			}
		}
		if old, dup, e := prior(tx, m.DeliveryID); e != nil || dup {
			accepted = old
			if e == nil && old.Event.WorkID != w.ID {
				return storage.ErrConflict
			}
			return e
		}
		if w.Revision != expected {
			return ErrRevision
		}
		if w.State != domain.WorkWaiting || waitID == "" {
			return storage.ErrConflict
		}
		if !containsKey(w, *m.CorrelationKey) {
			return storage.ErrInvalid
		}
		m.WorkID = w.ID
		accepted, _, e = c.acceptInTx(tx, w.ScopeID, m, m.Type)
		return e
	})
	if err != nil {
		return accepted, false, err
	}
	c.routeBestEffort(ctx, accepted.Event.ID)
	err = c.transact(ctx, func(tx storage.Tx) error {
		if waitID == "" {
			return nil
		}
		wait, e := tx.Wait(waitID)
		matched = e == nil && wait.WakeEventID == accepted.Event.ID
		if a, er := tx.Event(accepted.Event.ID); er == nil {
			accepted = a
		}
		return e
	})
	return
}

// EventScopeInTx returns the scope an accepted event was accepted into.
func EventScopeInTx(tx storage.Tx, a domain.AcceptedEvent) (domain.ID, error) {
	return scopeForEvent(tx, a)
}
