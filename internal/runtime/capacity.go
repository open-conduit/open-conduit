package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
// core.capacity.held Event. A Timer armed with the hold fires at the
// provider's hinted time or the host's backoff; its core.capacity.due Event
// ends the hold with a core.capacity.released Event, and the next pass tries
// again. An operator's release tries again at once, and the Timer then finds
// the hold gone and does nothing.
//
// A refused fresh attempt ends as cancelled, since an Execution can't go
// back to pending; a refused resume goes back to parked, the same attempt.
// The refusal is recorded in the attempt state, whose launch stays launching
// or resuming with nothing pinned: an attempt that loses its daemon before
// the claim is returned is relaunched (relaunchable), so it spends no retry.

const (
	// HoldKindCapacity marks a hold placed because a container provider had
	// no room for the launch.
	HoldKindCapacity = "capacity"

	// typeCapacityDue is the Event a capacity hold's Timer fires.
	typeCapacityDue = "core.capacity.due"

	capacityBackoffBase = 30 * time.Second
	capacityBackoffMax  = 5 * time.Minute
	// capacityHintMin and capacityHintMax bound a provider's RetryAt, so a
	// hint can neither spin the scheduler nor park Work out of reach.
	capacityHintMin = 5 * time.Second
	capacityHintMax = time.Hour

	maxCapacityReason = 500

	// capacityLaunchState is the LaunchState of a fresh attempt ended by a
	// refusal.
	capacityLaunchState = "no_capacity"
)

// ErrContainerCapacity is a launch refused because the container provider
// had no room: the claim was returned and the Work is held.
var ErrContainerCapacity = fmt.Errorf("%w: held for container capacity", ErrNotEligible)

// StartCharge is what a claim took for its start, so a launch refused for
// capacity can give it back: when it was charged, whether it was a new
// attempt, and the budget release whose one-turn pass it used, if any. A
// start gate's pass needs no giving back: a gate only asks about a Work's
// first attempt, which the refused attempt was.
type StartCharge struct {
	At         time.Time
	NewAttempt bool      `json:",omitempty"`
	BudgetPass domain.ID `json:",omitempty"`
}

