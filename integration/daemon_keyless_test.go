package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
)

// keylessSpecialistScript asks a question, then makes a gated call, then
// makes it again and completes, keeping its progress per Work.
const keylessSpecialistScript = `set -e
s="$KL_DIR/$(printf %s "$OS_WORK_ID" | tr -c 'a-zA-Z0-9' _)"
mkdir -p "$s"
if [ ! -f "$s/asked" ]; then
  touch "$s/asked"
  os call human.ask --data '{"question":"Go ahead?","options":["yes","no"]}' --request-id ask > "$s/ask.out"
elif [ ! -f "$s/gated" ]; then
  touch "$s/gated"
  os call introspection.reports --data '{"state":"all"}' --request-id gate-1 > "$s/gate.out"
else
  os call introspection.reports --data '{"state":"all"}' --request-id gate-2 > "$s/ran.out" 2> "$s/ran.err"
  os outcome completed --summary done --schema core.turn.v1 --data '{"status":"done","result":{"ok":true}}' --request-id done > /dev/null 2> "$s/done.err"
fi
`

const keylessFrontScript = `set -e
os work create --kind eng.task --request-id child > /dev/null
os outcome completed --summary handed --request-id done > /dev/null
`

func keylessConfig(dir string) []byte {
	shell := func(name, script string, ops []string, kinds []string) map[string]any {
		return map[string]any{"name": name, "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops, "work_kinds": kinds,
			"secret_names": []any{}, "config": map[string]any{"command": script, "env": map[string]string{"KL_DIR": dir}}}
	}
	specialist := []string{"read_context", "declare_outcome", "human.ask", is.OpList}
	front := []string{"read_context", "declare_outcome", "create_work"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "eng", "name": "Engineering",
			"authorization": map[string]any{"connections": []any{}, "operations": append(append([]string{}, specialist...), "create_work"), "read_scopes": []any{}, "work_kinds": []string{"eng.task"}},
			"bindings": []any{
				shell("specialist", keylessSpecialistScript, specialist, []string{}),
				shell("front", keylessFrontScript, front, []string{"eng.task"}),
			},
			"routes": []any{
				map[string]any{"name": "incoming", "handler": "manual", "binding": "specialist", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "children", "handler": "work", "binding": "specialist", "active_policy": "queue", "config": map[string]any{"kinds": []any{map[string]any{"kind": "eng.task"}}}},
			},
			"extensions": []any{
				map[string]any{"name": "reports", "type": is.Type, "config": map[string]any{}, "secret_names": []any{}},
				map[string]any{"name": "decisions", "type": "human", "secret_names": []any{},
					"config": map[string]any{"gates": []any{map[string]any{"operations": []string{is.OpList}, "for_bindings": []string{"specialist"}, "timeout": "2h"}}}},
			},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonKeylessWorkWaitsForAPerson: Work made with no correlation key,
// by an operator (POST /v1/work) or by another Work (create_work), can
// still wait for a person. Its human.ask parks and an answer resumes it; its
// gated call parks, shows its approval on the Work, and an approval resumes
// it and lets the call through. The host gives it its own key to wait on.
func TestDaemonKeylessWorkWaitsForAPerson(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	d := newDrill(t)
	dir := filepath.Join(d.root, "kl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config, keylessConfig(dir), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*", "*"))
		for _, f := range files {
			b, _ := os.ReadFile(f)
			t.Logf("%s: %s", f, b)
		}
		page, _ := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		for _, w := range page.Items {
			t.Logf("work %s %s %s", w.ID, w.Kind, w.State)
			for _, x := range d.executions(w.ID) {
				t.Logf("  execution %s %s %+v", x.ID, x.State, x.Outcome)
			}
		}
	})
	d.start("")

	operator, err := d.c.CreateWork(ctx, client.WorkRequest{ScopeID: "eng", Kind: "manual.task", Binding: "specialist"}, "operator-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(operator.Work.CorrelationKeys) != 0 {
		t.Fatalf("operator Work made with keys: %+v", operator.Work.CorrelationKeys)
	}
	front, err := d.c.CreateWork(ctx, client.WorkRequest{ScopeID: "eng", Kind: "manual.front", Binding: "front"}, "front-1")
	if err != nil {
		t.Fatal(err)
	}
	child := func() string {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}, "kind": {"eng.task"}})
		if err != nil {
			return ""
		}
		for _, w := range page.Items {
			if w.ParentID == front.Work.ID {
				return w.ID
			}
		}
		return ""
	}
	d.until("the child Work", 30*time.Second, func() bool { return child() != "" })
	ids := map[string]string{"operator": operator.Work.ID, "child": child()}

	offered := func(id string) []client.Action {
		acts, _ := d.c.Actions(ctx, client.ActionTarget{WorkID: id})
		var out []client.Action
		for _, a := range acts {
			if a.Provider == "decisions" {
				out = append(out, a)
			}
		}
		return out
	}
	parked := func(id string) bool {
		w, err := d.c.Work(ctx, id)
		return err == nil && w.State == "waiting" && len(offered(id)) == 1
	}
	for name, id := range ids {
		d.until(name+" parked on its question", 30*time.Second, func() bool { return parked(id) })
		w, _ := d.c.Work(ctx, id)
		if len(w.CorrelationKeys) != 1 || w.CorrelationKeys[0].Source != "core.work" || w.CorrelationKeys[0].ExternalID != id {
			t.Fatalf("%s: keys %+v", name, w.CorrelationKeys)
		}
		if _, err := d.c.InvokeAction(ctx, client.ActionTarget{WorkID: id}, "decisions", "answer", "yes", name+"-answer"); err != nil {
			t.Fatalf("%s: answer: %v", name, err)
		}
		d.until(name+" parked on its gated call", 30*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(dir, safeName(id), "gated"))
			return err == nil && parked(id)
		})
		if _, err := os.Stat(filepath.Join(dir, safeName(id), "ran.out")); err == nil {
			t.Fatalf("%s: the gated call ran before approval", name)
		}
		if _, err := d.c.InvokeAction(ctx, client.ActionTarget{WorkID: id}, "decisions", "answer", "approve", name+"-approve"); err != nil {
			t.Fatalf("%s: approve: %v", name, err)
		}
		d.until(name+" completed", 30*time.Second, func() bool {
			w, err := d.c.Work(ctx, id)
			return err == nil && w.State == "completed"
		})
		if b, err := os.ReadFile(filepath.Join(dir, safeName(id), "ran.out")); err != nil || len(b) == 0 {
			t.Fatalf("%s: the approved call: %s %v", name, b, err)
		}
	}
}

func safeName(id string) string {
	out := []byte(id)
	for i, c := range out {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			out[i] = '_'
		}
	}
	return string(out)
}
