package runtime

import (
	"context"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// ActionEvent is the Event an authorised extension action produces
// (extension.ActionPlan). Only the action path accepts Events under
// extension.ActionSource; Type, key and data are the provider's, while the
// delivery identity (provider, action and the caller's request key), the
// source and the causal root are assigned here.
type ActionEvent struct {
	ScopeID, WorkID  domain.ID
	Provider, Action string
	Key              string
	Event            domain.Event
}

// SubmitAction accepts an action's Event as an operator root of its own
// chain; a replay of the same request key returns the original.
func (c *Control) SubmitAction(ctx context.Context, a ActionEvent) (accepted domain.AcceptedEvent, added bool, err error) {
	delivery := a.Provider + "/" + a.Action + "/" + a.Key
	if a.ScopeID == "" || a.Provider == "" || a.Action == "" || !validManualType(a.Event.Type) || !validDelivery(a.Key) || !validDelivery(delivery) {
		return accepted, false, storage.ErrInvalid
	}
	var refusal error
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := deliveryDuplicate(tx, domain.Event{Source: extension.ActionSource, DeliveryID: delivery}); e != nil || dup {
			accepted = old
			if e == nil {
				if s, er := scopeForEvent(tx, old); er != nil || s != a.ScopeID || old.Event.Type != a.Event.Type {
					return storage.ErrConflict
				}
			}
			return e
		}
		if _, e := tx.Scope(a.ScopeID); e != nil {
			return e
		}
		if a.WorkID != "" {
			w, e := tx.Work(a.WorkID)
			if e != nil {
				return e
			}
			if w.ScopeID != a.ScopeID {
				return storage.ErrNotFound
			}
		}
		now := c.now()
		e := domain.Event{ID: domain.ID("action/" + identity(delivery)), Type: a.Event.Type, Source: extension.ActionSource, DeliveryID: delivery, OccurredAt: now, ReceivedAt: now, CorrelationKey: a.Event.CorrelationKey, WorkID: a.WorkID, Data: a.Event.Data}
		var err error
		accepted, added, err = AcceptInTx(tx, a.ScopeID, e)
		if errors.Is(err, ErrCausalRejected) {
			refusal = err
			return nil
		}
		return err
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
