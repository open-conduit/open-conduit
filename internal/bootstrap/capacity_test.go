package bootstrap

import (
	"maps"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
)

// TestScopeLimits: a scope's limits.concurrency reaches the scheduler only
// where it narrows what the scope inherits (#39); the scheduler checks every
// ancestor, so inherited limits need no entry.
func TestScopeLimits(t *testing.T) {
	s := &config.Snapshot{Settings: config.Settings{Concurrency: 4}, Scopes: map[domain.ID]*config.EffectiveScope{
		"root":    {ID: "root", Concurrency: 4},
		"team":    {ID: "team", ParentID: "root", Concurrency: 2},
		"inherit": {ID: "inherit", ParentID: "team", Concurrency: 2},
		"narrow":  {ID: "narrow", ParentID: "team", Concurrency: 1},
		"capped":  {ID: "capped", Concurrency: 3},
	}}
	want := map[domain.ID]int{"team": 2, "narrow": 1, "capped": 3}
	if got := scopeLimits(s); !maps.Equal(got, want) {
		t.Fatalf("scope limits %v, want %v", got, want)
	}
}
