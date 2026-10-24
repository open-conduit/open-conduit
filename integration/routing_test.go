package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
	"github.com/alsiola/open-conduit/storage"
)

// Each transaction gets an independent SQLite handle, without a test-level
// mutex that could conceal cross-instance coordination problems.
type routingReopening struct{ path string }

func (s *routingReopening) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	db, err := sqlite.Open(ctx, s.path)
	if err != nil {
		return err
	}
	err = db.Transact(ctx, g, fn)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Fixture controls must never be invoked by routing: this ticket persists
// intents only. Advertising a capability also requires its concrete interface.
type routingFixtureExecutor struct{ caps domain.Capabilities }

func (e routingFixtureExecutor) Capabilities() domain.Capabilities {
	c := e.caps
	if c.Toolbelt == "" {
		c.Toolbelt = extension.ToolbeltNone
	}
	return c
}
func (routingFixtureExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, errRoutingFixtureUnsupported
}
func (routingFixtureExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errRoutingFixtureUnsupported
}
func (routingFixtureExecutor) Resume(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errRoutingFixtureUnsupported
}
func (routingFixtureExecutor) Inject(context.Context, domain.ID, domain.Event) error {
	return errRoutingFixtureUnsupported
}
func (routingFixtureExecutor) Interrupt(context.Context, domain.ID) error {
	return errRoutingFixtureUnsupported
}

// These shared cases exercise the production router and lifecycle. Start only
// installs a fixture execution through the public storage contract: production
// scheduling, dispatch, waits and recovery are delivered by subsequent tickets.
type routingDriver struct {
	store   storage.Store
	clock   *conformance.Clock
	router  *runtime.Router
	ingress *runtime.Ingress
	initErr error
}

func (d *routingDriver) Accept(e domain.Event) error {
	if d.initErr != nil {
		return d.initErr
	}
	_, _, err := d.ingress.Accept(context.Background(), "routing-scope", e)
	return err
}
func (d *routingDriver) Route(id domain.ID) error {
	if d.initErr != nil {
		return d.initErr
	}
	return d.router.Route(context.Background(), id)
}
func (d *routingDriver) Complete(ref domain.LeaseRef) error {
	return d.router.Complete(context.Background(), ref)
}
func (d *routingDriver) Cancel(id domain.ID) error { return d.router.Cancel(context.Background(), id) }
func (d *routingDriver) Start(id domain.ID) (ref domain.LeaseRef, err error) {
	if d.initErr != nil {
		return ref, d.initErr
	}
	err = d.store.Transact(context.Background(), storage.Guard{Now: d.clock.Now()}, func(tx storage.Tx) error {
		w, e := tx.Work(id)
		if e != nil {
			return e
		}
		if w.State != domain.WorkReady {
			return storage.ErrConflict
		}
		inputs, e := runtime.PendingInputsInTx(tx, id)
		if e != nil {
			return e
		}
		if len(inputs) == 0 {
			return storage.ErrInvalid
		}
		xs, e := tx.ListExecutions(id)
		if e != nil {
			return e
		}
		var attempt uint32
		for _, x := range xs {
			if x.Attempt > attempt {
				attempt = x.Attempt
			}
		}
		xid := domain.ID(fmt.Sprintf("%s/fixture-attempt/%d", id, attempt+1))
		if e = tx.PutExecution(domain.Execution{ID: xid, WorkID: id, Attempt: attempt + 1, State: domain.ExecutionPending, ExecutorBinding: "fixture", TriggerID: inputs[0].EventID}, 0); e != nil {
			return e
		}
		ref, e = tx.Claim(xid, "fixture-owner", time.Minute)
		if e != nil {
			return e
		}
		_, e = runtime.ConsumeInputsInTx(tx, id, xid)
		return e
	})
	return
}

var errRoutingFixtureUnsupported = errors.New("routing fixture has no scheduler/wait/recovery implementation")

