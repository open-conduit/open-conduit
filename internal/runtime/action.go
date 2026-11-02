package runtime

import (
	"context"
	"errors"
	"strings"

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

// Act authorises call with its provider and accepts the Event it plans,
// under key (a replay of the same key returns the first Event). It is the
// one action path: the management API invokes it as the local operator,
// and an extension acting for a person on its own surface (Host.Act) with
// that person as caller. A refusal is the provider's *OperationError and
// leaves no Event. The message is the provider's, trimmed to 500 bytes.
func (c *Control) Act(ctx context.Context, name string, p extension.ActionProvider, call extension.ActionCall, key string) (accepted domain.AcceptedEvent, added bool, message string, err error) {
	plan, err := p.Invoke(ctx, call)
	if err != nil {
		return accepted, false, "", err
	}
	a := ActionEvent{ScopeID: call.Target.Scope.ID, Provider: name, Action: call.Action, Key: key, Event: plan.Event}
	// An Event that names a correlation key is addressed by it (an answer
	// on a key no Work owns, say); one without is the target Work's.
	if call.Target.Work != nil && plan.Event.CorrelationKey == nil {
		a.WorkID = call.Target.Work.ID
	}
	accepted, added, err = c.SubmitAction(ctx, a)
	if err != nil {
		return accepted, false, "", err
	}
	message = strings.ToValidUTF8(plan.Message, "")
	if len(message) > 500 {
		message = strings.ToValidUTF8(message[:500], "")
	}
	return accepted, added, message, nil
}
