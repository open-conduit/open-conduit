package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/storage"
)

// TestStartReadyHandsLaunchesOffTheLoop (#456): the loop's start step
// claims each ready Work and hands its launch off, so a launch that is
// stuck (a slow clone, a container runtime pulling an image) neither holds
// the loop nor the next Work's start. The control: the stuck launch really
// is blocked in its executor when the step returns.
func TestStartReadyHandsLaunchesOffTheLoop(t *testing.T) {
	f, executor := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
	for _, id := range []string{"stuck", "next"} {
		key := domain.CorrelationKey{Source: "fixture/account", ExternalID: id}
		runtimetest.RequireAccept(t, f, "trigger/"+id, &key)
		runtimetest.RequireRoute(t, f.Driver, domain.ID("trigger/"+id))
	}
	entered, release, next := make(chan struct{}), make(chan struct{}), make(chan struct{})
	executor.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		if r.Work.ID == "work/stuck" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		} else {
			close(next)
		}
		return &runtimetest.Handle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	// A lease of an hour: the holder's ticker never fires in this test.
	o := runtime.SchedulerOptions{Owner: "daemon", LeaseDuration: time.Hour, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}}
	s := runtimetest.NewScheduler(t, f, o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &daemon{o: Options{LeaseDuration: o.LeaseDuration}, coord: f.Store, now: f.Clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		reported: map[string]string{}, reportedAt: map[string]time.Time{}, inflight: map[attemptKey]domain.LeaseRef{}, ctx: ctx, nudge: make(chan struct{}, 1)}
	d.current.Store(&components{scheduler: s})
	returned := make(chan struct{})
	go func() {
		d.startReady(ctx)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the start step is held by a stuck launch")
	}
	select {
	case <-next:
	case <-time.After(10 * time.Second):
		t.Fatal("the next ready Work did not start while the first launch was stuck")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("control: the stuck launch never reached its executor")
	}
	select {
	case <-release:
		t.Fatal("unreachable")
	default:
	}
	// A second pass doesn't claim or launch the stuck Work again.
	d.startReady(ctx)
	if runs, _, _, _ := executor.Counts(); runs != 2 {
		t.Fatalf("%d runs, want the two Works launched once each", runs)
	}
	close(release)
	d.wg.Wait()
	err := f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		for _, id := range []domain.ID{"work/stuck/attempt/1", "work/next/attempt/1"} {
			x, err := tx.Execution(id)
			if err != nil {
				return err
			}
			if x.State != domain.ExecutionCompleted {
				t.Errorf("%s is %s, want completed", id, x.State)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
