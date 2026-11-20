package bootstrap

import (
	"context"
	"errors"
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

// TestEndedTurnKeepsItsTranscript (#647): the Work is cancelled while its
// turn runs, so the hold loses the lease before the turn's process has
// written its result. The observer stops the process, then collects the
// result the stopped turn wrote and records its transcript on the run and
// its segment before confirming the stop. The negative control: an
// executor that cannot give the stopped turn's result still has its stop
// confirmed, and the segment is left final with no artifacts.
func TestEndedTurnKeepsItsTranscript(t *testing.T) {
	transcript := domain.ArtifactRef{ID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Size: 7, MediaType: "application/x-ndjson"}
	for _, gives := range []bool{true, false} {
		name := "result collected"
		if !gives {
			name = "no result to collect"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, base := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
			executor := &stoppingExecutor{Executor: base}
			key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "stopped"}
			runtimetest.RequireAccept(t, f, "trigger/stopped", &key)
			runtimetest.RequireRoute(t, f.Driver, "trigger/stopped")
			work := domain.ID("work/stopped")
			lease := time.Minute
			s := runtimetest.NewScheduler(t, f, runtime.SchedulerOptions{Owner: "daemon", LeaseDuration: lease, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}})
			d := &daemon{o: Options{LeaseDuration: lease}, coord: f.Store, now: f.Clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				reported: map[string]string{}, reportedAt: map[string]time.Time{}, inflight: map[attemptKey]domain.LeaseRef{}, ctx: ctx, nudge: make(chan struct{}, 1),
				external: externalState{executors: map[string]extension.Executor{"fixture": executor}}}
			c := &components{scheduler: s}
			d.current.Store(c)

			a, err := s.Start(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			waits := 0
			a.Handle = &runtimetest.Handle{WaitFn: func(ctx context.Context) (extension.Result, error) {
				waits++
				if len(executor.stopped()) == 0 {
					// The turn is still running: the observer's hold ends
					// first.
					<-ctx.Done()
					return extension.Result{}, ctx.Err()
				}
				if !gives {
					return extension.Result{}, errors.New("no result")
				}
				return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "stopped"}, Artifacts: []domain.ArtifactRef{transcript}}, nil
			}}
			if err = s.Cancel(ctx, work); err != nil {
				t.Fatal(err)
			}
			held, cancel := context.WithCancel(ctx)
			lost := make(chan struct{})
			close(lost)
			cancel()
			d.observe(held, c, a, lost)

			if got := executor.stopped(); len(got) != 1 || waits != 2 {
				t.Fatalf("stops %v, waits %d; want one stop and a wait after it", got, waits)
			}
			var run runtime.Run
			if err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
				run, err = runtime.RunInTx(tx, a.Lease.ExecutionID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			g := run.Segments[len(run.Segments)-1]
			if !run.ResourceExcluded || g.ArtifactsPending {
				t.Fatalf("stop not confirmed: excluded %v, pending %v", run.ResourceExcluded, g.ArtifactsPending)
			}
			want := 0
			if gives {
				want = 1
			}
			if len(g.Artifacts) != want || len(run.Execution.Artifacts) != want {
				t.Fatalf("segment artifacts %+v, run %+v; want %d", g.Artifacts, run.Execution.Artifacts, want)
			}
		})
	}
}

// TestFencedResultIsKeptFromTheHandlesOneAnswer (#647, after #646): the
// Work is cancelled just before its turn's process ends, so the result
// reaches the daemon's Await and Complete refuses it. Its transcript is
// kept from that one answer: the handle, which answers only once as an
// out-of-process executor's does, is never waited on again. The negative
// control: a handle that answers with an executor error is not waited on
// again either, and the segment is left final with no artifacts.
func TestFencedResultIsKeptFromTheHandlesOneAnswer(t *testing.T) {
	transcript := domain.ArtifactRef{ID: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Size: 9, MediaType: "application/x-ndjson"}
	for _, fails := range []bool{false, true} {
		name := "result"
		if fails {
			name = "executor error"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, base := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
			executor := &stoppingExecutor{Executor: base}
			key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "answered"}
			runtimetest.RequireAccept(t, f, "trigger/answered", &key)
			runtimetest.RequireRoute(t, f.Driver, "trigger/answered")
			work := domain.ID("work/answered")
			lease := time.Minute
			s := runtimetest.NewScheduler(t, f, runtime.SchedulerOptions{Owner: "daemon", LeaseDuration: lease, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}})
			d := &daemon{o: Options{LeaseDuration: lease}, coord: f.Store, now: f.Clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				reported: map[string]string{}, reportedAt: map[string]time.Time{}, inflight: map[attemptKey]domain.LeaseRef{}, ctx: ctx, nudge: make(chan struct{}, 1),
				external: externalState{executors: map[string]extension.Executor{"fixture": executor}}}
			c := &components{scheduler: s}
			d.current.Store(c)

			a, err := s.Start(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			waits := 0
			a.Handle = &runtimetest.Handle{WaitFn: func(context.Context) (extension.Result, error) {
				waits++
				if waits > 1 {
					return extension.Result{}, errors.New("not_found: the handle has already answered")
				}
				if fails {
					return extension.Result{}, errors.New("executor transport failed")
				}
				return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "stopped"}, Artifacts: []domain.ArtifactRef{transcript}}, nil
			}}
			if err = s.Cancel(ctx, work); err != nil {
				t.Fatal(err)
			}
			d.observe(ctx, c, a, make(chan struct{}))

			if got := executor.stopped(); len(got) != 1 || waits != 1 {
				t.Fatalf("stops %v, waits %d; want one stop and the handle's one answer", got, waits)
			}
			var run runtime.Run
			if err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
				run, err = runtime.RunInTx(tx, a.Lease.ExecutionID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			g := run.Segments[len(run.Segments)-1]
			want := 1
			if fails {
				want = 0
			}
			if !run.ResourceExcluded || g.ArtifactsPending || len(g.Artifacts) != want || len(run.Execution.Artifacts) != want {
				t.Fatalf("excluded %v, pending %v, segment %+v, run %+v; want %d artifacts", run.ResourceExcluded, g.ArtifactsPending, g.Artifacts, run.Execution.Artifacts, want)
			}
		})
	}
}
