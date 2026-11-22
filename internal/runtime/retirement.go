package runtime

import (
	"errors"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Why an Event must stay in the live log (RetirementGuard.Blocked).
const (
	RetireBlockedUnrouted   = "unrouted"
	RetireBlockedRetired    = "already_retired"
	RetireBlockedReroutable = "reroutable"
	RetireBlockedControl    = "control_replay"
	RetireBlockedCreation   = "work_creation"
	RetireBlockedOnce       = "once_emission"
	RetireBlockedLiveWork   = "unfinished_work"
	RetireBlockedAttempt    = "attempt_input"
	RetireBlockedWaitScan   = "wait_scan"
)

// RetirementGuard says which Events the live system no longer decides with,
// so their payload may leave the log. Build it in the transaction that
// retires them, so nothing it read can change before they go.
//
// It keeps an Event whose payload a later decision reads from the Tx:
//   - one an operator can still route again, whose copy carries the payload;
//   - an operator control or action, whose replay answers from its payload;
//   - a Work's creation Event, which a repeated creation is compared with;
//   - a once-only emission, which a repeat is compared with;
//   - any Event of unfinished Work, and the trigger, inputs and wake of its
//     attempts, wherever they were routed;
//   - anything past the oldest position an active Wait or dependency has yet
//     to scan, which could still wake it.
type RetirementGuard struct {
	live, attempts, once map[domain.ID]bool
	scanned              uint64
	bounded              bool
}

// NewRetirementGuardInTx reads what the live system still needs. Its cost
// follows the live Work and the once-only emissions, not retained history.
func NewRetirementGuardInTx(tx storage.Tx) (*RetirementGuard, error) {
	g := &RetirementGuard{live: map[domain.ID]bool{}, attempts: map[domain.ID]bool{}, once: map[domain.ID]bool{}}
	works, err := tx.LiveWorks()
	if err != nil {
		return nil, err
	}
	for _, w := range works {
		g.live[w.ID] = true
		xs, err := tx.ListExecutions(w.ID)
		if err != nil {
			return nil, err
		}
		for _, x := range xs {
			g.need(x.TriggerID)
			state, err := attemptState(tx, x.ID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			g.need(state.WakeEventID)
			for _, id := range state.InputEventIDs {
				g.need(id)
			}
		}
		waits, err := tx.ListWaits(w.ID)
		if err != nil {
			return nil, err
		}
		for _, wait := range waits {
			if wait.State != domain.WaitActive {
				continue
			}
			var meta waitMetadata
			if err = getJSON(tx, waitNamespace, identity(string(wait.ID)), &meta); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return nil, err
			}
			g.bound(max(wait.AfterSequence, meta.Scanned))
		}
	}
	subscriptions, err := tx.List(subscriptionNamespace, "")
	if err != nil {
		return nil, err
	}
	for _, row := range subscriptions {
		var sub subscription
		if err = getJSON(tx, subscriptionNamespace, row.Key, &sub); err != nil {
			return nil, err
		}
		if sub.Active {
			g.bound(sub.Checkpoint)
		}
	}
	emitted, err := tx.List(onceNamespace, "")
	if err != nil {
		return nil, err
	}
	for _, row := range emitted {
		g.once[domain.ID(row.Value)] = true
	}
	return g, nil
}

func (g *RetirementGuard) need(id domain.ID) {
	if id != "" {
		g.attempts[id] = true
	}
}

func (g *RetirementGuard) bound(sequence uint64) {
	if !g.bounded || sequence < g.scanned {
		g.scanned, g.bounded = sequence, true
	}
}

// Blocked is why a must stay in the live log, empty when it may be retired.
func (g *RetirementGuard) Blocked(a domain.AcceptedEvent) string {
	e := a.Event
	switch {
	case a.Retired:
		return RetireBlockedRetired
	case !a.Routed:
		return RetireBlockedUnrouted
	case a.Disposition == DispositionFailed || a.Disposition == DispositionSkipped:
		return RetireBlockedReroutable
	case e.Source == ManualSource || e.Source == extension.ActionSource:
		return RetireBlockedControl
	case e.Source == domain.LineageSource && strings.HasPrefix(e.DeliveryID, "created/"):
		return RetireBlockedCreation
	case g.once[e.ID]:
		return RetireBlockedOnce
	case g.live[e.WorkID] || g.live[a.RoutedWorkID]:
		return RetireBlockedLiveWork
	case g.attempts[e.ID]:
		return RetireBlockedAttempt
	case g.bounded && a.Sequence > g.scanned:
		return RetireBlockedWaitScan
	}
	return ""
}
