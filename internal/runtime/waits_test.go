package runtime_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

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
	d.ExecutorWithResume.Mu.Lock()
	requests := append([]extension.RunRequest(nil), d.ExecutorWithResume.ResumeRequests...)
	d.ExecutorWithResume.Mu.Unlock()
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
		accepted, _, err := d.Causal.Derive(context.Background(), "routing-scope", parent, wake)
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
		state, err := d.Scheduler.InspectAttempt(context.Background(), ref.ExecutionID)
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
			a, _, err := d.Causal.Emit(context.Background(), ref, emitted)
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
	if _, _, err := d.Causal.Emit(context.Background(), ref, routingEvent(f, "too-deep-after-resume", nil)); !errors.Is(err, runtime.ErrCausalRejected) {
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
	if err := d.Scheduler.Subscribe(ctx, request); err != nil {
		t.Fatal(err)
	}
	f.Driver = f.Restart()
	d = f.Driver.(*waitDriver)
	for range 2 {
		if err := d.Scheduler.ReconcileSubscriptions(ctx, 10); err != nil {
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
	if err := d.Scheduler.Subscribe(ctx, runtime.SubscriptionRequest{ID: "cancel-sub", WaitID: w.ID, SourceScopeID: "routing-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias"}); err != nil {
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
	if err := d.Scheduler.ReconcileSubscriptions(ctx, 10); err != nil {
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
	if _, _, err := d.Ingress.Accept(ctx, "source-scope", observed); err != nil {
		t.Fatal(err)
	}
	w := domain.Wait{ID: "cross-scope-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{target}, Types: []string{"fixture.alias"}}}
	if err := d.Wait(ref, w, false); err != nil {
		t.Fatal(err)
	}
	request := runtime.SubscriptionRequest{ID: "cross-scope-sub", WaitID: w.ID, SourceScopeID: "source-scope", SourceKey: source, Types: []string{"fixture.ready"}, TargetKey: target, AliasType: "fixture.alias"}
	if err := d.Scheduler.Subscribe(ctx, request); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("cross-scope alias without policy: %v", err)
	}
	options := d.Options
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
	authorized, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
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
	path := f.Store.(*routingReopening).Path
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
	inputs, err := d.Router.PendingInputs(context.Background(), work)
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

// A wake is charged to the Event that woke it, not to the attempt's previous
// wake (#185): one attempt parks and wakes on independent external roots far
// more often than causal_max_depth, and every wake goes through.
func TestProductionIndependentWakesDoNotDeepenTheChain(t *testing.T) {
	o := waitOptions(true)
	o.MaxDepth = 2
	o.PerMinute = 100
	f := waitFixture(t, "reopen", o)
	work, ref, key := waitForWork(t, f, "external-loop", true)
	for n := 1; n <= 3*o.MaxDepth+1; n++ {
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
		if err != nil {
			t.Fatalf("wake %d on an independent root refused: %v", n, err)
		}
		if next.ExecutionID != ref.ExecutionID || next.Epoch <= ref.Epoch {
			t.Fatalf("resume authority changed attempt: %+v %+v", ref, next)
		}
		ref = next
		f.Driver = f.Restart()
	}
	audits, err := f.Driver.(*waitDriver).Causal.Rejections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 0 {
		t.Fatalf("independent wakes were refused: %+v", audits)
	}
}

// waitLoopFixture parks an attempt on its own key and wakes it with Events
// derived from its own chain (an execution → event → wake loop with no
// outside input), until the causal limits refuse a wake. trace, if given,
// runs once each wake's winner is accepted and before it wakes. It returns
// the fixture, the Work, the refused wait and its round.
func waitLoopFixture(t *testing.T, name string, trace func(f conformance.RuntimeFixture, key domain.CorrelationKey, n int)) (conformance.RuntimeFixture, domain.ID, domain.ID, int) {
	t.Helper()
	o := waitOptions(true)
	o.MaxDepth = 3
	o.PerMinute = 100
	f := waitFixture(t, "persistent", o)
	work, ref, key := waitForWork(t, f, name, true)
	for n := 1; ; n++ {
		var boundary uint64
		routingRead(t, f, func(tx storage.Tx) error {
			all, err := tx.Events(0, 0)
			if err == nil {
				boundary = all[len(all)-1].Sequence
			}
			return err
		})
		id := domain.ID(fmt.Sprintf("%s-wait-%d", name, n))
		registerWait(t, f, ref, id, key, true, boundary)
		d := f.Driver.(*waitDriver)
		state, err := d.Scheduler.InspectAttempt(context.Background(), ref.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		parent := state.CausalTriggerID
		if parent == "" {
			parent = domain.ID("trigger/" + name)
		}
		wake := routingEvent(f, fmt.Sprintf("%s-wake-%d", name, n), &key)
		wake.WorkID = work
		if _, _, err = d.Causal.Derive(context.Background(), "routing-scope", parent, wake); err != nil {
			t.Fatal(err)
		}
		if trace != nil {
			trace(f, key, n)
		}
		if err = d.Wake(id); err != nil {
			t.Fatal(err)
		}
		next, err := d.Start(work)
		if err != nil {
			if n == 1 {
				t.Fatalf("the first wake was refused: %v", err)
			}
			return f, work, id, n
		}
		ref = next
		if n > o.MaxDepth {
			t.Fatal("a wake loop on the attempt's own output was never refused")
		}
	}
}

// A wake the attempt's own output caused still deepens its chain, so a loop
// with no outside input stops at the depth limit. The refused wake does not
// leave the Work quietly waiting: it is held with the reason, and releasing
// the hold wakes the wait on what had arrived.
func TestProductionRefusedWakeHoldsTheWork(t *testing.T) {
	f, work, wait, _ := waitLoopFixture(t, "self-loop", nil)
	d := f.Driver.(*waitDriver)
	audits, err := d.Causal.Rejections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, audit := range audits {
		if audit.RootID == "trigger/self-loop" && audit.Reason == "depth_limit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing depth_limit refusal on the original root: %+v", audits)
	}
	var hold runtime.Hold
	var w domain.Work
	routingRead(t, f, func(tx storage.Tx) error {
		var held bool
		var err error
		if hold, held, err = runtime.HoldInTx(tx, work); err == nil && !held {
			return errors.New("refused wake left the Work unheld")
		}
		if err == nil {
			w, err = tx.Work(work)
		}
		return err
	})
	if !strings.Contains(hold.Reason, "depth_limit") || !strings.Contains(hold.Reason, "release") {
		t.Fatalf("hold reason does not say why or what to do: %q", hold.Reason)
	}
	if hold.EventID == "" {
		t.Fatal("hold has no audit Event")
	}
	control, err := runtime.NewControl(f.Store, f.Clock.Now, d.Router, d.Scheduler, d.Options.CausalLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.ReleaseWork(context.Background(), work, w.Revision, "let it go on", "release-self-loop"); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		got, err := tx.Wait(wait)
		if err == nil && got.State != domain.WaitWoken {
			return fmt.Errorf("release did not wake the blocked wait: %+v", got)
		}
		return err
	})
	if _, err = d.Start(work); err != nil {
		t.Fatalf("released Work did not resume: %v", err)
	}
}

// The trace reports a refused wake without writing. An earlier accepted
// winner from the attempt's own loop that the limits would refuse keeps the
// traced Event as context; until then the earlier winner wakes the Work.
func TestProductionTraceReportsARefusedWake(t *testing.T) {
	predicted := 0
	_, _, _, refused := waitLoopFixture(t, "traced-loop", func(f conformance.RuntimeFixture, key domain.CorrelationKey, n int) {
		tr, err := f.Driver.(*waitDriver).Router.Trace(context.Background(), "routing-scope", routingEvent(f, fmt.Sprintf("probe-%d", n), &key), nil)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case tr.Effect.Kind == runtime.EffectQueue && len(tr.Also) == 0 && strings.Contains(tr.Effect.Detail, "causal limits would refuse its wake (depth_limit)"):
			predicted = n
		case tr.Effect.Kind == runtime.EffectQueue && len(tr.Also) == 1:
		default:
			t.Fatalf("round %d: %+v", n, tr)
		}
	})
	if predicted != refused {
		t.Fatalf("trace predicted a refusal in round %d, the wake was refused in round %d", predicted, refused)
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
	options := d.Options
	var polled []domain.ID
	options.AuthorizeAlias = func(_ context.Context, a runtime.AliasAuthorization) error {
		polled = append(polled, a.SourceScopeID)
		return nil
	}
	makeScheduler := func() *runtime.Scheduler {
		s, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
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
	if _, _, err := d.Ingress.Accept(ctx, domain.ID(readyScope), observed); err != nil {
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
	if _, err := d.Scheduler.Recover(ctx, ref.ExecutionID); err == nil {
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
	resumed, err := d.Scheduler.Start(ctx, work)
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
	recovered, err := d.Scheduler.Recover(ctx, ref.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	check(recovered.Request)
	if recovered.Lease.ExecutionID != ref.ExecutionID || recovered.Lease.Epoch <= resumed.Lease.Epoch {
		t.Fatalf("recovery changed resumed attempt: %+v %+v", resumed.Lease, recovered.Lease)
	}
	if err = d.Scheduler.Observe(ctx, recovered); err != nil {
		t.Fatal(err)
	}
}

func TestProductionResumePreservesRemainingExecutionTimeout(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	options := d.Options
	options.Timeout = 30 * time.Second
	scheduler, err := runtime.NewScheduler(f.Store, f.Clock.Now, d.Router, options)
	if err != nil {
		t.Fatal(err)
	}
	d.Scheduler = scheduler
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
	resumed, err = d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	state, err = d.Scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Deadline.Equal(f.Clock.Now().Add(15 * time.Second)) {
		t.Fatalf("restart/repeatedpark restored full timeout: now%s deadline%s", f.Clock.Now(), state.Deadline)
	}
	f.Clock.Advance(14 * time.Second)
	if err = d.Scheduler.Timeout(ctx, resumed.Lease); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("timeout before remaining budget: %v", err)
	}
	f.Clock.Advance(time.Second)
	if err = d.Scheduler.Timeout(ctx, resumed.Lease); err != nil {
		t.Fatal(err)
	}
}

func TestProductionRegisteredWaitOutcomeAcknowledgementRetainsHistory(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "wait-outcome")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "wait-outcome"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	tokens := uint64(17)
	cost := 0.25
	result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "registered suspension", Data: domain.Payload{Schema: "fixture.wait-result.v1", Data: []byte(`{"suspended":true}`)}}, Usage: extension.Usage{OutputTokens: &tokens, Cost: &cost, Currency: "USD"}, Artifacts: []domain.ArtifactRef{{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 10, MediaType: "text/plain"}}}
	if err = d.Scheduler.Complete(ctx, attempt.Lease, result); !errors.Is(err, extension.ErrUnsupported) {
		t.Fatalf("bare waiting outcome bypassed predicate registration: %v", err)
	}
	registerWait(t, f, attempt.Lease, "outcome-wait", key, true, 1)
	// The old authority stays revoked; only the trusted host's observation of the
	// already registered suspension may append result metadata for this epoch.
	attempt.Handle = &schedulerHandle{Result: result}
	if err = d.Scheduler.Observe(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	state, err := d.Scheduler.InspectAttempt(ctx, attempt.Lease.ExecutionID)
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
	if _, _, err = d.Causal.Emit(ctx, attempt.Lease, routingEvent(f, "stale-emission", nil)); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("waiting metadata acknowledgement revived lease: %v", err)
	}
	requireAccept(t, f, "outcome-wake", &key)
	if err = d.Wake("outcome-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	before, err := d.Scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	result.Outcome.Summary = "late old epoch"
	if err = d.Scheduler.Complete(ctx, attempt.Lease, result); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("old wait result overwrote resumed epoch: %v", err)
	}
	after, err := d.Scheduler.InspectAttempt(ctx, resumed.Lease.ExecutionID)
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
	if err := d.Scheduler.WaitWithResult(ctx, ref, wait, false, result); err != nil {
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
	if err := d.Scheduler.Complete(ctx, ref, more); err != nil {
		t.Fatal(err)
	}
	state, err := d.Scheduler.InspectAttempt(ctx, ref.ExecutionID)
	if err != nil || !reflect.DeepEqual(state.Result, &more) {
		t.Fatalf("same terminal result measurement acknowledgement: %+v %v", state.Result, err)
	}
	changed := result
	changed.Artifacts = []domain.ArtifactRef{{ID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Size: 12, MediaType: "text/plain"}}
	if err = d.Scheduler.Complete(ctx, ref, changed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("terminal wait references changed after release: %v", err)
	}
	changed = result
	changed.Outcome.Summary = "changed terminal outcome"
	if err = d.Scheduler.Complete(ctx, ref, changed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("terminal waiting outcome rewritten: %v", err)
	}
	inspect()
	state, err = d.Scheduler.InspectAttempt(ctx, ref.ExecutionID)
	if err != nil || !reflect.DeepEqual(state.Result, &more) {
		t.Fatalf("rejected terminal change altered measurements: %+v %v", state.Result, err)
	}
}

// A parked-then-resumed attempt reports two active segments, the second
// woken by the wake event, whose sum is its active time; the parked days
// count in neither.
func TestProductionResumedRunReportsActiveSegments(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "segments")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "segments"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	started := f.Clock.Now()
	initial, err := d.Scheduler.InspectAttempt(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	budget := initial.Deadline.Sub(started)
	f.Clock.Advance(10 * time.Second)
	registerWait(t, f, attempt.Lease, "segments-wait", key, true, 1)
	run := func() runtime.Run {
		var r runtime.Run
		routingRead(t, f, func(tx storage.Tx) error {
			var e error
			r, e = runtime.RunInTx(tx, attempt.Lease.ExecutionID)
			return e
		})
		return r
	}
	if r := run(); len(r.Segments) != 1 || !r.Segments[0].Parked || r.ActiveTime != 10*time.Second || r.ActiveTime+r.RemainingTimeout != budget {
		t.Fatalf("parked run: %+v active %s remaining %s of %s", r.Segments, r.ActiveTime, r.RemainingTimeout, budget)
	}
	f.Clock.Advance(72 * time.Hour)
	requireAccept(t, f, "segments-wake", &key)
	if err = d.Wake("segments-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Lease.ExecutionID != attempt.Lease.ExecutionID {
		t.Fatalf("resume started a new attempt: %s", resumed.Lease.ExecutionID)
	}
	woken := f.Clock.Now()
	if r := run(); len(r.Segments) != 2 || r.Segments[1].EndedAt != nil || r.ActiveTime != 10*time.Second {
		t.Fatalf("running resumed run: %+v active %s", r.Segments, r.ActiveTime)
	}
	f.Clock.Advance(5 * time.Second)
	if err = d.Scheduler.Complete(ctx, resumed.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	r := run()
	if len(r.Segments) != 2 {
		t.Fatalf("segments: %+v", r.Segments)
	}
	first, second := r.Segments[0], r.Segments[1]
	if !first.StartedAt.Equal(started) || first.EndedAt == nil || !first.EndedAt.Equal(started.Add(10*time.Second)) || !first.Parked || first.WakeEventID != "" {
		t.Fatalf("first segment: %+v", first)
	}
	if !second.StartedAt.Equal(woken) || second.EndedAt == nil || !second.EndedAt.Equal(woken.Add(5*time.Second)) || second.Parked || second.WakeEventID == "" || second.WakeEventID != r.WakeEventID {
		t.Fatalf("second segment: %+v (run wake %s)", second, r.WakeEventID)
	}
	if sum := first.EndedAt.Sub(first.StartedAt) + second.EndedAt.Sub(second.StartedAt); r.ActiveTime != sum || sum != 15*time.Second {
		t.Fatalf("active %s, segments sum %s", r.ActiveTime, sum)
	}
}

// A parked-then-resumed attempt's usage is the sum of its segments':
// the resumed segment's result adds to the parked one's, never replaces it.
func TestProductionResumedRunSumsSegmentUsage(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "segment-usage")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "segment-usage"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	usage := func(in, out uint64, cost float64) extension.Usage {
		return extension.Usage{InputTokens: &in, OutputTokens: &out, Cost: &cost, Currency: "USD"}
	}
	wait := domain.Wait{ID: "segment-usage-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.changed"}}}
	parked := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "parked"}, Usage: usage(100, 865, 0.25)}
	if err = d.Scheduler.WaitWithResult(ctx, attempt.Lease, wait, true, parked); err != nil {
		t.Fatal(err)
	}
	run := func() runtime.Run {
		var r runtime.Run
		routingRead(t, f, func(tx storage.Tx) error {
			var e error
			r, e = runtime.RunInTx(tx, attempt.Lease.ExecutionID)
			return e
		})
		return r
	}
	check := func(when string, in, out uint64, cost float64) {
		t.Helper()
		u := run().Usage
		if u == nil || u.InputTokens == nil || *u.InputTokens != in || u.OutputTokens == nil || *u.OutputTokens != out || u.Cost == nil || *u.Cost != cost || u.Currency != "USD" {
			t.Fatalf("%s usage: %+v", when, u)
		}
	}
	check("parked", 100, 865, 0.25)
	requireAccept(t, f, "segment-usage-wake", &key)
	if err = d.Wake("segment-usage-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Lease.ExecutionID != attempt.Lease.ExecutionID {
		t.Fatalf("resume started a new attempt: %s", resumed.Lease.ExecutionID)
	}
	check("resumed", 100, 865, 0.25)
	if err = d.Scheduler.Complete(ctx, resumed.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: usage(20, 499, 0.125)}); err != nil {
		t.Fatal(err)
	}
	check("finished", 120, 1364, 0.375)
}

