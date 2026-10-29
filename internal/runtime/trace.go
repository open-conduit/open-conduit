package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Route trace verdicts.
const (
	VerdictMatch      = "match"
	VerdictSkip       = "skip"
	VerdictNotReached = "not_reached"
	// VerdictBypassed: the route was not consulted (the Work is waiting
	// or terminal).
	VerdictBypassed = "bypassed"
)

// Route trace effects.
const (
	EffectStart     = "start"
	EffectQueue     = "queue"
	EffectInject    = "inject"
	EffectInterrupt = "interrupt"
	EffectWake      = "wake"
	EffectDrop      = "drop"
	EffectUnmatched = "unmatched"
)

// RouteTrace is a dry run of routing for a hypothetical Event against the
// active generation and current Work state. Producing it accepts, routes and
// records nothing.
type RouteTrace struct {
	Generation string
	ScopeID    domain.ID
	// Work is the Work the Event resolves to before routing, if any.
	Work   *domain.Work
	Routes []RouteStep
	Effect RouteEffect
	// Also lists other wakes: Work woken through a dependency
	// subscription, or another input waking the Event's own Work first.
	Also []RouteEffect
}

// RouteStep is one effective route in order with the verdict on the Event.
type RouteStep struct {
	Route, Handler, Binding, Verdict, Reason string
}

// RouteEffect is what routing would do: Kind is one of the Effect constants.
type RouteEffect struct {
	Kind    string
	WorkID  domain.ID
	Binding string
	Detail  string
}

// TraceSample is a handler's named sample Event for one route of a scope.
type TraceSample struct {
	Route, Handler string
	extension.RouteSample
}

// traceWait is what an Event would do to a waiting Work's active wait,
// decided by the same winner selection WakeWaitInTx uses.
type traceWait struct {
	wait domain.Wait
	meta waitMetadata
	work domain.Work
	// wakes: the Event itself wakes the Work. first: another input (an
	// earlier accepted Event or the due timeout) wakes it first. refused:
	// the causal limits would refuse the wake's core.wait.woken bridge
	// ("depth_limit"), so the wait would be blocked instead.
	wakes   bool
	first   string
	refused string
}

