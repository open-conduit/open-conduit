package artifactstore_test

import (
	"os"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/artifactstore"
)

// The published protocol names every method and error code a provider
// has to implement.
func TestProtocolPageCoversTheProtocol(t *testing.T) {
	raw, err := os.ReadFile("../../site/src/content/docs/architecture/artifact-providers.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	names := append([]string{artifactstore.CodeCorrupt, artifactstore.CodeInUse, protocol.CodeNotFound, protocol.CodeConflict, protocol.CodeInvalid, protocol.CodeUnsupportedVersion}, artifactstore.Methods...)
	for _, name := range names {
		if !strings.Contains(page, "`"+name+"`") {
			t.Errorf("artifact-providers.md doesn't describe %s", name)
		}
	}
}
