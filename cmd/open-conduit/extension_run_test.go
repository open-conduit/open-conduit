package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// printed is what `open-conduit extension-run NAME package-manifest`
// prints: Main's stdout for that subcommand.
func printed(t *testing.T, name string) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code := runnable[name].Main([]string{"package-manifest"})
	os.Stdout = stdout
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil || code != 0 {
		t.Fatalf("%s package-manifest: exit %d %v", name, code, err)
	}
	return out
}

// The manifest discovery reads in process (hostManifest) is, for every
// extension this binary serves, the one `extension-run NAME
// package-manifest` prints, and it is the named extension's, run on the
// host; a name it doesn't serve is not served.
func TestHostManifestMatchesPackageManifest(t *testing.T) {
	for name := range runnable {
		raw, m, err := hostManifest(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := protocol.ParseManifest(raw); err != nil {
			t.Fatalf("%s: the in-process manifest is refused: %v", name, err)
		}
		if m.Name != name || !m.RunsOnHost() {
			t.Fatalf("%s: answered %q with command %q", name, m.Name, m.Command)
		}
		want, err := protocol.ParseManifest(printed(t, name))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(m)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Fatalf("%s: the in-process manifest differs from package-manifest's:\n%s\n%s", name, a, b)
		}
	}
	if _, _, err := hostManifest("nothing-served"); err == nil {
		t.Fatal("a name this binary doesn't serve was answered")
	}
}
