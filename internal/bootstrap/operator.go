package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/playbooks"
	"github.com/open-conduit/open-conduit/extensions/jira"
	"github.com/open-conduit/open-conduit/extensions/linear"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/extensions/setup"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// Operator setup, doctor and controls (03-05) composed from public
// extension surfaces: offline validation against the daemon's own
// registrations, the agent setup session, Work annotations, attach, and the
// GitHub, capability and Claude Code doctor checks.

// registry is the daemon's registration of executors, route handlers,
// operations, connection and extension types and resource providers.
func registry(executors map[string]extension.Executor) config.Registry {
	reg := config.Registry{Executors: executors, Handlers: handlerNames(), Operations: append(append(gh.Operations(), sl.Operations()...), trackerOperations()...)}
	trackerRegistry(&reg)
	slackRegistry(&reg)
	scheduleRegistry(&reg)
	inprocRegistry(&reg, firstParty(nil))
	resourceRegistry(&reg)
	sandboxRegistry(&reg)
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
// extension packages installed beside it contribute their declared types
// (their opaque config is checked by the package once a daemon validates or
// applies it; an offline check never starts a process).
func ValidateConfigAt(configPath string, data []byte) ([]config.Diagnostic, error) {
	return validateWith(configPath, "", data)
}

// validateWith validates offline, leaving out the installed package named
// exclude (the removal check).
func validateWith(configPath, exclude string, data []byte) ([]config.Diagnostic, error) {
	reg := registry(offlineExecutors())
	offlineExternal(&reg, configPath, exclude)
	_, err := config.ValidateFile(data, reg, fileIncludes(configPath))
	var invalid *config.ValidationError
	if errors.As(err, &invalid) {
		return invalid.Diagnostics, nil
	}
	return nil, err
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
	inner := &shell.Executor{}
	out := map[string]extension.Executor{"shell": inner, ClaudeCodeExecutor: &claudecode.Executor{}, gh.HoldExecutor: gh.NewHoldExecutor()}
	if legacy, err := sc.NewLegacyExecutor(sc.LegacyOptions{Inner: inner}); err == nil {
		out[sc.LegacyExecutorName] = legacy
	}
	return out
}

// Setups are the registered extensions' optional setup flows (`open-conduit
// init <type>`). Core CLI hosts them through cli.Env.Setups and imports no
// extension.
func Setups() []extension.Setup {
	return []extension.Setup{gh.Setup{}, sl.Setup{}, linear.Setup{}, jira.Setup{}, setup.Setup{}}
}

// DefaultConfig is the configuration the daemon runs on when it starts with
// no config file and no stored generation: what `init setup` would write
// into an empty file, so the web view can start a setup session.
func DefaultConfig() []byte {
	r, err := setup.Setup{}.Setup(context.Background(), nil, nil)
	if err != nil {
		return nil
	}
	doc, err := cli.MergeSetup(nil, r, false)
	if err != nil {
		return nil
	}
	return doc
}

// SetupOptions start a coding agent with the setup guide.
type SetupOptions struct {
	// Agent is claude or claude-code.
	Agent      string
	ConfigPath string
	StateDir   string
	Cwd        string
	// Bin is how the agent runs open-conduit (its name when PATH finds this
	// executable, else the path).
	Bin string
	// Binary is the Claude Code CLI (default claude).
	Binary string
	// Environ is the process environment (default os.Environ).
	Environ []string
}

// SetupCommand prepares the setup playbook as its own one-skill plugin and
// returns the interactive claude command
// started on it (v1 `init --with`). Reading and changing Open Conduit's own
// config, and running doctor, go without asking; nothing else is pre-allowed.
func SetupCommand(o SetupOptions) (*exec.Cmd, error) {
	if o.Agent != "claude" && o.Agent != ClaudeCodeExecutor {
		return nil, fmt.Errorf("no setup agent %q: use --with claude", o.Agent)
	}
	if o.Binary == "" {
		o.Binary = "claude"
	}
	if o.Bin == "" {
		o.Bin = "open-conduit"
	}
	dir := filepath.Join(o.StateDir, "playbooks", "setup")
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := playbooks.WritePlugin(dir, "Sets Open Conduit up", map[string]string{setup.PlaybookName: setup.Playbook(o.Bin)}); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(o.Binary); err != nil {
		return nil, fmt.Errorf("%s is not on PATH: install Claude Code first", o.Binary)
	}
	// The prompt first and the tools last: --allowedTools takes every
	// argument after it.
	args := []string{"/" + playbooks.PluginName + ":" + setup.PlaybookName, "--plugin-dir", dir, "--allowedTools"}
	for _, sub := range []string{"config", "doctor", "health", "secret path", "init github --dry-run"} {
		args = append(args, "Bash("+o.Bin+" "+sub+":*)")
	}
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
	return cmd, nil
}

// annotate contributes the GitHub entries' context for Work they own.
func (d *daemon) annotate(ctx context.Context, w domain.Work) []management.Annotation {
	d.github.mu.Lock()
	sources := make([]*githubSource, 0, len(d.github.sources))
	for _, s := range d.github.sources {
		sources = append(sources, s)
	}
	d.github.mu.Unlock()
	out := d.capacityAnnotation(ctx, w)
	out = append(out, d.inprocAnnotations(ctx, w)...)
	for _, src := range sources {
		lines, ok := gh.Annotate(src.ingress.Settings(), src.client.Web(), w)
		if !ok {
			continue
		}
		for _, l := range lines {
			out = append(out, management.Annotation{Source: src.name, Name: l.Name, Value: l.Value})
		}
	}
	return out
}

// actions are the action providers on a scope: each GitHub entry the scope
// configures (by entry name, as its annotations) and each package it uses.
func (d *daemon) actions(_ context.Context, scope domain.ID) map[string]extension.ActionProvider {
	out := map[string]extension.ActionProvider{}
	d.github.mu.Lock()
	for _, src := range d.github.sources {
		if src.scope == scope {
			out[src.name] = gh.Actions{Store: d.coord, Settings: src.ingress.Settings()}
		}
	}
	d.github.mu.Unlock()
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
	return l.Live(ctx, execution, tail)
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

// capabilityCheck reports engineering configuration gaps the validator
// accepts but that leave GitHub work unable to proceed: no `github` route, an
// agent binding without the GitHub tools, scheduled issues with no hold
// route, and a hold route after the github route.
func (d *daemon) capabilityCheck(context.Context) runtime.Check {
	snapshot, _ := d.configs.Current()
	return capabilityCheckFor(snapshot, d.executors)
}

// capabilityCheckFor is capabilityCheck for a config snapshot and the
// executors it is checked against (offline doctor uses the registrations).
func capabilityCheckFor(snapshot *config.Snapshot, executors map[string]extension.Executor) runtime.Check {
	check := runtime.Check{Name: "github_capability", Status: runtime.CheckOK, Code: "ok"}
	if snapshot == nil {
		return check
	}
	warn := func(code, item string) {
		if check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckWarn, code
		}
		check.Items = append(check.Items, item)
	}
	info := func(item string) {
		check.Items = append(check.Items, item)
	}
	any := false
	for _, id := range snapshot.Order {
		scope := snapshot.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		owns := false
		for _, x := range scope.Extensions {
			if s, err := gh.ParseExtension(x.Config, x.SecretNames); x.Type == gh.Type && err == nil && s.Scope == string(id) {
				owns = true
			}
		}
		if !owns {
			continue
		}
		any = true
		bindings := map[string]config.Binding{}
		for _, b := range scope.Bindings {
			bindings[b.Name] = b
		}
		githubAt, holdAt := -1, -1
		for i, r := range scope.Routes {
			switch r.Handler {
			case gh.Type:
				if githubAt < 0 {
					githubAt = i
				}
				b := bindings[r.Binding]
				if c, ok := executors[b.Executor]; ok && c.Capabilities().Toolbelt == extension.ToolbeltNone {
					warn("no_toolbelt", fmt.Sprintf("%s: route %s binds %s, whose executor has no toolbelt to report status", id, r.Name, r.Binding))
				}
				missing := []string{}
				for _, op := range []string{gh.OpContext, "declare_outcome"} {
					found := false
					for _, have := range b.Operations {
						found = found || have == op
					}
					if !found {
						missing = append(missing, op)
					}
				}
				if len(missing) > 0 && b.Executor == ClaudeCodeExecutor {
					warn("agent_operations_missing", fmt.Sprintf("%s: binding %s lacks %s (the work-issue playbook needs them)", id, r.Binding, strings.Join(missing, ", ")))
				}
			case gh.HoldHandler:
				if holdAt < 0 {
					holdAt = i
				}
				b := bindings[r.Binding]
				if b.Executor != gh.HoldExecutor {
					warn("hold_binding", fmt.Sprintf("%s: route %s must bind a github-hold executor", id, r.Name))
				}
			}
		}
		switch {
		case githubAt < 0:
			warn("no_github_route", fmt.Sprintf("%s: no route uses handler github, so issues are never claimed", id))
		case holdAt < 0:
			info(fmt.Sprintf("%s: no github-hold route; issues labelled scheduled are not claimed", id))
		case holdAt > githubAt:
			warn("hold_route_order", fmt.Sprintf("%s: the github-hold route must come before the github route", id))
		default:
			info(fmt.Sprintf("%s: claims issues; scheduled issues are held", id))
		}
	}
	if !any {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
	}
	sort.Strings(check.Items)
	return check
}

// claudeCheck runs `claude --version` for every claude-code binding's
// binary and, when enabled, reads the subscription usage windows.
func (d *daemon) claudeCheck(ctx context.Context) runtime.Check {
	snapshot, _ := d.configs.Current()
	check := claudeVersionCheck(ctx, snapshot)
	if check.Code == "not_configured" {
		return check
	}
	if d.o.DryRun {
		check.Items = append(check.Items, "dry run: turns are stubs on a cheap model")
	}
	if !d.o.ClaudeUsage {
		return check
	}
	cx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	windows, err := claudecode.Usage(cx, claudecode.UsageOptions{URL: d.o.ClaudeUsageURL, Token: d.o.ClaudeUsageToken, HTTP: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		check.Items = append(check.Items, "usage unknown")
		return check
	}
	for _, w := range windows {
		item := fmt.Sprintf("usage %s: %.0f%% (resets %s)", w.Name, w.Used, w.ResetsAt.UTC().Format(time.RFC3339))
		if w.Used >= 90 && check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckWarn, "usage_high"
		}
		check.Items = append(check.Items, item)
	}
	return check
}

// claudeVersionCheck runs `claude --version` for every claude-code binding's
// binary in snapshot.
func claudeVersionCheck(ctx context.Context, snapshot *config.Snapshot) runtime.Check {
	check := runtime.Check{Name: "claude_code", Status: runtime.CheckOK, Code: "installed"}
	binaries := map[string][]string{}
	if snapshot != nil {
		for _, id := range snapshot.Order {
			scope := snapshot.Scopes[id]
			if scope == nil || !scope.Enabled {
				continue
			}
			for _, b := range scope.Bindings {
				if b.Executor != ClaudeCodeExecutor {
					continue
				}
				cfg, err := claudecode.ParseConfig(b.Config)
				if err != nil {
					continue
				}
				bin := cfg.Binary
				if bin == "" {
					bin = "claude"
				}
				binaries[bin] = append(binaries[bin], string(id)+"/"+b.Name)
			}
		}
	}
	if len(binaries) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no claude-code bindings"
		return check
	}
	names := make([]string, 0, len(binaries))
	for bin := range binaries {
		names = append(names, bin)
	}
	sort.Strings(names)
	for _, bin := range names {
		v, err := claudecode.Version(ctx, bin)
		if err != nil {
			check.Status, check.Code = runtime.CheckFail, "claude_missing"
			check.Items = append(check.Items, fmt.Sprintf("%s (bindings %s): not runnable", filepath.Base(bin), strings.Join(binaries[bin], ", ")))
			continue
		}
		check.Items = append(check.Items, fmt.Sprintf("%s: %s", filepath.Base(bin), v))
	}
	return check
}

