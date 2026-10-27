package extproc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// writePackage lays out a minimal package (a shell script command) in
// dir/<name>.
func writePackage(t *testing.T, dir string, m protocol.Manifest) string {
	t.Helper()
	pkg := filepath.Join(dir, m.Name)
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(pkg, protocol.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, m.Command[0]), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return pkg
}

func manifest(name, version string, kv int) protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: name, Version: version, Command: []string{name + "-extension"}, KVVersion: kv,
		Provides: protocol.Provides{Checks: []protocol.CheckDecl{{Name: name}}}}
}

func TestInstallUpdateRollbackRemove(t *testing.T) {
	src, root := t.TempDir(), filepath.Join(t.TempDir(), Dir)
	v1 := writePackage(t, filepath.Join(src, "v1"), manifest("acme", "1.0.0", 1))
	v2 := writePackage(t, filepath.Join(src, "v2"), manifest("acme", "2.0.0", 2))
	old := writePackage(t, filepath.Join(src, "old"), manifest("acme", "0.9.0", 0))

	if _, err := Install(root, v1, InstallOptions{Update: true}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("update of nothing: %v", err)
	}
	res, err := Install(root, v1, InstallOptions{})
	if err != nil || res.Version != "1.0.0" || res.Previous != "" {
		t.Fatalf("install: %+v %v", res, err)
	}
	if _, err := Install(root, v2, InstallOptions{}); !errors.Is(err, ErrInstalled) {
		t.Fatalf("install over an installed package: %v", err)
	}
	if _, err := Install(root, old, InstallOptions{Update: true}); !errors.Is(err, ErrIncompatiblePackage) || !errors.Is(err, protocol.ErrKVDowngrade) {
		t.Fatalf("a lower kv_version: %v", err)
	}
	if d := Diagnose("acme", func() error { _, err := Install(root, old, InstallOptions{Update: true}); return err }(), Paths{}); d.Code != "kv_downgrade" {
		t.Fatalf("diagnosis %+v", d)
	}
	res, err = Install(root, v2, InstallOptions{Update: true})
	if err != nil || res.Version != "2.0.0" || res.Previous != "1.0.0" {
		t.Fatalf("update: %+v %v", res, err)
	}
	list, errs := List(root)
	if len(errs) != 0 || len(list) != 1 || list[0].Manifest.Version != "2.0.0" || list[0].Previous != "1.0.0" {
		t.Fatalf("list: %+v %v", list, errs)
	}
	// The installer's own entries are never discovered.
	if found, errs := Discover(root); len(found) != 1 || len(errs) != 0 {
		t.Fatalf("discover sees %d (%v)", len(found), errs)
	}
	res, err = Rollback(root, "acme")
	if err != nil || res.Version != "1.0.0" || res.Previous != "2.0.0" {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if res, err = Rollback(root, "acme"); err != nil || res.Version != "2.0.0" {
		t.Fatalf("rollback of the rollback: %+v %v", res, err)
	}
	if _, err := Remove(root, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme")); !os.IsNotExist(err) {
		t.Fatal("removed package still installed")
	}
	if _, err := Rollback(root, "acme"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("rollback after removal: %v", err)
	}
	if _, err := Remove(root, "acme"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("remove twice: %v", err)
	}
}

func TestInstallRefusals(t *testing.T) {
	src, root := t.TempDir(), filepath.Join(t.TempDir(), Dir)
	taken := Taken{}
	taken.Add("name", "slack")
	slack := writePackage(t, filepath.Join(src, "a"), manifest("slack", "1.0.0", 0))
	if _, err := Install(root, slack, InstallOptions{Taken: taken}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a first-party name: %v", err)
	} else if d := Diagnose("slack", err, Paths{}); d.Code != "registration_conflict" {
		t.Fatalf("diagnosis %+v", d)
	}
	future := manifest("future", "1.0.0", 0)
	future.Protocol = []int{protocol.Version, protocol.Version + 1}
	dir := writePackage(t, filepath.Join(src, "b"), future)
	raw, _ := json.Marshal(map[string]any{"protocol": []int{protocol.Version + 1}, "name": "future", "version": "1", "command": []string{"future-extension"}, "provides": map[string]any{}})
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root, dir, InstallOptions{}); err == nil {
		t.Fatal("a package for an unspoken protocol installed")
	}
	broken := writePackage(t, filepath.Join(src, "c"), manifest("broken", "1.0.0", 0))
	if err := os.Chmod(filepath.Join(broken, "broken-extension"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(root, broken, InstallOptions{}); !errors.Is(err, ErrPackage) {
		t.Fatalf("a non-executable command: %v", err)
	} else if d := Diagnose("broken", err, Paths{}); d.Code != "invalid_package" {
		t.Fatalf("diagnosis %+v", d)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("refused installs left %d entries", len(entries))
	}
}

func TestDiagnoseStartFailures(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{errors.Join(ErrRefused, protocol.ErrIncompatible), "incompatible_protocol"},
		{errors.Join(ErrRefused, ErrManifestMismatch), "manifest_mismatch"},
		{errors.Join(ErrRefused, protocol.ErrKVDowngrade), "kv_downgrade"},
		{ErrStart, "start_failed"},
		{errors.Join(ErrHandshake, protocol.ErrTransport), "handshake_failed"},
		{ErrConfigure, "configure_refused"},
		{protocol.ErrNewerManifest, "needs_newer_host"},
	} {
		d := Diagnose("acme", c.err, Paths{Log: "/state/logs/extensions/acme.log"})
		if d.Code != c.code || d.Hint == "" || d.Summary == "" {
			t.Errorf("%v: %+v, want %s", c.err, d, c.code)
		}
	}
}
