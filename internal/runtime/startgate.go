package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// HoldKindGate marks a hold a start gate (extension.StartGate) placed on
// Work before its first attempt. Only the gate's registration settles it
// (Control.SettleGateHold); an operator can cancel the Work but not release
// the hold.
const HoldKindGate = "gate"

// gatePassNamespace holds, per Work, the settlement that lets its next
// start through without asking the start gates again.
const gatePassNamespace = "core.gate.pass"

var (
	// ErrGateHeld is a start refused because a start gate held the Work.
	ErrGateHeld = fmt.Errorf("%w: held by a start gate", ErrNotEligible)
	// ErrStartGate is a start refused because a start gate could not
	// decide; it is asked again on the next pass.
	ErrStartGate = fmt.Errorf("%w: a start gate could not decide", ErrNotEligible)
	// ErrGateHold is an operator release of a hold a start gate placed:
	// only the gate's registration settles it.
	ErrGateHold = fmt.Errorf("%w: a start gate holds this Work; answer its request, or cancel the Work", ErrHeld)
)

// gatePassedInTx reports whether a settlement lets Work's next start
// through its start gates.
func gatePassedInTx(tx storage.Tx, work domain.ID) (bool, error) {
	var pass domain.ID
	err := getJSON(tx, gatePassNamespace, identity(string(work)), &pass)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// holdForGateInTx records a start gate's hold on Work: a core.gate.held
// Event in the Work's history, naming the gate and its reason, and the hold.
func holdForGateInTx(tx storage.Tx, now time.Time, w domain.Work, gate, reason string) error {
	data, _ := json.Marshal(struct {
		Gate   string `json:"gate"`
		Reason string `json:"reason"`
	}{gate, reason})
	delivery := "held/" + identity(string(w.ID), gate, now.UTC().Format(time.RFC3339Nano))
	e := domain.Event{ID: domain.ID("gate-" + delivery), Type: "core.gate.held", Source: "core.gate", DeliveryID: delivery, WorkID: w.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.gate.held.v1", Data: data}}
	accepted, _, err := AcceptInTx(tx, w.ScopeID, e)
	if err != nil {
		return err
	}
	if err = tx.MarkRouted(accepted.Event.ID, "", "control"); err != nil {
		return err
	}
	if err = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: accepted.Event.ID, Reason: reason, At: now.UTC(), Kind: HoldKindGate, Holder: gate}); err != nil {
		return err
	}
	return tx.PutWork(w, w.Revision)
}

// admitStart asks the start gates about Work's first attempt, outside any
// transaction.
func (s *Scheduler) admitStart(ctx context.Context, w domain.Work, binding string, input domain.Event) (*extension.StartHold, string, error) {
	hold, gate, err := s.options.Toolbelt.AdmitStart(ctx, extension.StartCall{Work: w, Binding: binding, Event: input})
	if err != nil {
		return nil, gate, fmt.Errorf("%w: %s: %v", ErrStartGate, gate, err)
	}
	if hold != nil && hold.Reason == "" {
		hold.Reason = "held by " + gate + " before it starts"
	}
	return hold, gate, nil
}

// SettleGateHold settles the hold the start gate of registration gate
// placed on Work: admit lets its next start through once, without asking
// the start gates again; otherwise the Work is cancelled. Either way a
// core.gate.released or core.gate.refused Event records it with the reason.
// It is idempotent per key. A Work that gate does not hold is ErrHeld.
func (c *Control) SettleGateHold(ctx context.Context, gate string, workID domain.ID, admit bool, reason, key string) (event domain.ID, err error) {
	if !validDelivery(key) || gate == "" {
		return "", storage.ErrInvalid
	}
	delivery := "gate-settle/" + gate + "/" + key
	err = c.transact(ctx, func(tx storage.Tx) error {
		if old, dup, e := prior(tx, delivery); e != nil || dup {
			if e == nil && old.Event.WorkID != workID {
				return storage.ErrConflict
			}
			event = old.Event.ID
			return e
		}
		w, e := tx.Work(workID)
		if e != nil {
			return e
		}
		h, held, e := HoldInTx(tx, w.ID)
		if e != nil {
			return e
		}
		if !held || h.Kind != HoldKindGate || h.Holder != gate {
			return ErrHeld
		}
		typ := "core.gate.refused"
		if admit {
			typ = "core.gate.released"
		}
		data, _ := json.Marshal(struct {
			Gate   string `json:"gate"`
			Reason string `json:"reason,omitempty"`
		}{gate, reason})
		event, e = c.controlEvent(tx, w, delivery, typ, domain.Payload{Schema: typ + ".v1", Data: data})
		if e != nil {
			return e
		}
		if e = tx.Delete(holdNamespace, identity(string(w.ID))); e != nil {
			return e
		}
		if !admit {
			return cancelWorkInTx(tx, w.ID)
		}
		if e = putJSON(tx, gatePassNamespace, identity(string(w.ID)), event); e != nil {
			return e
		}
		return tx.PutWork(w, w.Revision)
	})
	return
}
