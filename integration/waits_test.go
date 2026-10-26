package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// Wait fixtures use the same real claim/Run/recovery implementation as the
// scheduler cases. Resume is a controllable executor operation, not a storage
// shortcut or a reference coordinator.
type waitExecutor struct {
	*schedulerExecutor
	resumeRequests []extension.RunRequest
}

func (e *waitExecutor) Resume(_ context.Context, r extension.RunRequest, p domain.Payload) (extension.RunHandle, error) {
	if err := r.PersistRecovery(p); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resumes = append(e.resumes, r.Execution.ID)
	e.resumeRequests = append(e.resumeRequests, r)
	h := e.handles[r.Execution.ID]
	if h == nil {
		h = &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
	}
	return h, nil
}

type waitDriver struct {
	*schedulerDriver
	timers             *runtime.Timers
	causal             *runtime.Causal
	executorWithResume *waitExecutor
}

func (d *waitDriver) Wait(ref domain.LeaseRef, w domain.Wait, resume bool) error {
	return d.scheduler.Wait(context.Background(), ref, w, resume)
}
func (d *waitDriver) Wake(id domain.ID) error { return d.scheduler.Wake(context.Background(), id) }
func (d *waitDriver) Derive(e domain.Event) error {
	_, _, err := d.causal.Derive(context.Background(), "routing-scope", e.CausationID, e)
	return err
}
func (d *waitDriver) Retry(work, event domain.ID, manual bool) error {
	if event == "" && !manual {
		var found domain.Timer
		err := d.store.Transact(context.Background(), storage.Guard{Now: d.clock.Now()}, func(tx storage.Tx) error {
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
		if _, err = d.timers.Fire(context.Background(), found.ID, found.Revision, nil); err != nil {
			return err
		}
		event = found.Event.ID
	}
	return d.scheduler.ProcessRetry(context.Background(), event)
}
func waitFixture(t *testing.T, mode string, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	f, baseExecutor := schedulerFixture(t, mode, o)
	base := f.Driver.(*schedulerDriver)
	executor := &waitExecutor{schedulerExecutor: baseExecutor}
	options := base.options
	options.Executors = map[string]extension.Executor{"fixture": executor}
	options.CausalLimits = runtime.CausalLimits{MaxDepth: o.MaxDepth, PerMinute: o.PerMinute}
	makeDriver := func() *waitDriver {
		s, err := runtime.NewScheduler(f.Store, f.Clock.Now, base.router, options)
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
		return &waitDriver{schedulerDriver: &schedulerDriver{routingDriver: base.routingDriver, scheduler: s, executor: baseExecutor, options: options}, timers: timers, causal: causal, executorWithResume: executor}
	}
	f.Driver = makeDriver()
	f.Restart = func() conformance.RuntimeDriver { return makeDriver() }
	return f
}
func waitOptions(resume bool) conformance.RuntimeOptions {
	o := routingOptions(routingHandlerFunc(routingDecision), "queue")
	o.Capabilities = domain.Capabilities{Resume: resume}
	return o
}
func TestProductionRuntimeAllSharedCases(t *testing.T) {
	for _, mode := range []string{"persistent", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			conformance.RunRuntime(t, func(o conformance.RuntimeOptions) conformance.RuntimeFixture { return waitFixture(t, mode, o) })
		})
	}
}
func waitForWork(t *testing.T, f conformance.RuntimeFixture, work string, resume bool) (domain.ID, domain.LeaseRef, domain.CorrelationKey) {
	t.Helper()
	id := seedSchedulerWork(t, f, work)
	ref, err := f.Driver.Start(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, ref, domain.CorrelationKey{Source: "fixture/account", ExternalID: work}
}
func registerWait(t *testing.T, f conformance.RuntimeFixture, ref domain.LeaseRef, id domain.ID, key domain.CorrelationKey, resume bool, after uint64) {
	t.Helper()
	w := domain.Wait{ID: id, AfterSequence: after, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.changed"}}}
	if err := f.Driver.Wait(ref, w, resume); err != nil {
		t.Fatal(err)
	}
}

func TestProductionWaitAlreadyRoutedMatchAndRestart(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			f := waitFixture(t, "reopen", waitOptions(resume))
			work, ref, key := waitForWork(t, f, "already", resume)
			requireAccept(t, f, "already-observed", &key)
			requireRoute(t, f.Driver, "already-observed")
			registerWait(t, f, ref, "waiting", key, resume, 1)
			f.Driver = f.Restart()
			if err := f.Driver.Wake("waiting"); err != nil {
				t.Fatal(err)
			}
			next, err := f.Driver.Start(work)
			if err != nil {
				t.Fatal(err)
			}
			if resume && (next.ExecutionID != ref.ExecutionID || next.Epoch <= ref.Epoch) || !resume && next.ExecutionID == ref.ExecutionID {
				t.Fatalf("wrong continuation: old%+v new%+v", ref, next)
			}
			routingRead(t, f, func(tx storage.Tx) error {
				w, err := tx.Wait("waiting")
				if err != nil {
					return err
				}
				if w.State != domain.WaitWoken || w.WakeEventID != "already-observed" {
					return fmt.Errorf("lost registration backscan winner: %+v", w)
				}
				return nil
			})
			if err = f.Driver.Complete(ref); !errors.Is(err, storage.ErrLeaseLost) {
				t.Fatalf("old registration authority survived: %v", err)
			}
			if err = f.Driver.Complete(next); err != nil {
				t.Fatalf("resumed completion blocked by internal wake history: %v", err)
			}
		})
	}
}

