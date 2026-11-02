package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const routeNamespace = "core.routing"
const queueNamespace = "core.routing.queue"
const intentNamespace = "core.routing.intents"
const consumedNamespace = "core.routing.consumed"

// RouteBinding is supplied by validated composition. Capabilities must describe
// installed control implementations, not an executor's unsupported promises.
// This router commits intents; scheduling dispatches them outside transactions.
type RouteBinding struct {
	Executor     extension.Executor
	Capabilities domain.Capabilities
	Fallback     string
}
type RouterOptions struct {
	Generation string
	Handlers   map[string]extension.RouteHandler
	// Bindings is keyed by effective Binding.Executor registration, not name.
	Bindings     map[string]RouteBinding
	CausalLimits CausalLimits
	// RouteTimeout bounds one handler's decision (DefaultRouteTimeout
	// when zero).
	RouteTimeout time.Duration
	// MaxRouteFailures is how many retryable failures an Event gets before
	// it is routed as failed (DefaultMaxRouteFailures when zero).
	MaxRouteFailures int
	// RouteFailureWindow is the least time from an Event's first
	// retryable failure to giving up on it (DefaultRouteFailureWindow when
	// zero).
	RouteFailureWindow time.Duration
}
type Router struct {
	store   storage.Store
	now     func() time.Time
	options RouterOptions
}

type RouteRecord struct {
	EventID, WorkID                            domain.ID
	Route, Binding, Generation, Policy, Reason string
	// DisplayRefused says why the route's Work display was dropped (over a
	// limit, a link that is not http or https). The Work kept its display
	// and the Event was routed as usual.
	DisplayRefused string `json:",omitempty"`
	// Error and Attempts say why routing gave up on the Event (Policy
	// "failed") and after how many tries.
	Error    string `json:",omitempty"`
	Attempts int    `json:",omitempty"`
}
type QueuedInput struct {
	EventID  domain.ID
	Sequence uint64
	Binding  string
}
type ControlIntent struct {
	ID                           string
	EventID, WorkID, ExecutionID domain.ID
	Epoch                        uint64
	Policy, Binding              string
}

func NewRouter(s storage.Store, now func() time.Time, o RouterOptions) (*Router, error) {
	if s == nil || now == nil || o.Generation == "" {
		return nil, storage.ErrInvalid
	}
	if _, err := NewCausal(s, now, o.CausalLimits); err != nil {
		return nil, err
	}
	handlers := map[string]extension.RouteHandler{}
	for name, h := range o.Handlers {
		if name == "" || h == nil {
			return nil, extension.ErrInvalid
		}
		handlers[name] = h
	}
	bindings := map[string]RouteBinding{}
	for name, b := range o.Bindings {
		if name == "" || b.Fallback != "" && b.Fallback != "queue" && b.Fallback != "drop" {
			return nil, extension.ErrInvalid
		}
		if b.Executor != nil {
			if err := extension.ValidateBinding(domain.Binding{Name: name}, b.Executor); err != nil {
				return nil, err
			}
			b.Capabilities = b.Executor.Capabilities()
			// Freeze metadata for route commits, then verify this exact cached
			// advertisement also has the corresponding optional interfaces.
			if b.Capabilities.Inject {
				if _, ok := b.Executor.(extension.InjectExecutor); !ok {
					return nil, extension.ErrInvalid
				}
			}
			if b.Capabilities.Interrupt {
				if _, ok := b.Executor.(extension.InterruptExecutor); !ok {
					return nil, extension.ErrInvalid
				}
			}
			if b.Capabilities.Resume {
				if _, ok := b.Executor.(extension.ResumeExecutor); !ok {
					return nil, extension.ErrInvalid
				}
			}
		} else if b.Capabilities.Inject || b.Capabilities.Interrupt || b.Capabilities.Resume || b.Capabilities.Workspace || b.Capabilities.Toolbelt != "" && b.Capabilities.Toolbelt != extension.ToolbeltNone {
			return nil, extension.ErrInvalid
		}
		bindings[name] = b
	}
	o.Handlers, o.Bindings = handlers, bindings
	return &Router{s, now, o}, nil
}
func (r *Router) transact(ctx context.Context, lease *domain.LeaseRef, fn func(storage.Tx) error) error {
	return r.store.Transact(ctx, storage.Guard{Now: r.now(), Lease: lease}, fn)
}

