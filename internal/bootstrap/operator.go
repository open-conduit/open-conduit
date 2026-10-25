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

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/executors/claudecode"
	"github.com/alsiola/open-conduit/executors/shell"
	"github.com/alsiola/open-conduit/extension"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/extensions/github/playbooks"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/management"
	"github.com/alsiola/open-conduit/internal/runtime"
)

// Operator setup, doctor and controls (03-05) composed from public
// extension surfaces: offline validation against the daemon's own
// registrations, the agent setup session, Work annotations, attach, and the
// GitHub, capability and Claude Code doctor checks.

// registry is the daemon's registration of executors, route handlers,
// operations, connection and extension types and resource providers.
func registry(executors map[string]extension.Executor) config.Registry {
	reg := config.Registry{Executors: executors, Handlers: handlerNames(), Operations: gh.Operations()}
	githubRegistry(&reg)
	resourceRegistry(&reg)
	return reg
}

// ValidateConfig validates a config document offline against the same
// registrations `serve` uses, without a daemon, storage or secret
// resolution. It returns the diagnostics (empty when valid).
func ValidateConfig(data []byte) ([]config.Diagnostic, error) {
	// Capability and config checks need only the registrations: executors'
	// capabilities and config validators do not touch their runtime state.
	reg := registry(map[string]extension.Executor{"shell": &shell.Executor{}, ClaudeCodeExecutor: &claudecode.Executor{}, gh.HoldExecutor: gh.NewHoldExecutor()})
	_, err := config.Validate(data, reg)
	var invalid *config.ValidationError
	if errors.As(err, &invalid) {
		return invalid.Diagnostics, nil
	}
	return nil, err
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

// SetupCommand prepares the setup guide as its own one-skill plugin (kept
// apart from the turn playbooks) and returns the interactive claude command
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
	if err := playbooks.PrepareSetup(dir, o.Bin); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(o.Binary); err != nil {
		return nil, fmt.Errorf("%s is not on PATH: install Claude Code first", o.Binary)
	}
	// The prompt first and the tools last: --allowedTools takes every
	// argument after it.
	args := []string{"/" + playbooks.PluginName + ":setup", "--plugin-dir", dir, "--allowedTools"}
	for _, sub := range []string{"config", "doctor", "health", "secret path", "init --dry-run"} {
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
	var out []management.Annotation
	for _, src := range sources {
		lines, ok := gh.Annotate(ctx, d.coord, src.ingress.Settings(), src.client.Web(), w)
		if !ok {
			continue
		}
		for _, l := range lines {
			out = append(out, management.Annotation{Source: src.name, Name: l.Name, Value: l.Value})
		}
	}
	return out
}

// attach returns the attach command of the Work's executor registration.
func (d *daemon) attach(ctx context.Context, executor string, w domain.Work) (management.AttachSpec, error) {
	if executor != ClaudeCodeExecutor || d.claude.executor == nil {
		return management.AttachSpec{}, management.ErrAttachUnsupported
	}
	spec, err := d.claude.executor.Attach(ctx, w.ID)
	if errors.Is(err, claudecode.ErrNoSession) {
		return management.AttachSpec{}, management.ErrNoSession
	}
	if err != nil {
		return management.AttachSpec{}, err
	}
	return management.AttachSpec{Argv: spec.Argv, Dir: spec.Dir}, nil
}

// githubCheck reports registration, rate-limit, reconcile and projection
// state per GitHub entry. It makes no service calls; credentials are
// checked by githubAuthCheck under check_secrets.
func (d *daemon) githubCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "github", Status: runtime.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	d.github.mu.Lock()
	failed := map[string]string{}
	for ns, code := range d.github.failed {
		failed[ns] = code
	}
	sources := map[string]*githubSource{}
	for ns, s := range d.github.sources {
		sources[ns] = s
	}
	d.github.mu.Unlock()
	if len(sources) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no GitHub entries are configured"
		return check
	}
	for ns, code := range failed {
		raise(runtime.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	scans := map[string]runtime.IngressStatus{}
	if statuses, err := d.ingress.Inspect(ctx); err == nil {
		for _, s := range statuses {
			scans[s.Name] = s
		}
	}
	for ns, src := range sources {
		item := ns + ": "
		var notes []string
		if until, paused := src.client.PausedUntil(); paused {
			raise(runtime.CheckWarn, "rate_limited")
			notes = append(notes, "rate limited until "+until.UTC().Format(time.RFC3339))
		}
		if s, ok := scans[src.name]; ok {
			switch {
			case s.LastError != "":
				raise(runtime.CheckWarn, "reconcile_failing")
				notes = append(notes, fmt.Sprintf("reconcile failing (%s, %d failures)", s.LastError, s.Failures))
			case s.LastComplete.IsZero():
				notes = append(notes, "first reconcile pending")
			default:
				notes = append(notes, "reconciled "+d.now().Sub(s.LastComplete).Truncate(time.Second).String()+" ago")
			}
		}
		if code := d.reportedCode("github_projection", src.name); code != "" {
			raise(runtime.CheckWarn, "projection_failing")
			notes = append(notes, "projection "+code)
		}
		if d.o.DryRun || src.ingress.Settings().Projection.DryRun {
			notes = append(notes, "dry run (no GitHub writes)")
		}
		check.Items = append(check.Items, item+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
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

// githubAuthCheck (check_secrets only) resolves each entry's credentials
// privately and reads its repository: the connection test. It reports codes
// and permissions, never the reference or the token.
func (d *daemon) githubAuthCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "github_auth", Status: runtime.CheckOK, Code: "verified"}
	d.github.mu.Lock()
	sources := make([]*githubSource, 0, len(d.github.sources))
	for _, s := range d.github.sources {
		sources = append(sources, s)
	}
	d.github.mu.Unlock()
	if len(sources) == 0 {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
		return check
	}
	for _, src := range sources {
		s := src.ingress.Settings()
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		a, err := src.client.Verify(cx, s.TrackerRepo)
		cancel()
		item := s.Namespace + ": "
		if err != nil {
			code := hookCode(err)
			switch {
			case code == "hook_permission_denied":
				code = "repo_unreachable"
			case code == "hook_update_failed":
				code = "verify_failed"
			}
			if rank(runtime.CheckFail) > rank(check.Status) {
				check.Status, check.Code = runtime.CheckFail, code
			}
			check.Items = append(check.Items, item+code)
			continue
		}
		var notes []string
		if a.Known {
			notes = append(notes, fmt.Sprintf("pull=%t push=%t admin=%t", a.Pull, a.Push, a.Admin))
			if !a.Push && !a.Writer && !d.o.DryRun && !s.Projection.DryRun {
				if check.Status == runtime.CheckOK {
					check.Status, check.Code = runtime.CheckWarn, "read_only"
				}
				notes = append(notes, "labels, comments and PRs need write access")
			}
			if s.ManageWebhook && !a.Admin {
				if check.Status == runtime.CheckOK {
					check.Status, check.Code = runtime.CheckWarn, "hook_admin_missing"
				}
				notes = append(notes, "manage_webhook needs admin (hooks) permission")
			}
		} else {
			notes = append(notes, "authenticated (installation permissions are not reported)")
		}
		if a.Writer {
			notes = append(notes, "bot writer authenticated")
		}
		check.Items = append(check.Items, item+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// capabilityCheck reports engineering configuration gaps the validator
// accepts but that leave GitHub work unable to proceed: no `github` route, an
// agent binding without the GitHub tools, scheduled issues with no hold
// route, and a hold route after the github route.
func (d *daemon) capabilityCheck(context.Context) runtime.Check {
	check := runtime.Check{Name: "github_capability", Status: runtime.CheckOK, Code: "ok"}
	snapshot, _ := d.configs.Current()
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
				if c, ok := d.executors[b.Executor]; ok && c.Capabilities().Toolbelt == extension.ToolbeltNone {
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
	check := runtime.Check{Name: "claude_code", Status: runtime.CheckOK, Code: "installed"}
	snapshot, _ := d.configs.Current()
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
