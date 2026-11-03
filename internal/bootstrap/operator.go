package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// Operator setup, doctor and controls (03-05) composed from public
// extension surfaces: offline validation against the daemon's own
// registrations, the agent setup session, Work annotations, attach, and the
// Claude Code doctor check.

// registry is the daemon's registration of executors, route handlers,
// operations, connection and extension types and resource providers.
func registry(executors map[string]extension.Executor) config.Registry {
	reg := config.Registry{Executors: executors, Handlers: handlerNames()}
	inprocRegistry(&reg, firstParty())
	containerRegistry(&reg)
	return reg
}

// ValidateConfig validates a config document offline against the same
// registrations `serve` uses, without a daemon, storage or secret
// resolution. It returns the diagnostics (empty when valid).
func ValidateConfig(data []byte) ([]config.Diagnostic, error) {
	return ValidateConfigAt("", data)
}

// ValidateConfigAt is ValidateConfig for a config file at configPath: the
// extension packages installed beside it contribute their declared types,
// and each validates its own opaque config, started for validate alone
// (offlineExternal).
func ValidateConfigAt(configPath string, data []byte) ([]config.Diagnostic, error) {
	return validateWith(configPath, "", data)
}

// validateWith validates offline, leaving out the installed package named
// exclude (the removal check).
func validateWith(configPath, exclude string, data []byte) ([]config.Diagnostic, error) {
	diags, _, err := checkWith(configPath, exclude, data)
	return diags, err
}

// CheckConfigAt is ValidateConfigAt plus config.ModeWarnings and the
// executors' config.BindingWarnings and config.GrantWarnings from the same validation, so a config
// check starts each installed package once. A config that doesn't validate
// has no such warnings (its diagnostics say what is wrong).
func CheckConfigAt(configPath string, data []byte) ([]config.Diagnostic, []string, error) {
	return checkWith(configPath, "", data)
}

func checkWith(configPath, exclude string, data []byte) ([]config.Diagnostic, []string, error) {
	reg := registry(offlineExecutors())
	pkgs := offlineExternal(&reg, configPath, exclude)
	defer pkgs.close()
	s, err := config.ValidateFile(data, reg, fileIncludes(configPath))
	if err == nil {
		return nil, append(append(config.ModeWarnings(s, reg), config.BindingWarnings(s, reg)...), config.GrantWarnings(s, reg)...), nil
	}
	var invalid *config.ValidationError
	if errors.As(err, &invalid) {
		return append(invalid.Diagnostics, pkgs.diagnostics()...), nil, nil
	}
	return nil, nil, err
}

// fileIncludes resolves a config file's includes beside it. Offline there is
// no committed generation, so a folder that has gone contributes nothing.
func fileIncludes(configPath string) config.Includes {
	if configPath == "" {
		return config.Includes{}
	}
	return config.Includes{Base: filepath.Dir(configPath)}
}

// offlineExecutors are the first-party executor registrations for offline
// checks: capability and config checks need only the registrations, and
// executors' capabilities and config validators do not touch runtime state.
func offlineExecutors() map[string]extension.Executor {
	out := map[string]extension.Executor{}
	inprocExecutors(firstParty(), out, map[string]string{})
	return out
}

// DefaultConfig is the configuration the daemon runs on when it starts with
// no config file and no stored generation: what `init` of the first-run
// setup would write into an empty file, so the web view can start a setup
// session. The first-run setup is a registered one, or one of the packages
// installed beside configPath or bundled with the binary.
func DefaultConfig(configPath, stateDir string) []byte {
	return firstRunConfig(extensionSetups(configPath, stateDir, ""))
}

