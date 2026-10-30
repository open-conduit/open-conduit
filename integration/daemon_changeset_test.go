package integration_test

import (
	"bytes"
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

// changeSetClaude is a fake Claude Code CLI for a setup session that
// proposes changes. On its first turn it proposes through the toolbelt's
// MCP server, validates, leaves its execution token and endpoint for the
// test, waits for the test to release it, then asks whether to go on and
// ends the turn; on the next it keeps what it was resumed with and
// finishes.
const changeSetClaude = `#!/bin/sh
f=${FAKE_DIR:?}
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
cmd=$(sed -n 's/.*"command": "\(.*\)",*$/\1/p' "$mcp")
ep=$(sed -n 's/.*"OS_ENDPOINT": "\(.*\)",*$/\1/p' "$mcp")
tf=$(sed -n 's/.*"OS_TOKEN_FILE": "\(.*\)",*$/\1/p' "$mcp")
call() {
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake","version":"0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    "$@" | env OS_ENDPOINT="$ep" OS_TOKEN_FILE="$tf" "$cmd" mcp
}
if [ -f "$f/proposed" ]; then
  printf '%s\n' "$line" > "$f/resumed"
  state=finished
else
  : > "$f/proposed"
  call \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.propose","input":{"op":"replace","pointer":"/scopes/setup/name","value":"Set up","why":"a clearer name"},"request_id":"c1"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.propose","input":{"op":"add","pointer":"/settings/reconcile_seconds","value":600,"why":"reconcile every ten minutes"},"request_id":"c2"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.propose","input":{"op":"add","pointer":"/scopes/setup/secret_refs/token","value":"ghp_notareference","why":"the token"},"request_id":"literal"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.propose","input":{"op":"add","pointer":"/scopes/setup/secret_refs/token","value":"keyring:github-token","why":"the token, by reference"},"request_id":"c3"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.withdraw","input":{"change":"c3","reason":"not needed yet"},"request_id":"w3"}}}' \
    '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.propose","input":{"op":"add","pointer":"/scopes/setup/secret_refs/token","value":"keyring:github-token","why":"the token, by reference"},"request_id":"c4"}}}' \
    '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.validate","input":{},"request_id":"v1"}}}' \
    > "$f/mcp-out"
  call \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.probe","input":{"probe":"setup.tools","why":"which tools are here"},"request_id":"p1"}}}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.probe","input":{"probe":"setup.cat","args":{"path":"/etc/passwd"},"why":"peek"},"request_id":"p2"}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.read","input":{},"request_id":"r1"}}}' \
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.say","input":{"text":"your token is ghp_abcdefghijklmnopqrstuvwxyz0123456789"},"request_id":"leak"}}}' \
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.trace","input":{"scope":"setup","sample":"setup/session-started","why":"would a session start"},"request_id":"t1"}}}' \
    '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.trace","input":{"scope":"nowhere","why":"no such scope"},"request_id":"t2"}}}' \
    > "$f/probe-out"
  cp "$tf" "$f/token"
  printf '%s' "$ep" > "$f/endpoint"
  while [ ! -f "$f/release" ]; do sleep 0.1; done
  call '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call","arguments":{"operation":"setup.ask","input":{"question":"Shall I go on?","choices":["yes","no"]},"request_id":"go-on"}}}' >> "$f/mcp-out"
  state=help
fi
echo '{"type":"user"}' >> "$t/$session.jsonl"
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"%s","num_turns":1,"total_cost_usd":0,"structured_output":{"state":"%s","summary":"set","handoff":"none"}}\n' "$session" "$state"
`

// TestDaemonSetupChangeSet drives a change set through the composed daemon.
// The agent proposes and withdraws, and a literal secret is refused. The
// operator sees each proposal (references only in the panel, never in an
// Event), rejects one, approves the rest with Approve all, validates, and
// applies the candidate with its expected generation. The applied hook
// tells the agent. While the agent's turn runs, its own execution token
// can't apply, read the candidate, invoke an action or validate as the
// operator: only the operator applies.
func TestDaemonSetupChangeSet(t *testing.T) {
	d := newDrill(t)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(changeSetClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	binding := setupBinding(d.root, binary)
	binding["operations"] = []string{"read_context", "wait", "declare_outcome", "setup.say", "setup.ask", "setup.probe", "setup.read", "setup.propose", "setup.withdraw", "setup.validate", "setup.trace"}
	var doc bytes.Buffer
	if err := json.Indent(&doc, setupDaemonConfig(binding), "", "    "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, doc.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	res, err := d.c.InvokeAction(ctx, client.ActionTarget{ScopeID: "setup"}, "setup", "start", "opus", "start")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	session := client.ActionTarget{WorkID: res.Event.RoutedWorkID}
	d.until("the agent proposed and handed over its token", 30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(d.root, "endpoint"))
		return err == nil
	})
	out, _ := os.ReadFile(filepath.Join(d.root, "mcp-out"))
	if !strings.Contains(string(out), "not_a_reference") {
		t.Fatalf("a literal secret was not refused: %s", out)
	}
	// Probes, the read and the trace, each through the toolbelt: an unknown
	// probe and text with a token's shape are refused.
	probed, _ := os.ReadFile(filepath.Join(d.root, "probe-out"))
	for _, want := range []string{`\"name\": \"git\"`, "unknown_probe", `\"outline\"`, `\"name\": \"Setup\"`, "looks_like_secret", `\"kind\": \"start\"`, `\"verdict\": \"match\"`, "has no scope nowhere"} {
		if !strings.Contains(string(probed), want) {
			t.Errorf("probes without %s: %s", want, probed)
		}
	}
	if strings.Contains(string(probed), "ghp_abcdef") {
		t.Errorf("a token reached the output: %s", probed)
	}

	// Only the operator applies: the agent's own endpoint serves none of it,
	// and its token is refused on the management socket too.
	token, _ := os.ReadFile(filepath.Join(d.root, "token"))
	endpoint, _ := os.ReadFile(filepath.Join(d.root, "endpoint"))
	exec, err := client.New(string(endpoint), strings.TrimSpace(string(token)))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := d.c.Config(ctx)
	if _, err = exec.ApplyConfigText(ctx, doc.Bytes(), cfg.Hash, cfg.Generation, "agent-apply"); !isStatus(err, 404) {
		t.Errorf("the agent applied config: %v", err)
	}
	if _, err = exec.WorkConfigCandidate(ctx, session.WorkID); !isStatus(err, 404) {
		t.Errorf("the agent read the candidate: %v", err)
	}
	if _, err = exec.ValidateConfig(ctx, doc.Bytes()); !isStatus(err, 404) {
		t.Errorf("the agent validated as the operator: %v", err)
	}
	if _, err = exec.InvokeAction(ctx, session, "setup", "approve_c1", "", "agent-approve"); !isStatus(err, 404) {
		t.Errorf("the agent approved its own change: %v", err)
	}
	mgmt, err := client.New("unix://"+filepath.Join(d.state, "management.sock"), strings.TrimSpace(string(token)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mgmt.ValidateConfig(ctx, doc.Bytes()); !isStatus(err, 403) {
		t.Errorf("the agent's token reached an operator endpoint on the management socket: %v", err)
	}
	for name, call := range map[string]client.ToolCall{
		"an apply op":         {Operation: "setup.propose", Input: json.RawMessage(`{"op":"apply","pointer":"/","value":{},"why":"apply it"}`)},
		"an expected gen":     {Operation: "setup.propose", Input: json.RawMessage(`{"op":"replace","pointer":"/scopes/setup/name","value":"x","why":"x","expected_generation":"1"}`)},
		"a validate to apply": {Operation: "setup.validate", Input: json.RawMessage(`{"apply":true}`)},
		"core config apply":   {Operation: "config.apply", Input: json.RawMessage(`{}`)},
		"core emit":           {Operation: "emit", Input: json.RawMessage(`{"type":"setup.applied","delivery_id":"x"}`)},
	} {
		if _, err = exec.ToolbeltCall(ctx, call); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if now, _ := d.c.Config(ctx); now.Generation != "1" {
		t.Fatalf("the generation moved under the agent: %s", now.Generation)
	}
	if err = os.WriteFile(filepath.Join(d.root, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	work := func() client.Work {
		w, _ := d.c.Work(ctx, session.WorkID)
		return w
	}
	d.until("the agent parked on its question", 30*time.Second, func() bool { return work().State == "waiting" })

	events := func(typ string) []client.Event {
		page, _ := d.c.Events(ctx, url.Values{"work_id": {session.WorkID}, "type": {typ}})
		return page.Items
	}
	if probes := events("setup.probe"); len(probes) != 3 || !strings.Contains(string(probes[0].Data.Data), `"probe":"setup.tools"`) || !strings.Contains(string(probes[1].Data.Data), `"verdict":"match"`) || !strings.Contains(string(probes[2].Data.Data), `"error":"the candidate has no scope nowhere"`) {
		t.Fatalf("probe events: %+v", probes)
	}
	if said := events("setup.message"); len(said) != 0 {
		t.Fatalf("a message with a token's shape was recorded: %+v", said)
	}
	proposals := events("setup.proposal")
	if len(proposals) != 4 {
		t.Fatalf("proposals: %+v", proposals)
	}
	for _, p := range proposals {
		if strings.Contains(string(p.Data.Data), "keyring:") || strings.Contains(string(p.Data.Data), "ghp_") {
			t.Fatalf("an Event carries reference text: %s", p.Data.Data)
		}
	}
	if !strings.Contains(string(proposals[3].Data.Data), `"after":"[redacted]"`) {
		t.Fatalf("the reference's proposal: %s", proposals[3].Data.Data)
	}
	panels, err := d.c.Panels(ctx, session)
	if err != nil || len(panels) != 1 || !strings.Contains(mustJSONString(panels), "keyring:github-token") || !strings.Contains(mustJSONString(panels), `"Before","value":"\"Setup\""`) {
		t.Fatalf("panel: %s %v", mustJSONString(panels), err)
	}

	actions := func() map[string]client.Action {
		acts, _ := d.c.Actions(ctx, session)
		out := map[string]client.Action{}
		for _, a := range acts {
			if a.Provider == "setup" {
				out[a.ID] = a
			}
		}
		return out
	}
	if _, ok := actions()["approve_c3"]; ok {
		t.Fatal("a withdrawn change can be approved")
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", "reject_c4", "not until the secret is stored", "reject-c4"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	all := ""
	for id, a := range actions() {
		if strings.HasPrefix(id, "approve_all_") {
			all = id
			if a.Label != "Approve all 2" {
				t.Fatalf("approve all: %+v", a)
			}
		}
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", "approve_all_00000000", "", "stale"); err == nil {
		t.Fatal("an Approve all for proposals the operator didn't see was accepted")
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", all, "", "all"); err != nil {
		t.Fatalf("approve all: %v", err)
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", "validate", "", "validate"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	checks := events("setup.checks")
	if len(checks) != 2 || !strings.Contains(string(checks[1].Data.Data), `"valid":true`) || !strings.Contains(string(checks[1].Data.Data), `"approved":2`) {
		t.Fatalf("checks: %+v", checks)
	}

	cand, err := d.c.WorkConfigCandidate(ctx, session.WorkID)
	if err != nil || cand.BaseGeneration != "1" || !strings.Contains(cand.ConfigText, `"name": "Set up"`) || !strings.Contains(cand.ConfigText, `"reconcile_seconds": 600`) || strings.Contains(cand.ConfigText, "github-token") {
		t.Fatalf("candidate: %+v %v", cand, err)
	}
	if _, err = d.c.ApplyConfigText(ctx, []byte(cand.ConfigText), cand.CandidateHash, cand.BaseGeneration, "apply"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if saved, _ := os.ReadFile(d.config); string(saved) != cand.ConfigText {
		t.Fatalf("apply did not save the text as given:\n%s", saved)
	}
	d.until("the session heard its changes were applied", 30*time.Second, func() bool { return len(events("setup.applied")) == 1 })
	if a := events("setup.applied")[0]; !strings.Contains(string(a.Data.Data), `"generation":"2"`) || !strings.Contains(string(a.Data.Data), `"changes":["c1","c2"]`) {
		t.Fatalf("applied: %s", a.Data.Data)
	}
	if _, ok := actions()["validate"]; ok {
		t.Fatal("validate is still offered with nothing approved left to apply")
	}

	if _, err = d.c.InvokeAction(ctx, session, "setup", "answer", "yes", "answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	d.until("the session finished", 30*time.Second, func() bool { return work().State == "completed" })
	resumed, _ := os.ReadFile(filepath.Join(d.root, "resumed"))
	for _, want := range []string{"rejected c4", "not until the secret is stored", "approved c1, c2", "applied your changes", "Generation 2"} {
		if !strings.Contains(string(resumed), want) {
			t.Errorf("resumed without %q: %s", want, resumed)
		}
	}
}

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
