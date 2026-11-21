package runtime_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

func admissionFixture(t *testing.T, limits map[domain.ID]int) (conformanceFixture, *runtime.Scheduler, *runtime.Control) {
	f, s, c, _ := admissionFixtureWith(t, limits)
	return f, s, c
}

func admissionFixtureWith(t *testing.T, limits map[domain.ID]int) (conformanceFixture, *runtime.Scheduler, *runtime.Control, *schedulerExecutor) {
	t.Helper()
	f, e := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	e.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	o := schedulerDefaultOptions(f, e)
	o.AdmissionLimits = limits
	s := newTestScheduler(t, f, o)
	control, err := runtime.NewControl(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).Router, s, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return f, s, control, e
}

// admissionEvents lists the core.admission.* Event types in Work's history.
func admissionEvents(t *testing.T, f conformanceFixture, work domain.ID) []string {
	t.Helper()
	var out []string
	routingRead(t, f, func(tx storage.Tx) error {
		out = nil // A read can run again; each run lists the history afresh.
		events, err := tx.Events(0, 0)
		for _, a := range events {
			if a.Event.WorkID == work && strings.HasPrefix(a.Event.Type, "core.admission.") {
				out = append(out, a.Event.Type)
			}
		}
		return err
	})
	return out
}

// TestProductionSchedulerAdmission: with an admission limit of two, the
// third open Work is held, not refused; it cannot start; when admitted Work
// finishes, the admission pass releases it, and its history records both.
func TestProductionSchedulerAdmission(t *testing.T) {
	f, s, _ := admissionFixture(t, map[domain.ID]int{"routing-scope": 2})
	ctx := context.Background()
	a := seedSchedulerWork(t, f, "a")
	b := seedSchedulerWork(t, f, "b")
	c := seedSchedulerWork(t, f, "c")
	if _, err := s.Start(ctx, a); !errors.Is(err, runtime.ErrNotAdmitted) || !errors.Is(err, runtime.ErrNotEligible) {
		t.Fatalf("Work started before the admission pass: %v", err)
	}
	r, err := s.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Admitted, []domain.ID{a, b}) || !slices.Equal(r.Held, []domain.ID{c}) || len(r.Released) != 0 {
		t.Fatalf("first pass: %+v", r)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		h, held, err := runtime.HoldInTx(tx, c)
		if !held || h.Kind != runtime.HoldKindAdmission || !strings.Contains(h.Reason, "admission limit of 2") {
			t.Fatalf("Work beyond the limit not held with the reason: %v %+v", held, h)
		}
		return err
	})
	if _, err = s.Start(ctx, c); !errors.Is(err, runtime.ErrNotEligible) {
		t.Fatalf("held Work started: %v", err)
	}
	// A second pass changes nothing while both places are taken.
	if r, err = s.Admit(ctx); err != nil || len(r.Admitted)+len(r.Held)+len(r.Released) != 0 {
		t.Fatalf("idle pass: %+v %v", r, err)
	}
	x, err := s.Start(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Observe(ctx, x); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, err := tx.Work(a)
		if err == nil && !w.State.Terminal() {
			t.Fatalf("admitted Work did not finish: %s", w.State)
		}
		return err
	})
	if r, err = s.Admit(ctx); err != nil || !slices.Equal(r.Released, []domain.ID{c}) {
		t.Fatalf("pass after admitted Work finished: %+v %v", r, err)
	}
	if got := admissionEvents(t, f, c); !slices.Equal(got, []string{"core.admission.held", "core.admission.released"}) {
		t.Fatalf("held Work's history: %v", got)
	}
	if x, err = s.Start(ctx, c); err != nil {
		t.Fatalf("released Work did not start: %v", err)
	}
	if err = s.Observe(ctx, x); err != nil {
		t.Fatal(err)
	}
}

// TestProductionSchedulerAdmissionRelease: an operator's release admits
// held Work at once, past the limit.
func TestProductionSchedulerAdmissionRelease(t *testing.T) {
	f, s, control := admissionFixture(t, map[domain.ID]int{"routing-scope": 1})
	ctx := context.Background()
	seedSchedulerWork(t, f, "a")
	b := seedSchedulerWork(t, f, "b")
	if r, err := s.Admit(ctx); err != nil || !slices.Equal(r.Held, []domain.ID{b}) {
		t.Fatalf("pass: %+v %v", r, err)
	}
	var w domain.Work
	routingRead(t, f, func(tx storage.Tx) (err error) { w, err = tx.Work(b); return })
	if _, err := control.ReleaseWork(ctx, b, w.Revision, "admit now", "admission-release"); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Admit(ctx); err != nil || len(r.Held) != 0 {
		t.Fatalf("released Work held again: %+v %v", r, err)
	}
	if _, err := s.Start(ctx, b); err != nil {
		t.Fatalf("Work an operator released did not start: %v", err)
	}
}

// TestProductionSchedulerAdmissionLimitRemoved: held Work is released once
// a new generation has no admission limit at all.
func TestProductionSchedulerAdmissionLimitRemoved(t *testing.T) {
	f, s, _, e := admissionFixtureWith(t, map[domain.ID]int{"routing-scope": 1})
	ctx := context.Background()
	seedSchedulerWork(t, f, "a")
	b := seedSchedulerWork(t, f, "b")
	if r, err := s.Admit(ctx); err != nil || !slices.Equal(r.Held, []domain.ID{b}) {
		t.Fatalf("pass: %+v %v", r, err)
	}
	o := schedulerDefaultOptions(f, e)
	unlimited := newTestScheduler(t, f, o)
	if r, err := unlimited.Admit(ctx); err != nil || !slices.Equal(r.Released, []domain.ID{b}) {
		t.Fatalf("pass without limits: %+v %v", r, err)
	}
	if _, err := unlimited.Start(ctx, b); err != nil {
		t.Fatalf("released Work did not start: %v", err)
	}
}

// TestProductionSchedulerAdmissionOff: without admission limits every Work
// starts and the pass holds nothing (the negative control for the above).
func TestProductionSchedulerAdmissionOff(t *testing.T) {
	f, s, _ := admissionFixture(t, nil)
	ctx := context.Background()
	seedSchedulerWork(t, f, "a")
	b := seedSchedulerWork(t, f, "b")
	if r, err := s.Admit(ctx); err != nil || len(r.Admitted)+len(r.Held) != 0 {
		t.Fatalf("pass without limits: %+v %v", r, err)
	}
	if _, err := s.Start(ctx, b); err != nil {
		t.Fatalf("Work without admission limits did not start: %v", err)
	}
}