// A recovered attempt's segments count lease-held time as its deadline
// budget does: recovery keeps the deadline, so the lost-lease gap stays in
// the interrupted segment, and a recovered segment starts with no gap.
func TestProductionRecoveredRunSegmentsMatchTheDeadlineBudget(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "segments-recover")
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	started := f.Clock.Now()
	before, err := d.Scheduler.InspectAttempt(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(d.Options.LeaseDuration + 20*time.Second)
	recovered, err := d.Scheduler.Recover(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	recoveredAt := f.Clock.Now()
	after, err := d.Scheduler.InspectAttempt(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Deadline.Equal(before.Deadline) {
		t.Fatalf("recovery moved the deadline: %s -> %s", before.Deadline, after.Deadline)
	}
	f.Clock.Advance(5 * time.Second)
	if err = d.Scheduler.Complete(ctx, recovered.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	var r runtime.Run
	routingRead(t, f, func(tx storage.Tx) error {
		var e error
		r, e = runtime.RunInTx(tx, attempt.Lease.ExecutionID)
		return e
	})
	if len(r.Segments) != 2 {
		t.Fatalf("segments: %+v", r.Segments)
	}
	first, second := r.Segments[0], r.Segments[1]
	if !first.StartedAt.Equal(started) || first.EndedAt == nil || !first.EndedAt.Equal(recoveredAt) || first.Parked || first.Recovered {
		t.Fatalf("first segment should run to the recovery at %s: %+v", recoveredAt, first)
	}
	if !second.Recovered || !second.StartedAt.Equal(recoveredAt) || second.EndedAt == nil || !second.EndedAt.Equal(recoveredAt.Add(5*time.Second)) {
		t.Fatalf("recovered segment: %+v", second)
	}
	// The budget charged from the start to the finish, gap included; so do
	// the segments.
	if want := f.Clock.Now().Sub(started); r.ActiveTime != want {
		t.Fatalf("active %s, budget charged %s", r.ActiveTime, want)
	}
}

// An attempt parked before segments were recorded stays without them after
// it resumes, rather than reporting a partial resume-to-finish segment.
func TestProductionPreUpgradeParkedRunStaysSegmentless(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "segments-upgrade")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "segments-upgrade"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(10 * time.Second)
	registerWait(t, f, attempt.Lease, "segments-upgrade-wait", key, true, 1)
	// The attempt state as an older binary wrote it: no segments.
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", len(attempt.Lease.ExecutionID), attempt.Lease.ExecutionID)))
	stateKey := hex.EncodeToString(h[:])
	err = f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		b, e := tx.Get("core.scheduler.attempts", stateKey)
		if e != nil {
			return e
		}
		var state map[string]json.RawMessage
		if e = json.Unmarshal(b, &state); e != nil {
			return e
		}
		if _, ok := state["Segments"]; !ok {
			return fmt.Errorf("attempt state has no Segments: %s", b)
		}
		delete(state, "Segments")
		b, _ = json.Marshal(state)
		return tx.Put("core.scheduler.attempts", stateKey, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(time.Hour)
	requireAccept(t, f, "segments-upgrade-wake", &key)
	if err = d.Wake("segments-upgrade-wait"); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(5 * time.Second)
	if err = d.Scheduler.Complete(ctx, resumed.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	var r runtime.Run
	routingRead(t, f, func(tx storage.Tx) error {
		var e error
		r, e = runtime.RunInTx(tx, attempt.Lease.ExecutionID)
		return e
	})
	if len(r.Segments) != 0 || r.ActiveTime != 0 {
		t.Fatalf("pre-upgrade run gained partial segments: %+v active %s", r.Segments, r.ActiveTime)
	}
}

// Input no route passes to a waiting Work is recorded as unmatched, as it
// would be were the Work not waiting, and never reaches it as context on
// wake. What the wait asks for still wakes it though the route skips it.
func TestProductionWaitContextFollowsTheRoutes(t *testing.T) {
	ctx := context.Background()
	o := waitOptions(true)
	o.Routes[0].Handler = routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if in.Work != nil && (in.Event.Event.Type == "fixture.noise" || in.Event.Event.Type == "fixture.ready") {
			return extension.Skip(in.Event.Event.Type + " is not passed to the agent"), nil
		}
		return routingDecision(in)
	})
	f := waitFixture(t, "reopen", o)
	work, ref, key := waitForWork(t, f, "route-context", true)
	d := f.Driver.(*waitDriver)
	if err := d.Wait(ref, domain.Wait{ID: "route-context-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.ready"}}}, true); err != nil {
		t.Fatal(err)
	}
	var ids []domain.ID
	for _, typ := range []string{"fixture.noise", "fixture.context", "fixture.noise", "fixture.ready"} {
		e := routingEvent(f, fmt.Sprintf("route-context-%d", len(ids)), &key)
		e.Type = typ
		if err := d.Accept(e); err != nil {
			t.Fatal(err)
		}
		requireRoute(t, d, e.ID)
		ids = append(ids, e.ID)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		for i, want := range []string{"unmatched", "wait_context", "unmatched", "wake"} {
			a, err := tx.Event(ids[i])
			if err != nil {
				return err
			}
			if !a.Routed || a.Disposition != want {
				t.Errorf("%s (%s): routed %v as %q, want %q", ids[i], a.Event.Type, a.Routed, a.Disposition, want)
			}
		}
		return nil
	})
	resumed, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	var got []domain.ID
	for _, event := range resumed.Request.Inputs {
		got = append(got, event.ID)
	}
	if want := []domain.ID{ids[1], ids[3]}; !reflect.DeepEqual(got, want) || resumed.Request.Trigger.ID != ids[3] {
		t.Fatalf("woken with %v (trigger %s), want %v", got, resumed.Request.Trigger.ID, want)
	}
}
