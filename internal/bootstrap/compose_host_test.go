package bootstrap

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// ownFamilyIngress produces the families it is given.
type ownFamilyIngress struct{ families []string }

func (f ownFamilyIngress) Spec() extension.IngressSpec {
	var spec extension.IngressSpec
	for _, t := range f.families {
		spec.Families = append(spec.Families, extension.EventFamily{Type: t, Loss: "one-shot"})
	}
	return spec
}
func (ownFamilyIngress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (ownFamilyIngress) Reconcile(context.Context, extension.Scan) (extension.Snapshot, error) {
	return extension.Snapshot{Complete: true}, nil
}

// A package's ingress sources register only under its own names and only
// producing its own families; a foreign family is refused even under its
// own name. Only its timed Work kinds are held in a dry run.
func TestPackageSourcesStayOwn(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	ingress, err := runtime.NewIngress(store, now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	x := &installed{m: protocol.Manifest{Name: "acme", Provides: protocol.Provides{
		WorkKinds: []protocol.WorkKindDecl{{Kind: "acme.tick", Timed: true}, {Kind: "acme.item"}}}}}
	d := &daemon{coord: store, now: now, log: slog.New(slog.DiscardHandler), ingress: ingress, packages: []*installed{x}, nudge: make(chan struct{}, 1),
		reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	src := func(name, ns string, families ...string) ingressSource {
		return ingressSource{Name: name, Source: extension.Source{Namespace: ns, ScopeID: "eng"}, Ingress: ownFamilyIngress{families: families}}
	}
	d.registerSources(x, []ingressSource{
		src("acme/own", "acme-ns", "acme.hook"),
		src("acme/foreign", "acme-gh", "acme.hook", "github.delivery"),
		src("other/x", "other-ns", "acme.hook"),
		src("acme/none", "acme-none"),
	})
	for name, want := range map[string]bool{"acme/own": true, "acme/foreign": false, "other/x": false, "acme/none": false} {
		if got := d.external.sources[name]; got != want {
			t.Errorf("source %s registered %v, want %v", name, got, want)
		}
		if !want && d.reportedCode("extension_ingress", name) != "not_own" {
			t.Errorf("source %s refused without not_own", name)
		}
	}
	if timed := d.timedKinds(); !timed["acme.tick"] || timed["acme.item"] {
		t.Fatalf("timed kinds %v", timed)
	}
	if d.packageWebhook("other/hook") != nil || d.packageWebhook("acmehook") != nil {
		t.Fatal("a webhook outside the package's own prefix was routed to it")
	}
}

// Configure carries a package's entries in disabled scopes apart from its
// enabled ones, so it can keep what it holds for them.
func TestConfigureParamsCarryDisabledEntries(t *testing.T) {
	d := &daemon{}
	d.publishGeneration(&config.Snapshot{Order: []domain.ID{"on", "off"}, Scopes: map[domain.ID]*config.EffectiveScope{
		"on":  {ID: "on", Enabled: true, Extensions: []config.Extension{{Name: "a", Type: "acme"}, {Name: "g", Type: "github"}}},
		"off": {ID: "off", Extensions: []config.Extension{{Name: "b", Type: "acme"}, {Name: "g", Type: "github"}}},
	}}, 3)
	p := d.configureParams(map[string]bool{"acme": true}, nil)
	want := func(es []protocol.ConfigEntry, scope, name string) {
		t.Helper()
		if len(es) != 1 || es[0].ScopeID != scope || es[0].Name != name {
			t.Fatalf("entries %+v, want %s/%s", es, scope, name)
		}
	}
	want(p.Entries, "on", "a")
	want(p.Disabled, "off", "b")
}

// Only a pool a setting bounds is known: agent turns ("") and jobs.
// Discovery refuses a manifest naming another, and composition refuses its
// executor rather than run it unbounded.
func TestKnownPool(t *testing.T) {
	for pool, want := range map[string]bool{"": true, protocol.PoolJobs: true, "job": false, "turns": false} {
		if got := knownPool(pool); got != want {
			t.Errorf("knownPool(%q) = %v, want %v", pool, got, want)
		}
	}
}
