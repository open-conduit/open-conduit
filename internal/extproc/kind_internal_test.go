package extproc

import (
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// TestRouteKindCheck: a route creates Work only of its extension's kinds,
// except a successor that carries on the finished Work it was asked about
// as that Work's kind (issue #639). A successor of another kind, a
// successor asked for live Work, and new Work of a foreign kind are each
// still refused.
func TestRouteKindCheck(t *testing.T) {
	own := func(k string) bool { return k == "slack.conversation" }
	done := &domain.Work{ID: "webhook/x", Kind: "webhook.delivery", State: domain.WorkCompleted}
	live := &domain.Work{ID: "webhook/x", Kind: "webhook.delivery", State: domain.WorkReady}
	for _, c := range []struct {
		name      string
		d         extension.RouteDecision
		w         *domain.Work
		undeclare bool
	}{
		{"own kind", extension.RouteDecision{Match: true, Kind: "slack.conversation"}, nil, false},
		{"no kind", extension.RouteDecision{Match: true}, live, false},
		{"no match", extension.RouteDecision{Kind: "github.issue"}, nil, false},
		{"successor of the finished Work's kind", extension.RouteDecision{Match: true, Successor: true, Kind: "webhook.delivery"}, done, false},
		{"new Work of a foreign kind", extension.RouteDecision{Match: true, Kind: "webhook.delivery"}, nil, true},
		{"successor of another kind", extension.RouteDecision{Match: true, Successor: true, Kind: "github.issue"}, done, true},
		{"successor of live Work", extension.RouteDecision{Match: true, Successor: true, Kind: "webhook.delivery"}, live, true},
		{"successor with no Work", extension.RouteDecision{Match: true, Successor: true, Kind: "webhook.delivery"}, nil, true},
	} {
		if got := undeclaredKind(c.d, c.w, own); got != c.undeclare {
			t.Errorf("%s: undeclared %v, want %v", c.name, got, c.undeclare)
		}
	}
}