func TestProductionWaitPredicateFiltersAndObservationBoundary(t *testing.T) {
	f := waitFixture(t, "reopen", waitOptions(false))
	work, ref, key := waitForWork(t, f, "filter", false)
	w := domain.Wait{ID: "filtered", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.allowed"}}}
	if err := f.Driver.Wait(ref, w, false); err != nil {
		t.Fatal(err)
	}
	nonmatching := routingEvent(f, "wrong-type", &key)
	nonmatching.Type = "fixture.other"
	if err := f.Driver.Accept(nonmatching); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, nonmatching.ID)
	if err := f.Driver.Wake(w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Driver.Start(work); err == nil {
		t.Fatal("ordinary input bypassed active Wait")
	}
	matched := routingEvent(f, "right-type", &key)
	matched.Type = "fixture.allowed"
	if err := f.Driver.Accept(matched); err != nil {
		t.Fatal(err)
	}
	if err := f.Driver.Wake(w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Driver.Start(work); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		wait, err := tx.Wait(w.ID)
		if err != nil {
			return err
		}
		if wait.WakeEventID != matched.ID {
			return fmt.Errorf("wrong type woke Wait: %+v", wait)
		}
		return nil
	})
}

func TestProductionWaitConcurrentWakeExactlyOnce(t *testing.T) {
	f := waitFixture(t, "reopen", waitOptions(true))
	work, ref, key := waitForWork(t, f, "race", true)
	registerWait(t, f, ref, "race-wait", key, true, 1)
	requireAccept(t, f, "first", &key)
	requireAccept(t, f, "second", &key)
	gate := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		d := f.Restart()
		go func() { <-gate; results <- d.Wake("race-wait") }()
	}
	close(gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	next, err := f.Driver.Start(work)
	if err != nil {
		t.Fatal(err)
	}
	d := f.Driver.(*waitDriver)
	d.executorWithResume.mu.Lock()
	requests := append([]extension.RunRequest(nil), d.executorWithResume.resumeRequests...)
	d.executorWithResume.mu.Unlock()
	if len(requests) != 1 || requests[0].Trigger.ID != "first" || requests[0].Execution.TriggerID != "trigger/race" || next.ExecutionID != ref.ExecutionID {
		t.Fatalf("lost immutable trigger or duplicate resume: %+v", requests)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, err := tx.Wait("race-wait")
		if err != nil {
			return err
		}
		if w.WakeEventID != "first" {
			return fmt.Errorf("lowest acceptance sequence lost: %+v", w)
		}
		return nil
	})
}

