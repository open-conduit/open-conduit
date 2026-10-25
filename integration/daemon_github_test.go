package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/extensions/github/githubtest"
)

// githubDaemonConfig binds a GitHub scope whose shell binding completes with a
// declared tracker status and comment.
func githubDaemonConfig(apiURL string) []byte {
	script := `os outcome completed --summary fixed --schema github.status.v1 --data '{"status":"done","comment":"Fixed by the daemon."}' --request-id done >/dev/null`
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{},
		"connections": []any{map[string]any{"id": "github", "type": "github", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"token": "env:OC_TEST_GITHUB_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "eng", "name": "Engineering",
			"authorization": map[string]any{"connections": []string{"github"}, "operations": []string{"read_context", "declare_outcome"}, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": []string{"read_context", "declare_outcome"},
				"secret_names": []any{}, "config": map[string]any{"command": script}}},
			"routes": []any{map[string]any{"name": "issues", "handler": "github", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
			"extensions": []any{map[string]any{"name": "widgets", "type": "github", "connection": "github", "secret_names": []any{},
				"config": map[string]any{"namespace": "github/github.com/octo/widgets", "scope": "eng", "repo": "octo/widgets"}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonGitHubReconcileToProjection drives the composed `serve` daemon:
// the registered GitHub ingress reconciles a fake repository, the github route
// claims the issue, the shell attempt declares a status and comment, and the
// projector converges labels and posts the marked comment exactly once, also
// across a restart.
func TestDaemonGitHubReconcileToProjection(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	n := fake.OpenIssue("Header wobbles", "", githubtest.User{Login: "alice", Type: "User"}, "bug")
	skipped := fake.OpenIssue("Later", "", githubtest.User{Login: "alice", Type: "User"}, "backlog")
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	d := newDrill(t)
	if err := os.WriteFile(d.config, githubDaemonConfig(fake.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	works := func() []string {
		page, err := d.c.Works(context.Background(), url.Values{"scope_id": {"eng"}})
		if err != nil {
			return nil
		}
		var states []string
		for _, w := range page.Items {
			states = append(states, w.Kind+":"+w.State)
		}
		return states
	}
	d.until("issue claimed and completed", 30*time.Second, func() bool {
		s := works()
		return len(s) == 1 && s[0] == "github.issue:completed"
	})
	d.until("labels and comment projected", 30*time.Second, func() bool {
		return equal(fake.Labels(n), "agent-in-progress", "bug", "might-be-done") && len(fake.Comments(n)) == 1
	})
	body := fake.Comments(n)[0].Body
	if !strings.Contains(body, "Fixed by the daemon.") || !strings.Contains(body, "open-conduit:agent") {
		t.Fatalf("projected comment: %q", body)
	}
	if len(fake.Labels(skipped)) != 1 || len(fake.Comments(skipped)) != 0 {
		t.Fatal("skipped issue was touched")
	}
	report, err := d.c.Doctor(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range report.Checks {
		if c.Name == "github" {
			found = c.Code == "ok"
		}
	}
	if !found {
		t.Fatalf("doctor github check: %+v", report.Checks)
	}
	// Restart: nothing is claimed or posted again.
	d.kill()
	d.start("")
	time.Sleep(2 * time.Second)
	if s := works(); len(s) != 1 || len(fake.Comments(n)) != 1 {
		t.Fatalf("after restart: works %v comments %d", s, len(fake.Comments(n)))
	}
}

// githubToolsDaemonConfig binds the hold route and an agent stand-in (a
// shell binding calling the GitHub tools through `os call`).
func githubToolsDaemonConfig(apiURL string) []byte {
	script := `os call github.issue.comment --data '{"body":"Picked up by the daemon."}' --request-id start >/dev/null && ` +
		`os outcome completed --summary fixed --schema github.status.v1 --data '{"status":"done"}' --request-id done >/dev/null`
	runOps := []string{"read_context", "declare_outcome", "github.issue.comment", "github.context"}
	holdOps := []string{"emit", "github.hold.status", "github.hold"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{},
		"connections": []any{map[string]any{"id": "github", "type": "github", "config": map[string]any{"api_url": apiURL},
			"secret_refs": map[string]any{"token": "env:OC_TEST_GITHUB_TOKEN"}}},
		"scopes": []any{map[string]any{
			"id": "eng", "name": "Engineering",
			"authorization": map[string]any{"connections": []string{"github"}, "operations": append(append([]string{}, runOps...), holdOps...), "read_scopes": []any{}},
			"bindings": []any{
				map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": runOps,
					"secret_names": []any{}, "config": map[string]any{"command": script}},
				map[string]any{"name": "hold", "executor": "github-hold", "requires": map[string]any{"toolbelt": "sdk"}, "operations": holdOps,
					"secret_names": []any{}, "config": map[string]any{}},
			},
			"routes": []any{
				map[string]any{"name": "held", "handler": "github-hold", "binding": "hold", "active_policy": "queue", "config": map[string]any{}},
				map[string]any{"name": "issues", "handler": "github", "binding": "run", "active_policy": "queue", "config": map[string]any{}},
			},
			"extensions": []any{map[string]any{"name": "widgets", "type": "github", "connection": "github", "secret_names": []any{},
				"config": map[string]any{"namespace": "github/github.com/octo/widgets", "scope": "eng", "repo": "octo/widgets"}}},
			"secret_refs": map[string]any{},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonGitHubHeldIssueAndTools drives the composed daemon: the GitHub
// tool operations validate in config, a scheduled issue is claimed by the
// hold route and parked by the in-process github-hold executor over the real
// management socket (a core timeout timer), released when its time comes,
// and the agent stand-in then comments through `os call` exactly once.
func TestDaemonGitHubHeldIssueAndTools(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	at := time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339)
	n := fake.OpenIssue("Match day", "Report on it.\n\n<!-- runAt: "+at+" -->", githubtest.User{Login: "alice", Type: "User"}, "scheduled")
	t.Setenv("OC_TEST_GITHUB_TOKEN", "tok")
	d := newDrill(t)
	if err := os.WriteFile(d.config, githubToolsDaemonConfig(fake.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	state := func() string {
		page, err := d.c.Works(context.Background(), url.Values{"scope_id": {"eng"}})
		if err != nil || len(page.Items) != 1 {
			return ""
		}
		return page.Items[0].Kind + ":" + page.Items[0].State
	}
	d.until("held issue parked", 30*time.Second, func() bool { return state() == "github.issue.held:waiting" })
	d.until("held labels", 30*time.Second, func() bool { return equal(fake.Labels(n), "scheduled") })
	if len(fake.Comments(n)) != 0 {
		t.Fatal("the agent ran while the issue was held")
	}
	d.until("released and worked", 60*time.Second, func() bool { return state() == "github.issue.held:completed" })
	d.until("comment and labels", 30*time.Second, func() bool {
		return len(fake.Comments(n)) == 1 && equal(fake.Labels(n), "agent-in-progress", "might-be-done")
	})
	body := fake.Comments(n)[0].Body
	if !strings.Contains(body, "Picked up by the daemon.") || !strings.Contains(body, "open-conduit:agent") {
		t.Fatalf("tool comment: %q", body)
	}
	page, err := d.c.Events(context.Background(), url.Values{"type": {"github.hold.released"}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("release events: %v %d", err, len(page.Items))
	}
}
