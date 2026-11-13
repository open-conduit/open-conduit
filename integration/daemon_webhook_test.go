package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/webhook"
)

// webhookDaemonConfig has one webhook entry with a deploys hook
// (deduplicated by a header, threaded by the body's service) and a shell
// binding that keeps a copy of each run's input file.
func webhookDaemonConfig(root string) []byte {
	script := `cp "$OS_INPUT_FILE" "$(mktemp "` + root + `/run-XXXXXX")"`
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Operations", "secret_refs": map[string]any{"hook": "env:OC_TEST_HOOK_SECRET"},
			"authorization": map[string]any{"connections": []any{}, "operations": []string{"read_context"}, "read_scopes": []any{}},
			"extensions": []any{map[string]any{"name": "inbox", "type": "webhook", "secret_names": []string{"hook"},
				"config": map[string]any{"scope": "ops", "webhook_secret": "hook", "hooks": []any{
					map[string]any{"name": "deploys", "delivery_key": "header:X-Delivery-Id", "correlation_key": "body:/service"}}}}},
			"bindings": []any{map[string]any{"name": "triage", "executor": "shell", "requires": map[string]any{}, "operations": []string{"read_context"}, "secret_names": []any{},
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
	r, err := d.c.Doctor(ctx, true)
	if err != nil || !strings.Contains(check(r, "webhook-urls").Detail, "https://hooks.example.test"+path) {
		t.Fatalf("doctor --check-secrets: %+v %v", check(r, "webhook-urls"), err)
	}
}
