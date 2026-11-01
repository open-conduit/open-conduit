package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
	bb "github.com/open-conduit/open-conduit/extensions/bitbucket"
	gitws "github.com/open-conduit/open-conduit/extensions/git"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/jira"
	"github.com/open-conduit/open-conduit/extensions/linear"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// The setup guide's example configuration is a valid Open Conduit config
// for the executors, handlers, operations and providers the daemon
// registers (bootstrap's registry), so an agent following it writes one
// that `config validate` accepts.
func TestSetupGuideExampleConfigValidates(t *testing.T) {
	g := setupPlaybook(t, "open-conduit")
	start, end := strings.Index(g, "```json\n"), strings.Index(g, "\n```\n\nOther `github`")
	if start < 0 || end < 0 {
		t.Fatal("no example config in the guide")
	}
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir := filepath.Join(t.TempDir(), "claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	claude, err := claudecode.New(claudecode.Options{StateDir: dir, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	reg := config.Registry{
		Executors:      map[string]extension.Executor{"claude-code": claude, gh.HoldExecutor: gh.NewHoldExecutor()},
		Handlers:       map[string]bool{"manual": true, gh.Type: true, gh.HoldHandler: true},
		Extensions:     map[string]config.ExtensionType{gh.Type: {Ingress: true, Validate: gh.ValidateExtension}},
		Connections:    map[string]func(json.RawMessage) error{gh.Type: gh.ValidateConnection},
		Operations:     gh.Operations(),
		Resources:      map[string]bool{gitws.Type: true},
		ResourceConfig: map[string]func(json.RawMessage) error{gitws.Type: gitws.ValidateConfig},
	}
	if _, err := config.Validate([]byte(g[start+8:end]), reg); err != nil {
		t.Fatalf("the setup guide's example config is invalid: %v", err)
	}
}

// The daemon's own offline validator (what `config check`, `init` and
// `config set` use) accepts the setup guide's example and every scope shape
// `init` writes: with a checkout, workspace-free, and with holds.
func TestOfflineValidationMatchesTheDaemonRegistry(t *testing.T) {
	g := setupPlaybook(t, "open-conduit")
	start, end := strings.Index(g, "```json\n"), strings.Index(g, "\n```\n\nOther `github`")
	if diags, err := bootstrap.ValidateConfig([]byte(g[start+8 : end])); err != nil || len(diags) != 0 {
		t.Fatalf("setup guide example: %v %v", diags, err)
	}
	var doc []byte
	for _, s := range []gh.GitHubScope{
		{Repo: "acme/web", Scope: "web", Connection: "github", Token: "cmd:gh auth token", Checkout: t.TempDir(), Holds: true},
		{Repo: "acme/support", Scope: "support", Connection: "github", Token: "keyring:github"},
	} {
		r, err := s.Result()
		if err != nil {
			t.Fatal(err)
		}
		if doc, err = cli.MergeSetup(doc, r, false); err != nil {
			t.Fatal(err)
		}
	}
	if diags, err := bootstrap.ValidateConfig(doc); err != nil || len(diags) != 0 {
		t.Fatalf("init output: %v %v\n%s", diags, err, doc)
	}
	bad := strings.Replace(string(doc), `"executor": "claude-code"`, `"executor": "nope"`, 1)
	if diags, err := bootstrap.ValidateConfig([]byte(bad)); err != nil || len(diags) == 0 {
		t.Fatalf("invalid executor accepted: %v %v", diags, err)
	}
}

