package protocol_test

import (
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// An executor's capacity pool is agent turns (empty) or jobs; any other
// pool is refused, so a package can't invent a pool no setting bounds.
func TestExecutorPool(t *testing.T) {
	m := func(pool string) protocol.Manifest {
		return protocol.Manifest{Protocol: []int{1}, Name: "runner", Version: "1", Command: []string{"./runner"}, Provides: protocol.Provides{
			Executors: []protocol.ExecutorDecl{{Name: "runner", Capabilities: protocol.Capabilities{Toolbelt: "cli"}, Pool: pool}},
		}}
	}
	for _, pool := range []string{"", "jobs"} {
		if err := m(pool).Validate(); err != nil {
			t.Errorf("pool %q: %v", pool, err)
		}
	}
	if err := m("turbo").Validate(); !errors.Is(err, protocol.ErrManifest) {
		t.Fatalf("pool turbo: %v", err)
	}
}
