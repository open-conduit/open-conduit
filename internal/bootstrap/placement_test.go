package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
)

// The Work's last placement is kept beside its home, per provider: a
// launch under another provider, or with nothing kept, prefers none.
func TestWorkPlacement(t *testing.T) {
	home := filepath.Join(t.TempDir(), "homes", "k")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := workPlacement(home, "ssh"); got != "" {
		t.Fatalf("prefer %q with nothing kept, want none", got)
	}
	placeWork(home, "ssh", extension.Placement{ID: "agent@runner:22", Daemon: "d"})
	if got := workPlacement(home, "ssh"); got != "agent@runner:22" {
		t.Fatalf("prefer %q, want agent@runner:22", got)
	}
	if got := workPlacement(home, "pool"); got != "" {
		t.Fatalf("prefer %q under another provider, want none", got)
	}
	if fi, err := os.Stat(home + ".site"); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("site file %v %v, want owner-only", fi, err)
	}
}

type sitePlacer struct{ extension.ContainerProvider }

func (sitePlacer) Place(context.Context, extension.ResourceOwner, extension.ContainerPolicy, string) (extension.Placement, error) {
	return extension.Placement{}, nil
}
func (sitePlacer) SiteRun(context.Context, string, extension.SiteCommand) (extension.SiteResult, error) {
	return extension.SiteResult{}, nil
}

// The host resolves a site for a resource provider only when a binding of
// an enabled scope pairs its type with the site's container provider, and
// that provider places launches.
func TestSiteFor(t *testing.T) {
	snap := &config.Snapshot{Scopes: map[domain.ID]*config.EffectiveScope{
		"eng": {Enabled: true, Bindings: []config.Binding{{Resource: &config.Resource{Provider: "git"}, Container: &config.Container{Provider: "ssh"}}, {Resource: &config.Resource{Provider: "git"}, Container: &config.Container{Provider: "docker"}}}},
		"off": {Enabled: false, Bindings: []config.Binding{{Resource: &config.Resource{Provider: "scratch"}, Container: &config.Container{Provider: "ssh"}}}},
	}}
	providers := map[string]extension.ContainerProvider{"ssh": sitePlacer{}, "docker": nil}
	site, err := siteFor(snap, providers, "git", "ssh/oc@runner:22")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := site.(extension.PlacedSite); !ok || site.ID() != "ssh/oc@runner:22" || p.At.ID != "oc@runner:22" || p.Toolbelt != "" || p.Env != nil {
		t.Fatalf("site %+v, want the placement with no toolbelt", site)
	}
	for _, c := range []struct{ typ, id string }{
		{"git", "docker/x"},   // paired, but doesn't place
		{"git", "pool/x"},     // no binding pairs them
		{"scratch", "ssh/x"},  // paired only in a disabled scope
		{"other", "ssh/x"},    // another resource type
		{"git", "not-a-site"}, // not a site ID
	} {
		if _, err := siteFor(snap, providers, c.typ, c.id); !errors.Is(err, extension.ErrSiteUnavailable) {
			t.Errorf("%s at %s: %v, want site_unavailable", c.typ, c.id, err)
		}
	}
}