// firstRunConfig is the first first-run setup's result with no flags,
// merged into an empty config; nil when there is none or it fails. The
// setup's host prompts for nothing, stores nothing and runs nothing.
func firstRunConfig(setups []extension.Setup) []byte {
	for _, s := range setups {
		if !s.SetupInfo().FirstRun {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r, err := s.Setup(ctx, firstRunHost{}, nil)
		cancel()
		if err != nil {
			return nil
		}
		doc, err := cli.MergeSetup(nil, r, false)
		if err != nil {
			return nil
		}
		return doc
	}
	return nil
}

// firstRunHost is the host a first-run setup builds the default
// configuration with: no operator, no config, no secrets and nothing run.
type firstRunHost struct{ probeSetupHost }

func (firstRunHost) LookPath(string) (string, bool) { return "", false }

// SetupOptions start a coding agent with the setup guide.
type SetupOptions struct {
	// Agent is claude (or claude-code) or codex.
	Agent      string
	ConfigPath string
	StateDir   string
	Cwd        string
	// Bin is how the agent runs open-conduit (its name when PATH finds this
	// executable, else the path).
	Bin string
	// Binary is the agent's CLI (default claude or codex).
	Binary string
	// Environ is the process environment (default os.Environ).
	Environ []string
}

// SetupCommand prepares the setup playbook as its own one-skill plugin and
// returns the interactive claude command
// started on it (v1 `init --with`). Reading and changing Open Conduit's own
// config, running doctor and dry runs of each setup go without asking;
// nothing else is pre-allowed.
func SetupCommand(o SetupOptions) (*exec.Cmd, error) {
	codex := o.Agent == CodexAgent
	if o.Agent != "claude" && o.Agent != "claude-code" && !codex {
		return nil, fmt.Errorf("no setup agent %q: use --with claude or --with codex", o.Agent)
	}
	if o.Binary == "" {
		o.Binary = "claude"
		if codex {
			o.Binary = "codex"
		}
	}
	if o.Bin == "" {
		o.Bin = "open-conduit"
	}
	dir := filepath.Join(o.StateDir, "playbooks", "setup")
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	name, text, err := firstRunPlaybook(o.ConfigPath, o.Bin)
	if err != nil {
		return nil, err
	}
	if !codex {
		if err := writePlugin(dir, "Sets Open Conduit up", map[string]string{name: text}); err != nil {
			return nil, err
		}
	}
	if _, err := exec.LookPath(o.Binary); err != nil {
		if codex {
			return nil, fmt.Errorf("%s is not on PATH: install the Codex CLI first", o.Binary)
		}
		return nil, fmt.Errorf("%s is not on PATH: install Claude Code first", o.Binary)
	}
	if codex {
		// Codex loads no plugin skills and has no allowed-tools list: the
		// guide is the session's first message, and Codex's own sandbox
		// asks the owner before any command that writes outside the
		// checkout, the config included.
		return agentCommand(o, []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request", setupPrompt(text)}), nil
	}
	// The prompt first and the tools last: --allowedTools takes every
	// argument after it.
	args := []string{"/" + pluginName + ":" + name, "--plugin-dir", dir, "--allowedTools"}
	allowed := []string{"config", "doctor", "health", "secret path"}
	for _, t := range dryRunSetups(o.ConfigPath, o.StateDir) {
		allowed = append(allowed, "init "+t+" --dry-run")
	}
	for _, sub := range allowed {
		args = append(args, "Bash("+o.Bin+" "+sub+":*)")
	}
	return agentCommand(o, args), nil
}

// CodexAgent is `init --with codex`: the Codex CLI.
const CodexAgent = "codex"

// setupPrompt is the setup playbook as a first message: its text without
// the skill frontmatter.
func setupPrompt(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
			text = text[4+i+5:]
		}
	}
	return strings.TrimSpace(text)
}

