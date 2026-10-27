package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// daemonClaude is a fake Claude Code CLI for the composed daemon. It reads
// stream-json user messages (two when the first names the hold-for word,
// otherwise one), echoes
// each back as a replay, writes the session transcript where claude keeps it
// and ends with a structured status. Modes (the mode file): review; wait
// (block until released, so a daemon kill finds it alive); mcp (drive the
// toolbelt's MCP server named in --mcp-config: initialize, list tools, read
// context and wait for agent.wake, then end the turn "building").
const daemonClaude = `#!/bin/sh
session=""; resume=0; mcp=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id) session=$2; shift ;;
    --resume) session=$2; resume=1; shift ;;
    --mcp-config) mcp=$2; shift ;;
  esac
  shift
done
f=${FAKE_DIR:?}
mode=$(cat "$f/mode" 2>/dev/null || echo review)
printf '%s session=%s resume=%s\n' "$OS_EXECUTION_ID" "$session" "$resume" >> "$f/invocations"
t="$CLAUDE_CONFIG_DIR/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$t"
echo '{"type":"system","subtype":"init"}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$f/stdin"
  content=$(printf '%s' "$line" | sed -E 's/^\{"message":\{"content":"(.*)","role":"user"\},"type":"user"\}$/\1/')
  printf '{"type":"user","isReplay":true,"message":{"role":"user","content":"%s"}}\n' "$content"
  if [ "$n" -eq 1 ]; then
    : > "$f/started"
    want=1
    hold=$(cat "$f/hold-for" 2>/dev/null)
    if [ -n "$hold" ] && printf '%s' "$line" | grep -q "$hold"; then want=2; fi
  fi
  [ "$n" -ge "$want" ] && break
done
state=review
case "$mode" in
wait)
  echo $$ > "$f/pid"
  while [ ! -e "$f/release" ]; do sleep 0.05; done ;;
mcp)
  cmd=$(sed -n 's/.*"command": "\(.*\)",*$/\1/p' "$mcp")
  ep=$(sed -n 's/.*"OS_ENDPOINT": "\(.*\)",*$/\1/p' "$mcp")
  tf=$(sed -n 's/.*"OS_TOKEN_FILE": "\(.*\)",*$/\1/p' "$mcp")
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake","version":"0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_context","arguments":{}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wait","arguments":{"match":{"types":["agent.wake"]},"timeout_seconds":3600,"request_id":"wait-1"}}}' \
    | env OS_ENDPOINT="$ep" OS_TOKEN_FILE="$tf" "$cmd" mcp > "$f/mcp-out"
  state=building ;;
esac
echo '{"type":"user"}' >> "$t/$session.jsonl"
turns=$(wc -l < "$t/$session.jsonl" | tr -d ' ')
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"%s","num_turns":1,"total_cost_usd":%s,"structured_output":{"state":"%s","summary":"turn %s","handoff":"note %s"},"modelUsage":{"claude-x":{"inputTokens":%s,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}\n' "$session" "$turns" "$state" "$turns" "$turns" $((turns*10))
`