func TestProductionResumeRetainsCausalDepth(t *testing.T) {
	o := waitOptions(true)
	o.MaxDepth = 4
	o.PerMinute = 100
	f := waitFixture(t, "reopen", o)
	work, ref, key := waitForWork(t, f, "causal-resume", true)
	parent := domain.ID("trigger/causal-resume")
	for n := 1; n <= 2; n++ {
		var boundary uint64
		routingRead(t, f, func(tx storage.Tx) error {
			all, err := tx.Events(0, 0)
			if err == nil {
				boundary = all[len(all)-1].Sequence
			}
			return err
		})
		registerWait(t, f, ref, domain.ID(fmt.Sprintf("wait-%d", n)), key, true, boundary)
		d := f.Driver.(*waitDriver)
		wake := routingEvent(f, fmt.Sprintf("wake-%d", n), &key)
		wake.WorkID = work
		accepted, _, err := d.causal.Derive(context.Background(), "routing-scope", parent, wake)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.Wake(domain.ID(fmt.Sprintf("wait-%d", n))); err != nil {
			t.Fatal(err)
		}
		ref, err = d.Start(work)
		if err != nil {
			t.Fatal(err)
		}
		state, err := d.scheduler.InspectAttempt(context.Background(), ref.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		parent = state.CausalTriggerID
		if parent == "" {
			t.Fatal("resume omitted durable causal bridge")
		}
		if state.WakeEventID != accepted.Event.ID {
			t.Fatal("resume lost accepted winner linkage")
		}
		if n == 1 {
			emitted := routingEvent(f, "after-first-resume", nil)
			a, _, err := d.causal.Emit(context.Background(), ref, emitted)
			if err != nil {
				t.Fatal(err)
			}
			if a.Event.CausationID != parent {
				t.Fatalf("resume severed immediate ancestry: %+v", a.Event)
			}
			requireRoute(t, d, a.Event.ID)
		}
		f.Driver = f.Restart()
	}
	d := f.Driver.(*waitDriver)
	if _, _, err := d.causal.Emit(context.Background(), ref, routingEvent(f, "too-deep-after-resume", nil)); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("resuming reset depth bound: %v", err)
	}
}

func TestProductionDependencySubscriptionBackscanAndReplay(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(false))
	work, ref, target := waitForWork(t, f, "dependent", false)
	sourceWork := seedSchedulerWork(t, f, "prerequisite")
	source := domain.CorrelationKey{Source: "fixture/account", ExternalID: "prerequisite"}
	observed := routingEvent(f, "prerequisite-ready", &source)
	observed.Type = "fixture.ready"
	observed.Data = domain.Payload{Schema: "fixture.private.v1", Data: []byte(`{"message":"private-source-body"}`)}
	if err := f.Driver.Accept(observed); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, observed.ID)
	wait := domain.Wait{ID: "dependency-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{target}, Types: []string{"fixture.alias"}}}
	if err := f.Driver.Wait(ref, wait, false); err != nil {
		t.Fatal(err)
	}
	d := f.Driver.(*waitDriver)
	request := runtime.SubscriptionRequest{ID: "dependency-subscription", WaitID: wait.ID, SourceScopeID: "routing-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias"}
	if err := d.scheduler.Subscribe(ctx, request); err != nil {
		t.Fatal(err)
	}
	f.Driver = f.Restart()
	d = f.Driver.(*waitDriver)
	for range 2 {
		if err := d.scheduler.ReconcileSubscriptions(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Wake(wait.ID); err != nil {
		t.Fatal(err)
	}
	var aliases []domain.AcceptedEvent
	routingRead(t, f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		for _, a := range all {
			if a.Event.Type == "fixture.alias" {
				aliases = append(aliases, a)
			}
		}
		owner, err := tx.WorkByCorrelation(source)
		if err != nil {
			return err
		}
		if owner.ID != sourceWork {
			return errors.New("subscription transferred source ownership")
		}
		w, err := tx.Wait(wait.ID)
		if err != nil {
			return err
		}
		if len(aliases) != 1 || w.WakeEventID != aliases[0].Event.ID {
			return fmt.Errorf("alias acceptance/replay was not atomic: aliases%+v wait%+v", aliases, w)
		}
		alias := aliases[0].Event
		if alias.CausationID != observed.ID || alias.WorkID != work || alias.CorrelationKey == nil || *alias.CorrelationKey != target {
			return fmt.Errorf("alias lost trusted ancestry/target: %+v", alias)
		}
		if string(alias.Data.Data) == string(observed.Data.Data) {
			return errors.New("subscription copied source private payload")
		}
		return nil
	})
	if _, err := d.Start(work); err != nil {
		t.Fatal(err)
	}
}

func TestProductionDependencySubscriptionCancelledWaitEmitsNothing(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(false))
	work, ref, target := waitForWork(t, f, "cancel-dependent", false)
	seedSchedulerWork(t, f, "cancel-source")
	source := domain.CorrelationKey{Source: "fixture/account", ExternalID: "cancel-source"}
	w := domain.Wait{ID: "cancel-sub-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{target}, Types: []string{"fixture.alias"}}}
	if err := f.Driver.Wait(ref, w, false); err != nil {
		t.Fatal(err)
	}
	d := f.Driver.(*waitDriver)
	if err := d.scheduler.Subscribe(ctx, runtime.SubscriptionRequest{ID: "cancel-sub", WaitID: w.ID, SourceScopeID: "routing-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Cancel(work); err != nil {
		t.Fatal(err)
	}
	observed := routingEvent(f, "ready-after-cancel", &source)
	observed.Type = "fixture.ready"
	if err := d.Accept(observed); err != nil {
		t.Fatal(err)
	}
	f.Driver = f.Restart()
	d = f.Driver.(*waitDriver)
	if err := d.scheduler.ReconcileSubscriptions(ctx, 10); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		for _, a := range all {
			if a.Event.Type == "fixture.alias" {
				return errors.New("cancelled subscription emitted runnable alias")
			}
		}
		return nil
	})
}

