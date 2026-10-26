package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

func TestSchedulerRetryPolicyPinnedAcrossGeneration(t *testing.T) {
	ctx := context.Background()
	f, executor := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "retry-policy-pin")
	original := f.Driver.(*schedulerDriver).scheduler
	first, err := original.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	changed := schedulerDefaultOptions(f, executor)
	changed.Generation = "new-generation"
	changed.HistoricalExecutors = map[string]map[string]extension.Executor{"fixture-generation": {"fixture": executor}}
	changed.Retry = runtime.RetryPolicy{MaxAttempts: 1, TerminalOnExhaustion: true}
	replacement := newTestScheduler(t, f, changed)
	f.Clock.Advance(time.Minute)
	recovered, err := replacement.Recover(ctx, first.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Complete(ctx, recovered.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed}}); err != nil {
		t.Fatal(err)
	}
	status, err := replacement.RetryStatus(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if status.Exhausted || status.FailureCount != 1 || !status.EligibleAt.Equal(f.Clock.Now().Add(5*time.Minute)) {
		t.Fatalf("reloaded policy replaced pinned attempt policy: %+v", status)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, er := tx.Work(work)
		if er != nil {
			return er
		}
		if w.State != domain.WorkReady {
			t.Fatalf("old policy Work prematurely terminal: %+v", w)
		}
		return nil
	})
	// A newly created attempt pins the new generation's one-attempt policy.
	f.Clock.Advance(5 * time.Minute)
	if err = f.Driver.(*schedulerDriver).Retry(work, "", false); err != nil {
		t.Fatal(err)
	}
	next, err := replacement.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	state, err := replacement.InspectAttempt(ctx, next.Lease.ExecutionID)
	if err != nil || state.Generation != changed.Generation || state.Retry.MaxAttempts != 1 {
		t.Fatalf("new attempt didn't pin new policy: %+v %v", state, err)
	}
	if err = replacement.Complete(ctx, next.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, er := tx.Work(work)
		if er != nil {
			return er
		}
		if w.State != domain.WorkFailed {
			t.Fatalf("new explicit terminal policy not applied: %+v", w)
		}
		return nil
	})
}
func TestSchedulerRetryPolicyConfigurationBounds(t *testing.T) {
	f, executor := schedulerFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	options := schedulerDefaultOptions(f, executor)
	options.Retry = runtime.RetryPolicy{Delays: make([]time.Duration, 100)}
	for i := range options.Retry.Delays {
		options.Retry.Delays[i] = time.Second
	}
	if _, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).router, options); err != nil {
		t.Fatalf("B05's hundred-delay maximum rejected: %v", err)
	}
	options.Retry = runtime.RetryPolicy{MaxAttempts: 102}
	if _, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).router, options); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("unbounded attempt policy accepted: %v", err)
	}
	options.Retry = runtime.RetryPolicy{MaxAttempts: 3, Delays: []time.Duration{time.Second}}
	if _, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).router, options); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("contradictory attempt/delay counts accepted: %v", err)
	}
}

func TestSchedulerControlUsesPinnedCapabilitiesAfterReload(t *testing.T) {
	ctx := context.Background()
	f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "pinned-control")
	if _, err := f.Driver.(*schedulerDriver).scheduler.Start(ctx, work); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		scope, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		scope.Routes[0].ActivePolicy = "inject"
		return tx.PutScope(scope, scope.Revision)
	})
	replacement := &schedulerExecutor{caps: domain.Capabilities{Inject: true}}
	options := runtime.RouterOptions{Generation: "replacement", Handlers: map[string]extension.RouteHandler{"handler-0": routingHandlerFunc(routingDecision)}, Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: replacement}}}
	router, err := runtime.NewRouter(f.Store, f.Clock.Now, options)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "pinned-control"}
	requireAccept(t, f, "control-after-reload", &key)
	if err = router.Route(ctx, "control-after-reload"); !errors.Is(err, extension.ErrUnsupported) {
		t.Fatalf("new registration granted old attempt capability: %v", err)
	}
	options.Bindings["fixture"] = runtime.RouteBinding{Executor: replacement, Fallback: "queue"}
	router, err = runtime.NewRouter(f.Store, f.Clock.Now, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = router.Route(ctx, "control-after-reload"); err != nil {
		t.Fatal(err)
	}
	decision, err := router.Decision(ctx, "control-after-reload")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Policy != "queue" || decision.Reason != "target_control_unsupported" {
		t.Fatalf("missing pinned capability fallback: %+v", decision)
	}
	intents, err := router.Intents(ctx, work)
	if err != nil || len(intents) != 0 {
		t.Fatalf("unsupported old attempt got control intent: %+v %v", intents, err)
	}
}

