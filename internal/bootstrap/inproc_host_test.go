package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// hostProbe is an extension that keeps its host services and serves one
// webhook path.
type hostProbe struct{ h inproc.Host }

func (*hostProbe) Name() string                                   { return "acme" }
func (p *hostProbe) Start(_ context.Context, h inproc.Host) error { p.h = h; return nil }
func (*hostProbe) WorkKinds() []inproc.WorkKind {
	return []inproc.WorkKind{{Kind: "acme.tick", Timed: true}, {Kind: "acme.item"}}
}
func (*hostProbe) Webhook(path string) http.Handler {
	if path != "hook" {
		return nil
	}
	return http.NotFoundHandler()
}

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

// The host services #357 added stay within the extension's own: a derived
// Event only of its own families and only from a parent in the same scope,
// with the causal bound reported as inproc.ErrCausalLimit; deliveries only
// to its own sources; webhooks only under its own name; and only its timed
// kinds held in a dry run.
func TestInprocHostServicesStayOwn(t *testing.T) {
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
	probe := &hostProbe{}
	d := &daemon{coord: store, now: now, log: slog.New(slog.DiscardHandler), ingress: ingress, inproc: []inproc.Extension{probe}, nudge: make(chan struct{}, 1),
		reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	if err := d.startInproc(ctx); err != nil {
		t.Fatal(err)
	}
	root := func(scope domain.ID, id string) domain.Event {
		return domain.Event{ID: domain.ID(id), Type: "acme.seen", Source: "acme", DeliveryID: id, OccurredAt: now(), ReceivedAt: now()}
	}
	if err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		for _, s := range []domain.ID{"eng", "ops"} {
			if err := tx.PutScope(domain.Scope{ID: s, Name: string(s)}, 0); err != nil {
				return err
			}
		}
		if _, _, err := runtime.AcceptInTx(tx, "eng", root("eng", "p1")); err != nil {
			return err
		}
		_, _, err := runtime.AcceptInTx(tx, "ops", root("ops", "p2"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	derive := func(scope, parent domain.ID, typ, id string) error {
		return store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
			e := domain.Event{ID: domain.ID(id), Type: typ, Source: "acme", DeliveryID: id, OccurredAt: now(), ReceivedAt: now()}
			a, added, err := probe.h.AcceptDerivedInTx(tx, scope, parent, e)
			if err != nil {
				return err
			}
			if !added || a.Event.CausationID != parent {
				t.Errorf("derived %+v added %v", a.Event, added)
			}
			if s, err := probe.h.EventScope(tx, a); err != nil || s != scope {
				t.Errorf("event scope %q %v", s, err)
			}
			return nil
		})
	}
	if err := derive("eng", "p1", "acme.derived", "d1"); err != nil {
		t.Fatalf("own derived event: %v", err)
	}
	if err := derive("eng", "p1", "other.derived", "d2"); !errors.Is(err, inproc.ErrNotOwn) {
		t.Fatalf("another family: %v", err)
	}
	if err := derive("eng", "p2", "acme.derived", "d3"); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("a parent in another scope: %v", err)
	}
	if err := probe.h.Deliver(ctx, "other/x", extension.Delivery{}); !errors.Is(err, inproc.ErrNotOwn) {
		t.Fatalf("another's source: %v", err)
	}
	if err := probe.h.Deliver(ctx, "acmeish", extension.Delivery{}); !errors.Is(err, inproc.ErrNotOwn) {
		t.Fatalf("a source sharing only a prefix: %v", err)
	}
	if d.inprocWebhook("acme/hook") == nil || d.inprocWebhook("acme/other") != nil || d.inprocWebhook("acmehook") != nil || d.inprocWebhook("other/hook") != nil {
		t.Fatal("webhooks are not routed by the extension's own prefix")
	}
	// Ingress sources register only under the extension's own names and
	// only producing its own families; a foreign family is refused even
	// under its own name.
	src := func(name, ns string, families ...string) inproc.Source {
		return inproc.Source{Name: name, Source: extension.Source{Namespace: ns, ScopeID: "eng"}, Ingress: ownFamilyIngress{families: families}}
	}
	d.registerInprocSources(probe, []inproc.Source{
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
}

// Configure carries an extension's entries in disabled scopes apart from
// its enabled ones, so it can keep what it holds for them.
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

// The first-party extensions' executors join the registrations with the
// pool each declares: GitHub's hold executor counts against
// settings.job_concurrency.
func TestInprocExecutorsCarryPools(t *testing.T) {
	executors, pools := map[string]extension.Executor{}, map[string]string{}
	if refused := inprocExecutors(firstParty(), executors, pools); len(refused) != 0 {
		t.Fatalf("refused %v", refused)
	}
	if executors["github-hold"] == nil || pools["github-hold"] != inproc.PoolJobs {
		t.Fatalf("executors %v pools %v", executors, pools)
	}
	// A pool no setting bounds is refused rather than left unbounded.
	executors, pools = map[string]extension.Executor{}, map[string]string{}
	refused := inprocExecutors([]inproc.Extension{typoPool{}}, executors, pools)
	if len(refused) != 1 || refused[0] != "typo" || executors["typo"] != nil || pools["typo"] != "" {
		t.Fatalf("an unknown pool: refused %v executors %v pools %v", refused, executors, pools)
	}
}

// typoPool registers an executor in a pool no setting bounds.
type typoPool struct{}

func (typoPool) Name() string { return "typo" }
func (typoPool) Executors() map[string]inproc.Executor {
	return map[string]inproc.Executor{"typo": {Executor: &shellStub{}, Pool: "job"}}
}

// shellStub is an executor that is never run.
type shellStub struct{ extension.Executor }
