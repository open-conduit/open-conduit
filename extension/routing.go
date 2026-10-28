package extension

import (
	"context"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// RouteInput is a detached, read-only snapshot. A handler may be evaluated again
// after a revision conflict; it must be deterministic and perform no side effects.
type RouteInput struct {
	Event domain.AcceptedEvent
	Scope domain.Scope
	Work  *domain.Work
	// Route is the effective route being evaluated, including its opaque
	// handler Config, so one handler can serve several routes (for example
	// one per binding) and select only the input its own route declares.
	Route domain.Route
}

// RouteDecision describes a first match. CreateID/Kind are used only when no
// owner exists; AttachKeys are appended atomically or the entire route fails.
// A successor must use a distinct key; terminal ownership is never transferred.
// Reason says in a short phrase why the handler matched or skipped ("a new
// issue the labels admit", "not an issue this entry watches"). It explains a
// route trace and never changes the decision.
// Display, when set on a match, replaces the Work's display (ValidateDisplay);
// nil keeps what it has, so a handler sets it only when it has something to
// say (the handler sees the current one on RouteInput.Work).
type RouteDecision struct {
	Match      bool
	CreateID   domain.ID
	Kind       string
	AttachKeys []domain.CorrelationKey
	Reason     string
	Display    *domain.WorkDisplay
}

type RouteHandler interface {
	Match(context.Context, RouteInput) (RouteDecision, error)
}

// Skip is a non-matching decision with its reason.
func Skip(reason string) RouteDecision { return RouteDecision{Reason: reason} }

// RouteSample is a named hypothetical Event a view offers for a route trace
// ("Suppose a new issue is opened"). Event carries the type, source,
// correlation key and data; the host assigns identity and times.
type RouteSample struct {
	ID    string
	Title string
	Event domain.Event
}

// SampleInput is what a RouteSampler builds its samples from: the scope and
// route they are for, and the host's clock, for samples whose data carries a
// time the handler compares with the host's (a message written just now).
type SampleInput struct {
	Scope domain.Scope
	Route domain.Route
	Now   time.Time
}

// RouteSampler is optional on a RouteHandler: the samples it offers for a
// route in a scope, built from the route's and scope's configuration (the
// entry's source namespace, its labels) so they trace as written. IDs are
// stable and unique within the handler. Like Match it must perform no side
// effects.
type RouteSampler interface {
	Samples(context.Context, SampleInput) []RouteSample
}

// InheritRoutes resolves root-to-leaf routes. A same-name override keeps the
// ancestor's slot; new names append in declaration order. Configuration validation
// must reject duplicate names within one scope and missing handlers/bindings.
func InheritRoutes(levels ...[]domain.Route) []domain.Route {
	var result []domain.Route
	slots := map[string]int{}
	for _, level := range levels {
		for _, route := range level {
			if i, ok := slots[route.Name]; ok {
				result[i] = route
			} else {
				slots[route.Name] = len(result)
				result = append(result, route)
			}
		}
	}
	return result
}
