package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/playbooks"
	hu "github.com/open-conduit/open-conduit/extensions/human"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	isplaybooks "github.com/open-conduit/open-conduit/extensions/introspection/playbooks"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	slplaybooks "github.com/open-conduit/open-conduit/extensions/slack/playbooks"
	trackerplaybooks "github.com/open-conduit/open-conduit/extensions/tracker/playbooks"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
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
	tools, err := gh.NewTools(gh.ToolsOptions{Store: d.coord, Sources: d.githubSourcesList, Now: d.now, Jobs: d.scheduleJob})
	if err != nil {
		return err
	}
	d.tools = tools
	if d.slackTools, err = sl.NewTools(sl.ToolsOptions{Store: d.coord, Sources: d.slackSourcesList, Now: d.now}); err != nil {
		return err
	}
	if d.reports, err = is.NewTools(is.ToolsOptions{Store: d.coord, Now: d.now}); err != nil {
		return err
	}
	if d.humanTools, err = hu.NewTools(hu.Options{Store: d.coord, Entry: d.humanEntry, Now: d.now}); err != nil {
		return err
	}
	return d.openTrackerTools()
}

// registerTools adds the extension operations to a generation's toolbelt.
// Issue reporting is registered only for a generation that enables it.
func (d *daemon) registerTools(belt *runtime.Toolbelt, s *config.Snapshot) error {
	if err := belt.Register(d.tools.Registration()); err != nil {
		return err
	}
	if err := belt.Register(d.slackTools.Registration()); err != nil {
		return err
	}
	if err := belt.Register(d.humanTools.Registration()); err != nil {
		return err
	}
	if err := d.registerTrackerTools(belt); err != nil {
		return err
	}
	if introspectionEnabled(s) {
		return belt.Register(d.reports.Registration())
	}
	return nil
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
// upgrade replaces old playbook text): the GitHub playbooks, then the Slack
// support and introspection (sweep, triage, fix) playbooks beside them.
func (d *daemon) preparePlaybooks() error {
	if err := playbooks.Prepare(d.pluginDir()); err != nil {
		return err
	}
	books := slplaybooks.All()
	for name, text := range isplaybooks.All() {
		books[name] = text
	}
	for name, text := range trackerplaybooks.All() {
		books[name] = text
	}
	for name, text := range books {
		dir := filepath.Join(d.pluginDir(), "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		tmp := filepath.Join(dir, ".SKILL.md.tmp")
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, "SKILL.md")); err != nil {
			return err
		}
	}
	return nil
}

// renderEvents renders GitHub and Slack Events with their reply hints, then
// installed packages' own families (event.render), and falls back to the
// executor's generic renderer for everything else.
func (d *daemon) renderEvents(events []domain.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		if text, ok := d.extensionRender(e); ok {
			parts = append(parts, text)
			continue
		}
		if text, ok := gh.RenderEvent(e); ok {
			parts = append(parts, text)
			continue
		}
		if text, ok := sl.RenderEvent(e); ok {
			parts = append(parts, text)
			continue
		}
		if text, ok := is.RenderEvent(e); ok {
			parts = append(parts, text)
			continue
		}
		if text, ok := hu.RenderEvent(e, hu.Lookup(context.Background(), d.coord, d.now)); ok {
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