func routeOwner(tx storage.Tx, e domain.Event) (domain.Work, error) {
	var result domain.Work
	check := func(w domain.Work) error {
		if result.ID != "" && result.ID != w.ID {
			return storage.ErrInvalid
		}
		result = w
		return nil
	}
	if e.WorkID != "" {
		w, err := tx.Work(e.WorkID)
		if err != nil {
			return result, err
		}
		if err = check(w); err != nil {
			return result, err
		}
	}
	if e.ExecutionID != "" {
		x, err := tx.Execution(e.ExecutionID)
		if err != nil {
			return result, err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return result, err
		}
		if err = check(w); err != nil {
			return result, err
		}
	}
	if e.CorrelationKey != nil {
		w, err := tx.WorkByCorrelation(*e.CorrelationKey)
		if errors.Is(err, storage.ErrNotFound) {
			if result.ID != "" {
				return result, storage.ErrInvalid
			}
		} else if err != nil {
			return result, err
		} else if err = check(w); err != nil {
			return result, err
		}
	}
	if result.ID == "" {
		return result, storage.ErrNotFound
	}
	return result, nil
}
func scopeForEvent(tx storage.Tx, a domain.AcceptedEvent) (domain.ID, error) {
	var scope domain.ID
	err := getJSON(tx, acceptanceNamespace, identity(string(a.Event.ID)), &scope)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", err
	}
	return resolveScope(tx, a.Event, scope)
}

type routeSnapshot struct {
	event  domain.AcceptedEvent
	scope  domain.Scope
	scopes []domain.Scope
	work   *domain.Work
	events []domain.AcceptedEvent
	routes []domain.Route
}

func snapshot(tx storage.Tx, id domain.ID) (s routeSnapshot, err error) {
	s.event, err = tx.Event(id)
	if err != nil || s.event.Routed {
		return
	}
	if err = routeContext(tx, &s); err != nil {
		return
	}
	w, e := routeOwner(tx, s.event.Event)
	if e == nil {
		s.work = &w
	} else if !errors.Is(e, storage.ErrNotFound) {
		return s, e
	}
	s.events, err = tx.UnroutedEvents(0, 0)
	return
}

// routeContext fills in s.event's Scope chain and its effective routes and
// bindings.
func routeContext(tx storage.Tx, s *routeSnapshot) error {
	scopeID, e := scopeForEvent(tx, s.event)
	if e != nil {
		return e
	}
	if scopeID == "" {
		return storage.ErrInvalid
	}
	return scopeSnapshot(tx, scopeID, s)
}

// scopeSnapshot resolves the scope chain, its effective routes (root to leaf)
// and its effective bindings into s.
func scopeSnapshot(tx storage.Tx, scopeID domain.ID, s *routeSnapshot) error {
	seen := map[domain.ID]bool{}
	for scopeID != "" {
		if seen[scopeID] {
			return extension.ErrInvalid
		}
		seen[scopeID] = true
		scope, e := tx.Scope(scopeID)
		if e != nil {
			return e
		}
		s.scopes = append(s.scopes, scope)
		scopeID = scope.ParentID
	}
	s.scope = s.scopes[0]
	effectiveBindings := map[string]domain.Binding{}
	for i := len(s.scopes) - 1; i >= 0; i-- {
		level := s.scopes[i]
		names := map[string]bool{}
		for _, route := range level.Routes {
			if route.Name == "" || names[route.Name] {
				return extension.ErrInvalid
			}
			names[route.Name] = true
		}
		s.routes = extension.InheritRoutes(s.routes, level.Routes)
		bindingNames := map[string]bool{}
		for _, b := range level.Bindings {
			if b.Name == "" || b.Executor == "" || bindingNames[b.Name] {
				return extension.ErrInvalid
			}
			bindingNames[b.Name] = true
			effectiveBindings[b.Name] = b
		}
	}
	s.scope.Bindings = nil
	for _, b := range effectiveBindings {
		s.scope.Bindings = append(s.scope.Bindings, b)
	}
	sort.Slice(s.scope.Bindings, func(i, j int) bool { return s.scope.Bindings[i].Name < s.scope.Bindings[j].Name })
	return nil
}
func detachedInput(s routeSnapshot) extension.RouteInput {
	a := s.event
	a.Event.Data.Data = append([]byte(nil), a.Event.Data.Data...)
	if a.Event.CorrelationKey != nil {
		k := *a.Event.CorrelationKey
		a.Event.CorrelationKey = &k
	}
	scope := s.scope
	scope.Config.Data = append([]byte(nil), scope.Config.Data...)
	scope.Routes = append([]domain.Route(nil), scope.Routes...)
	for i := range scope.Routes {
		scope.Routes[i].Config.Data = append([]byte(nil), scope.Routes[i].Config.Data...)
	}
	scope.Bindings = append([]domain.Binding(nil), scope.Bindings...)
	for i := range scope.Bindings {
		scope.Bindings[i].Config.Data = append([]byte(nil), scope.Bindings[i].Config.Data...)
	}
	scope.SecretRefs = map[string]string{}
	for k, v := range s.scope.SecretRefs {
		scope.SecretRefs[k] = v
	}
	var work *domain.Work
	if s.work != nil {
		w := *s.work
		w.CorrelationKeys = append([]domain.CorrelationKey(nil), w.CorrelationKeys...)
		w.Context.Data = append([]byte(nil), w.Context.Data...)
		if w.Display != nil {
			d := *w.Display
			d.Links = append([]domain.Link(nil), d.Links...)
			w.Display = &d
		}
		work = &w
	}
	return extension.RouteInput{Event: a, Scope: scope, Work: work}
}

