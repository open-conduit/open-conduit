package runtime_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// activeBudgetRun runs turns of Work whose executor reports no usage and
// takes five seconds each, under one active_seconds budget of max. It returns
// the error of the third start.
func activeBudgetRun(t *testing.T, max float64) error {
	t.Helper()
	ctx := context.Background()
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "active-budget")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "active-budget"}
	e.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		f.Clock.Advance(5 * time.Second)
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	o := schedulerDefaultOptions(f, e)
	o.Budgets = []runtime.Budget{{Unit: extension.UnitActiveSeconds, Per: runtime.BudgetPerWork, Max: max}}
	s := newTestScheduler(t, f, o)
	for _, next := range []string{"active-budget-2", "active-budget-3"} {
		a, err := s.Start(ctx, work)
		if err != nil {
			t.Fatalf("a turn under budget was refused: %v", err)
		}
		requireAccept(t, f, next, &key)
		requireRoute(t, f.Driver, domain.ID(next))
		if err = s.Observe(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Start(ctx, work)
	return err
}

// A budget on active time holds Work whose executor reports nothing; the
// same turns under a higher limit are the negative control.
func TestProductionActiveTimeBudgetHoldsWorkThatReportsNoUsage(t *testing.T) {
	if err := activeBudgetRun(t, 10); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("ten seconds against a ten-second budget: %v", err)
	}
	if err := activeBudgetRun(t, 11); err != nil {
		t.Fatalf("under the limit: %v", err)
	}
}

func chargedActive(t *testing.T, s *runtime.Scheduler, id domain.ID) float64 {
	t.Helper()
	state, err := s.InspectAttempt(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return state.Charged[extension.UnitActiveSeconds]
}

// Parked time isn't counted, and the time across a recovery that adopts the
// attempt is counted once, matching the run's active time.
func TestProductionActiveTimeSkipsParksAndCountsRecoveryOnce(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "active-park")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "active-park"}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	id := attempt.Lease.ExecutionID
	f.Clock.Advance(10 * time.Second)
	registerWait(t, f, attempt.Lease, "active-park-wait", key, true, 1)
	if got := chargedActive(t, d.Scheduler, id); got != 10 {
		t.Fatalf("charged %v at the park, want 10", got)
	}
	f.Clock.Advance(time.Hour)
	requireAccept(t, f, "active-park-wake", &key)
	if err = d.Wake("active-park-wait"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Scheduler.Start(ctx, work); err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(d.Options.LeaseDuration + 20*time.Second)
	recovered, err := d.Scheduler.Recover(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(5 * time.Second)
	if err = d.Scheduler.Complete(ctx, recovered.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	want := (10*time.Second + d.Options.LeaseDuration + 25*time.Second).Seconds()
	if got := chargedActive(t, d.Scheduler, id); math.Abs(got-want) > 1e-6 {
		t.Fatalf("charged %v, want %v", got, want)
	}
	var r runtime.Run
	routingRead(t, f, func(tx storage.Tx) error {
		var e error
		r, e = runtime.RunInTx(tx, id)
		return e
	})
	if math.Abs(r.ActiveTime.Seconds()-want) > 1e-6 {
		t.Fatalf("run history says %s, budgets %vs", r.ActiveTime, want)
	}
}

// A turn that dies without a result still spends its time.
func TestProductionActiveTimeCountsAFailedTurn(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "active-failed")
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(7 * time.Second)
	if err = d.Scheduler.Fail(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "died"}}); err != nil {
		t.Fatal(err)
	}
	if got := chargedActive(t, d.Scheduler, attempt.Lease.ExecutionID); got != 7 {
		t.Fatalf("charged %v, want 7", got)
	}
}

// A launch refused for capacity is refunded, so its time isn't charged.
func TestProductionActiveTimeSkipsACapacityRefusal(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "active-refused")
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	f.Clock.Advance(7 * time.Second)
	if err = runtime.HoldForCapacity(ctx, d.Scheduler, attempt.Lease, &extension.NoCapacityError{Reason: "full"}); err != nil {
		t.Fatal(err)
	}
	if got := chargedActive(t, d.Scheduler, attempt.Lease.ExecutionID); got != 0 {
		t.Fatalf("charged %v for a refused launch", got)
	}
}
