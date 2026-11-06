package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// observeDaemonConfig has a shell poller, started by hand, that records
// two items of one thread with the os CLI, repeats the first, and tries an
// undeclared source; the observe route gives the thread its own Work.
func observeDaemonConfig(root string) []byte {
	script := `set -e
os call observe.record --data '{"item": "1730451600.000100", "thread": "1730451600.000100", "text": "How do I reset my VPN password?"}' > ` + root + `/first.json
os call observe.record --data '{"item": "1730451600.000100", "thread": "1730451600.000100", "text": "How do I reset my VPN password?"}' > ` + root + `/again.json
os call observe.record --data '{"item": "1730451700.000200", "thread": "1730451600.000100", "text": "It says my token expired"}' > /dev/null
if os call observe.record --data '{"source": "elsewhere", "item": "1", "thread": "1", "text": "x"}' 2> ` + root + `/refused.txt; then exit 9; fi
os outcome completed --summary polled --schema core.turn.v1 --data '{"status":"done"}' --request-id done >/dev/null`
	ops := []string{"read_context", "declare_outcome", "observe.record"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Operations", "secret_refs": map[string]any{},
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"extensions": []any{map[string]any{"name": "help", "type": "observe", "secret_names": []any{},
				"config": map[string]any{"sources": []any{map[string]any{"name": "slack-help", "reporters": []string{"poller"}}}}}},
			"bindings": []any{
				map[string]any{"name": "poller", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "secret_names": []any{},
					"config": map[string]any{"command": script}},
				map[string]any{"name": "responder", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"}, "secret_names": []any{},
					"config": map[string]any{"command": "true"}},
			},
			"routes": []any{
				map[string]any{"name": "incoming", "handler": "manual", "binding": "poller", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "threads", "handler": "observe", "binding": "responder", "active_policy": "queue", "config": map[string]any{"source": "slack-help"}},
			},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonObservedSource drives the composed daemon with observe as its
// bundled package (#428): the route's sample traces to a new thread, a
// shell poller's items are recorded once each through the toolbelt, an
// undeclared source is refused, and the thread gets one Work holding both
// items.
func TestDaemonObservedSource(t *testing.T) {
	d := newDrill(t)
	if err := os.WriteFile(d.config, observeDaemonConfig(d.root), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()

	samples, err := d.c.RouteSamples(ctx, "ops")
	i := slices.IndexFunc(samples, func(s client.RouteSample) bool { return s.Handler == "observe" })
	if err != nil || i < 0 {
		t.Fatalf("samples: %+v %v", samples, err)
	}
	trace, err := d.c.TraceRoute(ctx, "ops", samples[i].TraceEvent)
	if err != nil || trace.Effect.Kind != "start" || trace.Effect.Binding != "responder" {
		t.Fatalf("trace: %+v %v", trace, err)
	}

	if _, err := d.c.SubmitEvent(ctx, client.EventRequest{ScopeID: "ops", Type: "operator.requested", DeliveryID: "poll-1",
		CorrelationKey: &client.Key{Source: "drill", ExternalID: "poll"}}, "submit-poll-1"); err != nil {
		t.Fatal(err)
	}
	threads := func() []client.Work {
		page, err := d.c.Works(ctx, url.Values{"kind": {"observe.thread"}})
		if err != nil {
			return nil
		}
		return page.Items
	}
	polled := func() bool {
		page, err := d.c.Works(ctx, url.Values{"kind": {"manual.work"}})
		return err == nil && len(page.Items) == 1 && page.Items[0].State == "completed"
	}
	d.until("the poller finished and the thread has its Work", 30*time.Second, func() bool { return polled() && len(threads()) == 1 })

	read := func(name string) map[string]any {
		var r struct {
			Data map[string]any `json:"data"`
		}
		b, err := os.ReadFile(filepath.Join(d.root, name))
		if err != nil || json.Unmarshal(b, &r) != nil {
			t.Fatalf("%s: %q %v", name, b, err)
		}
		return r.Data
	}
	if first, again := read("first.json"), read("again.json"); first["duplicate"] != false || again["duplicate"] != true || first["source"] != "slack-help" {
		t.Fatalf("first %v again %v", first, again)
	}
	if refused, _ := os.ReadFile(filepath.Join(d.root, "refused.txt")); !strings.Contains(string(refused), "not_a_reporter") {
		t.Fatalf("refusal: %q", refused)
	}
	// Routing follows acceptance, so wait until both items are routed
	// before checking where they went.
	var page client.Page[client.Event]
	d.until("both recorded items are routed", 30*time.Second, func() bool {
		var err error
		page, err = d.c.Events(ctx, url.Values{"type": {"observe.item.recorded"}})
		if err != nil || len(page.Items) != 2 {
			return false
		}
		for _, e := range page.Items {
			if e.RoutedWorkID == "" {
				return false
			}
		}
		return true
	})
	w := threads()[0]
	for _, e := range page.Items {
		if e.RoutedWorkID != w.ID {
			t.Fatalf("item %s went to %q, not the thread %s", e.ID, e.RoutedWorkID, w.ID)
		}
	}
	d.settle()
}