func claudeDrillConfig(root, binary string) []byte {
	ops := []string{"read_context", "declare_outcome", "wait"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "drill", "name": "Drill",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "agent", "executor": "claude-code", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true}, "operations": ops, "secret_names": []any{},
				"config": map[string]any{"binary": binary, "status_schema": "github.status.v1", "cold_turns": "resume",
					"env": map[string]string{"FAKE_DIR": root, "CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}}}},
			"routes":      []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "inject", "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonClaudeCodeExecution drives the composed daemon with the Claude
// Code executor against a fake claude: the agent reads context and parks on a
// wait through the real `os mcp` server, the wake resumes the same session as
// a new segment, an event arriving mid-turn is injected and consumed, and a
// turn running when the daemon is SIGKILLed is adopted after restart without
// relaunch. Session continuity stays private: no runtime API shows it.
func TestDaemonClaudeCodeExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	d := newDrill(t)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(daemonClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, claudeDrillConfig(d.root, binary), 0o600); err != nil {
		t.Fatal(err)
	}
	set := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d.root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	submit := func(typ, delivery string) {
		t.Helper()
		if _, err := d.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "drill", Type: typ, DeliveryID: delivery, CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-"+delivery); err != nil {
			t.Fatal(err)
		}
	}
	invocations := func() []string {
		b, _ := os.ReadFile(filepath.Join(d.root, "invocations"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	data := func(x client.Execution) map[string]any {
		m := map[string]any{}
		if x.Outcome != nil && x.Outcome.Data != nil {
			_ = json.Unmarshal(x.Outcome.Data.Data, &m)
		}
		return m
	}
	set("mode", "mcp")
	d.start("")

	// The agent parks through MCP; its turn's status is kept with the wait.
	submit("agent.start", "start-1")
	w := d.waitState("waiting")
	out := readFile(t, filepath.Join(d.root, "mcp-out"))
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("mcp output %q", out)
		}
		replies = append(replies, m)
	}
	if len(replies) != 4 {
		t.Fatalf("mcp replies %s", out)
	}
	if init := replies[0]["result"].(map[string]any); init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize %v", init)
	}
	if tools := replies[1]["result"].(map[string]any)["tools"].([]any); len(tools) != 6 {
		t.Fatalf("tools %v", tools)
	}
	for _, r := range replies[2:] {
		if res := r["result"].(map[string]any); res["isError"] != false {
			t.Fatalf("tool call failed: %v", r)
		}
	}
	execs := d.executions(w.ID)
	if len(execs) != 1 || execs[0].State != "parked" || execs[0].Outcome == nil || execs[0].Outcome.Kind != "waiting" || data(execs[0])["status"] != "building" {
		t.Fatalf("parked attempt %+v", execs)
	}
	// The rendered activity (06-01) is the first artifact, so the log view
	// shows what the agent said and did; the raw transcript follows.
	if id, _ := data(execs[0])["activity"].(string); id == "" || len(execs[0].Artifacts) == 0 || execs[0].Artifacts[0].ID != id {
		t.Fatalf("activity artifact: %v %+v", data(execs[0])["activity"], execs[0].Artifacts)
	}
	session := strings.TrimPrefix(strings.Fields(invocations()[0])[1], "session=")

	// The wake resumes the same attempt and session as a new segment.
	set("mode", "review")
	if _, err := d.c.Wake(context.Background(), w.ID, client.WakeRequest{ExpectedRevision: w.Revision, Type: "agent.wake", DeliveryID: "wake-1", CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "wake-1"); err != nil {
		t.Fatal(err)
	}
	d.until("resumed turn handed back", 30*time.Second, func() bool {
		x := d.executions(w.ID)
		return len(x) == 1 && x[0].State == "handoff"
	})
	inv := invocations()
	if len(inv) != 2 || !strings.Contains(inv[1], "session="+session+" resume=1") {
		t.Fatalf("the wake did not resume the session: %v", inv)
	}
	if got, _ := d.work(); got.State != "ready" {
		t.Fatalf("a handed-back Work is %s", got.State)
	}

	// An event during a running turn is injected into it and consumed. claude's
	// echo is the executor's consumption proof (Result.ConsumedInputs), so core
	// consumes the input with that turn and starts no further attempt for it
	// (v1 TestInterjectionReachesTheRunningTurn).
	os.Remove(filepath.Join(d.root, "started"))
	set("hold-for", "agent.more")
	submit("agent.more", "more-1")
	d.until("second turn started", 30*time.Second, func() bool { return len(invocations()) == 3 && exists(filepath.Join(d.root, "started")) })
	submit("agent.interject", "interject-1")
	d.until("injected turn handed back", 30*time.Second, func() bool {
		x := d.executions(w.ID)
		return len(x) == 2 && x[1].State == "handoff"
	})
	time.Sleep(time.Second)
	if x := d.executions(w.ID); len(x) != 2 {
		t.Fatalf("a consumed interjection started another attempt: %d attempts", len(x))
	}
	injected, _ := data(d.executions(w.ID)[1])["injected"].(map[string]any)
	if injected == nil || injected["accepted"] != float64(1) || injected["consumed"] != float64(1) {
		t.Fatalf("injection %v", data(d.executions(w.ID)[1]))
	}
	if strings.Count(readFile(t, filepath.Join(d.root, "stdin")), "agent.interject") != 1 || !strings.Contains(readFile(t, filepath.Join(d.root, "stdin")), "Arrived during this turn") {
		t.Fatalf("the interjection did not reach claude exactly once, mid-turn:\n%s", readFile(t, filepath.Join(d.root, "stdin")))
	}

	// A turn running when the daemon dies is adopted, not relaunched.
	set("mode", "wait")
	os.Remove(filepath.Join(d.root, "started"))
	submit("agent.slow", "slow-1")
	pidFile := filepath.Join(d.root, "pid")
	d.until("slow turn running", 30*time.Second, func() bool { return fileHasLine(pidFile) })
	pid, _ := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	d.kill()
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the turn did not survive the daemon")
	}
	d.start("")
	time.Sleep(drillLease)
	set("release", "")
	d.until("adopted turn finished", 60*time.Second, func() bool {
		x := d.executions(w.ID)
		return len(x) == 3 && x[2].State == "handoff"
	})
	launches := 0
	for _, line := range invocations() {
		if strings.HasPrefix(line, d.executions(w.ID)[2].ID+" ") {
			launches++
		}
	}
	if launches != 1 {
		t.Fatalf("the adopted turn launched %d times: %v", launches, invocations())
	}

	// No runtime API carries the session: not the Work, its context or any
	// outcome.
	got, _ := d.work()
	blob, _ := json.Marshal(struct {
		W client.Work
		X []client.Execution
	}{got, d.executions(w.ID)})
	if strings.Contains(string(blob), session) || strings.Contains(out, session) {
		t.Fatal("the private session reached a runtime API")
	}
	// Per-turn telemetry is reported outside core.
	f, err := os.Open(filepath.Join(d.state, "logs", "claude-code.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	turns := 0
	for s := bufio.NewScanner(f); s.Scan(); {
		var r struct{ Name string }
		if json.Unmarshal(s.Bytes(), &r) == nil && r.Name == "agent_turn" {
			turns++
		}
	}
	if turns != 4 {
		t.Fatalf("%d agent_turn records for four segments", turns)
	}
}

// TestDaemonClaudeCodeContinuityKill SIGKILLs the daemon at the executor's
// continuity write, the decision recorded before claude launches: inside the
// transaction (rolled back) and right after its commit (nothing launched).
// After restart the unlaunched attempt is not adoptable, its retry launches
// exactly once and still resumes the Work's session.
func TestDaemonClaudeCodeContinuityKill(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			d := newDrill(t)
			binary := filepath.Join(d.root, "claude")
			if err := os.WriteFile(binary, []byte(daemonClaude), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(d.config, claudeDrillConfig(d.root, binary), 0o600); err != nil {
				t.Fatal(err)
			}
			submit := func(typ, delivery string) {
				t.Helper()
				if _, err := d.c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "drill", Type: typ, DeliveryID: delivery, CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-"+delivery); err != nil {
					t.Fatal(err)
				}
			}
			invocations := func() []string {
				b, _ := os.ReadFile(filepath.Join(d.root, "invocations"))
				return strings.Split(strings.TrimSpace(string(b)), "\n")
			}
			d.start("")
			submit("agent.start", "start-1")
			var w client.Work
			d.until("first turn handed back", 30*time.Second, func() bool {
				var ok bool
				if w, ok = d.work(); !ok {
					return false
				}
				x := d.executions(w.ID)
				return len(x) == 1 && x[0].State == "handoff"
			})
			session := strings.TrimPrefix(strings.Fields(invocations()[0])[1], "session=")
			d.kill()

			d.start("kv:" + phase + ":executor.claudecode")
			submit("agent.more", "more-1")
			d.killed()
			if n := len(invocations()); n != 1 {
				t.Fatalf("claude launched before the continuity write committed: %v", invocations())
			}
			d.start("")
			d.until("retried turn handed back", 60*time.Second, func() bool {
				for _, x := range d.executions(w.ID) {
					if x.Attempt > 1 && x.State == "handoff" {
						return true
					}
				}
				return false
			})
			inv := invocations()
			if len(inv) != 2 || !strings.Contains(inv[1], "session="+session+" resume=1") {
				t.Fatalf("the retried turn did not resume the Work's session exactly once: %v", inv)
			}
		})
	}
}
