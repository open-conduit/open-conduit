package bootstrap

import (
	"encoding/json"

	is "github.com/alsiola/open-conduit/extensions/introspection"
	"github.com/alsiola/open-conduit/internal/config"
)

// Issue reporting (03-08). The introspection extension is composed like any
// other first-party extension: its entry type and route config validator are
// registered with the config registry, its route handler with the router,
// and its operations and report family on a generation's toolbelt only when
// that generation has an enabled scope with an `introspection` entry. Removing
// or disabling the entries therefore removes the operations (calls are
// undeclared) without any change to core; the route matches nothing in a
// scope without an entry. The processing executor is whatever binding the
// user's `introspection` route names: none is built in.

// introspectionRegistry registers the extension type and the route handler's
// config validator (the handler itself is in handlers()).
func introspectionRegistry(reg *config.Registry) {
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	if reg.HandlerConfig == nil {
		reg.HandlerConfig = map[string]func(json.RawMessage) error{}
	}
	reg.Extensions[is.Type] = config.ExtensionType{Validate: is.ValidateExtension}
	reg.HandlerConfig[is.Type] = is.ValidateRoute
	reg.Operations = append(reg.Operations, is.Operations()...)
}

// introspectionEnabled reports whether any enabled scope of the generation
// has an `introspection` entry.
func introspectionEnabled(s *config.Snapshot) bool {
	if s == nil {
		return false
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type == is.Type {
				return true
			}
		}
	}
	return false
}

// registeredOnly narrows a binding's granted operations to those registered
// on the generation's toolbelt, so a binding that still lists the operations
// of a disabled extension keeps running without them (its calls are
// undeclared) instead of failing to obtain a token.
func registeredOnly(granted, registered []string) []string {
	known := map[string]bool{}
	for _, op := range registered {
		known[op] = true
	}
	out := make([]string, 0, len(granted))
	for _, op := range granted {
		if known[op] {
			out = append(out, op)
		}
	}
	return out
}
