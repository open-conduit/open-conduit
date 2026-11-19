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
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

// childRequestsFront hands the thread's task to a specialist and hands back
// to the person, as a front agent does after create_work.
const childRequestsFront = `set -e
os work create --kind support.task --request-id child > /dev/null
os call slack.reply --data '{"text":"Steve is on it."}' --request-id ack > /dev/null
os outcome handoff --summary handed --request-id done > /dev/null
`

// childRequestsSpecialist makes a call the human entry gates for a Slack
// person, and parks on its approval.
const childRequestsSpecialist = `set -e
os call ` + is.OpList + ` --data '{"state":"all"}' --request-id gated > "$CR_DIR/gate.out" 2>&1
`

func childRequestsConfig(apiURL, dir string) []byte {
	shell := func(name, script string, ops, kinds []string) map[string]any {
		return map[string]any{"name": name, "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "work_kinds": kinds,
			"secret_names": []any{}, "config": map[string]any{"command": script, "env": map[string]string{"CR_DIR": dir}}}
	}
	front := []string{"read_context", "declare_outcome", "create_work", "slack.reply"}
	specialist := []string{"read_context", "declare_outcome", is.OpList}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{"reconcile_seconds": 3600},
		"connections": []any{map[string]any{"id": "slack", "type": "slack", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"bot_token": "env:OC_TEST_SLACK_BOT_TOKEN", "app_token": "env:OC_TEST_SLACK_APP_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "support", "name": "Support",
			"authorization": map[string]any{"connections": []string{"slack"}, "operations": append(append([]string{}, front...), is.OpList), "read_scopes": []any{}, "work_kinds": []string{"support.task"}},
			"bindings": []any{
				shell("front", childRequestsFront, front, []string{"support.task"}),
				shell("steve", childRequestsSpecialist, specialist, []string{}),
			},
			"routes": []any{
				map[string]any{"name": "threads", "handler": "slack", "binding": "front", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "children", "handler": "work", "binding": "steve", "active_policy": "queue", "config": map[string]any{"kinds": []any{map[string]any{"kind": "support.task"}}}},
			},
			"extensions": []any{
				map[string]any{"name": "helpdesk", "type": "slack", "connection": "slack", "secret_names": []any{},
					"config": slackEntry(map[string]any{"claim_from": "all", "progress": map[string]any{"enabled": true}, "child_requests": true})},
				map[string]any{"name": "reports", "type": is.Type, "config": map[string]any{}, "secret_names": []any{}},
				map[string]any{"name": "decisions", "type": "human", "secret_names": []any{},
					"config": map[string]any{"gates": []any{map[string]any{"operations": []string{is.OpList}, "for_bindings": []string{"steve"},
						"responders": []string{"operator:local", "slack:UALICE"}, "timeout": "2h"}}}},
			},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSlackShowsAChildsGatedCall drives the composed daemon (issue
// #759): a conversation hands its task to a specialist, whose gated call
// parks on an approval a Slack person may give. The request shows in the
// conversation's thread: as a task of the run's progress message and as a
// prompt with its buttons.
func TestDaemonSlackShowsAChildsGatedCall(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	fake := slacktest.New("T0TEAM", "xoxb-test")
	defer fake.Close()
	fake.AddAppToken("xapp-test")
	fake.SetClock(time.Now().Add(-time.Minute))
	fake.Join("C0SUPPORT", "UBOB")
	root := fake.Post("C0SUPPORT", "UALICE", "Fix the VPN config")
	t.Setenv("OC_TEST_SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("OC_TEST_SLACK_APP_TOKEN", "xapp-test")
	d := newDrill(t)
	dir := filepath.Join(d.root, "cr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, childRequestsConfig(fake.URL, dir), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		page, _ := d.c.Works(ctx, url.Values{"scope_id": {"support"}})
		for _, w := range page.Items {
			acts, _ := d.c.Actions(ctx, client.ActionTarget{WorkID: w.ID})
			t.Logf("work %s %s %s parent=%s actions=%+v", w.ID, w.Kind, w.State, w.ParentID, acts)
		}
		for _, m := range fake.Thread("C0SUPPORT", root) {
			b, _ := json.Marshal(m.Blocks)
			t.Logf("message %s %q %s", m.TS, m.Text, b)
		}
		out, _ := os.ReadFile(filepath.Join(dir, "gate.out"))
		t.Logf("gate.out: %s", out)
	})
	d.start("")
	child := func() (string, string) {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"support"}, "kind": {"support.task"}})
		if err != nil || len(page.Items) != 1 {
			return "", ""
		}
		return page.Items[0].ID, page.Items[0].State
	}
	d.until("the child parked on its approval", 60*time.Second, func() bool {
		id, state := child()
		if id == "" || state != "waiting" {
			return false
		}
		acts, _ := d.c.Actions(ctx, client.ActionTarget{WorkID: id})
		return len(acts) > 0
	})
	shown := func() (task, prompt bool) {
		for _, m := range fake.Thread("C0SUPPORT", root) {
			for _, b := range m.Blocks {
				raw, _ := json.Marshal(b)
				switch {
				case b["type"] == "plan" && strings.Contains(string(raw), "steve wants to call "+is.OpList):
					task = true
				case b["type"] == "actions" && strings.HasPrefix(m.Text, "steve wants to call "+is.OpList):
					prompt = true
				}
			}
		}
		return
	}
	d.until("the request shows in the conversation's thread", 30*time.Second, func() bool {
		task, prompt := shown()
		return task && prompt
	})
}