func TestProductionDependencySubscriptionRequiresCrossScopeAuthorization(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(false))
	_, ref, target := waitForWork(t, f, "cross-scope-dependent", false)
	source := domain.CorrelationKey{Source: "fixture/foreign", ExternalID: "prerequisite"}
	routingRead(t, f, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "source-policy", Name: "source-policy"}, 0); err != nil {
			return err
		}
		if err := tx.PutScope(domain.Scope{ID: "source-scope", Name: "source", ParentID: "source-policy"}, 0); err != nil {
			return err
		}
		return tx.PutWork(domain.Work{ID: "foreign-owner", Kind: "fixture.work", ScopeID: "source-scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{source}}, 0)
	})
	observed := routingEvent(f, "cross-scope-ready", &source)
	observed.Type = "fixture.ready"
	d := f.Driver.(*waitDriver)
	if _, _, err := d.ingress.Accept(ctx, "source-scope", observed); err != nil {
		t.Fatal(err)
	}
	w := domain.Wait{ID: "cross-scope-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{target}, Types: []string{"fixture.alias"}}}
	if err := d.Wait(ref, w, false); err != nil {
		t.Fatal(err)
	}
	request := runtime.SubscriptionRequest{ID: "cross-scope-sub", WaitID: w.ID, SourceScopeID: "source-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias"}
	if err := d.scheduler.Subscribe(ctx, request); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("cross-scope alias without policy: %v", err)
	}
	options := d.options
	var calls int
	mutateAncestor := true
	options.AuthorizeAlias = func(ctx context.Context, a runtime.AliasAuthorization) error {
		calls++
		if a.SourceScopeID != "source-scope" || a.TargetScopeID != "routing-scope" || a.Generation != "fixture-generation" {
			return fmt.Errorf("incorrect authorization context: %+v", a)
		}
		// A policy can read independent state: this would fail if invoked while the
		// scheduler held an SQLite write transaction around external policy code.
		return f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
			policy, err := tx.Scope("source-policy")
			if err != nil {
				return err
			}
			if mutateAncestor {
				mutateAncestor = false
				policy.Name = "changed-policy"
				return tx.PutScope(policy, policy.Revision)
			}
			return nil
		})
	}
	authorized, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.router, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = authorized.Subscribe(ctx, request); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("ancestor policy changed during authorization: %v", err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		for _, a := range all {
			if a.Event.Type == "fixture.alias" {
				return errors.New("stale authorization published alias")
			}
		}
		return nil
	})
	if err = authorized.Subscribe(ctx, request); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatal("installed policy not consulted")
	}
	routingRead(t, f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		count := 0
		for _, a := range all {
			if a.Event.Type == "fixture.alias" {
				count++
				if a.Event.CausationID != observed.ID {
					return errors.New("cross-scope alias lost parent")
				}
			}
		}
		if count != 1 {
			return fmt.Errorf("authorized backscan aliases=%d", count)
		}
		return nil
	})
}

