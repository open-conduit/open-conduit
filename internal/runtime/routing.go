package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
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
	// Refusals is, for an Event no route selected (Policy "unmatched"),
	// each route that declined it and why, in route order.
	Refusals []RouteRefusal `json:",omitempty"`
}

// RouteRefusal is one route declining an Event: the route's name and the
// reason its handler gave (extension.Skip), cut to MaxRefusalReason bytes.
type RouteRefusal struct {
	Route, Reason string
}

// MaxRefusalReason bounds each recorded refusal reason, in bytes.
const MaxRefusalReason = 500

// ReasonAddressed is a RouteRecord's reason when no route took an operator
// Event and it was queued for the Work it named (addressedBindingInTx).
const ReasonAddressed = "addressed"

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
	// Operator marks an operator's interrupt (Control.InterruptWork): its
	// Event is a control, not input, and a failed dispatch never takes the
	// binding's control_error_fallback.
	Operator bool `json:",omitempty"`
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
		if err == nil && result.ID == "" {
			// A key alone names the Work its owner's successors lead to;
			// an explicit Work or Execution names that Work and no other.
			w, err = latestSuccessor(tx, w)
		}
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

// successorNamespace maps a finished Work to the Work a route opened to
// succeed it (RouteDecision.Successor), keyed by the predecessor's
// identity.
const successorNamespace = "core.routing.successor"

// predecessorNamespace is successorNamespace reversed: a successor to the
// finished Work it succeeds.
const predecessorNamespace = "core.routing.predecessor"

// inheritedKeysInTx are the keys that route to w from the Work it
// succeeds, and that one's, and so on back: keys w doesn't own.
func inheritedKeysInTx(tx storage.Tx, w domain.Work) ([]domain.CorrelationKey, error) {
	var keys []domain.CorrelationKey
	for range maxSuccessors {
		var prev domain.ID
		err := getJSON(tx, predecessorNamespace, identity(string(w.ID)), &prev)
		if errors.Is(err, storage.ErrNotFound) {
			return keys, nil
		}
		if err != nil {
			return nil, err
		}
		if w, err = tx.Work(prev); err != nil {
			return nil, err
		}
		keys = append(keys, w.CorrelationKeys...)
	}
	return nil, storage.ErrInvalid
}

// asSuccessorInput is a as successor w's Waits see it. A Wait subscribes on
// keys its Work owns, and input reaching w through a key of the Work it
// succeeds (inherited) carries that key, so to its Waits it carries w's
// generation key, the first key w owns.
func asSuccessorInput(inherited []domain.CorrelationKey, w domain.Work, a domain.AcceptedEvent) domain.AcceptedEvent {
	if a.Event.CorrelationKey == nil || len(w.CorrelationKeys) == 0 || !slices.Contains(inherited, *a.Event.CorrelationKey) || a.Event.WorkID != "" || a.Event.ExecutionID != "" {
		return a
	}
	k := w.CorrelationKeys[0]
	a.Event.CorrelationKey = &k
	return a
}

// maxSuccessors bounds a chain of successors routeOwner follows.
const maxSuccessors = 10000

// latestSuccessor follows finished Work to its successor, and that one's,
// to the last of the chain: the Work its keys route to.
func latestSuccessor(tx storage.Tx, w domain.Work) (domain.Work, error) {
	for range maxSuccessors {
		if !w.State.Terminal() {
			return w, nil
		}
		var next domain.ID
		err := getJSON(tx, successorNamespace, identity(string(w.ID)), &next)
		if errors.Is(err, storage.ErrNotFound) {
			return w, nil
		}
		if err != nil {
			return w, err
		}
		if w, err = tx.Work(next); err != nil {
			return w, err
		}
	}
	return w, storage.ErrInvalid
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
	// addressed is the binding an operator Event naming this Work is input
	// on when no route takes it (addressedBindingInTx), or "".
	addressed string
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
	if s.addressed, err = addressedBindingInTx(tx, s.event.Event, s.work); err != nil {
		return
	}
	s.events, err = tx.UnroutedEvents(0, 0)
	return
}

// addressedEvent reports whether e is input for the Work it names without
// a route: an operator's Event (not a core type), or a child's end or
// report.
func addressedEvent(e domain.Event) bool {
	if e.Source == ManualSource {
		return !strings.HasPrefix(e.Type, "core.")
	}
	return e.Source == domain.LineageSource && domain.IsChildNews(e.Type)
}

