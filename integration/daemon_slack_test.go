package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

// slackDaemonConfig binds a workspace-free support scope: no resource, no Git
// or GitHub entry. Its shell binding stands in for an agent: it replies in the
// thread through `os call slack.reply` and hands back, so the conversation
// stays open for the thread's next message.
func slackDaemonConfig(apiURL string) []byte {
	script := `os call slack.reply --data '{"text":"Looking into it."}' --request-id ack >/dev/null && ` +
		`os outcome handoff --summary replied --request-id done >/dev/null`
	ops := []string{"read_context", "declare_outcome", "slack.reply", "slack.context", "slack.thread"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"reconcile_seconds": 3600},
		"connections": []any{map[string]any{"id": "slack", "type": "slack", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"bot_token": "env:OC_TEST_SLACK_BOT_TOKEN", "app_token": "env:OC_TEST_SLACK_APP_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "support", "name": "Support",
			"authorization": map[string]any{"connections": []string{"slack"}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops,
				"secret_names": []any{}, "config": map[string]any{"command": script}}},
			"routes": []any{map[string]any{"name": "threads", "handler": "slack", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"extensions": []any{map[string]any{"name": "helpdesk", "type": "slack", "connection": "slack", "secret_names": []any{},
				"config": map[string]any{"namespace": "slack/T0TEAM", "scope": "support", "team": "T0TEAM", "channels": []string{"C0SUPPORT"}, "bot_user_id": "UBOT"}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSlackSocketModeConversation drives the composed `serve` daemon
// with no Git provider: the first reconcile starts a conversation for a
// thread, the agent stand-in replies once through the toolbelt, a human
// reply arrives over Socket Mode (acknowledged after acceptance) and resumes
// the same Work, the agent's own replies never start another attempt, and a
// restart reconnects the socket and posts nothing again.
func TestDaemonSlackSocketModeConversation(t *testing.T) {
	fake := slacktest.New("T0TEAM", "xoxb-test")
	defer fake.Close()
	fake.AddAppToken("xapp-test")
	fake.SetClock(time.Now().Add(-time.Minute))
	fake.Join("C0SUPPORT", "UBOB")
	root := fake.Post("C0SUPPORT", "UALICE", "The VPN drops every hour")
	t.Setenv("OC_TEST_SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("OC_TEST_SLACK_APP_TOKEN", "xapp-test")
	d := newDrill(t)
	if err := os.WriteFile(d.config, slackDaemonConfig(fake.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	work := func() (string, string) {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"support"}})
		if err != nil || len(page.Items) != 1 {
			return "", ""
		}
		return page.Items[0].ID, page.Items[0].Kind + ":" + page.Items[0].State
	}
	attempts := func(id string) int {
		page, err := d.c.Executions(ctx, url.Values{"work_id": {id}})
		if err != nil {
			return -1
		}
		return len(page.Items)
	}
	d.until("conversation started and replied", 30*time.Second, func() bool {
		return len(fake.AgentReplies("C0SUPPORT", root)) == 1
	})
	id, state := work()
	if !strings.HasPrefix(state, "slack.conversation:") {
		t.Fatalf("work: %s %s", id, state)
	}
	d.until("socket connected", 30*time.Second, func() bool { return fake.SocketsOpen() == 1 })
	// The human answer arrives over Socket Mode only (reconcile is hourly).
	fake.SetClock(time.Now())
	reply := fake.Reply("C0SUPPORT", root, "UALICE", "Still dropping after the update")
	env := fake.SocketSend(fake.MessageEvent("C0SUPPORT", reply))
	if env == "" {
		t.Fatal("no socket open")
	}
	d.until("envelope acknowledged", 30*time.Second, func() bool { return fake.Acked(env) })
	d.until("same conversation replied again", 30*time.Second, func() bool {
		return len(fake.AgentReplies("C0SUPPORT", root)) == 2
	})
	// The agent's own replies pushed back over the socket are never input.
	for _, m := range fake.AgentReplies("C0SUPPORT", root) {
		e := fake.SocketSend(fake.MessageEvent("C0SUPPORT", m.TS))
		d.until("own reply acknowledged", 30*time.Second, func() bool { return fake.Acked(e) })
	}
	time.Sleep(3 * time.Second)
	if again, _ := work(); again != id || attempts(id) != 2 || len(fake.AgentReplies("C0SUPPORT", root)) != 2 {
		t.Fatalf("after own replies: work %s attempts %d replies %d", again, attempts(id), len(fake.AgentReplies("C0SUPPORT", root)))
	}
	report, err := d.c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := check(report, "slack"); c.Code != "ok" || !strings.Contains(strings.Join(c.Items, " "), "socket connected") {
		t.Fatalf("doctor slack: %+v", c)
	}
	if c := check(report, "github"); c.Code != "not_configured" {
		t.Fatalf("doctor github: %+v", c)
	}
	// Restart: the socket reconnects and nothing is posted or claimed again.
	d.kill()
	d.start("")
	d.until("socket reconnected", 30*time.Second, func() bool { return fake.SocketConnects() >= 2 && fake.SocketsOpen() == 1 })
	time.Sleep(3 * time.Second)
	if again, _ := work(); again != id || attempts(id) != 2 || len(fake.AgentReplies("C0SUPPORT", root)) != 2 {
		t.Fatalf("after restart: work %s attempts %d replies %d", again, attempts(id), len(fake.AgentReplies("C0SUPPORT", root)))
	}
}

// TestDaemonSlackHTTPEventsAPI: receiver http mounts the entry's Events API
// Request URL on the webhook listener: URL verification is echoed, a signed
// callback is accepted (200) and starts the conversation long before the
// next reconcile, and a forged one is refused (401).
func TestDaemonSlackHTTPEventsAPI(t *testing.T) {
	fake := slacktest.New("T0TEAM", "xoxb-test")
	defer fake.Close()
	fake.SetClock(time.Now().Add(-time.Minute))
	fake.Join("C0SUPPORT", "UBOB")
	t.Setenv("OC_TEST_SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("OC_TEST_SLACK_SIGNING", "signing-secret")
	var cfg map[string]any
	_ = json.Unmarshal(slackDaemonConfig(fake.URL), &cfg)
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	scope["secret_refs"] = map[string]any{"signing": "env:OC_TEST_SLACK_SIGNING"}
	x := scope["extensions"].([]any)[0].(map[string]any)
	x["secret_names"] = []string{"signing"}
	xc := x["config"].(map[string]any)
	xc["receiver"], xc["signing_secret"] = "http", "signing"
	data, _ := json.Marshal(cfg)
	d := newDrill(t)
	if err := os.WriteFile(d.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0"}
	d.start("")
	ctx := context.Background()
	var addr string
	d.until("listener with the Slack route", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if err != nil {
			return false
		}
		c := check(r, "webhooks")
		m := listenerAddr.FindStringSubmatch(c.Detail)
		if m == nil || !strings.Contains(strings.Join(c.Items, " "), "POST /slack/support/helpdesk") {
			return false
		}
		addr = m[1]
		return strings.Contains(strings.Join(check(r, "slack").Items, " "), "reconciled")
	})
	post := func(body []byte, secret string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/slack/support/helpdesk", bytes.NewReader(body))
		req.Header = slacktest.Headers(secret, body, time.Now())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	challenge := []byte(`{"type":"url_verification","challenge":"c-123"}`)
	if code, body := post(challenge, "signing-secret"); code != http.StatusOK || body != "c-123" {
		t.Fatalf("url verification: %d %q", code, body)
	}
	fake.SetClock(time.Now())
	root := fake.Post("C0SUPPORT", "UALICE", "Wifi on floor 2 is down")
	event := fake.MessageEvent("C0SUPPORT", root)
	if code, _ := post(event, "forged"); code != http.StatusUnauthorized {
		t.Fatalf("forged: %d", code)
	}
	if code, _ := post(event, "signing-secret"); code != http.StatusOK {
		t.Fatalf("signed: %d", code)
	}
	d.until("pushed thread replied", 30*time.Second, func() bool { return len(fake.AgentReplies("C0SUPPORT", root)) == 1 })
}
