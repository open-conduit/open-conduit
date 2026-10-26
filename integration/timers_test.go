package integration_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func timerEvent(id string) domain.Event {
	e := causalEvent(id)
	e.Type = "fixture.timer.fired"
	return e
}
func timerRead(t *testing.T, store storage.Store, now time.Time, id domain.ID) domain.Timer {
	t.Helper()
	var timer domain.Timer
	if err := store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error { var e error; timer, e = tx.Timer(id); return e }); err != nil {
		t.Fatal(err)
	}
	return timer
}
func newTimerDriver(t *testing.T, store storage.Store, now func() time.Time, limits runtime.CausalLimits) *runtime.Timers {
	t.Helper()
	driver, err := runtime.NewTimers(store, now, runtime.TimerOptions{CausalLimits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}
func TestDurableTimerRecurrenceReplayAndRecreation(t *testing.T) {
	ctx := context.Background()
	db, _, clock, path := causalFixture(t, runtime.CausalLimits{})
	timers := newTimerDriver(t, db, clock.Now, runtime.CausalLimits{})
	initial := domain.Timer{ID: "recurring", FireAt: clock.Now(), Event: timerEvent("tick-1")}
	if err := timers.ArmRecurring(ctx, "scope_a", initial, 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	original := timerRead(t, db, clock.Now(), initial.ID)
	first, err := timers.Fire(ctx, original.ID, original.Revision, nil)
	if err != nil || !first.Fired {
		t.Fatalf("first fire: %+v %v", first, err)
	}
	next := timerRead(t, db, clock.Now(), initial.ID)
	if next.Revision <= original.Revision || !next.FireAt.Equal(initial.FireAt.Add(time.Minute)) || next.Event.ID == initial.Event.ID {
		t.Fatalf("lost recurrence: %+v", next)
	}
	replay, err := timers.Fire(ctx, original.ID, original.Revision, nil)
	if err != nil || replay.Event.Event.ID != first.Event.Event.ID {
		t.Fatalf("old revision replay: %+v %v", replay, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	timers = newTimerDriver(t, db, clock.Now, runtime.CausalLimits{})
	replay, err = timers.Fire(ctx, original.ID, original.Revision, nil)
	if err != nil || replay.Event.Sequence != first.Event.Sequence {
		t.Fatalf("restart replay: %+v %v", replay, err)
	}
	if err = timers.Cancel(ctx, next.ID, next.Revision); err != nil {
		t.Fatal(err)
	}
	recreated := domain.Timer{ID: initial.ID, FireAt: clock.Now(), Event: timerEvent("new-generation")}
	if err = timers.Arm(ctx, "scope_a", recreated, 0); err != nil {
		t.Fatal(err)
	}
	current := timerRead(t, db, clock.Now(), recreated.ID)
	if current.Revision <= next.Revision {
		t.Fatal("recreated timer reused revision")
	}
	if _, err = timers.Fire(ctx, original.ID, original.Revision, nil); err != nil {
		t.Fatal(err)
	}
	if unchanged := timerRead(t, db, clock.Now(), current.ID); unchanged.Revision != current.Revision {
		t.Fatal("stale fire changed recreated timer")
	}
	if got := len(ingressEvents(t, db)); got != 2 {
		t.Fatalf("replays accepted %d events, want root+tick", got)
	}
}
func TestDurableTimerBudgetRefusalDisablesRecurrence(t *testing.T) {
	for _, kind := range []string{"rate", "depth"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			limits := runtime.CausalLimits{MaxDepth: 1, PerMinute: 1}
			db, causal, clock, _ := causalFixture(t, limits)
			if _, _, err := causal.Derive(ctx, "scope_a", "root", causalEvent("spent")); err != nil {
				t.Fatal(err)
			}
			event := timerEvent("refused-tick")
			event.CausationID = "root"
			if kind == "depth" {
				clock.Set(clock.Now().Add(time.Minute))
				event.CausationID = "spent"
			}
			timers := newTimerDriver(t, db, clock.Now, limits)
			timer := domain.Timer{ID: "loop", FireAt: clock.Now(), Event: event}
			if err := timers.ArmRecurring(ctx, "scope_a", timer, 0, time.Minute); err != nil {
				t.Fatal(err)
			}
			timer = timerRead(t, db, clock.Now(), timer.ID)
			fired, err := timers.Fire(ctx, timer.ID, timer.Revision, nil)
			if err != nil || !fired.Rejected || fired.Fired {
				t.Fatalf("refusal: %+v %v", fired, err)
			}
			if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
				if _, e := tx.Timer(timer.ID); !errors.Is(e, storage.ErrNotFound) {
					return fmt.Errorf("refused timer still armed: %v", e)
				}
				if _, e := tx.Event(event.ID); !errors.Is(e, storage.ErrNotFound) {
					return fmt.Errorf("refused event accepted: %v", e)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			again, err := timers.Fire(ctx, timer.ID, timer.Revision, nil)
			if err != nil || !again.Rejected {
				t.Fatalf("refusal replay: %+v %v", again, err)
			}
			if due, err := timers.Due(ctx, 1); err != nil || len(due) != 0 {
				t.Fatalf("rejected recurrence busy loop: %+v %v", due, err)
			}
			audits, err := causal.Rejections(ctx)
			if err != nil || len(audits) != 1 {
				t.Fatalf("refusal audit: %+v %v", audits, err)
			}
		})
	}
}
func TestDurableTimerCompetingHandlesAndFairPoll(t *testing.T) {
	ctx := context.Background()
	db, _, clock, path := causalFixture(t, runtime.CausalLimits{})
	timers := newTimerDriver(t, db, clock.Now, runtime.CausalLimits{})
	timer := domain.Timer{ID: "a", FireAt: clock.Now().Add(-time.Hour), Event: timerEvent("a-first")}
	if err := timers.ArmRecurring(ctx, "scope_a", timer, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	second, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	other := newTimerDriver(t, second, clock.Now, runtime.CausalLimits{})
	original := timerRead(t, db, clock.Now(), timer.ID)
	gate := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, driver := range []*runtime.Timers{timers, other} {
		wg.Add(1)
		go func(driver *runtime.Timers) {
			defer wg.Done()
			<-gate
			_, e := driver.Fire(ctx, original.ID, original.Revision, nil)
			results <- e
		}(driver)
	}
	close(gate)
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	if got := len(ingressEvents(t, db)); got != 2 {
		t.Fatalf("duplicate concurrent firing: %d", got)
	}
	if err = timers.Arm(ctx, "scope_a", domain.Timer{ID: "b", FireAt: clock.Now(), Event: timerEvent("b")}, 0); err != nil {
		t.Fatal(err)
	}
	first, err := timers.Due(ctx, 1)
	if err != nil || len(first) != 1 || first[0].TimerID != "a" {
		t.Fatalf("poll1: %+v %v", first, err)
	}
	secondPoll, err := other.Due(ctx, 1)
	if err != nil || len(secondPoll) != 1 || secondPoll[0].TimerID != "b" {
		t.Fatalf("fixedclock recurrence starved independent timer: %+v %v", secondPoll, err)
	}
}
func TestDurableTimerInvalidInputDoesNotStarveIndependentTimer(t *testing.T) {
	ctx := context.Background()
	db, _, clock, _ := causalFixture(t, runtime.CausalLimits{})
	timers := newTimerDriver(t, db, clock.Now, runtime.CausalLimits{})
	err := db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		return tx.PutTimer(domain.Timer{ID: "a-unbound", FireAt: clock.Now(), Event: timerEvent("unbound")}, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = timers.Arm(ctx, "scope_a", domain.Timer{ID: "b-valid", FireAt: clock.Now(), Event: timerEvent("valid")}, 0); err != nil {
		t.Fatal(err)
	}
	first, err := timers.Due(ctx, 1)
	if err != nil || len(first) != 1 || first[0].Diagnostic != "invalid_timer" {
		t.Fatalf("invalid isolation: %+v %v", first, err)
	}
	second, err := timers.Due(ctx, 1)
	if err != nil || len(second) != 1 || !second[0].Fired {
		t.Fatalf("invalid timer starved valid timer: %+v %v", second, err)
	}
}

type timerCrashStore struct {
	storage.Store
	phase string
}
type timerCrashTx struct {
	storage.Tx
	phase string
}

func (t timerCrashTx) FireTimer(id domain.ID, revision domain.Revision, next *domain.Timer) error {
	if t.phase == "between" {
		fmt.Println("staged")
		select {}
	}
	err := t.Tx.FireTimer(id, revision, next)
	if err == nil && t.phase == "staged" {
		fmt.Println("staged")
		select {}
	}
	return err
}
func (s timerCrashStore) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	err := s.Store.Transact(ctx, g, func(tx storage.Tx) error { return fn(timerCrashTx{tx, s.phase}) })
	if err == nil && s.phase == "committed" {
		fmt.Println("staged")
		select {}
	}
	return err
}
func TestDurableTimerCrashHelper(t *testing.T) {
	if os.Getenv("CONDUIT_TIMER_HELPER") == "" {
		return
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, os.Getenv("CONDUIT_TIMER_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := timerCrashStore{db, os.Getenv("CONDUIT_TIMER_PHASE")}
	timers := newTimerDriver(t, store, func() time.Time { return ingressTime }, runtime.CausalLimits{})
	if _, err = timers.Fire(ctx, "crash-recurrence", 1, nil); err != nil {
		t.Fatal(err)
	}
}
func TestDurableTimerCrashAtomicFireAndRearm(t *testing.T) {
	for _, phase := range []string{"between", "staged", "committed"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "timer.db")
			clock := &ingressClock{at: ingressTime}
			db, _ := openIngress(t, path, clock)
			timers := newTimerDriver(t, db, clock.Now, runtime.CausalLimits{})
			if err := timers.ArmRecurring(ctx, "scope_a", domain.Timer{ID: "crash-recurrence", FireAt: clock.Now(), Event: timerEvent("crash-event")}, 0, time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDurableTimerCrashHelper$")
			cmd.Env = append(os.Environ(), "CONDUIT_TIMER_HELPER=1", "CONDUIT_TIMER_DB="+path, "CONDUIT_TIMER_PHASE="+phase)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			ready := make(chan bool, 1)
			go func() { ready <- bufio.NewScanner(stdout).Scan() }()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("helper exited before boundary")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("helper never reached boundary")
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			db, err = sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			timers = newTimerDriver(t, db, clock.Now, runtime.CausalLimits{})
			before := timerRead(t, db, clock.Now(), "crash-recurrence")
			wantRev := domain.Revision(1)
			wantEvents := 0
			if phase == "committed" {
				wantRev = 2
				wantEvents = 1
			}
			if before.Revision != wantRev || len(ingressEvents(t, db)) != wantEvents {
				t.Fatalf("partial firing after %s: revision%d events%d", phase, before.Revision, len(ingressEvents(t, db)))
			}
			fired, err := timers.Fire(ctx, "crash-recurrence", 1, nil)
			if err != nil || !fired.Fired {
				t.Fatalf("recovery fire: %+v %v", fired, err)
			}
			after := timerRead(t, db, clock.Now(), "crash-recurrence")
			if after.Revision != 2 || len(ingressEvents(t, db)) != 1 {
				t.Fatalf("recovery lost/repeated recurrence: rev%d events%d", after.Revision, len(ingressEvents(t, db)))
			}
		})
	}
}

func TestDurableTimerRetryAcceptanceSurvivesCoordinatorRestart(t *testing.T) {
	ctx := context.Background()
	f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "retry-poll")
	scheduler := f.Driver.(*schedulerDriver).scheduler
	attempt, err := scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err = scheduler.Fail(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed}}); err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(5 * time.Minute)
	timers := newTimerDriver(t, f.Store, f.Clock.Now, runtime.CausalLimits{})
	retry := timerRead(t, f.Store, f.Clock.Now(), domain.ID("retry/"+string(work)))
	fired, err := timers.Fire(ctx, retry.ID, retry.Revision, nil)
	if err != nil || !fired.Fired {
		t.Fatalf("retry fire: %+v %v", fired, err)
	}
	// Simulate a lost coordinator after durable acceptance and before eligibility.
	scheduler = f.Restart().(*schedulerDriver).scheduler
	if _, err = scheduler.Poll(ctx, timers, 1); err != nil {
		t.Fatal(err)
	}
	next, err := scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if next.Request.Execution.Attempt != 2 || next.Request.Trigger.ID != fired.Event.Event.ID {
		t.Fatalf("retry bypassed accepted event: %+v", next.Request.Execution)
	}
}
func TestDurableTimerPollRecoversExpiredLeaseWithoutReplacement(t *testing.T) {
	ctx := context.Background()
	f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "lease-poll")
	scheduler := f.Driver.(*schedulerDriver).scheduler
	original, err := scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(time.Minute)
	scheduler = f.Restart().(*schedulerDriver).scheduler
	polled, err := scheduler.Poll(ctx, newTimerDriver(t, f.Store, f.Clock.Now, runtime.CausalLimits{}), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(polled.Recovered) != 1 || polled.Recovered[0].Lease.ExecutionID != original.Lease.ExecutionID || polled.Recovered[0].Lease.Epoch <= original.Lease.Epoch {
		t.Fatalf("poll replaced or lost expired attempt: %+v", polled)
	}
	if err = scheduler.Complete(ctx, original.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("old poll lease retained authority: %v", err)
	}
	if err = scheduler.Observe(ctx, polled.Recovered[0]); err != nil {
		t.Fatal(err)
	}
}
func TestDurableTimerExecutionTimeoutUsesAcceptedEvent(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprint("refused_", refuse), func(t *testing.T) {
			ctx := context.Background()
			f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
			work := seedSchedulerWork(t, f, "deadline-poll")
			options := schedulerDefaultOptions(f, executor)
			options.Timeout = 30 * time.Second
			scheduler := newTestScheduler(t, f, options)
			attempt, err := scheduler.Start(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			limits := runtime.CausalLimits{PerMinute: 1}
			if refuse {
				causal, err := runtime.NewCausal(f.Store, f.Clock.Now, limits)
				if err != nil {
					t.Fatal(err)
				}
				event := timerEvent("budget-spent")
				event.WorkID = work
				if _, _, err = causal.Derive(ctx, "routing-scope", attempt.Request.Trigger.ID, event); err != nil {
					t.Fatal(err)
				}
			}
			f.Clock.Advance(30 * time.Second)
			polled, err := scheduler.Poll(ctx, newTimerDriver(t, f.Store, f.Clock.Now, limits), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(polled.Timers) != 1 || polled.Timers[0].Rejected != refuse {
				t.Fatalf("deadline fire: %+v", polled)
			}
			routingRead(t, f, func(tx storage.Tx) error {
				x, e := tx.Execution(attempt.Lease.ExecutionID)
				if e != nil {
					return e
				}
				if x.State != domain.ExecutionFailed || x.Lease.Owner != "" {
					return fmt.Errorf("deadline didn't fence attempt: %+v", x)
				}
				if !refuse {
					event, e := tx.Event(polled.Timers[0].Event.Event.ID)
					if e != nil {
						return e
					}
					if !event.Routed || event.Disposition != "execution_timeout" || event.Event.WorkID != work || event.Event.ExecutionID != x.ID {
						return fmt.Errorf("missing timeout control audit: %+v", event)
					}
				}
				return nil
			})
			state, err := scheduler.InspectAttempt(ctx, attempt.Lease.ExecutionID)
			if err != nil || state.Excluded {
				t.Fatalf("deadline claimed process stopped: %+v %v", state, err)
			}
			retry, err := scheduler.RetryStatus(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			if refuse && (retry.BlockedReason != "causal_rejection" || !retry.Blocked || retry.TimerID != "" || retry.Exhausted) {
				t.Fatalf("causal refusal fabricated automaticretry/exhaustion: %+v", retry)
			}
			if err = scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, storage.ErrLeaseLost) {
				t.Fatalf("deadline stale outcome: %v", err)
			}
		})
	}
}
func TestDurableTimerDerivedRecurrenceKeepsArmingParent(t *testing.T) {
	ctx := context.Background()
	f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "derived-timer")
	scheduler := f.Driver.(*schedulerDriver).scheduler
	attempt, err := scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	timers := newTimerDriver(t, f.Store, f.Clock.Now, runtime.CausalLimits{})
	timer := domain.Timer{ID: "derived-recurrence", FireAt: f.Clock.Now().Add(30 * time.Second), Event: timerEvent("derived-first")}
	if err = timers.ArmDerivedRecurring(ctx, attempt.Lease, timer, 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	timer = timerRead(t, f.Store, f.Clock.Now(), timer.ID)
	if timer.Event.CausationID != attempt.Request.Trigger.ID || timer.Event.WorkID != work {
		t.Fatalf("untrusted arming ancestry: %+v", timer)
	}
	f.Clock.Advance(30 * time.Second)
	if _, err = timers.Fire(ctx, timer.ID, timer.Revision, nil); err != nil {
		t.Fatal(err)
	}
	next := timerRead(t, f.Store, f.Clock.Now(), timer.ID)
	if next.Event.CausationID != attempt.Request.Trigger.ID {
		t.Fatal("recurrence severed causal root")
	}
	forged := next
	forged.FireAt = next.FireAt.Add(time.Minute)
	forged.Event.ID = "forged-next"
	forged.Event.DeliveryID = "forged-next"
	forged.Event.CausationID = "unrelated-root"
	f.Clock.Advance(time.Minute)
	if _, err = timers.Fire(ctx, next.ID, next.Revision, &forged); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("recurrence changed root: %v", err)
	}
}