func (*routingDriver) Wait(domain.LeaseRef, domain.Wait, bool) error {
	return errRoutingFixtureUnsupported
}
func (*routingDriver) Wake(domain.ID) error                   { return errRoutingFixtureUnsupported }
func (*routingDriver) Fail(domain.LeaseRef) error             { return errRoutingFixtureUnsupported }
func (*routingDriver) Retry(domain.ID, domain.ID, bool) error { return errRoutingFixtureUnsupported }
func (*routingDriver) Recover(domain.ID, bool) (domain.LeaseRef, error) {
	return domain.LeaseRef{}, errRoutingFixtureUnsupported
}
func (*routingDriver) Derive(domain.Event) error { return errRoutingFixtureUnsupported }

func routingFixture(t *testing.T, mode string, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.db")
	var backend storage.Store
	if mode == "reopen" {
		backend = &routingReopening{path: path}
	} else {
		db, err := sqlite.Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		backend = db
	}
	clock := conformance.NewClock()
	handlers := map[string]extension.RouteHandler{}
	routes := make([]domain.Route, len(o.Routes))
	for i, r := range o.Routes {
		routes[i] = r.Route
		routes[i].Handler = fmt.Sprintf("handler-%d", i)
		if routes[i].Binding == "" {
			routes[i].Binding = "fixture"
		}
		handlers[routes[i].Handler] = r.Handler
	}
	if err := backend.Transact(context.Background(), storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "routing-scope", Name: "routing", Routes: routes, Bindings: []domain.Binding{{Name: "fixture", Executor: "fixture"}}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	options := runtime.RouterOptions{Generation: "fixture-generation", Handlers: handlers, Bindings: map[string]runtime.RouteBinding{"fixture": {Capabilities: o.Capabilities, Fallback: o.Fallback, Executor: routingFixtureExecutor{o.Capabilities}}}}
	makeDriver := func() *routingDriver {
		r, err := runtime.NewRouter(backend, clock.Now, options)
		i, ie := runtime.NewIngress(backend, clock.Now, runtime.IngressOptions{})
		if err == nil {
			err = ie
		}
		return &routingDriver{store: backend, clock: clock, router: r, ingress: i, initErr: err}
	}
	return conformance.RuntimeFixture{Driver: makeDriver(), Store: backend, Clock: clock, Restart: func() conformance.RuntimeDriver { return makeDriver() }}
}
func TestProductionRoutingSharedCases(t *testing.T) {
	selected := map[string]bool{"concurrent_correlations_and_attempts": true, "independent_work_progress": true, "first_match_replay_and_drop_audit": true, "multi_key_attachment_conflict_and_order": true, "uncorrelated_and_terminal_successor": true, "scope_route_inheritance": true, "unsupported_policy_explicit_fallback": true, "completion_inject_orders": true, "unmatched_and_contradictory_target": true}
	for _, mode := range []string{"persistent", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			for _, c := range conformance.RuntimeCases() {
				if !selected[c.Name] {
					continue
				}
				t.Run(c.Name, func(t *testing.T) {
					if err := c.Check(func(o conformance.RuntimeOptions) conformance.RuntimeFixture { return routingFixture(t, mode, o) }); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

type routingHandlerFunc func(extension.RouteInput) (extension.RouteDecision, error)

func (f routingHandlerFunc) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(in)
}
func routingDecision(in extension.RouteInput) (extension.RouteDecision, error) {
	id := domain.ID("work/" + string(in.Event.Event.ID))
	if in.Event.Event.CorrelationKey != nil {
		id = domain.ID("work/" + in.Event.Event.CorrelationKey.ExternalID)
	}
	return extension.RouteDecision{Match: true, CreateID: id, Kind: "fixture.work"}, nil
}
func routingOptions(handler extension.RouteHandler, policy string) conformance.RuntimeOptions {
	return conformance.RuntimeOptions{Routes: []conformance.RuntimeRoute{{Route: domain.Route{Name: "default", ActivePolicy: policy}, Handler: handler}}}
}
func routingEvent(f conformance.RuntimeFixture, id string, key *domain.CorrelationKey) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: "fixture.changed", Source: "fixture/account", DeliveryID: id, OccurredAt: f.Clock.Now(), ReceivedAt: f.Clock.Now(), CorrelationKey: key}
}
func requireRoute(t *testing.T, d conformance.RuntimeDriver, id domain.ID) {
	t.Helper()
	if err := d.Route(id); err != nil {
		t.Fatal(err)
	}
}
func requireAccept(t *testing.T, f conformance.RuntimeFixture, id string, key *domain.CorrelationKey) {
	t.Helper()
	if err := f.Driver.Accept(routingEvent(f, id, key)); err != nil {
		t.Fatal(err)
	}
}
func routingRead(t *testing.T, f conformance.RuntimeFixture, fn func(storage.Tx) error) {
	t.Helper()
	if err := conformance.RuntimeRead(f, fn); err != nil {
		t.Fatal(err)
	}
}

func TestProductionRoutingCompetingHandles(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "shared"}
	// Independent runtime instances open a distinct SQLite handle per transaction;
	// they share no process-local guard in the test storage adapter.
	f := routingFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	requireAccept(t, f, "a", &key)
	requireAccept(t, f, "b", &key)
	second := f.Restart()
	gate := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-gate; results <- f.Driver.Route("a") }()
	go func() { <-gate; results <- second.Route("b") }()
	close(gate)
	for range 2 {
		if err := <-results; err != nil && !errors.Is(err, storage.ErrConflict) {
			t.Fatal(err)
		}
	}
	requireRoute(t, f.Driver, "a")
	requireRoute(t, second, "b")
	routingRead(t, f, func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		if err == nil && len(works) != 1 {
			return fmt.Errorf("competing routers created %d works", len(works))
		}
		return err
	})
}

