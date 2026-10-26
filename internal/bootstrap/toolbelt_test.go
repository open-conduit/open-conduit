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
