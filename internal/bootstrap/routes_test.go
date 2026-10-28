package bootstrap

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func TestManualRouteSamples(t *testing.T) {
	samples := manualRoute{}.Samples(context.Background(), extension.SampleInput{Scope: domain.Scope{}})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	d, err := manualRoute{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: samples[0].Event}})
	if err != nil || !d.Match || d.Kind != "manual.work" {
		t.Fatalf("the keyed sample must create manual work: %+v, %v", d, err)
	}
}

type familyIngress struct{ families []string }

func (f familyIngress) Spec() extension.IngressSpec {
	var spec extension.IngressSpec
	for _, t := range f.families {
		spec.Families = append(spec.Families, extension.EventFamily{Type: t, ReDerivable: true})
	}
	return spec
}
func (familyIngress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (familyIngress) Reconcile(context.Context, extension.Scan) (extension.Snapshot, error) {
	return extension.Snapshot{}, nil
}

// A trace refuses what acceptance would from the Event's source: any type
// from an operator; a registered source in its own scope; else a type an
// in-process extension owns; nothing from a source nobody knows.
func TestAdmitTrace(t *testing.T) {
	ctx := context.Background()
	clock := func() time.Time { return time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC) }
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.Transact(ctx, storage.Guard{Now: clock()}, func(tx storage.Tx) error {
		return tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	ingress, err := runtime.NewIngress(store, clock, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = ingress.Register(ctx, "chat/eng", extension.Source{Namespace: "chat/eng", ScopeID: "eng"}, familyIngress{[]string{"chat.message"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	d := &daemon{ingress: ingress, inproc: []inproc.Extension{introspection.New()}}
	for name, c := range map[string]struct {
		scope domain.ID
		e     domain.Event
		ok    bool
	}{
		"operator":            {"eng", domain.Event{Type: "anything.at.all", Source: runtime.ManualSource}, true},
		"source family":       {"eng", domain.Event{Type: "chat.message", Source: "chat/eng"}, true},
		"derived type":        {"eng", domain.Event{Type: "chat.reminder", Source: "chat/eng"}, true},
		"other scope":         {"ops", domain.Event{Type: "chat.message", Source: "chat/eng"}, false},
		"extension family":    {"eng", domain.Event{Type: introspection.TypeReported, Source: introspection.SourceNamespace("eng")}, true},
		"unknown source":      {"eng", domain.Event{Type: "nobody.thing", Source: "nobody/x"}, false},
		"unknown source type": {"eng", domain.Event{Type: "chat.message", Source: "chat/ops"}, false},
	} {
		if err := d.admitTrace(c.scope, c.e); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}