func TestProductionRoutingAttachmentBacklogAndReplay(t *testing.T) {
	primary := domain.CorrelationKey{Source: "fixture/account", ExternalID: "primary"}
	second := domain.CorrelationKey{Source: primary.Source, ExternalID: "second"}
	handler := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, e := routingDecision(in)
		if in.Event.Event.ID == "attach" {
			d.AttachKeys = []domain.CorrelationKey{second}
		}
		return d, e
	})
	f := routingFixture(t, "reopen", routingOptions(handler, "queue"))
	requireAccept(t, f, "initial", &primary)
	requireRoute(t, f.Driver, "initial")
	requireAccept(t, f, "older-unresolved", &second)
	requireAccept(t, f, "attach", &primary)
	requireRoute(t, f.Driver, "attach")
	routingRead(t, f, func(tx storage.Tx) error {
		older, err := tx.Event("older-unresolved")
		if err != nil {
			return err
		}
		if !older.Routed || older.RoutedWorkID != "work/primary" {
			return fmt.Errorf("earlier attached backlog not routed to owner: %+v", older)
		}
		return nil
	})
	f.Driver = f.Restart()
	requireRoute(t, f.Driver, "attach")
	inputs, err := f.Driver.(*routingDriver).router.PendingInputs(context.Background(), "work/primary")
	if err != nil {
		t.Fatal(err)
	}
	var ids []domain.ID
	for _, in := range inputs {
		ids = append(ids, in.EventID)
	}
	if !reflect.DeepEqual(ids, []domain.ID{"initial", "older-unresolved", "attach"}) {
		t.Fatalf("replay/backlog changed queue: %v", ids)
	}
}

