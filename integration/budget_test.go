package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// budgetEvents lists the core.budget.* Events accepted so far, by type.
func budgetEvents(t *testing.T, read func(func(storage.Tx) error)) map[string][]runtime.BudgetNotice {
	t.Helper()
	out := map[string][]runtime.BudgetNotice{}
	read(func(tx storage.Tx) error {
		events, err := tx.Events(0, 0)
		for _, a := range events {
			if !strings.HasPrefix(a.Event.Type, "core.budget.") {
				continue
			}
			var n runtime.BudgetNotice
			_ = json.Unmarshal(a.Event.Data.Data, &n)
			out[a.Event.Type] = append(out[a.Event.Type], n)
		}
		return err
	})
	return out
}

// TestProductionSchedulerBudgets: an executor's reported cost is charged to
// the Work's budget; crossing the warning threshold and the limit accept
// routable notices; the next start is refused and the Work held with the
// reason; releasing the hold lets exactly one more turn start.
func TestProductionSchedulerBudgets(t *testing.T) {
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "budget")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "budget"}
	e.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		cost := 0.6
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: extension.Usage{Cost: &cost, Currency: "USD"}}}, nil
	}
	o := schedulerDefaultOptions(f, e)
	o.Budgets = []runtime.Budget{
		{Unit: "usd", Per: runtime.BudgetPerWork, Max: 1, WarnAt: 0.5},
		{Unit: "attempts", Per: runtime.BudgetPerWork, Max: 10},
	}
	s := newTestScheduler(t, f, o)
	control, err := runtime.NewControl(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).router, s, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	read := func(fn func(storage.Tx) error) { routingRead(t, f, fn) }
	ctx := context.Background()
	turn := func(next string) error {
		t.Helper()
		a, err := s.Start(ctx, work)
		if err != nil {
			return err
		}
		// Queue the next input while the turn runs, so the Work stays ready.
		if next != "" {
			requireAccept(t, f, next, &key)
			requireRoute(t, f.Driver, domain.ID(next))
		}
		return s.Observe(ctx, a)
	}
	if err := turn("budget-2"); err != nil {
		t.Fatal(err)
	}
	if err := turn("budget-3"); err != nil {
		t.Fatalf("a turn under budget was refused: %v", err)
	}
	if got := budgetEvents(t, read)["core.budget.warning"]; len(got) != 1 || got[0].Unit != "usd" || got[0].WorkID != work {
		t.Fatalf("crossing the warning threshold accepted %+v", got)
	}
	if err := turn(""); !errors.Is(err, runtime.ErrBudgetHeld) || !errors.Is(err, runtime.ErrNotEligible) {
		t.Fatalf("a start over budget was not refused: %v", err)
	}
	if got := budgetEvents(t, read)["core.budget.exceeded"]; len(got) != 1 || got[0].Used < 1.2-1e-9 || got[0].Max != 1 {
		t.Fatalf("crossing the limit accepted %+v", got)
	}
	var w domain.Work
	read(func(tx storage.Tx) error {
		h, held, err := runtime.HoldInTx(tx, work)
		if err != nil {
			return err
		}
		if !held || h.Kind != runtime.HoldKindBudget || !strings.Contains(h.Reason, "1.2 usd used of 1 usd per Work") {
			t.Fatalf("over-budget Work not held with the reason: %v %+v", held, h)
		}
		w, err = tx.Work(work)
		return err
	})
	uses, err := s.BudgetsFor(ctx, work)
	if err != nil || len(uses) != 2 {
		t.Fatalf("budgets for the Work: %+v %v", uses, err)
	}
	for _, u := range uses {
		if u.Unit == "attempts" && u.Used != 2 {
			t.Fatalf("attempts counted %v, want 2", u.Used)
		}
	}
	if _, err := control.ReleaseWork(ctx, work, w.Revision, "one more", "budget-release"); err != nil {
		t.Fatal(err)
	}
	// The release lets one turn past the limit; the one after is held again.
	if err := turn("budget-4"); err != nil {
		t.Fatalf("a released Work did not start: %v", err)
	}
	if err := turn(""); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("the turn after a released one was not checked again: %v", err)
	}
}

// TestProductionSchedulerBudgetsOffByDefault is the negative control: with
// no budgets the same usage never holds the Work.
func TestProductionSchedulerBudgetsOffByDefault(t *testing.T) {
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, "unbudgeted")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "unbudgeted"}
	e.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: extension.Usage{Units: map[string]float64{"minutes": 100}}}}, nil
	}
	s := newTestScheduler(t, f, schedulerDefaultOptions(f, e))
	for i, next := range []string{"unbudgeted-2", "unbudgeted-3", ""} {
		a, err := s.Start(context.Background(), work)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if next != "" {
			requireAccept(t, f, next, &key)
			requireRoute(t, f.Driver, domain.ID(next))
		}
		if err = s.Observe(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SettleBudgets(context.Background()); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		if _, held, err := runtime.HoldInTx(tx, work); err != nil || held {
			t.Fatalf("Work held without budgets: %v %v", held, err)
		}
		rows, err := tx.List("core.budget.pending", "")
		if len(rows) != 0 {
			t.Fatalf("staged usage kept without budgets: %d rows", len(rows))
		}
		return err
	})
}

// TestUsageUnitsContract: named units are validated, the names core counts
// are refused, and cost and tokens become units.
func TestUsageUnitsContract(t *testing.T) {
	ok := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: extension.Usage{Units: map[string]float64{"minutes": 2.5}}}
	if err := extension.ValidateResult(ok); err != nil {
		t.Fatal(err)
	}
	for _, units := range []map[string]float64{{"turns": 1}, {"attempts": 1}, {"Bad Unit": 1}, {"usd": -1}} {
		bad := ok
		bad.Usage.Units = units
		if err := extension.ValidateResult(bad); !errors.Is(err, extension.ErrInvalid) {
			t.Fatalf("units %v accepted", units)
		}
	}
	cost, in, out := 0.25, uint64(10), uint64(5)
	got := extension.Usage{Cost: &cost, Currency: "USD", InputTokens: &in, OutputTokens: &out, Units: map[string]float64{"minutes": 1}}.Amounts()
	if got["usd"] != 0.25 || got["tokens"] != 15 || got["minutes"] != 1 {
		t.Fatalf("amounts %v", got)
	}
}
