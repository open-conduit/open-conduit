package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// A route that delivers to the Work's current binding (DeliverCurrent)
// queues its input on the binding the Work last ran on, not its own; the
// same route without it queues on its own binding (the negative control).
func TestRouteDeliversOnTheWorksCurrentBinding(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "report"}
	start := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if in.Work != nil {
			return extension.Skip("owned"), nil
		}
		return routingDecision(in)
	})
	reply := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d := extension.RouteDecision{Match: true, Kind: "fixture.work"}
		if in.Event.Event.Type == "fixture.reply" {
			d.Deliver = extension.DeliverCurrent
		}
		return d, nil
	})
	o := conformance.RuntimeOptions{Routes: []conformance.RuntimeRoute{
		{Route: domain.Route{Name: "start", Binding: "job"}, Handler: start},
		{Route: domain.Route{Name: "reply"}, Handler: reply},
	}}
	f, executor := schedulerFixture(t, "reopen", o)
	executor.OnRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "turn ended"}}}, nil
	}
	routingRead(t, f, func(tx storage.Tx) error {
		scope, e := tx.Scope("routing-scope")
		if e != nil {
			return e
		}
		scope.Bindings = append(scope.Bindings, domain.Binding{Name: "job", Executor: "fixture"})
		return tx.PutScope(scope, scope.Revision)
	})
	driver := f.Driver.(*schedulerDriver)
	ctx := context.Background()
	requireAccept(t, f, "started", &key)
	requireRoute(t, f.Driver, "started")
	a, err := driver.Scheduler.Start(ctx, "work/report")
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Scheduler.Observe(ctx, a); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		id, typ, binding string
	}{{"answer", "fixture.reply", "job"}, {"plain", "fixture.changed", "fixture"}} {
		e := routingEvent(f, c.id, &key)
		e.Type = c.typ
		if err := f.Driver.Accept(e); err != nil {
			t.Fatal(err)
		}
		requireRoute(t, f.Driver, e.ID)
		rec, err := driver.Router.Decision(ctx, e.ID)
		if err != nil || rec.Route != "reply" || rec.Policy != "queue" || rec.Binding != c.binding {
			t.Fatalf("%s: want queued on %s, got %+v %v", c.id, c.binding, rec, err)
		}
	}
	pending, err := driver.Router.PendingInputs(ctx, "work/report")
	if err != nil || len(pending) != 2 || pending[0].Binding != "job" || pending[1].Binding != "fixture" {
		t.Fatalf("pending input %+v %v", pending, err)
	}
	trace, err := driver.Router.Trace(ctx, "routing-scope", domain.Event{Type: "fixture.reply", Source: "fixture/account", CorrelationKey: &key}, nil)
	if err != nil || trace.Effect.Binding != "job" {
		t.Fatalf("trace does not show the Work's binding: %+v %v", trace.Effect, err)
	}
}

// A match that only attaches keys (DeliverNone) to waiting Work leaves it
// waiting: it isn't input, so its Wait has nothing to decide.
func TestAttachOnlyLeavesWaitingWorkWaiting(t *testing.T) {
	opened := domain.CorrelationKey{Source: "fixture/account", ExternalID: "opened"}
	o := waitOptions(false)
	base := o.Routes[0].Handler
	o.Routes[0].Handler = routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, err := base.Match(context.Background(), in)
		if in.Event.Event.Type == "fixture.opened" {
			d.AttachKeys, d.Deliver = []domain.CorrelationKey{opened}, extension.DeliverNone
		}
		return d, err
	})
	f := waitFixture(t, "reopen", o)
	id, ref, key := waitForWork(t, f, "job", false)
	registerWait(t, f, ref, id, key, false, 1)
	e := routingEvent(f, "opened", nil)
	e.Type, e.WorkID = "fixture.opened", id
	if err := f.Driver.Accept(e); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, e.ID)
	routingRead(t, f, func(tx storage.Tx) error {
		a, err := tx.Event(e.ID)
		if err != nil {
			return err
		}
		if !a.Routed || a.Disposition != extension.DispositionAttach || a.RoutedWorkID != "work/job" {
			t.Fatalf("attach not recorded: %+v", a)
		}
		w, err := tx.WorkByCorrelation(opened)
		if err != nil || w.ID != "work/job" || w.State != domain.WorkWaiting {
			t.Fatalf("waiting Work did not get the key and stay waiting: %+v %v", w, err)
		}
		inputs, err := runtime.PendingInputsInTx(tx, w.ID)
		if err != nil || len(inputs) != 0 {
			t.Fatalf("attach-only event became input: %+v %v", inputs, err)
		}
		return nil
	})
}

// A decision with a delivery the runtime does not know fails the route as
// invalid input, never guessing.
func TestUnknownDeliveryFailsTheRoute(t *testing.T) {
	bogus := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, err := routingDecision(in)
		d.Deliver = "sideways"
		return d, err
	})
	f := routingFixture(t, "persistent", routingOptions(bogus, "queue"))
	requireAccept(t, f, "e", &domain.CorrelationKey{Source: "fixture/account", ExternalID: "x"})
	var re *runtime.RouteError
	if err := f.Driver.Route("e"); !errors.As(err, &re) || re.Code != runtime.RouteHandlerInvalid {
		t.Fatalf("unknown delivery was not refused: %v", err)
	}
}
