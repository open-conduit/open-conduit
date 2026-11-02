package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// Reopening gives each transaction an independent SQLite handle, without a
// test-level mutex that could conceal cross-instance coordination problems.
type Reopening struct{ Path string }

func (s *Reopening) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	db, err := sqlite.Open(ctx, s.Path)
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

// RouterExecutor is bound for routing only, which persists intents and never
// invokes an executor's controls. Advertising a capability also requires its concrete interface.
type RouterExecutor struct{ Caps domain.Capabilities }

func (e RouterExecutor) Capabilities() domain.Capabilities {
	c := e.Caps
	if c.Toolbelt == "" {
		c.Toolbelt = extension.ToolbeltNone
	}
	return c
}
func (RouterExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, errRoutingUnsupported
}
func (RouterExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errRoutingUnsupported
}
func (RouterExecutor) Resume(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errRoutingUnsupported
}
func (RouterExecutor) Inject(context.Context, domain.ID, domain.Event) error {
	return errRoutingUnsupported
}
func (RouterExecutor) Interrupt(context.Context, domain.ID) error {
	return errRoutingUnsupported
}

// RoutingDriver drives the production router and lifecycle. Start only
// installs a fixture execution through the public storage contract.
type RoutingDriver struct {
	Store   storage.Store
	Clock   *conformance.Clock
	Router  *runtime.Router
	Ingress *runtime.Ingress
	InitErr error
}

func (d *RoutingDriver) Accept(e domain.Event) error {
	if d.InitErr != nil {
		return d.InitErr
	}
	_, _, err := d.Ingress.Accept(context.Background(), "routing-scope", e)
	return err
}
func (d *RoutingDriver) Route(id domain.ID) error {
	if d.InitErr != nil {
		return d.InitErr
	}
	return d.Router.Route(context.Background(), id)
}
func (d *RoutingDriver) Complete(ref domain.LeaseRef) error {
	return d.Router.Complete(context.Background(), ref)
}
func (d *RoutingDriver) Cancel(id domain.ID) error { return d.Router.Cancel(context.Background(), id) }
func (d *RoutingDriver) Start(id domain.ID) (ref domain.LeaseRef, err error) {
	if d.InitErr != nil {
		return ref, d.InitErr
	}
	err = d.Store.Transact(context.Background(), storage.Guard{Now: d.Clock.Now()}, func(tx storage.Tx) error {
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

var errRoutingUnsupported = errors.New("routing fixture has no scheduler/wait/recovery implementation")

func (*RoutingDriver) Wait(domain.LeaseRef, domain.Wait, bool) error {
	return errRoutingUnsupported
}
func (*RoutingDriver) Wake(domain.ID) error                   { return errRoutingUnsupported }
func (*RoutingDriver) Fail(domain.LeaseRef) error             { return errRoutingUnsupported }
func (*RoutingDriver) Retry(domain.ID, domain.ID, bool) error { return errRoutingUnsupported }
func (*RoutingDriver) Recover(domain.ID, bool) (domain.LeaseRef, error) {
	return domain.LeaseRef{}, errRoutingUnsupported
}
func (*RoutingDriver) Derive(domain.Event) error { return errRoutingUnsupported }

// Store opens a SQLite store in a temp dir. Mode "reopen" gives every
// transaction an independent handle; anything else keeps one open handle.
func Store(t testing.TB, mode string) storage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.db")
	if mode == "reopen" {
		return &Reopening{Path: path}
	}
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// RoutingFixture drives the production router and ingress over backend
// (see Store). Start installs a fixture execution through the public storage
// contract; scheduling, waits and recovery are SchedulerFixture's and
// WaitFixture's.
func RoutingFixture(t testing.TB, backend storage.Store, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
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
	options := runtime.RouterOptions{Generation: "fixture-generation", Handlers: handlers, CausalLimits: runtime.CausalLimits{MaxDepth: o.MaxDepth, PerMinute: o.PerMinute}, Bindings: map[string]runtime.RouteBinding{"fixture": {Capabilities: o.Capabilities, Fallback: o.Fallback, Executor: RouterExecutor{o.Capabilities}}}}
	makeDriver := func() *RoutingDriver {
		r, err := runtime.NewRouter(backend, clock.Now, options)
		i, ie := runtime.NewIngress(backend, clock.Now, runtime.IngressOptions{})
		if err == nil {
			err = ie
		}
		return &RoutingDriver{Store: backend, Clock: clock, Router: r, Ingress: i, InitErr: err}
	}
	return conformance.RuntimeFixture{Driver: makeDriver(), Store: backend, Clock: clock, Restart: func() conformance.RuntimeDriver { return makeDriver() }}
}

// HandlerFunc is a route handler from a function.
type HandlerFunc func(extension.RouteInput) (extension.RouteDecision, error)

func (f HandlerFunc) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(in)
}

// Decision matches every Event, creating Work named for its correlation key's
// external ID (or the Event's ID when it has none).
func Decision(in extension.RouteInput) (extension.RouteDecision, error) {
	id := domain.ID("work/" + string(in.Event.Event.ID))
	if in.Event.Event.CorrelationKey != nil {
		id = domain.ID("work/" + in.Event.Event.CorrelationKey.ExternalID)
	}
	return extension.RouteDecision{Match: true, CreateID: id, Kind: "fixture.work"}, nil
}

// Options routes every Event through handler under one route with policy.
func Options(handler extension.RouteHandler, policy string) conformance.RuntimeOptions {
	return conformance.RuntimeOptions{Routes: []conformance.RuntimeRoute{{Route: domain.Route{Name: "default", ActivePolicy: policy}, Handler: handler}}}
}
