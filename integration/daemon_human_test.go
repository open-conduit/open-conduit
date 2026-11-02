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
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

// humanDaemonConfig binds GitHub issues to an agent stand-in that asks a
// private question through human.ask on its first run and completes on the
// next, keeping what it was resumed with.
func humanDaemonConfig(apiURL, root string) []byte {
	script := `if [ -f ` + root + `/asked ]; then cp "$OS_INPUT_FILE" ` + root + `/resumed.json && ` +
		`os outcome completed --summary decided --schema core.turn.v1 --data '{"status":"done"}' --request-id done >/dev/null; ` +
		`else touch ` + root + `/asked && os call human.ask --data '{"question":"Private: ship the migration tonight?","options":["yes","no"]}' --request-id ask >/dev/null; fi`
	ops := []string{"read_context", "declare_outcome", "human.ask"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{},
		"connections": []any{map[string]any{"id": "github", "type": "github", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"token": "env:OC_TEST_GITHUB_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "eng", "name": "Engineering",
			"authorization": map[string]any{"connections": []string{"github"}, "operations": ops, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": ops,
				"secret_names": []any{}, "config": map[string]any{"command": script}}},
			"routes": []any{map[string]any{"name": "issues", "handler": "github", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"extensions": []any{
				map[string]any{"name": "widgets", "type": "github", "connection": "github", "secret_names": []any{},
					"config": map[string]any{"namespace": "github/github.com/octo/widgets", "scope": "eng", "repo": "octo/widgets"}},
				map[string]any{"name": "decisions", "type": "human", "secret_names": []any{}, "config": map[string]any{}},
			},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonHumanAskFromGitHubWork drives the composed daemon: a GitHub
// Work asks a private question and parks; across a restart the operator
// sees it on the Work, a wrong answer is refused, the right one wakes the
// Work exactly once, and nothing about the ask reaches the issue.
func TestDaemonHumanAskFromGitHubWork(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	n := fake.OpenIssue("Migrate the orders table", "", githubtest.User{Login: "alice", Type: "User"}, "bug")
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	d := newDrill(t)
	if err := os.WriteFile(d.config, humanDaemonConfig(fake.URL, d.root), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	ctx := context.Background()
	work := func() client.Work {
		page, err := d.c.Works(ctx, url.Values{"scope_id": {"eng"}})
		if err != nil || len(page.Items) != 1 {
			return client.Work{}
		}
		return page.Items[0]
	}
	answer := func() []client.Action {
		acts, _ := d.c.Actions(ctx, client.ActionTarget{WorkID: work().ID})
		var out []client.Action
		for _, a := range acts {
			if a.Provider == "decisions" {
				out = append(out, a)
			}
		}
		return out
	}
	d.until("asker parked with an answer action", 30*time.Second, func() bool { return work().State == "waiting" && len(answer()) == 1 })

	d.kill()
	d.start("")
	w := work()
	if w.State != "waiting" || len(answer()) != 1 || !strings.Contains(answer()[0].Prompt, "1. yes, 2. no") {
		t.Fatalf("after restart: %+v %+v", w, answer())
	}
	target := client.ActionTarget{WorkID: w.ID}
	if _, err := d.c.InvokeAction(ctx, target, "decisions", "answer", "maybe", "bad"); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("invalid answer: %v", err)
	}
	res, err := d.c.InvokeAction(ctx, target, "decisions", "answer", "1", "first")
	if err != nil || res.Event.Type != "human.answer" || res.Event.RoutedWorkID != "" {
		t.Fatalf("answer: %+v %v", res, err)
	}
	d.until("asker resumed and completed", 30*time.Second, func() bool { return work().State == "completed" })
	if _, err = d.c.InvokeAction(ctx, target, "decisions", "answer", "2", "second"); err == nil || !strings.Contains(err.Error(), "not_outstanding") {
		t.Fatalf("answer to a closed ask: %v", err)
	}
	if xs := d.executions(w.ID); len(xs) != 2 {
		t.Fatalf("executions: %d", len(xs))
	}
	resumed, err := os.ReadFile(filepath.Join(d.root, "resumed.json"))
	var in struct {
		Trigger struct{ Type string }
		Inputs  []struct{ Type string }
	}
	if err != nil || json.Unmarshal(resumed, &in) != nil || in.Trigger.Type != "human.answered" || len(in.Inputs) != 1 || strings.Contains(string(resumed), "migration tonight") {
		t.Fatalf("resumed with: %s %v", resumed, err)
	}
	page, err := d.c.Events(ctx, url.Values{"type": {"human.answered"}})
	if err != nil || len(page.Items) != 1 || page.Items[0].CausationID != res.Event.ID {
		t.Fatalf("aliases: %+v %v", page, err)
	}
	d.settle()
	for _, c := range fake.Comments(n) {
		if strings.Contains(c.Body, "migration tonight") || strings.Contains(c.Body, "yes") {
			t.Fatalf("the ask reached the issue: %q", c.Body)
		}
	}
}
