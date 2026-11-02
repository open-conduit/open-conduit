package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Operator controls for an Event routing is stuck on or gave up on (issue
// #342). Each commits an accepted core.manual control event naming the
// Event, so the action shows in the Work's history beside it.

type eventControlPayload struct {
	Reason  string    `json:"reason"`
	EventID domain.ID `json:"event_id"`
	// RoutedAs is the copy a reroute accepted, when it made one.
	RoutedAs domain.ID `json:"routed_as,omitempty"`
}

// eventScopeInTx is the Scope an Event was accepted into, even when its
// identity no longer resolves (the Work it named is gone).
func eventScopeInTx(tx storage.Tx, a domain.AcceptedEvent) (domain.ID, error) {
	if scope, err := scopeForEvent(tx, a); err == nil && scope != "" {
		return scope, nil
	}
	var scope domain.ID
	err := getJSON(tx, acceptanceNamespace, identity(string(a.Event.ID)), &scope)
	if errors.Is(err, storage.ErrNotFound) || err == nil && scope == "" {
		return "", storage.ErrInvalid
	}
	return scope, err
}

// eventControlInTx accepts the control event recording an action on a.
func (c *Control) eventControlInTx(tx storage.Tx, a domain.AcceptedEvent, delivery, typ string, payload eventControlPayload) (domain.ID, domain.ID, error) {
	scope, err := eventScopeInTx(tx, a)
	if err != nil {
		return "", "", err
	}
	var work domain.ID
	if w, e := routeOwner(tx, a.Event); e == nil && w.ScopeID == scope {
		work = w.ID
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	accepted, _, err := c.acceptInTx(tx, scope, ManualEvent{WorkID: work, DeliveryID: delivery, Data: domain.Payload{Schema: typ + ".v1", Data: b}}, typ)
	if err != nil {
		return "", "", err
	}
	return accepted.Event.ID, work, tx.MarkRouted(accepted.Event.ID, "", "control")
}

// SkipEvent routes an unrouted Event as skipped, so the Work it holds can
// take its later input. The routes are not asked about it again.
func (c *Control) SkipEvent(ctx context.Context, event domain.ID, reason, key string) (control domain.ID, err error) {
	if !validDelivery(key) {
		return "", storage.ErrInvalid
	}
	delivery := "skip-event/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			control = old.Event.ID
			return e
		}
		a, e := tx.Event(event)
		if e != nil {
			return e
		}
		if a.Routed {
			return storage.ErrConflict
		}
		var work domain.ID
		if control, work, e = c.eventControlInTx(tx, a, delivery, "core.manual.skip_event", eventControlPayload{Reason: reason, EventID: event}); e != nil {
			return e
		}
		rec := RouteRecord{EventID: event, WorkID: work, Generation: c.router.options.Generation, Policy: DispositionSkipped, Reason: "operator", Error: reason}
		if f, ok, e := RouteFailureInTx(tx, event); e != nil {
			return e
		} else if ok {
			rec.Route, rec.Attempts = f.Route, f.Attempts
		}
		if e = putJSON(tx, routeNamespace, identity(string(event)), rec); e != nil {
			return e
		}
		if e = tx.Delete(routeFailureNamespace, routeFailureKey(event)); e != nil {
			return e
		}
		return tx.MarkRouted(event, work, DispositionSkipped)
	})
	return
}

// RerouteEvent routes an Event again. One still unrouted has its failures
// forgotten, so the next routing pass tries it at once; one routing gave up
// on or that was skipped is accepted again as a copy (the same envelope
// under a new ID), which is then routed like any new input. routed is the
// Event that will be routed.
func (c *Control) RerouteEvent(ctx context.Context, event domain.ID, reason, key string) (control, routed domain.ID, err error) {
	if !validDelivery(key) {
		return "", "", storage.ErrInvalid
	}
	delivery := "reroute-event/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			if e != nil {
				return e
			}
			var p eventControlPayload
			_ = json.Unmarshal(old.Event.Data.Data, &p)
			control, routed = old.Event.ID, p.RoutedAs
			if routed == "" {
				routed = p.EventID
			}
			return nil
		}
		a, e := tx.Event(event)
		if e != nil {
			return e
		}
		routed = event
		switch {
		case !a.Routed:
			if e = tx.Delete(routeFailureNamespace, routeFailureKey(event)); e != nil {
				return e
			}
		case a.Disposition == DispositionFailed || a.Disposition == DispositionSkipped:
			scope, e := eventScopeInTx(tx, a)
			if e != nil {
				return e
			}
			copied := a.Event
			copied.ID = domain.ID("reroute/" + identity(string(event), key))
			copied.DeliveryID = "reroute/" + identity(string(event), key)
			copied.ReceivedAt = c.now()
			accepted, _, e := AcceptInTx(tx, scope, copied)
			if e != nil {
				return e
			}
			routed = accepted.Event.ID
		default:
			return storage.ErrConflict
		}
		payload := eventControlPayload{Reason: reason, EventID: event}
		if routed != event {
			payload.RoutedAs = routed
		}
		control, _, e = c.eventControlInTx(tx, a, delivery, "core.manual.reroute_event", payload)
		return e
	})
	return
}
