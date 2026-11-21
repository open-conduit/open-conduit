package protocol_test

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// TestTypeScriptLanesMatchTheCatalogue: an extension gets the same capacity
// whichever SDK it is built on.
func TestTypeScriptLanesMatchTheCatalogue(t *testing.T) {
	raw, err := os.ReadFile("../sdk/typescript/src/jsonrpc.ts")
	if err != nil {
		t.Fatal(err)
	}
	for name, lane := range map[string]protocol.Lane{"heldMethods": protocol.LaneHeld, "controlMethods": protocol.LaneControl} {
		block := regexp.MustCompile(`const ` + name + ` = new Set\(\[([^\]]*)\]`).FindSubmatch(raw)
		if block == nil {
			t.Fatalf("%s not found", name)
		}
		var got, want []string
		for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(block[1], -1) {
			got = append(got, string(m[1]))
		}
		for _, m := range protocol.Methods {
			if m.Lane == lane {
				want = append(want, m.Name)
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s: %v, want %v", name, got, want)
		}
	}
}
