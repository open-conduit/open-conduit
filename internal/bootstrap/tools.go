package bootstrap

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// Engineering tools, waits and playbooks (03-04). The GitHub extension's
// toolbelt operations are registered on every generation's toolbelt through
// the public Registrar; held issues run on the in-process github-hold
// executor; agent turn outcomes are published as derived turn Events (the
// source of `issue:N:<state>` waits); and the playbooks are written once as
// the Claude Code plugin every claude-code binding uses by default.

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
// upgrade replaces old playbook text): every extension's playbooks,
// GitHub's, Slack's and the trackers' among them.
func (d *daemon) preparePlaybooks() error {
	books := map[string]string{}
	d.packagePlaybooks(books)
	return writePlugin(d.pluginDir(), "Playbooks for agents run by Open Conduit", books)
}
