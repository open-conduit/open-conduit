package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestExportFileRoundTrips: a stored generation exports as a config file
// that composes back to the same generation. A folder's scopes go back to
// the include that brought them in, nested ones included, and secret
// references stay reference text.
func TestExportFileRoundTrips(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "web")
	writeFragment(t, folder, `{"version":1,"scopes":[{"id":"web","name":"web"},{"id":"web-docs","name":"docs","parent":"web"}]}`, 0o644)
	s, err := ValidateFile(central(folder), includeRegistry(), Includes{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := ExportFile(s.Normalized)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(text)
	if err != nil {
		t.Fatalf("export is not a config file: %v\n%s", err, text)
	}
	if len(doc.Includes) != 1 || doc.Includes[0] != (Include{Path: folder, Parent: "home"}) || len(doc.Scopes) != 1 || doc.Scopes[0].ID != "home" {
		t.Fatalf("folder scopes not turned back into their include: %+v %+v", doc.Includes, doc.Scopes)
	}
	if doc.Scopes[0].SecretRefs["key"] != "env:KEY" {
		t.Fatalf("secret reference lost: %v", doc.Scopes[0].SecretRefs)
	}
	again, err := ValidateFile(text, includeRegistry(), Includes{})
	if err != nil || again.Hash != s.Hash {
		t.Fatalf("exported file composes to another generation: %v\n%s", err, text)
	}
	if !strings.HasSuffix(string(text), "}\n") || !strings.Contains(string(text), "\n  ") {
		t.Fatalf("export is not indented text: %q", text)
	}
}
