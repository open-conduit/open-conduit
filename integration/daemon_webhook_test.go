package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/webhook"
)

// webhookDaemonConfig has one webhook entry with a deploys hook
// (deduplicated by a header, threaded by the body's service) and a shell
// binding that keeps a copy of each run's input file.
func webhookDaemonConfig(root string) []byte {
	script := `cp "$OS_INPUT_FILE" "$(mktemp "` + root + `/run-XXXXXX")" && os call webhook.delivery.read --data "{\"event\": \"$(grep -o 'webhook-[0-9a-f]\{40\}' "$OS_INPUT_FILE" | head -1)\"}" > "$(mktemp "` + root + `/read-XXXXXX")"`
	ops := []string{"read_context", "webhook.delivery.read"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Operations", "secret_refs": map[string]any{"hook": "env:OC_TEST_HOOK_SECRET"},
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"extensions": []any{map[string]any{"name": "inbox", "type": "webhook", "secret_names": []string{"hook"},
				"config": map[string]any{"scope": "ops", "webhook_secret": "hook", "hooks": []any{
					map[string]any{"name": "deploys", "delivery_key": "header:X-Delivery-Id", "correlation_key": "body:/service"}}}}},
			"bindings": []any{map[string]any{"name": "triage", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
				"config": map[string]any{"command": script}}},
			"routes": []any{map[string]any{"name": "deliveries", "handler": "webhook", "binding": "triage", "active_policy": "queue", "config": map[string]any{}}},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonWebhookDeliveries drives the composed daemon with the webhook
// package: each delivery to a signed URL becomes Work of its own and runs
// with its body as the trigger in OS_INPUT_FILE, a sender's retry makes no
// second Work, a wrong token is refused, and doctor --check-secrets
// prints the signed URL.
func TestDaemonWebhookDeliveries(t *testing.T) {
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	runs := filepath.Join(d.root, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, webhookDaemonConfig(runs), 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
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
		return strings.Contains(check(r, "webhook").Detail, "hook deploys: POST https://hooks.example.test/webhook/ops/inbox/deploys/<token>")
	})
	ns := webhook.Namespace("ops", "inbox")
	path := "/" + ns + "/deploys/" + webhook.Token("hook-secret", ns, "deploys")
	post := func(path, id, body string) int {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Delivery-Id", id)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("/"+ns+"/deploys/wrong", "d-0", `{}`); code != http.StatusNotFound {
		t.Fatalf("wrong token: %d", code)
	}
	for _, c := range []struct{ id, body string }{
		{"d-1", `{"service":"checkout","status":"failed"}`},
		{"d-1", `{"service":"checkout","status":"failed"}`},
		{"d-2", `{"service":"billing","status":"failed"}`},
	} {
		if code := post(path, c.id, c.body); code != http.StatusAccepted {
			t.Fatalf("%s: %d", c.id, code)
		}
	}
	works := func() []string {
		page, err := d.c.Works(ctx, url.Values{"kind": {webhook.WorkKind}})
		if err != nil {
			return nil
		}
		var out []string
		for _, w := range page.Items {
			if w.State == "completed" {
				out = append(out, string(w.ID))
			}
		}
		return out
	}
	d.until("each delivery's Work has run", 30*time.Second, func() bool { return len(works()) == 2 })
	files, _ := filepath.Glob(filepath.Join(runs, "run-*"))
	if len(files) != 2 {
		t.Fatalf("runs: %v", files)
	}
	var bodies []string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		bodies = append(bodies, string(b))
	}
	all := strings.Join(bodies, "\n")
	if !strings.Contains(all, `\"status\":\"failed\"`) && !strings.Contains(all, `"status":"failed"`) || !strings.Contains(all, "checkout") || !strings.Contains(all, "billing") {
		t.Fatalf("input files: %s", all)
	}
	// Each run read its own delivery, whole, through the toolbelt.
	reads, _ := filepath.Glob(filepath.Join(runs, "read-*"))
	var services []string
	for _, f := range reads {
		var res struct {
			Data webhook.ReadOutput `json:"data"`
		}
		b, _ := os.ReadFile(f)
		if json.Unmarshal(b, &res) != nil || res.Data.Hook != "deploys" {
			t.Fatalf("read output %s: %q", f, b)
		}
		var body struct{ Service string }
		_ = json.Unmarshal(res.Data.Body, &body)
		services = append(services, body.Service)
	}
	if sort.Strings(services); strings.Join(services, ",") != "billing,checkout" {
		t.Fatalf("read services: %v", services)
	}
	r, err := d.c.Doctor(ctx, true)
	if err != nil || !strings.Contains(check(r, "webhook-urls").Detail, "https://hooks.example.test"+path) {
		t.Fatalf("doctor --check-secrets: %+v %v", check(r, "webhook-urls"), err)
	}

	// A hook added to the entry, and a second entry, by config apply serve
	// their URLs at once, without a restart.
	var cfg map[string]any
	_ = json.Unmarshal(webhookDaemonConfig(runs), &cfg)
	scope := cfg["scopes"].([]any)[0].(map[string]any)
	inbox := scope["extensions"].([]any)[0].(map[string]any)
	ic := inbox["config"].(map[string]any)
	ic["hooks"] = append(ic["hooks"].([]any), map[string]any{"name": "forms"})
	scope["extensions"] = append(scope["extensions"].([]any), map[string]any{"name": "alerts", "type": "webhook", "secret_names": []string{"hook"},
		"config": map[string]any{"scope": "ops", "webhook_secret": "hook", "hooks": []any{map[string]any{"name": "pages"}}}})
	next, _ := json.Marshal(cfg)
	v, err := d.c.ValidateConfig(ctx, next)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if applied, err := d.c.ApplyConfig(ctx, next, v.CandidateHash, "1", "apply-hooks"); err != nil || applied.Generation != "2" {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	alerts := webhook.Namespace("ops", "alerts")
	added := []string{"/" + ns + "/forms/" + webhook.Token("hook-secret", ns, "forms"), "/" + alerts + "/pages/" + webhook.Token("hook-secret", alerts, "pages")}
	// Neither added hook has a delivery key, so each accepted POST is a
	// delivery: post to each until one is accepted, and no more.
	for _, p := range added {
		d.until("the added hook "+p+" takes a delivery", 30*time.Second, func() bool { return post(p, "", `{"n":1}`) == http.StatusAccepted })
	}
	d.until("each added hook's delivery has run", 30*time.Second, func() bool { return len(works()) == 4 })
}

// TestDaemonWebhookThreadGenerations drives the composed daemon through a
// thread that ends and starts again. A ticket's first delivery has Work of
// its own, which finishes. The ticket's next delivery opens a successor,
// linked to it, which parks on a type-only wait. The delivery after that
// wakes the successor rather than starting more Work, and the first Work
// keeps the ticket's key.
func TestDaemonWebhookThreadGenerations(t *testing.T) {
	t.Setenv("OC_TEST_HOOK_SECRET", "hook-secret")
	d := newDrill(t)
	runs := filepath.Join(d.root, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	// The run that is handed the ticket's "again" delivery waits for more of
	// the thread; every other run ends.
	script := `cp "$OS_INPUT_FILE" "$(mktemp "` + runs + `/run-XXXXXX")"; if grep -q again "$OS_INPUT_FILE" && [ ! -e "` + runs + `/waited" ]; then touch "` + runs + `/waited"; os wait --type webhook.delivery --timeout 3600 --request-id more > /dev/null; fi`
	ops := []string{"read_context", "wait"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Operations", "secret_refs": map[string]any{"hook": "env:OC_TEST_HOOK_SECRET"},
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"extensions": []any{map[string]any{"name": "inbox", "type": "webhook", "secret_names": []string{"hook"},
				"config": map[string]any{"scope": "ops", "webhook_secret": "hook", "hooks": []any{
					map[string]any{"name": "tickets", "delivery_key": "header:X-Delivery-Id", "correlation_key": "body:/ticket"}}}}},
			"bindings": []any{map[string]any{"name": "triage", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
				"config": map[string]any{"command": script}}},
			"routes": []any{map[string]any{"name": "deliveries", "handler": "webhook", "binding": "triage", "active_policy": "queue", "config": map[string]any{}}},
		}},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(d.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d.extra = []string{"--webhook-addr", "127.0.0.1:0", "--public-url", "https://hooks.example.test"}
	d.start("")
	ctx := context.Background()
	var addr string
	d.until("webhook listener", 30*time.Second, func() bool {
		r, err := d.c.Doctor(ctx, false)
		if err != nil {
			return false
		}
		m := listenerAddr.FindStringSubmatch(check(r, "webhooks").Detail)
		if m != nil {
			addr = m[1]
		}
		return m != nil
	})
	ns := webhook.Namespace("ops", "inbox")
	path := "/" + ns + "/tickets/" + webhook.Token("hook-secret", ns, "tickets")
	post := func(id, body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Delivery-Id", id)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: %d", id, res.StatusCode)
		}
	}
	works := func() map[string]string {
		page, err := d.c.Works(ctx, url.Values{"kind": {webhook.WorkKind}})
		out := map[string]string{}
		if err == nil {
			for _, w := range page.Items {
				out[w.ID] = w.State
			}
		}
		return out
	}
	first := ns + "/" + webhook.Thread("tickets", "T-1", "")
	second := string(webhook.NextGeneration(domain.ID(first)))
	post("t-1", `{"ticket":"T-1","step":"opened"}`)
	d.until("the ticket's first Work finishes", 30*time.Second, func() bool { return works()[first] == "completed" })
	post("t-2", `{"ticket":"T-1","step":"again"}`)
	d.until("its successor parks on the thread", 30*time.Second, func() bool { w := works(); return w[first] == "completed" && w[second] == "waiting" })
	post("t-3", `{"ticket":"T-1","step":"more"}`)
	d.until("the next delivery wakes the successor", 30*time.Second, func() bool { return works()[second] == "completed" })
	if w := works(); len(w) != 2 {
		t.Fatalf("works: %v", w)
	}
	page, err := d.c.Events(ctx, url.Values{"type": {webhook.TypeDelivery}})
	if err != nil {
		t.Fatal(err)
	}
	routed := map[string]string{}
	for _, e := range page.Items {
		var p webhook.Payload
		if e.Data != nil {
			_ = json.Unmarshal(e.Data.Data, &p)
		}
		routed[p.Delivery] = e.RoutedWorkID
	}
	if routed["t-1"] != first || routed["t-2"] != second || routed["t-3"] != second {
		t.Fatalf("routed: %v", routed)
	}
	created, err := d.c.Events(ctx, url.Values{"type": {"core.work.created"}})
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, e := range created.Items {
		linked = linked || e.WorkID == second && e.Data != nil && strings.Contains(string(e.Data.Data), `"PredecessorID":"`+first+`"`)
	}
	if !linked {
		t.Fatalf("no core.work.created links %s to %s: %+v", second, first, created.Items)
	}
}