// init slack's workspace-free scope (here from token references,
// so nothing is called) merges beside a GitHub scope and validates against
// the daemon registry, with no git resource or provider in it.
func TestSlackSetupScopeValidatesOffline(t *testing.T) {
	r, err := sl.Setup{}.Setup(context.Background(), nil, []string{"--bot-token", "keyring:slack-bot-token", "--app-token", "keyring:slack-app-token", "--team", "T0123", "--channel", "C0SUPPORT"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gh.GitHubScope{Repo: "acme/web", Scope: "web", Connection: "github", Token: "cmd:gh auth token"}.Result()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := cli.MergeSetup(nil, g, false)
	if err != nil {
		t.Fatal(err)
	}
	if doc, err = cli.MergeSetup(doc, r, false); err != nil {
		t.Fatal(err)
	}
	if diags, err := bootstrap.ValidateConfig(doc); err != nil || len(diags) != 0 {
		t.Fatalf("init slack output: %v %v\n%s", diags, err, doc)
	}
	if strings.Contains(string(r.Scope), `"resource"`) || strings.Contains(string(r.Scope), "git") {
		t.Fatalf("init slack scope reaches a Git concept: %s", r.Scope)
	}
	names := []string{}
	for _, s := range bootstrap.Setups() {
		names = append(names, s.SetupInfo().Type)
	}
	if strings.Join(names, ",") != "github,slack,linear,jira" {
		t.Fatalf("registered setups: %v", names)
	}
}

// The default configuration (init setup into an empty file) validates
// against the daemon registry with the setup package installed, so a daemon
// with no config file can start. Without the package there is none.
func TestDefaultConfigValidatesOffline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if doc := bootstrap.DefaultConfig(path, filepath.Join(dir, "state")); doc != nil {
		t.Fatalf("a default config with no setup package: %s", doc)
	}
	placeSetup(t, dir)
	doc := bootstrap.DefaultConfig(path, filepath.Join(dir, "state"))
	if diags, err := bootstrap.ValidateConfigAt(path, doc); doc == nil || err != nil || len(diags) != 0 {
		t.Fatalf("default config: %v %v\n%s", diags, err, doc)
	}
}

// init linear and init jira (06-01) write scopes, here with a Bitbucket code
// host and a checkout, that merge beside each other and validate against the
// daemon registry; the kit's Bitbucket operation list matches the extension.
func TestTrackerSetupScopesValidateOffline(t *testing.T) {
	ctx := context.Background()
	lr, err := linear.Setup{}.Setup(ctx, nil, []string{"--teams", "eng,ops", "--token", "keyring:linear-token", "--no-workspace",
		"--bitbucket", "acme/app", "--bitbucket-username", "me@example.com", "--bitbucket-token", "keyring:bitbucket-token"})
	if err != nil {
		t.Fatal(err)
	}
	jr, err := jira.Setup{}.Setup(ctx, &setupHost{cwd: t.TempDir()}, []string{"--site", "https://acme.atlassian.net", "--projects", "PROJ", "--email", "me@example.com",
		"--token", "env:JIRA_TOKEN", "--checkout", t.TempDir(), "--jql", "component = Web", "--fields", "customfield_10010"})
	if err != nil {
		t.Fatal(err)
	}
	var doc []byte
	for _, r := range []extension.SetupResult{lr, jr} {
		if doc, err = cli.MergeSetup(doc, r, false); err != nil {
			t.Fatal(err)
		}
	}
	if diags, err := bootstrap.ValidateConfig(doc); err != nil || len(diags) != 0 {
		t.Fatalf("init linear/jira output: %v %v\n%s", diags, err, doc)
	}
	for _, want := range []string{`"handler": "linear"`, `"handler": "bitbucket"`, `"handler": "jira"`, `"playbook": "work-tracker-issue"`, `"issue_ref": "Closes {key}"`} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("setup output lacks %s", want)
		}
	}
	if strings.Join(tracker.BitbucketOperations, ",") != strings.Join(bb.Operations(), ",") {
		t.Fatal("the kit's Bitbucket operation list drifted from the extension's")
	}
	if _, err := (linear.Setup{}).Setup(ctx, nil, []string{"--teams", "ENG", "--token", "plain-secret"}); !errors.Is(err, extension.ErrSetupUsage) {
		t.Fatalf("a secret value as --token: %v", err)
	}
}