// rebindNamespace holds each Work's last operator rebind (Control.Rebind),
// so input addressed to it later follows the rebind even when nothing was
// queued or held for retry when it was rebound.
const rebindNamespace = "core.routing.rebind"

// rebound is a Work's last rebind: its binding, and the number of the
// Work's latest attempt then (0 before the first), so a later attempt is
// told apart without a clock.
type rebound struct {
	Binding string
	After   uint32
}

// latestAttemptInTx is the Work's attempt with the highest number, or nil.
func latestAttemptInTx(tx storage.Tx, work domain.ID) (*domain.Execution, error) {
	xs, err := tx.ListExecutions(work)
	if err != nil {
		return nil, err
	}
	var latest *domain.Execution
	for i := range xs {
		if latest == nil || xs[i].Attempt > latest.Attempt {
			latest = &xs[i]
		}
	}
	return latest, nil
}

// addressedBindingInTx is the binding on which an operator Event that names
// Work w directly (its WorkID) is that Work's input when no route takes it:
// the binding the Work runs on next (workBindingInTx). The operator named
// the Work, so the input needs no route to find it,
// and it continues on the binding the Work already runs on. Work created
// without a route (Control.CreateWork) has no route to take follow-up input
// at all. A child's domain.TypeChildEnded and domain.TypeChildReported
// Events are addressed to its parent the same way: the parent asked for
// the child, so its result needs no
// route. Any other Event, Work that has ended, and Work with no binding to
// continue on get "", and routing is as usual.
func addressedBindingInTx(tx storage.Tx, e domain.Event, w *domain.Work) (string, error) {
	if w == nil || w.State.Terminal() || e.WorkID != w.ID || !addressedEvent(e) {
		return "", nil
	}
	return workBindingInTx(tx, *w)
}

// workBindingInTx is the binding Work w runs on next: its queued input's,
// else its retry state's (both of which Control.Rebind rewrites), else the
// newer of its last rebind and its latest attempt's, or "" for Work that has
// none yet.
func workBindingInTx(tx storage.Tx, w domain.Work) (string, error) {
	inputs, err := pendingInputs(tx, w.ID)
	if err != nil {
		return "", err
	}
	for _, in := range inputs {
		if in.Binding != "" {
			return in.Binding, nil
		}
	}
	if state, err := retryState(tx, w.ID); err == nil && state.Binding != "" {
		return state.Binding, nil
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", err
	}
	var last rebound
	if err := getJSON(tx, rebindNamespace, identity(string(w.ID)), &last); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return "", err
	}
	latest, err := latestAttemptInTx(tx, w.ID)
	if err != nil {
		return "", err
	}
	if latest != nil && (last.Binding == "" || latest.Attempt > last.After) {
		return latest.ExecutorBinding, nil
	}
	return last.Binding, nil
}

