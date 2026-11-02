package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
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
	turnsCost := 5.0
	if err := extension.ValidateResult(extension.Result{Outcome: ok.Outcome, Usage: extension.Usage{Cost: &turnsCost, Currency: "TURNS"}}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatal("a currency naming a reserved unit was accepted")
	}
	if got := (extension.Usage{Cost: &turnsCost, Currency: "Turns"}).Amounts(); len(got) != 0 {
		t.Fatalf("a reserved currency was charged as a unit: %v", got)
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

// budgetFixture seeds Work in two scopes whose executor reports cost each
// turn, and a scheduler over the given budgets.
func budgetFixture(t *testing.T, cost float64, budgets []runtime.Budget) (f conformanceFixture, s *runtime.Scheduler, control *runtime.Control, a, b domain.ID) {
	t.Helper()
	fx, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	a = seedSchedulerWork(t, fx, "scope-a")
	routingRead(t, fx, func(tx storage.Tx) error {
		sc, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		sc.ID, sc.Name = "other-scope", "other"
		return tx.PutScope(sc, 0)
	})
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "scope-b"}
	event := routingEvent(fx, "scope-b-trigger", &key)
	if _, _, err := fx.Driver.(*schedulerDriver).ingress.Accept(context.Background(), "other-scope", event); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, fx.Driver, event.ID)
	b = "work/scope-b"
	e.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		c := cost
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: extension.Usage{Cost: &c, Currency: "USD"}}}, nil
	}
	o := schedulerDefaultOptions(fx, e)
	o.Budgets = budgets
	s = newTestScheduler(t, fx, o)
	var err error
	if control, err = runtime.NewControl(fx.Store, fx.Clock.Now, fx.Driver.(*schedulerDriver).router, s, runtime.CausalLimits{}); err != nil {
		t.Fatal(err)
	}
	return fx, s, control, a, b
}

type conformanceFixture = conformance.RuntimeFixture

// requeue routes another input to Work so it is ready for another turn.
func requeue(t *testing.T, f conformanceFixture, external, id string) {
	t.Helper()
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: external}
	event := routingEvent(f, id, &key)
	scope := domain.ID("routing-scope")
	if external == "scope-b" {
		scope = "other-scope"
	}
	if _, _, err := f.Driver.(*schedulerDriver).ingress.Accept(context.Background(), scope, event); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, event.ID)
}

// TestProductionSchedulerBudgetAcrossScopes: one per: all budget holds Work
// in a second scope once Work in the first used it up, with each scope's
// own exceeded notice, rather than failing the claim.
func TestProductionSchedulerBudgetAcrossScopes(t *testing.T) {
	f, s, _, a, b := budgetFixture(t, 2, []runtime.Budget{{Unit: "usd", Per: runtime.BudgetPerAll, Period: "week", Max: 1, WarnAt: 0.5}})
	ctx := context.Background()
	x, err := s.Start(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(ctx, x); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleBudgets(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, b); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("Work in a second scope over a per: all budget: %v", err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		h, held, err := runtime.HoldInTx(tx, b)
		if !held || h.Kind != runtime.HoldKindBudget {
			t.Fatalf("second scope's Work not held: %v %+v", held, h)
		}
		return err
	})
	scopes := map[domain.ID]bool{}
	for _, n := range budgetEvents(t, func(fn func(storage.Tx) error) { routingRead(t, f, fn) })["core.budget.exceeded"] {
		scopes[n.ScopeID] = true
	}
	if !scopes["routing-scope"] || !scopes["other-scope"] || len(scopes) != 2 {
		t.Fatalf("exceeded notices by scope: %v", scopes)
	}
}

// TestProductionSchedulerBudgetPassIsSpentOnce: a release whose limit was
// then raised spends its pass on the next turn, so the Work is held when it
// goes over the new limit.
func TestProductionSchedulerBudgetPassIsSpentOnce(t *testing.T) {
	f, s, control, a, _ := budgetFixture(t, 0.6, []runtime.Budget{{Unit: "usd", Per: runtime.BudgetPerWork, Max: 0.5}})
	ctx := context.Background()
	turn := func(next string) error {
		t.Helper()
		x, err := s.Start(ctx, a)
		if err != nil {
			return err
		}
		if next != "" {
			requeue(t, f, "scope-a", next)
		}
		return s.Observe(ctx, x)
	}
	if err := turn("pass-2"); err != nil {
		t.Fatal(err)
	}
	if err := turn(""); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("over budget not held: %v", err)
	}
	var w domain.Work
	routingRead(t, f, func(tx storage.Tx) (err error) { w, err = tx.Work(a); return })
	if _, err := control.ReleaseWork(ctx, a, w.Revision, "raised", "pass-release"); err != nil {
		t.Fatal(err)
	}
	// The operator also raises the limit: the next turn is within it, and
	// still spends the pass.
	raised := newTestScheduler(t, f, func() runtime.SchedulerOptions {
		o := schedulerDefaultOptions(f, f.Driver.(*schedulerDriver).executor)
		o.Budgets = []runtime.Budget{{Unit: "usd", Per: runtime.BudgetPerWork, Max: 1}}
		return o
	}())
	s = raised
	if err := turn("pass-3"); err != nil {
		t.Fatalf("a turn within the raised limit: %v", err)
	}
	if err := turn(""); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("a pass left from the release let an over-budget turn through: %v", err)
	}
}
