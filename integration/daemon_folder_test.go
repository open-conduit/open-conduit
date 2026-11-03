package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/config"
)

// folderClaude records the folder it was started in, then ends its turn.
const folderClaude = `#!/bin/sh
pwd -P > "${FAKE_DIR:?}/cwd"
echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`

// TestDaemonIncludedFolderAdaptsBinding: a claude-code binding in an
// included folder's config, which the executor package adapts to that
// folder (executors[].folder_config, validate with kind executor_folder),
// resolves its relative dir against the folder, so its turn starts there.
// Before the package could declare the hook, the relative dir reached
// validation unresolved and the config never applied.
func TestDaemonIncludedFolderAdaptsBinding(t *testing.T) {
	d := newDrill(t)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(folderClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(d.root, "web")
	app := filepath.Join(folder, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(folder, ".open-conduit"), 0o755); err != nil {
		t.Fatal(err)
	}
	ops := []string{"read_context", "declare_outcome"}
	fragment, _ := json.Marshal(map[string]any{"version": 1, "scopes": []any{map[string]any{
		"id": "web", "name": "Web",
		"bindings": []any{map[string]any{"name": "agent", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp"}, "operations": ops, "secret_names": []any{},
			"config": map[string]any{"binary": binary, "dir": "app",
				"env": map[string]string{"FAKE_DIR": d.root, "CLAUDE_CONFIG_DIR": filepath.Join(d.root, "claude-config")}}}},
		"routes": []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": map[string]any{}}},
	}}})
	if err := os.WriteFile(filepath.Join(folder, config.FragmentFile), fragment, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
		"includes": []any{map[string]any{"path": "web", "parent": "drill"}},
		"scopes": []any{map[string]any{
			"id": "drill", "name": "Drill",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"secret_refs":   map[string]any{},
		}},
	})
	if err := os.WriteFile(d.config, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	if _, err := d.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "web", Type: "agent.start", DeliveryID: "start-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-start-1"); err != nil {
		t.Fatal(err)
	}
	var got string
	d.until("claude launched", 30*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(d.root, "cwd"))
		got = strings.TrimSpace(string(b))
		return err == nil && got != ""
	})
	want, err := filepath.EvalSymlinks(app)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("claude started in %q, want the included folder's app dir %q", got, want)
	}
}
