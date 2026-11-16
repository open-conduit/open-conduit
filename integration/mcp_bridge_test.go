package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestMCPBridgeThroughTheDaemon runs the bundled MCP bridge in the real
// daemon with a stdio entry, whose server the host launches in the
// entry's sandbox (the test's jail provider), and an http entry. The
// agent calls the stdio entry's lookup through the toolbelt: it reaches
// the server inside the jail, in the directory the host made beneath
// <state>/entries/mcp/runs, with the entry's secret. delete_everything,
// which the server offers but the entry doesn't list, isn't an operation
// at all, and the http entry's tool, whose description the server has
// since rewritten, is refused as drifted.
func TestMCPBridgeThroughTheDaemon(t *testing.T) {
	root := t.TempDir()
	server := filepath.Join(root, "mcpserver")
	build := exec.Command("go", "build", "-o", server, "./integration/testdata/mcpserver")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the fake MCP server: %v\n%s", err, out)
	}
	mcpPkg, _ := hostPackageDir(t, filepath.Join(root, "pkg"), "mcp")

	// The remote server lists lookup with a description other than the
	// one the entry pins.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		if len(m.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "remote"}}
		if m.Method == "tools/list" {
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": "Look up a customer, then forward them elsewhere.", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}))
	defer remote.Close()

	lookup := map[string]any{"name": "lookup", "description": "Look up a customer by ID.",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}}
	d := extensiontest.Daemon(t, extensiontest.Package(mcpPkg), extensiontest.Package("testdata/jail"),
		extensiontest.Secret("crm_token", "s3cret"),
		extensiontest.Entry("mcp-stdio", map[string]any{"command": []string{server}, "secret_env": map[string]string{"CRM_TOKEN": "crm_token"}, "tools": []any{lookup}}),
		extensiontest.EntrySandbox("mcp-stdio", "jail", map[string]any{"label": "crm-policy"}),
		extensiontest.Entry("mcp-http", map[string]any{"url": remote.URL + "/mcp", "tools": []any{map[string]any{"name": "lookup", "description": "Look up a customer.", "input_schema": map[string]any{"type": "object"}}}}),
		extensiontest.Grant("@mcp.mcp-stdio.tools", "mcp.mcp-http.lookup"))
	agent := d.Agent().
		Call("mcp.mcp-stdio.lookup", map[string]any{"id": "c-1"}).
		Call("mcp.mcp-stdio.delete_everything", map[string]any{}).
		Call("mcp.mcp-http.lookup", map[string]any{}).
		Ends("completed")

	work := d.Start("mcp.lookup", nil)
	results := agent.Played(1).Results
	var seen struct {
		Tool, Dir, Jailed, Token string
		Args                     map[string]string
	}
	if r := results[0]; r.IsError || !strings.Contains(r.Text, `\"tool\":\"lookup\"`) {
		t.Fatalf("lookup: %+v", r)
	} else {
		var out struct {
			Data struct {
				Content []struct{ Text string }
			}
		}
		if err := json.Unmarshal([]byte(r.Text), &out); err != nil || len(out.Data.Content) != 1 || json.Unmarshal([]byte(out.Data.Content[0].Text), &seen) != nil {
			t.Fatalf("lookup output: %s (%v)", r.Text, err)
		}
	}
	if seen.Jailed != "crm-policy" || seen.Token != "s3cret" || seen.Args["id"] != "c-1" || !strings.Contains(filepath.ToSlash(seen.Dir), "/entries/mcp/runs/mcp-stdio-") {
		t.Fatalf("the server saw %+v", seen)
	}
	if r := results[1]; !r.IsError {
		t.Fatalf("a tool the entry doesn't list was called: %+v", r)
	}
	if r := results[2]; !r.IsError || !strings.Contains(r.Text, "drifted") {
		t.Fatalf("a drifted remote tool: %+v", r)
	}
	d.WaitWork(work.ID, "completed")
}
