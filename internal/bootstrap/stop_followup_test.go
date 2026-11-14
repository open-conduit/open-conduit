package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/storage"
)

// stoppingExecutor is the fixture executor with a Stop the test controls,
// recording the epoch of each call.
type stoppingExecutor struct {
	*runtimetest.Executor
	mu     sync.Mutex
	stops  []uint64
	refuse bool
}

func (e *stoppingExecutor) Stop(_ context.Context, _ domain.ID, epoch uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stops = append(e.stops, epoch)
	if e.refuse {
		return errors.New("still running")
	}
	return nil
}

func (e *stoppingExecutor) stopped() []uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uint64(nil), e.stops...)
}

// TestUnconfirmedStopIsFollowedUp (#612): a recovery whose launch fails
// after its claim (the toolbelt endpoint can't be served) leaves the
// failed attempt unexcluded, the launch failure doesn't stop it, and the
// superseded observer's stop at the old epoch confirms nothing. The
// Work's next start is refused as resource_held, naming the held attempt.
// The runtime's follow-up then hands the attempt to the host at its
// current epoch, the executor proves the stop, the hold is released and
// the Work's next attempt starts. The negative control: with Stop failing,
// every follow-up reports stop_unverified, the hold stays, and the next
// follow-up comes after a longer backoff.
func TestUnconfirmedStopIsFollowedUp(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := "stop proven"
		if refuse {
			name = "stop keeps failing"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f, base := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
			base.Caps.Toolbelt = extension.ToolbeltCLI
			executor := &stoppingExecutor{Executor: base, refuse: refuse}
			key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "held"}
			runtimetest.RequireAccept(t, f, "trigger/held", &key)
			runtimetest.RequireRoute(t, f.Driver, "trigger/held")
			work := domain.ID("work/held")

			router := f.Driver.(*runtimetest.SchedulerDriver).Router
			belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, router, runtime.ToolbeltOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var endpointDown atomic.Bool
			lease := time.Minute
			o := runtime.SchedulerOptions{Owner: "daemon", LeaseDuration: lease, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}, Toolbelt: belt,
				ToolbeltEndpoints: func(context.Context, domain.ID, string) (string, string, error) {
					if endpointDown.Load() {
						return "", "", errors.New("socket unavailable")
					}
					return "unix:///fixture", "", nil
				}}
			s := runtimetest.NewScheduler(t, f, o)
			timers, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			d := &daemon{o: Options{LeaseDuration: lease}, coord: f.Store, now: f.Clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				reported: map[string]string{}, reportedAt: map[string]time.Time{}, inflight: map[attemptKey]domain.LeaseRef{}, ctx: ctx, nudge: make(chan struct{}, 1),
				executors: map[string]extension.Executor{"fixture": executor}}
			c := &components{scheduler: s, timers: timers}
			d.current.Store(c)
			// pass is the loop's poll step: recoveries and due stop follow-ups.
			pass := func() runtime.SchedulerPoll {
				t.Helper()
				poll, err := s.PollClaims(ctx, timers, passLimit)
				if err != nil {
					t.Fatal(err)
				}
				for _, due := range poll.Stops {
					d.followUpStop(ctx, c, due)
				}
				d.wg.Wait()
				return poll
			}

			// The first attempt runs, then its lease lapses on a loaded host.
			first, err := s.Claim(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Launch(ctx, first); err != nil {
				t.Fatal(err)
			}
			execution := first.Lease.ExecutionID
			f.Clock.Advance(2 * lease)

			// Recovery claims it at a newer epoch, and its launch fails at
			// the toolbelt.
			endpointDown.Store(true)
			poll := pass()
			if len(poll.Recovered) != 1 {
				t.Fatalf("recovered %+v, want the lapsed attempt", poll.Recovered)
			}
			recovered := poll.Recovered[0]
			if recovered.Lease.Epoch <= first.Lease.Epoch {
				t.Fatalf("recovered at epoch %d, want above %d", recovered.Lease.Epoch, first.Lease.Epoch)
			}
			_, err = s.Launch(ctx, recovered)
			if !errors.Is(err, extension.ErrUnauthorized) {
				t.Fatalf("recovered launch: %v, want the toolbelt failure", err)
			}
			d.launchFailed(c, recovered, "", make(chan struct{}), err)
			// The superseded observer gives up at its own epoch.
			d.confirmStopped(ctx, c, execution, first.Lease.Epoch)
			if got := executor.stopped(); len(got) != 0 {
				t.Fatalf("stops %v before the follow-up, want none (the incident's two missed chances)", got)
			}
			state, err := s.InspectAttempt(ctx, execution)
			if err != nil || state.Excluded {
				t.Fatalf("attempt %+v (%v), want it failed and unexcluded", state, err)
			}

			// The retry comes due, and the start is refused for the hold.
			endpointDown.Store(false)
			f.Clock.Advance(10 * time.Minute)
			pass()
			_, err = s.Claim(ctx, work)
			if !errors.Is(err, runtime.ErrResourceHeld) || errorCode(err) != "resource_held" || heldBy(err) != "held by "+string(execution) {
				t.Fatalf("start while held: %v (code %q, cause %q), want resource_held naming %s", err, errorCode(err), heldBy(err), execution)
			}
			holds := heldItems(t, f.Store, f.Clock.Now())
			if len(holds) != 1 || holds[0] != string(execution)+" blocks work "+string(work) {
				t.Fatalf("doctor resource_holds %v, want the held attempt and the Work it blocks", holds)
			}

			// The first pass that sees the hold schedules its follow-up;
			// it comes due after the base delay.
			if got := executor.stopped(); len(got) != 0 {
				t.Fatalf("stops %v on the pass that scheduled the follow-up, want none yet", got)
			}
			f.Clock.Advance(runtime.StopFollowupBase)
			if poll = pass(); len(poll.Stops) != 1 || poll.Stops[0].Epoch != recovered.Lease.Epoch {
				t.Fatalf("due stops %+v, want the attempt at its current epoch %d", poll.Stops, recovered.Lease.Epoch)
			}
			if got := executor.stopped(); len(got) != 1 || got[0] != recovered.Lease.Epoch {
				t.Fatalf("stops %v, want one at epoch %d", got, recovered.Lease.Epoch)
			}

			if refuse {
				if code := d.reportedCode("stop", string(execution)); code != "stop_unverified" {
					t.Fatalf("stop diagnostic %q, want stop_unverified", code)
				}
				if state, _ = s.InspectAttempt(ctx, execution); state.Excluded {
					t.Fatal("a failed stop released the hold")
				}
				if _, err = s.Claim(ctx, work); !errors.Is(err, runtime.ErrResourceHeld) {
					t.Fatalf("start after a failed stop: %v, want still held", err)
				}
				// Backoff: not again after the base delay, then again after twice it.
				f.Clock.Advance(runtime.StopFollowupBase)
				if poll = pass(); len(poll.Stops) != 0 {
					t.Fatalf("due stops %+v one base delay after the first, want the backoff to double", poll.Stops)
				}
				f.Clock.Advance(runtime.StopFollowupBase)
				if poll = pass(); len(poll.Stops) != 1 || poll.Stops[0].Attempt != 2 {
					t.Fatalf("due stops %+v, want the second follow-up", poll.Stops)
				}
				if got := executor.stopped(); len(got) != 2 {
					t.Fatalf("stops %v, want two follow-ups", got)
				}
				return
			}

			if state, _ = s.InspectAttempt(ctx, execution); !state.Excluded {
				t.Fatal("a proven stop left the hold")
			}
			if code := d.reportedCode("stop", string(execution)); code != "" {
				t.Fatalf("stop diagnostic %q after the stop was proven, want it cleared", code)
			}
			if _, ok, err := s.StopFollowupStatus(ctx, execution); err != nil || ok {
				t.Fatalf("follow-up still recorded (%v) once the attempt is excluded", err)
			}
			next, err := s.Claim(ctx, work)
			if err != nil {
				t.Fatalf("the Work's next start: %v", err)
			}
			if next.Lease.ExecutionID == execution {
				t.Fatalf("next start reused %s, want a new attempt", execution)
			}
			if poll = pass(); len(poll.Stops) != 0 {
				t.Fatalf("due stops %+v after the release, want none", poll.Stops)
			}
		})
	}
}

