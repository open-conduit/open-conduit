package runtimetest

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// WaitExecutor adds Resume to Executor. Wait fixtures use the same real claim/Run/recovery implementation as the
// scheduler cases. Resume is a controllable executor operation, not a storage
// shortcut or a reference coordinator.
type WaitExecutor struct {
	*Executor
	ResumeRequests []extension.RunRequest
}

func (e *WaitExecutor) Resume(_ context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
	if err := r.PersistRecovery(p); err != nil {
		return nil, err
	}
	e.Mu.Lock()
	defer e.Mu.Unlock()
	e.Resumes = append(e.Resumes, r.Execution.ID)
	e.ResumeRequests = append(e.ResumeRequests, r)
	h := e.Handles[r.Execution.ID]
	if h == nil {
		h = &Handle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
	}
	return h, nil
}

// WaitDriver adds waits, timers and causal derivation to SchedulerDriver.
type WaitDriver struct {
	*SchedulerDriver
	Timers             *runtime.Timers
	Causal             *runtime.Causal
	ExecutorWithResume *WaitExecutor
}

func (d *WaitDriver) Wait(ref domain.LeaseRef, w domain.Wait, resume bool) error {
	return d.Scheduler.Wait(context.Background(), ref, w, resume)
}
func (d *WaitDriver) Wake(id domain.ID) error { return d.Scheduler.Wake(context.Background(), id) }
func (d *WaitDriver) Derive(e domain.Event) error {
	_, _, err := d.Causal.Derive(context.Background(), "routing-scope", e.CausationID, e)
	return err
}
func (d *WaitDriver) Retry(work, event domain.ID, manual bool) error {
	if event == "" && !manual {
		var found domain.Timer
		err := d.Store.Transact(context.Background(), storage.Guard{Now: d.Clock.Now()}, func(tx storage.Tx) error {
			timers, err := tx.ListTimers()
			if err != nil {
				return err
			}
			for _, timer := range timers {
				if timer.Event.WorkID == work && timer.Event.Type == "core.retry.due" {
					found = timer
					return nil
				}
			}
			return storage.ErrNotFound
		})
		if err != nil {
			return err
		}
		if _, err = d.Timers.Fire(context.Background(), found.ID, found.Revision, nil); err != nil {
			return err
		}
		event = found.Event.ID
	}
	return d.Scheduler.ProcessRetry(context.Background(), event)
}

// WaitFixture adds waits, timers and causal derivation to SchedulerFixture:
// the whole runtime the shared suite checks.
func WaitFixture(t testing.TB, backend storage.Store, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	f, baseExecutor := SchedulerFixture(t, backend, o)
	base := f.Driver.(*SchedulerDriver)
	executor := &WaitExecutor{Executor: baseExecutor}
	options := base.Options
	options.Executors = map[string]extension.Executor{"fixture": executor}
	options.CausalLimits = runtime.CausalLimits{MaxDepth: o.MaxDepth, PerMinute: o.PerMinute}
	makeDriver := func() *WaitDriver {
		s, err := runtime.NewScheduler(f.Store, f.Clock.Now, base.Router, options)
		if err != nil {
			t.Fatal(err)
		}
		timers, err := runtime.NewTimers(f.Store, f.Clock.Now, runtime.TimerOptions{CausalLimits: options.CausalLimits})
		if err != nil {
			t.Fatal(err)
		}
		causal, err := runtime.NewCausal(f.Store, f.Clock.Now, options.CausalLimits)
		if err != nil {
			t.Fatal(err)
		}
		return &WaitDriver{SchedulerDriver: &SchedulerDriver{RoutingDriver: base.RoutingDriver, Scheduler: s, Executor: baseExecutor, Options: options}, Timers: timers, Causal: causal, ExecutorWithResume: executor}
	}
	f.Driver = makeDriver()
	f.Restart = func() conformance.RuntimeDriver { return makeDriver() }
	return f
}

// WaitOptions is Options with the queue policy, resumable or not.
func WaitOptions(resume bool) conformance.RuntimeOptions {
	o := Options(HandlerFunc(Decision), "queue")
	o.Capabilities = domain.Capabilities{Resume: resume}
	return o
}
