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
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

// supportClaude is a fake Claude Code CLI for the Slack support binding. Each
// segment reads its prompt (and, when the prompt names the hold-for word, one
// injected message), echoes every message, then drives the real `os mcp`
// server: it replies in the thread with `call slack.reply` and waits on the
// thread with the builtin `wait` the playbook prescribes, ending the turn
// "help". Nothing here knows any repository or checkout.
const supportClaude = `#!/bin/sh
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
printf '%s session=%s resume=%s pwd=%s\n' "$OS_EXECUTION_ID" "$session" "$resume" "$(pwd -P)" >> "$f/invocations"
seg=$(wc -l < "$f/invocations" | tr -d ' ')
t="$CLAUDE_CONFIG_DIR/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$t"
echo '{"type":"system","subtype":"init"}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$f/stdin-$seg"
  content=$(printf '%s' "$line" | sed -E 's/^\{"message":\{"content":"(.*)","role":"user"\},"type":"user"\}$/\1/')
  printf '{"type":"user","isReplay":true,"message":{"role":"user","content":"%s"}}\n' "$content"
  if [ "$n" -eq 1 ]; then
    : > "$f/started-$seg"
    want=1
    hold=$(cat "$f/hold-for" 2>/dev/null)
    if [ -n "$hold" ] && printf '%s' "$line" | grep -q "$hold"; then want=2; fi
  fi
  [ "$n" -ge "$want" ] && break
done
cmd=$(sed -n 's/.*"command": "\(.*\)",*$/\1/p' "$mcp")
ep=$(sed -n 's/.*"OS_ENDPOINT": "\(.*\)",*$/\1/p' "$mcp")
tf=$(sed -n 's/.*"OS_TOKEN_FILE": "\(.*\)",*$/\1/p' "$mcp")
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call","arguments":{"operation":"slack.context","input":{}}}}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"call","arguments":{"operation":"slack.reply","input":{"text":"Answer '"$seg"'"},"request_id":"reply-'"$seg"'"}}}' \
  '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wait","arguments":{"match":{"types":["slack.message.human"]},"timeout_seconds":604800,"request_id":"wait-'"$seg"'"}}}' \
  | env OS_ENDPOINT="$ep" OS_TOKEN_FILE="$tf" "$cmd" mcp > "$f/mcp-out-$seg"
echo '{"type":"user"}' >> "$t/$session.jsonl"
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"%s","num_turns":1,"total_cost_usd":0.01,"structured_output":{"state":"help","summary":"segment %s","handoff":"note"},"modelUsage":{"claude-x":{"inputTokens":1,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}\n' "$session" "$seg"
`

