package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/open-conduit/open-conduit/extension"
)

// DefaultScopeID is the scope `init setup` writes, and the one the daemon
// runs on when it starts with no config file.
const DefaultScopeID = "setup"

// DefaultModels are the models a session may choose in the scope Scope writes.
var DefaultModels = []string{"opus", "sonnet"}

var scopeID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Scope is a setup scope with the given ID: the `setup` entry and route,
// bound to a Claude Code agent with its built-in tools off and only the
// operations a session may have, starting on the setup playbook in session
// mode. It passes ValidateBinding.
func Scope(id string) json.RawMessage {
	ops := append(append([]string{}, builtins...), Operations()...)
	scope := map[string]any{
		"id": id, "name": "Setup",
		"authorization": map[string]any{"connections": []string{}, "read_scopes": []string{}, "operations": ops},
		"extensions":    []any{map[string]any{"name": Type, "type": Type, "secret_names": []string{}, "config": map[string]any{}}},
		"routes": []any{map[string]any{"name": Type, "handler": Type, "binding": "agent", "active_policy": "inject",
			"unsupported_fallback": "queue", "error_fallback": "queue", "config": map[string]any{}}},
		"bindings": []any{map[string]any{"name": "agent", "executor": Executor, "requires": map[string]any{"toolbelt": "mcp"},
			"operations": ops, "secret_names": []string{},
			"config": map[string]any{"playbook": "setup", "args": "session", "builtin_tools": []string{}, "models": DefaultModels}}},
		"secret_refs": map[string]any{},
	}
	b, _ := json.Marshal(scope)
	return b
}

// Setup is `open-conduit init setup`: it adds a setup scope to the config
// file, for an operator who wants it written before the daemon starts.
type Setup struct{}

var _ extension.Setup = Setup{}

func (Setup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: Type, Summary: "a scope for setting Open Conduit up from the web view", Usage: "[--scope ID]"}
}

func (Setup) Setup(_ context.Context, _ extension.SetupHost, args []string) (extension.SetupResult, error) {
	id := DefaultScopeID
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--scope" && i+1 < len(args):
			id = args[i+1]
			i++
		default:
			return extension.SetupResult{}, fmt.Errorf("%w: unknown argument %q (usage: init setup [--scope ID])", extension.ErrSetupUsage, args[i])
		}
	}
	if !scopeID.MatchString(id) {
		return extension.SetupResult{}, fmt.Errorf("%w: --scope must be a scope ID such as setup", extension.ErrSetupUsage)
	}
	return extension.SetupResult{Scope: Scope(id),
		Summary: []string{"Added scope " + id + ": start a setup session from it in the web view."},
		Next:    []string{"open-conduit serve", "open-conduit web"}}, nil
}