func TestProductionRoutingHandlerFailureStopsMatching(t *testing.T) {
	failure := errors.New("fixture handler failure")
	var laterCalls int
	f := routingFixture(t, "reopen", conformance.RuntimeOptions{Routes: []conformance.RuntimeRoute{{Route: domain.Route{Name: "first", ActivePolicy: "queue"}, Handler: routingHandlerFunc(func(extension.RouteInput) (extension.RouteDecision, error) { return extension.RouteDecision{}, failure })}, {Route: domain.Route{Name: "later", ActivePolicy: "queue"}, Handler: routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		laterCalls++
		return routingDecision(in)
	})}}})
	requireAccept(t, f, "failure", nil)
	if err := f.Driver.Route("failure"); !errors.Is(err, failure) {
		t.Fatalf("lost handler error: %v", err)
	}
	if laterCalls != 0 {
		t.Fatal("handler error fell through")
	}
	routingRead(t, f, func(tx storage.Tx) error {
		a, err := tx.Event("failure")
		if err == nil && a.Routed {
			return errors.New("handler failure committed checkpoint")
		}
		return err
	})
}

func TestProductionRoutingCrossKeyInFlightOrder(t *testing.T) {
	primary := domain.CorrelationKey{Source: "fixture/account", ExternalID: "primary"}
	second := domain.CorrelationKey{Source: primary.Source, ExternalID: "second"}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	handler := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, e := routingDecision(in)
		if in.Event.Event.ID == "initial" {
			d.AttachKeys = []domain.CorrelationKey{second}
		}
		if in.Event.Event.ID == "earlier" {
			once.Do(func() { close(entered) })
			<-release
		}
		return d, e
	})
	f := routingFixture(t, "reopen", routingOptions(handler, "queue"))
	requireAccept(t, f, "initial", &primary)
	requireRoute(t, f.Driver, "initial")
	requireAccept(t, f, "earlier", &primary)
	requireAccept(t, f, "later", &second)
	done := make(chan error, 1)
	go func() { done <- f.Driver.Route("earlier") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not enter")
	}
	err := f.Restart().Route("later")
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("later key bypassed in-flight earlier event: %v", err)
	}
	requireRoute(t, f.Driver, "later")
}

func TestProductionRoutingDetachedHandlerSnapshots(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "original"}
	first := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if in.Event.Event.ID == "inspect" {
			in.Event.Event.Type = "fixture.mutated"
			in.Event.Event.CorrelationKey.ExternalID = "mutated"
			in.Event.Event.Data.Data[0] = '!'
			in.Scope.Routes[1].Binding = "missing"
			if in.Work != nil {
				in.Work.Kind = "fixture.mutated"
				in.Work.CorrelationKeys[0].ExternalID = "mutated"
			}
		}
		return extension.RouteDecision{}, nil
	})
	second := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if in.Event.Event.ID == "inspect" && (in.Event.Event.Type != "fixture.changed" || in.Event.Event.CorrelationKey.ExternalID != "original" || string(in.Event.Event.Data.Data) != `{"ok":true}` || in.Scope.Routes[1].Binding != "fixture" || in.Work == nil || in.Work.Kind != "fixture.work" || in.Work.CorrelationKeys[0] != key) {
			return extension.RouteDecision{}, errors.New("previous handler mutated subsequent snapshot")
		}
		return routingDecision(in)
	})
	f := routingFixture(t, "reopen", conformance.RuntimeOptions{Routes: []conformance.RuntimeRoute{{Route: domain.Route{Name: "first", ActivePolicy: "queue"}, Handler: first}, {Route: domain.Route{Name: "second", ActivePolicy: "queue"}, Handler: second}}})
	requireAccept(t, f, "initial", &key)
	requireRoute(t, f.Driver, "initial")
	e := routingEvent(f, "inspect", &key)
	e.Data = domain.Payload{Schema: "fixture.input.v1", Data: []byte(`{"ok":true}`)}
	if err := f.Driver.Accept(e); err != nil {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, "inspect")
	routingRead(t, f, func(tx storage.Tx) error {
		a, err := tx.Event("inspect")
		if err != nil {
			return err
		}
		w, err := tx.WorkByCorrelation(key)
		if err != nil {
			return err
		}
		if a.Event.Type != e.Type || string(a.Event.Data.Data) != string(e.Data.Data) || w.Kind != "fixture.work" || w.CorrelationKeys[0] != key {
			return errors.New("handler snapshot mutation reached storage")
		}
		return nil
	})
}

