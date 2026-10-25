package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

const controlNamespace = "core.scheduler.controls"

var ErrControlBusy = errors.New("control intent is being dispatched")
var ErrControlDispatch = errors.New("control dispatch failed or was ambiguous")

type ControlState struct {
	IntentID, Status, Reason, EffectivePolicy string
	Attempts                                  uint64
	Fenced                                    bool
	Token                                     string
	LeaseUntil                                time.Time
}

func controlState(tx storage.Tx, id string) (ControlState, error) {
	var state ControlState
	err := getJSON(tx, controlNamespace, id, &state)
	return state, err
}
func (s *Scheduler) ControlStatus(ctx context.Context, id string) (state ControlState, err error) {
	err = s.transact(ctx, nil, func(tx storage.Tx) error { var er error; state, er = controlState(tx, id); return er })
	return
}

// DispatchControl retries only the persisted target/epoch and original Event
// identity. Executor I/O occurs after the dispatch claim commits. Inject success
// does not consume input. Interrupt fences first and keeps the physical resource
// hold until ConfirmStopped or observation proves the old process has finished.
func (s *Scheduler) DispatchControl(ctx context.Context, id string) error {
	var intent ControlIntent
	var state ControlState
	var attempt AttemptState
	var event domain.Event
	var executor extension.Executor
	var dispatch bool
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		if err := getJSON(tx, intentNamespace, id, &intent); err != nil {
			return err
		}
		x, err := tx.Execution(intent.ExecutionID)
		if err != nil {
			return err
		}
		attempt, err = attemptState(tx, x.ID)
		if err != nil {
			return err
		}
		state, err = controlState(tx, id)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if state.Status == "accepted" || state.Status == "stale" || state.Status == "fallback" {
			return nil
		}
		now := s.now()
		if state.Status == "dispatching" && now.Before(state.LeaseUntil) {
			return ErrControlBusy
		}
		if x.Lease.Epoch != intent.Epoch || attempt.Epoch != intent.Epoch || x.State != domain.ExecutionRunning && !(intent.Policy == "interrupt" && state.Fenced && x.State == domain.ExecutionCancelled) {
			state.IntentID = id
			state.Status = "stale"
			state.Reason = "target_finished_or_superseded"
			return putJSON(tx, controlNamespace, id, state)
		}
		if !state.Fenced && (x.Lease.Owner == "" || !now.Before(x.Lease.ExpiresAt)) {
			state.IntentID = id
			state.Status = "stale"
			state.Reason = "target_authority_lost"
			return putJSON(tx, controlNamespace, id, state)
		}
		executor, err = s.executorFor(attempt)
		if err != nil {
			return err
		}
		a, err := tx.Event(intent.EventID)
		if err != nil {
			return err
		}
		event = a.Event
		switch intent.Policy {
		case "inject":
			if _, ok := executor.(extension.InjectExecutor); !ok || !attempt.Capabilities.Inject {
				return extension.ErrUnsupported
			}
		case "interrupt":
			if _, ok := executor.(extension.InterruptExecutor); !ok || !attempt.Capabilities.Interrupt {
				return extension.ErrUnsupported
			}
			if !state.Fenced {
				work, er := tx.Work(x.WorkID)
				if er != nil {
					return er
				}
				x.State = domain.ExecutionCancelled
				x.Outcome = nil
				if er = tx.PutExecution(x, x.Revision); er != nil {
					return er
				}
				if !work.State.Terminal() {
					work.State = domain.WorkReady
					if er = tx.PutWork(work, work.Revision); er != nil {
						return er
					}
				}
				attempt.Excluded = false
				if er = putAttemptState(tx, attempt); er != nil {
					return er
				}
				state.Fenced = true
			}
		default:
			return storage.ErrInvalid
		}
		token, err := randomToken()
		if err != nil {
			return err
		}
		state.IntentID = id
		state.Status = "dispatching"
		state.Reason = ""
		state.EffectivePolicy = intent.Policy
		state.Attempts++
		state.Token = token
		state.LeaseUntil = now.Add(s.options.LeaseDuration)
		dispatch = true
		return putJSON(tx, controlNamespace, id, state)
	})
	if err != nil || !dispatch {
		return err
	}
	var callErr error
	if intent.Policy == "inject" {
		callErr = executor.(extension.InjectExecutor).Inject(ctx, intent.ExecutionID, event)
	} else {
		callErr = executor.(extension.InterruptExecutor).Interrupt(ctx, intent.ExecutionID)
	}
	// Commit dispatch acknowledgement independently of caller cancellation. The
	// durable claim remains retryable if this transaction cannot finish.
	commitErr := s.transact(context.WithoutCancel(ctx), nil, func(tx storage.Tx) error {
		current, err := controlState(tx, id)
		if err != nil {
			return err
		}
		if current.Token != state.Token || !s.now().Before(current.LeaseUntil) {
			return ErrControlBusy
		}
		x, err := tx.Execution(intent.ExecutionID)
		if err != nil {
			return err
		}
		if x.Lease.Epoch != intent.Epoch || intent.Policy == "inject" && (x.State != domain.ExecutionRunning || x.Lease.Owner == "" || !s.now().Before(x.Lease.ExpiresAt)) {
			current.Status = "stale"
			current.Reason = "target_finished_or_superseded"
		} else if callErr == nil {
			current.Status = "accepted"
			current.Reason = "accepted_not_consumed"
			if intent.Policy == "interrupt" {
				current.Reason = "stop_confirmation_required"
			}
		} else {
			current.Status = "retryable"
			current.Reason = "dispatch_failed_or_ambiguous"
			fallback := attempt.ControlErrorFallback
			if fallback != "" {
				current.Status = "fallback"
				current.Reason = "configured_dispatch_error_fallback"
				current.EffectivePolicy = fallback
				if fallback == "drop" {
					a, er := tx.Event(intent.EventID)
					if er != nil {
						return er
					}
					if er = tx.Delete(queueNamespace, queueKey(intent.WorkID, a.Sequence)); er != nil {
						return er
					}
				}
			}
		}
		current.Token = ""
		current.LeaseUntil = time.Time{}
		return putJSON(tx, controlNamespace, id, current)
	})
	if commitErr != nil {
		return commitErr
	}
	if callErr != nil {
		return ErrControlDispatch
	}
	return nil
}

