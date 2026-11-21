package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// fullPool is a host's container binding on a pool: while full its Launch
// refuses for capacity, and Refusal says so.
type fullPool struct {
	full    *atomic.Bool
	refused atomic.Pointer[extension.NoCapacityError]
	hint    *time.Time
	pin     func(extension.ContainerPin) error
}

func (b *fullPool) Launch(context.Context, extension.SandboxNeeds, extension.SandboxLaunch) (extension.SandboxPlan, error) {
	if b.full.Load() {
		r := &extension.NoCapacityError{Reason: "all 3 nodes are full", RetryAt: b.hint}
		b.refused.Store(r)
		return extension.SandboxPlan{}, r
	}
	b.refused.Store(nil)
	c := extension.ContainerPin{Digest: "sha256:plan", Summary: "pool", Image: "img@sha256:1", Daemon: "node-a", ID: "c1"}
	return extension.SandboxPlan{Argv: []string{"run"}, Digest: c.Digest, Signals: extension.SandboxTerm}, b.pin(c)
}

func (*fullPool) Adopt(context.Context, bool) (extension.ContainerExit, error) {
	return extension.ContainerExit{}, nil
}

// capacityRig is a scheduler whose executor reports a refused launch as a
// failed result, as the bundled executors do.
func capacityRig(t *testing.T, report bool) (*runtime.Scheduler, *runtime.Timers, func(time.Duration), domain.ID, *atomic.Bool, *atomic.Int32, func(func(storage.Tx) error)) {
	t.Helper()
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "pooled")
	full := &atomic.Bool{}
	full.Store(true)
	ran := &atomic.Int32{}
	executor.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		if _, err := r.Context.Container.Launch(ctx, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"x"}}); err != nil {
			return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: err.Error()}}}, nil
		}
		ran.Add(1)
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	o := schedulerDefaultOptions(f, executor)
	o.Container = func(_ context.Context, _ runtime.AttemptState, resources extension.ResourceBroker, _ map[string]string, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		b := &fullPool{full: full, pin: pin}
		c := runtime.ContainerBinding{Broker: b, Resources: resources, Settle: func(r extension.Result) extension.Result { return r }}
		if report {
			c.Refusal = b.refused.Load
		}
		return c, nil
	}
	s := newTestScheduler(t, f, o)
	timers, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	view := func(fn func(storage.Tx) error) {
		t.Helper()
		if err := f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, fn); err != nil {
			t.Fatal(err)
		}
	}
	return s, timers, f.Clock.Advance, work, full, ran, view
}

// TestFullPoolHoldsWorkWithoutSpendingARetry: a refusal for capacity returns
// the claim, keeps the input, spends no retry and records a hold; the poll
// releases it at its time and the Work then runs.
func TestFullPoolHoldsWorkWithoutSpendingARetry(t *testing.T) {
	ctx := context.Background()
	s, timers, advance, work, full, ran, view := capacityRig(t, true)
	for round := 1; round <= 4; round++ {
		a, err := s.Start(ctx, work)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if err = s.Observe(ctx, a); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		view(func(tx storage.Tx) error {
			w, e := tx.Work(work)
			if e != nil {
				return e
			}
			if w.State != domain.WorkReady {
				t.Fatalf("round %d: work is %s, want ready", round, w.State)
			}
			h, held, e := runtime.HoldInTx(tx, work)
			if e != nil {
				return e
			}
			if !held || h.Kind != runtime.HoldKindCapacity {
				t.Fatalf("round %d: hold %+v (%v), want a capacity hold", round, h, held)
			}
			inputs, e := runtime.PendingInputsInTx(tx, work)
			if e != nil {
				return e
			}
			if len(inputs) != 1 {
				t.Fatalf("round %d: %d pending inputs, want the refused one back", round, len(inputs))
			}
			return nil
		})
		if _, err := s.RetryStatus(ctx, work); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("round %d: a retry was recorded: %v", round, err)
		}
		if _, err := s.Claim(ctx, work); !errors.Is(err, runtime.ErrNotEligible) {
			t.Fatalf("round %d: a held Work was claimed: %v", round, err)
		}
		advance(5*time.Minute + time.Second)
		if _, err := s.Poll(ctx, timers, 10); err != nil {
			t.Fatal(err)
		}
	}
	full.Store(false)
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("ran %d times, want once", ran.Load())
	}
	view(func(tx storage.Tx) error {
		w, e := tx.Work(work)
		if e == nil && w.State != domain.WorkCompleted {
			t.Fatalf("work is %s, want completed", w.State)
		}
		return e
	})
}

