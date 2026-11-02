package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// lastFixture is the fixture the last startGateHarness made, for a test
// that seeds more Work into it.
var lastFixture conformance.RuntimeFixture

// startGateHarness is a scheduler whose toolbelt has one start gate, the
// registration "approvals", which decide answers.
func startGateHarness(t *testing.T, name string, decide extension.StartGate, tweak ...func(*runtime.SchedulerOptions)) (context.Context, *runtime.Scheduler, *runtime.Control, domain.ID, *int, func(func(storage.Tx) error)) {
	t.Helper()
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	work := seedSchedulerWork(t, f, name)
	runs := 0
	e.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		runs++
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	router := f.Driver.(*schedulerDriver).Router
	belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, router, runtime.ToolbeltOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if decide != nil {
		if err = belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "approvals", StartGate: decide}); err != nil {
			t.Fatal(err)
		}
	}
	o := schedulerDefaultOptions(f, e)
	o.Toolbelt = belt
	o.ToolbeltEndpoints = func(context.Context, domain.ID, string) (string, string, error) { return "", "", nil }
	for _, fn := range tweak {
		fn(&o)
	}
	s := newTestScheduler(t, f, o)
	lastFixture = f
	control, err := runtime.NewControl(f.Store, f.Clock.Now, router, s, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return context.Background(), s, control, work, &runs, func(fn func(storage.Tx) error) { routingRead(t, f, fn) }
}

// A start gate asked about Work's first attempt can hold it: no attempt
// starts, the hold names the gate and its reason, and the Work's history
// records it. An operator can't release that hold; the gate's own
// settlement lets exactly that start through, and later attempts are not
// asked.
func TestProductionSchedulerStartGateHoldsTheFirstStart(t *testing.T) {
	var asked []extension.StartCall
	hold := true
	ctx, s, control, work, runs, read := startGateHarness(t, "gated", func(_ context.Context, c extension.StartCall) (*extension.StartHold, error) {
		asked = append(asked, c)
		if hold {
			return &extension.StartHold{Reason: "waiting for a person's approval"}, nil
		}
		return nil, nil
	})
	if _, err := s.Start(ctx, work); !errors.Is(err, runtime.ErrGateHeld) || !errors.Is(err, runtime.ErrNotEligible) {
		t.Fatalf("a held start was not refused: %v", err)
	}
	if *runs != 0 || len(asked) != 1 || asked[0].Work.ID != work || asked[0].Event.ID == "" || asked[0].Binding == "" {
		t.Fatalf("gate asked %+v, runs %d", asked, *runs)
	}
	var w domain.Work
	read(func(tx storage.Tx) error {
		h, held, err := runtime.HoldInTx(tx, work)
		if err != nil {
			return err
		}
		if !held || h.Kind != runtime.HoldKindGate || h.Holder != "approvals" || h.Reason != "waiting for a person's approval" {
			t.Fatalf("not held by the gate: %v %+v", held, h)
		}
		a, err := tx.Event(h.EventID)
		if err != nil || a.Event.Type != "core.gate.held" || a.Event.WorkID != work {
			t.Fatalf("hold not in history: %+v %v", a, err)
		}
		w, err = tx.Work(work)
		return err
	})
	// Held Work is not asked again, and doesn't start.
	if _, err := s.Start(ctx, work); !errors.Is(err, runtime.ErrNotEligible) || len(asked) != 1 {
		t.Fatalf("held Work asked again or started: %v (%d asks)", err, len(asked))
	}
	if _, err := control.ReleaseWork(ctx, work, w.Revision, "skip it", "op-release"); !errors.Is(err, runtime.ErrGateHold) {
		t.Fatalf("an operator released a gate's hold: %v", err)
	}
	if _, err := control.SettleGateHold(ctx, "someone-else", work, true, "", "k1"); !errors.Is(err, runtime.ErrHeld) {
		t.Fatalf("another registration settled the hold: %v", err)
	}
	ev, err := control.SettleGateHold(ctx, "approvals", work, true, "approved by operator:local", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := control.SettleGateHold(ctx, "approvals", work, true, "approved by operator:local", "k1"); err != nil || again != ev {
		t.Fatalf("settlement not idempotent: %v %v", again, err)
	}
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatalf("the settled start did not run: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("a settled start asked the gate again: %d", len(asked))
	}
	if err = s.Observe(ctx, a); err != nil || *runs != 1 {
		t.Fatalf("observe: %v (runs %d)", err, *runs)
	}
}

// Refusing settles the hold by cancelling the Work, with the reason in its
// history.
func TestProductionSchedulerStartGateRefusalCancels(t *testing.T) {
	ctx, s, control, work, runs, read := startGateHarness(t, "refused", func(context.Context, extension.StartCall) (*extension.StartHold, error) {
		return &extension.StartHold{}, nil
	})
	if _, err := s.Start(ctx, work); !errors.Is(err, runtime.ErrGateHeld) {
		t.Fatal(err)
	}
	read(func(tx storage.Tx) error {
		h, _, err := runtime.HoldInTx(tx, work)
		if !strings.Contains(h.Reason, "approvals") {
			t.Fatalf("an empty reason was not filled in: %q", h.Reason)
		}
		return err
	})
	if _, err := control.SettleGateHold(ctx, "approvals", work, false, "refused by operator:local", "no"); err != nil {
		t.Fatal(err)
	}
	read(func(tx storage.Tx) error {
		w, err := tx.Work(work)
		if err != nil || w.State != domain.WorkCancelled {
			t.Fatalf("refused Work not cancelled: %v %v", w.State, err)
		}
		if _, held, _ := runtime.HoldInTx(tx, work); held {
			t.Fatal("hold left on cancelled Work")
		}
		events, err := tx.Events(0, 0)
		found := false
		for _, a := range events {
			found = found || a.Event.Type == "core.gate.refused" && a.Event.WorkID == work && strings.Contains(string(a.Event.Data.Data), "refused by operator:local")
		}
		if !found {
			t.Fatal("refusal not in history")
		}
		return err
	})
	if *runs != 0 {
		t.Fatalf("refused Work ran %d times", *runs)
	}
}

// A gate that can't decide (an error, or a panic) refuses the start for now
// without holding the Work, so it is asked again next time: the host fails
// closed. With no start gate the same Work starts (the negative control).
func TestProductionSchedulerStartGateFailsClosed(t *testing.T) {
	for name, gate := range map[string]extension.StartGate{
		"error": func(context.Context, extension.StartCall) (*extension.StartHold, error) {
			return nil, errors.New("unreachable")
		},
		"panic": func(context.Context, extension.StartCall) (*extension.StartHold, error) { panic("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, s, _, work, runs, read := startGateHarness(t, "failing-"+name, gate)
			for range 2 {
				if _, err := s.Start(ctx, work); !errors.Is(err, runtime.ErrStartGate) {
					t.Fatalf("an undecided start was not refused: %v", err)
				}
			}
			read(func(tx storage.Tx) error {
				_, held, err := runtime.HoldInTx(tx, work)
				if held {
					t.Fatal("an undecided gate held the Work")
				}
				return err
			})
			if *runs != 0 {
				t.Fatalf("ran %d times", *runs)
			}
		})
	}
	ctx, s, _, work, runs, _ := startGateHarness(t, "ungated", nil)
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatalf("ungated Work did not start: %v", err)
	}
	if err = s.Observe(ctx, a); err != nil || *runs != 1 {
		t.Fatalf("observe: %v (runs %d)", err, *runs)
	}
}