// heldItems is doctor's resource_holds items.
func heldItems(t *testing.T, store storage.Store, now time.Time) []string {
	t.Helper()
	for _, c := range runtime.Diagnose(context.Background(), store, now, runtime.DoctorOptions{}) {
		if c.Name == "resource_holds" {
			if c.Code == "released" {
				return nil
			}
			return c.Items
		}
	}
	t.Fatal("no resource_holds check")
	return nil
}

// blockingStopper is an executor whose Stop hangs until released.
type blockingStopper struct {
	*runtimetest.Executor
	calls   atomic.Int32
	release chan struct{}
}

func (e *blockingStopper) Stop(ctx context.Context, _ domain.ID, _ uint64) error {
	e.calls.Add(1)
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestStopFollowupRunsOffTheLoop: a follow-up whose stop hangs (a wedged
// container runtime) returns to the loop at once, and the next follow-up
// for the same attempt and epoch doesn't start a second stop while the
// first is still running. The control: the first stop really is running.
func TestStopFollowupRunsOffTheLoop(t *testing.T) {
	ctx := context.Background()
	f, base := runtimetest.SchedulerFixture(t, runtimetest.Store(t, "direct"), runtimetest.Options(runtimetest.HandlerFunc(runtimetest.Decision), "queue"))
	executor := &blockingStopper{Executor: base, release: make(chan struct{})}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "wedged"}
	runtimetest.RequireAccept(t, f, "trigger/wedged", &key)
	runtimetest.RequireRoute(t, f.Driver, "trigger/wedged")
	o := runtime.SchedulerOptions{Owner: "daemon", LeaseDuration: time.Minute, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}}
	s := runtimetest.NewScheduler(t, f, o)
	a, err := s.Claim(ctx, "work/wedged")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Launch(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Cancelling fences the attempt and leaves its stop unconfirmed.
	if err = s.Cancel(ctx, "work/wedged"); err != nil {
		t.Fatal(err)
	}
	if state, err := s.InspectAttempt(ctx, a.Lease.ExecutionID); err != nil || state.Excluded {
		t.Fatalf("attempt %+v (%v), want it cancelled and unexcluded", state, err)
	}
	d := &daemon{o: Options{LeaseDuration: o.LeaseDuration}, coord: f.Store, now: f.Clock.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		reported: map[string]string{}, reportedAt: map[string]time.Time{}, inflight: map[attemptKey]domain.LeaseRef{}, ctx: ctx, nudge: make(chan struct{}, 1),
		executors: map[string]extension.Executor{"fixture": executor}}
	c := &components{scheduler: s}
	due := runtime.StopDue{ExecutionID: a.Lease.ExecutionID, Epoch: a.Lease.Epoch, Attempt: 1}
	returned := make(chan struct{})
	go func() {
		d.followUpStop(ctx, c, due)
		d.followUpStop(ctx, c, due)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung stop held the loop")
	}
	deadline := time.Now().Add(10 * time.Second)
	for executor.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := executor.calls.Load(); n != 1 {
		t.Fatalf("%d stops running, want the first one only", n)
	}
	close(executor.release)
	d.wg.Wait()
	if state, err := s.InspectAttempt(ctx, a.Lease.ExecutionID); err != nil || !state.Excluded {
		t.Fatalf("attempt %+v (%v), want it confirmed stopped once the stop returned", state, err)
	}
}