// supportDaemonConfig is the scope `init slack` writes (support binding on
// claude-code, workspace-free, the slack route with active policy inject),
// pointed at the fake Slack and the fake claude.
func supportDaemonConfig(apiURL, root, binary string) []byte {
	ops := []string{"read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"reconcile_seconds": 3600},
		"connections": []any{map[string]any{"id": "slack", "type": "slack", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"bot_token": "env:OC_TEST_SLACK_BOT_TOKEN", "app_token": "env:OC_TEST_SLACK_APP_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "support", "name": "Slack support",
			"authorization": map[string]any{"connections": []string{"slack"}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "support", "executor": "claude-code", "connection": "slack",
				"requires":   map[string]any{"toolbelt": "mcp", "inject": true, "resume": true, "workspace": false},
				"operations": ops, "secret_names": []any{},
				"config": map[string]any{"binary": binary, "playbook": "slack-thread", "args": "{key}", "cold_turns": "resume",
					"env": map[string]string{"FAKE_DIR": root, "CLAUDE_CONFIG_DIR": filepath.Join(root, "claude-config")}}}},
			"routes": []any{map[string]any{"name": "conversations", "handler": "slack", "binding": "support", "active_policy": "inject", "config": map[string]any{}}},
			"extensions": []any{map[string]any{"name": "support", "type": "slack", "connection": "slack", "secret_names": []any{},
				"config": map[string]any{"namespace": "slack/T0TEAM", "scope": "support", "team": "T0TEAM", "channels": []string{"C0SUPPORT"}, "bot_user_id": "UBOT", "receiver": "socket"}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSlackSupportConversation is workflow B on fixtures with the real
// support binding (claude-code, no workspace, no Git provider): the first
// turn investigates, replies and waits on the thread; a human reply wakes and
// resumes the same session; a human message during a running turn is
// injected and consumed (the turn's wait is not woken by it and no other
// attempt starts); and a restart mid-wait keeps the thread's correlation, so
// the next reply resumes the same session again.
func TestDaemonSlackSupportConversation(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon drills")
	}
	fake := slacktest.New("T0TEAM", "xoxb-test")
	defer fake.Close()
	fake.AddAppToken("xapp-test")
	fake.SetClock(time.Now().Add(-time.Minute))
	root := fake.Post("C0SUPPORT", "UALICE", "The VPN drops every hour")
	t.Setenv("OC_TEST_SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("OC_TEST_SLACK_APP_TOKEN", "xapp-test")
	d := newDrill(t)
	binary := filepath.Join(d.root, "claude")
	if err := os.WriteFile(binary, []byte(supportClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, supportDaemonConfig(fake.URL, d.root, binary), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	invocations := func() []string {
		b, _ := os.ReadFile(filepath.Join(d.root, "invocations"))
		if len(strings.TrimSpace(string(b))) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	conversation := func() client.Work {
		page, err := d.c.Works(ctx, nil)
		if err != nil || len(page.Items) != 1 {
			return client.Work{}
		}
		return page.Items[0]
	}
	replies := func() int { return len(fake.AgentReplies("C0SUPPORT", root)) }
	waiting := func(segments, answers int) func() bool {
		return func() bool {
			w := conversation()
			if w.ID == "" || w.State != "waiting" || len(invocations()) != segments || replies() != answers {
				return false
			}
			x := d.executions(w.ID)
			return len(x) == 1 && x[0].State == "parked"
		}
	}
	human := func(text string) {
		t.Helper()
		fake.SetClock(time.Now())
		ts := fake.Reply("C0SUPPORT", root, "UALICE", text)
		env := fake.SocketSend(fake.MessageEvent("C0SUPPORT", ts))
		if env == "" {
			t.Fatal("no socket open")
		}
		d.until("envelope acknowledged", 30*time.Second, func() bool { return fake.Acked(env) })
	}

	// Turn 1: the reconcile starts the conversation; the agent replies and
	// parks on the thread.
	d.until("first answer, parked", 45*time.Second, waiting(1, 1))
	w := conversation()
	if w.Kind != "slack.conversation" {
		t.Fatalf("work %+v", w)
	}
	session := strings.TrimPrefix(strings.Fields(invocations()[0])[1], "session=")
	if !strings.Contains(readFile(t, filepath.Join(d.root, "stdin-1")), "slack-thread") || !strings.Contains(readFile(t, filepath.Join(d.root, "stdin-1")), "The VPN drops every hour") {
		t.Fatalf("first prompt:\n%s", readFile(t, filepath.Join(d.root, "stdin-1")))
	}
	if out := readFile(t, filepath.Join(d.root, "mcp-out-1")); strings.Contains(out, `"isError":true`) {
		t.Fatalf("toolbelt refused the support turn:\n%s", out)
	}
	// Workspace-free: the session runs in the executor's private directory.
	if !strings.Contains(invocations()[0], filepath.Join("executors", "claude-code", "work")+"/") {
		t.Fatalf("a workspace reached the support turn: %s", invocations()[0])
	}
	d.until("socket connected", 30*time.Second, func() bool { return fake.SocketsOpen() == 1 })

	// A human reply wakes the parked turn: same attempt, same session.
	human("Still dropping after the update")
	d.until("second answer, parked again", 45*time.Second, waiting(2, 2))
	if inv := invocations(); !strings.Contains(inv[1], "session="+session+" resume=1") {
		t.Fatalf("the wake did not resume the session: %v", inv)
	}
	if !strings.Contains(readFile(t, filepath.Join(d.root, "stdin-2")), "Still dropping after the update") {
		t.Fatalf("resumed prompt lacks the reply:\n%s", readFile(t, filepath.Join(d.root, "stdin-2")))
	}

	// A message during a running turn is injected and consumed: the turn
	// holds for it, takes it in, answers, and its wait is not woken by it.
	if err := os.WriteFile(filepath.Join(d.root, "hold-for"), []byte("PAUSE"), 0o600); err != nil {
		t.Fatal(err)
	}
	human("PAUSE: one more detail coming")
	d.until("third segment running", 45*time.Second, func() bool { return exists(filepath.Join(d.root, "started-3")) })
	human("It only happens on wifi")
	d.until("third answer, parked", 45*time.Second, waiting(3, 3))
	time.Sleep(3 * time.Second)
	if !waiting(3, 3)() {
		t.Fatalf("a consumed injection woke the wait or started another segment: %v", invocations())
	}
	stdin3 := readFile(t, filepath.Join(d.root, "stdin-3"))
	if strings.Count(stdin3, "It only happens on wifi") != 1 || !strings.Contains(stdin3, "Arrived during this turn") {
		t.Fatalf("the interjection did not reach the turn exactly once:\n%s", stdin3)
	}

	// Restart mid-wait: the thread still resolves to the same Work, and the
	// next reply resumes the same session.
	d.kill()
	d.start("")
	d.until("socket reconnected", 30*time.Second, func() bool { return fake.SocketConnects() >= 2 && fake.SocketsOpen() == 1 })
	if got := conversation(); got.ID != w.ID || got.State != "waiting" {
		t.Fatalf("after restart: %+v", got)
	}
	_ = os.Remove(filepath.Join(d.root, "hold-for"))
	human("Thanks, any update?")
	d.until("fourth answer after restart", 45*time.Second, waiting(4, 4))
	if inv := invocations(); !strings.Contains(inv[3], "session="+session+" resume=1") {
		t.Fatalf("after restart the wake did not resume the session: %v", inv)
	}
	report, err := d.c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := check(report, "github"); c.Code != "not_configured" {
		t.Fatalf("doctor github: %+v", c)
	}
	if c := check(report, "resources"); c.Status == "fail" {
		t.Fatalf("doctor resources: %+v", c)
	}
}
