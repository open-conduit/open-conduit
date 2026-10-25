package bootstrap

import (
	"context"
	"strings"

	"github.com/alsiola/open-conduit/extension"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/internal/runtime"
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
	if e.Source != runtime.ManualSource || strings.HasPrefix(e.Type, "core.") {
		return extension.RouteDecision{}, nil
	}
	if in.Work != nil {
		return extension.RouteDecision{Match: true}, nil
	}
	if e.CorrelationKey == nil {
		return extension.RouteDecision{}, nil
	}
	return extension.RouteDecision{Match: true, Kind: "manual.work"}, nil
}

func handlers() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{"manual": manualRoute{}, gh.Type: gh.Route{}}
}

func handlerNames() map[string]bool {
	names := map[string]bool{}
	for name := range handlers() {
		names[name] = true
	}
	return names
}
