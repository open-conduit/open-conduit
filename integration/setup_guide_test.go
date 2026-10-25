package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/executors/claudecode"
	"github.com/alsiola/open-conduit/extension"
	gitws "github.com/alsiola/open-conduit/extensions/git"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/extensions/github/playbooks"
	"github.com/alsiola/open-conduit/internal/bootstrap"
	"github.com/alsiola/open-conduit/internal/cli"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
)

// The setup guide's example configuration is a valid Open Conduit config
// for the executors, handlers, operations and providers the daemon
// registers (bootstrap's registry), so an agent following it writes one
// that `config validate` accepts.
func TestSetupGuideExampleConfigValidates(t *testing.T) {
	g := playbooks.Setup("open-conduit")
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
	g := playbooks.Setup("open-conduit")
	start, end := strings.Index(g, "```json\n"), strings.Index(g, "\n```\n\nOther `github`")
	if diags, err := bootstrap.ValidateConfig([]byte(g[start+8 : end])); err != nil || len(diags) != 0 {
		t.Fatalf("setup guide example: %v %v", diags, err)
	}
	var doc []byte
	for _, s := range []cli.GitHubScope{
		{Repo: "acme/web", Scope: "web", Connection: "github", Token: "cmd:gh auth token", Checkout: t.TempDir(), Holds: true},
		{Repo: "acme/support", Scope: "support", Connection: "github", Token: "keyring:github"},
	} {
		var err error
		if doc, err = cli.AddGitHubScope(doc, s, false); err != nil {
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

// init --with claude prepares the setup guide as its own plugin and starts
// claude on it, pre-allowing only open-conduit's config, doctor and health.
func TestSetupCommand(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	cmd, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: "claude", ConfigPath: filepath.Join(dir, "config.json"), StateDir: state, Cwd: dir, Bin: "/opt/bin/open-conduit", Binary: fake,
		Environ: []string{"PATH=/usr/bin", "OS_TOKEN=leak", "OPEN_CONDUIT_CONFIG=/elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	plugin := filepath.Join(state, "playbooks", "setup")
	if !strings.HasPrefix(args, fake+" /open-conduit:setup --plugin-dir "+plugin+" --allowedTools") || !strings.Contains(args, "Bash(/opt/bin/open-conduit config:*)") || !strings.Contains(args, "Bash(/opt/bin/open-conduit doctor:*)") || strings.Contains(args, "serve") {
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