// daemonChecks are the doctor checks only a running daemon makes.
var daemonChecks = []string{"storage", "executions", "lease_recovery", "resource_holds", "retention", "reconcile_lag", "bindings", "connections", "secrets", "pinned_executors", "telemetry", "github", "trackers", "slack", "posthog", "webhooks", "schedule", "resources", "introspection", "extensions"}

// OfflineChecks are the doctor checks that need no daemon: the config file
// validated against the same registrations `serve` uses, the GitHub
// capability gaps in it, and `claude --version` for its claude-code
// bindings, then a skipped check naming the daemon's own. The CLI runs them
// when no daemon is reachable.
func OfflineChecks(ctx context.Context, configPath string, data []byte) []runtime.Check {
	skipped := runtime.Check{Name: "skipped", Status: runtime.CheckInfo, Code: "daemon_not_running", Detail: "need a running daemon: " + strings.Join(daemonChecks, ", ")}
	executors := offlineExecutors()
	reg := registry(executors)
	offlineExternal(&reg, configPath, "")
	snapshot, err := config.ValidateFile(data, reg, fileIncludes(configPath))
	var invalid *config.ValidationError
	switch {
	case errors.As(err, &invalid):
		check := runtime.Check{Name: "config", Status: runtime.CheckFail, Code: "invalid", Detail: fmt.Sprintf("%s has %d problem(s); see open-conduit config check", configPath, len(invalid.Diagnostics))}
		for _, d := range invalid.Diagnostics {
			check.Items = append(check.Items, fmt.Sprintf("%s: %s: %s", d.Path, d.Code, d.Message))
		}
		return []runtime.Check{check, skipped}
	case err != nil:
		return []runtime.Check{{Name: "config", Status: runtime.CheckFail, Code: "unvalidated", Detail: "config could not be validated: " + err.Error()}, skipped}
	}
	return []runtime.Check{
		{Name: "config", Status: runtime.CheckOK, Code: "valid", Detail: configPath},
		capabilityCheckFor(snapshot, executors),
		claudeVersionCheck(ctx, snapshot),
		gitBotCheck(ctx, snapshot, filepath.Dir(configPath)),
		skipped,
	}
}
