package config

import (
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

func TestGrantedConnection(t *testing.T) {
	types := map[string]bool{"github": true}
	s := &Snapshot{
		Connections: []Connection{{ID: "gh", Type: "github"}, {ID: "sl", Type: "slack"}},
		Scopes: map[domain.ID]*EffectiveScope{
			"on":  {Enabled: true, Authorization: Authorization{Connections: []string{"gh", "sl"}}},
			"off": {Enabled: false, Authorization: Authorization{Connections: []string{"gh"}}},
			"no":  {Enabled: true},
		},
	}
	if c, ok := s.GrantedConnection("on", "gh", types); !ok || c.ID != "gh" {
		t.Fatalf("granted: %+v %v", c, ok)
	}
	for _, x := range []struct {
		scope domain.ID
		id    string
	}{{"on", "sl"}, {"on", "missing"}, {"on", ""}, {"off", "gh"}, {"no", "gh"}, {"absent", "gh"}} {
		if _, ok := s.GrantedConnection(x.scope, x.id, types); ok {
			t.Errorf("%s/%q was granted", x.scope, x.id)
		}
	}
}
