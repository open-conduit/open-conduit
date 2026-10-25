package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/domain"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/storage"
)

// A manual claim (an operator core.manual Event keyed to the entry's issue)
// creates Work whatever the labels; nothing else can claim this way.
func TestRouteManualClaim(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`)
	claim := event(gh.TypeClaimRequested, "issue/7", "github.claim.v1", map[string]int{"number": 7})
	claim.Event.Source = gh.ManualSource
	if d := match(t, scope, claim, nil); !d.Match || d.Kind != gh.WorkKind {
		t.Fatalf("manual claim: %+v", d)
	}
	if d := match(t, scope, claim, &domain.Work{ID: "w", Kind: gh.WorkKind}); d.Match {
		t.Fatal("a claim fed existing Work")
	}
	forged := event(gh.TypeClaimRequested, "issue/7", "github.claim.v1", nil)
	forged.Event.Source = "execution/x"
	if match(t, scope, forged, nil).Match {
		t.Fatal("a non-operator claim matched")
	}
	gen := event(gh.TypeClaimRequested, "issue/7/generation/reopen-1", "github.claim.v1", nil)
	gen.Event.Source = gh.ManualSource
	if match(t, scope, gen, nil).Match {
		t.Fatal("a generation key was claimed by hand")
	}
	other := event(gh.TypeClaimRequested, "issue/7", "github.claim.v1", nil)
	other.Event.Source = gh.ManualSource
	other.Event.CorrelationKey.Source = "github/github.com/other/repo"
	if match(t, scope, other, nil).Match {
		t.Fatal("a claim for another entry matched")
	}
	text, ok := gh.RenderEvent(claim.Event)
	if !ok || !strings.Contains(text, "#7") || !strings.Contains(text, gh.OpContext) {
		t.Fatalf("render: %q", text)
	}
}

func TestAnnotate(t *testing.T) {
	store := notesStore{rows: []storage.Entry{{Key: "problem/x", Value: []byte(`{"kind":"problem","work":"w1"}`)}}}
	s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","projection":{"dry_run":true}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	w := domain.Work{ID: "w1", Kind: gh.WorkKindHeld, State: domain.WorkWaiting, CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: "issue/12"}}}
	lines, ok := gh.Annotate(context.Background(), store, s, "https://github.com", w)
	got := map[string]string{}
	for _, l := range lines {
		got[l.Name] = l.Value
	}
	if !ok || got["issue"] != repo+"#12" || got["url"] != "https://github.com/"+repo+"/issues/12" || got["held"] == "" || got["projection"] == "" || got["problems"] != "1" {
		t.Fatalf("annotations: %v %v", got, ok)
	}
	if _, ok := gh.Annotate(context.Background(), store, s, "https://github.com", domain.Work{ID: "x", CorrelationKeys: []domain.CorrelationKey{{Source: "other", ExternalID: "issue/1"}}}); ok {
		t.Fatal("annotated Work of another entry")
	}
}

// notesStore serves only List over the notes namespace.
type notesStore struct{ rows []storage.Entry }
type notesTx struct {
	storage.Tx
	rows []storage.Entry
}

func (n notesStore) Transact(_ context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	return fn(notesTx{rows: n.rows})
}
func (n notesTx) List(namespace, _ string) ([]storage.Entry, error) {
	if namespace != gh.NotesNamespace {
		return nil, nil
	}
	return n.rows, nil
}
