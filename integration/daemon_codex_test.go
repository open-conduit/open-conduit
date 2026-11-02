package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest"
)

// TestDaemonCodexExecution runs the composed daemon with the Codex
// executor installed as a package (packages/codex), nothing in the daemon
// knowing about it, against a fake codex. The agent reads its context
// through the real `os mcp` server; an Event arriving mid-turn is injected
// and taken in by a follow-up turn on the same thread, so it starts no
// attempt of its own; the next Event resumes the thread as attempt 2, which
// finishes the Work.
func TestDaemonCodexExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon with an installed package")
	}
	root := t.TempDir()
	barrier := filepath.Join(root, "barrier")
	fake, err := extensiontest.FakeCodex(filepath.Join(root, "codex"),
		extensiontest.Turn{Calls: []extensiontest.Call{{Operation: "read_context", RequestID: "ctx-1"}}, Barrier: barrier, State: "review", Summary: "read it"},
		extensiontest.Turn{State: "review", Summary: "took the interjection in"},
		extensiontest.Turn{State: "finished", Summary: "all done"})
	if err != nil {
		t.Fatal(err)
	}
	ops := []string{"read_context", "declare_outcome", "wait"}
	cfg, _ := json.Marshal(map[string]any{
		"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
		"scopes": []any{map[string]any{
			"id": "drill", "name": "Drill",
			"authorization": map[string]any{"connections": []any{}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "agent", "executor": "codex", "requires": map[string]any{"toolbelt": "mcp", "inject": true, "resume": true}, "operations": ops, "secret_names": []any{},
				"config": map[string]any{"binary": fake.Path, "playbook": "-"}}},
			"routes":      []any{map[string]any{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "inject", "config": map[string]any{}}},
			"secret_refs": map[string]any{},
		}},
	})
	d := extensiontest.Daemon(t, extensiontest.Package("../packages/codex"), extensiontest.Options{Config: cfg})
	c := d.Client()
	submit := func(typ, delivery string) {
		t.Helper()
		if _, err := c.SubmitEvent(context.Background(), client.EventRequest{ScopeID: "drill", Type: typ, DeliveryID: delivery, CorrelationKey: &client.Key{Source: "drill", ExternalID: "one"}}, "submit-"+delivery); err != nil {
			t.Fatal(err)
		}
	}
	turn := func(n int) extensiontest.TurnRecord {
		t.Helper()
		var r extensiontest.TurnRecord
		d.Until("codex turn", extensiontest.Deadline, func() bool {
			var ok bool
			r, ok = fake.Turn(n)
			return ok
		})
		return r
	}
	work := func() client.Work {
		t.Helper()
		page, err := c.Works(context.Background(), url.Values{"scope_id": {"drill"}})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("work: %+v %v", page.Items, err)
		}
		return page.Items[0]
	}

	submit("drill.start", "d1")
	first := turn(1)
	if first.Token == "" || first.Endpoint == "" || len(first.Results) != 1 || first.Results[0].IsError {
		t.Fatalf("turn 1: %+v", first)
	}
	if !strings.Contains(first.Input, "## Already waiting") || strings.Contains(strings.Join(first.Args, " "), "resume") {
		t.Fatalf("turn 1 launch: %+v", first)
	}

	// Mid-turn: the Event is injected into the running segment's inbox.
	submit("drill.more", "d2")
	d.Until("the injected event in the segment's inbox", extensiontest.Deadline, func() bool { return inboxHasEvent(d.StateDir()) })
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second := turn(2)
	if args := strings.Join(second.Args, " "); !strings.HasPrefix(args, "exec resume ") || !strings.HasSuffix(args, " fake-thread -") {
		t.Fatalf("follow-up launch: %s", args)
	}
	if !strings.Contains(second.Input, "## Arrived during this turn") || !strings.Contains(second.Input, "drill.more") {
		t.Fatalf("follow-up input: %s", second.Input)
	}
	d.Until("the Work handed back", extensiontest.Deadline, func() bool { return work().State == "ready" })

	// The next Event is attempt 2, resuming the thread; it finishes.
	submit("drill.final", "d3")
	third := turn(3)
	if args := strings.Join(third.Args, " "); !strings.HasPrefix(args, "exec resume ") || !strings.Contains(third.Input, "## Since your last turn") || !strings.Contains(third.Input, "drill.final") || strings.Contains(third.Input, "drill.more") {
		t.Fatalf("turn 3: %s\n%s", args, third.Input)
	}
	w := d.WaitWork(work().ID, "completed")
	execs, err := c.Executions(context.Background(), url.Values{"work_id": {w.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(execs.Items) != 2 {
		t.Fatalf("executions: %d, want 2 (the injected event started none)", len(execs.Items))
	}
	if _, ok := fake.Turn(4); ok {
		t.Fatal("a fourth turn ran")
	}
}

// inboxHasEvent reports a delivered event file in any codex segment's
// inbox under the daemon's state.
func inboxHasEvent(state string) bool {
	found := false
	_ = filepath.WalkDir(state, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && filepath.Base(filepath.Dir(path)) == "inbox" && strings.HasSuffix(path, ".json") {
			found = true
		}
		return nil
	})
	return found
}
