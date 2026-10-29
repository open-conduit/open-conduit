package bootstrap

import (
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/internal/cli"
)

// Every `init` setup has an `adapters` entry, so what init can add can also
// be looked up with `adapters TYPE`.
func TestEverySetupHasAnAdapter(t *testing.T) {
	catalog := Adapters()
	for _, s := range Setups() {
		typ := s.SetupInfo().Type
		if !slices.ContainsFunc(catalog, func(a cli.Adapter) bool { return a.Type == typ }) {
			t.Errorf("init %s has no adapters entry", typ)
		}
	}
}