func TestProductionRoutingExecutionOnlyBacklog(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "execution"}
	f := routingFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	requireAccept(t, f, "initial", &key)
	requireRoute(t, f.Driver, "initial")
	ref, err := f.Driver.Start("work/execution")
	if err != nil {
		t.Fatal(err)
	}
	earlier := routingEvent(f, "execution-only", nil)
	earlier.ExecutionID = ref.ExecutionID
	if err = f.Driver.Accept(earlier); err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "later-key", &key)
	if err = f.Driver.Route("later-key"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("execution target lost ordered backlog: %v", err)
	}
	if err = f.Driver.Complete(ref); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("completion overtook execution-only pending input: %v", err)
	}
	requireRoute(t, f.Driver, "execution-only")
	requireRoute(t, f.Driver, "later-key")
	if err = f.Driver.Complete(ref); err != nil {
		t.Fatal(err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		w, err := tx.Work("work/execution")
		if err == nil && w.State != domain.WorkReady {
			return errors.New("completion discarded unconsumed routed input")
		}
		return err
	})
}

func TestProductionRoutingRevalidatesAncestorScope(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		once.Do(func() { close(entered); <-release })
		return routingDecision(in)
	})
	f := routingFixture(t, "reopen", routingOptions(h, "queue"))
	routingRead(t, f, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "root-scope", Name: "root", Routes: []domain.Route{{Name: "default", Handler: "handler-0", Binding: "fixture", ActivePolicy: "queue"}}, Bindings: []domain.Binding{{Name: "fixture", Executor: "fixture"}}}, 0); err != nil {
			return err
		}
		s, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		s.ParentID = "root-scope"
		s.Routes = nil
		s.Bindings = nil
		return tx.PutScope(s, s.Revision)
	})
	requireAccept(t, f, "ancestor", nil)
	done := make(chan error, 1)
	go func() { done <- f.Driver.Route("ancestor") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not enter")
	}
	routingRead(t, f, func(tx storage.Tx) error {
		s, err := tx.Scope("root-scope")
		if err != nil {
			return err
		}
		s.Routes = nil
		return tx.PutScope(s, s.Revision)
	})
	close(release)
	err := <-done
	if err != nil && !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	requireRoute(t, f.Driver, "ancestor")
	routingRead(t, f, func(tx storage.Tx) error {
		a, err := tx.Event("ancestor")
		if err == nil && (!a.Routed || a.Disposition != "unmatched" || a.RoutedWorkID != "") {
			return fmt.Errorf("stale ancestor decision committed: %+v", a)
		}
		return err
	})
}

func TestProductionRoutingInheritedOverrideSlots(t *testing.T) {
	var called []string
	makeHandler := func(name string, match bool) extension.RouteHandler {
		return routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
			called = append(called, name)
			d, err := routingDecision(in)
			d.Match = match
			return d, err
		})
	}
	f := routingFixture(t, "reopen", conformance.RuntimeOptions{Routes: []conformance.RuntimeRoute{{Route: domain.Route{Name: "first", ActivePolicy: "queue"}, Handler: makeHandler("first", false)}, {Route: domain.Route{Name: "second", ActivePolicy: "queue"}, Handler: makeHandler("ancestor-second", true)}, {Route: domain.Route{Name: "third", ActivePolicy: "queue"}, Handler: makeHandler("child-second", true)}}})
	routingRead(t, f, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "root-scope", Name: "root", Routes: []domain.Route{{Name: "first", Handler: "handler-0", Binding: "fixture", ActivePolicy: "queue"}, {Name: "second", Handler: "handler-1", Binding: "fixture", ActivePolicy: "queue"}}, Bindings: []domain.Binding{{Name: "fixture", Executor: "fixture"}}}, 0); err != nil {
			return err
		}
		s, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		s.ParentID = "root-scope"
		s.Bindings = nil
		s.Routes = []domain.Route{{Name: "second", Handler: "handler-2", Binding: "fixture", ActivePolicy: "queue"}}
		return tx.PutScope(s, s.Revision)
	})
	requireAccept(t, f, "inherited", nil)
	requireRoute(t, f.Driver, "inherited")
	if !reflect.DeepEqual(called, []string{"first", "child-second"}) {
		t.Fatalf("effective route order: %v", called)
	}
}

