package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/runtime"
)

// OfflineChecks validates the config and runs the claude-code bindings'
// binaries without a daemon; an invalid config stops at its diagnostics.
func TestOfflineChecks(t *testing.T) {
	config := `{
  "version": 1, "settings": {}, "connections": [],
  "scopes": [{
    "id": "ops", "name": "Operations",
    "authorization": {"connections": [], "operations": ["read_context", "declare_outcome"], "read_scopes": []},
    "bindings": [{"name": "agent", "executor": "claude-code", "requires": {"toolbelt": "mcp"}, "operations": ["read_context", "declare_outcome"], "secret_names": [], "config": {"binary": "/nonexistent/claude-oc-test"}}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": {}}],
    "secret_refs": {}
  }]
}`
	checks := OfflineChecks(context.Background(), "/cfg/config.json", []byte(config))
	got := map[string]runtime.Check{}
	for _, c := range checks {
		got[c.Name] = c
	}
	if got["config"].Status != runtime.CheckOK || got["github_capability"].Name == "" || !strings.Contains(checks[len(checks)-1].Detail, "storage") {
		t.Fatalf("offline checks: %+v", checks)
	}
	if c := got["claude_code"]; c.Status != runtime.CheckFail || c.Code != "claude_missing" || len(c.Items) != 1 || !strings.Contains(c.Items[0], "ops/agent") {
		t.Fatalf("claude check: %+v", c)
	}

	checks = OfflineChecks(context.Background(), "/cfg/config.json", []byte(`{"version": 1}`))
	if len(checks) != 2 || checks[1].Name != "skipped" || checks[0].Name != "config" || checks[0].Status != runtime.CheckFail || len(checks[0].Items) == 0 {
		t.Fatalf("invalid config: %+v", checks)
	}
}