func TestProductionWaitWakeCommitSurvivesProcessKill(t *testing.T) {
	f := waitFixture(t, "reopen", waitOptions(false))
	work, ref, key := waitForWork(t, f, "wake-crash", false)
	registerWait(t, f, ref, "wake-crash-wait", key, false, 1)
	requireAccept(t, f, "wake-before-crash", &key)
	path := f.Store.(*routingReopening).path
	cmd := exec.Command(os.Args[0], "-test.run=^TestProductionWaitWakeCrashHelper$")
	cmd.Env = append(os.Environ(), "CONDUIT_WAIT_CRASH_DB="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	committed := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		if scanner.Scan() {
			committed <- scanner.Text()
		} else {
			committed <- ""
		}
	}()
	select {
	case barrier := <-committed:
		if barrier != "WAKE_COMMITTED" {
			t.Fatalf("wake helper failed: %s %s", barrier, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wake helper did not reach commit barrier")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	f.Driver = f.Restart()
	d := f.Driver.(*waitDriver)
	for range 2 {
		if err = d.Wake("wake-crash-wait"); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := d.router.PendingInputs(context.Background(), work)
	if err != nil || len(inputs) != 1 || inputs[0].EventID != "wake-before-crash" {
		t.Fatalf("restart duplicated/lost wake input: %+v %v", inputs, err)
	}
	if _, err = d.Start(work); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, err := tx.Wait("wake-crash-wait")
		if err != nil {
			return err
		}
		xs, err := tx.ListExecutions(work)
		if err != nil {
			return err
		}
		if w.State != domain.WaitWoken || w.WakeEventID != "wake-before-crash" || len(xs) != 2 {
			return fmt.Errorf("wake crash lost committed history: %+v %+v", w, xs)
		}
		return nil
	})
}
func TestProductionWaitWakeCrashHelper(t *testing.T) {
	path := os.Getenv("CONDUIT_WAIT_CRASH_DB")
	if path == "" {
		return
	}
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := conformance.NewClock()
	router, err := runtime.NewRouter(db, clock.Now, runtime.RouterOptions{Generation: "fixture-generation"})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := runtime.NewScheduler(db, clock.Now, router, runtime.SchedulerOptions{Owner: "crash-helper", Generation: "fixture-generation"})
	if err != nil {
		t.Fatal(err)
	}
	if err = scheduler.Wake(context.Background(), "wake-crash-wait"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("WAKE_COMMITTED")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestProductionExternalResumeCannotResetOriginalRoot(t *testing.T) {
	o := waitOptions(true)
	o.MaxDepth = 2
	o.PerMinute = 100
	f := waitFixture(t, "reopen", o)
	work, ref, key := waitForWork(t, f, "external-loop", true)
	for n := 1; n <= 3; n++ {
		var boundary uint64
		routingRead(t, f, func(tx storage.Tx) error {
			all, err := tx.Events(0, 0)
			if err == nil {
				boundary = all[len(all)-1].Sequence
			}
			return err
		})
		id := domain.ID(fmt.Sprintf("external-wait-%d", n))
		registerWait(t, f, ref, id, key, true, boundary)
		requireAccept(t, f, fmt.Sprintf("independent-wake-%d", n), &key)
		d := f.Driver.(*waitDriver)
		if err := d.Wake(id); err != nil {
			t.Fatal(err)
		}
		next, err := d.Start(work)
		if n <= 2 {
			if err != nil {
				t.Fatal(err)
			}
			if next.ExecutionID != ref.ExecutionID || next.Epoch <= ref.Epoch {
				t.Fatalf("resume authority changed attempt: %+v %+v", ref, next)
			}
			ref = next
		} else if err == nil {
			t.Fatal("independent external wake reset original causal depth")
		}
		f.Driver = f.Restart()
	}
	audits, err := f.Driver.(*waitDriver).causal.Rejections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, audit := range audits {
		if audit.RootID == "trigger/external-loop" && audit.Reason == "depth_limit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing originalroot refusal: %+v", audits)
	}
}

func TestProductionSubscriptionPollFairnessSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(false))
	targets := map[string]domain.CorrelationKey{}
	waits := map[string]domain.ID{}
	sources := map[string]domain.CorrelationKey{}
	for _, name := range []string{"first", "second"} {
		_, ref, key := waitForWork(t, f, "fair-"+name, false)
		var boundary uint64
		routingRead(t, f, func(tx storage.Tx) error {
			all, err := tx.Events(0, 0)
			if err == nil {
				boundary = all[len(all)-1].Sequence
			}
			return err
		})
		waitID := domain.ID("fair-wait-" + name)
		if err := f.Driver.Wait(ref, domain.Wait{ID: waitID, AfterSequence: boundary, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.alias"}}}, false); err != nil {
			t.Fatal(err)
		}
		scope := domain.ID("fair-source-" + name)
		source := domain.CorrelationKey{Source: "fixture/" + name, ExternalID: "source"}
		routingRead(t, f, func(tx storage.Tx) error {
			if err := tx.PutScope(domain.Scope{ID: scope, Name: string(scope)}, 0); err != nil {
				return err
			}
			return tx.PutWork(domain.Work{ID: domain.ID("fair-source-owner-" + name), Kind: "fixture.work", ScopeID: scope, State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{source}}, 0)
		})
		targets[string(scope)] = key
		waits[string(scope)] = waitID
		sources[string(scope)] = source
	}
	d := f.Driver.(*waitDriver)
	options := d.options
	var polled []domain.ID
	options.AuthorizeAlias = func(_ context.Context, a runtime.AliasAuthorization) error {
		polled = append(polled, a.SourceScopeID)
		return nil
	}
	makeScheduler := func() *runtime.Scheduler {
		s, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.router, options)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := makeScheduler()
	for scope, key := range targets {
		if err := s.Subscribe(ctx, runtime.SubscriptionRequest{ID: "fair-sub-" + scope, WaitID: waits[scope], SourceScopeID: domain.ID(scope), SourceKey: sources[scope], Types: []string{"fixture.ready"}, TargetKey: key, AliasType: "fixture.alias"}); err != nil {
			t.Fatal(err)
		}
	}
	polled = nil
	if err := s.ReconcileSubscriptions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(polled) != 1 {
		t.Fatalf("one poll consulted %d policies", len(polled))
	}
	var readyScope string
	for scope := range targets {
		if domain.ID(scope) != polled[0] {
			readyScope = scope
		}
	}
	source := sources[readyScope]
	observed := routingEvent(f, "fair-ready", &source)
	observed.Type = "fixture.ready"
	if _, _, err := d.ingress.Accept(ctx, domain.ID(readyScope), observed); err != nil {
		t.Fatal(err)
	}
	// Recreate the host between every bounded poll: process-local cursors cannot
	// make the later ready subscription advance behind a standing no-op Wait.
	for range 2 {
		s = makeScheduler()
		if err := s.ReconcileSubscriptions(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, err := tx.Wait(waits[readyScope])
		if err != nil {
			return err
		}
		if w.State != domain.WaitWoken {
			return fmt.Errorf("standing subscription starved ready wait: %+v", w)
		}
		return nil
	})
}

func TestProductionResumeCarriesOrderedContextAndRecoveryTrigger(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	work, ref, key := waitForWork(t, f, "resume-context", true)
	d := f.Driver.(*waitDriver)
	if err := d.Wait(ref, domain.Wait{ID: "resume-context-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.ready"}}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.scheduler.Recover(ctx, ref.ExecutionID); err == nil {
		t.Fatal("Recover bypassed an active Wait instead of accepted-event Resume")
	}
	ordinary := routingEvent(f, "context-before-wake", &key)
	ordinary.Type = "fixture.context"
	if err := d.Accept(ordinary); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, d, ordinary.ID)
	winner := routingEvent(f, "winner-after-context", &key)
	winner.Type = "fixture.ready"
	if err := d.Accept(winner); err != nil {
		t.Fatal(err)
	}
	if err := d.Wake("resume-context-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	check := func(request extension.RunRequest) {
		t.Helper()
		var ids []domain.ID
		for _, event := range request.Inputs {
			ids = append(ids, event.ID)
		}
		if !reflect.DeepEqual(ids, []domain.ID{ordinary.ID, winner.ID}) || request.Trigger.ID != winner.ID || request.Execution.TriggerID != "trigger/resume-context" {
			t.Fatalf("resumption lost context/wake identity: inputs%v trigger%s original%s", ids, request.Trigger.ID, request.Execution.TriggerID)
		}
	}
	check(resumed.Request)
	f.Clock.Advance(time.Minute)
	recovered, err := d.scheduler.Recover(ctx, ref.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	check(recovered.Request)
	if recovered.Lease.ExecutionID != ref.ExecutionID || recovered.Lease.Epoch <= resumed.Lease.Epoch {
		t.Fatalf("recovery changed resumed attempt: %+v %+v", resumed.Lease, recovered.Lease)
	}
	if err = d.scheduler.Observe(ctx, recovered); err != nil {
		t.Fatal(err)
	}
}

func TestProductionResumePreservesRemainingExecutionTimeout(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	options := d.options
	options.Timeout = 30 * time.Second
	scheduler, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.router, options)
	if err != nil {
		t.Fatal(err)
	}
	d.scheduler = scheduler
	work := seedSchedulerWork(t, f, "paused-deadline")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "paused-deadline"}
	attempt, err := scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(10 * time.Second)
	registerWait(t, f, attempt.Lease, "deadline-wait-1", key, true, 1)
	f.Clock.Advance(72 * time.Hour)
	requireAccept(t, f, "deadline-wake-1", &key)
	if err = d.Wake("deadline-wait-1"); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	state, err := scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Deadline.Equal(f.Clock.Now().Add(20 * time.Second)) {
		t.Fatalf("parked days consumed/reset timeout: now%s deadline%s", f.Clock.Now(), state.Deadline)
	}
	f.Clock.Advance(5 * time.Second)
	var observed uint64
	routingRead(t, f, func(tx storage.Tx) error { a, err := tx.Event("deadline-wake-1"); observed = a.Sequence; return err })
	registerWait(t, f, resumed.Lease, "deadline-wait-2", key, true, observed)
	f.Clock.Advance(72 * time.Hour)
	f.Driver = f.Restart()
	d = f.Driver.(*waitDriver)
	requireAccept(t, f, "deadline-wake-2", &key)
	if err = d.Wake("deadline-wait-2"); err != nil {
		t.Fatal(err)
	}
	resumed, err = d.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	state, err = d.scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Deadline.Equal(f.Clock.Now().Add(15 * time.Second)) {
		t.Fatalf("restart/repeatedpark restored full timeout: now%s deadline%s", f.Clock.Now(), state.Deadline)
	}
	f.Clock.Advance(14 * time.Second)
	if err = d.scheduler.Timeout(ctx, resumed.Lease); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("timeout before remaining budget: %v", err)
	}
	f.Clock.Advance(time.Second)
	if err = d.scheduler.Timeout(ctx, resumed.Lease); err != nil {
		t.Fatal(err)
	}
}