func TestProductionRoutingAttachmentDoesNotResurrectCheckpointedEvent(t *testing.T) {
	primary := domain.CorrelationKey{Source: "fixture/account", ExternalID: "primary"}
	second := domain.CorrelationKey{Source: primary.Source, ExternalID: "second"}
	h := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, e := routingDecision(in)
		if in.Event.Event.ID == "attach" {
			d.AttachKeys = []domain.CorrelationKey{second}
		}
		return d, e
	})
	f := routingFixture(t, "reopen", routingOptions(h, "queue"))
	requireAccept(t, f, "initial", &primary)
	requireRoute(t, f.Driver, "initial")
	requireAccept(t, f, "already-unmatched", &second)
	routingRead(t, f, func(tx storage.Tx) error { return tx.MarkRouted("already-unmatched", "", "unmatched") })
	requireAccept(t, f, "attach", &primary)
	requireRoute(t, f.Driver, "attach")
	routingRead(t, f, func(tx storage.Tx) error {
		a, err := tx.Event("already-unmatched")
		if err == nil && (a.Disposition != "unmatched" || a.RoutedWorkID != "") {
			return errors.New("attachment resurrected a checkpointed event")
		}
		return err
	})
	inputs, err := f.Driver.(*routingDriver).router.PendingInputs(context.Background(), "work/primary")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range inputs {
		if in.EventID == "already-unmatched" {
			t.Fatal("checkpointed event requeued on attachment")
		}
	}
}

func TestProductionRoutingDurableControlIntentIdentity(t *testing.T) {
	for _, policy := range []string{"inject", "interrupt"} {
		t.Run(policy, func(t *testing.T) {
			key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "control"}
			o := routingOptions(routingHandlerFunc(routingDecision), policy)
			o.Capabilities = domain.Capabilities{Inject: true, Interrupt: true}
			f := routingFixture(t, "reopen", o)
			requireAccept(t, f, "initial", &key)
			requireRoute(t, f.Driver, "initial")
			ref, err := f.Driver.Start("work/control")
			if err != nil {
				t.Fatal(err)
			}
			requireAccept(t, f, "control", &key)
			requireRoute(t, f.Driver, "control")
			f.Driver = f.Restart()
			requireRoute(t, f.Driver, "control")
			router := f.Driver.(*routingDriver).router
			intents, err := router.Intents(context.Background(), "work/control")
			if err != nil {
				t.Fatal(err)
			}
			if len(intents) != 1 {
				t.Fatalf("control replay produced %d intents", len(intents))
			}
			intent := intents[0]
			if intent.ID == "" || intent.EventID != "control" || intent.WorkID != "work/control" || intent.ExecutionID != ref.ExecutionID || intent.Epoch != ref.Epoch || intent.Policy != policy {
				t.Fatalf("wrong control target: %+v", intent)
			}
			pending, err := router.PendingInputs(context.Background(), "work/control")
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].EventID != "control" {
				t.Fatalf("unconfirmed control lost pending input: %+v", pending)
			}
			decision, err := router.Decision(context.Background(), "control")
			if err != nil {
				t.Fatal(err)
			}
			if decision.Generation != "fixture-generation" || decision.Route != "default" || decision.Policy != policy {
				t.Fatalf("missing committed decision metadata: %+v", decision)
			}
			routingRead(t, f, func(tx storage.Tx) error {
				owned := routingEvent(f, "owned-timer-event", nil)
				owned.WorkID = "work/control"
				owned.CausationID = "initial"
				if err := tx.PutTimer(domain.Timer{ID: "owned-timer", FireAt: f.Clock.Now().Add(time.Hour), Event: owned}, 0); err != nil {
					return err
				}
				independent := routingEvent(f, "independent-timer-event", nil)
				return tx.PutTimer(domain.Timer{ID: "independent-timer", FireAt: f.Clock.Now().Add(time.Hour), Event: independent}, 0)
			})
			if err = f.Driver.Cancel("work/control"); err != nil {
				t.Fatal(err)
			}
			if err = f.Driver.Complete(ref); !errors.Is(err, storage.ErrLeaseLost) {
				t.Fatalf("cancelled control retained authority: %v", err)
			}
			routingRead(t, f, func(tx storage.Tx) error {
				if _, err := tx.Timer("owned-timer"); !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("cancel retained owned timer: %v", err)
				}
				if _, err := tx.Timer("independent-timer"); err != nil {
					return fmt.Errorf("cancel removed independent timer: %v", err)
				}
				return nil
			})
			pending, err = router.PendingInputs(context.Background(), "work/control")
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatal("cancel retained queued input")
			}
			after, err := router.Intents(context.Background(), "work/control")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, intents) {
				t.Fatal("cancel rewrote historical control intent")
			}
		})
	}
}