// predecessorBindingInTx is the binding the Work w succeeds ran on, or
// that one's predecessor's when it never ran, and so on back: "" when w
// succeeds nothing or no generation before it ran.
func predecessorBindingInTx(tx storage.Tx, w domain.ID) (string, error) {
	for range maxSuccessors {
		var prev domain.ID
		err := getJSON(tx, predecessorNamespace, identity(string(w)), &prev)
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		pw, err := tx.Work(prev)
		if err != nil {
			return "", err
		}
		if b, err := workBindingInTx(tx, pw); err != nil || b != "" {
			return b, err
		}
		w = prev
	}
	return "", storage.ErrInvalid
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
	// refusals are the routes tried before one matched, or all of them
	// when none did, with each handler's reason.
	refusals []RouteRefusal
	// addressed says no route took the Event and it is its Work's input
	// because the operator addressed it there; selected then names no
	// route, only the Work's binding.
	addressed bool
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
		if !d.Match {
			p.refusals = append(p.refusals, RouteRefusal{Route: route.Name, Reason: extension.Clip(d.Reason, MaxRefusalReason)})
		}
		if d.Match && !d.Deliver.Valid() {
			return p, &RouteError{EventID: id, Route: route.Name, Code: RouteHandlerInvalid, Err: fmt.Errorf("%w: unknown delivery %q", extension.ErrInvalid, d.Deliver)}
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
	if p.selected == nil && s.addressed != "" {
		// A binding config has since removed leaves the Event unmatched.
		if _, err := r.binding(s.scope, s.addressed); err == nil {
			p.selected, p.addressed = &domain.Route{Binding: s.addressed}, true
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
		var wp routePlan
		var werr error
		attachOnly := false
		if s.work != nil && s.work.State == domain.WorkWaiting {
			// The Wait decides whether the input wakes the Work. The routes
			// still say what the Work is, so its display follows the source,
			// and whether input that doesn't wake it reaches it at all: one
			// no route passes to the Work is recorded as unmatched, as it
			// would be were the Work not waiting, not kept as context.
			// A match that only attaches keys (DeliverNone) isn't input, so
			// the Wait has nothing to decide: it is routed below as for
			// Work that isn't waiting.
			wp, werr = r.evaluate(ctx, s, nil)
			attachOnly = werr == nil && wp.selected != nil && wp.decision.Deliver == extension.DeliverNone
		}
		if s.work != nil && s.work.State == domain.WorkWaiting && !attachOnly {
			display, skip := r.waitingDecision(s, wp, werr)
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
		finished := s.work != nil && s.work.State.Terminal()
		p, err := wp, werr
		if !attachOnly {
			p, err = r.evaluate(ctx, s, nil)
		}
		if err != nil && !finished {
			return err
		}
		// Terminal ownership is final: late input never mutates finished
		// Work or makes it runnable. Only a route asking for a successor
		// acts on it, by opening new Work; a successor's first input is
		// the Event, so a match that only attaches never opens one.
		var predecessor *domain.Work
		if finished {
			// A route that can't be evaluated asks for nothing: the input is
			// terminal, as it always was for finished Work.
			if err == nil && p.selected != nil && !p.addressed && p.decision.Successor && p.decision.Deliver != extension.DeliverNone {
				predecessor = s.work
			} else {
				p = routePlan{event: s.event}
			}
		}
		var projected *domain.Work
		if predecessor != nil {
			w, e := successorWork(id, *predecessor, p.decision)
			if e != nil {
				return e
			}
			projected = &w
		} else if s.work != nil {
			w := *s.work
			w.CorrelationKeys = append([]domain.CorrelationKey(nil), w.CorrelationKeys...)
			projected = &w
		} else if p.selected != nil && p.decision.Deliver != extension.DeliverNone && (s.event.Event.CorrelationKey != nil || p.decision.CreateID != "") {
			// A match that only attaches needs Work to attach to; it
			// creates none.
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
		// Waiting Work's input is the Wait's to decide, so an attachment to
		// it brings nothing in: earlier input on the new keys conflicts
		// below and is routed on its own first.
		if projected != nil && !projected.State.Terminal() && projected.State != domain.WorkWaiting {
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
					var addressed string
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
						addressed, e = addressedBindingInTx(tx, a.Event, projected)
						return e
					})
					if e != nil {
						return e
					}
					bs := s
					bs.event = a
					bs.work = projected
					bs.addressed = addressed
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
				if predecessor != nil {
					if e = r.openSuccessorInTx(tx, *predecessor, *projected, s.event.Event.ID); e != nil {
						return e
					}
				} else if !projected.State.Terminal() {
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

// successorWork is the Work a route's Successor decision opens for the
// Event id, whose owner, predecessor, has finished: new, ready Work in the
// predecessor's scope with the decision's identity, kind and keys. The
// Event's own key stays with the predecessor, which routes it on.
func successorWork(id domain.ID, predecessor domain.Work, d extension.RouteDecision) (domain.Work, error) {
	if d.Kind == "" {
		return domain.Work{}, extension.ErrInvalid
	}
	w := domain.Work{ID: d.CreateID, Kind: d.Kind, ScopeID: predecessor.ScopeID, State: domain.WorkReady, Context: d.Context, Grants: slices.Clone(d.Grants)}
	if w.ID == "" {
		w.ID = domain.ID("work/" + identity(string(id)))
	}
	// A successor owns a key of its own, its generation key (B04): its
	// Waits subscribe on it.
	if w.ID == predecessor.ID || len(d.AttachKeys) == 0 {
		return domain.Work{}, extension.ErrInvalid
	}
	for _, k := range d.AttachKeys {
		if containsKey(predecessor, k) {
			return domain.Work{}, extension.ErrInvalid
		}
	}
	appendKeys(&w, d.AttachKeys)
	setDisplay(&w, d.Display)
	return w, nil
}

// openSuccessorInTx stores w, the successor of finished Work predecessor,
// so predecessor's keys route to it from now on, and records the link as
// a core.work.created Event caused by the Event that asked for it.
func (r *Router) openSuccessorInTx(tx storage.Tx, predecessor, w domain.Work, trigger domain.ID) error {
	var existing domain.ID
	if err := getJSON(tx, successorNamespace, identity(string(predecessor.ID)), &existing); err == nil {
		return errRouteRetry
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if _, err := tx.Work(w.ID); err == nil {
		return storage.ErrConflict
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := tx.PutWork(w, 0); err != nil {
		return err
	}
	if err := putJSON(tx, successorNamespace, identity(string(predecessor.ID)), w.ID); err != nil {
		return err
	}
	if err := putJSON(tx, predecessorNamespace, identity(string(w.ID)), predecessor.ID); err != nil {
		return err
	}
	data, err := json.Marshal(struct{ WorkID, ParentID, PredecessorID domain.ID }{w.ID, "", predecessor.ID})
	if err != nil {
		return err
	}
	now := r.now()
	created := domain.Event{ID: domain.ID("work-created/" + identity(string(w.ID))), Type: "core.work.created", Source: "core.work", DeliveryID: "created/" + string(w.ID),
		OccurredAt: now, ReceivedAt: now, CausationID: trigger, WorkID: w.ID, Data: domain.Payload{Schema: "core.work.created.v1", Data: data}}
	_, _, err = AcceptDerivedInTx(tx, now, r.options.CausalLimits, w.ScopeID, trigger, created)
	return err
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
	} else if p.selected != nil && p.decision.Deliver == extension.DeliverNone {
		// Only the keys were for the Work; the Event is in its history
		// but is not its input.
		rec.Policy, rec.Reason = extension.DispositionAttach, "attached"
		if w == nil {
			rec.Policy, rec.Reason = "unmatched", "nothing_to_attach"
		}
	} else if p.selected != nil && w != nil {
		var target *domain.Execution
		var err error
		route, err := r.deliveryRouteInTx(tx, p, *w, scope)
		if err != nil {
			return err
		}
		rec.Binding = route.Binding
		if rec.Policy, rec.Reason, target, err = r.policyInTx(tx, route, *w, scope); err != nil {
			// The running attempt can't be given this input (another
			// binding, or its pinned capabilities lack the control). It
			// isn't a route failure: the input waits, uncounted, until
			// the attempt ends and it queues.
			return err
		}
		if p.addressed {
			rec.Reason = ReasonAddressed
		}
		if rec.Policy != "drop" {
			input := QueuedInput{p.event.Event.ID, p.event.Sequence, route.Binding}
			if err := putJSON(tx, queueNamespace, queueKey(w.ID, p.event.Sequence), input); err != nil {
				return err
			}
			if rec.Policy == "inject" || rec.Policy == "interrupt" {
				intent := ControlIntent{ID: identity(string(p.event.Event.ID), string(target.ID), fmt.Sprint(target.Lease.Epoch), rec.Policy), EventID: p.event.Event.ID, WorkID: w.ID, ExecutionID: target.ID, Epoch: target.Lease.Epoch, Policy: rec.Policy, Binding: rec.Binding}
				if err := putJSON(tx, intentNamespace, intent.ID, intent); err != nil {
					return err
				}
			}
		}
	}
	if p.selected == nil && !(w != nil && w.State.Terminal()) {
		rec.WorkID = ""
		// Nothing else reports why no route took the Event, so keep what
		// each handler said.
		rec.Refusals = p.refusals
	}
	if err := putJSON(tx, routeNamespace, identity(string(rec.EventID)), rec); err != nil {
		return err
	}
	if err := tx.Delete(routeFailureNamespace, routeFailureKey(rec.EventID)); err != nil {
		return err
	}
	return tx.MarkRouted(rec.EventID, rec.WorkID, rec.Policy)
}

// deliveryRouteInTx is the route p's input is delivered on for Work w: the
// selected route, on the binding the Work runs on when the decision asks for
// it (DeliverCurrent) and the Work has one that is still configured.
func (r *Router) deliveryRouteInTx(tx storage.Tx, p routePlan, w domain.Work, scope domain.Scope) (domain.Route, error) {
	route := *p.selected
	if p.decision.Deliver != extension.DeliverCurrent {
		return route, nil
	}
	current, err := workBindingInTx(tx, w)
	if err == nil && current == "" {
		// A successor that hasn't run yet carries on its predecessor's
		// Work, so it runs where its predecessor did.
		current, err = predecessorBindingInTx(tx, w.ID)
	}
	if err != nil || current == "" || current == route.Binding {
		return route, err
	}
	if _, err := r.binding(scope, current); err != nil {
		// A binding config has since removed: the route's own takes it.
		return route, nil
	}
	route.Binding = current
	return route, nil
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
