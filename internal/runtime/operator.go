package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Operator controls added for engineering operation (03-05): binding
// repair, scheduling holds (used while an operator attaches to a session)
// and on-demand reconcile. Each commits one accepted core.manual.* control
// Event in the same transaction as its state change, so history and causal
// chains explain it; none launches an executor or forces a wake, except
// that releasing a hold the causal limits placed rearms the blocked wait.

const holdNamespace = "core.manual.hold"

// ErrHeld reports that Work is (or is not) held contrary to the request.
var ErrHeld = errors.New("operator hold state conflict")

// Hold is an operator scheduling hold: while it exists no attempt starts or
// resumes for the Work. Input keeps queueing and waits keep resolving, so
// the next attempt sees everything that arrived while held.
type Hold struct {
	EventID domain.ID `json:"event_id"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
	// Kind says what placed a hold the runtime placed itself: "budget"
	// (HoldKindBudget) for Work over a budget, "gate" (HoldKindGate) for
	// Work a start gate held. "" is an operator's hold or a blocked wait's.
	Kind string `json:"kind,omitempty"`
	// Holder is the registration whose start gate placed a gate hold.
	Holder string `json:"holder,omitempty"`
}

// HoldInTx returns a Work's operator hold, if any.
func HoldInTx(tx storage.Tx, work domain.ID) (Hold, bool, error) {
	var h Hold
	err := getJSON(tx, holdNamespace, identity(string(work)), &h)
	if errors.Is(err, storage.ErrNotFound) {
		return Hold{}, false, nil
	}
	return h, err == nil, err
}

func heldInTx(tx storage.Tx, work domain.ID) error {
	_, held, err := HoldInTx(tx, work)
	if err != nil {
		return err
	}
	if held {
		return ErrNotEligible
	}
	return nil
}

type operatorPayload struct {
	Reason     string          `json:"reason,omitempty"`
	Revision   domain.Revision `json:"revision"`
	Binding    string          `json:"binding,omitempty"`
	Previous   []string        `json:"previous,omitempty"`
	Generation string          `json:"generation,omitempty"`
	Sources    []string        `json:"sources,omitempty"`
}

func operatorData(schema string, p operatorPayload) domain.Payload {
	b, _ := json.Marshal(p)
	return domain.Payload{Schema: schema, Data: b}
}

// controlEvent accepts a core.manual.* control Event addressed to Work and
// marks it routed as control, so it never becomes runnable input.
func (c *Control) controlEvent(ctx context.Context, tx storage.Tx, w domain.Work, delivery, typ string, data domain.Payload) (domain.ID, error) {
	accepted, _, err := c.acceptInTx(ctx, tx, w.ScopeID, ManualEvent{WorkID: w.ID, DeliveryID: delivery, Data: data}, typ)
	if err != nil {
		return "", err
	}
	return accepted.Event.ID, tx.MarkRouted(accepted.Event.ID, "", "control")
}

// noLiveAttempt refuses Work with a pending or running attempt; parked
// attempts are allowed only when allowParked.
func noLiveAttempt(tx storage.Tx, work domain.ID, allowParked bool) error {
	xs, err := tx.ListExecutions(work)
	if err != nil {
		return err
	}
	for _, x := range xs {
		if x.State.Terminal() || allowParked && x.State == domain.ExecutionParked {
			continue
		}
		return ErrNotEligible
	}
	return nil
}

// Rebind changes the binding of ready Work's queued input (and of its retry
// state) to another enabled binding of the Work's scope. It is the explicit
// repair for input whose pinned binding a reload removed or disabled. Only
// ready Work with no live or parked attempt may be rebound; Work never moves
// scope. The control Event records the binding, the previous bindings and the
// scheduler's current configuration generation.
func (c *Control) Rebind(ctx context.Context, workID domain.ID, expected domain.Revision, binding, reason, key string) (event domain.ID, err error) {
	if !validDelivery(key) || binding == "" {
		return "", storage.ErrInvalid
	}
	delivery := "rebind/" + key
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
		acted, ok := ActOnRevision(w.Revision, expected)
		if !ok {
			return ErrRevision
		}
		if w.State != domain.WorkReady {
			return storage.ErrConflict
		}
		if e = noLiveAttempt(tx, w.ID, false); e != nil {
			return e
		}
		if _, e = effectiveBinding(tx, w.ScopeID, binding); e != nil {
			if errors.Is(e, extension.ErrInvalid) {
				return storage.ErrInvalid
			}
			return e
		}
		inputs, e := pendingInputs(tx, w.ID)
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		var previous []string
		for _, in := range inputs {
			if !seen[in.Binding] {
				seen[in.Binding] = true
				previous = append(previous, in.Binding)
			}
		}
		event, e = c.controlEvent(ctx, tx, w, delivery, "core.manual.rebind", operatorData("core.manual.rebind.v1", operatorPayload{Reason: reason, Revision: acted, Binding: binding, Previous: previous, Generation: c.scheduler.options.Generation}))
		if e != nil {
			return e
		}
		for _, in := range inputs {
			in.Binding = binding
			if e = putJSON(tx, queueNamespace, queueKey(w.ID, in.Sequence), in); e != nil {
				return e
			}
		}
		after := rebound{Binding: binding}
		if latest, er := latestAttemptInTx(tx, w.ID); er != nil {
			return er
		} else if latest != nil {
			after.After = latest.Attempt
		}
		if e = putJSON(tx, rebindNamespace, identity(string(w.ID)), after); e != nil {
			return e
		}
		if state, er := retryState(tx, w.ID); er == nil {
			state.Binding = binding
			if e = putJSON(tx, retryNamespace, identity(string(w.ID)), state); e != nil {
				return e
			}
		} else if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		// Advance the revision so a concurrent control sees the change.
		return tx.PutWork(w, w.Revision)
	})
	return
}

// HoldWork places an operator scheduling hold on nonterminal Work that has
// no pending or running attempt (a parked attempt stays parked and cannot
// resume while held).
func (c *Control) HoldWork(ctx context.Context, workID domain.ID, expected domain.Revision, reason, key string) (event domain.ID, err error) {
	if !validDelivery(key) {
		return "", storage.ErrInvalid
	}
	delivery := "hold/" + key
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
		acted, ok := ActOnRevision(w.Revision, expected)
		if !ok {
			return ErrRevision
		}
		if w.State.Terminal() || w.State == domain.WorkRunning {
			return storage.ErrConflict
		}
		if _, held, e := HoldInTx(tx, w.ID); e != nil {
			return e
		} else if held {
			return ErrHeld
		}
		if e = noLiveAttempt(tx, w.ID, true); e != nil {
			return e
		}
		event, e = c.controlEvent(ctx, tx, w, delivery, "core.manual.hold", operatorData("core.manual.hold.v1", operatorPayload{Reason: reason, Revision: acted}))
		if e != nil {
			return e
		}
		if e = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: event, Reason: reason, At: c.now().UTC()}); e != nil {
			return e
		}
		return tx.PutWork(w, w.Revision)
	})
	return
}

// ReleaseWork removes an operator hold; queued input then starts normally.
func (c *Control) ReleaseWork(ctx context.Context, workID domain.ID, expected domain.Revision, reason, key string) (event domain.ID, err error) {
	if !validDelivery(key) {
		return "", storage.ErrInvalid
	}
	delivery := "release/" + key
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
		acted, ok := ActOnRevision(w.Revision, expected)
		if !ok {
			return ErrRevision
		}
		h, held, e := HoldInTx(tx, w.ID)
		if e != nil {
			return e
		} else if !held {
			return ErrHeld
		} else if h.Kind == HoldKindGate {
			return ErrGateHold
		}
		event, e = c.controlEvent(ctx, tx, w, delivery, "core.manual.release", operatorData("core.manual.release.v1", operatorPayload{Reason: reason, Revision: acted}))
		if e != nil {
			return e
		}
		// Releasing a budget hold lets the next turn start past the limits
		// that held it; the one after is checked again.
		if h.Kind == HoldKindBudget {
			if e = passBudgetInTx(tx, w.ID, event); e != nil {
				return e
			}
		}
		if e = tx.Delete(holdNamespace, identity(string(w.ID))); e != nil {
			return e
		}
		if w.State.Terminal() {
			return nil
		}
		if e = tx.PutWork(w, w.Revision); e != nil {
			return e
		}
		// A wait the causal limits blocked held the Work; releasing it lets
		// that wait wake again (on what has already arrived, if anything).
		return unblockWaitsInTx(tx, c.now(), c.limits, w.ID, event)
	})
	return
}

// RequestReconcile accepts an operator core.manual.reconcile Event in the
// scope and makes every ingress source registered for that scope due now.
// The scan itself is the normal asynchronous reconcile; the returned source
// names are those made due. A generation already under way when the request
// commits completes and leaves the source due, so a fresh generation always
// starts after the request.
func (c *Control) RequestReconcile(ctx context.Context, scopeID domain.ID, expected domain.Revision, key string) (event domain.ID, sources []string, err error) {
	if !validDelivery(key) || scopeID == "" {
		return "", nil, storage.ErrInvalid
	}
	delivery := "reconcile/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			if e != nil {
				return e
			}
			if s, er := scopeForEvent(tx, old); er != nil || s != scopeID {
				return storage.ErrConflict
			}
			event = old.Event.ID
			var p operatorPayload
			_ = json.Unmarshal(old.Event.Data.Data, &p)
			sources = p.Sources
			return nil
		}
		scope, e := tx.Scope(scopeID)
		if e != nil {
			return e
		}
		acted, ok := ActOnRevision(scope.Revision, expected)
		if !ok {
			return ErrRevision
		}
		rows, e := tx.List("core.ingress.registration", "")
		if e != nil {
			return e
		}
		now := c.now()
		var states []ingressState
		for _, row := range rows {
			var source extension.Source
			if e = getJSON(tx, "core.ingress.registration", row.Key, &source); e != nil {
				return e
			}
			if source.ScopeID != scopeID {
				continue
			}
			var state ingressState
			if e = getJSON(tx, sourceNamespace(source), "state", &state); e != nil {
				if errors.Is(e, storage.ErrNotFound) {
					continue
				}
				return e
			}
			sources = append(sources, state.Name)
			states = append(states, state)
		}
		accepted, _, e := c.acceptInTx(ctx, tx, scopeID, ManualEvent{DeliveryID: delivery, Data: operatorData("core.manual.reconcile.v1", operatorPayload{Revision: acted, Sources: sources})}, "core.manual.reconcile")
		if e != nil {
			return e
		}
		event = accepted.Event.ID
		if e = tx.MarkRouted(event, "", "control"); e != nil {
			return e
		}
		for _, state := range states {
			// Counting the request lets a scan already under way finish and
			// then run again, so the request sees state at least as new as
			// itself.
			state.Requests++
			if state.NextDue.After(now) {
				state.NextDue = now
			}
			if e = putJSON(tx, sourceNamespace(state.Source), "state", state); e != nil {
				return e
			}
		}
		return nil
	})
	return
}

// MaxAttaches bounds the attach containers an attempt's history keeps.
const MaxAttaches = 20

// RecordAttachInTx records an operator's attach container with the
// attempt it attached to, for history, before the container starts.
func RecordAttachInTx(tx storage.Tx, execution domain.ID, pin extension.ContainerPin) error {
	state, err := attemptState(tx, execution)
	if err != nil {
		return err
	}
	state.Attaches = append(state.Attaches, pin)
	if n := len(state.Attaches); n > MaxAttaches {
		state.Attaches = state.Attaches[n-MaxAttaches:]
	}
	return putAttemptState(tx, state)
}

// AttemptStateInTx exposes an attempt's trusted scheduling record (executor
// registration, binding and generation) to composition and management.
func AttemptStateInTx(tx storage.Tx, execution domain.ID) (AttemptState, error) {
	return attemptState(tx, execution)
}

// ErrInterruptUnsupported reports that the running attempt's executor does
// not advertise interrupt, so an operator's interrupt is refused rather
// than turned into a cancel.
var ErrInterruptUnsupported = errors.New("the running attempt's executor does not support interrupt")

// InterruptWork stops running Work's current attempt without ending the
// Work, through the same interrupt dispatch a route with policy interrupt
// uses. It commits a core.manual.interrupt control Event and an interrupt
// intent against the running attempt and its lease epoch; dispatch fences
// the attempt (cancelled, with the Event recorded as its InterruptEventID),
// leaves the Work ready with its queued input kept, and calls the
// executor's Interrupt. The next attempt starts with that input; with none
// queued, the Work stays ready for its next input. It fails closed: Work
// whose running attempt was not pinned with interrupt, or whose executor
// does not implement it, is refused with ErrInterruptUnsupported, and a
// failed dispatch never takes the binding's control_error_fallback.
// Replaying the same key returns the original Event and attempt.
func (c *Control) InterruptWork(ctx context.Context, workID domain.ID, expected domain.Revision, reason, key string) (event, execution domain.ID, err error) {
	if !validDelivery(key) {
		return "", "", storage.ErrInvalid
	}
	delivery := "interrupt/" + key
	var intentID string
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			if e != nil {
				return e
			}
			if old.Event.WorkID != workID {
				return storage.ErrConflict
			}
			var p controlPayload
			_ = json.Unmarshal(old.Event.Data.Data, &p)
			event, execution = old.Event.ID, p.ExecutionID
			return nil
		}
		w, e := tx.Work(workID)
		if e != nil {
			return e
		}
		acted, ok := ActOnRevision(w.Revision, expected)
		if !ok {
			return ErrRevision
		}
		if w.State != domain.WorkRunning {
			return storage.ErrConflict
		}
		xs, e := tx.ListExecutions(w.ID)
		if e != nil {
			return e
		}
		var target *domain.Execution
		for i := range xs {
			if xs[i].State == domain.ExecutionRunning {
				target = &xs[i]
			}
		}
		if target == nil {
			return storage.ErrConflict
		}
		attempt, e := attemptState(tx, target.ID)
		if errors.Is(e, storage.ErrNotFound) {
			return storage.ErrConflict
		} else if e != nil {
			return e
		}
		executor, e := c.scheduler.executorFor(attempt)
		if e != nil {
			return ErrInterruptUnsupported
		}
		if _, ok := executor.(extension.InterruptExecutor); !ok || !attempt.Capabilities.Interrupt {
			return ErrInterruptUnsupported
		}
		accepted, _, e := c.acceptInTx(ctx, tx, w.ScopeID, ManualEvent{WorkID: w.ID, DeliveryID: delivery, Data: controlData("core.manual.interrupt.v1", reason, target.ID, acted)}, "core.manual.interrupt")
		if e != nil {
			return e
		}
		event, execution = accepted.Event.ID, target.ID
		if e = tx.MarkRouted(event, "", "control"); e != nil {
			return e
		}
		intent := ControlIntent{ID: identity(string(event), string(target.ID), fmt.Sprint(target.Lease.Epoch), "interrupt"), EventID: event, WorkID: w.ID, ExecutionID: target.ID, Epoch: target.Lease.Epoch, Policy: "interrupt", Binding: target.ExecutorBinding, Operator: true}
		intentID = intent.ID
		return putJSON(tx, intentNamespace, intent.ID, intent)
	})
	if err == nil && intentID != "" {
		// Dispatch now rather than on the host's next pass; a failure
		// leaves the intent for that pass, and a fenced attempt is stopped
		// by its holder either way.
		_ = c.scheduler.DispatchControl(ctx, intentID)
	}
	return
}