type setupHost struct{ cwd string }

func (h *setupHost) Printf(string, ...any) {}
func (h *setupHost) Prompt(string, extension.Prompt) (string, error) {
	return "", extension.ErrSetupNoInput
}
func (h *setupHost) Choose(string, []string) (int, error)                 { return 0, extension.ErrSetupNoInput }
func (h *setupHost) OpenURL(string) error                                 { return nil }
func (h *setupHost) StoreSecret(name, _ string) (string, error)           { return "keyring:" + name, nil }
func (h *setupHost) LookPath(string) (string, bool)                       { return "", false }
func (h *setupHost) Run(context.Context, string, string, ...string) error { return nil }
func (h *setupHost) Config() []byte                                       { return nil }
func (h *setupHost) Cwd() string                                          { return h.cwd }
func (h *setupHost) StateDir() string                                     { return h.cwd }
func (h *setupHost) DryRun() bool                                         { return true }

// init --with claude prepares the setup playbook as its own plugin and starts
// claude on it, pre-allowing only open-conduit's config, doctor and health.
func TestSetupCommand(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	placeSetup(t, dir) // installed beside config.json
	cmd, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "claude", ConfigPath: filepath.Join(dir, "config.json"), StateDir: state, Cwd: dir, Bin: "/opt/bin/open-conduit", Binary: fake,
		Environ: []string{"PATH=/usr/bin", "OS_TOKEN=leak", "OPEN_CONDUIT_CONFIG=/elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	plugin := filepath.Join(state, "playbooks", "setup")
	if !strings.HasPrefix(args, fake+" /open-conduit:setup --plugin-dir "+plugin+" --allowedTools") || !strings.Contains(args, "Bash(/opt/bin/open-conduit config:*)") || !strings.Contains(args, "Bash(/opt/bin/open-conduit doctor:*)") || !strings.Contains(args, "Bash(/opt/bin/open-conduit init github --dry-run:*)") || strings.Contains(args, "init setup") || strings.Contains(args, "serve") {
		t.Fatalf("args: %s", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "OS_TOKEN") || strings.Contains(env, "/elsewhere") || !strings.Contains(env, "OPEN_CONDUIT_CONFIG="+filepath.Join(dir, "config.json")) {
		t.Fatalf("env: %s", env)
	}
	skill, err := os.ReadFile(filepath.Join(plugin, "skills", "setup", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), "/opt/bin/open-conduit init") || strings.Contains(string(skill), "{{bin}}") {
		t.Fatalf("skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plugin, "skills", "work-issue")); err == nil {
		t.Fatal("turn playbooks leaked into the setup plugin")
	}
	if _, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "codex", StateDir: state, Binary: fake}); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

// Every operation the setup playbook names is one the setup package
// declares or core registers, or a probe a session can run: what
// `open-conduit extension check` checks for every package. The setup
// package's own probes run inside its process, so the check cannot see
// them offline and they are allowed by name.
func TestSetupPlaybookNamesRegisteredOperations(t *testing.T) {
	if g := setupPlaybook(t, "open-conduit"); !strings.HasPrefix(g, "---\nname: setup\n") {
		t.Fatal("the setup playbook has no frontmatter")
	}
	r := bootstrap.ExtensionOps("test", "").Check(setupPackage(t), []string{"setup.fs.list", "setup.tools"})
	if !r.OK || len(r.Findings) != 0 {
		t.Fatalf("extension check on the setup package: %+v", r.Findings)
	}
	// The check does flag a name the package neither declares nor allows.
	if r := bootstrap.ExtensionOps("test", "").Check(setupPackage(t), nil); r.OK || len(r.Findings) != 2 || r.Findings[0].Code != "unknown_operation" {
		t.Fatalf("extension check without the package's probes: %+v", r.Findings)
	}
}
