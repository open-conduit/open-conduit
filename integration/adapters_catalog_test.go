package integration_test

import (
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// The adapters catalogue is built from the release's bundled packages'
// manifests (#494): every config type each declares is listed, the
// first-party ones describe their fields themselves, and their operations
// come from their manifests.
func TestAdaptersCatalogueListsBundledTypes(t *testing.T) {
	releaseLayout(t)
	catalog, refused := bootstrap.Adapters("")
	if len(refused) != 0 {
		t.Fatalf("the release refuses bundled packages: %v", refused)
	}
	find := func(typ string) *cli.Adapter {
		for i := range catalog {
			if catalog[i].Type == typ {
				return &catalog[i]
			}
		}
		return nil
	}
	listed := func(pkg, kind, typ string) bool {
		if a := find(typ); a != nil && slices.Contains(a.Kinds, kind) {
			return true
		}
		a := find(pkg)
		return a != nil && (typ == pkg && slices.Contains(a.Kinds, kind) || slices.Contains(a.Kinds, kind+":"+typ))
	}
	for _, m := range bootstrap.BundledManifests() {
		p := m.Provides
		check := func(kind, typ string) {
			if !listed(m.Name, kind, typ) {
				t.Errorf("bundled %s's %s %s is not in the catalogue", m.Name, kind, typ)
			}
		}
		for _, c := range p.Connections {
			check("connection", c.Type)
		}
		for _, x := range p.Extensions {
			check("extension", x.Type)
		}
		for _, r := range p.Routes {
			check("route", r.Handler)
		}
		for _, x := range p.Executors {
			check("executor", x.Name)
		}
		for _, r := range p.Resources {
			check("resource", r.Type)
		}
		for _, s := range p.Sandboxes {
			check("sandbox", s.Type)
		}
	}
	// The first-party packages describe their own fields, which the core
	// no longer writes for them.
	for typ, fields := range map[string]func(a *cli.Adapter) []cli.Field{
		"github":      func(a *cli.Adapter) []cli.Field { return a.Entry },
		"linear":      func(a *cli.Adapter) []cli.Field { return a.Route },
		"jira":        func(a *cli.Adapter) []cli.Field { return a.Secrets },
		"slack":       func(a *cli.Adapter) []cli.Field { return a.Connection },
		"claude-code": func(a *cli.Adapter) []cli.Field { return a.Binding },
		"codex":       func(a *cli.Adapter) []cli.Field { return a.Binding },
		"git":         func(a *cli.Adapter) []cli.Field { return a.Resource },
	} {
		a := find(typ)
		if a == nil || len(fields(a)) == 0 {
			t.Errorf("%s describes no fields: %+v", typ, a)
		}
	}
	if a := find("github"); a == nil || !slices.Contains(a.Operations, "github.issue.comment") || a.Setup != "github" {
		t.Errorf("github entry: %+v", a)
	}
}
