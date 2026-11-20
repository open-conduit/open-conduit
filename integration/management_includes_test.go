package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// includesConfig attaches two submitted includes, web and api, under the
// scope teams, beside a scope of the config file's own. Their fragments
// may define bindings for the fixture executor, and select no secret.
const includesConfig = `{"version": 1, "settings": {}, "connections": [],
  "includes": [{"name": "web", "parent": "teams", "executors": ["fixture"]}, {"name": "api", "parent": "teams", "executors": ["fixture"]}],
  "scopes": [
    {"id": "teams", "name": "teams",
     "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome"], "read_scopes": []}},
    {"id": "platform", "name": "platform"}]}`

// teamFragment is one scope, id, with a binding and route.
func teamFragment(id string) []byte {
	return []byte(`{"version": 1, "scopes": [{"id": "` + id + `", "name": "` + id + `",
    "bindings": [{"name": "run", "executor": "fixture", "requires": {"toolbelt": "cli"}, "operations": ["emit"], "config": {}}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}]}]}`)
}

// TestManagementSubmittedIncludes checks that a team granted configure on
// an include's parent applies that include's fragment and nothing else:
// two teams' applies never conflict with each other, a stale fragment hash
// does, a fragment is validated with the whole configuration and its
// diagnostics say whose they are, removing a scope still drains, a
// config file apply keeps what the teams applied, and each apply is in the
// operator trail under its caller.
func TestManagementSubmittedIncludes(t *testing.T) {
	h := newManagementHarnessWith(t, includesConfig, nil)
	ctx := context.Background()
	owner := h.operatorClient()
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
	web := login("web-team", "--grants", "teams=configure")
	api := login("api-team", "--grants", "teams=configure")
	viewer := login("viewer", "--grants", "teams=view")
	stranger := login("stranger", "--grants", "platform=configure")

	// Both teams read their empty include, then apply in turn over it:
	// the second isn't refused for the first's generation.
	webFrag, err := web.Include(ctx, "web")
	if err != nil || webFrag.Parent != "teams" || webFrag.Generation != "1" {
		t.Fatalf("web's include: %+v %v", webFrag, err)
	}
	apiFrag, err := api.Include(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := web.ApplyFragmentText(ctx, "web", teamFragment("web"), webFrag.Hash, "web-1")
	if err != nil || applied.Generation != "2" || applied.Hash == webFrag.Hash {
		t.Fatalf("web's apply: %+v %v", applied, err)
	}
	if _, err = api.ApplyFragmentText(ctx, "api", teamFragment("api"), apiFrag.Hash, "api-1"); err != nil {
		t.Fatalf("api's apply after web's: %v", err)
	}
	if sc, err := owner.Scope(ctx, "web"); err != nil || sc.ParentID != "teams" {
		t.Fatalf("web scope: %+v %v", sc, err)
	}

	// A stale fragment hash is refused, and so is an include no grant
	// reaches, a caller with only view, and one that doesn't exist.
	_, err = web.ApplyFragmentText(ctx, "web", teamFragment("web"), webFrag.Hash, "web-stale")
	refused("a stale fragment hash", err, 409, "fragment_conflict")
	_, err = stranger.ApplyFragmentText(ctx, "web", teamFragment("web"), applied.Hash, "stranger-1")
	refused("configure on another scope", err, 403, "forbidden_scope")
	_, err = stranger.Include(ctx, "web")
	refused("reading an include no grant covers", err, 403, "forbidden_scope")
	if _, err = viewer.Include(ctx, "web"); err != nil {
		t.Fatalf("view reading the include: %v", err)
	}
	_, err = viewer.ApplyFragmentText(ctx, "web", teamFragment("web"), applied.Hash, "viewer-1")
	refused("view applying", err, 403, "forbidden_role")
	_, err = owner.Include(ctx, "nowhere")
	refused("an include that isn't declared", err, 404, "not_found")
	_, err = web.Config(ctx)
	refused("configure reading the whole config", err, 403, "forbidden_role")

	// A fragment is checked with the whole configuration: clashing with a
	// scope elsewhere is the fragment's to fix, and says so.
	_, err = web.ApplyFragmentText(ctx, "web", teamFragment("platform"), applied.Hash, "web-clash")
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_config" || len(apiErr.Details) == 0 || !strings.HasPrefix(apiErr.Details[0].Path, "fragment/scopes/0") {
		t.Fatalf("a clashing fragment: %+v", err)
	}

	// It may define bindings only for the executors the include allows,
	// and select only the secrets it allows.
	other := strings.Replace(string(teamFragment("web")), `"executor": "fixture"`, `"executor": "shell"`, 1)
	_, err = web.ApplyFragmentText(ctx, "web", []byte(other), applied.Hash, "web-shell")
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_config" || !strings.Contains(apiErr.Error(), "fragment/scopes/0/bindings/0/executor: not_allowed") {
		t.Fatalf("an executor the include doesn't allow: %v", err)
	}
	secret := strings.Replace(string(teamFragment("web")), `"operations": ["emit"],`, `"operations": ["emit"], "secret_names": ["key"],`, 1)
	_, err = web.ApplyFragmentText(ctx, "web", []byte(secret), applied.Hash, "web-secret")
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Error(), "fragment/scopes/0/bindings/0/secret_names/0: not_allowed") {
		t.Fatalf("a secret the include doesn't allow: %v", err)
	}

	// A config file apply validated before a team's apply still applies
	// over it, and keeps it.
	current, err := owner.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fileText, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	renamed := []byte(strings.Replace(string(fileText), `"name": "platform"`, `"name": "Platform"`, 1))
	v, err := owner.ValidateConfig(ctx, renamed)
	if err != nil {
		t.Fatal(err)
	}
	apiNow, err := api.Include(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	apiBound := strings.Replace(string(teamFragment("api")), `"name": "api"`, `"name": "api team"`, 1)
	if _, err = api.ApplyFragmentText(ctx, "api", []byte(apiBound), apiNow.Hash, "api-2"); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.ApplyConfigText(ctx, renamed, v.CandidateHash, current.Generation, "file-over-fragment"); err != nil {
		t.Fatalf("a config file apply staled only by a fragment apply: %v", err)
	}
	if sc, err := owner.Scope(ctx, "api"); err != nil || sc.Name != "api team" {
		t.Fatalf("the config file apply undid api's: %+v %v", sc, err)
	}
	// A config file apply since its base still conflicts.
	_, err = owner.ApplyConfigText(ctx, fileText, mustValidate(t, owner, fileText), current.Generation, "file-stale")
	refused("a config file apply based before another", err, 409, "generation_conflict")

	// Removing a scope with live Work is refused until it drains.
	w, err := owner.CreateWork(ctx, client.WorkRequest{ScopeID: "web", Kind: "manual.task", Binding: "run"}, "make-web")
	if err != nil {
		t.Fatal(err)
	}
	empty := []byte(`{"version": 1, "scopes": []}`)
	_, err = web.ApplyFragmentText(ctx, "web", empty, applied.Hash, "web-empty")
	refused("removing a scope with live Work", err, 409, "scope_in_use")
	if _, err = owner.CancelWork(ctx, w.Work.ID, "", "", "cancel-web"); err != nil {
		t.Fatal(err)
	}
	if _, err = web.ApplyFragmentText(ctx, "web", empty, applied.Hash, "web-empty-2"); err != nil {
		t.Fatalf("removing a drained scope: %v", err)
	}
	if after, err := web.Include(ctx, "web"); err != nil || strings.Contains(after.FragmentText, `"id"`) {
		t.Fatalf("web's include after removal: %+v %v", after, err)
	}
	if exported, err := owner.ExportConfig(ctx); err != nil || strings.Contains(exported.ConfigText, "submitted:web") {
		t.Fatalf("export after removal: %v", err)
	}

	// A config file apply keeps the teams' fragments; export shows the
	// includes, not their scopes.
	exported, err := owner.ExportConfig(ctx)
	if err != nil || !strings.Contains(exported.ConfigText, `"name": "api"`) || strings.Contains(exported.ConfigText, `"id": "api"`) {
		t.Fatalf("export: %v %s", err, exported.ConfigText)
	}
	h.mustCLI(nil, "config", "apply", h.configPath)
	if _, err = owner.Scope(ctx, "api"); err != nil {
		t.Fatalf("api after a config file apply: %v", err)
	}

	// The CLI reads and applies a fragment too.
	var shown client.Include
	h.mustCLI(&shown, "config", "include", "show", "api")
	file := filepath.Join(h.state, "api.json")
	if err = os.WriteFile(file, []byte(strings.Replace(string(teamFragment("api")), `"name": "api"`, `"name": "API"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out client.FragmentApplied
	h.mustCLI(&out, "config", "include", "apply", "api", file, "--hash", shown.Hash)
	if sc, err := owner.Scope(ctx, "api"); err != nil || sc.Name != "API" {
		t.Fatalf("api after the CLI's apply: %+v %v", sc, err)
	}

	outcomes := map[string]int{}
	for _, r := range h.observed.named("operator.include_applied") {
		outcomes[r.Attributes["operator.actor"]+" "+r.Attributes["operator.target"]+" "+r.Attributes["operator.outcome"]]++
	}
	for _, want := range []string{"operator:web-team web ok", "operator:api-team api ok", "operator:web-team web fragment_conflict", "operator:web-team web scope_in_use", "operator:local api ok"} {
		if outcomes[want] == 0 {
			t.Errorf("trail lacks %q: %v", want, outcomes)
		}
	}
}

func mustValidate(t *testing.T, c *client.Client, text []byte) string {
	t.Helper()
	v, err := c.ValidateConfig(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	return v.CandidateHash
}