// agentCommand is the interactive agent command in the owner's
// directory, with no toolbelt credential in its environment and the
// config it sets up named.
func agentCommand(o SetupOptions, args []string) *exec.Cmd {
	cmd := exec.Command(o.Binary, args...)
	cmd.Dir = o.Cwd
	env := o.Environ
	if env == nil {
		env = os.Environ()
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "OS_TOKEN" || name == "OS_ENDPOINT" || name == "OS_TOKEN_FILE" || name == "OPEN_CONDUIT_CONFIG" {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "OPEN_CONDUIT_CONFIG="+o.ConfigPath)
	return cmd
}

// dryRunSetups are the setup types whose `init TYPE --dry-run` the setup
// agent may run without asking: every registered or installed setup but the
// first-run one, so no playbook's choice of setup is wired into core.
func dryRunSetups(configPath, stateDir string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range extensionSetups(configPath, stateDir, "") {
		info := s.SetupInfo()
		if !info.FirstRun && !seen[info.Type] {
			seen[info.Type] = true
			out = append(out, info.Type)
		}
	}
	return out
}

// firstRunPlaybook is the setup playbook `init --with claude` starts on:
// the first-run setup's playbook, named after its type, with `{{bin}}` as
// the agent runs open-conduit. It comes from the first-run package
// installed beside configPath or bundled with the binary (#243).
func firstRunPlaybook(configPath, bin string) (string, string, error) {
	found, _ := discoverPackages(configPath, "")
	for _, f := range found {
		d := f.Manifest.Provides.Setup
		if d == nil || !d.FirstRun {
			continue
		}
		books, err := extproc.Playbooks(f.Dir, f.Manifest)
		if text, ok := books[d.Type]; ok {
			return d.Type, extproc.RenderPlaybook(text, bin), nil
		}
		if err != nil {
			return "", "", fmt.Errorf("the %s package's playbooks: %w", f.Manifest.Name, err)
		}
		return "", "", fmt.Errorf("the %s package has no %s playbook", f.Manifest.Name, d.Type)
	}
	return "", "", errors.New("no setup package is installed: install it with open-conduit extension install")
}

// annotate contributes capacity and the extensions' context for Work.
func (d *daemon) annotate(ctx context.Context, w domain.Work) []management.Annotation {
	out := d.capacityAnnotation(ctx, w)
	out = append(out, d.inprocAnnotations(ctx, w)...)
	return out
}

// actions are the action providers on a scope, every extension's.
func (d *daemon) actions(_ context.Context, scope domain.ID) map[string]extension.ActionProvider {
	out := map[string]extension.ActionProvider{}
	d.inprocActions(scope, out)
	return out
}

// attach returns the attach command of the Work's executor registration,
// through the optional public extension.AttachExecutor.
func (d *daemon) attach(ctx context.Context, executor string, w domain.Work) (management.AttachSpec, error) {
	a, ok := d.executors[executor].(extension.AttachExecutor)
	if !ok {
		return management.AttachSpec{}, management.ErrAttachUnsupported
	}
	spec, err := a.Attach(ctx, w.ID)
	if errors.Is(err, extension.ErrUnsupported) {
		// A package's executor that doesn't declare the control.
		return management.AttachSpec{}, management.ErrAttachUnsupported
	}
	if err != nil {
		return management.AttachSpec{}, err
	}
	return management.AttachSpec{Argv: spec.Argv, Dir: spec.Dir}, nil
}

// live returns a running attempt's output so far from its executor
// registration, through the optional public extension.LiveExecutor.
func (d *daemon) live(ctx context.Context, executor string, execution domain.ID, tail int64) ([]extension.LiveOutput, error) {
	l, ok := d.executors[executor].(extension.LiveExecutor)
	if !ok {
		return nil, management.ErrLiveUnsupported
	}
	out, err := l.Live(ctx, execution, tail)
	if errors.Is(err, extension.ErrUnsupported) {
		return nil, management.ErrLiveUnsupported
	}
	return out, err
}

func rank(status string) int {
	switch status {
	case runtime.CheckFail:
		return 3
	case runtime.CheckWarn:
		return 2
	case runtime.CheckInfo:
		return 1
	}
	return 0
}

func (d *daemon) reportedCode(kind, subject string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reported[kind+"\x00"+subject]
}

// daemonChecks are the doctor checks only a running daemon makes.
var daemonChecks = []string{"storage", "executions", "lease_recovery", "resource_holds", "retention", "routing", "reconcile_lag", "bindings", "connections", "secrets", "pinned_executors", "telemetry", "github", "linear", "jira", "bitbucket", "slack", "posthog", "webhooks", "schedule", "resources", "introspection", "extensions"}

// OfflineChecks are the doctor checks that need no daemon: the config file
// validated against the same registrations `serve` uses, the first-party
// extensions' Offline checks on it (github-capability, github-git-bot,
// claude-code), then a skipped check naming the daemon's own. The CLI runs them
// when no daemon is reachable.
func OfflineChecks(ctx context.Context, configPath string, data []byte) []runtime.Check {
	skipped := runtime.Check{Name: "skipped", Status: runtime.CheckInfo, Code: "daemon_not_running", Detail: "need a running daemon: " + strings.Join(daemonChecks, ", ")}
	executors := offlineExecutors()
	reg := registry(executors)
	pkgs := offlineExternal(&reg, configPath, "")
	snapshot, err := config.ValidateFile(data, reg, fileIncludes(configPath))
	defer pkgs.close()
	var invalid *config.ValidationError
	switch {
	case errors.As(err, &invalid):
		diags := append(invalid.Diagnostics, pkgs.diagnostics()...)
		check := runtime.Check{Name: "config", Status: runtime.CheckFail, Code: "invalid", Detail: fmt.Sprintf("%s has %d problem(s); see open-conduit config check", configPath, len(diags))}
		for _, d := range diags {
			check.Items = append(check.Items, fmt.Sprintf("%s: %s: %s", d.Path, d.Code, d.Message))
		}
		return []runtime.Check{check, skipped}
	case err != nil:
		return []runtime.Check{{Name: "config", Status: runtime.CheckFail, Code: "unvalidated", Detail: "config could not be validated: " + err.Error()}, skipped}
	}
	out := []runtime.Check{{Name: "config", Status: runtime.CheckOK, Code: "valid", Detail: configPath}, grantsCheck(snapshot, reg)}
	out = append(out, offlineChecks(ctx, snapshot, executors, filepath.Dir(configPath), pkgs.installed)...)
	return append(out, skipped)
}
