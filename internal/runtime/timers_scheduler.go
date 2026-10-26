package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

type executionTimeoutPayload struct {
	ExecutionID domain.ID
	Deadline    time.Time
}

func ExecutionDeadlineTimerID(id domain.ID) domain.ID {
	return domain.ID("execution-deadline/" + identity(string(id)))
}
func executionTimeoutEvent(state AttemptState, parent domain.ID) domain.Event {
	key := identity(string(state.ExecutionID), state.Deadline.UTC().Format(time.RFC3339Nano), string(parent))
	data, _ := json.Marshal(executionTimeoutPayload{state.ExecutionID, state.Deadline})
	return domain.Event{ID: domain.ID("execution-timeout/" + key), Type: "core.execution.timeout", Source: "core.execution", DeliveryID: "timeout/" + key, OccurredAt: state.Deadline, ReceivedAt: state.Deadline, WorkID: state.WorkID, ExecutionID: state.ExecutionID, CausationID: parent, Data: domain.Payload{Schema: "core.execution.timeout.v1", Data: data}}
}

// ArmExecutionDeadlineInTx pins the active segment deadline to a durable Event
// template. Recovery retains its identity; Resume's new active segment gets a new
// identity. A fired, already accepted timeout is never rearmed during recovery.
func ArmExecutionDeadlineInTx(tx storage.Tx, now time.Time, state AttemptState, ref domain.LeaseRef) error {
	if state.Deadline.IsZero() {
		return CancelExecutionDeadlineInTx(tx, ref.ExecutionID)
	}
	x, err := tx.Execution(ref.ExecutionID)
	if err != nil {
		return err
	}
	parent, err := executionTriggerInTx(tx, x)
	if err != nil {
		return err
	}
	event := executionTimeoutEvent(state, parent)
	if _, err = tx.Event(event.ID); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	id := ExecutionDeadlineTimerID(ref.ExecutionID)
	old, err := tx.Timer(id)
	var revision domain.Revision
	if err == nil {
		if old.FireAt.Equal(state.Deadline) && old.Event.ID == event.ID {
			return nil
		}
		revision = old.Revision
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err = tx.PutTimer(domain.Timer{ID: id, FireAt: state.Deadline, Event: event}, revision); err != nil {
		return err
	}
	return putJSON(tx, timerNamespace, timerKey(id), timerMetadata{ScopeID: state.ScopeID})
}
func CancelExecutionDeadlineInTx(tx storage.Tx, id domain.ID) error {
	timerID := ExecutionDeadlineTimerID(id)
	timer, err := tx.Timer(timerID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = tx.DeleteTimer(timerID, timer.Revision); err != nil {
		return err
	}
	return tx.Delete(timerNamespace, timerKey(timerID))
}
func executionTimeoutMatchesInTx(tx storage.Tx, event domain.Event, state AttemptState) (bool, error) {
	if event.Type != "core.execution.timeout" || event.Source != "core.execution" || event.ExecutionID != state.ExecutionID || state.Deadline.IsZero() {
		return false, nil
	}
	x, err := tx.Execution(state.ExecutionID)
	if err != nil {
		return false, err
	}
	parent, err := executionTriggerInTx(tx, x)
	if err != nil {
		return false, err
	}
	expected := executionTimeoutEvent(state, parent)
	return event.ID == expected.ID && event.WorkID == state.WorkID && event.CausationID == parent, nil
}

// rejectExecutionTimeoutInTx still fences expired execution authority when the
// causal event budget refuses the branch. It emits no runnable Event or retry
// Timer; a fresh authorized manual retry is required to clear this hold.
func rejectExecutionTimeoutInTx(tx storage.Tx, now time.Time, event domain.Event) error {
	if event.Type != "core.execution.timeout" || event.ExecutionID == "" {
		return nil
	}
	state, err := attemptState(tx, event.ExecutionID)
	if err != nil {
		return err
	}
	matches, err := executionTimeoutMatchesInTx(tx, event, state)
	if err != nil || !matches {
		return err
	}
	x, err := tx.Execution(event.ExecutionID)
	if err != nil {
		return err
	}
	if x.State.Terminal() {
		return nil
	}
	if now.Before(state.Deadline) {
		return storage.ErrInvalid
	}
	old, err := retryState(tx, x.WorkID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if old.TimerID != "" {
		timer, e := tx.Timer(old.TimerID)
		if e == nil {
			if e = tx.DeleteTimer(timer.ID, timer.Revision); e != nil {
				return e
			}
		} else if !errors.Is(e, storage.ErrNotFound) {
			return e
		}
	}
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "execution timed out; causal continuation refused"}}
	x.State = domain.ExecutionFailed
	x.Outcome = &result.Outcome
	if err = tx.PutExecution(x, x.Revision); err != nil {
		return err
	}
	w, err := tx.Work(x.WorkID)
	if err != nil {
		return err
	}
	w.State = domain.WorkReady
	if err = tx.PutWork(w, w.Revision); err != nil {
		return err
	}
	state.Excluded = false
	state.LaunchState = "timed_out"
	state.Result = &result
	if err = putAttemptState(tx, state); err != nil {
		return err
	}
	history, err := tx.Events(0, 0)
	if err != nil {
		return err
	}
	var boundary uint64
	if len(history) > 0 {
		boundary = history[len(history)-1].Sequence
	}
	retry := RetryState{WorkID: w.ID, FailedExecutionID: x.ID, Binding: x.ExecutorBinding, FailureCount: old.FailureCount + 1, FailureSequence: boundary, Blocked: true, BlockedReason: "causal_rejection"}
	return putJSON(tx, retryNamespace, identity(string(w.ID)), retry)
}

// ProcessExecutionTimeout applies an accepted deadline Event under current
// attempt authority. Recovery of an expired lease occurs outside transactions;
// it never launches a replacement. The control audit uses an empty routed Work
// association so earlier unrouted runnable input cannot delay lease fencing;
// immutable Event.WorkID/ExecutionID retain the target association.
func (s *Scheduler) ProcessExecutionTimeout(ctx context.Context, eventID domain.ID) error {
	var event domain.AcceptedEvent
	var state AttemptState
	var x domain.Execution
	var stale bool
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		var e error
		event, e = tx.Event(eventID)
		if e != nil {
			return e
		}
		if event.Routed {
			return nil
		}
		if event.Event.Type != "core.execution.timeout" || event.Event.ExecutionID == "" {
			return storage.ErrInvalid
		}
		x, e = tx.Execution(event.Event.ExecutionID)
		if e != nil {
			return e
		}
		state, e = attemptState(tx, x.ID)
		if e != nil {
			return e
		}
		matches, e := executionTimeoutMatchesInTx(tx, event.Event, state)
		if e != nil {
			return e
		}
		if !matches || x.State != domain.ExecutionRunning {
			stale = true
			return tx.MarkRouted(eventID, "", "stale_execution_timeout")
		}
		acceptedScope, e := scopeForEvent(tx, event)
		if e != nil {
			return e
		}
		if acceptedScope != state.ScopeID {
			return storage.ErrConflict
		}
		if s.now().Before(state.Deadline) {
			return storage.ErrInvalid
		}
		return nil
	})
	if err != nil || event.Routed || stale {
		return err
	}
	if x.State.Terminal() {
		return s.transact(ctx, nil, func(tx storage.Tx) error { return tx.MarkRouted(eventID, "", "stale_execution_timeout") })
	}
	now := s.now()
	if x.Lease.Owner == "" || !now.Before(x.Lease.ExpiresAt) {
		recovered, e := s.Recover(ctx, x.ID)
		if e != nil {
			// Recovery may have atomically failed an unverifiable attempt. Its durable
			// hold and retry policy remain authoritative; timeout cannot resurrect it.
			if errors.Is(e, extension.ErrNotAdoptable) || errors.Is(e, ErrExecutorCall) {
				var terminal bool
				inspectErr := s.transact(ctx, nil, func(tx storage.Tx) error {
					latest, readErr := tx.Execution(x.ID)
					if readErr != nil {
						return readErr
					}
					terminal = latest.State.Terminal()
					if terminal {
						return tx.MarkRouted(eventID, "", "stale_execution_timeout")
					}
					return nil
				})
				if inspectErr != nil {
					return inspectErr
				}
				if terminal {
					return nil
				}
			}
			return e
		}
		x = recovered.Request.Execution
		x.Lease.Owner = recovered.Lease.Owner
		x.Lease.Epoch = recovered.Lease.Epoch
	} else if x.Lease.Owner != s.options.Owner {
		return storage.ErrClaimed
	}
	ref := domain.LeaseRef{ExecutionID: x.ID, Owner: x.Lease.Owner, Epoch: x.Lease.Epoch}
	return s.transact(ctx, &ref, func(tx storage.Tx) error {
		a, e := tx.Event(eventID)
		if e != nil {
			return e
		}
		if a.Routed {
			return nil
		}
		current, e := attemptState(tx, x.ID)
		if e != nil {
			return e
		}
		matches, e := executionTimeoutMatchesInTx(tx, a.Event, current)
		if e != nil {
			return e
		}
		if !matches {
			return tx.MarkRouted(eventID, "", "stale_execution_timeout")
		}
		if s.now().Before(current.Deadline) {
			return storage.ErrInvalid
		}
		execution, e := tx.Execution(x.ID)
		if e != nil {
			return e
		}
		result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "execution timed out"}}
		current.Excluded = false
		current.LaunchState = "timed_out"
		current.Result = &result
		if e = putAttemptState(tx, current); e != nil {
			return e
		}
		if e = CancelExecutionDeadlineInTx(tx, x.ID); e != nil {
			return e
		}
		if e = s.failureInTx(tx, execution, result, s.now()); e != nil {
			return e
		}
		return tx.MarkRouted(eventID, "", "execution_timeout")
	})
}

