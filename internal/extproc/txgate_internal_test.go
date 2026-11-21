package extproc

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

func heldCount(g *txGate, r *txRound) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return r.held
}

// awaitHeld returns once a callback is held in r, failing if the callback
// was answered instead of held.
func awaitHeld(t *testing.T, g *txGate, r *txRound, answered <-chan error) {
	t.Helper()
	for heldCount(g, r) == 0 {
		select {
		case err := <-answered:
			t.Fatalf("callback answered during the in-transaction call instead of held: %v", err)
		default:
			runtime.Gosched()
		}
	}
}

// TestCallbackOverlappingAnInTxCallIsServed (#839): a call from another of
// the extension's handlers that lands while the host waits on an
// in-transaction method is served once that method is answered, not refused.
func TestCallbackOverlappingAnInTxCallIsServed(t *testing.T) {
	g := &txGate{}
	s := &Service{enabled: map[string]bool{"state.sequence": true}, hold: g.hold}
	r := g.begin()
	answered := make(chan error, 1)
	go func() {
		_, err := s.handle(context.Background(), "state.sequence", []byte(`{}`))
		answered <- err
	}()
	awaitHeld(t, g, r, answered)
	if g.end(r, false) {
		t.Fatal("an answered call was reported reentrant")
	}
	// This Service has no store, so reaching the method answers unsupported.
	if err := <-answered; protocol.ErrorCode(err) != protocol.CodeUnsupported {
		t.Fatalf("overlapping callback: %v, want it to reach the method", err)
	}
}

// TestCallbackHeldThroughAnExpiredInTxCallIsRefused: when the in-transaction
// call runs out its deadline with a callback held, both are reentrant.
func TestCallbackHeldThroughAnExpiredInTxCallIsRefused(t *testing.T) {
	g := &txGate{}
	r := g.begin()
	answered := make(chan error, 1)
	go func() { answered <- g.hold(context.Background()) }()
	awaitHeld(t, g, r, answered)
	if !g.end(r, true) {
		t.Fatal("an expired call with a callback held was not reported reentrant")
	}
	if err := <-answered; protocol.ErrorCode(err) != protocol.CodeReentrant {
		t.Fatalf("held callback: %v, want reentrant_call", err)
	}
}

// TestExpiredInTxCallWithNoCallbackIsNotReentrant: a handler that merely
// hangs stays a timeout.
func TestExpiredInTxCallWithNoCallbackIsNotReentrant(t *testing.T) {
	g := &txGate{}
	if g.end(g.begin(), true) {
		t.Fatal("a hung call with no callback was reported reentrant")
	}
	if err := g.hold(context.Background()); err != nil {
		t.Fatalf("callback after the round ended: %v", err)
	}
}

// TestHeldCallbackEndsWithItsContext: a cancelled callback stops waiting and
// no longer counts against the in-transaction call.
func TestHeldCallbackEndsWithItsContext(t *testing.T) {
	g := &txGate{}
	r := g.begin()
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() { answered <- g.hold(ctx) }()
	awaitHeld(t, g, r, answered)
	cancel()
	if err := <-answered; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled callback: %v", err)
	}
	if g.end(r, true) {
		t.Fatal("a callback that left was counted as reentrant")
	}
}

// TestOverlappingInTxCallsShareARound: a held callback waits for every call
// under way, and one expiring refuses it even if another was answered.
func TestOverlappingInTxCallsShareARound(t *testing.T) {
	g := &txGate{}
	first, second := g.begin(), g.begin()
	answered := make(chan error, 1)
	go func() { answered <- g.hold(context.Background()) }()
	awaitHeld(t, g, first, answered)
	g.end(first, false)
	select {
	case err := <-answered:
		t.Fatalf("callback released with a call still under way: %v", err)
	default:
	}
	if !g.end(second, true) {
		t.Fatal("the expired call was not reported reentrant")
	}
	if err := <-answered; protocol.ErrorCode(err) != protocol.CodeReentrant {
		t.Fatalf("held callback: %v, want reentrant_call", err)
	}
}
