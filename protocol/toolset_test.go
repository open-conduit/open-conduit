package protocol_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// A package publishes toolsets of its own operations and builtins; one
// outside its namespace, granting an operation it doesn't declare or
// another extension's, or declared twice, is refused with the manifest.
func TestManifestToolsets(t *testing.T) {
	ops := func(names ...string) []extension.ToolsetOperation {
		var out []extension.ToolsetOperation
		for i, n := range names {
			out = append(out, extension.ToolsetOperation{Name: n, Since: i + 1})
		}
		return out
	}
	m := func(ts ...protocol.ToolsetDecl) protocol.Manifest {
		return protocol.Manifest{Protocol: []int{1}, Name: "notes", Version: "1", Command: []string{"./notes"}, Provides: protocol.Provides{
			Operations: []protocol.OperationDecl{{Name: "notes.read", Kind: protocol.OperationHandler}, {Name: "notes.write", Kind: protocol.OperationHandler}},
			Toolsets:   ts,
		}}
	}
	ok := protocol.ToolsetDecl{Name: "notes.author", Operations: ops("read_context", "notes.read", "notes.write")}
	if err := m(ok).Validate(); err != nil {
		t.Fatalf("valid toolset: %v", err)
	}
	for name, ts := range map[string][]protocol.ToolsetDecl{
		"namespace":   {{Name: "other.author", Operations: ops("notes.read")}},
		"undeclared":  {{Name: "notes.author", Operations: ops("notes.delete")}},
		"foreign":     {{Name: "notes.author", Operations: ops("github.pr.edit")}},
		"empty":       {{Name: "notes.author"}},
		"twice":       {ok, ok},
		"repeated op": {{Name: "notes.author", Operations: ops("notes.read", "notes.read")}},
	} {
		if err := m(ts...).Validate(); !errors.Is(err, protocol.ErrManifest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPlaybookOperations(t *testing.T) {
	for text, want := range map[string]string{
		"---\nname: x\noperations: [a.b, \"c.d\"]\n---\nbody":       "a.b c.d",
		"---\nname: x\noperations: a.b, c.d\n---\nbody":             "a.b c.d",
		"---\nname: x\n---\noperations: [a.b]\n":                    "",
		"no frontmatter\noperations: [a.b]\n":                       "",
		"---\noperations: [\"a.b\",\"a.b\"]\ndescription: y\n---\n": "a.b",
	} {
		if got := strings.Join(extension.PlaybookOperations(text), " "); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}
