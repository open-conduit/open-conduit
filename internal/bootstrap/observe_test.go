package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/storage"
)

// TestObserveReappliesTheResultAfterUnroutedInput (#645): an attempt whose
// run accepts input for its own Work (a Slack post's thread opening) just
// before it finishes can't complete until that input is routed. The daemon
// routes it and applies the attempt's result again, the same result: a
// handle answers once (an out-of-process executor forgets the handle as it
// answers), so waiting on it again failed the finished attempt. The
// control: the emission was still unrouted when the result came, so the
// first Complete was refused and the daemon had to route it.
func TestObserveReappliesTheResultAfterUnroutedInput(t *testing.T) {
	f, executor := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
	base := f.Driver.(*runtimetest.SchedulerDriver).RoutingDriver
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "post"}
	runtimetest.RequireAccept(t, f, "trigger/post", &key)
	runtimetest.RequireRoute(t, f.Driver, "trigger/post")
	var mu sync.Mutex
	waits := 0
	executor.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &runtimetest.Handle{WaitFn: func(context.Context) (extension.Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if waits++; waits > 1 {
				return extension.Result{}, errors.New("unknown handle")
			}
			// The run's own emission, accepted and not yet routed.
			runtimetest.RequireAccept(t, f, "thread/opened", &key)
			return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}, nil
		}}, nil
	}
	o := runtime.SchedulerOptions{Owner: "daemon", LeaseDuration: time.Hour, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}}
	s := runtimetest.NewScheduler(t, f, o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &daemon{o: Options{LeaseDuration: o.LeaseDuration}, coord: f.Store, now: f.Clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ingress: base.Ingress,
		reported: map[string]string{}, reportedAt: map[string]time.Time{}, inflight: map[attemptKey]domain.LeaseRef{}, ctx: ctx, nudge: make(chan struct{}, 1)}
	c := &components{scheduler: s, router: base.Router}
	d.current.Store(c)

	a, err := s.Start(ctx, "work/post")
	if err != nil {
		t.Fatal(err)
	}
	d.observe(ctx, c, a, make(chan struct{}))
	if waits != 1 {
		t.Fatalf("the handle was awaited %d times, want once", waits)
	}
	runtimetest.Read(t, f, func(tx storage.Tx) error {
		if r, err := tx.Event("thread/opened"); err != nil || !r.Routed {
			t.Errorf("control: the emission was not routed by the daemon: %+v %v", r, err)
		}
		x, err := tx.Execution(a.Lease.ExecutionID)
		if err != nil {
			return err
		}
		if x.State != domain.ExecutionCompleted {
			t.Errorf("the attempt is %s (%+v), want completed", x.State, x.Outcome)
		}
		return nil
	})
}
