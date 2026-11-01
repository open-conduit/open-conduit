package extproc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// A package's playbooks are its declared directory's `<name>.md` files;
// anything else there is ignored, and `{{bin}}` is the host's to fill in.
func TestPlaybooks(t *testing.T) {
	dir := t.TempDir()
	books := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(filepath.Join(books, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"setup.md": "run {{bin}} doctor", "README.txt": "no", "Bad_Name.md": "no", "nested/inner.md": "no"} {
		if err := os.WriteFile(filepath.Join(books, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := protocol.Manifest{Name: "acme", Provides: protocol.Provides{Playbooks: "playbooks"}}
	got, err := Playbooks(dir, m)
	if err != nil || len(got) != 1 || got["setup"] != "run {{bin}} doctor" {
		t.Fatalf("%v %v", got, err)
	}
	if RenderPlaybook(got["setup"], "/opt/oc") != "run /opt/oc doctor" {
		t.Fatal("bin not rendered")
	}
	if got, err := Playbooks(dir, protocol.Manifest{Name: "acme"}); err != nil || got != nil {
		t.Fatalf("undeclared: %v %v", got, err)
	}
	m.Provides.Playbooks = "missing"
	if _, err := Playbooks(dir, m); err == nil {
		t.Fatal("a missing directory read")
	}
}
