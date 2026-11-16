package mcpext

import (
	"testing"

	"github.com/open-conduit/open-conduit/extensions/mcp"
	"github.com/open-conduit/open-conduit/protocol"
)

// The manifest is valid, declares both entry types' operations, and
// requires a sandbox for the stdio type only.
func TestManifest(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{mcp.StdioType: protocol.EntrySandboxRequired, mcp.HTTPType: ""}
	if len(m.Provides.Extensions) != len(want) {
		t.Fatalf("extensions = %+v", m.Provides.Extensions)
	}
	for _, x := range m.Provides.Extensions {
		if sb, ok := want[x.Type]; !ok || !x.Operations || x.Sandbox != sb {
			t.Errorf("extension type %+v", x)
		}
	}
}