type PollDiagnostic struct {
	ID   domain.ID
	Code string
}
type SchedulerPoll struct {
	Timers      []TimerFire
	Recovered   []Attempt
	Diagnostics []PollDiagnostic
}

func rotateIDs(ids []domain.ID, cursor string, limit int) []domain.ID {
	sort.Slice(ids, func(i, j int) bool {
		a, b := string(ids[i]), string(ids[j])
		aa, bb := a > cursor, b > cursor
		if aa != bb {
			return aa
		}
		return a < b
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}
func pollDiagnostic(err error) string {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			if pollDiagnostic(part) == "" {
				return ""
			}
		}
	}
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "identity_or_revision_conflict"
	case errors.Is(err, storage.ErrInvalid):
		return "invalid_state"
	case errors.Is(err, storage.ErrNotFound):
		return "not_found"
	case errors.Is(err, storage.ErrClaimed), errors.Is(err, storage.ErrLeaseLost):
		return "authority_unavailable"
	case errors.Is(err, ErrNotEligible):
		return "not_eligible"
	case errors.Is(err, extension.ErrUnsupported):
		return "executor_unavailable"
	case errors.Is(err, extension.ErrNotAdoptable), errors.Is(err, ErrExecutorCall):
		return "executor_recovery_failed"
	}
	return ""
}