func TestProductionRegisteredWaitOutcomeAcknowledgementRetainsHistory(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "wait-outcome")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "wait-outcome"}
	attempt, err := d.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	tokens := uint64(17)
	cost := 0.25
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "registered suspension", Data: domain.Payload{Schema: "fixture.wait-result.v1", Data: []byte(`{"suspended":true}`)}}, Usage: extension.Usage{OutputTokens: &tokens, Cost: &cost, Currency: "USD"}, Artifacts: []domain.ArtifactRef{{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 10, MediaType: "text/plain"}}}
	if err = d.scheduler.Complete(ctx, attempt.Lease, result); !errors.Is(err, extension.ErrUnsupported) {
		t.Fatalf("bare waiting outcome bypassed predicate registration: %v", err)
	}
	registerWait(t, f, attempt.Lease, "outcome-wait", key, true, 1)
	// The old authority stays revoked; only the trusted host's observation of the
	// already registered suspension may append result metadata for this epoch.
	attempt.Handle = &schedulerHandle{result: result}
	if err = d.scheduler.Observe(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	state, err := d.scheduler.InspectAttempt(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Result, &result) {
		t.Fatalf("waiting result lost usage/artifacts: %+v", state.Result)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		x, err := tx.Execution(attempt.Lease.ExecutionID)
		if err != nil {
			return err
		}
		w, err := tx.Work(work)
		if err != nil {
			return err
		}
		wait, err := tx.Wait("outcome-wait")
		if err != nil {
			return err
		}
		if x.State != domain.ExecutionParked || x.Lease.Owner != "" || w.State != domain.WorkWaiting || wait.State != domain.WaitActive {
			return fmt.Errorf("metadata acknowledgement changed wait authority: %+v %+v %+v", x, w, wait)
		}
		if !reflect.DeepEqual(x.Artifacts, result.Artifacts) || !reflect.DeepEqual(x.Outcome, &result.Outcome) {
			return fmt.Errorf("waiting result missing backup-visible execution references: %+v", x)
		}
		return nil
	})
	if _, _, err = d.causal.Emit(ctx, attempt.Lease, routingEvent(f, "stale-emission", nil)); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("waiting metadata acknowledgement revived lease: %v", err)
	}
	requireAccept(t, f, "outcome-wake", &key)
	if err = d.Wake("outcome-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	before, err := d.scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	result.Outcome.Summary = "late old epoch"
	if err = d.scheduler.Complete(ctx, attempt.Lease, result); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("old wait result overwrote resumed epoch: %v", err)
	}
	after, err := d.scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused old acknowledgement changed resumed metadata")
	}
}

