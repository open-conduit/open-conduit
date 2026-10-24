package extension

import (
	"context"

	"github.com/alsiola/open-conduit/domain"
)

// RouteInput is a detached, read-only snapshot. A handler may be evaluated again
// after a revision conflict; it must be deterministic and perform no side effects.
type RouteInput struct {
	Event domain.AcceptedEvent
	Scope domain.Scope
	Work  *domain.Work
}

// RouteDecision describes a first match. CreateID/Kind are used only when no
// owner exists; AttachKeys are appended atomically or the entire route fails.
// A successor must use a distinct key; terminal ownership is never transferred.
type RouteDecision struct {
	Match      bool
	CreateID   domain.ID
	Kind       string
	AttachKeys []domain.CorrelationKey
}

type RouteHandler interface {
	Match(context.Context, RouteInput) (RouteDecision, error)
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
