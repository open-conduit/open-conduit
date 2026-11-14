package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/extensions/webhook"
)

// slackPostDaemonConfig is a scope whose one binding, job, is bound by a
// webhook route, a work route and a slack route, in that order. A delivery's
// turn posts its report to C0SUPPORT with slack.post and completes; a turn
// for anything else answers in its thread with slack.reply and completes.
// The slack entry's post config lets job post in C0SUPPORT with follow_up
// set and no grants.
func slackPostDaemonConfig(apiURL string) []byte {
	script := `if grep -q '"type": *"webhook.delivery"' "$OS_INPUT_FILE"; then ` +
		`os call slack.post --data '{"channel":"C0SUPPORT","text":"Deploy of checkout failed"}' --request-id report >/dev/null; ` +
		`else os call slack.reply --data '{"text":"It failed on the migration."}' --request-id answer >/dev/null; fi && ` +
		`os outcome completed --summary done --request-id done >/dev/null`
	ops := []string{"read_context", "declare_outcome", "slack.post", "slack.reply", "slack.context", "slack.thread"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"reconcile_seconds": 3600},
		"connections": []any{map[string]any{"id": "slack", "type": "slack", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"bot_token": "env:OC_TEST_SLACK_BOT_TOKEN", "app_token": "env:OC_TEST_SLACK_APP_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "support", "name": "Support", "secret_refs": map[string]any{"hook": "env:OC_TEST_HOOK_SECRET"},
			"authorization": map[string]any{"connections": []string{"slack"}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "job", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops,
				"secret_names": []any{}, "config": map[string]any{"command": script}}},
			"routes": []any{
				map[string]any{"name": "deliveries", "handler": "webhook", "binding": "job", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "children", "handler": "work", "binding": "job", "active_policy": "queue", "config": map[string]any{"kinds": []any{map[string]any{"kind": "eng.task"}}}},
				map[string]any{"name": "threads", "handler": "slack", "binding": "job", "active_policy": "queue", "config": map[string]any{"channels": []string{"C0SUPPORT"}}},
			},
			"extensions": []any{
				map[string]any{"name": "inbox", "type": "webhook", "secret_names": []string{"hook"},
					"config": map[string]any{"scope": "support", "webhook_secret": "hook", "hooks": []any{
						map[string]any{"name": "deploys", "delivery_key": "header:X-Delivery-Id", "correlation_key": "body:/service"}}}},
				map[string]any{"name": "helpdesk", "type": "slack", "connection": "slack", "secret_names": []any{},
					"config": slackEntry(map[string]any{"start": "mention", "post": []any{
						map[string]any{"binding": "job", "channels": []string{"C0SUPPORT"}, "follow_up": map[string]any{}}}})},
			},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSlackReplyToAFinishedDeliveryOpensItsSuccessor: a webhook
// delivery's Work posts its report to Slack and completes in its one turn.
// A person's reply in the post's thread opens the delivery's successor on
// the binding that posted it, which answers in the thread; the finished
// Work is not changed. The scope's webhook and work routes come before its
// slack route, and its post config sets follow_up with no grants.
func TestDaemonSlackReplyToAFinishedDeliveryOpensItsSuccessor(t *testing.T) {
	fake := slacktest.New("T0TEAM", "xoxb-test")
	defer fake.Close()
	fake.AddAppToken("xapp-test")
	fake.Join("C0SUPPORT", "UBOB")
	t.Setenv("OC_TEST_SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("OC_TEST_SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	if err := os.WriteFile(d.config, slackPostDaemonConfig(fake.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		page, _ := d.c.Works(ctx, url.Values{"scope_id": {"support"}})
		for _, w := range page.Items {
			t.Logf("work %s %s %s keys %+v", w.ID, w.Kind, w.State, w.CorrelationKeys)
			xs, _ := d.c.Executions(ctx, url.Values{"work_id": {w.ID}})
			for _, x := range xs.Items {
				t.Logf("  execution %s %s outcome %+v", x.ID, x.State, x.Outcome)
			}
		}
		events, _ := d.c.Events(ctx, url.Values{"scope_id": {"support"}})
		for _, e := range events.Items {
			r, _ := d.c.EventRoute(ctx, e.ID)
			t.Logf("event %s %s: %+v", e.ID, e.Type, r)
		}
	})
	var addr string
	d.until("webhook listener", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if err != nil {
			return false
		}
		m := listenerAddr.FindStringSubmatch(check(r, "webhooks").Detail)
		if m == nil {
			return false
		}
		addr = m[1]
		return true
	})
	d.until("socket connected", 30*time.Second, func() bool { return fake.SocketsOpen() == 1 })
	fake.SetClock(time.Now())
	ns := webhook.Namespace("support", "inbox")
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/"+ns+"/deploys/"+webhook.Token("hook-secret", ns, "deploys", 0),
		bytes.NewReader([]byte(`{"service":"checkout","status":"failed"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Delivery-Id", "d-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery: %d", res.StatusCode)
	}
	works := func() map[string]client.Work {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"support"}})
		if err != nil {
			return nil
		}
		out := map[string]client.Work{}
		for _, w := range page.Items {
			out[w.ID] = w
		}
		return out
	}
	var delivery client.Work
	d.until("the delivery's Work posted its report and completed", 30*time.Second, func() bool {
		for _, w := range works() {
			if w.Kind == webhook.WorkKind && w.State == "completed" {
				delivery = w
				return true
			}
		}
		return false
	})
	var post string
	d.until("the post's thread is the delivery's", 30*time.Second, func() bool {
		w, err := d.c.Work(ctx, delivery.ID)
		if err != nil {
			return false
		}
		for _, k := range w.CorrelationKeys {
			if rest, ok := strings.CutPrefix(k.ExternalID, "channel/C0SUPPORT/thread/"); ok {
				if ts, ok := strings.CutSuffix(rest, "/posted/job"); ok {
					post = ts
					return true
				}
			}
		}
		return false
	})
	if m, ok := fake.Message("C0SUPPORT", post); !ok || m.Text != "Deploy of checkout failed" {
		t.Fatalf("the report is not in the channel: %+v %v", m, ok)
	}

	fake.SetClock(time.Now())
	reply := fake.Reply("C0SUPPORT", post, "UALICE", "Which step failed?")
	if fake.SocketSend(fake.MessageEvent("C0SUPPORT", reply)) == "" {
		t.Fatal("no socket open")
	}
	successor := delivery.ID + "/g2"
	d.until("the successor answered in the thread", 30*time.Second, func() bool {
		for _, m := range fake.AgentReplies("C0SUPPORT", post) {
			if m.Text == "It failed on the migration." {
				return true
			}
		}
		return false
	})
	w, ok := works()[successor]
	if !ok || w.Kind != webhook.WorkKind {
		t.Fatalf("no successor %s: %+v", successor, works())
	}
	if old, err := d.c.Work(ctx, delivery.ID); err != nil || old.State != "completed" {
		t.Fatalf("the finished delivery changed: %+v %v", old, err)
	}
}