// capacityDue is the data of a core.capacity.due Event: the hold it ends.
type capacityDue struct {
	HeldEventID domain.ID `json:"held_event_id"`
	Execution   domain.ID `json:"execution"`
	Refusals    int       `json:"refusals"`
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
	delivery := strings.TrimPrefix(typ, "core.capacity.") + "/" + identity(string(w.ID), now.UTC().Format(time.RFC3339Nano), string(n.Execution), strconv.Itoa(n.Refusals))
	e := domain.Event{ID: domain.ID("capacity-" + delivery), Type: typ, Source: "core.capacity", DeliveryID: delivery, WorkID: w.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.capacity.v1", Data: data}}
	accepted, _, err := AcceptInTx(tx, w.ScopeID, e)
	if err != nil {
		return "", err
	}
	return accepted.Event.ID, tx.MarkRouted(accepted.Event.ID, "", "control")
}

// requeueInputsInTx puts the inputs this claim took (claimed, the attempt's
// InputEventIDs) back on the Work's queue: the launch never saw them. What
// the execution consumed in earlier segments it has handled, and stays
// consumed.
func requeueInputsInTx(tx storage.Tx, work, execution domain.ID, claimed []domain.ID) ([]QueuedInput, error) {
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
		if !slices.Contains(claimed, consumed.Input.EventID) {
			continue
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

// refundBudgetInTx gives back what a start took: one turn, one attempt for
// a new attempt, from the ledgers of the moment it was charged, never below
// zero, and the budget release's pass it used.
func refundBudgetInTx(tx storage.Tx, charge *StartCharge, budgets []Budget, w domain.Work) error {
	if charge == nil {
		return nil
	}
	if charge.BudgetPass != "" {
		if err := putJSON(tx, budgetPassNamespace, identity(string(w.ID)), charge.BudgetPass); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, b := range budgets {
		if !b.applies(w.ScopeID) {
			continue
		}
		key := b.ledgerKey(w.ID, w.ScopeID, charge.At)
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
		if charge.NewAttempt {
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
		inputs, err := requeueInputsInTx(tx, w.ID, x.ID, state.InputEventIDs)
		if err != nil {
			return err
		}
		n := len(state.Segments)
		// An attempt that has parked before was resuming.
		resumed := state.WaitID != ""
		if err = refundBudgetInTx(tx, state.StartCharge, s.options.Budgets, w); err != nil {
			return err
		}
		refusals, err := priorRefusalsInTx(tx, x, state)
		if err != nil {
			return err
		}
		refusals++
		state.StartCharge, state.NoCapacity, state.CapacityRefusals = nil, nil, refusals
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
			if n > 0 && state.Segments[n-1].EndedAt == nil {
				state.Segments[n-1].Unmetered = true
			}
			state.endSegment(now, false)
			x.State, x.Outcome = domain.ExecutionCancelled, nil
			state.LaunchState = capacityLaunchState
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
		// A hold someone else placed meanwhile stands: it says when the
		// Work may start.
		if _, held, e := HoldInTx(tx, w.ID); e != nil {
			return e
		} else if held {
			return nil
		}
		retryAt := now.Add(capacityDelay(now, refusal.RetryAt, refusals)).UTC()
		detail := strings.ToValidUTF8(refusal.Reason, "")
		if len(detail) > maxCapacityReason {
			detail = strings.ToValidUTF8(detail[:maxCapacityReason], "")
		}
		reason := "Waiting for container capacity. Trying again at " + retryAt.Format(time.RFC3339) + "; release the hold to try now."
		if detail != "" {
			reason = "Waiting for container capacity: " + detail + ". Trying again at " + retryAt.Format(time.RFC3339) + "; release the hold to try now."
		}
		event, err := capacityEventInTx(tx, now, w, domain.EventCapacityHeld, CapacityNotice{Reason: reason, Execution: x.ID, Refusals: refusals, RetryAt: &retryAt})
		if err != nil {
			return err
		}
		if err = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: event, Reason: reason, At: now.UTC(), Kind: HoldKindCapacity}); err != nil {
			return err
		}
		if err = armCapacityTimerInTx(tx, w, x.ID, event, refusals, retryAt); err != nil {
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

// priorRefusalsInTx is the Work's run of refusals before this one: the
// attempt's own for a resume, the attempt before's when that one was refused
// too. A launch that gets a container ends the run.
func priorRefusalsInTx(tx storage.Tx, x domain.Execution, state AttemptState) (int, error) {
	if state.WaitID != "" {
		return state.CapacityRefusals, nil
	}
	xs, err := tx.ListExecutions(x.WorkID)
	if err != nil {
		return 0, err
	}
	for _, other := range xs {
		if other.Attempt+1 != x.Attempt {
			continue
		}
		prior, err := attemptState(tx, other.ID)
		if errors.Is(err, storage.ErrNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if prior.LaunchState == capacityLaunchState {
			return prior.CapacityRefusals, nil
		}
	}
	return 0, nil
}

// armCapacityTimerInTx arms the Timer that ends the hold event recorded,
// replacing one left by a hold an operator released early.
func armCapacityTimerInTx(tx storage.Tx, w domain.Work, execution, event domain.ID, refusals int, at time.Time) error {
	id := domain.ID("capacity/" + string(w.ID))
	if old, err := tx.Timer(id); err == nil {
		if err = tx.DeleteTimer(old.ID, old.Revision); err != nil {
			return err
		}
		if err = tx.Delete(timerNamespace, timerKey(old.ID)); err != nil {
			return err
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	data, err := json.Marshal(capacityDue{HeldEventID: event, Execution: execution, Refusals: refusals})
	if err != nil {
		return err
	}
	due := identity(string(event))
	return tx.PutTimer(domain.Timer{ID: id, FireAt: at, Event: domain.Event{ID: domain.ID("capacity-due/" + due), Type: typeCapacityDue, Source: "core.capacity", DeliveryID: "due/" + due, OccurredAt: at, ReceivedAt: at, CausationID: event, WorkID: w.ID, Data: domain.Payload{Schema: "core.capacity.due.v1", Data: data}}}, 0)
}

// ProcessCapacityDue consumes a capacity hold's fired Timer Event. It ends
// the hold the Event names; a hold since released or replaced, or Work
// since finished, is left as it is.
func (s *Scheduler) ProcessCapacityDue(ctx context.Context, eventID domain.ID) error {
	return s.transact(ctx, nil, func(tx storage.Tx) error {
		a, err := tx.Event(eventID)
		if err != nil {
			return err
		}
		if a.Event.Type != typeCapacityDue || a.Event.WorkID == "" {
			return storage.ErrInvalid
		}
		if a.Routed {
			return nil
		}
		var due capacityDue
		if err = json.Unmarshal(a.Event.Data.Data, &due); err != nil {
			return storage.ErrInvalid
		}
		w, err := tx.Work(a.Event.WorkID)
		if err != nil {
			return err
		}
		h, held, err := HoldInTx(tx, w.ID)
		if err != nil {
			return err
		}
		if w.State.Terminal() || !held || h.Kind != HoldKindCapacity || h.EventID != due.HeldEventID {
			return tx.MarkRouted(eventID, w.ID, "stale_capacity")
		}
		if err = tx.Delete(holdNamespace, identity(string(w.ID))); err != nil {
			return err
		}
		if _, err = capacityEventInTx(tx, s.now(), w, domain.EventCapacityReleased, CapacityNotice{Reason: "Trying for container capacity again.", Execution: due.Execution, Refusals: due.Refusals}); err != nil {
			return err
		}
		if err = tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		return tx.MarkRouted(eventID, w.ID, "capacity")
	})
}

// recordRefusal keeps a launch's refusal in its attempt state, with the
// launch still launching or resuming, so it survives this daemon.
func recordRefusal(state *AttemptState, r *extension.NoCapacityError) {
	state.NoCapacity = r
	state.LaunchState = "launching"
	if state.WaitID != "" {
		state.LaunchState = "resuming"
	}
}
