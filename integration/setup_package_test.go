package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// The setup extension is an installed package from its own module
// (github.com/open-conduit/extension-setup, pinned in go.mod with a `tool`
// directive), never imported here (#243). The tests build the pinned
// version with scripts/bundle-packages.sh, the script the release uses, and
// lay it out the way an operator's install has it: bundled beside the
// daemon binary, or installed beside a config file. No network is used.
var (
	setupOnce sync.Once
	setupDir  string
	setupErr  error
)

// setupPackage is the built setup package directory (extensions/setup in a
// release archive for this host).
func setupPackage(t *testing.T) string {
	t.Helper()
	dir, err := buildSetupPackage()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func buildSetupPackage() (string, error) {
	setupOnce.Do(func() {
		out, err := os.MkdirTemp("", "oc-setup-package-")
		if err != nil {
			setupErr = err
			return
		}
		cmd := exec.Command("sh", "scripts/bundle-packages.sh", out)
		cmd.Dir = ".."
		if o, err := cmd.CombinedOutput(); err != nil {
			setupErr = &bundleError{err: err, out: string(o)}
			return
		}
		matches, _ := filepath.Glob(filepath.Join(out, "*", "extensions", "setup"))
		if len(matches) != 1 {
			setupErr = &bundleError{out: "no extensions/setup in " + out}
			return
		}
		setupDir = matches[0]
	})
	return setupDir, setupErr
}

type bundleError struct {
	err error
	out string
}

func (e *bundleError) Error() string {
	if e.err == nil {
		return "bundle-packages: " + e.out
	}
	return "bundle-packages: " + e.err.Error() + "\n" + e.out
}

// placeSetup copies the setup package into root/extensions/setup: beside a
// binary, that is the release archive's bundled copy; beside a config file,
// an installed one.
func placeSetup(t *testing.T, root string) {
	t.Helper()
	if err := copySetup(root); err != nil {
		t.Fatal(err)
	}
}

func copySetup(root string) error {
	src, err := buildSetupPackage()
	if err != nil {
		return err
	}
	dst := filepath.Join(root, "extensions", "setup")
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.CopyFS(dst, os.DirFS(src))
}

// setupManifest is the package's manifest.
func setupManifest(t *testing.T) protocol.Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(setupPackage(t), protocol.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// setupPlaybook is the package's setup playbook with {{bin}} as bin, as the
// host renders it.
func setupPlaybook(t *testing.T, bin string) string {
	t.Helper()
	m := setupManifest(t)
	raw, err := os.ReadFile(filepath.Join(setupPackage(t), m.Provides.Playbooks, "setup.md"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "{{bin}}", bin)
}