func TestProductionRoutingScopeBindingOverrideUsesInstalledExecutor(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "override"}
	h := routingHandlerFunc(routingDecision)
	f := routingFixture(t, "reopen", routingOptions(h, "inject"))
	routingRead(t, f, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "root-scope", Name: "root", Routes: []domain.Route{{Name: "default", Handler: "handler-0", Binding: "fixture", ActivePolicy: "inject"}}, Bindings: []domain.Binding{{Name: "fixture", Executor: "supports-inject"}}}, 0); err != nil {
			return err
		}
		s, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		s.ParentID = "root-scope"
		s.Routes = nil
		s.Bindings = []domain.Binding{{Name: "fixture", Executor: "queue-only"}}
		return tx.PutScope(s, s.Revision)
	})
	router, err := runtime.NewRouter(f.Store, f.Clock.Now, runtime.RouterOptions{Generation: "override-generation", Handlers: map[string]extension.RouteHandler{"handler-0": h}, Bindings: map[string]runtime.RouteBinding{
		"supports-inject": {Capabilities: domain.Capabilities{Inject: true}, Executor: routingFixtureExecutor{domain.Capabilities{Inject: true}}},
		"queue-only":      {Fallback: "drop", Executor: routingFixtureExecutor{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.Driver.(*routingDriver).router = router
	requireAccept(t, f, "initial", &key)
	requireRoute(t, f.Driver, "initial")
	if _, err = f.Driver.Start("work/override"); err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "active", &key)
	requireRoute(t, f.Driver, "active")
	decision, err := router.Decision(context.Background(), "active")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Policy != "drop" || decision.Reason == "" {
		t.Fatalf("nearest binding override did not apply explicitfallback: %+v", decision)
	}
	intents, err := router.Intents(context.Background(), "work/override")
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("unsupported overridden executor got control intent: %+v", intents)
	}
}

func TestProductionRoutingUnrelatedMalformedIdentityDoesNotBlockProgress(t *testing.T) {
	a := domain.CorrelationKey{Source: "fixture/account", ExternalID: "a"}
	b := domain.CorrelationKey{Source: a.Source, ExternalID: "b"}
	unowned := domain.CorrelationKey{Source: a.Source, ExternalID: "unowned"}
	f := routingFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	requireAccept(t, f, "initial-a", &a)
	requireRoute(t, f.Driver, "initial-a")
	ref, err := f.Driver.Start("work/a")
	if err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "initial-b", &b)
	requireRoute(t, f.Driver, "initial-b")
	bad := routingEvent(f, "malformed-b", &unowned)
	bad.WorkID = "work/b"
	// Raw accepted rows can exist from storage-backed timers or older writers.
	// Its identity needs diagnosis, while independent Work remains operable.
	routingRead(t, f, func(tx storage.Tx) error { _, _, err := tx.AcceptEvent(bad); return err })
	requireAccept(t, f, "later-a", &a)
	requireRoute(t, f.Driver, "later-a")
	if err = f.Driver.Complete(ref); err != nil {
		t.Fatalf("unrelated malformed row blocked completion: %v", err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		bad, err := tx.Event("malformed-b")
		if err == nil && bad.Routed {
			return errors.New("unrelated malformed row silently checkpointed")
		}
		return err
	})
}

func TestProductionSuccessorPreservesAcceptedTargetScope(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "late-owner"}
	f := routingFixture(t, "reopen", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	routingRead(t, f, func(tx storage.Tx) error { return tx.PutScope(domain.Scope{ID: "other-scope", Name: "other"}, 0) })
	driver := f.Driver.(*routingDriver)
	if _, _, err := driver.ingress.Accept(context.Background(), "other-scope", routingEvent(f, "foreign-target", &key)); err != nil {
		t.Fatal(err)
	}
	// Ownership can be established after an unresolved acceptance. That cannot
	// retroactively grant the original target scope authority over this Work.
	routingRead(t, f, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "old", Kind: "fixture.work", ScopeID: "routing-scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key}}, 0)
	})
	if err := driver.router.Cancel(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	successorKey := domain.CorrelationKey{Source: key.Source, ExternalID: key.ExternalID + "/generation/2"}
	_, err := driver.router.CreateSuccessor(context.Background(), "old", "foreign-target", runtime.ChildRequest{ID: "successor", Kind: "fixture.work", Keys: []domain.CorrelationKey{successorKey}})
	if !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("successor escaped immutable acceptedtarget scope: %v", err)
	}
	routingRead(t, f, func(tx storage.Tx) error {
		if _, err := tx.Work("successor"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("crossscope successor leaked: %v", err)
		}
		return nil
	})
}