// TestFullPoolWithoutARefusalFails is the negative control: the same failed
// result with no refusal from the host is an ordinary failure, which spends
// a retry.
func TestFullPoolWithoutARefusalFails(t *testing.T) {
	ctx := context.Background()
	s, _, _, work, _, _, _ := capacityRig(t, false)
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
	st, err := s.RetryStatus(ctx, work)
	if err != nil || st.FailureCount != 1 {
		t.Fatalf("retry %+v %v, want one failure", st, err)
	}
}

// TestFullPoolPutsAResumeBackToParked: a resume refused for capacity is the
// same attempt, parked again with its wake ready, and its next start resumes
// it rather than beginning a new one.
func TestFullPoolPutsAResumeBackToParked(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "pool-resume")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "pool-resume"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	registerWait(t, f, attempt.Lease, "pool-resume-wait", key, true, 1)
	requireAccept(t, f, "pool-resume-wake", &key)
	if err = d.Wake("pool-resume-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.HoldForCapacity(ctx, d.Scheduler, resumed.Lease, &extension.NoCapacityError{Reason: "full"}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		x, e := tx.Execution(attempt.Lease.ExecutionID)
		if e != nil {
			return e
		}
		if x.State != domain.ExecutionParked {
			t.Fatalf("execution is %s, want parked", x.State)
		}
		inputs, e := runtime.PendingInputsInTx(tx, work)
		if e != nil {
			return e
		}
		// Only what this claim took: the first segment's trigger was handled.
		if len(inputs) != 1 || inputs[0].EventID != "pool-resume-wake" {
			t.Fatalf("pending inputs %+v, want only the wake's", inputs)
		}
		return nil
	})
	f.Clock.Advance(31 * time.Second)
	timers, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Scheduler.Poll(ctx, timers, 10); err != nil {
		t.Fatal(err)
	}
	again, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if again.Lease.ExecutionID != attempt.Lease.ExecutionID {
		t.Fatalf("the resume began a new attempt: %s", again.Lease.ExecutionID)
	}
}

// TestFullPoolGivesBackABudgetRelease: an operator's release of a budget
// hold survives a start refused for capacity.
func TestFullPoolGivesBackABudgetRelease(t *testing.T) {
	ctx := context.Background()
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "pool-budget")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "pool-budget"}
	full := &atomic.Bool{}
	e.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		if _, err := r.Context.Container.Launch(ctx, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"x"}}); err != nil {
			return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: err.Error()}}}, nil
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	o := schedulerDefaultOptions(f, e)
	o.Budgets = []runtime.Budget{{Unit: "turns", Per: runtime.BudgetPerWork, Max: 1}}
	o.Container = func(_ context.Context, _ runtime.AttemptState, resources extension.ResourceBroker, _ map[string]string, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		b := &fullPool{full: full, pin: pin}
		return runtime.ContainerBinding{Broker: b, Resources: resources, Settle: func(r extension.Result) extension.Result { return r }, Refusal: b.refused.Load}, nil
	}
	s := newTestScheduler(t, f, o)
	control, err := runtime.NewControl(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).Router, s, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	timers, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "pool-budget-2", &key)
	requireRoute(t, f.Driver, "pool-budget-2")
	if err = s.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, work); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("over budget: %v", err)
	}
	var w domain.Work
	routingRead(t, f, func(tx storage.Tx) error { var e error; w, e = tx.Work(work); return e })
	if _, err = control.ReleaseWork(ctx, work, w.Revision, "one more", "pool-budget-release"); err != nil {
		t.Fatal(err)
	}
	full.Store(true)
	if a, err = s.Start(ctx, work); err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(31 * time.Second)
	if _, err = s.Poll(ctx, timers, 10); err != nil {
		t.Fatal(err)
	}
	full.Store(false)
	if a, err = s.Start(ctx, work); err != nil {
		t.Fatalf("the release was lost to the refused start: %v", err)
	}
	if err = s.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
}

