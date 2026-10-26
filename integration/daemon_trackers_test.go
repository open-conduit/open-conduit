package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/bitbucket/bitbuckettest"
	"github.com/open-conduit/open-conduit/extensions/jira/jiratest"
	"github.com/open-conduit/open-conduit/extensions/linear/lineartest"
)

// posthogFake records captured events (no real PostHog is ever called).
type posthogFake struct {
	mu     sync.Mutex
	events []map[string]any
	srv    *httptest.Server
}

func newPostHogFake() *posthogFake {
	p := &posthogFake{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e map[string]any
		if r.URL.Path != "/i/v0/e/" || json.Unmarshal(body, &e) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.events = append(p.events, e)
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return p
}

func (p *posthogFake) named(name string) []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []map[string]any
	for _, e := range p.events {
		if e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

func trackersDaemonConfig(linearURL, jiraURL, bbURL, posthogURL string) []byte {
	linearScript := `os call linear.issue.comment --data '{"body":"Picked up."}' --request-id start >/dev/null && ` +
		`os outcome completed --summary fixed --schema linear.status.v1 --data '{"status":"review","comment":"Ready for review."}' --request-id done >/dev/null`
	jiraScript := `os call jira.issue.comment --data '{"body":"Picked up."}' --request-id start >/dev/null && ` +
		`os call bitbucket.pr.comment --data '{"number":1,"body":"Pushed the fix."}' --request-id pr >/dev/null && ` +
		`os outcome completed --summary fixed --schema jira.status.v1 --data '{"status":"done","comment":"Done in the daemon."}' --request-id done >/dev/null`
	linearOps := []string{"read_context", "declare_outcome", "linear.issue.comment", "linear.context"}
	jiraOps := []string{"read_context", "declare_outcome", "jira.issue.comment", "bitbucket.pr.comment"}
	cfg := map[string]any{
		"version": 1, "settings": map[string]any{},
		"connections": []any{
			map[string]any{"id": "linear", "type": "linear", "config": map[string]any{"api_url": linearURL}, "secret_refs": map[string]any{"token": "env:OC_TEST_LINEAR_TOKEN"}},
			map[string]any{"id": "jira", "type": "jira", "config": map[string]any{"base_url": jiraURL, "email": "me@example.com"}, "secret_refs": map[string]any{"token": "env:OC_TEST_JIRA_TOKEN"}},
			map[string]any{"id": "bitbucket", "type": "bitbucket", "config": map[string]any{"api_url": bbURL, "username": "me@example.com"}, "secret_refs": map[string]any{"token": "env:OC_TEST_BB_TOKEN"}},
			map[string]any{"id": "posthog", "type": "posthog", "config": map[string]any{"host": posthogURL, "distinct_id": "drill"}, "secret_refs": map[string]any{"token": "env:OC_TEST_POSTHOG_KEY"}},
		},
		"scopes": []any{
			map[string]any{
				"id": "eng", "name": "Linear",
				"authorization": map[string]any{"connections": []string{"linear"}, "operations": linearOps, "read_scopes": []any{}},
				"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": linearOps,
					"secret_names": []any{}, "config": map[string]any{"command": linearScript}}},
				"routes": []any{map[string]any{"name": "issues", "handler": "linear", "binding": "run", "active_policy": "queue", "config": map[string]any{"match": map[string]any{"team": []string{"ENG"}}}}},
				"extensions": []any{map[string]any{"name": "eng", "type": "linear", "connection": "linear", "secret_names": []any{},
					"config": map[string]any{"namespace": "linear/eng", "scope": "eng", "teams": []string{"ENG"}, "transitions": map[string]any{"claimed": "In Progress", "review": "In Review"}}}},
				"secret_refs": map[string]any{},
			},
			map[string]any{
				"id": "proj", "name": "Jira",
				"authorization": map[string]any{"connections": []string{"jira", "bitbucket", "posthog"}, "operations": jiraOps, "read_scopes": []any{}},
				"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{"toolbelt": "cli"}, "operations": jiraOps,
					"secret_names": []any{}, "config": map[string]any{"command": jiraScript}}},
				"routes": []any{
					map[string]any{"name": "issues", "handler": "jira", "binding": "run", "active_policy": "queue", "config": map[string]any{}},
					map[string]any{"name": "prs", "handler": "bitbucket", "binding": "run", "active_policy": "queue", "config": map[string]any{}},
				},
				"extensions": []any{
					map[string]any{"name": "proj", "type": "jira", "connection": "jira", "secret_names": []any{},
						"config": map[string]any{"namespace": "jira/acme/proj", "scope": "proj", "projects": []string{"PROJ"}, "transitions": map[string]any{"done": "Done"}}},
					map[string]any{"name": "app", "type": "bitbucket", "connection": "bitbucket", "secret_names": []any{},
						"config": map[string]any{"namespace": "bitbucket/acme/app", "scope": "proj", "repo": "acme/app", "tracker": map[string]any{"namespace": "jira/acme/proj", "keys": []string{"PROJ"}}}},
					map[string]any{"name": "telemetry", "type": "posthog", "connection": "posthog", "secret_names": []any{}, "config": map[string]any{}},
				},
				"secret_refs": map[string]any{},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// TestDaemonTrackersAndPostHog drives the composed `serve` daemon with the
// installed-integration ports (06-01) on fakes only: Linear and Jira
// ingress reconcile, their routes claim (Linear through a route match), the
// shell attempts call the tracker and Bitbucket tools through `os call` and
// declare a status with a comment, the projector converges labels, workflow
// statuses and marked comments exactly once, PostHog receives agent_turn
// events, doctor reports the entries, and a restart posts nothing again.
func TestDaemonTrackersAndPostHog(t *testing.T) {
	lf := lineartest.New("lin_api_t", "ENG")
	defer lf.Close()
	jf := jiratest.New("me@example.com", "jt")
	defer jf.Close()
	bf := bitbuckettest.New("acme/app", "me@example.com", "bt")
	defer bf.Close()
	ph := newPostHogFake()
	defer ph.srv.Close()
	lf.AddIssue("ENG", 1, "Header wobbles", "Todo", "bug")
	lf.AddIssue("ENG", 2, "Later", "Todo", "backlog")
	jf.AddIssue("PROJ-1", "Fix login", "To Do")
	bf.AddPR(1, "PROJ-1 fix login", "agent/issue/PROJ-1", "abc1234def")
	t.Setenv("OC_TEST_LINEAR_TOKEN", "lin_api_t")
	t.Setenv("OC_TEST_JIRA_TOKEN", "jt")
	t.Setenv("OC_TEST_BB_TOKEN", "bt")
	t.Setenv("OC_TEST_POSTHOG_KEY", "phc_drill")
	d := newDrill(t)
	if err := os.WriteFile(d.config, trackersDaemonConfig(lf.URL(), jf.Server.URL, bf.URL(), ph.srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	d.start("")
	works := func(scope string) []string {
		page, err := d.c.Works(context.Background(), url.Values{"scope_id": {scope}})
		if err != nil {
			return nil
		}
		var states []string
		for _, w := range page.Items {
			states = append(states, w.Kind+":"+w.State)
		}
		return states
	}
	d.until("linear issue claimed and completed", 30*time.Second, func() bool {
		s := works("eng")
		return len(s) == 1 && s[0] == "linear.issue:completed"
	})
	d.until("jira issue claimed and completed", 30*time.Second, func() bool {
		s := works("proj")
		return len(s) == 1 && s[0] == "jira.issue:completed"
	})
	d.until("linear labels, status and comments", 30*time.Second, func() bool {
		labels, state, comments := lf.Snapshot("ENG-1")
		return equal(labels, "agent-in-progress", "bug", "ready-for-review") && state == "In Review" && len(comments) == 2
	})
	d.until("jira labels, status and comments", 30*time.Second, func() bool {
		labels, state, comments := jf.Snapshot("PROJ-1")
		return equal(labels, "agent-in-progress", "might-be-done") && state == "Done" && len(comments) == 2
	})
	_, _, lc := lf.Snapshot("ENG-1")
	if !strings.Contains(lc[0].Body+lc[1].Body, "Ready for review.") || !strings.Contains(lc[1].Body, "Posted by an open-supervisor agent working on ENG-1") {
		t.Fatalf("linear comments: %+v", lc)
	}
	if prc := bf.PRComments(1); len(prc) != 1 || !strings.Contains(prc[0].Body, "Pushed the fix.") || !strings.Contains(prc[0].Body, "open-conduit:agent") {
		t.Fatalf("bitbucket comment: %+v", prc)
	}
	if labels, _, comments := lf.Snapshot("ENG-2"); len(labels) != 1 || len(comments) != 0 {
		t.Fatal("skipped issue was touched")
	}
	d.until("posthog agent_turn for the jira scope", 30*time.Second, func() bool {
		for _, e := range ph.named("agent_turn") {
			props, _ := e["properties"].(map[string]any)
			if props["scope"] == "proj" && e["api_key"] == "phc_drill" && e["distinct_id"] == "drill" {
				return true
			}
		}
		return false
	})
	for _, e := range ph.named("agent_turn") {
		if props, _ := e["properties"].(map[string]any); props["scope"] != "proj" {
			t.Fatalf("a scope without a posthog entry was sent: %v", props)
		}
	}
	report, err := d.c.Doctor(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]string{}
	for _, c := range report.Checks {
		codes[c.Name] = c.Code
	}
	if codes["trackers"] != "ok" || codes["trackers_auth"] != "verified" || codes["posthog"] != "ok" || codes["posthog_auth"] != "verified" {
		t.Fatalf("doctor: %+v", report.Checks)
	}
	// Route explanation agrees with execution: the claim and the inputs are
	// the router's own durable decisions.
	op := newOperatorCLI(t, d)
	var explained struct {
		Works []struct {
			ID, Kind, State string
		} `json:"works"`
		Events []struct {
			Type, Route, Binding, Disposition string
			RoutedWorkID                      string `json:"routed_work_id"`
		} `json:"events"`
	}
	op.must(&explained, "route", "ENG-1", "--scope", "eng")
	if len(explained.Works) != 1 || explained.Works[0].Kind != "linear.issue" {
		t.Fatalf("route works: %+v", explained)
	}
	claimed := false
	for _, e := range explained.Events {
		if e.Type == "linear.issue.open" && e.Route == "issues" && e.Binding == "run" && e.RoutedWorkID == explained.Works[0].ID {
			claimed = true
		}
	}
	if !claimed {
		t.Fatalf("route events: %+v", explained.Events)
	}
	var none struct {
		Note string `json:"note"`
	}
	op.must(&none, "route", "ENG-404")
	if none.Note == "" {
		t.Fatal("an unknown key explains nothing")
	}
	if out := op.must(nil, "adapters", "linear"); !strings.Contains(out, "teams (required)") || !strings.Contains(out, "linear.issue.comment") {
		t.Fatalf("adapters linear: %s", out)
	}
	// Restart: nothing is claimed or posted again.
	d.kill()
	d.start("")
	time.Sleep(3 * time.Second)
	_, _, lc = lf.Snapshot("ENG-1")
	_, _, jc := jf.Snapshot("PROJ-1")
	if s := works("eng"); len(s) != 1 || len(lc) != 2 || len(jc) != 2 || len(bf.PRComments(1)) != 1 {
		t.Fatalf("after restart: works %v comments %d/%d/%d", s, len(lc), len(jc), len(bf.PRComments(1)))
	}
}