func TestProductionTerminalWaitResultKeepsBackupVisibleArtifacts(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(false))
	work, ref, key := waitForWork(t, f, "terminal-wait-result", false)
	d := f.Driver.(*waitDriver)
	tokens := uint64(3)
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "terminal suspension", Data: domain.Payload{Schema: "fixture.wait-result.v1", Data: []byte(`{"suspended":true}`)}}, Usage: extension.Usage{InputTokens: &tokens}, Artifacts: []domain.ArtifactRef{{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 11, MediaType: "text/plain"}}}
	wait := domain.Wait{ID: "terminal-result-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.changed"}}}
	if err := d.scheduler.WaitWithResult(ctx, ref, wait, false, result); err != nil {
		t.Fatal(err)
	}
	inspect := func() {
		t.Helper()
		routingRead(t, f, func(tx storage.Tx) error {
			x, err := tx.Execution(ref.ExecutionID)
			if err != nil {
				return err
			}
			w, err := tx.Work(work)
			if err != nil {
				return err
			}
			if x.State != domain.ExecutionWaiting || x.Lease.Owner != "" || w.State != domain.WorkWaiting {
				return fmt.Errorf("wrong terminal waiting lifecycle: %+v %+v", x, w)
			}
			if !reflect.DeepEqual(x.Artifacts, result.Artifacts) || !reflect.DeepEqual(x.Outcome, &result.Outcome) {
				return fmt.Errorf("terminal waiting omitted immutable backup references: %+v", x)
			}
			return nil
		})
	}
	inspect()
	// The same finished outcome/reference set may gain measurements after lease
	// release, while a terminal attempt's immutable backup references cannot move.
	more := result
	output := uint64(9)
	more.Usage.OutputTokens = &output
	if err := d.scheduler.Complete(ctx, ref, more); err != nil {
		t.Fatal(err)
	}
	state, err := d.scheduler.InspectAttempt(ctx, ref.ExecutionID)
	if err != nil || !reflect.DeepEqual(state.Result, &more) {
		t.Fatalf("same terminal result measurement acknowledgement: %+v %v", state.Result, err)
	}
	changed := result
	changed.Artifacts = []domain.ArtifactRef{{ID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Size: 12, MediaType: "text/plain"}}
	if err = d.scheduler.Complete(ctx, ref, changed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("terminal wait references changed after release: %v", err)
	}
	changed = result
	changed.Outcome.Summary = "changed terminal outcome"
	if err = d.scheduler.Complete(ctx, ref, changed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("terminal waiting outcome rewritten: %v", err)
	}
	inspect()
	state, err = d.scheduler.InspectAttempt(ctx, ref.ExecutionID)
	if err != nil || !reflect.DeepEqual(state.Result, &more) {
		t.Fatalf("rejected terminal change altered measurements: %+v %v", state.Result, err)
	}
}
