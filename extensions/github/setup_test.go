package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

// host is a minimal non-interactive setup host.
type host struct{ cwd string }

func (h host) Printf(string, ...any)                                {}
func (h host) Prompt(string, extension.Prompt) (string, error)      { return "", extension.ErrSetupNoInput }
func (h host) Choose(string, []string) (int, error)                 { return -1, extension.ErrSetupNoInput }
func (h host) OpenURL(string) error                                 { return nil }
func (h host) StoreSecret(name, _ string) (string, error)           { return "keyring:" + name, nil }
func (h host) LookPath(string) (string, bool)                       { return "", false }
func (h host) Run(context.Context, string, string, ...string) error { return errors.New("no commands") }
func (h host) Config() []byte                                       { return nil }
func (h host) Cwd() string                                          { return h.cwd }
func (h host) StateDir() string                                     { return h.cwd }
func (h host) DryRun() bool                                         { return true }

type scopeShape struct {
	ID       string           `json:"id"`
	Bindings []map[string]any `json:"bindings"`
	Routes   []map[string]any `json:"routes"`
}

func TestGitHubScopeConfig(t *testing.T) {
	ws := gh.GitHubScope{Repo: "acme/web", Scope: "web", Connection: "github", Token: "cmd:gh auth token", Checkout: "/src/web", Holds: true}
	r, err := ws.Result()
	if err != nil {
		t.Fatal(err)
	}
	var s scopeShape
	if err = json.Unmarshal(r.Scope, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Bindings) != 2 || s.Bindings[0]["resource"] == nil || len(s.Routes) != 2 || s.Routes[0]["handler"] != "github-hold" {
		t.Fatalf("scope: %+v", s)
	}
	if !strings.Contains(string(r.Scope), `"github.git.credential"`) {
		t.Fatalf("a workspace binding is not granted github.git.credential: %s", r.Scope)
	}
	if s.Bindings[0]["config"].(map[string]any)["playbook"] != gh.Playbook {
		t.Fatalf("the agent binding names no playbook: %+v", s.Bindings[0])
	}
	if len(r.Connections) != 1 || !strings.Contains(string(r.Connections[0]), `"cmd:gh auth token"`) {
		t.Fatalf("connection: %s", r.Connections)
	}
	free := gh.GitHubScope{Repo: "acme/support", Scope: "support", Connection: "github", Token: "env:GH"}
	if r, err = free.Result(); err != nil {
		t.Fatal(err)
	}
	s = scopeShape{}
	if err = json.Unmarshal(r.Scope, &s); err != nil {
		t.Fatal(err)
	}
	if b := s.Bindings[0]; b["resource"] != nil || b["requires"].(map[string]any)["workspace"] != false {
		t.Fatalf("workspace-free binding: %+v", b)
	}
	if strings.Contains(string(r.Scope), `"github.git.credential"`) {
		t.Fatalf("a workspace-free binding is granted github.git.credential: %s", r.Scope)
	}
	if !strings.Contains(strings.Join(r.Summary, "\n"), "workspace-free") {
		t.Fatalf("summary: %v", r.Summary)
	}
	// Next says what gets picked up and how to preview it.
	next := strings.Join(r.Next, "\n")
	for _, want := range []string{"serve --dry-run", "owner, members or collaborators", "forks", "backlog, human or milestone", "--require LABEL"} {
		if !strings.Contains(next, want) {
			t.Fatalf("next lacks %q: %s", want, next)
		}
	}
	// --require LABEL picks up only the issues labelled with it.
	opt := gh.GitHubScope{Repo: "acme/support", Scope: "support", Connection: "github", Token: "env:GH", Require: "agent"}
	if r, err = opt.Result(); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Extensions []struct {
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if err = json.Unmarshal(r.Scope, &cfg); err != nil || len(cfg.Extensions) != 1 {
		t.Fatalf("scope %s: %v", r.Scope, err)
	}
	settings, err := gh.ParseExtension(cfg.Extensions[0].Config, nil)
	if err != nil || len(settings.Labels.Require) != 1 || settings.Labels.Require[0] != "agent" || !strings.Contains(strings.Join(r.Next, "\n"), "labelled agent") {
		t.Fatalf("require: %+v %v %v", settings.Labels, err, r.Next)
	}
}

func TestGitHubSetupFlags(t *testing.T) {
	setup := gh.Setup{}
	if setup.SetupInfo().Type != "github" {
		t.Fatal(setup.SetupInfo())
	}
	h := host{cwd: t.TempDir()}
	for _, args := range [][]string{{"--token", "ghp_literal"}, {"--repo", "acme/web", "--token", "ghp_literal"}, {"--repo", "acme/web", "--scope", "Bad"}, {"--repo", "acme/web", "extra"}} {
		if _, err := setup.Setup(context.Background(), h, args); !errors.Is(err, extension.ErrSetupUsage) {
			t.Errorf("%v: %v", args, err)
		}
	}
	r, err := setup.Setup(context.Background(), h, []string{"--repo", "acme/My.Repo", "--no-workspace", "--holds"})
	if err != nil {
		t.Fatal(err)
	}
	var s scopeShape
	if err = json.Unmarshal(r.Scope, &s); err != nil || s.ID != "my-repo" || len(s.Routes) != 2 {
		t.Fatalf("%v %s", err, r.Scope)
	}
}
