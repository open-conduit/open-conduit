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
	// Also lists Work the Event would wake through a dependency
	// subscription, besides its own routing.
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

type traceWait struct {
	wait domain.Wait
	meta waitMetadata
	work domain.Work
}

// Trace explains how the router would route e in scopeID now. It reads one
// snapshot and evaluates the same deterministic handlers routing uses; it
// never writes. filter evaluates a subscription's opaque filter (nil: a
// filtered subscription never matches, as in the scheduler).
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
				if waiting, err = activeWait(tx, w); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		wakes, err = subscriptionWakes(tx, s.event, filter)
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
			out.Also = append(out.Also, wakeEffect(w, true))
		}
	}
	switch {
	case waiting != nil:
		// A waiting Work's wait decides, not its routes; credit the route
		// whose binding it waits on.
		matches := waitMatches(waiting.wait, s.event)
		out.Effect = RouteEffect{Kind: EffectQueue, WorkID: waiting.work.ID, Binding: waiting.meta.Binding, Detail: "kept as context; " + string(waiting.work.ID) + " keeps waiting" + waitingFor(waiting.wait)}
		reason := "its Work waits here; this event is kept as context"
		if matches {
			out.Effect = wakeEffect(*waiting, false)
			reason = "its Work waits here and this event wakes it"
		}
		creditWait(out.Routes, waiting.meta.Binding, reason)
		return out, nil
	case s.work != nil && s.work.State.Terminal():
		out.Effect = RouteEffect{Kind: EffectDrop, WorkID: s.work.ID, Detail: string(s.work.ID) + " is " + string(s.work.State) + "; later input is recorded as terminal"}
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
		if len(out.Also) > 0 && s.work == nil {
			// The Event's only consequence is waking another Work.
			out.Effect, out.Also = out.Also[0], out.Also[1:]
			creditWait(out.Routes, out.Effect.Binding, "its waiting Work is woken by this event")
			return out, nil
		}
		out.Effect = RouteEffect{Kind: EffectUnmatched, Detail: "no route matched; the event is kept unmatched"}
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
		id := p.decision.CreateID
		detail := "starts new " + p.decision.Kind + " Work"
		if id != "" {
			detail += " " + string(id)
		}
		if e.CorrelationKey != nil {
			detail += " owning " + e.CorrelationKey.ExternalID
		}
		out.Effect = RouteEffect{Kind: EffectStart, WorkID: id, Binding: route.Binding, Detail: detail + " on " + route.Binding}
		return out, nil
	}
	out.Effect = r.activeEffect(s, route)
	return out, nil
}

// activeEffect is the policy apply would record for existing, nonterminal
// Work. The attempt-level checks (pinned capabilities, binding mismatch)
// are reported only as the configured fallback the route would use.
func (r *Router) activeEffect(s routeSnapshot, route domain.Route) RouteEffect {
	w := s.work
	out := RouteEffect{WorkID: w.ID, Binding: route.Binding}
	policy := route.ActivePolicy
	if policy == "" {
		policy = EffectQueue
	}
	b, _ := r.binding(s.scope, route.Binding)
	note := ""
	if policy == EffectInject && !b.Capabilities.Inject || policy == EffectInterrupt && !b.Capabilities.Interrupt {
		note = "; " + route.Binding + " cannot " + policy + ", so its " + b.Fallback + " fallback applies"
		policy = b.Fallback
	}
	if w.State != domain.WorkRunning && policy != EffectDrop {
		out.Kind = EffectQueue
		out.Detail = "queued for " + string(w.ID) + "'s next turn (it is " + string(w.State) + ")" + note
		return out
	}
	out.Kind = policy
	switch policy {
	case EffectDrop:
		out.Detail = route.Name + " drops it while " + string(w.ID) + " is active" + note
	case EffectQueue:
		out.Detail = "queued for " + string(w.ID) + "'s next turn" + note
	default:
		out.Detail = policy + " into " + string(w.ID) + "'s running turn" + note
	}
	return out
}

func activeWait(tx storage.Tx, w domain.Work) (*traceWait, error) {
	waits, err := tx.ListWaits(w.ID)
	if err != nil {
		return nil, err
	}
	for _, wait := range waits {
		if wait.State != domain.WaitActive {
			continue
		}
		out := &traceWait{wait: wait, work: w}
		if err = getJSON(tx, waitNamespace, identity(string(wait.ID)), &out.meta); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
		return out, nil
	}
	return nil, nil
}

// subscriptionWakes finds the waiting Work whose active dependency
// subscriptions a, as a source Event, would satisfy: the alias it would
// publish matches that Work's wait.
func subscriptionWakes(tx storage.Tx, a domain.AcceptedEvent, filter func(string, domain.Payload, domain.Event) bool) ([]traceWait, error) {
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
		if !sub.Active || seen[sub.WorkID] || sub.Request.SourceKey != *a.Event.CorrelationKey {
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
		wait, err := activeWait(tx, w)
		if err != nil {
			return nil, err
		}
		if wait == nil || wait.wait.ID != sub.Request.WaitID {
			continue
		}
		key := sub.Request.TargetKey
		alias := a
		alias.Event.Type, alias.Event.WorkID, alias.Event.CorrelationKey = sub.Request.AliasType, sub.WorkID, &key
		if !waitMatches(wait.wait, alias) {
			continue
		}
		seen[w.ID] = true
		out = append(out, *wait)
	}
	return out, nil
}

func wakeEffect(w traceWait, alias bool) RouteEffect {
	detail := "wakes " + string(w.work.ID)
	if alias {
		detail += " through its dependency"
	}
	return RouteEffect{Kind: EffectWake, WorkID: w.work.ID, Binding: w.meta.Binding, Detail: detail + waitingFor(w.wait) + " on " + w.meta.Binding}
}

func waitingFor(w domain.Wait) string {
	var keys []string
	for _, k := range w.Match.Keys {
		keys = append(keys, k.ExternalID)
	}
	if len(keys) == 0 {
		return ""
	}
	out := " (waiting on " + strings.Join(keys, ", ")
	if len(w.Match.Types) > 0 {
		out += " for " + strings.Join(w.Match.Types, ", ")
	}
	return out + ")"
}

// creditWait marks the first route bound to the waiting binding as the match
// and leaves the rest not reached: a wait, not the routes, decides.
func creditWait(steps []RouteStep, binding, reason string) {
	for i := range steps {
		steps[i].Verdict, steps[i].Reason = VerdictNotReached, ""
	}
	for i := range steps {
		if binding != "" && steps[i].Binding == binding {
			steps[i].Verdict, steps[i].Reason = VerdictMatch, reason
			for j := 0; j < i; j++ {
				steps[j].Verdict, steps[j].Reason = VerdictSkip, "its Work waits on "+binding+", not this route's binding"
			}
			return
		}
	}
}

// Samples lists the sample Events the scope's route handlers offer, in route
// order, each sample once per handler.
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
		in := detachedInput(s)
		in.Route = route
		for _, sample := range sampler.Samples(in.Scope, route) {
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
