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
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	slplaybooks "github.com/open-conduit/open-conduit/extensions/slack/playbooks"
	trackerplaybooks "github.com/open-conduit/open-conduit/extensions/tracker/playbooks"
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
		out = append(out, gh.Source{Settings: s.ingress.Settings(), Connection: s.connection, Client: s.client, Projector: s.projector})
	}
	return out
}

// openTools prepares the shared tools instance.
func (d *daemon) openTools() error {
	tools, err := gh.NewTools(gh.ToolsOptions{Store: d.coord, Sources: d.githubSourcesList, Now: d.now, Jobs: d.scheduleJob, WaitProgress: d.waitProgress(gh.Type), Writers: d.githubWriters(), MayRead: d.mayRead, MayFile: d.mayFile})
	if err != nil {
		return err
	}
	d.tools = tools
	if d.slackTools, err = sl.NewTools(sl.ToolsOptions{Store: d.coord, Sources: d.slackSourcesList, Now: d.now}); err != nil {
		return err
	}
	return d.openTrackerTools()
}

// registerTools adds the extension operations to a generation's toolbelt.
func (d *daemon) registerTools(belt *runtime.Toolbelt) error {
	if err := belt.Register(d.tools.Registration()); err != nil {
		return err
	}
	if err := belt.Register(d.slackTools.Registration()); err != nil {
		return err
	}
	return d.registerTrackerTools(belt)
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

// mayRead answers whether reader's scope may depend on target's events, as
// the runtime authorizes a cross-scope wait when it registers.
func (d *daemon) mayRead(reader, target domain.ID) error {
	return d.authorizeAlias(context.Background(), runtime.AliasAuthorization{SourceScopeID: target, TargetScopeID: reader})
}

// mayFile answers whether filer's scope may file issues in, and comment on,
// target's tracker (authorization.file_scopes).
func (d *daemon) mayFile(filer, target domain.ID) error {
	s, _ := d.configs.Current()
	if s == nil {
		return errors.New("no configuration")
	}
	return s.AuthorizeFile(filer, target)
}

// pluginDir is where the playbook plugin is written: claude-code loads it
// as a plugin, and every execution is told it (ExecutionContext.PlaybookDir).
func (d *daemon) pluginDir() string { return filepath.Join(d.o.StateDir, "playbooks", "plugin") }

// preparePlaybooks writes the plugin (rewritten at every start, so an
// upgrade replaces old playbook text): the GitHub playbooks, then the Slack
// support and tracker playbooks and the extensions' own beside them.
func (d *daemon) preparePlaybooks() error {
	if err := playbooks.Prepare(d.pluginDir()); err != nil {
		return err
	}
	books := slplaybooks.All()
	for name, text := range trackerplaybooks.All() {
		books[name] = text
	}
	d.inprocPlaybooks(books)
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
// extensions' own families, first-party and installed (event.render), and
// falls back to the executor's generic renderer for everything else.
func (d *daemon) renderEvents(events []domain.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		if text, ok := gh.RenderEvent(e); ok {
			parts = append(parts, text)
			continue
		}
		if text, ok := sl.RenderEvent(e); ok {
			parts = append(parts, text)
			continue
		}
		if text, ok := d.inprocRender(e); ok {
			parts = append(parts, text)
			continue
		}
		parts = append(parts, claudecode.RenderEvents([]domain.Event{e}))
	}
	return strings.Join(parts, "\n\n")
}