type routePlan struct {
	event    domain.AcceptedEvent
	selected *domain.Route
	decision extension.RouteDecision
	// refused is why the decision's display was dropped.
	refused string
}

// binding resolves the whole nearest Scope binding before looking up its
// registered executor implementation. Equal binding names in different Scopes
// therefore cannot accidentally share another executor's control capabilities.
func (r *Router) binding(scope domain.Scope, name string) (RouteBinding, error) {
	if name == "" {
		return RouteBinding{}, nil
	}
	for _, b := range scope.Bindings {
		if b.Name == name {
			impl, ok := r.options.Bindings[b.Executor]
			if !ok {
				return impl, extension.ErrInvalid
			}
			// Capabilities were mechanically validated and frozen by NewRouter.
			// Never invoke extension methods from a route commit transaction.
			c, required := impl.Capabilities, b.Requires
			if required.Interrupt && !c.Interrupt || required.Inject && !c.Inject || required.Resume && !c.Resume || required.Workspace && !c.Workspace || required.Toolbelt != "" && required.Toolbelt != c.Toolbelt {
				return impl, extension.ErrUnsupported
			}
			return impl, nil
		}
	}
	return RouteBinding{}, extension.ErrInvalid
}

// evaluate tries the routes in order and stops at the first match. record,
// when set, sees each route's decision as it is made (a route trace).
func (r *Router) evaluate(ctx context.Context, s routeSnapshot, record func(domain.Route, extension.RouteDecision)) (p routePlan, err error) {
	p.event = s.event
	for _, route := range s.routes {
		id := s.event.Event.ID
		h := r.options.Handlers[route.Handler]
		if h == nil {
			return p, routeConfigError(id, route, RouteHandlerMissing, extension.ErrInvalid)
		}
		impl, bindingErr := r.binding(s.scope, route.Binding)
		if bindingErr != nil {
			return p, routeConfigError(id, route, RouteBindingInvalid, bindingErr)
		}
		policy := route.ActivePolicy
		if policy != "" && policy != "queue" && policy != "drop" && policy != "inject" && policy != "interrupt" {
			return p, routeConfigError(id, route, RoutePolicyInvalid, extension.ErrInvalid)
		}
		b := impl
		if policy == "inject" && !b.Capabilities.Inject || policy == "interrupt" && !b.Capabilities.Interrupt {
			if b.Fallback == "" {
				return p, routeConfigError(id, route, RoutePolicyInvalid, extension.ErrUnsupported)
			}
		}
		in := detachedInput(s)
		in.Route = route
		in.Route.Config.Data = append([]byte(nil), route.Config.Data...)
		d, e := r.match(ctx, id, route, h, in)
		if e != nil {
			return p, e
		}
		if record != nil {
			record(route, d)
		}
		if d.Match {
			// A display is optional: one over the limits is dropped and
			// recorded, never allowed to stop the Work receiving input.
			if err := extension.ValidateDisplay(d.Display); err != nil {
				d.Display, p.refused = nil, err.Error()
			}
			selected := route
			p.selected = &selected
			d.AttachKeys = append([]domain.CorrelationKey(nil), d.AttachKeys...)
			p.decision = d
			break
		}
	}
	return
}

