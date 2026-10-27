package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// turnEndedInTx publishes the turn-ended Event of an execution whose outcome
// declares a turn (domain.TurnSchema), in the transition that stores the
// outcome: one per correlation key of the Work, derived from the attempt's
// trigger, so another Work can wait on the key for a state. The delivery
// identity is the execution's lease epoch, so a repeated transition (a
// parked attempt's acknowledgement) is a no-op while a resumed attempt's
// next turn is a new Event. The Event is input to no Work: it is recorded as
// unmatched and reaches waiters only through their dependency subscriptions.
func (s *Scheduler) turnEndedInTx(tx storage.Tx, x domain.Execution, now time.Time) error {
	d, ok := domain.TurnOf(x.Outcome)
	if !ok || !domain.ValidTurnState(d.Status) {
		return nil
	}
	w, err := tx.Work(x.WorkID)
	if err != nil {
		return err
	}
	trigger, err := executionTriggerInTx(tx, x)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(domain.TurnEnded{Status: d.Status, Work: string(w.ID), Execution: string(x.ID)})
	keys := make([]*domain.CorrelationKey, 0, len(w.CorrelationKeys))
	for i := range w.CorrelationKeys {
		keys = append(keys, &w.CorrelationKeys[i])
	}
	if len(keys) == 0 {
		keys = append(keys, nil)
	}
	for _, k := range keys {
		delivery := fmt.Sprintf("turn/%s/%d", x.ID, x.Lease.Epoch)
		if k != nil {
			delivery += "/" + identity(k.Source, k.ExternalID)
		}
		e := domain.Event{ID: domain.ID("turn-event/" + identity(delivery)), Type: domain.TypeTurnEnded, Source: "core.execution", DeliveryID: delivery,
			CorrelationKey: k, WorkID: w.ID, ExecutionID: x.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: domain.SchemaTurnEnded, Data: data}}
		accepted, added, err := AcceptDerivedInTx(tx, now, s.options.CausalLimits, w.ScopeID, trigger, e)
		if errors.Is(err, ErrCausalRejected) {
			continue
		}
		if err != nil {
			return err
		}
		if added {
			if err = tx.MarkRouted(accepted.Event.ID, "", "unmatched"); err != nil {
				return err
			}
		}
	}
	return nil
}
