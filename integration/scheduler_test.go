package integration_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
)

// Executor methods are independently controllable. A returned handle does not
// complete coordination until the scheduler observes its outcome explicitly.
type schedulerHandle struct {
	result extension.Result
	err    error
	wait   func(context.Context) (extension.Result, error)
}

func (h *schedulerHandle) Wait(ctx context.Context) (extension.Result, error) {
	if h.wait != nil {
		return h.wait(ctx)
	}
	return h.result, h.err
}

type schedulerExecutor struct {
	mu                                             sync.Mutex
	caps                                           domain.Capabilities
	runs, recoveries, injects, interrupts, resumes []domain.ID
	handles                                        map[domain.ID]extension.RunHandle
	notAdoptable                                   bool
	onRun                                          func(context.Context, extension.RunRequest) (extension.RunHandle, error)
	onRecover                                      func(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error)
	onInject                                       func(context.Context, domain.ID, domain.Event) error
	onInterrupt                                    func(context.Context, domain.ID) error
}

func (e *schedulerExecutor) Capabilities() domain.Capabilities {
	c := e.caps
	if c.Toolbelt == "" {
		c.Toolbelt = extension.ToolbeltNone
	}
	return c
}
func (e *schedulerExecutor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	e.mu.Lock()
	e.runs = append(e.runs, r.Execution.ID)
	hook := e.onRun
	e.mu.Unlock()
	if hook != nil {
		return hook(ctx, r)
	}
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{"handle":"durable"}`)}); err != nil {
		return nil, err
	}
	h := &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
	e.mu.Lock()
	if e.handles == nil {
		e.handles = map[domain.ID]extension.RunHandle{}
	}
	e.handles[r.Execution.ID] = h
	e.mu.Unlock()
	return h, nil
}
func (e *schedulerExecutor) Recover(ctx context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
	e.mu.Lock()
	e.recoveries = append(e.recoveries, r.Execution.ID)
	hook := e.onRecover
	bad := e.notAdoptable
	h := e.handles[r.Execution.ID]
	e.mu.Unlock()
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
func (e *schedulerExecutor) Inject(ctx context.Context, id domain.ID, event domain.Event) error {
	e.mu.Lock()
	e.injects = append(e.injects, id)
	hook := e.onInject
	e.mu.Unlock()
	if hook != nil {
		return hook(ctx, id, event)
	}
	return nil
}
func (e *schedulerExecutor) Interrupt(ctx context.Context, id domain.ID) error {
	e.mu.Lock()
	e.interrupts = append(e.interrupts, id)
	hook := e.onInterrupt
	e.mu.Unlock()
	if hook != nil {
		return hook(ctx, id)
	}
	return nil
}
func (e *schedulerExecutor) Resume(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errors.New("wait/resume belongs to02-05")
}
func (e *schedulerExecutor) counts() (int, int, int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.runs), len(e.recoveries), len(e.injects), len(e.interrupts)
}

type schedulerDriver struct {
	*routingDriver
	scheduler *runtime.Scheduler
	executor  *schedulerExecutor
	options   runtime.SchedulerOptions
}

func (d *schedulerDriver) Start(id domain.ID) (domain.LeaseRef, error) {
	a, err := d.scheduler.Start(context.Background(), id)
	return a.Lease, err
}
func (d *schedulerDriver) Complete(ref domain.LeaseRef) error {
	return d.scheduler.Complete(context.Background(), ref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}})
}
func (d *schedulerDriver) Fail(ref domain.LeaseRef) error {
	return d.scheduler.Complete(context.Background(), ref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed}})
}
func (d *schedulerDriver) Cancel(id domain.ID) error {
	return d.scheduler.Cancel(context.Background(), id)
}
func (d *schedulerDriver) Recover(id domain.ID, adoptable bool) (domain.LeaseRef, error) {
	d.executor.mu.Lock()
	d.executor.notAdoptable = !adoptable
	d.executor.mu.Unlock()
	a, err := d.scheduler.Recover(context.Background(), id)
	if !adoptable && errors.Is(err, extension.ErrNotAdoptable) {
		err = nil
	}
	return a.Lease, err
}
func (d *schedulerDriver) Retry(work, event domain.ID, manual bool) error {
	if event == "" && !manual {
		err := d.store.Transact(context.Background(), storage.Guard{Now: d.clock.Now()}, func(tx storage.Tx) error {
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
				if _, _, err = runtime.AcceptDerivedInTx(tx, d.clock.Now(), runtime.CausalLimits{}, w.ScopeID, timer.Event.CausationID, timer.Event); err != nil {
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
	return d.scheduler.ProcessRetry(context.Background(), event)
}
func schedulerFixture(t *testing.T, mode string, o conformance.RuntimeOptions) (conformance.RuntimeFixture, *schedulerExecutor) {
	t.Helper()
	f := routingFixture(t, mode, o)
	base := f.Driver.(*routingDriver)
	executor := &schedulerExecutor{caps: o.Capabilities}
	options := runtime.SchedulerOptions{Owner: "fixture-owner", LeaseDuration: conformance.RuntimeLease, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}, Retry: runtime.RetryPolicy{MaxAttempts: o.MaxAttempts}}
	makeDriver := func() *schedulerDriver {
		s, err := runtime.NewScheduler(f.Store, f.Clock.Now, base.router, options)
		if err != nil {
			t.Fatal(err)
		}
		return &schedulerDriver{base, s, executor, options}
	}
	f.Driver = makeDriver()
	f.Restart = func() conformance.RuntimeDriver { return makeDriver() }
	return f, executor
}
func TestProductionSchedulerSharedCases(t *testing.T) {
	selected := map[string]bool{"concurrent_correlations_and_attempts": true, "independent_work_progress": true, "first_match_replay_and_drop_audit": true, "multi_key_attachment_conflict_and_order": true, "uncorrelated_and_terminal_successor": true, "unsupported_policy_explicit_fallback": true, "completion_inject_orders": true, "lease_loss_adoption_and_stale_result": true, "retry_backoff_exhaustion_and_manual_event": true, "retry_cancel_orders": true, "unmatched_and_contradictory_target": true}
	for _, mode := range []string{"persistent", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			for _, c := range conformance.RuntimeCases() {
				if !selected[c.Name] {
					continue
				}
				t.Run(c.Name, func(t *testing.T) {
					if err := c.Check(func(o conformance.RuntimeOptions) conformance.RuntimeFixture {
						f, _ := schedulerFixture(t, mode, o)
						return f
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
func newTestScheduler(t *testing.T, f conformance.RuntimeFixture, o runtime.SchedulerOptions) *runtime.Scheduler {
	t.Helper()
	s, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).router, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func seedSchedulerWork(t *testing.T, f conformance.RuntimeFixture, id string) domain.ID {
	t.Helper()
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: id}
	requireAccept(t, f, "trigger/"+id, &key)
	requireRoute(t, f.Driver, domain.ID("trigger/"+id))
	return domain.ID("work/" + id)
}
func schedulerDefaultOptions(f conformance.RuntimeFixture, executor extension.Executor) runtime.SchedulerOptions {
	return runtime.SchedulerOptions{Owner: "other-owner", LeaseDuration: time.Minute, Generation: "fixture-generation", Executors: map[string]extension.Executor{"fixture": executor}}
}

func TestProductionSchedulerBlockedRunAllowsIndependentProgress(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	a := seedSchedulerWork(t, f, "a")
	b := seedSchedulerWork(t, f, "b")
	entered, release := make(chan struct{}), make(chan struct{})
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		if r.Work.ID == a {
			close(entered)
			<-release
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	first := f.Driver.(*schedulerDriver).scheduler
	second := newTestScheduler(t, f, schedulerDefaultOptions(f, executor))
	done := make(chan error, 1)
	go func() { _, err := first.Start(context.Background(), a); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not enter")
	}
	progress := make(chan error, 1)
	go func() {
		attempt, err := second.Start(context.Background(), b)
		if err == nil {
			err = second.Observe(context.Background(), attempt)
		}
		progress <- err
	}()
	select {
	case err := <-progress:
		close(release)
		if firstErr := <-done; firstErr != nil {
			t.Fatal(firstErr)
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("blocked Run serialized unrelated Work or held storage transaction")
	}
}

func TestProductionSchedulerCapacityAcrossInstances(t *testing.T) {
	for _, kind := range []string{"global", "scope", "ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
			a := seedSchedulerWork(t, f, "a")
			b := seedSchedulerWork(t, f, "b")
			o := schedulerDefaultOptions(f, executor)
			switch kind {
			case "global":
				o.GlobalLimit = 1
			case "scope":
				o.ScopeLimits = map[domain.ID]int{"routing-scope": 1}
			case "ancestor":
				routingRead(t, f, func(tx storage.Tx) error {
					if err := tx.PutScope(domain.Scope{ID: "parent", Name: "parent"}, 0); err != nil {
						return err
					}
					s, err := tx.Scope("routing-scope")
					if err != nil {
						return err
					}
					s.ParentID = "parent"
					return tx.PutScope(s, s.Revision)
				})
				o.ScopeLimits = map[domain.ID]int{"parent": 1}
			}
			one := newTestScheduler(t, f, o)
			o.Owner = "third-owner"
			two := newTestScheduler(t, f, o)
			gate := make(chan struct{})
			results := make(chan error, 2)
			go func() { <-gate; _, err := one.Start(context.Background(), a); results <- err }()
			go func() { <-gate; _, err := two.Start(context.Background(), b); results <- err }()
			close(gate)
			success := 0
			for range 2 {
				if <-results == nil {
					success++
				}
			}
			if success != 1 {
				t.Fatalf("%s cap admitted %d competing attempts", kind, success)
			}
			runs, _, _, _ := executor.counts()
			if runs != 1 {
				t.Fatalf("cap checked after %d Run calls", runs)
			}
			routingRead(t, f, func(tx storage.Tx) error {
				xs, err := tx.ListExecutions("")
				if err == nil && len(xs) != 1 {
					return fmt.Errorf("capacity rejection leaked attempt: %d", len(xs))
				}
				return err
			})
		})
	}
}

func TestProductionSchedulerRecoveryFencesAndPreservesAttempt(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "recover")
	first := f.Driver.(*schedulerDriver).scheduler
	attempt, err := first.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	second := newTestScheduler(t, f, schedulerDefaultOptions(f, executor))
	if _, err = second.Recover(context.Background(), attempt.Lease.ExecutionID); err == nil {
		t.Fatal("live lease stolen")
	}
	f.Clock.Advance(time.Minute)
	if err = first.Renew(context.Background(), attempt.Lease); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("exactexpiry renewal: %v", err)
	}
	recovered, err := second.Recover(context.Background(), attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Lease.ExecutionID != attempt.Lease.ExecutionID || recovered.Lease.Epoch <= attempt.Lease.Epoch {
		t.Fatalf("bad recovered authority: %+v", recovered.Lease)
	}
	if err = first.Complete(context.Background(), attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale completion committed: %v", err)
	}
	if err = attempt.Request.PersistRecovery(domain.Payload{Schema: "fixture.stale.v1", Data: []byte(`{}`)}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale recoverymetadata write committed: %v", err)
	}
	if err = second.Observe(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	runs, recovers, _, _ := executor.counts()
	if runs != 1 || recovers != 1 {
		t.Fatalf("recovery launched replacement: run%d recover%d", runs, recovers)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(work)
		if err == nil && (len(xs) != 1 || xs[0].State != domain.ExecutionCompleted) {
			return fmt.Errorf("lost recovered attempt history: %+v", xs)
		}
		return err
	})
}

func TestProductionSchedulerRecoveryFailureRequiresStopEvidence(t *testing.T) {
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "uncertain")
	s := f.Driver.(*schedulerDriver).scheduler
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(time.Minute)
	executor.mu.Lock()
	executor.notAdoptable = true
	executor.mu.Unlock()
	second := newTestScheduler(t, f, schedulerDefaultOptions(f, executor))
	failedRecovery, recoverErr := second.Recover(context.Background(), a.Lease.ExecutionID)
	if err = recoverErr; !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("unverifiable process result: %v", err)
	}
	f.Clock.Advance(5 * time.Minute)
	if err = f.Driver.(*schedulerDriver).Retry(work, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err = second.Start(context.Background(), work); err == nil {
		t.Fatal("unverifiable previous process allowed replacement")
	}
	runs, _, _, _ := executor.counts()
	if runs != 1 {
		t.Fatal("replacement Run before exclusion proof")
	}
	if err = second.ConfirmStopped(context.Background(), a.Lease.ExecutionID, failedRecovery.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err = second.Start(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	runs, _, _, _ = executor.counts()
	if runs != 2 {
		t.Fatalf("exclusion proof did not allow replacement: %d", runs)
	}
}

func TestProductionSchedulerPinnedExecutorAfterBindingReload(t *testing.T) {
	f, old := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "pin")
	first := f.Driver.(*schedulerDriver).scheduler
	a, err := first.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		s, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		s.Bindings[0].Executor = "replacement"
		s.Bindings[0].Config = domain.Payload{Schema: "fixture.config.v1", Data: []byte(`{"generation":2}`)}
		return tx.PutScope(s, s.Revision)
	})
	replacement := &schedulerExecutor{}
	o := schedulerDefaultOptions(f, old)
	o.Generation = "new-generation"
	o.HistoricalExecutors = map[string]map[string]extension.Executor{"fixture-generation": {"fixture": old}}
	o.Executors["replacement"] = replacement
	second := newTestScheduler(t, f, o)
	f.Clock.Advance(time.Minute)
	recovered, err := second.Recover(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Request.Execution.ID != a.Request.Execution.ID || !reflect.DeepEqual(recovered.Request.Context.Config, a.Request.Context.Config) {
		t.Fatal("recovery used reloaded binding/config instead of attempt snapshot")
	}
	_, oldRecovers, _, _ := old.counts()
	newRuns, newRecovers, _, _ := replacement.counts()
	if oldRecovers != 1 || newRuns != 0 || newRecovers != 0 {
		t.Fatalf("recovery switched executors: oldRecover%d newRun%d newRecover%d", oldRecovers, newRuns, newRecovers)
	}
}

func TestProductionSchedulerReleaseRequiresSameAttemptRecovery(t *testing.T) {
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "release")
	s := f.Driver.(*schedulerDriver).scheduler
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Release(context.Background(), a.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(context.Background(), work); err == nil {
		t.Fatal("release permitted second attempt")
	}
	second := newTestScheduler(t, f, schedulerDefaultOptions(f, e))
	recovered, err := second.Recover(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Lease.Epoch <= a.Lease.Epoch || recovered.Lease.ExecutionID != a.Lease.ExecutionID {
		t.Fatal("release reset attempt identity or epoch")
	}
	if runs, recoveries, _, _ := e.counts(); runs != 1 || recoveries != 1 {
		t.Fatalf("release launched replacement: run%d recover%d", runs, recoveries)
	}
}

func TestProductionSchedulerCancellationAndOutcomeOrders(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelFirst), func(t *testing.T) {
			f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
			work := seedSchedulerWork(t, f, "cancel")
			s := f.Driver.(*schedulerDriver).scheduler
			a, err := s.Start(context.Background(), work)
			if err != nil {
				t.Fatal(err)
			}
			if cancelFirst {
				if err = s.Cancel(context.Background(), work); err != nil {
					t.Fatal(err)
				}
				if err = s.Observe(context.Background(), a); !errors.Is(err, storage.ErrLeaseLost) {
					t.Fatalf("cancelled outcome regained authority: %v", err)
				}
			} else {
				if err = s.Observe(context.Background(), a); err != nil {
					t.Fatal(err)
				}
				if err = s.Cancel(context.Background(), work); err != nil {
					t.Fatal(err)
				}
			}
			routingRead(t, f, func(tx storage.Tx) error {
				w, err := tx.Work(work)
				if err != nil {
					return err
				}
				x, err := tx.Execution(a.Lease.ExecutionID)
				if err != nil {
					return err
				}
				wantWork, wantAttempt := domain.WorkCompleted, domain.ExecutionCompleted
				if cancelFirst {
					wantWork, wantAttempt = domain.WorkCancelled, domain.ExecutionCancelled
				}
				if w.State != wantWork || x.State != wantAttempt {
					return fmt.Errorf("terminalwinner changed: work%s attempt%s", w.State, x.State)
				}
				return nil
			})
		})
	}
}

func TestProductionSchedulerScopeLimitDoesNotBecomeGlobal(t *testing.T) {
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	a := seedSchedulerWork(t, f, "limited")
	routingRead(t, f, func(tx storage.Tx) error {
		s, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		s.ID = "other-scope"
		s.Name = "other"
		return tx.PutScope(s, 0)
	})
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "independent-scope"}
	event := routingEvent(f, "other-trigger", &key)
	if _, _, err := f.Driver.(*schedulerDriver).ingress.Accept(context.Background(), "other-scope", event); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, event.ID)
	o := schedulerDefaultOptions(f, e)
	o.ScopeLimits = map[domain.ID]int{"routing-scope": 1}
	one := newTestScheduler(t, f, o)
	o.Owner = "third-owner"
	two := newTestScheduler(t, f, o)
	if _, err := one.Start(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := two.Start(context.Background(), "work/independent-scope"); err != nil {
		t.Fatalf("scope cap blocked independent scope: %v", err)
	}
}

type schedulerHintError struct{ hint extension.RetryHint }

func (schedulerHintError) Error() string                    { return "fixture generic rate limit" }
func (e schedulerHintError) RetryHint() extension.RetryHint { return e.hint }
func TestProductionSchedulerRetryHintValidation(t *testing.T) {
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "hint")
	s := f.Driver.(*schedulerDriver).scheduler
	reset := f.Clock.Now().Add(10 * time.Minute)
	e.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{err: schedulerHintError{runtime.RetryHint{ResetAt: &reset}}}, nil
	}
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Observe(context.Background(), a)
	state, err := s.RetryStatus(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if !state.EligibleAt.Equal(reset.Add(2 * time.Minute)) {
		t.Fatalf("retry hint not applied: %+v", state)
	}
	zero := time.Time{}
	past := f.Clock.Now().Add(-3 * time.Minute)
	tooFar := f.Clock.Now().Add(366 * 24 * time.Hour)
	for _, hint := range []runtime.RetryHint{{ResetAt: &zero}, {ResetAt: &past}, {ResetAt: &tooFar}} {
		if _, err := runtime.RetryDelay(f.Clock.Now(), hint); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("invalid retryhint accepted: %+v %v", hint, err)
		}
	}
	if delay, err := runtime.RetryDelay(f.Clock.Now(), runtime.RetryHint{}); err != nil || delay != 30*time.Minute {
		t.Fatalf("unknown reset delay: %v %v", delay, err)
	}
}

func TestProductionSchedulerInjectCompletionRaceRetainsInput(t *testing.T) {
	o := routingOptions(routingHandlerFunc(routingDecision), "inject")
	o.Capabilities = domain.Capabilities{Inject: true}
	f, e := schedulerFixture(t, "reopen", o)
	work := seedSchedulerWork(t, f, "inject")
	s := f.Driver.(*schedulerDriver).scheduler
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "inject"}
	requireAccept(t, f, "incoming", &key)
	requireRoute(t, f.Driver, "incoming")
	intents, err := f.Driver.(*schedulerDriver).router.Intents(context.Background(), work)
	if err != nil || len(intents) != 1 {
		t.Fatalf("intent: %+v %v", intents, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	e.onInject = func(context.Context, domain.ID, domain.Event) error { close(entered); <-release; return nil }
	done := make(chan error, 1)
	go func() { done <- s.DispatchControl(context.Background(), intents[0].ID) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Inject not invoked")
	}
	// Complete must transact while Inject is still blocked outside storage.
	completed := make(chan error, 1)
	go func() { completed <- s.Observe(context.Background(), a) }()
	select {
	case err = <-completed:
		if err != nil {
			close(release)
			<-done
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("Inject held coordination transaction")
	}
	pending, err := f.Driver.(*schedulerDriver).router.PendingInputs(context.Background(), work)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].EventID != "incoming" {
		close(release)
		<-done
		t.Fatalf("inflight injection discarded input: %+v", pending)
	}
	next, err := s.Start(context.Background(), work)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err = <-done; err != nil && !errors.Is(err, runtime.ErrControlDispatch) {
		t.Fatal(err)
	}
	if err = s.DispatchControl(context.Background(), intents[0].ID); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	targets := append([]domain.ID(nil), e.injects...)
	e.mu.Unlock()
	if !reflect.DeepEqual(targets, []domain.ID{a.Lease.ExecutionID}) || next.Lease.ExecutionID == a.Lease.ExecutionID {
		t.Fatalf("old injection retargeted replacement: %v", targets)
	}
}

func TestProductionSchedulerInterruptNeedsStopProof(t *testing.T) {
	o := routingOptions(routingHandlerFunc(routingDecision), "interrupt")
	o.Capabilities = domain.Capabilities{Interrupt: true}
	f, e := schedulerFixture(t, "reopen", o)
	work := seedSchedulerWork(t, f, "interrupt")
	s := f.Driver.(*schedulerDriver).scheduler
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "interrupt"}
	requireAccept(t, f, "incoming", &key)
	requireRoute(t, f.Driver, "incoming")
	intents, err := f.Driver.(*schedulerDriver).router.Intents(context.Background(), work)
	if err != nil || len(intents) != 1 {
		t.Fatalf("intent: %+v %v", intents, err)
	}
	e.onInterrupt = func(ctx context.Context, id domain.ID) error {
		// Authority must be fenced before the outside-transaction callback runs.
		return f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
			x, err := tx.Execution(id)
			if err == nil && x.State != domain.ExecutionCancelled {
				return fmt.Errorf("Interrupt before fencing: %s", x.State)
			}
			return err
		})
	}
	if err = s.DispatchControl(context.Background(), intents[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(context.Background(), a.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("interrupted result retained authority: %v", err)
	}
	if _, err = s.Start(context.Background(), work); !errors.Is(err, runtime.ErrResourceHeld) {
		t.Fatalf("interrupt acknowledgement treated as stopped: %v", err)
	}
	if err = s.ConfirmStopped(context.Background(), a.Lease.ExecutionID, a.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if runs, _, _, interrupts := e.counts(); runs != 2 || interrupts != 1 {
		t.Fatalf("wrong control/replacement counts: run%d interrupt%d", runs, interrupts)
	}
}

func TestProductionSchedulerManualRetryCannotPredateFailure(t *testing.T) {
	f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "manual")
	s := f.Driver.(*schedulerDriver).scheduler
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	event := routingEvent(f, "early-manual", nil)
	event.Type = "core.retry.requested"
	event.WorkID = work
	if err = f.Driver.Accept(event); err != nil {
		t.Fatal(err)
	}
	if err = f.Driver.(*schedulerDriver).Fail(a.Lease); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessRetry(context.Background(), event.ID); err != nil {
		t.Fatal(err)
	}
	state, err := s.RetryStatus(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Blocked || state.FailureCount != 1 {
		t.Fatalf("old accepted retry reset newerfailure: %+v", state)
	}
	if _, err = s.Start(context.Background(), work); err == nil {
		t.Fatal("old retry bypassed current backoff")
	}
}

func TestProductionSchedulerTimeoutAndHandoff(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
		work := seedSchedulerWork(t, f, "timeout")
		options := schedulerDefaultOptions(f, executor)
		options.Timeout = 30 * time.Second
		s := newTestScheduler(t, f, options)
		a, err := s.Start(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Timeout(context.Background(), a.Lease); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("timeout before persisted deadline: %v", err)
		}
		f.Clock.Advance(30 * time.Second)
		if err = s.Timeout(context.Background(), a.Lease); err != nil {
			t.Fatal(err)
		}
		if err = s.Complete(context.Background(), a.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
			t.Fatalf("timeout lateoutcome: %v", err)
		}
		state, err := s.InspectAttempt(context.Background(), a.Lease.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		if state.Excluded {
			t.Fatal("timeout claimed physicalprocess stopped")
		}
		retry, err := s.RetryStatus(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		if !retry.Blocked || retry.FailureCount != 1 {
			t.Fatalf("timeout did not use boundedfailurepolicy: %+v", retry)
		}
	})
	t.Run("handoff", func(t *testing.T) {
		f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
		work := seedSchedulerWork(t, f, "handoff")
		s := f.Driver.(*schedulerDriver).scheduler
		a, err := s.Start(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Handoff(context.Background(), a.Lease, domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "fixture transition"}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Start(context.Background(), work); err == nil {
			t.Fatal("handoff invented runnableinput")
		}
		routingRead(t, f, func(tx storage.Tx) error {
			w, err := tx.Work(work)
			if err != nil {
				return err
			}
			x, err := tx.Execution(a.Lease.ExecutionID)
			if err == nil && (w.State != domain.WorkReady || x.State != domain.ExecutionHandoff) {
				return fmt.Errorf("wrong handoff lifecycle: work%s attempt%s", w.State, x.State)
			}
			return err
		})
	})
}

func TestProductionSchedulerAmbiguousRunRestartUsesRecover(t *testing.T) {
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "ambiguous")
	s := f.Driver.(*schedulerDriver).scheduler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{"started":true}`)}); err != nil {
			return nil, err
		}
		e.mu.Lock()
		e.handles = map[domain.ID]extension.RunHandle{r.Execution.ID: &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}}
		e.mu.Unlock()
		cancel()
		return nil, ctx.Err()
	}
	a, err := s.Start(ctx, work)
	if err == nil {
		t.Fatal("ambiguous cancelled Run reported success")
	}
	second := newTestScheduler(t, f, schedulerDefaultOptions(f, e))
	if _, err = second.Start(context.Background(), work); err == nil {
		t.Fatal("restart repeated Run after ambiguous launch")
	}
	f.Clock.Advance(time.Minute)
	recovered, err := second.Recover(context.Background(), a.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Observe(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	runs, recovers, _, _ := e.counts()
	if runs != 1 || recovers != 1 {
		t.Fatalf("ambiguous launch duplicated Run: run%d recover%d", runs, recovers)
	}
}

func TestProductionSchedulerControlLeaseRejectsLateAcknowledgement(t *testing.T) {
	o := routingOptions(routingHandlerFunc(routingDecision), "inject")
	o.Capabilities = domain.Capabilities{Inject: true}
	f, e := schedulerFixture(t, "reopen", o)
	work := seedSchedulerWork(t, f, "control-expiry")
	s := f.Driver.(*schedulerDriver).scheduler
	a, err := s.Start(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "control-expiry"}
	requireAccept(t, f, "incoming", &key)
	requireRoute(t, f.Driver, "incoming")
	intents, err := f.Driver.(*schedulerDriver).router.Intents(context.Background(), work)
	if err != nil || len(intents) != 1 {
		t.Fatalf("intent: %+v %v", intents, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.onInject = func(context.Context, domain.ID, domain.Event) error {
		once.Do(func() { close(entered); <-release })
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.DispatchControl(context.Background(), intents[0].ID) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Inject did not enter")
	}
	f.Clock.Advance(30 * time.Second)
	if err = s.Renew(context.Background(), a.Lease); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	f.Clock.Advance(30 * time.Second)
	close(release)
	if err = <-done; !errors.Is(err, runtime.ErrControlBusy) {
		t.Fatalf("expired controlclaim acknowledged: %v", err)
	}
	// The target attempt still has a live renewed lease. Retry uses the exact
	// same intent, event and execution rather than redirecting to another attempt.
	if err = s.DispatchControl(context.Background(), intents[0].ID); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	targets := append([]domain.ID(nil), e.injects...)
	e.mu.Unlock()
	if !reflect.DeepEqual(targets, []domain.ID{a.Lease.ExecutionID, a.Lease.ExecutionID}) {
		t.Fatalf("ambiguous control changed target: %v", targets)
	}
	status, err := s.ControlStatus(context.Background(), intents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "accepted" || status.Attempts != 2 {
		t.Fatalf("control retry state: %+v", status)
	}
}
