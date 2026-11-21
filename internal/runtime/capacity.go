package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Capacity holds (#702). A container provider whose pool is full refuses a
// launch with extension.ErrNoCapacity. Nothing ran and nothing is wrong, so
// the host returns the claim instead of failing the attempt: the inputs go
// back on the queue, the budget charge is refunded, the retry count is left
// alone, and the Work is held with a hold of kind "capacity", recorded by a
// core.capacity.held Event. The poll releases it when the provider's hinted
// time or the host's backoff comes, with a core.capacity.released Event,
// and the next pass tries again. An operator's release tries again at once.
//
// A refused fresh attempt ends as cancelled, since an Execution can't go
// back to pending; a refused resume goes back to parked, the same attempt.

const (
	// HoldKindCapacity marks a hold placed because a container provider had
	// no room for the launch.
	HoldKindCapacity = "capacity"

	capacityNamespace = "core.capacity.state"

	capacityBackoffBase = 30 * time.Second
	capacityBackoffMax  = 5 * time.Minute
	// capacityHintMin and capacityHintMax bound a provider's RetryAt, so a
	// hint can neither spin the scheduler nor park Work out of reach.
	capacityHintMin = 5 * time.Second
	capacityHintMax = time.Hour

	maxCapacityReason = 500
)

// ErrContainerCapacity is a launch refused because the container provider
// had no room: the claim was returned and the Work is held.
var ErrContainerCapacity = fmt.Errorf("%w: held for container capacity", ErrNotEligible)

// capacityRecord is a Work's run of refusals: how many in a row, and the
// hold now waiting for RetryAt, if Held.
type capacityRecord struct {
	WorkID   domain.ID `json:"work_id,omitempty"`
	Refusals int       `json:"refusals"`
	Held     bool      `json:"held,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitzero"`
	EventID  domain.ID `json:"event_id,omitempty"`
}

// CapacityNotice is the data of a core.capacity.* Event.
type CapacityNotice struct {
	Reason    string     `json:"reason"`
	Execution domain.ID  `json:"execution,omitempty"`
	Refusals  int        `json:"refusals,omitempty"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
}

// capacityDelay is how long to hold after the n-th refusal in a row: the
// provider's hint, bounded, or a doubling backoff.
func capacityDelay(now time.Time, hint *time.Time, n int) time.Duration {
	if hint != nil {
		return min(max(hint.Sub(now), capacityHintMin), capacityHintMax)
	}
	delay := capacityBackoffBase
	for i := 1; i < n && delay < capacityBackoffMax; i++ {
		delay *= 2
	}
	return min(delay, capacityBackoffMax)
}

func capacityEventInTx(tx storage.Tx, now time.Time, w domain.Work, typ string, n CapacityNotice) (domain.ID, error) {
	data, _ := json.Marshal(n)
	delivery := strings.TrimPrefix(typ, "core.capacity.") + "/" + identity(string(w.ID), now.UTC().Format(time.RFC3339Nano))
	e := domain.Event{ID: domain.ID("capacity-" + delivery), Type: typ, Source: "core.capacity", DeliveryID: delivery, WorkID: w.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.capacity.v1", Data: data}}
	accepted, _, err := AcceptInTx(tx, w.ScopeID, e)
	if err != nil {
		return "", err
	}
	return accepted.Event.ID, tx.MarkRouted(accepted.Event.ID, "", "control")
}

// requeueInputsInTx puts the inputs execution consumed back on the Work's
// queue: it never saw them.
func requeueInputsInTx(tx storage.Tx, work, execution domain.ID) ([]QueuedInput, error) {
	rows, err := tx.List(consumedNamespace, identity(string(work))+"/")
	if err != nil {
		return nil, err
	}
	suffix := "/" + identity(string(execution))
	var inputs []QueuedInput
	for _, row := range rows {
		if !strings.HasSuffix(row.Key, suffix) {
			continue
		}
		var consumed struct {
			Input       QueuedInput
			ExecutionID domain.ID
		}
		if err = getJSON(tx, consumedNamespace, row.Key, &consumed); err != nil {
			return nil, err
		}
		if err = putJSON(tx, queueNamespace, queueKey(work, consumed.Input.Sequence), consumed.Input); err != nil {
			return nil, err
		}
		if err = tx.Delete(consumedNamespace, row.Key); err != nil {
			return nil, err
		}
		inputs = append(inputs, consumed.Input)
	}
	return inputs, nil
}

// refundBudgetInTx takes back what a start charged at at: one turn, and one
// attempt for a new attempt, from the ledgers of that moment, never below
// zero.
func refundBudgetInTx(tx storage.Tx, at time.Time, budgets []Budget, w domain.Work, newAttempt bool) error {
	seen := map[string]bool{}
	for _, b := range budgets {
		if !b.applies(w.ScopeID) {
			continue
		}
		key := b.ledgerKey(w.ID, w.ScopeID, at)
		if seen[key] {
			continue
		}
		seen[key] = true
		l, err := ledgerInTx(tx, key)
		if err != nil {
			return err
		}
		after := make(ledger, len(l))
		for unit, v := range l {
			after[unit] = v
		}
		after["turns"] = max(after["turns"]-1, 0)
		if newAttempt {
			after["attempts"] = max(after["attempts"]-1, 0)
		}
		if err = putJSON(tx, budgetLedgerNamespace, key, after); err != nil {
			return err
		}
	}
	return nil
}

// holdForCapacity returns a refused attempt's claim and holds its Work. It
// is fenced by the attempt's lease.
func (s *Scheduler) holdForCapacity(ctx context.Context, ref domain.LeaseRef, refusal *extension.NoCapacityError) error {
	return s.transact(ctx, &ref, func(tx storage.Tx) error {
		now := s.now()
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		state, err := attemptState(tx, x.ID)
		if err != nil {
			return err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		if err = CancelExecutionDeadlineInTx(tx, x.ID); err != nil {
			return err
		}
		inputs, err := requeueInputsInTx(tx, w.ID, x.ID)
		if err != nil {
			return err
		}
		claimedAt := now
		n := len(state.Segments)
		if n > 0 && state.Segments[n-1].EndedAt == nil {
			claimedAt = state.Segments[n-1].StartedAt
		}
		// An attempt that has parked before was resuming.
		resumed := state.WaitID != ""
		if err = refundBudgetInTx(tx, claimedAt, s.options.Budgets, w, !resumed); err != nil {
			return err
		}
		state.Excluded, state.Deadline = true, time.Time{}
		if resumed {
			// Back to parked, with its wake ready again: the segment never ran.
			if n > 0 && state.Segments[n-1].EndedAt == nil {
				state.Segments = state.Segments[:n-1]
			}
			outcome := domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "held for container capacity"}
			if state.Result != nil && state.Result.Outcome.Kind == domain.OutcomeWaiting {
				outcome = state.Result.Outcome
			}
			x.State, x.Outcome = domain.ExecutionParked, &outcome
			state.LaunchState = "waiting"
			if state.WakeEventID != "" {
				binding := x.ExecutorBinding
				for _, in := range inputs {
					if in.EventID == state.WakeEventID {
						binding = in.Binding
					}
				}
				if err = putJSON(tx, wakeReadyNamespace, identity(string(w.ID)), wakeReady{WaitID: state.WaitID, ExecutionID: x.ID, EventID: state.WakeEventID, Binding: binding}); err != nil {
					return err
				}
				state.WakeEventID = ""
			}
		} else {
			state.endSegment(now, false)
			x.State, x.Outcome = domain.ExecutionCancelled, nil
			state.LaunchState = "no_capacity"
		}
		if err = tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		if err = putAttemptState(tx, state); err != nil {
			return err
		}
		w.State = domain.WorkReady
		if err = tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		var record capacityRecord
		if err = getJSON(tx, capacityNamespace, identity(string(w.ID)), &record); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		record.WorkID = w.ID
		record.Refusals++
		// A hold someone else placed meanwhile stands: it says when the
		// Work may start.
		if _, held, e := HoldInTx(tx, w.ID); e != nil {
			return e
		} else if held {
			record.Held = false
			return putJSON(tx, capacityNamespace, identity(string(w.ID)), record)
		}
		retryAt := now.Add(capacityDelay(now, refusal.RetryAt, record.Refusals)).UTC()
		detail := strings.ToValidUTF8(refusal.Reason, "")
		if len(detail) > maxCapacityReason {
			detail = strings.ToValidUTF8(detail[:maxCapacityReason], "")
		}
		reason := "Waiting for container capacity. Trying again at " + retryAt.Format(time.RFC3339) + "; release the hold to try now."
		if detail != "" {
			reason = "Waiting for container capacity: " + detail + ". Trying again at " + retryAt.Format(time.RFC3339) + "; release the hold to try now."
		}
		event, err := capacityEventInTx(tx, now, w, domain.EventCapacityHeld, CapacityNotice{Reason: reason, Execution: x.ID, Refusals: record.Refusals, RetryAt: &retryAt})
		if err != nil {
			return err
		}
		if err = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: event, Reason: reason, At: now.UTC(), Kind: HoldKindCapacity}); err != nil {
			return err
		}
		record.Held, record.RetryAt, record.EventID = true, retryAt, event
		if err = putJSON(tx, capacityNamespace, identity(string(w.ID)), record); err != nil {
			return err
		}
		// Advance the revision, as an operator hold does, so views see it.
		current, err := tx.Work(w.ID)
		if err != nil {
			return err
		}
		return tx.PutWork(current, current.Revision)
	})
}

// releaseCapacity releases every capacity hold whose time has come. Work
// that finished, or whose hold has been released or replaced, only has its
// record settled.
func (s *Scheduler) releaseCapacity(ctx context.Context) error {
	var due, stale []string
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		due, stale = nil, nil
		rows, e := tx.List(capacityNamespace, "")
		if e != nil {
			return e
		}
		now := s.now()
		for _, row := range rows {
			var record capacityRecord
			if e = getJSON(tx, capacityNamespace, row.Key, &record); e != nil {
				return e
			}
			if record.Held {
				if !now.Before(record.RetryAt) {
					due = append(due, row.Key)
				}
				continue
			}
			// A run of refusals whose Work has finished is over.
			if finished, e := capacityWorkFinishedInTx(tx, row.Key); e != nil {
				return e
			} else if finished {
				stale = append(stale, row.Key)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		if err = s.transact(ctx, nil, func(tx storage.Tx) error {
			for _, key := range stale {
				if e := tx.Delete(capacityNamespace, key); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, key := range due {
		if err = s.transact(ctx, nil, func(tx storage.Tx) error { return s.releaseCapacityInTx(tx, key) }); err != nil {
			return err
		}
	}
	return nil
}

// capacityWorkFinishedInTx reports whether the Work a record is for has
// finished. Records are keyed by the Work's identity hash, so the Work is
// named by the record's last held Event.
func capacityWorkFinishedInTx(tx storage.Tx, key string) (bool, error) {
	var ref capacityWorkRef
	if err := getJSON(tx, capacityNamespace, key, &ref); err != nil {
		return false, err
	}
	if ref.WorkID == "" {
		return false, nil
	}
	w, err := tx.Work(ref.WorkID)
	if errors.Is(err, storage.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return w.State.Terminal(), nil
}

// capacityWorkRef reads only the Work a capacityRecord names.
type capacityWorkRef struct {
	WorkID domain.ID `json:"work_id,omitempty"`
}

func (s *Scheduler) releaseCapacityInTx(tx storage.Tx, key string) error {
	var record capacityRecord
	if err := getJSON(tx, capacityNamespace, key, &record); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	now := s.now()
	if !record.Held || now.Before(record.RetryAt) {
		return nil
	}
	var hold Hold
	err := getJSON(tx, holdNamespace, key, &hold)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err == nil && hold.Kind == HoldKindCapacity && hold.EventID == record.EventID {
		var notice CapacityNotice
		a, e := tx.Event(record.EventID)
		if e != nil {
			return e
		}
		if e = json.Unmarshal(a.Event.Data.Data, &notice); e != nil {
			return e
		}
		w, e := tx.Work(a.Event.WorkID)
		if e != nil {
			return e
		}
		if e = tx.Delete(holdNamespace, key); e != nil {
			return e
		}
		if w.State.Terminal() {
			return tx.Delete(capacityNamespace, key)
		}
		if _, e = capacityEventInTx(tx, now, w, domain.EventCapacityReleased, CapacityNotice{Reason: "Trying for container capacity again.", Execution: notice.Execution, Refusals: record.Refusals}); e != nil {
			return e
		}
		if e = tx.PutWork(w, w.Revision); e != nil {
			return e
		}
	}
	record.Held, record.RetryAt, record.EventID = false, time.Time{}, ""
	return putJSON(tx, capacityNamespace, key, record)
}
