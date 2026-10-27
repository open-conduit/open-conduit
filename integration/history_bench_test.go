package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
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

// How the daemon's hot paths cost as retained history grows (#36). History
// is one operator's: events of about 1 KiB spread over threads of 25 events,
// every one routed, and each thread's Work finished by one attempt. 1k, 10k and 50k events are roughly
// days, months and a year or more. Run with `make bench-history`.
var historySizes = []int{1_000, 10_000, 50_000}

const historyThread = 25

type history struct {
	store   *sqlite.Store
	clock   *conformance.Clock
	router  *runtime.Router
	ingress *runtime.Ingress
	sched   *runtime.Scheduler
	timers  *runtime.Timers
	next    int
}

var historyPayload = domain.Payload{Schema: "fixture.github.v1", Data: []byte(`{"body":"` + strings.Repeat("x", 1000) + `"}`)}

func historyEvent(clock *conformance.Clock, id string, thread string) domain.Event {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: thread}
	return domain.Event{ID: domain.ID(id), Type: "fixture.changed", Source: "fixture/account", DeliveryID: id, OccurredAt: clock.Now(), ReceivedAt: clock.Now(), CorrelationKey: &key, Data: historyPayload}
}

// seedHistory writes n routed events and their finished Works straight
// through storage, which is far cheaper than routing each one.
func seedHistory(b *testing.B, n int) *history {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "history.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	clock := conformance.NewClock()
	guard := storage.Guard{Now: clock.Now()}
	route := domain.Route{Name: "default", ActivePolicy: "queue", Handler: "default", Binding: "fixture"}
	if err = store.Transact(ctx, guard, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "history-scope", Name: "history", Routes: []domain.Route{route}, Bindings: []domain.Binding{{Name: "fixture", Executor: "fixture"}}}, 0)
	}); err != nil {
		b.Fatal(err)
	}
	const batch = 1000
	for start := 0; start < n; start += batch {
		if err = store.Transact(ctx, guard, func(tx storage.Tx) error {
			for i := start; i < min(start+batch, n); i++ {
				thread := fmt.Sprintf("thread-%d", i/historyThread)
				if i%historyThread == 0 {
					w := domain.Work{ID: domain.ID("work/" + thread), Kind: "fixture.work", ScopeID: "history-scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: "fixture/account", ExternalID: thread}}}
					if e := tx.PutWork(w, 0); e != nil {
						return e
					}
				}
				id := fmt.Sprintf("history-%d", i)
				if _, _, e := runtime.AcceptInTx(tx, "history-scope", historyEvent(clock, id, thread)); e != nil {
					return e
				}
				if e := tx.MarkRouted(domain.ID(id), "", "seeded"); e != nil {
					return e
				}
				if i%historyThread == historyThread-1 || i == n-1 {
					// One finished attempt per thread, triggered by its first event.
					xid := domain.ID("execution/" + thread)
					trigger := domain.ID(fmt.Sprintf("history-%d", i/historyThread*historyThread))
					if e := tx.PutExecution(domain.Execution{ID: xid, WorkID: domain.ID("work/" + thread), ExecutorBinding: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: trigger}, 0); e != nil {
						return e
					}
					if _, e := tx.Claim(xid, "fixture-owner", time.Minute); e != nil {
						return e
					}
					x, e := tx.Execution(xid)
					if e != nil {
						return e
					}
					x.State, x.Outcome = domain.ExecutionCompleted, &domain.Outcome{Kind: domain.OutcomeCompleted}
					if e = tx.PutExecution(x, x.Revision); e != nil {
						return e
					}
					w, e := tx.Work(domain.ID("work/" + thread))
					if e != nil {
						return e
					}
					w.State = domain.WorkCompleted
					if e = tx.PutWork(w, w.Revision); e != nil {
						return e
					}
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	handler := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		return extension.RouteDecision{Match: true, CreateID: domain.ID("work/" + in.Event.Event.CorrelationKey.ExternalID), Kind: "fixture.work"}, nil
	})
	executor := routingFixtureExecutor{}
	h := &history{store: store, clock: clock}
	if h.router, err = runtime.NewRouter(store, clock.Now, runtime.RouterOptions{Generation: "fixture-generation", Handlers: map[string]extension.RouteHandler{"default": handler}, Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: executor}}}); err != nil {
		b.Fatal(err)
	}
	if h.ingress, err = runtime.NewIngress(store, clock.Now, runtime.IngressOptions{}); err != nil {
		b.Fatal(err)
	}
	if h.sched, err = runtime.NewScheduler(store, clock.Now, h.router, runtime.SchedulerOptions{Owner: "fixture-owner", LeaseDuration: time.Minute, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}}); err != nil {
		b.Fatal(err)
	}
	if h.timers, err = runtime.NewTimers(store, clock.Now, runtime.TimerOptions{}); err != nil {
		b.Fatal(err)
	}
	return h
}

// routePending is the daemon loop's routing pass (bootstrap.routePending):
// page through every accepted event and route the unrouted ones.
func (h *history) routePending(b *testing.B) {
	ctx := context.Background()
	var after uint64
	for {
		page, err := h.ingress.Pending(ctx, after, 200)
		if err != nil {
			b.Fatal(err)
		}
		for _, p := range page.Items {
			if err = h.router.Route(ctx, p.Event.Event.ID); err != nil {
				b.Fatal(err)
			}
		}
		if !page.More {
			return
		}
		after = page.NextSequence
	}
}

func benchHistory(b *testing.B, pass func(*testing.B, *history)) {
	for _, n := range historySizes {
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			h := seedHistory(b, n)
			b.ResetTimer()
			for range b.N {
				pass(b, h)
			}
		})
	}
}

// One new thread arrives: accept its first event and route it, which opens
// its Work and queues the input.
func BenchmarkHistoryAcceptRoute(b *testing.B) {
	benchHistory(b, func(b *testing.B, h *history) {
		h.next++
		id := fmt.Sprintf("new-%d", h.next)
		if _, _, err := h.ingress.Accept(context.Background(), "history-scope", historyEvent(h.clock, id, id)); err != nil {
			b.Fatal(err)
		}
		if err := h.router.Route(context.Background(), domain.ID(id)); err != nil {
			b.Fatal(err)
		}
	})
}

// An idle daemon loop pass with nothing to route or recover.
func BenchmarkHistoryRoutePending(b *testing.B) {
	benchHistory(b, func(b *testing.B, h *history) { h.routePending(b) })
}

func BenchmarkHistoryPoll(b *testing.B) {
	benchHistory(b, func(b *testing.B, h *history) {
		if _, err := h.sched.Poll(context.Background(), h.timers, 100); err != nil {
			b.Fatal(err)
		}
	})
}

// The read startReady makes each pass: every Work, in one transaction.
func BenchmarkHistoryListWorks(b *testing.B) {
	benchHistory(b, func(b *testing.B, h *history) {
		if err := h.store.Transact(context.Background(), storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error { _, e := tx.ListWorks(); return e }); err != nil {
			b.Fatal(err)
		}
	})
}
