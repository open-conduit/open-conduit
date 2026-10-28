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
)

// setupDaemonConfig is a scope with a `setup` entry and route, bound to an
// agent stand-in that says hello and asks a question on its first turn and
// completes on the next, keeping what it was resumed with.
func setupDaemonConfig(root string) []byte {
	script := `if [ -f ` + root + `/asked ]; then cp "$OS_INPUT_FILE" ` + root + `/resumed.json && ` +
		`os outcome completed --summary set --schema core.turn.v1 --data '{"status":"finished"}' --request-id done >/dev/null; ` +
		`else touch ` + root + `/asked && os call setup.say --data '{"text":"Claude Code is installed and signed in."}' --request-id hello >/dev/null && ` +
		`os call setup.ask --data '{"question":"Which repository?","choices":["octo/widgets","octo/gadgets"]}' --request-id repo >/dev/null; fi`
	ops := []string{"read_context", "declare_outcome", "setup.say", "setup.ask"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "setup", "name": "Setup",
			"authorization": map[string]any{"connections": []string{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "agent", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops,
				"secret_names": []any{}, "config": map[string]any{"command": script}}},
			"routes": []any{map[string]any{"name": "setup", "handler": "setup", "binding": "agent", "active_policy": "inject",
				"unsupported_fallback": "queue", "error_fallback": "queue", "config": map[string]any{}}},
			"extensions":  []any{map[string]any{"name": "setup", "type": "setup", "secret_names": []any{}, "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonSetupSession drives a setup session through the composed
// daemon: the operator starts it from the scope, the agent's message and
// question arrive as typed Events on its Work, a message sent while it is
// parked waits for the answer, and the answer resumes it with both.
func TestDaemonSetupSession(t *testing.T) {
	d := newDrill(t)
	if err := os.WriteFile(d.config, setupDaemonConfig(d.root), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	scope := client.ActionTarget{ScopeID: "setup"}
	ids := func(target client.ActionTarget) string {
		acts, _ := d.c.Actions(ctx, target)
		var out []string
		for _, a := range acts {
			if a.Provider == "setup" {
				out = append(out, a.ID)
			}
		}
		return strings.Join(out, ",")
	}
	if got := ids(scope); got != "start" {
		t.Fatalf("scope actions: %q", got)
	}
	res, err := d.c.InvokeAction(ctx, scope, "setup", "start", "", "start")
	if err != nil || res.Event.Type != "setup.start" || !strings.HasPrefix(res.Event.RoutedWorkID, "setup/setup/") {
		t.Fatalf("start: %+v %v", res, err)
	}
	session := client.ActionTarget{WorkID: res.Event.RoutedWorkID}
	work := func() client.Work {
		w, _ := d.c.Work(ctx, session.WorkID)
		return w
	}
	if w := work(); w.Kind != "setup.session" || w.Display == nil || w.Display.Title != "Setup session" || w.Context == nil || w.Context.Schema != "setup.session.v1" {
		t.Fatalf("session: %+v", w)
	}
	if got := ids(scope); got != "" {
		t.Fatalf("a second start is offered while the session is open: %q", got)
	}
	d.until("the agent parked on its question", 30*time.Second, func() bool { return work().State == "waiting" && ids(session) == "answer,message" })

	events := func(typ string) []client.Event {
		page, _ := d.c.Events(ctx, url.Values{"work_id": {session.WorkID}, "type": {typ}})
		return page.Items
	}
	said, asked := events("setup.message"), events("setup.question")
	if len(said) != 1 || !strings.Contains(string(said[0].Data.Data), "signed in") || said[0].CorrelationKey != nil {
		t.Fatalf("said: %+v", said)
	}
	if len(asked) != 1 || !strings.Contains(string(asked[0].Data.Data), `"choices":["octo/widgets","octo/gadgets"]`) {
		t.Fatalf("asked: %+v", asked)
	}

	if _, err = d.c.InvokeAction(ctx, session, "setup", "message", "I use the work account.", "msg"); err != nil {
		t.Fatalf("message: %v", err)
	}
	if w := work(); w.State != "waiting" {
		t.Fatalf("a message woke the question: %s", w.State)
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", "answer", "octo/nothing", "bad"); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("invalid answer: %v", err)
	}
	if _, err = d.c.InvokeAction(ctx, session, "setup", "answer", "2", "answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	d.until("the session finished", 30*time.Second, func() bool { return work().State == "completed" })
	if xs := d.executions(session.WorkID); len(xs) != 2 {
		t.Fatalf("executions: %d", len(xs))
	}
	resumed, err := os.ReadFile(filepath.Join(d.root, "resumed.json"))
	var in struct {
		Trigger struct{ Type string }
		Inputs  []struct{ Type string }
	}
	if err != nil || json.Unmarshal(resumed, &in) != nil || in.Trigger.Type != "setup.answer" || !strings.Contains(string(resumed), "octo/gadgets") || !strings.Contains(string(resumed), "work account") {
		t.Fatalf("resumed with: %s %v", resumed, err)
	}
	if got := ids(scope); got != "start" {
		t.Fatalf("scope actions once the session ended: %q", got)
	}
}
