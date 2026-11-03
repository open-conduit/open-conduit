package runtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Issue #456: an attempt holds its lease from claim to release, so a slow
// launch keeps it; a launch whose lease is lost before anything is pinned
// is relaunched by recovery, not failed; and each launch phase is recorded.

// slowContainers is a container broker whose container takes slow() to
// create (a pull, a loaded host), then is pinned through the scheduler's
// pin, fenced by the lease. A refused pin removes the container, as the
// host's binding does.
type slowContainers struct {
	pin     func(extension.ContainerPin) error
	phase   runtime.PhaseFunc
	slow    func()
	mu      *sync.Mutex
	created *[]string
	removed *[]string
	id      string
}

func (b slowContainers) Launch(_ context.Context, _ extension.SandboxNeeds, _ extension.SandboxLaunch) (extension.SandboxPlan, error) {
	end := b.phase("container create")
	if b.slow != nil {
		b.slow()
	}
	b.mu.Lock()
	*b.created = append(*b.created, b.id)
	b.mu.Unlock()
	end(nil)
	c := extension.ContainerPin{Digest: "sha256:plan", Summary: "fake container", Image: "img@sha256:1", Daemon: "daemon-1", ID: b.id}
	if err := b.pin(c); err != nil {
		b.mu.Lock()
		*b.removed = append(*b.removed, b.id)
		b.mu.Unlock()
		return extension.SandboxPlan{}, err
	}
	return extension.SandboxPlan{Argv: []string{"run", b.id}, Digest: c.Digest, Signals: extension.SandboxTerm}, nil
}
func (slowContainers) Adopt(context.Context, bool) (extension.ContainerExit, error) {
	return extension.ContainerExit{}, nil
}

// launchRig is a scheduler whose bindings run in a slow fake container,
// and whose executor launches as Claude Code's does: recovery metadata
// first, then the container, then the process.
type launchRig struct {
	f        conformance.RuntimeFixture
	executor *schedulerExecutor
	o        runtime.SchedulerOptions
	mu       sync.Mutex
	created  []string
	removed  []string
	// slow runs while the container is created; contained says whether a
	// binding runs in one at all.
	slow      func()
	contained bool
	// lapse, when set, runs after the container is pinned and before the
	// executor acknowledges the launch.
	lapse func()
}

