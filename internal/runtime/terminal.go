package runtime

import (
	"context"
	"encoding/json"
	"errors"
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

// TerminalSession is the durable record of an open terminal. PlacedHold says
// the terminal placed the Work's hold (under OpenEvent) and so removes it;
// a hold that was already there is left as it was.
type TerminalSession struct {
	ID         string    `json:"id"`
	WorkID     domain.ID `json:"work_id"`
	Actor      string    `json:"actor,omitempty"`
	Via        string    `json:"via,omitempty"`
	OpenEvent  domain.ID `json:"open_event"`
	PlacedHold bool      `json:"placed_hold,omitempty"`
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
func (c *Control) OpenTerminal(ctx context.Context, workID domain.ID, session string) (TerminalSession, error) {
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
		out = TerminalSession{ID: session, WorkID: w.ID, Actor: ActorFrom(ctx), Via: ViaFrom(ctx), OpenEvent: event, PlacedHold: !held, At: now}
		if e = putJSON(tx, terminalNamespace, identity(string(w.ID)), out); e != nil {
			return e
		}
		return tx.PutWork(w, w.Revision)
	})
	return out, err
}

// CloseTerminal ends the session's record and commits
// core.manual.terminal_closed, removing the hold the terminal placed unless
// keepHold is set. Closing a session that is no longer the open one changes
// nothing, so a retry is safe.
func (c *Control) CloseTerminal(ctx context.Context, t TerminalSession, reason string, keepHold bool) error {
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
		release := open.PlacedHold && !keepHold && held && h.EventID == open.OpenEvent
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

// RecoverTerminals closes every recorded terminal. A terminal lives in the
// daemon's process, so at startup none of them is still connected. Each is
// closed under the operator who opened it. Call it once per process, never
// on a configuration reload, when terminals are still live.
func (c *Control) RecoverTerminals(ctx context.Context) (int, error) {
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
			open = append(open, t)
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
		if err := c.CloseTerminal(cx, t, "daemon restarted", false); err != nil {
			errs = append(errs, err)
			continue
		}
		closed++
	}
	return closed, errors.Join(errs...)
}