// setDisplay replaces the Work's display with a route's, when it gave one.
func setDisplay(w *domain.Work, d *domain.WorkDisplay) {
	if w != nil && d != nil && !w.State.Terminal() {
		w.Display = d
	}
}
func containsKey(w domain.Work, key domain.CorrelationKey) bool {
	for _, k := range w.CorrelationKeys {
		if k == key {
			return true
		}
	}
	return false
}
func appendKeys(w *domain.Work, keys []domain.CorrelationKey) {
	for _, key := range keys {
		if !containsKey(*w, key) {
			w.CorrelationKeys = append(w.CorrelationKeys, key)
		}
	}
}
func eventTargets(e domain.Event, w domain.Work) bool {
	return e.WorkID == w.ID || e.CorrelationKey != nil && containsKey(w, *e.CorrelationKey)
}

var errRouteRetry = errors.New("route snapshot changed")

// potentiallyTargets avoids interpreting unrelated malformed identity records.
func potentiallyTargets(tx storage.Tx, e domain.Event, w domain.Work) (bool, error) {
	if eventTargets(e, w) {
		return true, nil
	}
	if e.ExecutionID != "" {
		x, err := tx.Execution(e.ExecutionID)
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return x.WorkID == w.ID, nil
	}
	return false, nil
}

// Route evaluates detached handlers outside storage transactions. Optimistic
// revision validation serializes each Work without holding unrelated keys while
// a handler is blocked. An earlier accepted input is processed first.
//
// A route that fails returns a *RouteError. The failure is counted on the
// Event it was deciding, and once that Event may not be retried it is routed
// as failed (DeadLettered); an earlier input given up on this way no longer
// holds this one, which is routed in the same call.
func (r *Router) Route(ctx context.Context, id domain.ID) error {
	for {
		if err := r.backoff(ctx, id); err != nil {
			return err
		}
		err := r.route(ctx, id)
		var re *RouteError
		if !errors.As(err, &re) || ctx.Err() != nil {
			return err
		}
		if e := r.fail(ctx, re); e != nil {
			return e
		}
		if re.DeadLettered && re.EventID != id {
			continue
		}
		return re
	}
}