func newLaunchRig(t *testing.T) *launchRig {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	r := &launchRig{f: f, executor: executor, contained: true}
	n := 0
	r.o = schedulerDefaultOptions(f, executor)
	r.o.Container = func(ctx context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		if !r.contained {
			return runtime.ContainerBinding{}, nil
		}
		r.mu.Lock()
		n++
		id := "c" + string(rune('0'+n))
		r.mu.Unlock()
		return runtime.ContainerBinding{Broker: slowContainers{pin: pin, phase: runtime.LaunchPhases(ctx), slow: r.slow, mu: &r.mu, created: &r.created, removed: &r.removed, id: id}, Resources: resources}, nil
	}
	executor.OnRun = func(ctx context.Context, req extension.RunRequest) (extension.RunHandle, error) {
		if err := req.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		if req.Context.Container != nil {
			if _, err := req.Context.Container.Launch(ctx, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"x"}}); err != nil {
				return nil, err
			}
		}
		if r.lapse != nil {
			r.lapse()
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	executor.OnRecover = func(_ context.Context, req extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
		if err := req.PersistRecovery(state); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	return r
}

func (r *launchRig) scheduler(t *testing.T, owner string) *runtime.Scheduler {
	o := r.o
	o.Owner = owner
	return newTestScheduler(t, r.f, o)
}

// heldLaunch launches a claimed attempt while a holder renews its lease
// on ticks the launch's slow step sends: the clock moves on by a third of
// the lease, then the holder renews, every step. Each send waits for the
// holder to take it, and the holder takes the next only once its renewal
// is done, so the clock never passes a lease the holder has not renewed.
func heldLaunch(t *testing.T, r *launchRig, s *runtime.Scheduler, a runtime.Attempt, steps int, held bool) (runtime.Attempt, error) {
	t.Helper()
	ticks := make(chan time.Time)
	r.slow = func() {
		for i := 0; i < steps; i++ {
			r.f.Clock.Advance(r.o.LeaseDuration / 3)
			if held {
				ticks <- r.f.Clock.Now()
			}
		}
		if held {
			ticks <- r.f.Clock.Now()
		}
	}
	defer func() { r.slow = nil }()
	ctx, _, stop := s.Hold(context.Background(), a.Lease, ticks, func(err error) { t.Errorf("renewal: %v", err) })
	defer stop()
	return s.Launch(ctx, a)
}

// TestProductionSchedulerHeldLaunchOutlastsItsLease: a launch whose
// container takes three lease durations to create keeps its lease, because
// its holder renews it from the claim, and the attempt runs and completes.
// The control: the same launch with nothing holding the lease loses it at
// the pin, the container is removed, and the attempt is left to recovery.
func TestProductionSchedulerHeldLaunchOutlastsItsLease(t *testing.T) {
	for _, held := range []bool{true, false} {
		r := newLaunchRig(t)
		work := seedSchedulerWork(t, r.f, "slow")
		s := r.scheduler(t, "host-a")
		a, err := s.Claim(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		if a.Launched() {
			t.Fatal("Claim launched the attempt")
		}
		launched, err := heldLaunch(t, r, s, a, 9, held)
		state, inspectErr := s.InspectAttempt(context.Background(), a.Lease.ExecutionID)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if !held {
			if !errors.Is(err, storage.ErrLeaseLost) || state.ContainerPin() != nil || len(r.removed) != 1 {
				t.Fatalf("control: an unheld slow launch got %v, pin %+v, removed %v; want the lease lost at the pin and the container removed", err, state.ContainerPin(), r.removed)
			}
			continue
		}
		if err != nil {
			t.Fatalf("held slow launch: %v", err)
		}
		if state.LaunchState != "running" || state.ContainerPin() == nil || len(r.removed) != 0 {
			t.Fatalf("held slow launch: state %q pin %+v removed %v", state.LaunchState, state.ContainerPin(), r.removed)
		}
		if err = s.Observe(context.Background(), launched); err != nil {
			t.Fatalf("the slow launch's result: %v", err)
		}
	}
}

// TestProductionSchedulerRelaunchesALaunchThatPinnedNothing: a launch that
// loses its lease before its container is pinned is relaunched by
// recovery at the new epoch: Run again, not Recover, the attempt not
// failed, no retry used, the old epoch's container removed. The negative
// controls: a lease lost after the pin goes through adoption (Recover),
// and so does an unisolated launch whose executor recorded its metadata,
// since its process may be running; one that recorded nothing relaunches.
func TestProductionSchedulerRelaunchesALaunchThatPinnedNothing(t *testing.T) {
	cases := []struct {
		name               string
		contained, pinned  bool
		relaunch           bool
		runs, recoveries   int
		beforeExecutorCall bool
	}{
		{name: "container, nothing pinned", contained: true, relaunch: true, runs: 2},
		{name: "container pinned", contained: true, pinned: true, runs: 1, recoveries: 1},
		{name: "no container, metadata recorded", pinned: true, runs: 1, recoveries: 1},
		{name: "no container, before the executor", beforeExecutorCall: true, relaunch: true, runs: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLaunchRig(t)
			r.contained = tc.contained
			work := seedSchedulerWork(t, r.f, "lapse")
			first := r.scheduler(t, "host-a")
			a, err := first.Claim(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			lapse := func() { r.f.Clock.Advance(2 * r.o.LeaseDuration) }
			switch {
			case tc.beforeExecutorCall:
				// The lease lapses before the launch reaches its executor:
				// the launch's first fenced write refuses it.
				lapse()
			case tc.pinned:
				r.lapse = lapse
			default:
				r.slow = lapse
			}
			_, err = first.Launch(context.Background(), a)
			r.slow, r.lapse = nil, nil
			if err == nil && !tc.pinned {
				t.Fatal("a launch that lost its lease succeeded")
			}
			second := r.scheduler(t, "host-b")
			claimed, err := second.ClaimRecovery(context.Background(), a.Lease.ExecutionID)
			if err != nil {
				t.Fatal(err)
			}
			if claimed.Relaunch() != tc.relaunch || claimed.Lease.Epoch <= a.Lease.Epoch {
				t.Fatalf("recovery relaunch %v at epoch %d (from %d), want relaunch %v at a higher epoch", claimed.Relaunch(), claimed.Lease.Epoch, a.Lease.Epoch, tc.relaunch)
			}
			recovered, err := second.Launch(context.Background(), claimed)
			if err != nil {
				t.Fatalf("recovered launch: %v", err)
			}
			if err = second.Observe(context.Background(), recovered); err != nil {
				t.Fatal(err)
			}
			runs, recoveries, _, _ := r.executor.Counts()
			if runs != tc.runs || recoveries != tc.recoveries {
				t.Fatalf("runs %d recoveries %d, want %d and %d", runs, recoveries, tc.runs, tc.recoveries)
			}
			state, err := second.InspectAttempt(context.Background(), a.Lease.ExecutionID)
			if err != nil {
				t.Fatal(err)
			}
			if state.Result == nil || state.Result.Outcome.Kind != domain.OutcomeCompleted {
				t.Fatalf("result %+v, want the recovered attempt completed", state.Result)
			}
			last := state.Segments[len(state.Segments)-1]
			if last.Relaunched != tc.relaunch || !last.Recovered {
				t.Fatalf("last segment %+v, want recovered, relaunched %v", last, tc.relaunch)
			}
			retry, err := second.RetryStatus(context.Background(), work)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				t.Fatal(err)
			}
			if retry.FailureCount != 0 {
				t.Fatalf("failure count %d, want the lost launch not charged", retry.FailureCount)
			}
			if tc.relaunch && tc.contained && (len(r.created) != 2 || len(r.removed) != 1 || r.removed[0] != r.created[0] || last.Container == nil || last.Container.ID != r.created[1]) {
				t.Fatalf("created %v removed %v pinned %+v, want the old epoch's container removed and the new one pinned", r.created, r.removed, last.Container)
			}
		})
	}
}

// TestProductionSchedulerRecordsLaunchPhases: each launch phase is
// recorded with the open segment, with how long it took and its outcome,
// including those the container binding runs inside the executor's launch.
func TestProductionSchedulerRecordsLaunchPhases(t *testing.T) {
	r := newLaunchRig(t)
	work := seedSchedulerWork(t, r.f, "phases")
	r.slow = func() { r.f.Clock.Advance(40 * time.Second) }
	s := r.scheduler(t, "host-a")
	if _, err := s.Start(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	broken := seedSchedulerWork(t, r.f, "phases-broken")
	r.o.Container = func(context.Context, runtime.AttemptState, extension.ResourceBroker, func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		return runtime.ContainerBinding{}, errors.New("docker missing")
	}
	b, err := r.scheduler(t, "host-a").Start(context.Background(), broken)
	if !errors.Is(err, extension.ErrSandboxUnavailable) {
		t.Fatalf("broken container: %v", err)
	}
	for _, tc := range []struct {
		id   domain.ID
		want []string
	}{
		{work + "/attempt/1", []string{"container ok 0s", "executor ok 40s", "container create ok 40s", "container pin ok 0s"}},
		{b.Lease.ExecutionID, []string{"container failed 0s"}},
	} {
		state, err := s.InspectAttempt(context.Background(), tc.id)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range state.Segments[len(state.Segments)-1].Launch {
			if p.EndedAt == nil {
				t.Fatalf("phase %s never ended", p.Name)
			}
			got = append(got, p.Name+" "+p.Outcome+" "+p.Duration(time.Time{}).String())
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s phases %v, want %v", tc.id, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s phases %v, want %v", tc.id, got, tc.want)
			}
		}
	}
}

// TestProductionSchedulerHoldEndsOnLostLease: the holder's context ends,
// with ErrLeaseLost as its cause, once a renewal is refused, so a launch
// under it stops at once.
func TestProductionSchedulerHoldEndsOnLostLease(t *testing.T) {
	r := newLaunchRig(t)
	work := seedSchedulerWork(t, r.f, "hold")
	s := r.scheduler(t, "host-a")
	a, err := s.Claim(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time)
	ctx, lost, stop := s.Hold(context.Background(), a.Lease, ticks, nil)
	defer stop()
	ticks <- r.f.Clock.Now()
	select {
	case <-lost:
		t.Fatal("control: a renewal within the lease lost it")
	default:
	}
	r.f.Clock.Advance(2 * r.o.LeaseDuration)
	ticks <- r.f.Clock.Now()
	<-lost
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), storage.ErrLeaseLost) {
		t.Fatalf("cause %v, want the lease lost", context.Cause(ctx))
	}
}