// Trace explains how the router would route e in scopeID now. It reads the
// state routing reads and calls the same handlers, winner selection and
// policy decision; it never writes. filter evaluates a subscription's opaque
// filter (nil: a filtered subscription never matches, as in the scheduler).
func (r *Router) Trace(ctx context.Context, scopeID domain.ID, e domain.Event, filter func(string, domain.Payload, domain.Event) bool) (RouteTrace, error) {
	out := RouteTrace{Generation: r.options.Generation}
	if scopeID == "" || e.Type == "" {
		return out, storage.ErrInvalid
	}
	var s routeSnapshot
	var waiting *traceWait
	var wakes []traceWait
	err := r.transact(ctx, nil, func(tx storage.Tx) error {
		scope, err := resolveScope(tx, e, scopeID)
		if err != nil {
			return err
		}
		if err = scopeSnapshot(tx, scope, &s); err != nil {
			return err
		}
		latest, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		s.event = domain.AcceptedEvent{Event: e, Sequence: latest + 1}
		w, err := routeOwner(tx, e)
		if err == nil {
			s.work = &w
			if w.State == domain.WorkWaiting {
				if waiting, err = r.waitOutcomeInTx(tx, w, s.event); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		wakes, err = r.subscriptionWakes(tx, s.scope.ID, s.event, filter)
		return err
	})
	if err != nil {
		return out, err
	}
	out.ScopeID = s.scope.ID
	out.Work = s.work
	for _, route := range s.routes {
		out.Routes = append(out.Routes, RouteStep{Route: route.Name, Handler: route.Handler, Binding: route.Binding, Verdict: VerdictNotReached})
	}
	for _, w := range wakes {
		if s.work == nil || w.work.ID != s.work.ID {
			out.Also = append(out.Also, wakeEffect(w, "through its dependency on this event"))
		}
	}
	if waiting != nil && waiting.first == "" {
		// Routes are not consulted for waiting Work: its wait decides.
		for i := range out.Routes {
			out.Routes[i].Verdict, out.Routes[i].Reason = VerdictBypassed, string(waiting.work.ID)+" is waiting; its wait decides, routes are not consulted"
		}
		if waiting.wakes {
			out.Effect = wakeEffect(*waiting, "")
		} else {
			out.Effect = RouteEffect{Kind: EffectQueue, WorkID: waiting.work.ID, Binding: waiting.meta.Binding, Detail: "kept as context; " + string(waiting.work.ID) + " keeps waiting" + waitingFor(waiting.wait) + contextWhy(*waiting)}
		}
		return out, nil
	}
	if waiting != nil {
		// Something else wakes it first; this Event then routes to the
		// ready Work like any other input.
		out.Also = append([]RouteEffect{{Kind: EffectWake, WorkID: waiting.work.ID, Binding: waiting.meta.Binding, Detail: waiting.first + " wakes " + string(waiting.work.ID) + " first"}}, out.Also...)
		w := *s.work
		w.State = domain.WorkReady
		s.work = &w
	}
	if s.work != nil && s.work.State.Terminal() {
		for i := range out.Routes {
			out.Routes[i].Verdict, out.Routes[i].Reason = VerdictBypassed, string(s.work.ID)+" is "+string(s.work.State)+"; later input is never routed"
		}
		out.Effect = RouteEffect{Kind: EffectDrop, WorkID: s.work.ID, Detail: string(s.work.ID) + " is " + string(s.work.State) + "; the event is recorded as terminal"}
		return out, nil
	}
	i := 0
	p, err := r.evaluate(ctx, s, func(route domain.Route, d extension.RouteDecision) {
		for out.Routes[i].Route != route.Name {
			i++
		}
		out.Routes[i].Verdict, out.Routes[i].Reason = VerdictSkip, d.Reason
		if d.Match {
			out.Routes[i].Verdict = VerdictMatch
		}
		if out.Routes[i].Reason == "" {
			out.Routes[i].Reason = map[bool]string{true: "matched", false: "no match"}[d.Match]
		}
	})
	if err != nil {
		return out, err
	}
	if p.selected == nil {
		if s.work == nil && len(out.Also) > 0 {
			// The Event's only consequence is waking other Work.
			out.Effect, out.Also = out.Also[0], out.Also[1:]
			return out, nil
		}
		out.Effect = RouteEffect{Kind: EffectUnmatched, Detail: "no route matched; the event is recorded as unmatched"}
		return out, nil
	}
	route := *p.selected
	if s.work == nil {
		if e.CorrelationKey == nil && p.decision.CreateID == "" {
			out.Effect = RouteEffect{Kind: EffectUnmatched, Detail: route.Name + " matched, but the event has no correlation key for new Work to own"}
			return out, nil
		}
		if p.decision.Kind == "" {
			return out, extension.ErrInvalid
		}
		detail := "starts new " + p.decision.Kind + " Work"
		if p.decision.CreateID != "" {
			detail += " " + string(p.decision.CreateID)
		}
		if e.CorrelationKey != nil {
			detail += " owning " + e.CorrelationKey.ExternalID
		}
		out.Effect = RouteEffect{Kind: EffectStart, WorkID: p.decision.CreateID, Binding: route.Binding, Detail: detail + " on " + route.Binding}
		return out, nil
	}
	w := *s.work
	var policy, reason string
	var target *domain.Execution
	if err = r.transact(ctx, nil, func(tx storage.Tx) error {
		var e error
		policy, reason, target, e = r.policyInTx(tx, route, w, s.scope)
		return e
	}); err != nil {
		return out, err
	}
	out.Effect = RouteEffect{Kind: policy, WorkID: w.ID, Binding: route.Binding}
	switch policy {
	case EffectDrop:
		out.Effect.Detail = route.Name + " drops it while " + string(w.ID) + " is running"
	case EffectInject, EffectInterrupt:
		out.Effect.Detail = policy + " into " + string(w.ID) + "'s running attempt " + string(target.ID)
	default:
		out.Effect.Detail = "queued for " + string(w.ID) + "'s next turn (it is " + string(w.State) + ")"
	}
	if why, ok := fallbackWhy[reason]; ok {
		out.Effect.Detail += "; " + why + ", so the route's " + policy + " fallback applies"
	}
	return out, nil
}

var fallbackWhy = map[string]string{
	"unsupported_control":        "the binding's executor cannot do the route's active policy",
	"target_control_unsupported": "the running attempt's pinned executor cannot do the route's active policy",
	"target_binding_mismatch":    "the running attempt is on another binding",
}

func contextWhy(w traceWait) string {
	switch {
	case w.meta.Blocked:
		return "; its wake is blocked by the causal limits"
	case w.refused != "":
		return "; the causal limits would refuse its wake (" + w.refused + "), which blocks the wait"
	}
	return "; the event does not match its wait"
}

// waitOutcomeInTx decides, without writing, what a wakes Work w's active wait:
// WakeWaitInTx's winner selection with a appended as the newest input, then
// its due timeout. Routing refuses a waiting Work with no active wait.
func (r *Router) waitOutcomeInTx(tx storage.Tx, w domain.Work, a domain.AcceptedEvent) (*traceWait, error) {
	waits, err := tx.ListWaits(w.ID)
	if err != nil {
		return nil, err
	}
	var out *traceWait
	for _, wait := range waits {
		if wait.State == domain.WaitActive {
			out = &traceWait{wait: wait, work: w}
			break
		}
	}
	if out == nil {
		return nil, storage.ErrInvalid
	}
	if err = getJSON(tx, waitNamespace, identity(string(out.wait.ID)), &out.meta); err != nil {
		return nil, err
	}
	if out.meta.Blocked {
		return out, nil
	}
	all, err := waitCandidatesInTx(tx, out.wait, out.meta)
	if err != nil {
		return nil, err
	}
	winner, err := waitWinnerInTx(tx, out.wait, out.meta, w, append(all, a))
	if err != nil {
		return nil, err
	}
	switch {
	case winner.Event.ID == a.Event.ID && winner.Sequence == a.Sequence:
		out.wakes = true
	case winner.Event.ID != "":
		out.first = "accepted event " + string(winner.Event.ID)
	case out.meta.Timer.ID != "" && !r.now().Before(out.meta.Timer.FireAt):
		out.first = "its due timeout"
		return out, nil
	default:
		return out, nil
	}
	// A wake of a parked attempt derives a bridge the causal limits may
	// refuse, which blocks the wait rather than waking it (WakeWaitInTx).
	if out.refused, err = wakeRefusalInTx(tx, r.now(), r.options.CausalLimits, out.wait, winner.Event.ID, !out.wakes); err != nil {
		return nil, err
	}
	if out.refused != "" {
		out.wakes, out.first = false, ""
	}
	return out, nil
}

// subscriptionWakes finds the waiting Work that active dependency
// subscriptions on a, accepted in scope, would wake: the alias they would
// publish wins that Work's wait.
func (r *Router) subscriptionWakes(tx storage.Tx, scope domain.ID, a domain.AcceptedEvent, filter func(string, domain.Payload, domain.Event) bool) ([]traceWait, error) {
	if a.Event.CorrelationKey == nil {
		return nil, nil
	}
	rows, err := tx.List(subscriptionNamespace, "")
	if err != nil {
		return nil, err
	}
	var out []traceWait
	seen := map[domain.ID]bool{}
	for _, row := range rows {
		var sub subscription
		if err = json.Unmarshal(row.Value, &sub); err != nil {
			return nil, err
		}
		// The subscription reads only its own source scope's input.
		if !sub.Active || seen[sub.WorkID] || sub.Request.SourceScopeID != scope || sub.Request.SourceKey != *a.Event.CorrelationKey {
			continue
		}
		if len(sub.Request.Types) > 0 && !contains(sub.Request.Types, a.Event.Type) {
			continue
		}
		if sub.Request.Filter != nil && (filter == nil || !filter(sub.Request.Namespace, *sub.Request.Filter, a.Event)) {
			continue
		}
		w, err := tx.Work(sub.WorkID)
		if err != nil {
			return nil, err
		}
		if w.State != domain.WorkWaiting {
			continue
		}
		key := sub.Request.TargetKey
		alias := a
		alias.Event.ID = domain.ID("alias/" + identity(sub.Request.Namespace, sub.Request.ID, string(a.Event.ID)))
		alias.Event.Source, alias.Event.Type, alias.Event.WorkID, alias.Event.CorrelationKey = "core.subscription", sub.Request.AliasType, sub.WorkID, &key
		wait, err := r.waitOutcomeInTx(tx, w, alias)
		if err != nil {
			return nil, err
		}
		if wait.wait.ID != sub.Request.WaitID || !wait.wakes {
			continue
		}
		seen[w.ID] = true
		out = append(out, *wait)
	}
	return out, nil
}

func wakeEffect(w traceWait, how string) RouteEffect {
	detail := "wakes " + string(w.work.ID) + " in scope " + string(w.work.ScopeID)
	if how != "" {
		detail += " " + how
	}
	return RouteEffect{Kind: EffectWake, WorkID: w.work.ID, Binding: w.meta.Binding, Detail: detail + waitingFor(w.wait) + "; it resumes on " + w.meta.Binding}
}

func waitingFor(w domain.Wait) string {
	var keys []string
	for _, k := range w.Match.Keys {
		keys = append(keys, k.ExternalID)
	}
	out := " (wait " + string(w.ID)
	if len(keys) > 0 {
		out += " on " + strings.Join(keys, ", ")
	}
	if len(w.Match.Types) > 0 {
		out += " for " + strings.Join(w.Match.Types, ", ")
	}
	return out + ")"
}

// Samples lists the sample Events the scope's route handlers offer, in route
// order, each sample once per handler. Samplers are given the router's clock.
func (r *Router) Samples(ctx context.Context, scopeID domain.ID) ([]TraceSample, error) {
	var s routeSnapshot
	if err := r.transact(ctx, nil, func(tx storage.Tx) error { return scopeSnapshot(tx, scopeID, &s) }); err != nil {
		return nil, err
	}
	out := []TraceSample{}
	seen := map[string]bool{}
	for _, route := range s.routes {
		sampler, ok := r.options.Handlers[route.Handler].(extension.RouteSampler)
		if !ok {
			continue
		}
		in := extension.SampleInput{Scope: detachedInput(s).Scope, Route: route, Now: r.now()}
		for _, sample := range sampler.Samples(ctx, in) {
			id := route.Handler + "\x00" + sample.ID
			if sample.ID == "" || sample.Event.Type == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, TraceSample{Route: route.Name, Handler: route.Handler, RouteSample: sample})
		}
	}
	return out, nil
}

// CheckTraceEvent refuses a hypothetical Event whose envelope acceptance
// would refuse from any source: a type or payload schema that is not
// namespaced, data that is not JSON, and the host's own core types and
// sources, which no ingress, extension or operator may submit. Which types a
// source may send is the caller's to check (Ingress.AdmitsTrace).
func CheckTraceEvent(e domain.Event) error {
	switch {
	case !validType(e.Type):
		return fmt.Errorf("type %q is not namespaced (source.name)", e.Type)
	case strings.HasPrefix(e.Type, "core."):
		return fmt.Errorf("%s is a core event; only the host records those", e.Type)
	case e.Source == "":
		return errors.New("source is required")
	case strings.HasPrefix(e.Source, "core.") && e.Source != ManualSource:
		return fmt.Errorf("source %s is the host's own", e.Source)
	case e.CorrelationKey != nil && (e.CorrelationKey.Source == "" || e.CorrelationKey.ExternalID == ""):
		return errors.New("a correlation key needs a source and an external ID")
	case e.Data.Schema == "" && len(e.Data.Data) == 0:
		return nil
	case !validType(e.Data.Schema):
		return fmt.Errorf("payload schema %q is not namespaced", e.Data.Schema)
	case !json.Valid(e.Data.Data):
		return errors.New("payload data is not JSON")
	}
	return nil
}

// TraceRoute is Router.Trace with the scheduler's subscription filter.
func (c *Control) TraceRoute(ctx context.Context, scope domain.ID, e domain.Event) (RouteTrace, error) {
	if c.router == nil {
		return RouteTrace{}, fmt.Errorf("%w: no router", storage.ErrInvalid)
	}
	return c.router.Trace(ctx, scope, e, c.scheduler.options.AliasFilter)
}

// RouteSamples is Router.Samples.
func (c *Control) RouteSamples(ctx context.Context, scope domain.ID) ([]TraceSample, error) {
	return c.router.Samples(ctx, scope)
}
