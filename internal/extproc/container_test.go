package extproc

import (
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// A package's container provider is a placer only when declared with
// place, and a forwarder only when declared with forward, either or both.
func TestContainerCapabilitiesAsDeclared(t *testing.T) {
	for _, c := range []struct {
		decl            protocol.ContainerDecl
		forward, places bool
	}{
		{protocol.ContainerDecl{Type: "pod"}, false, false},
		{protocol.ContainerDecl{Type: "pod", Forward: true}, true, false},
		{protocol.ContainerDecl{Type: "pod", Place: true}, false, true},
		{protocol.ContainerDecl{Type: "pod", Forward: true, Place: true}, true, true},
	} {
		var m protocol.Manifest
		m.Provides.Containers = []protocol.ContainerDecl{c.decl}
		p, ok := (&Remote{m: m}).Container("pod")
		if !ok {
			t.Fatalf("%+v: no provider", c.decl)
		}
		_, forward := p.(extension.ContainerForwarder)
		_, places := p.(extension.ContainerPlacer)
		if forward != c.forward || places != c.places {
			t.Errorf("%+v: forwarder %v, placer %v; want %v, %v", c.decl, forward, places, c.forward, c.places)
		}
	}
}
