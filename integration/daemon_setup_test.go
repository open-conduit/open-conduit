package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// setupClaude is a fake Claude Code CLI for a setup session. On its first
// turn it says hello and asks a question through the toolbelt's MCP server
// (named in --mcp-config) and ends the turn; on the next it keeps what it
// was resumed with and finishes. It records its arguments.
const setupClaude = `#!/bin/sh
f=${FAKE_DIR:?}
printf '%s\n' "$*" >> "$f/args"
session=""; mcp=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id|--resume) session=$2; shift ;;
    --mcp-config) mcp=$2; shift ;;
  esac
  shift
done
t="$CLAUDE_CONFIG_DIR/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$t"
echo '{"type":"system","subtype":"init"}'
IFS= read -r line
content=$(printf '%s' "$line" | sed -E 's/^\{"message":\{"content":"(.*)","role":"user"\},"type":"user"\}$/\1/')
printf '{"type":"user","isReplay":true,"message":{"role":"user","content":"%s"}}\n' "$content"
if [ -f "$f/asked" ]; then
  printf '%s\n' "$line" > "$f/resumed"
  state=finished
else
  : > "$f/asked"
  cmd=$(sed -n 's/.*"command": "\(.*\)",*$/\1/p' "$mcp")
  ep=$(sed -n 's/.*"OS_ENDPOINT": "\(.*\)",*$/\1/p' "$mcp")
  tf=$(sed -n 's/.*"OS_TOKEN_FILE": "\(.*\)",*$/\1/p' "$mcp")
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake","version":"0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.say","input":{"text":"Claude Code is installed and signed in."},"request_id":"hello"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.ask","input":{"question":"Which repository?","choices":["octo/widgets","octo/gadgets"]},"request_id":"repo"}}}' \
    | env OS_ENDPOINT="$ep" OS_TOKEN_FILE="$tf" "$cmd" mcp > "$f/mcp-out"
  state=help
fi
echo '{"type":"user"}' >> "$t/$session.jsonl"
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"%s","num_turns":1,"total_cost_usd":0,"structured_output":{"state":"%s","summary":"set","handoff":"none"}}\n' "$session" "$state"
`

