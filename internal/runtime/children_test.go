package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// childOptions routes a child's creation to its existing Work (as the
// bundled work route does) and leaves its end to the parent's Wait;
// everything else creates Work as usual.
func childOptions() conformance.RuntimeOptions {
	return waitOptionsWith(routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		e := in.Event.Event
		if e.Type == "core.work.created" && in.Work != nil && in.Work.ParentID != "" {
			return extension.RouteDecision{Match: true, Reason: "child Work"}, nil
		}
		if e.Source == domain.LineageSource {
			return extension.Skip("a child's end"), nil
		}
		return routingDecision(in)
	}))
}

func waitOptionsWith(h extension.RouteHandler) conformance.RuntimeOptions {
	o := waitOptions(false)
	o.Routes[0].Handler = h
	return o
}

// askChild starts the parent's turn, asks for child Work in it and parks the
// parent on its children's ends. It returns the parent, the child and the
// child's creation Event.
func askChild(t *testing.T, f conformance.RuntimeFixture, name string) (domain.ID, domain.ID) {
	t.Helper()
	ctx := context.Background()
	parent, ref, _ := waitForWork(t, f, name, false)
	driver := f.Driver.(*waitDriver)
	child, err := driver.Router.CreateChild(ctx, ref, runtime.ChildRequest{ID: domain.ID("child/" + name), Kind: "fixture.child",
		Context: domain.Payload{Schema: "fixture.child.v1", Data: []byte(`{"task":"what is six times seven?"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var created domain.AcceptedEvent
	var after uint64
	routingRead(t, f, func(tx storage.Tx) error {
		w, e := tx.Work(parent)
		if e != nil {
			return e
		}
		owns := false
		for _, k := range w.CorrelationKeys {
			owns = owns || k == domain.LineageKey(parent)
		}
		if !owns {
			t.Fatalf("the parent did not get its lineage key: %+v", w.CorrelationKeys)
		}
		if created, e = tx.EventByDelivery("core.work", "created/"+string(child.ID)); e != nil {
			return e
		}
		var data struct {
			Kind    string
			Context *domain.Payload
		}
		if e = json.Unmarshal(created.Event.Data.Data, &data); e != nil || data.Kind != "fixture.child" || data.Context == nil || string(data.Context.Data) != `{"task":"what is six times seven?"}` {
			t.Fatalf("the child's creation lost what it was asked: %s", created.Event.Data.Data)
		}
		after, e = tx.LatestSequence()
		return e
	})
	requireRoute(t, f.Driver, created.Event.ID)
	wait := domain.Wait{ID: domain.ID("wait/" + name), AfterSequence: after, Match: domain.EventMatch{Keys: []domain.CorrelationKey{domain.LineageKey(parent)}, Types: []string{domain.TypeChildEnded}}}
	if err = f.Driver.Wait(ref, wait, false); err != nil {
		t.Fatal(err)
	}
	return parent, child.ID
}

// childEnded is the parent's TypeChildEnded Event for child, decoded.
func childEnded(t *testing.T, f conformance.RuntimeFixture, child domain.ID) (domain.AcceptedEvent, domain.ChildEnded) {
	t.Helper()
	var a domain.AcceptedEvent
	var c domain.ChildEnded
	routingRead(t, f, func(tx storage.Tx) error {
		var e error
		if a, e = tx.EventByDelivery(domain.LineageSource, "child-ended/"+string(child)); e != nil {
			return e
		}
		return json.Unmarshal(a.Event.Data.Data, &c)
	})
	return a, c
}

// A Work asks for child Work and parks on its end. The child runs, answers
// with a result, and the parent is woken by an Event carrying it, derived
// from the child's run so the chain stays one causal chain.
func TestChildResultWakesItsParent(t *testing.T) {
	f := waitFixture(t, "reopen", childOptions())
	ctx := context.Background()
	parent, child := askChild(t, f, "front")
	driver := f.Driver.(*waitDriver)
	cref, err := f.Driver.Start(child)
	if err != nil {
		t.Fatalf("the routed child did not start: %v", err)
	}
	turn := domain.Payload{Schema: domain.TurnSchema, Data: []byte(`{"status":"finished","result":"42"}`)}
	if err = driver.Scheduler.Complete(ctx, cref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "answered", Data: turn}}); err != nil {
		t.Fatal(err)
	}
	ended, c := childEnded(t, f, child)
	if ended.Event.WorkID != parent || ended.Event.CorrelationKey == nil || *ended.Event.CorrelationKey != domain.LineageKey(parent) || ended.Event.CausationID == "" {
		t.Fatalf("the child's end is not addressed to its parent: %+v", ended.Event)
	}
	if c.Work != string(child) || c.Kind != "fixture.child" || c.State != domain.WorkCompleted || c.Outcome != domain.OutcomeCompleted || c.Summary != "answered" || string(c.Result) != `"42"` {
		t.Fatalf("the child's end = %+v", c)
	}
	requireRoute(t, f.Driver, ended.Event.ID)
	routingRead(t, f, func(tx storage.Tx) error {
		w, e := tx.Work(parent)
		if e != nil {
			return e
		}
		wait, e := tx.Wait("wait/front")
		if e != nil {
			return e
		}
		if w.State != domain.WorkReady || wait.State != domain.WaitWoken || wait.WakeEventID != ended.Event.ID {
			t.Fatalf("the parent was not woken by its child's end: work %s, wait %+v", w.State, wait)
		}
		return nil
	})
}

// Child Work that completes without a result fails its attempt instead, so
// the parent never hears an empty success; when it fails for good, the
// parent hears that, with no result.
func TestChildWithoutResultFails(t *testing.T) {
	f := waitFixture(t, "reopen", childOptions())
	ctx := context.Background()
	_, child := askChild(t, f, "silent")
	driver := f.Driver.(*waitDriver)
	cref, err := f.Driver.Start(child)
	if err != nil {
		t.Fatal(err)
	}
	turn := domain.Payload{Schema: domain.TurnSchema, Data: []byte(`{"status":"finished"}`)}
	if err = driver.Scheduler.Complete(ctx, cref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "answered as text", Data: turn}, Permanent: false}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		x, e := tx.Execution(cref.ExecutionID)
		if e != nil {
			return e
		}
		w, e := tx.Work(child)
		if e != nil {
			return e
		}
		if x.Outcome == nil || x.Outcome.Kind != domain.OutcomeFailed || x.Outcome.Summary != runtime.MissingResultSummary || w.State == domain.WorkCompleted {
			t.Fatalf("a child without a result completed: execution %+v, work %s", x.Outcome, w.State)
		}
		if w.State == domain.WorkFailed {
			return nil
		}
		if _, e = tx.EventByDelivery(domain.LineageSource, "child-ended/"+string(child)); e == nil {
			t.Fatal("the parent heard from a child that has not ended")
		}
		return nil
	})
	if err = driver.Cancel(child); err != nil {
		t.Fatal(err)
	}
	_, c := childEnded(t, f, child)
	if c.State != domain.WorkCancelled || len(c.Result) != 0 {
		t.Fatalf("the cancelled child's end = %+v", c)
	}
}

// Negative control: Work an operator made a child of another, without the
// parent asking for it, neither tells the parent nor needs a result.
func TestUnaskedChildNeedsNoResult(t *testing.T) {
	f := waitFixture(t, "reopen", childOptions())
	ctx := context.Background()
	driver := f.Driver.(*waitDriver)
	parent := seedSchedulerWork(t, f, "operator-parent")
	control, err := runtime.NewControl(f.Store, f.Clock.Now, driver.Router, driver.Scheduler, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := control.CreateWork(ctx, runtime.ManualWork{ScopeID: "routing-scope", ParentID: parent, Kind: "fixture.manual", Binding: "fixture", DeliveryID: "unasked"})
	if err != nil {
		t.Fatal(err)
	}
	cref, err := f.Driver.Start(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = driver.Scheduler.Complete(ctx, cref, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, e := tx.Work(child.ID)
		if e == nil && w.State != domain.WorkCompleted {
			t.Fatalf("an unasked child was refused: %s", w.State)
		}
		if _, er := tx.EventByDelivery(domain.LineageSource, "child-ended/"+string(child.ID)); er == nil {
			t.Fatal("a parent that never asked heard its child end")
		}
		return e
	})
}
