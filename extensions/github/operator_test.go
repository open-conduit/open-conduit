package github_test

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

// A manual claim (the claim action's Event keyed to the entry's issue)
// creates Work whatever the labels; nothing else can claim this way, not
// even the same Event posted by the operator through POST /v1/events.
func TestRouteManualClaim(t *testing.T) {
	scope := scopeWith(t, `{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`)
	claim := event(gh.TypeClaimRequested, "issue/7", "github.claim.v1", map[string]int{"number": 7})
	claim.Event.Source = extension.ActionSource
	if d := match(t, scope, claim, nil); !d.Match || d.Kind != gh.WorkKind {
		t.Fatalf("manual claim: %+v", d)
	}
	if d := match(t, scope, claim, &domain.Work{ID: "w", Kind: gh.WorkKind}); d.Match {
		t.Fatal("a claim fed existing Work")
	}
	forged := event(gh.TypeClaimRequested, "issue/7", "github.claim.v1", nil)
	for _, source := range []string{"execution/x", "core.manual"} {
		forged.Event.Source = source
		if match(t, scope, forged, nil).Match {
			t.Fatalf("a claim from %s matched", source)
		}
	}
	gen := event(gh.TypeClaimRequested, "issue/7/generation/reopen-1", "github.claim.v1", nil)
	gen.Event.Source = extension.ActionSource
	if match(t, scope, gen, nil).Match {
		t.Fatal("a generation key was claimed by hand")
	}
	other := event(gh.TypeClaimRequested, "issue/7", "github.claim.v1", nil)
	other.Event.Source = extension.ActionSource
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
	s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`","projection":{"dry_run":true}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	w := domain.Work{ID: "w1", Kind: gh.WorkKindHeld, State: domain.WorkWaiting, CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: "issue/12"}}}
	lines, ok := gh.Annotate(s, "https://github.com", w)
	got := map[string]string{}
	for _, l := range lines {
		got[l.Name] = l.Value
	}
	if !ok || got["issue"] != repo+"#12" || got["url"] != "https://github.com/"+repo+"/issues/12" || got["held"] == "" || got["projection"] == "" {
		t.Fatalf("annotations: %v %v", got, ok)
	}
	if _, ok := gh.Annotate(s, "https://github.com", domain.Work{ID: "x", CorrelationKeys: []domain.CorrelationKey{{Source: "other", ExternalID: "issue/1"}}}); ok {
		t.Fatal("annotated Work of another entry")
	}
}