// setupBinding is a setup session's agent: Claude Code with its built-in
// tools off and only the operations a session may have, offering a shell
// the operator may turn on for one session.
func setupBinding(root, binary string) map[string]any {
	return map[string]any{"name": "agent", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true},
		"operations": []string{"read_context", "wait", "declare_outcome", "setup.say", "setup.ask"}, "secret_names": []any{},
		"config": map[string]any{"binary": binary, "playbook": "-", "builtin_tools": []string{}, "model": "opus", "models": []string{"opus", "sonnet"}, "cold_turns": "resume",
			"env": map[string]string{"FAKE_DIR": root, "CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}},
		"grants": []any{map[string]any{"name": "shell", "label": "A shell", "config": map[string]any{"builtin_tools": []string{"Bash"}}}}}
}

// setupDaemonConfig is a scope with a `setup` entry and route bound to
// binding.
func setupDaemonConfig(binding map[string]any) []byte {
	ops := binding["operations"]
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "setup", "name": "Setup",
			"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []any{}},
			"bindings":      []any{binding},
			"routes": []any{map[string]any{"name": "setup", "handler": "setup", "binding": "agent", "active_policy": "inject",
				"unsupported_fallback": "queue", "error_fallback": "queue", "config": map[string]any{}}},
			"extensions":  []any{map[string]any{"name": "setup", "type": "setup", "secret_names": []any{}, "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSetupSession drives a setup session through the composed
// daemon: the operator starts it from the scope on the model they chose,
// the agent's message and question arrive as typed Events on its Work, a
// message sent while it is parked waits for the answer, and the answer
// resumes it with both. The shell the operator turned on is in every turn.
// An agent with a shell for every session is refused.
func TestDaemonSetupSession(t *testing.T) {
	d := newDrill(t)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(setupClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, setupDaemonConfig(setupBinding(d.root, binary)), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	for name, b := range map[string]map[string]any{
		"a shell": {"name": "agent", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"},
			"secret_names": []any{}, "config": map[string]any{"command": "true"}},
		"built-in tools on": func() map[string]any {
			b := setupBinding(d.root, binary)
			delete(b["config"].(map[string]any), "builtin_tools")
			return b
		}(),
	} {
		if _, err := d.c.ValidateConfig(ctx, setupDaemonConfig(b)); err == nil || !strings.Contains(err.Error(), "binding_refused") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if cfg, err := d.c.Config(ctx); err != nil || len(cfg.Scopes) != 1 || len(cfg.Scopes[0].Bindings) != 1 || len(cfg.Scopes[0].Bindings[0].Grants) != 1 || cfg.Scopes[0].Bindings[0].Grants[0].Label != "A shell" {
		t.Fatalf("the config view's grants: %+v %v", cfg, err)
	}
	scope := client.ActionTarget{ScopeID: "setup"}
	ids := func(target client.ActionTarget) string {
		acts, _ := d.c.Actions(ctx, target)
		var out []string
		for _, a := range acts {
			if a.Provider == "setup" {
				out = append(out, a.ID)
			}
		}
		return strings.Join(out, ",")
	}
	if got := ids(scope); got != "start" {
		t.Fatalf("scope actions: %q", got)
	}
	if _, err := d.c.InvokeAction(ctx, scope, "setup", "start", "sonnet +web", "bad-start"); err == nil || !strings.Contains(err.Error(), "+shell") {
		t.Fatalf("a grant the binding doesn't offer: %v", err)
	}
	res, err := d.c.InvokeAction(ctx, scope, "setup", "start", "sonnet +shell", "start")
	if err != nil || res.Event.Type != "setup.start" || !strings.HasPrefix(res.Event.RoutedWorkID, "setup/setup/") {
		t.Fatalf("start: %+v %v", res, err)
	}
	session := client.ActionTarget{WorkID: res.Event.RoutedWorkID}
	work := func() client.Work {
		w, _ := d.c.Work(ctx, session.WorkID)
		return w
	}
	if w := work(); w.Kind != "setup.session" || w.Display == nil || w.Display.Title != "Setup session" || w.Context == nil || string(w.Context.Data) != `{"model":"sonnet","grants":["shell"]}` {
		t.Fatalf("session: %+v", w)
	}
	if got := ids(scope); got != "" {
		t.Fatalf("a second start is offered while the session is open: %q", got)
	}
	d.until("the agent parked on its question", 30*time.Second, func() bool { return work().State == "waiting" && ids(session) == "answer,message" })

	events := func(typ string) []client.Event {
		page, _ := d.c.Events(ctx, url.Values{"work_id": {session.WorkID}, "type": {typ}})
		return page.Items
	}
	said, asked := events("setup.message"), events("setup.question")
	if len(said) != 1 || !strings.Contains(string(said[0].Data.Data), "signed in") || said[0].CorrelationKey != nil {
		t.Fatalf("said: %+v", said)
	}
	if len(asked) != 1 || !strings.Contains(string(asked[0].Data.Data), `"choices":["octo/widgets","octo/gadgets"]`) {
		t.Fatalf("asked: %+v", asked)
	}

	if _, err = d.c.InvokeAction(ctx, session, "setup", "message", "I use the work account.", "msg"); err != nil {
		t.Fatalf("message: %v", err)
	}
	if w := work(); w.State != "waiting" {
		t.Fatalf("a message woke the question: %s", w.State)
	}
	// Choices are suggestions, so only a blank answer is refused.
	if _, err = d.c.InvokeAction(ctx, session, "setup", "answer", "   ", "bad"); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("invalid answer: %v", err)
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", "answer", "2", "answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	d.until("the session finished", 30*time.Second, func() bool { return work().State == "completed" })
	resumed, err := os.ReadFile(filepath.Join(d.root, "resumed"))
	if err != nil || !strings.Contains(string(resumed), "answered your question") || !strings.Contains(string(resumed), "octo/gadgets") || !strings.Contains(string(resumed), "work account") {
		t.Fatalf("resumed with: %s %v", resumed, err)
	}
	args, _ := os.ReadFile(filepath.Join(d.root, "args"))
	turns := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(turns) != 2 {
		t.Fatalf("turns: %q", turns)
	}
	for _, line := range turns {
		if !strings.Contains(line, "--model sonnet") || !strings.Contains(line, " --tools Bash") {
			t.Fatalf("a turn ran with: %s", line)
		}
	}
	if got := ids(scope); got != "start" {
		t.Fatalf("scope actions once the session ended: %q", got)
	}
}

// TestDaemonDefaultConfiguration: with no config file and no stored
// generation the daemon starts on the default configuration, a setup scope,
// without writing the file; doctor says so and the web view can start a
// session. The first apply writes the file, and a restart keeps it.
func TestDaemonDefaultConfiguration(t *testing.T) {
	d := newDrill(t)
	if err := os.Remove(d.config); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	cfg, err := d.c.Config(ctx)
	if err != nil || cfg.Generation != "1" || len(cfg.Scopes) != 1 || cfg.Scopes[0].ID != "setup" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if _, err = os.Stat(d.config); !os.IsNotExist(err) {
		t.Fatalf("the default configuration wrote the file: %v", err)
	}
	code := func() string {
		doc, err := d.c.Doctor(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Checks {
			if c.Name == "config" {
				return c.Status + "/" + c.Code
			}
		}
		return ""
	}
	if got := code(); got != "info/default_config" {
		t.Fatalf("doctor config: %s", got)
	}
	acts, err := d.c.Actions(ctx, client.ActionTarget{ScopeID: "setup"})
	if err != nil || len(acts) != 1 || acts[0].ID != "start" || strings.Join(acts[0].Choices, ",") != "opus,sonnet" {
		t.Fatalf("actions: %+v %v", acts, err)
	}

	d.kill()
	d.start("")
	if got := code(); got != "info/default_config" {
		t.Fatalf("doctor config after a restart: %s", got)
	}
	binary := filepath.Join(d.root, "claude")
	if err = os.WriteFile(binary, []byte(setupClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	next := setupDaemonConfig(setupBinding(d.root, binary))
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(d.config); err != nil {
		t.Fatalf("apply wrote no file: %v", err)
	}
	if got := code(); got != "ok/current" {
		t.Fatalf("doctor config after apply: %s", got)
	}
}
