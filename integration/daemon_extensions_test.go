package integration_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/protocol/examples/notes"
)

// TestDaemonExtensionPackages is the 07-03 daemon wiring drill: packages
// installed with the CLI beside the config are discovered at start, admitted
// against the first-party registrations, started behind their scoped host
// service and registered next to the first-party surfaces. The example
// extension's ingress, route and validator and the example executor run a
// workflow end to end inside the real daemon; config apply reaches the
// running package through configure; a daemon crash never orphans a package
// process; and removal is refused while the config uses the package.
func TestDaemonExtensionPackages(t *testing.T) {
	if testing.Short() {
		t.Skip("process drills")
	}
	ctx := context.Background()
	bins := packageBinaries(t)
	d := newDrill(t)
	pkgs := t.TempDir()
	cli := func(args ...string) (string, error) {
		out, err := exec.Command(daemonBinary(t), append(append([]string{"--data-dir", d.state}, args...), "--config", d.config)...).CombinedOutput()
		return string(out), err
	}
	for _, bin := range []string{"notes-extension", "runner-extension"} {
		dir, _ := assemble(t, pkgs, bins[bin])
		if out, err := cli("extension", "install", dir); err != nil || !strings.Contains(out, "Installed") {
			t.Fatalf("install %s: %v\n%s", bin, err, out)
		}
	}
	// A package claiming a first-party name is refused at install.
	slackDir, _ := assemble(t, pkgs, bins["slack-extension"])
	if out, err := cli("extension", "install", slackDir); err == nil || !strings.Contains(out, "registration_conflict") {
		t.Fatalf("installing a first-party name: %v\n%s", err, out)
	}
	if out, err := cli("extension", "list"); err != nil || !strings.Contains(out, "notes") || !strings.Contains(out, "runner") {
		t.Fatalf("list: %v\n%s", err, out)
	}

	writeNotes := func(path string, ids ...string) {
		var ns []notes.Note
		for _, id := range ids {
			ns = append(ns, notes.Note{ID: id, Rev: "r1", Text: "note " + id, At: extensionNoteTime})
		}
		raw, _ := json.Marshal(notes.ServiceFile{Notes: ns})
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := filepath.Join(d.root, "service.json")
	writeNotes(service, "a", "b")
	config := func(entry map[string]any) []byte {
		cfg := map[string]any{"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
			"scopes": []any{map[string]any{"id": "notes-scope", "name": "Notes",
				"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}},
				"bindings":      []any{map[string]any{"name": "run", "executor": "runner", "requires": map[string]any{"toolbelt": "none"}, "operations": []any{}, "secret_names": []any{}, "config": map[string]any{"auto": true}}},
				"routes":        []any{map[string]any{"name": "notes", "handler": "notes", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
				"extensions":    []any{map[string]any{"name": "notes", "type": "notes", "config": entry, "secret_names": []any{}}},
				"secret_refs":   map[string]any{},
			}}}
		b, _ := json.Marshal(cfg)
		return b
	}
	if err := os.WriteFile(d.config, config(map[string]any{"service": service, "scope": "notes-scope"}), 0o600); err != nil {
		t.Fatal(err)
	}
	// The offline check knows the installed packages' types.
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", d.config).CombinedOutput(); err != nil || !strings.Contains(string(out), "valid") {
		t.Fatalf("offline config check: %v\n%s", err, out)
	}

	d.start("")
	completed := func(n int) func() bool {
		return func() bool {
			page, err := d.c.Works(ctx, url.Values{"scope_id": {"notes-scope"}})
			if err != nil {
				return false
			}
			done := 0
			for _, w := range page.Items {
				if w.Kind == notes.Kind && w.State == "completed" {
					done++
				}
			}
			return done == n
		}
	}
	d.until("both notes worked by the external executor", 60*time.Second, completed(2))
	runs, err := d.c.Runs(ctx, url.Values{"scope_id": {"notes-scope"}})
	if err != nil || len(runs.Items) != 2 || runs.Items[0].Executor != "runner" {
		t.Fatalf("runs: %+v %v", runs.Items, err)
	}
	extensions := func() client.Check {
		doc, err := d.c.Doctor(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range doc.Checks {
			if c.Name == "extensions" {
				return c
			}
		}
		t.Fatal("doctor has no extensions check")
		return client.Check{}
	}
	if c := extensions(); c.Status != "ok" || !strings.Contains(strings.Join(c.Items, "\n"), "notes 0.2.0: running") || !strings.Contains(strings.Join(c.Items, "\n"), "runner 0.1.0: running") {
		t.Fatalf("doctor: %+v", c)
	}

	// The package validates its own entries when the daemon checks a config.
	if v, err := d.c.ValidateConfig(ctx, config(map[string]any{"scope": "notes-scope"})); err == nil || v.Valid || !strings.Contains(err.Error(), "rejected by its registered owner") {
		t.Fatalf("an entry without a service file validated: %+v %v", v, err)
	}
	// Apply reaches the running package through configure: the entry now
	// names a service file with a third note.
	next := filepath.Join(d.root, "service-2.json")
	writeNotes(next, "a", "b", "c")
	candidate := config(map[string]any{"service": next, "scope": "notes-scope"})
	v, err := d.c.ValidateConfig(ctx, candidate)
	if err != nil || !v.Valid {
		t.Fatalf("validate: %+v %v", v, err)
	}
	if applied, err := d.c.ApplyConfig(ctx, candidate, v.CandidateHash, "1", "apply-2"); err != nil || applied.Generation != "2" {
		t.Fatalf("apply: %+v %v", applied, err)
	}
	s, err := d.c.Scope(ctx, "notes-scope")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := d.c.ReconcileScope(ctx, "notes-scope", s.Revision, "reconcile-notes"); err != nil || len(res.Sources) != 1 {
		t.Fatalf("reconcile: %+v %v", res, err)
	}
	d.until("the third note after apply", 60*time.Second, completed(3))

	// A daemon crash never orphans a package process: each exits on EOF.
	d.kill()
	root := filepath.Join(d.root, "extensions")
	d.until("package processes exited with the daemon", 15*time.Second, func() bool {
		out, err := exec.Command("ps", "-ax", "-o", "command").Output()
		return err == nil && !strings.Contains(string(out), root)
	})
	d.start("")
	time.Sleep(2 * time.Second)
	if !completed(3)() {
		t.Fatal("work changed across the restart")
	}
	if c := extensions(); c.Status != "ok" {
		t.Fatalf("doctor after restart: %+v", c)
	}
	// Removal is refused while the config uses the package.
	if out, err := cli("extension", "remove", "notes"); err == nil || !strings.Contains(out, "still uses notes") {
		t.Fatalf("remove while used: %v\n%s", err, out)
	}
}