// A budget that holds the settled start keeps the gate's pass: once the
// budget hold is released, the Work starts without asking the gates again,
// so a person never approves the same start twice.
func TestProductionSchedulerStartGatePassSurvivesABudgetHold(t *testing.T) {
	asked := 0
	ctx, s, control, work, runs, read := startGateHarness(t, "budgeted", func(context.Context, extension.StartCall) (*extension.StartHold, error) {
		asked++
		return &extension.StartHold{Reason: "approve it"}, nil
	}, func(o *runtime.SchedulerOptions) {
		o.Budgets = []runtime.Budget{{Unit: "attempts", Per: runtime.BudgetPerWork, Max: 0}}
	})
	if _, err := s.Start(ctx, work); !errors.Is(err, runtime.ErrGateHeld) {
		t.Fatal(err)
	}
	if _, err := control.SettleGateHold(ctx, "approvals", work, true, "approved", "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, work); !errors.Is(err, runtime.ErrBudgetHeld) {
		t.Fatalf("the budget did not hold the settled start: %v", err)
	}
	var w domain.Work
	read(func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(work)
		return err
	})
	if _, err := control.ReleaseWork(ctx, work, w.Revision, "one more", "budget"); err != nil {
		t.Fatal(err)
	}
	a, err := s.Start(ctx, work)
	if err != nil {
		t.Fatalf("the released start did not run: %v", err)
	}
	if asked != 1 {
		t.Fatalf("the gate was asked %d times", asked)
	}
	if err = s.Observe(ctx, a); err != nil || *runs != 1 {
		t.Fatalf("observe: %v (runs %d)", err, *runs)
	}
}

// The gates are asked only about a start capacity allows: asking can have
// effects (a request a person sees), so Work held back by a limit isn't
// asked until it could start.
func TestProductionSchedulerStartGateWaitsForCapacity(t *testing.T) {
	var asked []domain.ID
	ctx, s, _, first, _, _ := startGateHarness(t, "first", func(_ context.Context, c extension.StartCall) (*extension.StartHold, error) {
		asked = append(asked, c.Work.ID)
		return nil, nil
	}, func(o *runtime.SchedulerOptions) { o.GlobalLimit = 1 })
	f := lastFixture
	second := seedSchedulerWork(t, f, "second")
	// The first start runs and, unobserved, keeps the one slot.
	if _, err := s.Start(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, second); !errors.Is(err, runtime.ErrCapacity) {
		t.Fatalf("a start over capacity was not refused: %v", err)
	}
	if len(asked) != 1 || asked[0] != first {
		t.Fatalf("gates asked about %v; want only %s", asked, first)
	}
}