func (r *Router) route(ctx context.Context, id domain.ID) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var s routeSnapshot
		if err := r.transact(ctx, nil, func(tx storage.Tx) error { var e error; s, e = snapshot(tx, id); return e }); err != nil {
			return err
		}
		if s.event.Routed {
			return nil
		}
		if s.work != nil && s.work.State == domain.WorkWaiting {
			// The Wait decides whether the input wakes the Work. The routes
			// still say what the Work is, so its display follows the source,
			// and whether input that doesn't wake it reaches it at all: one
			// no route passes to the Work is recorded as unmatched, as it
			// would be were the Work not waiting, not kept as context.
			display, skip := r.waitingDecision(ctx, s)
			var handled bool
			err := r.transact(ctx, nil, func(tx storage.Tx) error {
				var e error
				handled, e = routeWaitingInTx(tx, r.now(), r.options.CausalLimits, id, skip)
				if e == nil && handled {
					e = putDisplayInTx(tx, *s.work, display)
				}
				return e
			})
			if err != nil {
				return err
			}
			if handled {
				return nil
			}
			continue
		}
		if s.work != nil {
			var prior domain.ID
			err := r.transact(ctx, nil, func(tx storage.Tx) error {
				for _, a := range s.events {
					if a.Sequence >= s.event.Sequence {
						break
					}
					if a.Routed {
						continue
					}
					target, e := potentiallyTargets(tx, a.Event, *s.work)
					if e != nil {
						return e
					}
					if !target {
						continue
					}
					w, e := routeOwner(tx, a.Event)
					if e != nil && !errors.Is(e, storage.ErrNotFound) {
						return e
					}
					if w.ID == s.work.ID {
						prior = a.Event.ID
						break
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if prior != "" {
				return storage.ErrConflict
			}
		}
		p := routePlan{event: s.event}
		var err error
		// Terminal ownership is final; late input cannot ask a handler to
		// mutate it or become runnable through a new matching route.
		if s.work == nil || !s.work.State.Terminal() {
			p, err = r.evaluate(ctx, s, nil)
			if err != nil {
				return err
			}
		}
		var projected *domain.Work
		if s.work != nil {
			w := *s.work
			w.CorrelationKeys = append([]domain.CorrelationKey(nil), w.CorrelationKeys...)
			projected = &w
		} else if p.selected != nil && (s.event.Event.CorrelationKey != nil || p.decision.CreateID != "") {
			w := domain.Work{ID: p.decision.CreateID, Kind: p.decision.Kind, ScopeID: s.scope.ID, State: domain.WorkReady, Context: p.decision.Context, Grants: slices.Clone(p.decision.Grants)}
			if w.ID == "" {
				w.ID = domain.ID("work/" + identity(string(id)))
			}
			if w.Kind == "" {
				return extension.ErrInvalid
			}
			if s.event.Event.CorrelationKey != nil {
				w.CorrelationKeys = []domain.CorrelationKey{*s.event.Event.CorrelationKey}
			}
			projected = &w
		}
		if projected != nil && p.selected != nil && !projected.State.Terminal() {
			appendKeys(projected, p.decision.AttachKeys)
			// Displays apply in evaluation order: earlier input this
			// Event's keys bring in is evaluated below, on top of it, so
			// each handler sees the display it replaces and nothing one
			// added is lost.
			setDisplay(projected, p.decision.Display)
		}
		plans := []routePlan{}
		// New attachments bring earlier unprocessed events into this atomic batch.
		// They see the projected ownership; checkpointed history is never replayed.
		if projected != nil && !projected.State.Terminal() {
			planned := map[domain.ID]bool{}
			for {
				changed := false
				for _, a := range s.events {
					if a.Sequence >= s.event.Sequence {
						break
					}
					if a.Routed || planned[a.Event.ID] || !eventTargets(a.Event, *projected) {
						continue
					}
					// Scope metadata is immutable; a key attachment cannot grant
					// authority over input accepted into another Scope.
					e := r.transact(ctx, nil, func(tx storage.Tx) error {
						scope, e := scopeForEvent(tx, a)
						if e != nil {
							return e
						}
						if scope != "" && scope != projected.ScopeID {
							return storage.ErrConflict
						}
						if a.Event.WorkID != "" && a.Event.WorkID != projected.ID {
							return storage.ErrInvalid
						}
						if a.Event.ExecutionID != "" {
							x, e := tx.Execution(a.Event.ExecutionID)
							if e != nil {
								return e
							}
							if x.WorkID != projected.ID {
								return storage.ErrInvalid
							}
						}
						return nil
					})
					if e != nil {
						return e
					}
					bs := s
					bs.event = a
					bs.work = projected
					bp, e := r.evaluate(ctx, bs, nil)
					if e != nil {
						return e
					}
					plans = append(plans, bp)
					planned[a.Event.ID] = true
					changed = true
					if bp.selected != nil {
						appendKeys(projected, bp.decision.AttachKeys)
						setDisplay(projected, bp.decision.Display)
					}
				}
				if !changed {
					break
				}
			}
			sort.Slice(plans, func(i, j int) bool { return plans[i].event.Sequence < plans[j].event.Sequence })
		}
		plans = append(plans, p)
		err = r.transact(ctx, nil, func(tx storage.Tx) error {
			fresh, e := snapshot(tx, id)
			if e != nil {
				return e
			}
			if fresh.event.Routed {
				return nil
			}
			if !reflect.DeepEqual(fresh.scopes, s.scopes) || !reflect.DeepEqual(fresh.work, s.work) {
				return errRouteRetry
			}
			if projected != nil {
				// Recheck all preexisting pending inputs, including execution-only targets.
				included := map[domain.ID]bool{}
				for _, planned := range plans {
					included[planned.event.Event.ID] = true
				}
				for _, a := range fresh.events {
					if a.Sequence >= s.event.Sequence {
						break
					}
					if a.Routed {
						continue
					}
					target, e := potentiallyTargets(tx, a.Event, *projected)
					if e != nil {
						return e
					}
					if !target {
						continue
					}
					w, e := routeOwner(tx, a.Event)
					// Unowned newly attached keys are validated after PutWork.
					if e != nil && !errors.Is(e, storage.ErrNotFound) {
						return e
					}
					if (w.ID == projected.ID || eventTargets(a.Event, *projected)) && !included[a.Event.ID] {
						return storage.ErrConflict
					}
				}
				if !projected.State.Terminal() {
					if e = tx.PutWork(*projected, projected.Revision); e != nil {
						return e
					}
				}
			}
			for _, planned := range plans {
				a, e := tx.Event(planned.event.Event.ID)
				if e != nil {
					return e
				}
				if a.Routed {
					return errRouteRetry
				}
				if projected != nil {
					scope, e := scopeForEvent(tx, a)
					if e != nil {
						return e
					}
					if scope != "" && scope != projected.ScopeID {
						return storage.ErrConflict
					}
					owner, e := routeOwner(tx, a.Event)
					if e != nil && !errors.Is(e, storage.ErrNotFound) {
						return e
					}
					if owner.ID != "" && owner.ID != projected.ID {
						return storage.ErrInvalid
					}
				}
				if e = r.apply(tx, planned, projected, s.scope); e != nil {
					return e
				}
			}
			return nil
		})
		if errors.Is(err, errRouteRetry) {
			continue
		}
		return err
	}
}
func (r *Router) apply(tx storage.Tx, p routePlan, w *domain.Work, scope domain.Scope) error {
	rec := RouteRecord{EventID: p.event.Event.ID, Generation: r.options.Generation, Policy: "unmatched", Reason: "no_match", DisplayRefused: p.refused}
	if p.selected != nil {
		rec.Route, rec.Binding = p.selected.Name, p.selected.Binding
	}
	if w != nil {
		rec.WorkID = w.ID
	}
	if w != nil && w.State.Terminal() {
		rec.Policy, rec.Reason = "terminal", "terminal_work"
	} else if p.selected != nil && w != nil {
		var target *domain.Execution
		var err error
		if rec.Policy, rec.Reason, target, err = r.policyInTx(tx, *p.selected, *w, scope); err != nil {
			// The running attempt can't be given this input (another
			// binding, or its pinned capabilities lack the control). It
			// isn't a route failure: the input waits, uncounted, until
			// the attempt ends and it queues.
			return err
		}
		if rec.Policy != "drop" {
			input := QueuedInput{p.event.Event.ID, p.event.Sequence, p.selected.Binding}
			if err := putJSON(tx, queueNamespace, queueKey(w.ID, p.event.Sequence), input); err != nil {
				return err
			}
			if rec.Policy == "inject" || rec.Policy == "interrupt" {
				intent := ControlIntent{identity(string(p.event.Event.ID), string(target.ID), fmt.Sprint(target.Lease.Epoch), rec.Policy), p.event.Event.ID, w.ID, target.ID, target.Lease.Epoch, rec.Policy, rec.Binding}
				if err := putJSON(tx, intentNamespace, intent.ID, intent); err != nil {
					return err
				}
			}
		}
	}
	if p.selected == nil && !(w != nil && w.State.Terminal()) {
		rec.WorkID = ""
	}
	if err := putJSON(tx, routeNamespace, identity(string(rec.EventID)), rec); err != nil {
		return err
	}
	if err := tx.Delete(routeFailureNamespace, routeFailureKey(rec.EventID)); err != nil {
		return err
	}
	return tx.MarkRouted(rec.EventID, rec.WorkID, rec.Policy)
}

// policyInTx decides, without writing, what a selected route does with input
// for nonterminal Work w: the effective policy, its reason, and the running
// attempt an inject or interrupt targets. Routing records it; a route trace
// reports it.
func (r *Router) policyInTx(tx storage.Tx, route domain.Route, w domain.Work, scope domain.Scope) (policy, reason string, target *domain.Execution, err error) {
	policy, reason = route.ActivePolicy, "matched"
	if policy == "" {
		policy = "queue"
	}
	b, err := r.binding(scope, route.Binding)
	if err != nil {
		return "", "", nil, err
	}
	if policy == "inject" && !b.Capabilities.Inject || policy == "interrupt" && !b.Capabilities.Interrupt {
		policy, reason = b.Fallback, "unsupported_control"
	}
	if w.State != domain.WorkRunning {
		policy = "queue"
	}
	if policy != "inject" && policy != "interrupt" {
		return policy, reason, nil, nil
	}
	xs, err := tx.ListExecutions(w.ID)
	if err != nil {
		return "", "", nil, err
	}
	for i := range xs {
		if xs[i].State == domain.ExecutionRunning {
			target = &xs[i]
		}
	}
	if target == nil {
		return "", "", nil, storage.ErrConflict
	}
	pinned, pinErr := attemptState(tx, target.ID)
	if pinErr != nil && !errors.Is(pinErr, storage.ErrNotFound) {
		return "", "", nil, pinErr
	}
	if pinErr == nil && (policy == "inject" && !pinned.Capabilities.Inject || policy == "interrupt" && !pinned.Capabilities.Interrupt) {
		if b.Fallback == "" {
			return "", "", nil, extension.ErrUnsupported
		}
		policy, reason = b.Fallback, "target_control_unsupported"
	}
	if target.ExecutorBinding != route.Binding {
		// The old attempt retains its original binding. Config/attempt
		// generation pinning is owned by the scheduler, never inferred
		// from a new route's control-capable executor registration.
		if b.Fallback == "" {
			return "", "", nil, extension.ErrUnsupported
		}
		policy, reason = b.Fallback, "target_binding_mismatch"
	}
	return policy, reason, target, nil
}
func queueKey(work domain.ID, seq uint64) string {
	return identity(string(work)) + fmt.Sprintf("/%020d", seq)
}
func pendingInputs(tx storage.Tx, work domain.ID) (out []QueuedInput, err error) {
	entries, e := tx.List(queueNamespace, identity(string(work))+"/")
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		var input QueuedInput
		if e = getJSON(tx, queueNamespace, entry.Key, &input); e != nil {
			return nil, e
		}
		out = append(out, input)
	}
	return out, nil
}
func (r *Router) PendingInputs(ctx context.Context, work domain.ID) (out []QueuedInput, err error) {
	err = r.transact(ctx, nil, func(tx storage.Tx) error { var e error; out, e = pendingInputs(tx, work); return e })
	return
}
func (r *Router) Intents(ctx context.Context, work domain.ID) (out []ControlIntent, err error) {
	err = r.transact(ctx, nil, func(tx storage.Tx) error {
		entries, e := tx.List(intentNamespace, "")
		if e != nil {
			return e
		}
		for _, entry := range entries {
			var intent ControlIntent
			if e = getJSON(tx, intentNamespace, entry.Key, &intent); e != nil {
				return e
			}
			if work == "" || intent.WorkID == work {
				out = append(out, intent)
			}
		}
		return nil
	})
	return
}

// RouteDecisionInTx reads the durable routing decision recorded for an
// accepted Event (operator route explanation). ok is false while the Event
// is unrouted or matched no route.
func RouteDecisionInTx(tx storage.Tx, event domain.ID) (RouteRecord, bool, error) {
	var out RouteRecord
	err := getJSON(tx, routeNamespace, identity(string(event)), &out)
	if errors.Is(err, storage.ErrNotFound) {
		return out, false, nil
	}
	return out, err == nil, err
}

func (r *Router) Decision(ctx context.Context, event domain.ID) (out RouteRecord, err error) {
	err = r.transact(ctx, nil, func(tx storage.Tx) error { return getJSON(tx, routeNamespace, identity(string(event)), &out) })
	return
}

// ConsumeInputsInTx joins claiming with durable consumed-input history. The
// caller must apply its trusted scheduler/lease guard to the outer transaction.
func ConsumeInputsInTx(tx storage.Tx, work, execution domain.ID) (inputs []QueuedInput, err error) {
	x, e := tx.Execution(execution)
	if e != nil {
		return nil, e
	}
	if x.WorkID != work {
		return nil, storage.ErrInvalid
	}
	inputs, e = pendingInputs(tx, work)
	if e != nil {
		return nil, e
	}
	for _, input := range inputs {
		key := queueKey(work, input.Sequence)
		if e = putJSON(tx, consumedNamespace, key+"/"+identity(string(execution)), struct {
			Input       QueuedInput
			ExecutionID domain.ID
		}{input, execution}); e != nil {
			return nil, e
		}
		if e = tx.Delete(queueNamespace, key); e != nil {
			return nil, e
		}
	}
	return inputs, nil
}
