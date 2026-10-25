package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/executors/claudecode"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/extensions/github/playbooks"
	"github.com/alsiola/open-conduit/internal/runtime"
)

// Engineering tools, waits and playbooks (03-04). The GitHub extension's
// toolbelt operations are registered on every generation's toolbelt through
// the public Registrar; held issues run on the in-process github-hold
// executor; agent turn outcomes are published as derived turn Events (the
// source of `issue:N:<state>` waits); and the playbooks are written once as
// the Claude Code plugin every claude-code binding uses by default.

// githubSourcesList is the tools' view of the registered sources.
func (d *daemon) githubSourcesList() []gh.Source {
	d.github.mu.Lock()
	defer d.github.mu.Unlock()
	out := make([]gh.Source, 0, len(d.github.sources))
	for _, s := range d.github.sources {
		out = append(out, gh.Source{Settings: s.ingress.Settings(), Client: s.client, Projector: s.projector})
	}
	return out
}

// openTools prepares the shared tools instance.
func (d *daemon) openTools() error {
	tools, err := gh.NewTools(gh.ToolsOptions{Store: d.coord, Sources: d.githubSourcesList, Now: d.now})
	if err != nil {
		return err
	}
	d.tools = tools
	return nil
}

// registerTools adds the extension operations to a generation's toolbelt.
func (d *daemon) registerTools(belt *runtime.Toolbelt) error {
	return belt.Register(d.tools.Registration())
}

// authorizeAlias is the B04 cross-scope dependency ceiling: the waiting
// (target) scope must list the source scope in authorization.read_scopes.
func (d *daemon) authorizeAlias(_ context.Context, a runtime.AliasAuthorization) error {
	s, _ := d.configs.Current()
	if s == nil {
		return errors.New("no configuration")
	}
	return s.AuthorizeRead(a.TargetScopeID, a.SourceScopeID)
}

// pluginDir is where the playbook plugin is written for claude-code.
func (d *daemon) pluginDir() string { return filepath.Join(d.o.StateDir, "playbooks", "plugin") }

// preparePlaybooks writes the plugin (rewritten at every start, so an
// upgrade replaces old playbook text).
func (d *daemon) preparePlaybooks() error { return playbooks.Prepare(d.pluginDir()) }

// renderEvents renders GitHub Events with their reply hints and falls back to
// the executor's generic renderer for everything else.
func renderEvents(events []domain.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		if text, ok := gh.RenderEvent(e); ok {
			parts = append(parts, text)
			continue
		}
		parts = append(parts, claudecode.RenderEvents([]domain.Event{e}))
	}
	return strings.Join(parts, "\n\n")
}

// publishTurns accepts the turn Events a source's projector derives from
// durable outcomes, as trusted derived Events of the attempt's trigger.
// Delivery identity makes a repeat after a crash a no-op.
func (d *daemon) publishTurns(ctx context.Context, src *githubSource) string {
	turns, err := src.projector.Turns(ctx, passLimit)
	if err != nil {
		return errorCode(err)
	}
	if len(turns) == 0 {
		return ""
	}
	s, _ := d.configs.Current()
	limits := runtime.CausalLimits{}
	if s != nil {
		limits = runtime.CausalLimits{MaxDepth: s.Settings.CausalMaxDepth, PerMinute: s.Settings.CausalPerMinute}
	}
	causal, err := runtime.NewCausal(d.coord, d.now, limits)
	if err != nil {
		return errorCode(err)
	}
	code := ""
	for _, t := range turns {
		if _, _, err := causal.Derive(ctx, t.Scope, t.Parent, t.Event); err != nil && !errors.Is(err, runtime.ErrCausalRejected) {
			code = errorCode(err)
			continue
		}
		if err := src.projector.MarkTurn(ctx, domain.ID(strings.TrimPrefix(t.Event.DeliveryID, "turn/"))); err != nil {
			code = errorCode(err)
		}
	}
	if code != "" {
		return code
	}
	d.poke()
	return ""
}
