package extproc

import (
	"context"
	"sync"

	"github.com/open-conduit/open-conduit/protocol"
)

// txGate decides what happens to a callback from an extension while the host
// is waiting on one of that extension's in-transaction methods. The wire does
// not say which request a callback was made for, so one that arrives then is
// either the in-transaction handler calling back, which the protocol forbids,
// or an unrelated handler of the same process that happens to overlap it.
//
// The call's own end tells them apart. The callback is held until the
// in-transaction calls under way have ended. If they were answered, none of
// them was waiting on it, so it is served. If one ran out its deadline with a
// callback held, that callback may be what it was stuck on: the callback and
// the call are both refused with reentrant_call. The wait is bounded by
// protocol.InTransactionDeadlineMillis.
type txGate struct {
	mu    sync.Mutex
	round *txRound
	calls int
}

// txRound is one unbroken stretch with an in-transaction call outstanding.
type txRound struct {
	done    chan struct{}
	held    int
	refused bool
}

// begin records an in-transaction call and returns its round, for end.
func (g *txGate) begin() *txRound {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls == 0 {
		g.round = &txRound{done: make(chan struct{})}
	}
	g.calls++
	return g.round
}

// end records that a call of round r returned; expired says its deadline or
// context ended it unanswered. It reports whether that refused a callback
// held in the round, which makes the call itself a reentrant one.
func (g *txGate) end(r *txRound, expired bool) (reentrant bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if expired && r.held > 0 {
		r.refused, reentrant = true, true
	}
	if g.calls--; g.calls == 0 {
		close(r.done)
		g.round = nil
	}
	return reentrant
}

// hold waits out the in-transaction calls under way, if any, and returns nil
// when the callback may be served.
func (g *txGate) hold(ctx context.Context) error {
	g.mu.Lock()
	r := g.round
	if r == nil {
		g.mu.Unlock()
		return nil
	}
	r.held++
	g.mu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
		g.mu.Lock()
		r.held--
		g.mu.Unlock()
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.refused {
		return errReentrant()
	}
	return nil
}

func errReentrant() error {
	return protocol.Errorf(protocol.CodeReentrant, "callback while serving an in-transaction method")
}