func (s *Scheduler) handoffContinuationInTx(tx storage.Tx, x domain.Execution, now time.Time) error {
	work, err := tx.Work(x.WorkID)
	if err != nil {
		return err
	}
	trigger, err := executionTriggerInTx(tx, x)
	if err != nil {
		return err
	}
	event := domain.Event{ID: domain.ID("handoff-event/" + identity(string(x.ID))), Type: "core.execution.handoff", Source: "core.execution", DeliveryID: "handoff/" + string(x.ID), CausationID: trigger, WorkID: x.WorkID, ExecutionID: x.ID, OccurredAt: now, ReceivedAt: now}
	accepted, _, err := AcceptDerivedInTx(tx, now, s.options.CausalLimits, work.ScopeID, trigger, event)
	if errors.Is(err, ErrCausalRejected) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = putJSON(tx, queueNamespace, queueKey(work.ID, accepted.Sequence), QueuedInput{EventID: accepted.Event.ID, Sequence: accepted.Sequence, Binding: x.ExecutorBinding}); err != nil {
		return err
	}
	return tx.MarkRouted(accepted.Event.ID, work.ID, "handoff")
}

// consumeInjectedInTx applies an executor's consumption proof for injected
// input. Only an Event with an accepted inject intent targeting this execution
// (at any of its lease epochs) and still pending for its Work is consumed, with
// the same durable consumed-input history as a claim; anything else is
// ignored, so the executor's claim cannot remove input it was never given.
func consumeInjectedInTx(tx storage.Tx, x domain.Execution, ids []domain.ID) error {
	for _, id := range ids {
		injected := false
		for epoch := uint64(1); epoch <= x.Lease.Epoch && !injected; epoch++ {
			intentID := identity(string(id), string(x.ID), fmt.Sprint(epoch), "inject")
			var intent ControlIntent
			if err := getJSON(tx, intentNamespace, intentID, &intent); err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return err
			}
			state, err := controlState(tx, intentID)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return err
			}
			injected = intent.ExecutionID == x.ID && intent.WorkID == x.WorkID && intent.Policy == "inject" && state.Status == "accepted"
		}
		if !injected {
			continue
		}
		a, err := tx.Event(id)
		if err != nil {
			return err
		}
		key := queueKey(x.WorkID, a.Sequence)
		var input QueuedInput
		if err = getJSON(tx, queueNamespace, key, &input); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return err
		}
		if input.EventID != id {
			continue
		}
		if err = putJSON(tx, consumedNamespace, key+"/"+identity(string(x.ID)), struct {
			Input       QueuedInput
			ExecutionID domain.ID
		}{input, x.ID}); err != nil {
			return err
		}
		if err = tx.Delete(queueNamespace, key); err != nil {
			return err
		}
	}
	return nil
}
