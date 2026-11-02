package runtimetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Handle is a run handle whose outcome is fixed (Result, Err) or computed
// (WaitFn). It completes nothing until the scheduler observes it.
type Handle struct {
	Result extension.Result
	Err    error
	WaitFn func(context.Context) (extension.Result, error)
}

func (h *Handle) Wait(ctx context.Context) (extension.Result, error) {
	if h.WaitFn != nil {
		return h.WaitFn(ctx)
	}
	return h.Result, h.Err
}

// Executor records every call and lets a test take over each method with
// its On hook.
type Executor struct {
	Mu                                             sync.Mutex
	Caps                                           domain.Capabilities
	Runs, Recoveries, Injects, Interrupts, Resumes []domain.ID
	Handles                                        map[domain.ID]extension.RunHandle
	NotAdoptable                                   bool
	OnRun                                          func(context.Context, extension.RunRequest) (extension.RunHandle, error)
	OnRecover                                      func(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error)
	OnInject                                       func(context.Context, domain.ID, domain.Event) error
	OnInterrupt                                    func(context.Context, domain.ID) error
}

func (e *Executor) Capabilities() domain.Capabilities {
	c := e.Caps
	if c.Toolbelt == "" {
		c.Toolbelt = extension.ToolbeltNone
	}
	return c
}
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	e.Mu.Lock()
	e.Runs = append(e.Runs, r.Execution.ID)
	hook := e.OnRun
	e.Mu.Unlock()
	if hook != nil {
		return hook(ctx, r)
	}
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{"handle":"durable"}`)}); err != nil {
		return nil, err
	}
	h := &Handle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
	e.Mu.Lock()
	if e.Handles == nil {
		e.Handles = map[domain.ID]extension.RunHandle{}
	}
	e.Handles[r.Execution.ID] = h
	e.Mu.Unlock()
	return h, nil
}
func (e *Executor) Recover(ctx context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
	e.Mu.Lock()
	e.Recoveries = append(e.Recoveries, r.Execution.ID)
	hook := e.OnRecover
	bad := e.NotAdoptable
	h := e.Handles[r.Execution.ID]
	e.Mu.Unlock()
	if hook != nil {
		return hook(ctx, r, p)
	}
	if bad || h == nil {
		return nil, extension.ErrNotAdoptable
	}
	if err := r.PersistRecovery(p); err != nil {
		return nil, err
	}
	return h, nil
}
func (e *Executor) Inject(ctx context.Context, id domain.ID, event domain.Event) error {
	e.Mu.Lock()
	e.Injects = append(e.Injects, id)
	hook := e.OnInject
	e.Mu.Unlock()
	if hook != nil {
		return hook(ctx, id, event)
	}
	return nil
}
func (e *Executor) Interrupt(ctx context.Context, id domain.ID) error {
	e.Mu.Lock()
	e.Interrupts = append(e.Interrupts, id)
	hook := e.OnInterrupt
	e.Mu.Unlock()
	if hook != nil {
		return hook(ctx, id)
	}
	return nil
}
func (e *Executor) Resume(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errors.New("this executor does not resume; use WaitExecutor")
}
func (e *Executor) Counts() (int, int, int, int) {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	return len(e.Runs), len(e.Recoveries), len(e.Injects), len(e.Interrupts)
}

// SchedulerDriver drives the production scheduler on top of RoutingDriver.
type SchedulerDriver struct {
	*RoutingDriver
	Scheduler *runtime.Scheduler
	Executor  *Executor
	Options   runtime.SchedulerOptions
}

func (d *SchedulerDriver) Start(id domain.ID) (domain.LeaseRef, error) {
	a, err := d.Scheduler.Start(context.Background(), id)
	return a.Lease, err
}
func (d *SchedulerDriver) Complete(ref domain.LeaseRef) error {
	return d.Scheduler.Complete(context.Background(), ref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}})
}
func (d *SchedulerDriver) Fail(ref domain.LeaseRef) error {
	return d.Scheduler.Complete(context.Background(), ref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed}})
}
func (d *SchedulerDriver) Cancel(id domain.ID) error {
	return d.Scheduler.Cancel(context.Background(), id)
}
func (d *SchedulerDriver) Recover(id domain.ID, adoptable bool) (domain.LeaseRef, error) {
	d.Executor.Mu.Lock()
	d.Executor.NotAdoptable = !adoptable
	d.Executor.Mu.Unlock()
	a, err := d.Scheduler.Recover(context.Background(), id)
	if !adoptable && errors.Is(err, extension.ErrNotAdoptable) {
		err = nil
	}
	return a.Lease, err
}
func (d *SchedulerDriver) Retry(work, event domain.ID, manual bool) error {
	if event == "" && !manual {
		err := d.Store.Transact(context.Background(), storage.Guard{Now: d.Clock.Now()}, func(tx storage.Tx) error {
			timers, err := tx.ListTimers()
			if err != nil {
				return err
			}
			for _, timer := range timers {
				if timer.Event.WorkID != work || timer.Event.Type != "core.retry.due" {
					continue
				}
				w, err := tx.Work(work)
				if err != nil {
					return err
				}
				if _, _, err = runtime.AcceptDerivedInTx(tx, d.Clock.Now(), runtime.CausalLimits{}, w.ScopeID, timer.Event.CausationID, timer.Event); err != nil {
					return err
				}
				if err = tx.FireTimer(timer.ID, timer.Revision, nil); err != nil {
					return err
				}
				event = timer.Event.ID
				return nil
			}
			return storage.ErrInvalid
		})
		if err != nil {
			return err
		}
	}
	return d.Scheduler.ProcessRetry(context.Background(), event)
}

// SchedulerFixture adds the production scheduler to RoutingFixture, with a
// controllable executor.
func SchedulerFixture(t testing.TB, backend storage.Store, o conformance.RuntimeOptions) (conformance.RuntimeFixture, *Executor) {
	t.Helper()
	f := RoutingFixture(t, backend, o)
	base := f.Driver.(*RoutingDriver)
	executor := &Executor{Caps: o.Capabilities}
	options := runtime.SchedulerOptions{Owner: "fixture-owner", LeaseDuration: conformance.RuntimeLease, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}, Retry: runtime.RetryPolicy{MaxAttempts: o.MaxAttempts}}
	makeDriver := func() *SchedulerDriver {
		s, err := runtime.NewScheduler(f.Store, f.Clock.Now, base.Router, options)
		if err != nil {
			t.Fatal(err)
		}
		return &SchedulerDriver{base, s, executor, options}
	}
	f.Driver = makeDriver()
	f.Restart = func() conformance.RuntimeDriver { return makeDriver() }
	return f, executor
}
