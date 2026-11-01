package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/observe"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/storage"
)

// pollerScript is a non-agent observer: it records what it "read" with the
// shipped os CLI exactly as the observed-sources recipe shows, a repeat
// included, and keeps each call's output.
const pollerScript = `set -e
os call observe.record --data '{"item": "1730451600.000100", "thread": "1730451600.000100", "text": "How do I reset my VPN password?"}' > "$OS_OUTPUT_DIR/first.json"
os call observe.record --data '{"item": "1730451600.000100", "thread": "1730451600.000100", "text": "How do I reset my VPN password?"}' > "$OS_OUTPUT_DIR/again.json"
os call observe.record --data '{"item": "1730451700.000200", "thread": "1730451600.000100", "text": "It says my token expired"}' > "$OS_OUTPUT_DIR/reply.json"
if os call observe.record --data '{"source": "elsewhere", "item": "1", "thread": "1", "text": "x"}' 2> "$OS_OUTPUT_DIR/refused.txt"; then echo "recorded for an undeclared source" >&2; exit 9; fi
`

func observeShellConfig(t *testing.T) string {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "ops", "name": "Operations", "secret_refs": map[string]any{},
			"authorization": map[string]any{"connections": []any{}, "operations": []string{"read_context", observe.OpRecord}, "read_scopes": []any{}},
			"extensions": []any{map[string]any{"name": "help", "type": observe.Type, "secret_names": []any{},
				"config": map[string]any{"sources": []any{map[string]any{"name": "slack-help", "reporters": []string{"poller"}}}}}},
			"bindings": []any{
				map[string]any{"name": "poller", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context", observe.OpRecord}, "secret_names": []any{},
					"config": map[string]any{"command": pollerScript}},
				map[string]any{"name": "responder", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context"}, "secret_names": []any{},
					"config": map[string]any{"command": "true"}},
			},
			"routes": []any{
				map[string]any{"name": "incoming", "handler": "manual", "binding": "poller", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "threads", "handler": observe.Type, "binding": "responder", "active_policy": "queue", "config": map[string]any{"source": "slack-help"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(cfg)
}

// TestObservedSourceFromAShellScript runs the recipe's non-agent path end
// to end: a shell binding records items through the real os CLI over the
// Unix-socket toolbelt, its repeat is a duplicate, an undeclared source is
// refused, and the thread gets one Work with both items.
func TestObservedSourceFromAShellScript(t *testing.T) {
	state, bin := shellState(t, observeShellConfig(t))
	h := newShellHostWith(t, state, bin, conformance.NewClock(), "daemon", false, shellHostOptions{
		handlers:   map[string]extension.RouteHandler{observe.Type: observe.Route{}},
		extensions: map[string]config.ExtensionType{observe.Type: {Validate: observe.ValidateExtension}},
		operations: observe.Operations(),
		register: func(s storage.Store) extension.Registration {
			tools, err := observe.NewTools(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			return tools.Registration()
		}})
	ctx := context.Background()
	c := h.operator()
	root := h.submit("operator.requested", "poll-1", "poll")
	run, err := h.scheduler.Start(ctx, domain.ID(root.RoutedWorkID))
	if err != nil {
		t.Fatal(err)
	}
	h.observe(run)
	x, err := c.Execution(ctx, string(run.Lease.ExecutionID))
	if err != nil {
		t.Fatal(err)
	}
	if x.Outcome == nil || x.Outcome.Kind != "completed" {
		t.Fatalf("poller outcome: %+v", x.Outcome)
	}
	outputs := shellData(t, x.Outcome)["outputs"].(map[string]any)
	read := func(name string) map[string]any {
		var r struct {
			Schema string         `json:"schema"`
			Data   map[string]any `json:"data"`
		}
		text := h.artifact(outputs[name].(string))
		if err := json.Unmarshal([]byte(text), &r); err != nil || r.Schema != "observe.record.v1" {
			t.Fatalf("%s: %q", name, text)
		}
		return r.Data
	}
	if first, again := read("first.json"), read("again.json"); first["duplicate"] != false || again["duplicate"] != true || first["source"] != "slack-help" {
		t.Fatalf("first %v again %v", first, again)
	}
	if refused := h.artifact(outputs["refused.txt"].(string)); !strings.Contains(refused, "not_a_reporter") {
		t.Fatalf("refusal: %q", refused)
	}
	h.routeAll()
	works, err := c.Works(ctx, url.Values{"kind": {observe.WorkKind}})
	if err != nil || len(works.Items) != 1 {
		t.Fatalf("thread Work: %+v %v", works.Items, err)
	}
	inputs := 0
	h.coord.Transact(ctx, storage.Guard{Now: h.clock.Now()}, func(tx storage.Tx) error {
		events, err := tx.Events(0, 0)
		for _, e := range events {
			if e.Event.Type == observe.TypeRecorded {
				inputs++
			}
		}
		return err
	})
	if inputs != 2 {
		t.Fatalf("recorded %d items, want 2", inputs)
	}
}