func TestSchedulerRequiresDurableLaunchAcknowledgement(t *testing.T) {
	ctx := context.Background()
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "missing-ack")
	e.onRun = func(context.Context, extension.RunRequest) (extension.RunHandle, error) {
		return &schedulerHandle{}, nil
	}
	s := f.Driver.(*schedulerDriver).scheduler
	attempt, err := s.Start(ctx, work)
	if !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("unpersisted launch acknowledged: %v", err)
	}
	state, err := s.InspectAttempt(ctx, attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Excluded || state.RecoveryRecorded {
		t.Fatalf("unverifiable launch released resources: %+v", state)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		x, er := tx.Execution(attempt.Lease.ExecutionID)
		if er == nil && x.State != domain.ExecutionFailed {
			t.Fatalf("missing-ack attempt state: %s", x.State)
		}
		return er
	})
}

func TestSchedulerControlErrorPolicies(t *testing.T) {
	for _, fallback := range []string{"", "queue", "drop"} {
		t.Run("fallback_"+fallback, func(t *testing.T) {
			ctx := context.Background()
			o := routingOptions(routingHandlerFunc(routingDecision), "inject")
			o.Capabilities = domain.Capabilities{Inject: true}
			f, e := schedulerFixture(t, "reopen", o)
			work := seedSchedulerWork(t, f, "control-errors")
			options := schedulerDefaultOptions(f, e)
			if fallback != "" {
				options.ControlErrorFallback = map[string]string{"fixture": fallback}
			}
			s := newTestScheduler(t, f, options)
			attempt, err := s.Start(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "control-errors"}
			requireAccept(t, f, "incoming-error", &key)
			requireRoute(t, f.Driver, "incoming-error")
			intents, err := f.Driver.(*schedulerDriver).router.Intents(ctx, work)
			if err != nil || len(intents) != 1 {
				t.Fatalf("intents: %+v %v", intents, err)
			}
			e.onInject = func(context.Context, domain.ID, domain.Event) error { return errors.New("sensitive provider detail") }
			if err = s.DispatchControl(ctx, intents[0].ID); !errors.Is(err, runtime.ErrControlDispatch) {
				t.Fatalf("ambiguous delivery: %v", err)
			}
			state, err := s.ControlStatus(ctx, intents[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := f.Driver.(*schedulerDriver).router.PendingInputs(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			if fallback == "" {
				if state.Status != "retryable" || len(pending) != 1 {
					t.Fatalf("ambiguous input lost: %+v %+v", state, pending)
				}
				e.onInject = nil
				if err = s.DispatchControl(ctx, intents[0].ID); err != nil {
					t.Fatal(err)
				}
				state, err = s.ControlStatus(ctx, intents[0].ID)
				if err != nil || state.Status != "accepted" || state.Attempts != 2 {
					t.Fatalf("stable retry: %+v %v", state, err)
				}
			} else {
				want := 1
				if fallback == "drop" {
					want = 0
				}
				if state.Status != "fallback" || state.EffectivePolicy != fallback || len(pending) != want {
					t.Fatalf("explicit fallback: %+v %+v", state, pending)
				}
			}
			e.mu.Lock()
			targets := append([]domain.ID(nil), e.injects...)
			e.mu.Unlock()
			for _, target := range targets {
				if target != attempt.Lease.ExecutionID {
					t.Fatalf("retargeted control to %s", target)
				}
			}
		})
	}
}
