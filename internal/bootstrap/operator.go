package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
)

// Operator setup, doctor and controls (03-05) composed from public
// extension surfaces: offline validation against the daemon's own
// registrations, the agent setup session, Work annotations, attach, and the
// Claude Code doctor check.

// registry is the daemon's registration of executors, route handlers,
// operations, connection and extension types and resource providers.
func registry(executors map[string]extension.Executor) config.Registry {
	reg := config.Registry{Executors: executors, Handlers: handlerNames(), HandlerConfig: map[string]func(json.RawMessage) error{
		"work": func(raw json.RawMessage) error { _, err := parseWorkRoute(raw); return err },
	}, HandlerWorkKinds: map[string]func(json.RawMessage) []extension.WorkKind{"work": workRouteKinds}}
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

// CheckConfigAt is ValidateConfigAt plus config.ModeWarnings, the
// executors' config.BindingWarnings, config.GrantWarnings and
// config.PackageWarnings from the same validation, so a config
// check starts each installed package once. A config that doesn't validate
// has no such warnings (its diagnostics say what is wrong).
func CheckConfigAt(configPath string, data []byte) ([]config.Diagnostic, []string, error) {
	return checkWith(configPath, "", data)
}

func checkWith(configPath, exclude string, data []byte) ([]config.Diagnostic, []string, error) {
	reg := registry(map[string]extension.Executor{})
	pkgs := offlineExternal(&reg, configPath, exclude, data)
	defer pkgs.close()
	s, err := config.ValidateFile(data, reg, fileIncludes(configPath))
	if err == nil {
		warnings := append(append(config.ModeWarnings(s, reg), config.BindingWarnings(s, reg)...), config.GrantWarnings(s, reg)...)
		return nil, append(warnings, config.PackageWarnings(s, reg)...), nil
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
	// Agent is the executor `--with` names: its name or one of its launch
	// names, among the installed executors that offer an interactive launch.
	Agent      string
	ConfigPath string
	StateDir   string
	Cwd        string
	// Bin is how the agent runs open-conduit (its name when PATH finds this
	// executable, else the path).
	Bin string
	// Binary is the agent's CLI, "" for the executor's default.
	Binary string
	// Environ is the process environment (default os.Environ).
	Environ []string
}

// SetupLaunch is the interactive setup session to run: the command, and
// the executor and title of the package that launches it.
type SetupLaunch struct {
	Cmd      *exec.Cmd
	Executor string
	Title    string
}

// SetupCommand returns the interactive command `init --with` runs: the
// setup playbook, started by the executor --with names through its
// interactive launch (extension.InteractiveLauncher). The host decides what
// the session may do without asking (reading and changing Open Conduit's
// own config, doctor, health, the secret store's path and a dry run of each
// setup), its directory and its environment (no toolbelt credential, the
// config it sets up named); the executor only says how its CLI expresses
// them, never sees the operator's environment, and may only add variables
// the host does not decide.
func SetupCommand(o SetupOptions) (SetupLaunch, error) {
	if o.Bin == "" {
		o.Bin = "open-conduit"
	}
	f, decl, err := launcherFor(o.ConfigPath, o.Agent)
	if err != nil {
		return SetupLaunch{}, err
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return SetupLaunch{}, err
	}
	name, text, err := firstRunPlaybook(o.ConfigPath, o.Bin)
	if err != nil {
		return SetupLaunch{}, err
	}
	allowed := []string{"config", "doctor", "health", "secret path"}
	for _, t := range dryRunSetups(o.ConfigPath, o.StateDir) {
		allowed = append(allowed, "init "+t+" --dry-run")
	}
	for i, sub := range allowed {
		allowed[i] = o.Bin + " " + sub
	}
	req := extension.InteractiveLaunchRequest{Playbook: text, Name: name, Summary: "Sets Open Conduit up", Allow: allowed,
		Dir: o.Cwd, FilesDir: filepath.Join(o.StateDir, "playbooks", name), Program: o.Binary}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec, err := interactiveLaunch(ctx, f, decl.Name, o.ConfigPath, o.StateDir, req)
	if err != nil {
		return SetupLaunch{}, fmt.Errorf("the %s executor's launch: %w", decl.Name, err)
	}
	title := f.Manifest.Title
	if title == "" {
		title = decl.Name
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return SetupLaunch{}, fmt.Errorf("the %s executor's launch has no command", decl.Name)
	}
	if _, err := exec.LookPath(spec.Argv[0]); err != nil {
		return SetupLaunch{}, fmt.Errorf("%s is not on PATH: install %s first", spec.Argv[0], title)
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = o.Cwd
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if cmd.Env, err = launchEnv(o.Environ, o.ConfigPath, spec.SetEnv); err != nil {
		return SetupLaunch{}, fmt.Errorf("the %s executor's launch: %w", decl.Name, err)
	}
	return SetupLaunch{Cmd: cmd, Executor: decl.Name, Title: title}, nil
}

// launcherFor is the installed or bundled executor that offers an
// interactive launch under name: its own name first, then a launch name
// only one package claims. The error lists the executors that offer one.
func launcherFor(configPath, name string) (extproc.Found, protocol.ExecutorDecl, error) {
	found, _ := discoverPackages(configPath, "")
	if err := extproc.CheckConflicts(found, hostTaken()); err != nil {
		return extproc.Found{}, protocol.ExecutorDecl{}, err
	}
	type launcher struct {
		f    extproc.Found
		decl protocol.ExecutorDecl
	}
	var all, aliased []launcher
	for _, f := range found {
		for _, x := range f.Manifest.Provides.Executors {
			if !x.Controls.InteractiveLaunch {
				continue
			}
			if x.Name == name {
				return f, x, nil
			}
			all = append(all, launcher{f, x})
			if slices.Contains(x.LaunchNames, name) {
				aliased = append(aliased, launcher{f, x})
			}
		}
	}
	if len(aliased) == 1 {
		return aliased[0].f, aliased[0].decl, nil
	}
	if len(aliased) > 1 {
		names := make([]string, 0, len(aliased))
		for _, l := range aliased {
			names = append(names, l.decl.Name)
		}
		return extproc.Found{}, protocol.ExecutorDecl{}, fmt.Errorf("--with %s names more than one executor (%s): name one of them", name, strings.Join(names, ", "))
	}
	var offered []string
	for _, l := range all {
		label := l.decl.Name
		if len(l.decl.LaunchNames) > 0 {
			label += " (or " + strings.Join(l.decl.LaunchNames, ", ") + ")"
		}
		offered = append(offered, label)
	}
	if len(offered) == 0 {
		return extproc.Found{}, protocol.ExecutorDecl{}, fmt.Errorf("no setup agent %q: no installed executor offers an interactive launch", name)
	}
	return extproc.Found{}, protocol.ExecutorDecl{}, fmt.Errorf("no setup agent %q: --with takes an executor that offers an interactive launch: %s", name, strings.Join(offered, ", "))
}

// interactiveLaunch starts the executor's package for the one call, with
// the grant the config gives it, as a setup flow does.
func interactiveLaunch(ctx context.Context, f extproc.Found, executor, configPath, stateDir string, req extension.InteractiveLaunchRequest) (extension.InteractiveLaunchSpec, error) {
	paths := extproc.StatePaths(stateDir, f.Manifest.Name)
	var passEnv []string
	if configPath != "" {
		if data, err := config.ReadFile(configPath); err == nil {
			passEnv = config.FileGrants(data)[f.Manifest.Name]
		}
	}
	r, err := extproc.Load(ctx, f, extproc.Deps{Now: time.Now}, extproc.Options{StateDir: paths.State, LogDir: filepath.Dir(paths.Log), Host: protocol.HostInfo{Name: "open-conduit"},
		PassEnv: func() []string { return passEnv }})
	if err != nil {
		return extension.InteractiveLaunchSpec{}, diagnosed(f.Manifest.Name, err)
	}
	defer r.Process().Close()
	e, ok := r.Executor(executor)
	if !ok {
		return extension.InteractiveLaunchSpec{}, extension.ErrUnsupported
	}
	l, ok := e.(extension.InteractiveLauncher)
	if !ok {
		return extension.InteractiveLaunchSpec{}, extension.ErrUnsupported
	}
	return l.InteractiveLaunch(ctx, req)
}

// setupEnv is the setup session's environment: env with no toolbelt
// credential and the config it sets up named.
func setupEnv(env []string, configPath string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "OS_TOKEN" || name == "OS_ENDPOINT" || name == "OS_TOKEN_FILE" || name == "OPEN_CONDUIT_CONFIG" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "OPEN_CONDUIT_CONFIG="+configPath)
}

// launchEnv is the session's environment (setupEnv) with the variables the
// executor adds, refusing one only the host decides.
func launchEnv(environ []string, configPath string, add map[string]string) ([]string, error) {
	env := setupEnv(environ, configPath)
	names := make([]string, 0, len(add))
	for k := range add {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if k == "" || strings.ContainsAny(k, "=\x00") || hostSetEnv(k) {
			return nil, fmt.Errorf("it may not set %q: the host decides it", k)
		}
		env = append(env, k+"="+add[k])
	}
	return env, nil
}

// hostSetEnv reports the variables a launch may not set: the toolbelt's
// namespace (the session has no toolbelt credential) and the config path.
func hostSetEnv(name string) bool {
	return strings.HasPrefix(name, "OS_") || name == "OPEN_CONDUIT_CONFIG"
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

// firstRunPlaybook is the setup playbook `init --with` starts on:
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

// annotate contributes the host's own context for Work: its capacity
// wait. The extensions' come from annotators.
func (d *daemon) annotate(ctx context.Context, w domain.Work) []management.Annotation {
	return d.capacityAnnotation(ctx, w)
}

// actions are the action providers on a scope, every extension's.
func (d *daemon) actions(_ context.Context, scope domain.ID) map[string]extension.ActionProvider {
	out := map[string]extension.ActionProvider{}
	d.packageActions(scope, out)
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
	executors := map[string]extension.Executor{}
	reg := registry(executors)
	pkgs := offlineExternal(&reg, configPath, "", data)
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
