package runtime

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Why a daemon can't be stopped yet: core's own in-flight state, each read
// from storage so the answer doesn't depend on what this process remembers.
const (
	// QuiescentLiveExecution is an attempt pending or running. A parked
	// attempt has no process and holds nothing.
	QuiescentLiveExecution = "live_execution"
	// QuiescentUnconfirmedStop is a finished attempt whose process isn't yet
	// proved stopped, which the host follows up.
	QuiescentUnconfirmedStop = "unconfirmed_stop"
	// QuiescentUnroutedEvent is an accepted Event not yet routed.
	QuiescentUnroutedEvent = "unrouted_event"
	// QuiescentUnreadInput is ready Work, not held, with input it hasn't read.
	QuiescentUnreadInput = "unread_input"
	// QuiescentPendingControl is an inject or interrupt not yet settled on a
	// running attempt.
	QuiescentPendingControl = "pending_control"
	// QuiescentUnsettledUsage is usage staged and not yet charged to a budget.
	QuiescentUnsettledUsage = "unsettled_usage"
	// QuiescentUnjudgedWait is an active Wait with accepted Events its wake
	// scan hasn't judged yet.
	QuiescentUnjudgedWait = "unjudged_wait"
	// QuiescentPendingSubscription is a dependency subscription not yet
	// installed, or one behind accepted history.
	QuiescentPendingSubscription = "pending_subscription"
)

// QuiescenceReason is one kind of in-flight state and how many of it there
// are.
type QuiescenceReason struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// settledControl is whether a control's status is final.
func settledControl(status string) bool {
	switch status {
	case "accepted", "stale", "fallback", "consumed", "not_accepting":
		return true
	}
	return false
}

// QuiescenceInTx reports the in-flight state that needs a running daemon, in
// code order, and the earliest core Timer (zero when there is none). Its cost
// follows the live set, apart from the control intents, read only while an
// attempt is running.
func QuiescenceInTx(tx storage.Tx) (reasons []QuiescenceReason, nextTimer time.Time, err error) {
	counts := map[string]int{}
	live, err := tx.LiveExecutions()
	if err != nil {
		return nil, time.Time{}, err
	}
	running := map[domain.ID]bool{}
	for _, x := range live {
		if x.State == domain.ExecutionParked {
			continue
		}
		counts[QuiescentLiveExecution]++
		if x.State == domain.ExecutionRunning {
			running[x.ID] = true
		}
	}
	if len(running) > 0 {
		intents, e := tx.List(intentNamespace, "")
		if e != nil {
			return nil, time.Time{}, e
		}
		for _, row := range intents {
			var intent ControlIntent
			if e = json.Unmarshal(row.Value, &intent); e != nil {
				return nil, time.Time{}, e
			}
			if !running[intent.ExecutionID] {
				continue
			}
			state, e := controlState(tx, intent.ID)
			if e != nil && !errors.Is(e, storage.ErrNotFound) {
				return nil, time.Time{}, e
			}
			if !settledControl(state.Status) {
				counts[QuiescentPendingControl]++
			}
		}
	}
	for namespace, code := range map[string]string{
		unexcludedNamespace:          QuiescentUnconfirmedStop,
		budgetPendingNamespace:       QuiescentUnsettledUsage,
		pendingSubscriptionNamespace: QuiescentPendingSubscription,
	} {
		rows, e := tx.List(namespace, "")
		if e != nil {
			return nil, time.Time{}, e
		}
		counts[code] += len(rows)
	}
	const unroutedPage = 200
	var after uint64
	for {
		events, e := tx.UnroutedEvents(after, unroutedPage)
		if e != nil {
			return nil, time.Time{}, e
		}
		for _, a := range events {
			after = a.Sequence
			if !a.Routed {
				counts[QuiescentUnroutedEvent]++
			}
		}
		if len(events) < unroutedPage {
			break
		}
	}
	works, err := tx.LiveWorks()
	if err != nil {
		return nil, time.Time{}, err
	}
	for _, w := range works {
		if w.State != domain.WorkReady {
			continue
		}
		if _, held, e := HoldInTx(tx, w.ID); e != nil {
			return nil, time.Time{}, e
		} else if held {
			continue
		}
		inputs, e := pendingInputs(tx, w.ID)
		if e != nil {
			return nil, time.Time{}, e
		}
		if len(inputs) > 0 {
			counts[QuiescentUnreadInput]++
		}
	}
	if err = countUnjudgedInTx(tx, counts); err != nil {
		return nil, time.Time{}, err
	}
	timers, err := tx.ListTimers()
	if err != nil {
		return nil, time.Time{}, err
	}
	for _, t := range timers {
		if nextTimer.IsZero() || t.FireAt.Before(nextTimer) {
			nextTimer = t.FireAt
		}
	}
	for code, n := range counts {
		if n > 0 {
			reasons = append(reasons, QuiescenceReason{Code: code, Count: n})
		}
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i].Code < reasons[j].Code })
	return reasons, nextTimer, nil
}

// countUnjudgedInTx counts the active Waits and subscriptions with accepted
// Events after their watermark: the next pass has a wake to decide. A blocked
// Wait is left out, since only an operator's release moves it.
func countUnjudgedInTx(tx storage.Tx, counts map[string]int) error {
	waits, err := tx.ActiveWaits()
	if err != nil {
		return err
	}
	for _, w := range waits {
		var meta waitMetadata
		if err = getJSON(tx, waitNamespace, identity(string(w.ID)), &meta); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return err
		}
		if meta.Blocked {
			continue
		}
		// The wake scan passes over a Wait whose Work has ended.
		work, e := tx.Work(w.WorkID)
		if e != nil {
			return e
		}
		if work.State.Terminal() {
			continue
		}
		behind, e := tx.Events(max(w.AfterSequence, meta.Scanned), 1)
		if e != nil {
			return e
		}
		if len(behind) > 0 {
			counts[QuiescentUnjudgedWait]++
		}
	}
	rows, err := tx.List(subscriptionNamespace, "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		var sub subscription
		if err = json.Unmarshal(row.Value, &sub); err != nil {
			return err
		}
		if !sub.Active {
			continue
		}
		behind, e := tx.Events(sub.Checkpoint, 1)
		if e != nil {
			return e
		}
		if len(behind) > 0 {
			counts[QuiescentPendingSubscription]++
		}
	}
	return nil
}
