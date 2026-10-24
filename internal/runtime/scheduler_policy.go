package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

const retryNamespace = "core.scheduler.eligibility"
const retryEventsNamespace = "core.scheduler.retry.events"

var ErrNotEligible = errors.New("work is not eligible for an attempt")

type RetryPolicy struct {
	MaxAttempts          int
	Delays               []time.Duration
	TerminalOnExhaustion bool
}

func normalizeRetryPolicy(p RetryPolicy) (RetryPolicy, error) {
	if p.MaxAttempts == 0 {
		if p.Delays != nil {
			p.MaxAttempts = len(p.Delays) + 1
		} else {
			p.MaxAttempts = 3
		}
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 101 {
		return p, storage.ErrInvalid
	}
	if p.Delays == nil {
		p.Delays = make([]time.Duration, p.MaxAttempts-1)
		for i := range p.Delays {
			p.Delays[i] = time.Duration(i+1) * 5 * time.Minute
		}
	}
	if len(p.Delays) != p.MaxAttempts-1 {
		return p, storage.ErrInvalid
	}
	p.Delays = append([]time.Duration(nil), p.Delays...)
	for _, delay := range p.Delays {
		if delay <= 0 || delay > 365*24*time.Hour {
			return p, storage.ErrInvalid
		}
	}
	return p, nil
}

// RetryHint is generic executor classification. A known reset receives two
// minutes of margin; an unknown reset waits thirty minutes. Invalid or excessive
// hints are rejected rather than silently producing immediate retries.
type RetryHint = extension.RetryHint
type RetryHintProvider = extension.RetryHintProvider

func RetryDelay(now time.Time, h RetryHint) (time.Duration, error) {
	if h.ResetAt == nil {
		return 30 * time.Minute, nil
	}
	if h.ResetAt.IsZero() {
		return 0, storage.ErrInvalid
	}
	delay := h.ResetAt.Sub(now) + 2*time.Minute
	if delay <= 0 || delay > 365*24*time.Hour {
		return 0, storage.ErrInvalid
	}
	return delay, nil
}

type RetryState struct {
	WorkID, FailedExecutionID domain.ID
	Binding                   string
	FailureCount              int
	FailureSequence           uint64
	Blocked, Exhausted        bool
	BlockedReason             string
	EligibleAt                time.Time
	TimerID, RetryEventID     domain.ID
}

func retryState(tx storage.Tx, work domain.ID) (RetryState, error) {
	var state RetryState
	err := getJSON(tx, retryNamespace, identity(string(work)), &state)
	return state, err
}
func retryEligibleInTx(tx storage.Tx, work domain.ID) error {
	state, err := retryState(tx, work)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Blocked || state.Exhausted {
		return ErrNotEligible
	}
	return nil
}
func resetRetryInTx(tx storage.Tx, work domain.ID) error {
	state, err := retryState(tx, work)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.TimerID != "" {
		timer, er := tx.Timer(state.TimerID)
		if er == nil {
			if er = tx.DeleteTimer(timer.ID, timer.Revision); er != nil {
				return er
			}
			if er = tx.Delete(timerNamespace, timerKey(timer.ID)); er != nil {
				return er
			}
		} else if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
	}
	return tx.Delete(retryNamespace, identity(string(work)))
}
func (s *Scheduler) failureInTx(tx storage.Tx, x domain.Execution, result extension.Result, now time.Time) error {
	return s.failureWithHintInTx(tx, x, result, now, nil)
}
func (s *Scheduler) failureWithHintInTx(tx storage.Tx, x domain.Execution, result extension.Result, now time.Time, hint *RetryHint) error {
	policy := s.options.Retry
	pinned, pinErr := attemptState(tx, x.ID)
	if pinErr == nil && pinned.Retry.MaxAttempts != 0 {
		policy = pinned.Retry
	} else if pinErr != nil && !errors.Is(pinErr, storage.ErrNotFound) {
		return pinErr
	}
	policy, err := normalizeRetryPolicy(policy)
	if err != nil {
		return err
	}
	old, err := retryState(tx, x.WorkID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	failures := old.FailureCount + 1
	if old.FailedExecutionID == x.ID {
		return storage.ErrConflict
	}
	w, err := tx.Work(x.WorkID)
	if err != nil {
		return err
	}
	result.Outcome.Kind = domain.OutcomeFailed
	x.State = domain.ExecutionFailed
	x.Outcome = &result.Outcome
	x.Artifacts = append([]domain.ArtifactRef(nil), result.Artifacts...)
	if err = tx.PutExecution(x, x.Revision); err != nil {
		return err
	}
	history, err := tx.Events(0, 0)
	if err != nil {
		return err
	}
	var failureSequence uint64
	if len(history) > 0 {
		failureSequence = history[len(history)-1].Sequence
	}
	state := RetryState{FailureSequence: failureSequence, WorkID: w.ID, FailedExecutionID: x.ID, Binding: x.ExecutorBinding, FailureCount: failures, Blocked: true, Exhausted: failures >= policy.MaxAttempts, TimerID: domain.ID("retry/" + string(w.ID))}
	w.State = domain.WorkReady
	if state.Exhausted && policy.TerminalOnExhaustion {
		w.State = domain.WorkFailed
	}
	if err = tx.PutWork(w, w.Revision); err != nil {
		return err
	}
	if old.TimerID != "" {
		timer, er := tx.Timer(old.TimerID)
		if er == nil {
			if er = tx.DeleteTimer(timer.ID, timer.Revision); er != nil {
				return er
			}
			if er = tx.Delete(timerNamespace, timerKey(timer.ID)); er != nil {
				return er
			}
		} else if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
	}
	if !state.Exhausted {
		delay := policy.Delays[failures-1]
		if hint != nil {
			delay, err = RetryDelay(now, *hint)
			if err != nil {
				return err
			}
		}
		state.EligibleAt = now.Add(delay)
		state.RetryEventID = domain.ID("retry-event/" + identity(string(x.ID)))
		data, er := json.Marshal(struct{ WorkID, FailedExecutionID domain.ID }{w.ID, x.ID})
		if er != nil {
			return er
		}
		trigger, er := executionTriggerInTx(tx, x)
		if er != nil {
			return er
		}
		event := domain.Event{ID: state.RetryEventID, Type: "core.retry.due", Source: "core.retry", DeliveryID: "due/" + string(x.ID), OccurredAt: state.EligibleAt, ReceivedAt: state.EligibleAt, CausationID: trigger, WorkID: w.ID, Data: domain.Payload{Schema: "core.retry.due.v1", Data: data}}
		if err = tx.PutTimer(domain.Timer{ID: state.TimerID, FireAt: state.EligibleAt, Event: event}, 0); err != nil {
			return err
		}
	}
	return putJSON(tx, retryNamespace, identity(string(w.ID)), state)
}

// ProcessRetry consumes an accepted trusted retry Event. Timer dispatch must
// first use AcceptDerivedInTx and remove the timer atomically; a due clock alone
// cannot make Work eligible. The B05 operator family and B04 family are aliases.
func (s *Scheduler) ProcessRetry(ctx context.Context, eventID domain.ID) error {
	return s.transact(ctx, nil, func(tx storage.Tx) error {
		a, err := tx.Event(eventID)
		if err != nil {
			return err
		}
		manual := a.Event.Type == "core.retry.requested" || a.Event.Type == "core.manual.retry"
		if !manual && a.Event.Type != "core.retry.due" {
			return storage.ErrInvalid
		}
		if a.Event.WorkID == "" {
			return storage.ErrInvalid
		}
		work, err := tx.Work(a.Event.WorkID)
		if err != nil {
			return err
		}
		if acceptedScope, er := scopeForEvent(tx, a); er != nil {
			return er
		} else if acceptedScope != work.ScopeID {
			return storage.ErrConflict
		}
		var prior domain.ID
		err = getJSON(tx, retryEventsNamespace, identity(string(eventID)), &prior)
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		state, err := retryState(tx, work.ID)
		if errors.Is(err, storage.ErrNotFound) || work.State.Terminal() {
			return tx.MarkRouted(eventID, work.ID, "stale_retry")
		}
		if err != nil {
			return err
		}
		if manual && a.Sequence <= state.FailureSequence {
			return tx.MarkRouted(eventID, work.ID, "stale_retry")
		}
		if !manual && eventID != state.RetryEventID {
			return tx.MarkRouted(eventID, work.ID, "stale_retry")
		}
		if !manual && s.now().Before(state.EligibleAt) {
			return ErrNotEligible
		}
		xs, err := tx.ListExecutions(work.ID)
		if err != nil {
			return err
		}
		for _, x := range xs {
			if !x.State.Terminal() {
				return ErrNotEligible
			}
		}
		waits, err := tx.ListWaits(work.ID)
		if err != nil {
			return err
		}
		for _, wait := range waits {
			if wait.State == domain.WaitActive {
				return ErrNotEligible
			}
		}
		if a.Routed {
			return storage.ErrConflict
		}
		if state.TimerID != "" {
			timer, er := tx.Timer(state.TimerID)
			if er == nil {
				if er = tx.DeleteTimer(timer.ID, timer.Revision); er != nil {
					return er
				}
			} else if !errors.Is(er, storage.ErrNotFound) {
				return er
			}
		}
		state.Blocked = false
		state.BlockedReason = ""
		state.Exhausted = false
		state.TimerID = ""
		state.RetryEventID = ""
		state.EligibleAt = time.Time{}
		if manual {
			state.FailureCount = 0
		}
		if err = putJSON(tx, retryNamespace, identity(string(work.ID)), state); err != nil {
			return err
		}
		if err = putJSON(tx, retryEventsNamespace, identity(string(eventID)), state.FailedExecutionID); err != nil {
			return err
		}
		if err = putJSON(tx, queueNamespace, queueKey(work.ID, a.Sequence), QueuedInput{EventID: eventID, Sequence: a.Sequence, Binding: state.Binding}); err != nil {
			return err
		}
		return tx.MarkRouted(eventID, work.ID, "retry")
	})
}
func (s *Scheduler) RetryStatus(ctx context.Context, work domain.ID) (state RetryState, err error) {
	err = s.transact(ctx, nil, func(tx storage.Tx) error { var er error; state, er = retryState(tx, work); return er })
	return
}
