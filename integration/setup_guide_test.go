package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	bb "github.com/open-conduit/open-conduit/extensions/bitbucket"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
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
	"github.com/open-conduit/open-conduit/protocol/examples/runner"
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
	releaseLayout(t)
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
	// What init grants covers what the agent's playbook uses, through the
	// toolset, so nothing is reported.
	if _, warnings, err := bootstrap.CheckConfigAt("", doc); err != nil || len(warnings) != 0 {
		t.Fatalf("init output warnings: %q %v", warnings, err)
	}
	// Negative control: denying an operation the playbook uses is.
	denied := strings.Replace(string(doc), `"@github.author"`, `"@github.author", "!github.pr.edit"`, 1)
	if _, warnings, err := bootstrap.CheckConfigAt("", []byte(denied)); err != nil || len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "uses github.pr.edit") {
		t.Fatalf("denied github.pr.edit not reported: %q %v", warnings, err)
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
	// Slack is a bundled package (#428): validate as the release's binary
	// would, with it beside it.
	releaseLayout(t)
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
	// The setups come from the bundled packages (#477), each its own.
	releaseLayout(t)
	names := map[string]bool{}
	for _, s := range bootstrap.ExtensionOps("test", "abc").Setups("", t.TempDir()) {
		if info := s.SetupInfo(); !info.FirstRun {
			names[info.Type] = true
		}
	}
	for _, want := range []string{"github", "slack", "linear", "jira"} {
		if !names[want] {
			t.Fatalf("bundled setups %v lack %s", names, want)
		}
	}
	// Every init setup has an adapters entry, so what init can add can
	// also be looked up with `adapters TYPE`.
	for typ := range names {
		catalog, _ := bootstrap.Adapters("")
		if !slices.ContainsFunc(catalog, func(a cli.Adapter) bool { return a.Type == typ }) {
			t.Errorf("init %s has no adapters entry", typ)
		}
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
	// Its binding runs claude-code, a bundled package.
	releaseLayout(t)
	if diags, err := bootstrap.ValidateConfigAt(path, doc); doc == nil || err != nil || len(diags) != 0 {
		t.Fatalf("default config: %v %v\n%s", diags, err, doc)
	}
}

// init linear and init jira (06-01) write scopes, here with a Bitbucket code
// host and a checkout, that merge beside each other and validate against the
// daemon registry; the kit's Bitbucket operation list matches the extension.
func TestTrackerSetupScopesValidateOffline(t *testing.T) {
	// The trackers are bundled packages (#428): validate as the release's
	// binary would, with them beside it.
	releaseLayout(t)
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

// init --with claude asks the claude-code package for its interactive
// launch (#495): the setup playbook as its own plugin, and claude started
// on it, pre-allowing only the commands the host decided on.
func TestSetupCommand(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	placeSetup(t, dir) // installed beside config.json
	// The executors and the other setups the agent may dry-run come from
	// the bundled packages (#477).
	releaseLayout(t)
	for _, with := range []string{"claude", "claude-code"} {
		launch, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: with, ConfigPath: filepath.Join(dir, "config.json"), StateDir: state, Cwd: dir, Bin: "/opt/bin/open-conduit", Binary: fake,
			Environ: []string{"PATH=/usr/bin", "OS_TOKEN=leak", "OPEN_CONDUIT_CONFIG=/elsewhere"}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := launch.Cmd
		if launch.Executor != "claude-code" || launch.Title != "Claude Code" || cmd.Dir != dir {
			t.Fatalf("--with %s launched %+v in %s", with, launch, cmd.Dir)
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
	}
	// Negative control: an executor nobody offers a launch for is refused,
	// naming the ones that do.
	_, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "aider", ConfigPath: filepath.Join(dir, "config.json"), StateDir: state, Binary: fake})
	if err == nil || !strings.Contains(err.Error(), "claude-code (or claude)") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("unknown agent: %v", err)
	}
}

// init --with codex asks the codex package: the Codex CLI with the setup
// guide as its first message (Codex loads no plugin skills), in its
// workspace-write sandbox asking before anything else, and with the same
// environment rules.
func TestSetupCommandCodex(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	placeSetup(t, dir)
	releaseLayout(t)
	launch, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "codex", ConfigPath: filepath.Join(dir, "config.json"), StateDir: state, Cwd: dir, Bin: "/opt/bin/open-conduit", Binary: fake,
		Environ: []string{"PATH=/usr/bin", "OS_TOKEN=leak", "OPEN_CONDUIT_CONFIG=/elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := launch.Cmd
	if launch.Title != "Codex" || len(cmd.Args) != 6 || cmd.Args[0] != fake || strings.Join(cmd.Args[1:5], " ") != "--sandbox workspace-write --ask-for-approval on-request" {
		t.Fatalf("launch: %s %q", launch.Title, cmd.Args)
	}
	guide := cmd.Args[5]
	if strings.HasPrefix(guide, "---") || !strings.Contains(guide, "/opt/bin/open-conduit init") || strings.Contains(guide, "{{bin}}") {
		t.Fatalf("guide: %.300s", guide)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "OS_TOKEN") || strings.Contains(env, "/elsewhere") || !strings.Contains(env, "OPEN_CONDUIT_CONFIG="+filepath.Join(dir, "config.json")) {
		t.Fatalf("env: %s", env)
	}
	if _, err := os.Stat(filepath.Join(state, "playbooks", "setup")); err == nil {
		t.Fatal("a Claude Code plugin was written for codex")
	}
}

// init --with takes any installed executor that declares the interactive
// launch control, through the public contract alone (#495): the example
// runner, a shell rather than an agent, is launched with the environment
// it answers, still without the toolbelt credential. An executor that
// doesn't declare the control is refused.
func TestSetupCommandThroughTheContract(t *testing.T) {
	_, runnerBin := extensionBinaries(t)
	e := newExtEnv(t)
	cfgDir := filepath.Dir(e.root)
	placeSetup(t, cfgDir)
	e.install(runner.Manifest(), runnerBin)
	plain := runner.Manifest()
	plain.Name = "plain"
	plain.Provides.Executors[0].Name, plain.Provides.Executors[0].Controls.InteractiveLaunch = "plain", false
	plain.Provides.Checks = nil
	e.install(plain, runnerBin)
	state := filepath.Join(t.TempDir(), "state")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	launch, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "runner", ConfigPath: filepath.Join(cfgDir, "config.json"), StateDir: state, Cwd: cfgDir, Bin: "/opt/bin/open-conduit", Binary: sh,
		Environ: []string{"PATH=/usr/bin:/bin", "OS_TOKEN=leak"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := launch.Cmd
	book := filepath.Join(state, "playbooks", "setup", "playbook.md")
	if launch.Executor != "runner" || cmd.Args[0] != sh || cmd.Args[len(cmd.Args)-1] != book {
		t.Fatalf("launch: %+v %q", launch, cmd.Args)
	}
	if text, err := os.ReadFile(book); err != nil || !strings.Contains(string(text), "/opt/bin/open-conduit init") {
		t.Fatalf("playbook: %v", err)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "OS_TOKEN") || !strings.Contains(env, "RUNNER_PLAYBOOK="+book) || !strings.Contains(env, "OPEN_CONDUIT_CONFIG="+filepath.Join(cfgDir, "config.json")) {
		t.Fatalf("env: %s", env)
	}
	_, err = bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "plain", ConfigPath: filepath.Join(cfgDir, "config.json"), StateDir: state, Binary: sh})
	if err == nil || !strings.Contains(err.Error(), "no setup agent \"plain\"") || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("an executor without the control: %v", err)
	}
}
