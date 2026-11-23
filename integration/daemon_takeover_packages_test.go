package integration_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// takeoverScope is a config with one scope and the given extension entries.
func takeoverScope(t *testing.T, id string, extensions []any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
		"scopes": []any{map[string]any{"id": id, "name": id,
			"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}},
			"bindings":      []any{}, "routes": []any{}, "extensions": extensions, "secret_refs": map[string]any{},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// activeGeneration is the shared state's committed config generation.
func activeGeneration(t *testing.T, shared string) uint64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+shared+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw []byte
	if err = db.QueryRow(`SELECT value FROM kv WHERE namespace='core.config' AND key='active'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var active struct{ Generation uint64 }
	if err = json.Unmarshal(raw, &active); err != nil {
		t.Fatal(err)
	}
	return active.Generation
}

// Packages are installed on each host. A successor without one the shared
// config uses must not replace that config, for every host, with its own file.
func TestDaemonTakeoverKeepsConfigWithoutItsPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("process kill drills")
	}
	bins := packageBinaries(t)
	d := newDrill(t)
	shared := filepath.Join(d.root, "shared.db")
	dir, _ := assemble(t, t.TempDir(), bins["notes-extension"])
	if out, err := exec.Command(daemonBinary(t), "--data-dir", d.state, "extension", "install", dir, "--config", d.config).CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	service := filepath.Join(d.root, "service.json")
	if err := os.WriteFile(service, []byte(`{"notes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"name": "notes", "type": "notes", "secret_names": []any{}, "config": map[string]any{"service": service, "scope": "with-package"}}
	if err := os.WriteFile(d.config, takeoverScope(t, "with-package", []any{entry}), 0o600); err != nil {
		t.Fatal(err)
	}
	holder := startInstance(t, d, "a", shared)
	waitFor(t, "the holder serving", holder.serving)
	stored := activeGeneration(t, shared)

	// Another host: no package, and a config file of its own that validates.
	other := filepath.Join(d.root, "host-b", "config.json")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, takeoverScope(t, "local-only", []any{}), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := func(name string) {
		t.Helper()
		i := startInstance(t, d, name, shared, "--config", other)
		select {
		case <-i.exited:
		case <-time.After(45 * time.Second):
			t.Fatalf("%s kept running (generation %d, was %d):\n%s", name, activeGeneration(t, shared), stored, i.log())
		}
		if got := activeGeneration(t, shared); got != stored {
			t.Fatalf("%s replaced the shared config: generation %d, was %d\n%s", name, got, stored, i.log())
		}
		if log := i.log(); !strings.Contains(log, "it uses notes") || !strings.Contains(log, "--adopt-config") || strings.Contains(log, "open-conduit serving") {
			t.Fatalf("%s's refusal doesn't say what's missing:\n%s", name, log)
		}
	}
	// As a standby it is found at once, and the holder is undisturbed.
	refused("standby")
	if !holder.serving() {
		t.Fatal("refusing the standby disturbed the holder")
	}
	db, err := sql.Open("sqlite", "file:"+shared+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := holder.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-holder.exited
	// Refused before Open, so it doesn't take the lease a good host needs.
	before := readTenure(t, db)
	refused("successor")
	if after := readTenure(t, db); after != before {
		t.Fatalf("the refused successor took the lease: %+v -> %+v", before, after)
	}

	// The control: a host that has the package serves the same state.
	same := startInstance(t, d, "c", shared)
	waitFor(t, "a successor with the package serving", same.serving)
	if got := activeGeneration(t, shared); got != stored {
		t.Fatalf("a successor with the package changed the generation to %d", got)
	}
	if err := same.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-same.exited

	// Replacing it takes an operator naming the generation.
	adopted := startInstance(t, d, "e", shared, "--config", other, "--adopt-config", strconv.FormatUint(stored, 10))
	waitFor(t, "the operator's replacement serving", func() bool {
		select {
		case <-adopted.exited:
			t.Fatalf("the adopting daemon exited:\n%s", adopted.log())
		default:
		}
		return adopted.serving()
	})
	if got := activeGeneration(t, shared); got != stored+1 {
		t.Fatalf("adopting left generation %d, was %d", got, stored)
	}
}
