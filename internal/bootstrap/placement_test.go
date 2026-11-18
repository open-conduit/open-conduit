package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// The Work's last placement is kept beside its home, per provider: a
// launch under another provider, or with nothing kept, prefers none.
func TestWorkPlacement(t *testing.T) {
	home := filepath.Join(t.TempDir(), "homes", "k")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := workPlacement(home, "ssh"); got != "" {
		t.Fatalf("prefer %q with nothing kept, want none", got)
	}
	placeWork(home, "ssh", extension.Placement{ID: "agent@runner:22", Daemon: "d"})
	if got := workPlacement(home, "ssh"); got != "agent@runner:22" {
		t.Fatalf("prefer %q, want agent@runner:22", got)
	}
	if got := workPlacement(home, "pool"); got != "" {
		t.Fatalf("prefer %q under another provider, want none", got)
	}
	if fi, err := os.Stat(home + ".site"); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("site file %v %v, want owner-only", fi, err)
	}
}
