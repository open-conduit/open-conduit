package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// A backlog is what an outage leaves: Events accepted while nothing routed
// them, spread over threads, each thread's first opening its Work and the
// rest queued behind it (#705).
const backlogThread = 10

// backlog is a store holding n unrouted Events over n/backlogThread
// threads, and a router and ingress over it as the daemon's.
type backlog struct {
	store   storage.Store
	router  *runtime.Router
	ingress *runtime.Ingress
}

func seedBacklog(tb testing.TB, inner storage.Store, n int) *backlog {
	tb.Helper()
	ctx := context.Background()
	clock := conformance.NewClock()
	guard := storage.Guard{Now: clock.Now()}
	route := domain.Route{Name: "default", ActivePolicy: "queue", Handler: "default", Binding: "fixture"}
	if err := inner.Transact(ctx, guard, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "backlog-scope", Name: "backlog", Routes: []domain.Route{route}, Bindings: []domain.Binding{{Name: "fixture", Executor: "fixture"}}}, 0)
	}); err != nil {
		tb.Fatal(err)
	}
	const batch = 1000
	for start := 0; start < n; start += batch {
		if err := inner.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, n); i++ {
				// Threads interleave, as deliveries from many
				// repositories and channels do.
				thread := "thread-" + strconv.Itoa(i%max(n/backlogThread, 1))
				if _, _, e := runtime.AcceptInTx(tx, "backlog-scope", historyEvent(clock, "backlog-"+strconv.Itoa(i), thread)); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			tb.Fatal(err)
		}
	}
	handler := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		return extension.RouteDecision{Match: true, CreateID: domain.ID("work/" + in.Event.Event.CorrelationKey.ExternalID), Kind: "fixture.work"}, nil
	})
	b := &backlog{store: inner}
	var err error
	if b.router, err = runtime.NewRouter(inner, clock.Now, runtime.RouterOptions{Generation: "fixture-generation", Handlers: map[string]extension.RouteHandler{"default": handler}, Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: routingFixtureExecutor{}}}}); err != nil {
		tb.Fatal(err)
	}
	if b.ingress, err = runtime.NewIngress(inner, clock.Now, runtime.IngressOptions{}); err != nil {
		tb.Fatal(err)
	}
	return b
}

// drain is the daemon loop's routing pass over the backlog, calling each
// after every route.
func (b *backlog) drain(tb testing.TB, each func()) int {
	ctx := context.Background()
	var after uint64
	routed := 0
	for {
		page, err := b.ingress.Pending(ctx, after, 200)
		if err != nil {
			tb.Fatal(err)
		}
		for _, p := range page.Items {
			if err = b.router.Route(ctx, p.Event.Event.ID); err != nil {
				tb.Fatal(err)
			}
			each()
			routed++
		}
		if !page.More {
			return routed
		}
		after = page.NextSequence
	}
}

// readCounting counts the Event records every read of the unrouted backlog
// returns.
type readCounting struct {
	storage.Store
	read *int
	tx   func(storage.Tx) storage.Tx
}

func (s readCounting) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return s.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(s.tx(readCountingTx{tx, s.read})) })
}

type readCountingTx struct {
	storage.Tx
	read *int
}

func (t readCountingTx) UnroutedEvents(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	out, err := t.Tx.UnroutedEvents(after, limit)
	*t.read += len(out)
	return out, err
}
func (t readCountingTx) UnroutedInput(target storage.InputTarget, after, before uint64) ([]domain.AcceptedEvent, error) {
	out, err := t.Tx.UnroutedInput(target, after, before)
	*t.read += len(out)
	return out, err
}
func (t readCountingTx) Events(after uint64, limit int) ([]domain.AcceptedEvent, error) {
	out, err := t.Tx.Events(after, limit)
	*t.read += len(out)
	return out, err
}

// scanningInput is the negative control: a store that answers a Work's
// unrouted input by reading the whole unrouted backlog and keeping the
// Work's, as routing did before UnroutedInput (#705).
type scanningInput struct{ storage.Tx }

func (t scanningInput) UnroutedInput(target storage.InputTarget, after, before uint64) ([]domain.AcceptedEvent, error) {
	all, err := t.Tx.UnroutedEvents(after, 0)
	out := []domain.AcceptedEvent{}
	for _, a := range all {
		if before != 0 && a.Sequence >= before {
			break
		}
		if a.Event.WorkID == target.Work && target.Work != "" || a.Event.CorrelationKey != nil && slices.Contains(target.Keys, *a.Event.CorrelationKey) {
			out = append(out, a)
		}
	}
	return out, err
}

// TestBacklogDrainReadsEachWorksOwnInput: draining a backlog of unrouted
// Events, routing each Event reads its Work's own earlier input from
// storage, not the backlog. The most Event records any one route reads is
// bounded by a thread's length, whatever the backlog's size; a store that
// answers the read by scanning the backlog reads it all for the first
// route, and fails the bound.
func TestBacklogDrainReadsEachWorksOwnInput(t *testing.T) {
	const bound = 4 * backlogThread
	worst := func(n int, tx func(storage.Tx) storage.Tx) int {
		t.Helper()
		store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "backlog.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		var read, most int
		b := seedBacklog(t, readCounting{Store: store, read: &read, tx: tx}, n)
		// Only the routes' reads count, not the pass's pages of the
		// backlog: reset before each route, record after it.
		b.ingress, err = runtime.NewIngress(store, time.Now, runtime.IngressOptions{})
		if err != nil {
			t.Fatal(err)
		}
		read = 0
		if routed := b.drain(t, func() { most, read = max(most, read), 0 }); routed != n {
			t.Fatalf("drained %d Events, want %d", routed, n)
		}
		if err = store.Transact(context.Background(), storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
			left, e := tx.UnroutedEvents(0, 0)
			if e == nil && len(left) != 0 {
				e = fmt.Errorf("%d Events left unrouted", len(left))
			}
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return most
	}
	const n = 1_000
	if got := worst(n, func(tx storage.Tx) storage.Tx { return tx }); got > bound {
		t.Errorf("a route read %d Event records from a backlog of %d, want at most %d", got, n, bound)
	}
	// The control's backlog is smaller: scanning it costs its square.
	if got := worst(200, func(tx storage.Tx) storage.Tx { return scanningInput{tx} }); got <= bound {
		t.Errorf("control: a route over a scanning store read %d Event records, want more than %d", got, bound)
	}
}

// How long the daemon takes to drain a backlog of unrouted Events after an
// outage, on real SQLite (#705). Run with `make bench-backlog`;
// OC_BACKLOG_SIZES (comma-separated) picks other sizes.
func BenchmarkBacklogDrain(b *testing.B) {
	sizes := []int{1_000, 10_000}
	if v := os.Getenv("OC_BACKLOG_SIZES"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				b.Fatal("OC_BACKLOG_SIZES: " + f)
			}
			sizes = append(sizes, n)
		}
	}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				store, err := sqlite.Open(context.Background(), filepath.Join(b.TempDir(), "backlog.db"))
				if err != nil {
					b.Fatal(err)
				}
				bl := seedBacklog(b, store, n)
				b.StartTimer()
				bl.drain(b, func() {})
				b.StopTimer()
				store.Close()
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*n), "µs/event")
		})
	}
}
