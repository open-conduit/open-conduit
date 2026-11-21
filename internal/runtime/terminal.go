package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// An operator's terminal on a Work's session, streamed through the host.
// Unlike an attach the operator runs themselves, its lifetime is the host's
// to manage: the record below owns the hold it placed, so a client that
// vanishes, or a daemon that dies with it, can't leave the Work held.

const terminalNamespace = "core.manual.terminal"

// ErrTerminalOpen means a terminal is open on the Work: a second one, a
// local attach or a release would put two writers on one session.
var ErrTerminalOpen = errors.New("a terminal is open on this Work")

// TerminalSession is the durable record of an open terminal, and while it
// exists no attempt of the Work starts, whatever becomes of the Work's hold
// (heldInTx). PlacedHold says the terminal placed the hold (under OpenEvent)
// and so removes it, unless KeepHeld; a hold that was already there is left
// as it was. Stranded means its process is gone but what it ran in may not
// be: the record stays, keeping the session to itself, until SweepTerminals
// has removed that.
type TerminalSession struct {
	ID         string    `json:"id"`
	WorkID     domain.ID `json:"work_id"`
	Actor      string    `json:"actor,omitempty"`
	Via        string    `json:"via,omitempty"`
	OpenEvent  domain.ID `json:"open_event"`
	PlacedHold bool      `json:"placed_hold,omitempty"`
	KeepHeld   bool      `json:"keep_held,omitempty"`
	Stranded   bool      `json:"stranded,omitempty"`
	At         time.Time `json:"at"`
}

type terminalPayload struct {
	Session  string `json:"session"`
	Reason   string `json:"reason,omitempty"`
	HoldKept bool   `json:"hold_kept,omitempty"`
}

func terminalData(p terminalPayload) domain.Payload {
	b, _ := json.Marshal(p)
	return domain.Payload{Schema: "core.manual.terminal.v1", Data: b}
}

// TerminalInTx returns the Work's open terminal, if any.
func TerminalInTx(tx storage.Tx, work domain.ID) (TerminalSession, bool, error) {
	var t TerminalSession
	err := getJSON(tx, terminalNamespace, identity(string(work)), &t)
	if errors.Is(err, storage.ErrNotFound) {
		return TerminalSession{}, false, nil
	}
	return t, err == nil, err
}

// OpenTerminal records a terminal on nonterminal Work with no pending or
// running attempt, holding the Work unless it is already held, and commits
// core.manual.terminal_opened under the caller's actor in the same
// transaction.
func (c *Control) OpenTerminal(ctx context.Context, workID domain.ID, session string, keepHeld bool) (TerminalSession, error) {
	if !validDelivery(session) {
		return TerminalSession{}, storage.ErrInvalid
	}
	var out TerminalSession
	err := c.transact(ctx, func(tx storage.Tx) error {
		w, e := tx.Work(workID)
		if e != nil {
			return e
		}
		if w.State.Terminal() || w.State == domain.WorkRunning {
			return storage.ErrConflict
		}
		if e = noLiveAttempt(tx, w.ID, true); e != nil {
			return e
		}
		if _, open, e := TerminalInTx(tx, w.ID); e != nil {
			return e
		} else if open {
			return ErrTerminalOpen
		}
		_, held, e := HoldInTx(tx, w.ID)
		if e != nil {
			return e
		}
		event, e := c.controlEvent(ctx, tx, w, "terminal-open/"+session, "core.manual.terminal_opened", terminalData(terminalPayload{Session: session}))
		if e != nil {
			return e
		}
		now := c.now().UTC()
		if !held {
			if e = putJSON(tx, holdNamespace, identity(string(w.ID)), Hold{EventID: event, Reason: "operator terminal", At: now}); e != nil {
				return e
			}
		}
		out = TerminalSession{ID: session, WorkID: w.ID, Actor: ActorFrom(ctx), Via: ViaFrom(ctx), OpenEvent: event, PlacedHold: !held, KeepHeld: keepHeld, At: now}
		if e = putJSON(tx, terminalNamespace, identity(string(w.ID)), out); e != nil {
			return e
		}
		return tx.PutWork(w, w.Revision)
	})
	return out, err
}

