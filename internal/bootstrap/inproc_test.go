package bootstrap

import (
	"testing"

	"github.com/open-conduit/open-conduit/extension/inproc"
)

// The first-party list composes through the contract alone: each
// extension's config types and operations reach the registry, and no two
// share a name.
func TestFirstPartyCompose(t *testing.T) {
	reg := registry(offlineExecutors())
	seen := map[string]bool{}
	for _, x := range firstParty() {
		if seen[x.Name()] {
			t.Fatalf("%s listed twice", x.Name())
		}
		seen[x.Name()] = true
		if typed, ok := x.(inproc.Typed); ok {
			for _, c := range typed.Types() {
				_, conn := reg.Connections[c.Type]
				_, entry := reg.Extensions[c.Type]
				if !conn && !entry {
					t.Errorf("%s type %s not registered", x.Name(), c.Type)
				}
			}
		}
		if o, ok := x.(inproc.Operator); ok {
			for _, op := range o.Operations() {
				found := false
				for _, r := range reg.Operations {
					found = found || r == op
				}
				if !found {
					t.Errorf("%s operation %s not registered", x.Name(), op)
				}
			}
		}
	}
}