// TestFullPoolRefusalSurvivesTheDaemon: a refused launch whose daemon goes
// before the claim is returned is relaunched, not failed, and spends no
// retry.
func TestFullPoolRefusalSurvivesTheDaemon(t *testing.T) {
	ctx := context.Background()
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "pool-restart")
	full := &atomic.Bool{}
	full.Store(true)
	e.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		if _, err := r.Context.Container.Launch(ctx, extension.SandboxNeeds{}, extension.SandboxLaunch{Argv: []string{"x"}}); err != nil {
			return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: err.Error()}}}, nil
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	o := schedulerDefaultOptions(f, e)
	o.Container = func(_ context.Context, _ runtime.AttemptState, resources extension.ResourceBroker, _ map[string]string, pin func(extension.ContainerPin) error) (runtime.ContainerBinding, error) {
		b := &fullPool{full: full, pin: pin}
		return runtime.ContainerBinding{Broker: b, Resources: resources, Settle: func(r extension.Result) extension.Result { return r }, Refusal: b.refused.Load}, nil
	}
	s := newTestScheduler(t, f, o)
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.InspectAttempt(ctx, a.Lease.ExecutionID)
	if err != nil || state.NoCapacity == nil {
		t.Fatalf("the refusal was not recorded: %+v %v", state.NoCapacity, err)
	}
	// The daemon goes here: its result never reaches Complete.
	f.Clock.Advance(2 * time.Minute)
	full.Store(false)
	next := newTestScheduler(t, f, o)
	claimed, err := next.ClaimRecovery(ctx, a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.Relaunch() {
		t.Fatal("the refused launch was not relaunched")
	}
	launched, err := next.Launch(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Observe(ctx, launched); err != nil {
		t.Fatal(err)
	}
	if _, err = next.RetryStatus(ctx, work); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a retry was recorded: %v", err)
	}
}

// TestExecutorAtCapacityHoldsTheWork: a launch the executor's extension
// refused ran nothing, so it spends no retry and leaves no held attempt. Any
// other launch error (the negative control) still counts as a failure.
func TestExecutorAtCapacityHoldsTheWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		want     error
		failures int
	}{
		{"at capacity", &extension.NoCapacityError{Reason: "the extension is at its request limit", Executor: true}, runtime.ErrExecutorCapacity, 0},
		{"failed", errors.New("boom"), runtime.ErrExecutorCall, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
			work := seedSchedulerWork(t, f, "busy")
			executor.OnRun = func(context.Context, extension.RunRequest) (extension.RunHandle, error) {
				return nil, tc.err
			}
			s := newTestScheduler(t, f, schedulerDefaultOptions(f, executor))
			if _, err := s.Start(ctx, work); !errors.Is(err, tc.want) {
				t.Fatalf("start: %v, want %v", err, tc.want)
			}
			st, err := s.RetryStatus(ctx, work)
			if tc.failures == 0 && errors.Is(err, storage.ErrNotFound) {
				return
			}
			if err != nil || st.FailureCount != tc.failures {
				t.Fatalf("retry %+v %v, want %d failures", st, err, tc.failures)
			}
		})
	}
}