// Poll performs bounded host-driven passes for due timers, accepted retry/timeout
// Events, active Waits, and expired leases. Each pass uses a durable fair cursor.
// Recovered handles are returned for host observation; Poll never blocks in Wait.
func (s *Scheduler) Poll(ctx context.Context, timers *Timers, limit int) (result SchedulerPoll, err error) {
	if timers == nil || limit < 1 || limit > 200 {
		return result, storage.ErrInvalid
	}
	result.Timers, err = timers.Due(ctx, limit)
	if err != nil {
		return result, err
	}
	phases := []string{"events", "waits", "executions"}
	for _, phase := range phases {
		var ids []domain.ID
		err = s.transact(ctx, nil, func(tx storage.Tx) error {
			var cursor string
			e := getJSON(tx, timerNamespace, "poll_"+phase, &cursor)
			if e != nil && !errors.Is(e, storage.ErrNotFound) {
				return e
			}
			switch phase {
			case "events":
				events, e := tx.Events(0, 0)
				if e != nil {
					return e
				}
				for _, a := range events {
					if !a.Routed && (a.Event.Type == "core.retry.due" || a.Event.Type == "core.execution.timeout") {
						ids = append(ids, a.Event.ID)
					}
				}
			case "waits":
				waits, e := tx.ListWaits("")
				if e != nil {
					return e
				}
				for _, w := range waits {
					if w.State == domain.WaitActive {
						ids = append(ids, w.ID)
					}
				}
			case "executions":
				xs, e := tx.ListExecutions("")
				if e != nil {
					return e
				}
				for _, x := range xs {
					if x.State == domain.ExecutionRunning && (x.Lease.Owner == "" || !s.now().Before(x.Lease.ExpiresAt)) {
						ids = append(ids, x.ID)
					}
				}
			}
			ids = rotateIDs(ids, cursor, limit)
			return nil
		})
		if err != nil {
			return result, err
		}
		for _, id := range ids {
			var itemErr error
			switch phase {
			case "events":
				var a domain.AcceptedEvent
				itemErr = s.transact(ctx, nil, func(tx storage.Tx) error { var e error; a, e = tx.Event(id); return e })
				if itemErr == nil {
					if a.Event.Type == "core.retry.due" {
						itemErr = s.ProcessRetry(ctx, id)
					} else {
						itemErr = s.ProcessExecutionTimeout(ctx, id)
					}
				}
			case "waits":
				itemErr = s.Wake(ctx, id)
			case "executions":
				var recovered Attempt
				recovered, itemErr = s.Recover(ctx, id)
				if itemErr == nil {
					result.Recovered = append(result.Recovered, recovered)
				}
			}
			if itemErr != nil {
				code := pollDiagnostic(itemErr)
				if code == "" {
					return result, itemErr
				}
				result.Diagnostics = append(result.Diagnostics, PollDiagnostic{ID: id, Code: code})
			}
			if err = s.transact(ctx, nil, func(tx storage.Tx) error { return putJSON(tx, timerNamespace, "poll_"+phase, string(id)) }); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}
