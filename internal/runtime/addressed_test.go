package runtime_test

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/storage"
)

// An operator Event that names ready Work directly is that Work's input when
// no route takes it, on the binding the Work runs on: Work an operator
// created has no route to take its follow-up at all. An operator Event that
// names no Work, and another source's Event that names one, are still
// unmatched.
func TestOperatorEventAddressedToWorkIsItsInput(t *testing.T) {
	skip := routingHandlerFunc(func(extension.RouteInput) (extension.RouteDecision, error) {
		return extension.Skip("not this route's"), nil
	})
	f, executor := schedulerFixture(t, "reopen", routingOptions(skip, "queue"))
	executor.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "turn ended"}}}, nil
	}
	driver := f.Driver.(*schedulerDriver)
	control, err := runtime.NewControl(f.Store, f.Clock.Now, driver.Router, driver.Scheduler, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	work, _, err := control.CreateWork(ctx, runtime.ManualWork{ScopeID: "routing-scope", Kind: "fixture.manual", Binding: "fixture", DeliveryID: "manual-1"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := driver.Scheduler.Start(ctx, work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Scheduler.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, e := tx.Work(work.ID)
		if e == nil && w.State != domain.WorkReady {
			t.Fatalf("the turn left the Work %s, not ready", w.State)
		}
		return e
	})

	reply, _, err := control.SubmitEvent(ctx, runtime.ManualEvent{ScopeID: "routing-scope", WorkID: work.ID, Type: "operator.reply", DeliveryID: "reply-1"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := driver.Router.Decision(ctx, reply.Event.ID)
	if err != nil || rec.WorkID != work.ID || rec.Policy != "queue" || rec.Reason != runtime.ReasonAddressed || rec.Binding != "fixture" || rec.Route != "" {
		t.Fatalf("the operator's reply was not queued for the Work it named: %+v %v", rec, err)
	}
	pending, err := driver.Router.PendingInputs(ctx, work.ID)
	if err != nil || len(pending) != 1 || pending[0].EventID != reply.Event.ID || pending[0].Binding != "fixture" {
		t.Fatalf("pending input %+v %v", pending, err)
	}
	if a, err = driver.Scheduler.Start(ctx, work.ID); err != nil {
		t.Fatalf("the reply did not start the next turn: %v", err)
	}
	if err = driver.Scheduler.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}

	// Rebound with nothing queued, the Work's follow-up goes to the binding
	// the operator moved it to, not the one its last attempt ran on.
	var w domain.Work
	routingRead(t, f, func(tx storage.Tx) error {
		scope, e := tx.Scope("routing-scope")
		if e != nil {
			return e
		}
		scope.Bindings = append(scope.Bindings, domain.Binding{Name: "other", Executor: "fixture"})
		if e = tx.PutScope(scope, scope.Revision); e != nil {
			return e
		}
		w, e = tx.Work(work.ID)
		return e
	})
	if _, err = control.Rebind(ctx, work.ID, w.Revision, "other", "move it", "rebind-1"); err != nil {
		t.Fatal(err)
	}
	reply, _, err = control.SubmitEvent(ctx, runtime.ManualEvent{ScopeID: "routing-scope", WorkID: work.ID, Type: "operator.reply", DeliveryID: "reply-2"})
	if err != nil {
		t.Fatal(err)
	}
	if rec, err = driver.Router.Decision(ctx, reply.Event.ID); err != nil || rec.Reason != runtime.ReasonAddressed || rec.Binding != "other" {
		t.Fatalf("the follow-up after a rebind did not go to the new binding: %+v %v", rec, err)
	}
	if a, err = driver.Scheduler.Start(ctx, work.ID); err != nil {
		t.Fatal(err)
	}
	if err = driver.Scheduler.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}

	// Negative controls: neither of these is addressed input.
	loose, _, err := control.SubmitEvent(ctx, runtime.ManualEvent{ScopeID: "routing-scope", Type: "operator.reply", DeliveryID: "loose-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rec, err = driver.Router.Decision(ctx, loose.Event.ID); err != nil || rec.Policy != "unmatched" || rec.WorkID != "" {
		t.Fatalf("an operator Event naming no Work was routed: %+v %v", rec, err)
	}
	other := runtimetest.Event(f, "source-1", nil)
	other.WorkID = work.ID
	if err = f.Driver.Accept(other); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, other.ID)
	if rec, err = driver.Router.Decision(ctx, other.ID); err != nil || rec.Policy != "unmatched" {
		t.Fatalf("another source's Event naming the Work was taken as addressed: %+v %v", rec, err)
	}
	if pending, err = driver.Router.PendingInputs(ctx, work.ID); err != nil || len(pending) != 0 {
		t.Fatalf("unaddressed input reached the Work: %+v %v", pending, err)
	}
}
