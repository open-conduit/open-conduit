package integration_test

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/management"
)

// grantsConfig is two teams' scopes, the first with a scope beneath it.
var grantsConfig = func() string {
	scope := func(id, parent string) string {
		p := ""
		if parent != "" {
			p = `"parent": "` + parent + `", `
		}
		return `{"id": "` + id + `", "name": "` + id + `", ` + p + `
    "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "read_scopes": []},
    "bindings": [{"name": "run", "executor": "fixture", "requires": {"toolbelt": "cli"}, "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "config": {}}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}]}`
	}
	return `{"version": 1, "settings": {}, "connections": [], "scopes": [` + scope("team-a", "") + `, ` + scope("team-a-web", "team-a") + `, ` + scope("team-b", "") + `]}`
}()

// TestManagementScopedGrants checks that a grant at a scope covers that
// scope's subtree and nothing else: lists and facets hold only what it
// covers, whatever the filter says; a read, a control or an Event aimed
// outside it is refused before anything is read or written; and daemon
// routes need a daemon-wide role.
func TestManagementScopedGrants(t *testing.T) {
	h := newManagementHarnessWith(t, grantsConfig, nil)
	ctx := context.Background()
	owner := h.operatorClient()
	work := map[string]client.Work{}
	for _, scope := range []string{"team-a", "team-a-web", "team-b"} {
		created, err := owner.CreateWork(ctx, client.WorkRequest{ScopeID: scope, Kind: "manual.task", Binding: "run"}, "make-"+scope)
		if err != nil {
			t.Fatal(err)
		}
		work[scope] = created.Work
	}
	if _, err := h.scheduler.Start(ctx, domain.ID(work["team-b"].ID)); err != nil {
		t.Fatal(err)
	}
	login := func(name string, args ...string) *client.Client {
		t.Helper()
		path := filepath.Join(h.state, name+".token")
		h.mustCLI(nil, append([]string{"auth", "add", name, "--out", path}, args...)...)
		f, err := client.ReadTokenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(h.endpoint, f.Token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	refused := func(what string, err error, status int, code string) {
		t.Helper()
		var api *client.Error
		if !errors.As(err, &api) || api.Status != status || api.Code != code {
			t.Fatalf("%s: want %d %s, got %v", what, status, code, err)
		}
	}
	ids := func(items []client.Work) []string {
		var out []string
		for _, w := range items {
			out = append(out, w.ScopeID)
		}
		slices.Sort(out)
		return out
	}

	// Admin is only ever daemon-wide, and a grant names a role.
	if _, errOut, code := h.cli("auth", "add", "eve", "--grants", "team-a=admin", "--out", filepath.Join(h.state, "eve.token")); code == 0 {
		t.Fatalf("scoped admin accepted: %s", errOut)
	}
	if _, err := owner.IssueOperator(ctx, client.OperatorRequest{Name: "eve", Grants: []client.Grant{{Scope: "team-a", Role: "view"}, {Scope: "team-a", Role: "operate"}}}); !isStatus(err, 400) {
		t.Fatalf("a scope granted twice: %v", err)
	}

	alice := login("alice", "--grants", "team-a=operate")
	bob := login("bob", "--grants", "team-b=view")

	// Lists hold the grant's subtree, and a filter can't widen them.
	page, err := alice.Works(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Items); strings.Join(got, " ") != "team-a team-a-web" || page.Total == nil || *page.Total != 2 {
		t.Fatalf("alice's Work: %v (total %v)", got, page.Total)
	}
	page, err = alice.Works(ctx, url.Values{"scope_id": {"team-b"}})
	if err != nil || len(page.Items) != 0 || page.Total == nil || *page.Total != 0 {
		t.Fatalf("alice filtering on team-b: %+v %v", page, err)
	}
	page, err = alice.Works(ctx, url.Values{"q": {"manual"}})
	if err != nil || strings.Join(ids(page.Items), " ") != "team-a team-a-web" {
		t.Fatalf("alice searching: %+v %v", page, err)
	}
	facets, err := alice.WorkFacets(ctx, nil)
	if err != nil || strings.Join(facets.ScopeIDs, " ") != "team-a team-a-web" || facets.Total != 2 {
		t.Fatalf("alice's facets: %+v %v", facets, err)
	}
	scopes, err := alice.Scopes(ctx, nil)
	if err != nil || len(scopes.Items) != 2 {
		t.Fatalf("alice's scopes: %+v %v", scopes, err)
	}
	events, err := alice.Events(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events.Items {
		if e.ScopeID != "team-a" && e.ScopeID != "team-a-web" {
			t.Fatalf("alice sees an Event of %s", e.ScopeID)
		}
	}
	if len(events.Items) != 2 {
		t.Fatalf("alice's Events: %d", len(events.Items))
	}
	runs, err := alice.Runs(ctx, nil)
	if err != nil || len(runs.Items) != 0 {
		t.Fatalf("alice sees team-b's run: %+v %v", runs, err)
	}
	executions, err := bob.Executions(ctx, nil)
	if err != nil || len(executions.Items) != 1 {
		t.Fatalf("bob's executions: %+v %v", executions, err)
	}
	runID := executions.Items[0].ID

	// A record outside the grant is refused, not served.
	if _, err = alice.Work(ctx, work["team-a-web"].ID); err != nil {
		t.Fatalf("alice reading Work beneath her scope: %v", err)
	}
	_, err = alice.Work(ctx, work["team-b"].ID)
	refused("alice reading team-b's Work", err, 403, "forbidden_scope")
	_, err = alice.Run(ctx, runID)
	refused("alice reading team-b's run", err, 403, "forbidden_scope")
	_, err = alice.Execution(ctx, runID)
	refused("alice reading team-b's execution", err, 403, "forbidden_scope")
	_, err = alice.Scope(ctx, "team-b")
	refused("alice reading team-b", err, 403, "forbidden_scope")
	_, err = alice.Work(ctx, "work/missing")
	refused("alice reading Work that doesn't exist", err, 404, "not_found")

	// Controls and Events follow the same reach, checked before anything
	// is written.
	if _, err = alice.HoldWork(ctx, work["team-a-web"].ID, "", "", "hold-web"); err != nil {
		t.Fatalf("alice holding Work beneath her scope: %v", err)
	}
	_, err = alice.CancelWork(ctx, work["team-b"].ID, "", "", "cancel-b")
	refused("alice cancelling team-b's Work", err, 403, "forbidden_scope")
	if w, _ := owner.Work(ctx, work["team-b"].ID); w.State == "cancelled" {
		t.Fatal("team-b's Work was cancelled")
	}
	_, err = alice.CreateWork(ctx, client.WorkRequest{ScopeID: "team-b", Kind: "manual.task", Binding: "run"}, "make-b")
	refused("alice creating Work in team-b", err, 403, "forbidden_scope")
	_, err = alice.SubmitEvent(ctx, client.EventRequest{ScopeID: "team-b", Type: "operator.requested", DeliveryID: "a-1"}, "a-1")
	refused("alice sending an Event to team-b", err, 403, "forbidden_scope")
	e, err := alice.SubmitEvent(ctx, client.EventRequest{ScopeID: "team-a-web", Type: "operator.requested", DeliveryID: "a-2"}, "a-2")
	if err != nil || e.Actor != "operator:alice" {
		t.Fatalf("alice sending an Event beneath her scope: %+v %v", e, err)
	}

	// view at a scope reads it but runs no control there.
	if _, err = bob.Work(ctx, work["team-b"].ID); err != nil {
		t.Fatal(err)
	}
	_, err = bob.CancelWork(ctx, work["team-b"].ID, "", "", "cancel-b")
	refused("bob cancelling with view", err, 403, "forbidden_role")

	// Daemon routes need a daemon-wide role; a scoped one doesn't do.
	_, err = alice.Config(ctx)
	refused("alice reading the daemon's config", err, 403, "forbidden_role")
	_, err = alice.Doctor(ctx, false)
	refused("alice running doctor", err, 403, "forbidden_role")
	_, err = alice.Operators(ctx)
	refused("alice listing operators", err, 403, "forbidden_role")
	_, err = alice.WorkConfigCandidate(ctx, work["team-a"].ID)
	refused("alice reading a config candidate in her own scope", err, 403, "forbidden_role")
	if _, err = alice.Health(ctx); err != nil {
		t.Fatalf("health for a scoped operator: %v", err)
	}

	// A daemon-wide role and grants together: view everywhere, operate in
	// team-b.
	carol := login("carol", "--role", "view", "--grants", "team-b=operate")
	page, err = carol.Works(ctx, nil)
	// Every scope's: the three made above and the one alice's Event made.
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("carol's Work: %+v %v", page, err)
	}
	_, err = carol.HoldWork(ctx, work["team-a"].ID, "", "", "hold-a")
	refused("carol holding team-a's Work", err, 403, "forbidden_scope")

	var operators []client.Operator
	h.mustCLI(&operators, "auth", "operators")
	for _, o := range operators {
		if o.Name == "alice" && (o.Role != "" || len(o.Grants) != 1 || o.Grants[0] != (client.Grant{Scope: "team-a", Role: "operate"})) {
			t.Fatalf("alice listed as %+v", o)
		}
	}
}

// TestAPIRoutesDeclareTargets checks that every management route says
// what it acts on, so the router can check a scoped grant against it: a
// route added without one would be refused to every scoped operator, and
// this names it.
func TestAPIRoutesDeclareTargets(t *testing.T) {
	for _, r := range management.APIRoutes() {
		if r.Target == "" {
			t.Errorf("%s %s declares no target", r.Method, r.Path)
		}
		if strings.HasPrefix(r.Path, "/v1/toolbelt") != (r.Target == "execution token") {
			t.Errorf("%s %s has target %q", r.Method, r.Path, r.Target)
		}
	}
}
