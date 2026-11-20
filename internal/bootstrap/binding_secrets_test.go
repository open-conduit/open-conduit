package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestBindingSecrets: a package resolves the secrets a binding selects only
// when the binding's executor is one of its own, and only names the binding
// selects; another executor's binding, an unselected name, an unknown
// binding or scope and a disabled scope are refused.
func TestBindingSecrets(t *testing.T) {
	binding := func(name, executor string, secrets ...any) map[string]any {
		return map[string]any{"name": name, "executor": executor, "requires": map[string]any{"toolbelt": "mcp"}, "operations": []any{}, "secret_names": secrets, "config": map[string]any{}}
	}
	scope := func(id string, enabled bool) map[string]any {
		return map[string]any{"id": id, "name": id, "enabled": enabled,
			"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}},
			"bindings":      []any{binding("agent", "agent", "key"), binding("other", "other", "key")}, "routes": []any{}, "extensions": []any{},
			"secret_refs": map[string]any{"key": "env:KEY", "spare": "env:SPARE"}}
	}
	doc := map[string]any{"version": 1, "settings": map[string]any{}, "connections": []any{}, "scopes": []any{scope("work", true), scope("off", false)}}
	raw, _ := json.Marshal(doc)
	s, err := config.Validate(raw, config.Registry{Executors: map[string]extension.Executor{"agent": connTestExecutor{}, "other": connTestExecutor{}}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	env := map[string]string{"KEY": "the-key", "SPARE": "spare"}
	d := &daemon{resolver: &config.Resolver{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}}
	d.external.current.Store(&generationSnapshot{s: s, gen: 1})
	resolve := d.bindingSecrets(protocol.Manifest{Name: "agent-pkg", Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: "agent"}}}})
	ctx := context.Background()
	if got, err := resolve(ctx, "work", "agent", []string{"key"}); err != nil || got["key"] != "the-key" {
		t.Fatalf("own binding: %v %v", got, err)
	}
	for _, c := range []struct {
		name, scope, binding string
		names                []string
	}{
		{"another executor's binding", "work", "other", []string{"key"}},
		{"a name the binding doesn't select", "work", "agent", []string{"spare"}},
		{"an unknown binding", "work", "nope", []string{"key"}},
		{"an unknown scope", "nope", "agent", []string{"key"}},
		{"a disabled scope", "off", "agent", []string{"key"}},
	} {
		if got, err := resolve(ctx, domain.ID(c.scope), c.binding, c.names); !errors.Is(err, extension.ErrUnauthorized) {
			t.Errorf("%s: %v %v, want refused", c.name, got, err)
		}
	}
}