func TestProductionRoutingControlBindingMismatchUsesExplicitFallback(t *testing.T) {
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "binding"}
	initial := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, e := routingDecision(in)
		d.Match = in.Event.Event.ID == "initial"
		return d, e
	})
	f := routingFixture(t, "reopen", conformance.RuntimeOptions{Capabilities: domain.Capabilities{Inject: true}, Fallback: "queue", Routes: []conformance.RuntimeRoute{
		{Route: domain.Route{Name: "initial", Binding: "fixture", ActivePolicy: "queue"}, Handler: initial},
		{Route: domain.Route{Name: "active", Binding: "alternate", ActivePolicy: "inject"}, Handler: routingHandlerFunc(routingDecision)},
	}})
	routingRead(t, f, func(tx storage.Tx) error {
		s, err := tx.Scope("routing-scope")
		if err != nil {
			return err
		}
		s.Bindings = append(s.Bindings, domain.Binding{Name: "alternate", Executor: "fixture"})
		return tx.PutScope(s, s.Revision)
	})
	requireAccept(t, f, "initial", &key)
	requireRoute(t, f.Driver, "initial")
	if _, err := f.Driver.Start("work/binding"); err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "active", &key)
	requireRoute(t, f.Driver, "active")
	router := f.Driver.(*routingDriver).router
	decision, err := router.Decision(context.Background(), "active")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Policy != "queue" || decision.Reason != "target_binding_mismatch" {
		t.Fatalf("unsafe runningbinding control decision: %+v", decision)
	}
	intents, err := router.Intents(context.Background(), "work/binding")
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatal("bindingmismatch dispatched control intent")
	}
}
