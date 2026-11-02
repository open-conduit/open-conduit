package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// TestToolbeltBesideResolvesLinks: the cask and the install script link only
// open-conduit onto PATH and keep `os` beside the real binary, so the default
// toolbelt directory follows the link, and an unrelated `os` beside the link
// is not taken for it.
func TestToolbeltBesideResolvesLinks(t *testing.T) {
	root := t.TempDir()
	install, bin := filepath.Join(root, "install"), filepath.Join(root, "bin")
	for _, dir := range []string{install, bin} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(install, "open-conduit"), filepath.Join(install, "os"), filepath.Join(bin, "os")} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(bin, "open-conduit")
	if err := os.Symlink(filepath.Join(install, "open-conduit"), link); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(install)
	if got := toolbeltBeside(link); got != want {
		t.Fatalf("through the link: %q, want %q", got, want)
	}
	if got := toolbeltBeside(filepath.Join(install, "open-conduit")); got != want {
		t.Fatalf("direct: %q, want %q", got, want)
	}
	// Without a toolbelt beside the real binary, the link's directory is
	// still tried, as before.
	if err := os.Remove(filepath.Join(install, "os")); err != nil {
		t.Fatal(err)
	}
	if got := toolbeltBeside(link); got != bin {
		t.Fatalf("fallback: %q, want %q", got, bin)
	}
	if err := os.Remove(filepath.Join(bin, "os")); err != nil {
		t.Fatal(err)
	}
	if got := toolbeltBeside(link); got != "" {
		t.Fatalf("no toolbelt: %q", got)
	}
}

// TestStableToolbeltFollowsUpgrades: executions are given a toolbelt
// directory in the state directory whose `os` links to the install's, so a
// run that outlives an upgrade (which removes the versioned install) finds
// the new `os` once the daemon restarts and re-points the link.
func TestStableToolbeltFollowsUpgrades(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	install := func(version string) string {
		dir := filepath.Join(root, "Caskroom", version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "os"), []byte("#!/bin/sh\necho "+version+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	old := install("1.10.0")
	got, err := stableToolbelt(state, old)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(state, "toolbelt") {
		t.Fatalf("toolbelt dir %q", got)
	}
	read := func() string {
		b, err := os.ReadFile(filepath.Join(got, "os"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if b := read(); b != "#!/bin/sh\necho 1.10.0\n" {
		t.Fatalf("before the upgrade: %q", b)
	}
	// The upgrade installs the new version and removes the old one; the
	// next daemon start re-points the same path.
	upgraded := install("1.11.0")
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	if again, err := stableToolbelt(state, upgraded); err != nil || again != got {
		t.Fatalf("after the upgrade: %q, %v", again, err)
	}
	if b := read(); b != "#!/bin/sh\necho 1.11.0\n" {
		t.Fatalf("after the upgrade: %q", b)
	}
	// Starting again on the same install leaves the link as it is.
	if again, err := stableToolbelt(state, upgraded); err != nil || again != got {
		t.Fatalf("restart: %q, %v", again, err)
	}
	// No toolbelt, no directory.
	if none, err := stableToolbelt(state, ""); err != nil || none != "" {
		t.Fatalf("no toolbelt: %q, %v", none, err)
	}
}