// CloseTerminal ends the session's record and commits
// core.manual.terminal_closed, removing the hold the terminal placed unless
// it was opened to keep it. Closing a session that is no longer the open one
// changes nothing, so a retry is safe.
func (c *Control) CloseTerminal(ctx context.Context, t TerminalSession, reason string) error {
	return c.transact(ctx, func(tx storage.Tx) error {
		open, found, e := TerminalInTx(tx, t.WorkID)
		if e != nil || !found || open.ID != t.ID {
			return e
		}
		w, e := tx.Work(t.WorkID)
		if e != nil {
			return e
		}
		h, held, e := HoldInTx(tx, w.ID)
		if e != nil {
			return e
		}
		release := open.PlacedHold && !open.KeepHeld && held && h.EventID == open.OpenEvent
		kept := open.PlacedHold && !release && held && h.EventID == open.OpenEvent
		event, e := c.controlEvent(ctx, tx, w, "terminal-close/"+open.ID, "core.manual.terminal_closed", terminalData(terminalPayload{Session: open.ID, Reason: reason, HoldKept: kept}))
		if e != nil {
			return e
		}
		if e = tx.Delete(terminalNamespace, identity(string(w.ID))); e != nil {
			return e
		}
		if release {
			if e = tx.Delete(holdNamespace, identity(string(w.ID))); e != nil {
				return e
			}
		}
		if w.State.Terminal() {
			return nil
		}
		if e = tx.PutWork(w, w.Revision); e != nil {
			return e
		}
		if !release {
			return nil
		}
		return unblockWaitsInTx(tx, c.now(), c.limits, w.ID, event)
	})
}

// StrandTerminal marks a terminal whose process is gone but whose attach
// container could not be removed, and commits core.manual.terminal_stranded
// the first time. The Work stays out of dispatch until SweepTerminals clears
// it.
func (c *Control) StrandTerminal(ctx context.Context, t TerminalSession, reason string) error {
	return c.transact(ctx, func(tx storage.Tx) error {
		open, found, e := TerminalInTx(tx, t.WorkID)
		if e != nil || !found || open.ID != t.ID || open.Stranded {
			return e
		}
		w, e := tx.Work(t.WorkID)
		if e != nil {
			return e
		}
		if _, e = c.controlEvent(ctx, tx, w, "terminal-stranded/"+open.ID, "core.manual.terminal_stranded", terminalData(terminalPayload{Session: open.ID, Reason: reason})); e != nil {
			return e
		}
		open.Stranded = true
		return putJSON(tx, terminalNamespace, identity(string(w.ID)), open)
	})
}

// SweepTerminals finishes terminals no process is serving: every one on
// record when all is set (at startup, since a terminal lives in the daemon's
// process), else the stranded ones. detach removes what the terminal ran in;
// when it fails the terminal stays, stranded, for the next sweep. Each is
// closed under the operator who opened it. Never sweep all once this process
// serves terminals.
func (c *Control) SweepTerminals(ctx context.Context, all bool, detach func(context.Context, TerminalSession) error) (int, error) {
	var open []TerminalSession
	if err := c.transact(ctx, func(tx storage.Tx) error {
		open = nil
		rows, e := tx.List(terminalNamespace, "")
		if e != nil {
			return e
		}
		for _, row := range rows {
			var t TerminalSession
			if e = getJSON(tx, terminalNamespace, row.Key, &t); e != nil {
				return e
			}
			if all || t.Stranded {
				open = append(open, t)
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	var errs []error
	closed := 0
	for _, t := range open {
		cx := ctx
		if t.Actor != "" {
			cx = WithActor(cx, t.Actor)
		}
		if t.Via != "" {
			cx = WithVia(cx, t.Via)
		}
		if err := detach(cx, t); err != nil {
			errs = append(errs, fmt.Errorf("terminal on %s: %w", t.WorkID, err))
			if err = c.StrandTerminal(cx, t, "attach container not removed: "+err.Error()); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		reason := "daemon restarted"
		if t.Stranded {
			reason = "attach container removed"
		}
		if err := c.CloseTerminal(cx, t, reason); err != nil {
			errs = append(errs, err)
			continue
		}
		closed++
	}
	return closed, errors.Join(errs...)
}
