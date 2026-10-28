package bootstrap

import (
	"context"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// manualRoute is the first-party "manual" route handler. It matches only
// operator-submitted Events (source core.manual, never core control types):
// an Event addressed to existing Work, by Work ID or an owned correlation key,
// becomes that Work's queued input; an unowned correlation key creates Work of
// kind "manual.work" owning that key. Execution emissions, timer and core Events
// never match, so they cannot create Work through this handler. A waiting
// Work's wake is decided by its Wait, not by a route.
type manualRoute struct{}

func (manualRoute) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Source != runtime.ManualSource {
		return extension.Skip("not an operator's event"), nil
	}
	if strings.HasPrefix(e.Type, "core.") {
		return extension.Skip("a core event is never operator input"), nil
	}
	if in.Work != nil {
		return extension.RouteDecision{Match: true, Reason: "an operator's event for existing Work"}, nil
	}
	if e.CorrelationKey == nil {
		return extension.Skip("the event names no Work or key"), nil
	}
	return extension.RouteDecision{Match: true, Kind: "manual.work", Reason: "an operator's event for a new key"}, nil
}

var _ extension.RouteSampler = manualRoute{}

// Samples offers an operator's event for a key, and a core event the route
// never takes.
func (manualRoute) Samples(domain.Scope, domain.Route) []extension.RouteSample {
	key := domain.CorrelationKey{Source: runtime.ManualSource, ExternalID: "example/1"}
	return []extension.RouteSample{
		{ID: "operator-event", Title: "An operator sends an event for a key", Event: domain.Event{Type: "manual.note", Source: runtime.ManualSource, CorrelationKey: &key}},
		{ID: "operator-event-unkeyed", Title: "An operator sends an event with no key", Event: domain.Event{Type: "manual.note", Source: runtime.ManualSource}},
	}
}

func handlers() map[string]extension.RouteHandler {
	h := map[string]extension.RouteHandler{"manual": manualRoute{}, gh.Type: gh.Route{}, gh.HoldHandler: gh.HoldRoute{}, sl.Type: sl.Route{}, sc.Type: sc.Route{}}
	trackerHandlers(h)
	return h
}

func handlerNames() map[string]bool {
	names := map[string]bool{}
	for name := range handlers() {
		names[name] = true
	}
	return names
}
