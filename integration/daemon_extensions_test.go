package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
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
	const existingRef = "env:NOTES_EXISTING_REF"
	config := func(entry map[string]any) []byte {
		cfg := map[string]any{"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
			"scopes": []any{map[string]any{"id": "notes-scope", "name": "Notes",
				"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}},
				"bindings":      []any{map[string]any{"name": "run", "executor": "runner", "requires": map[string]any{"toolbelt": "none"}, "operations": []any{}, "secret_names": []any{}, "config": map[string]any{"auto": true}}},
				"routes":        []any{map[string]any{"name": "notes", "handler": "notes", "binding": "run", "active_policy": "queue", "config": map[string]any{}}},
				"extensions":    []any{map[string]any{"name": "notes", "type": "notes", "config": entry, "secret_names": []any{}}},
				"secret_refs":   map[string]any{"existing": existingRef},
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
	// Offline, the package validates its own entries too: started for
	// validate alone, it refuses an entry without a service file.
	bad := filepath.Join(filepath.Dir(d.config), "bad.json")
	if err := os.WriteFile(bad, config(map[string]any{"scope": "notes-scope"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", bad).CombinedOutput(); err == nil || !strings.Contains(string(out), "rejected by its registered owner") {
		t.Fatalf("offline check of an invalid entry: %v\n%s", err, out)
	}
	// The route handler checks its binding (provides.routes[].binding): a
	// binding granting create_work is refused with the package's reason.
	granting := bytes.Replace(config(map[string]any{"service": service, "scope": "notes-scope"}), []byte(`"name":"run","operations":[]`), []byte(`"name":"run","operations":["create_work"]`), 1)
	if err := os.WriteFile(bad, granting, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", bad).CombinedOutput(); err == nil || !strings.Contains(string(out), "binding_refused") || !strings.Contains(string(out), "may not grant create_work") {
		t.Fatalf("offline check of a refused binding: %v\n%s", err, out)
	}

	// The installed provider and executor answer config check's workspace
	// checks as git and Claude Code do: a sandboxed binding may not use a
	// scratch cache (sandbox_workspace, with the package's reason), and a
	// scratch mode the runner's own workspace_mode overrides is warned about.
	withBinding := func(binding string) []byte {
		raw := config(map[string]any{"service": service, "scope": "notes-scope"})
		return bytes.Replace(raw, []byte(`"bindings":[`), []byte(`"bindings":[`+binding+`,`), 1)
	}
	if err := os.WriteFile(bad, withBinding(`{"name":"boxed","executor":"shell","requires":{"toolbelt":"cli"},"operations":[],"secret_names":[],"config":{"command":"true"},"resource":{"provider":"notes-scratch","config":{"cache":true}},"sandbox":{"provider":"srt","config":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", bad).CombinedOutput(); err == nil || !strings.Contains(string(out), "sandbox_workspace") || !strings.Contains(string(out), "a scratch cache is shared by every Work outside the sandbox") {
		t.Fatalf("offline check of a sandboxed scratch cache: %v\n%s", err, out)
	}
	if err := os.WriteFile(bad, withBinding(`{"name":"boxed","executor":"shell","requires":{"toolbelt":"cli"},"operations":[],"secret_names":[],"config":{"command":"true"},"resource":{"provider":"notes-scratch","config":{}},"sandbox":{"provider":"srt","config":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", bad).CombinedOutput(); err != nil || strings.Contains(string(out), "sandbox_workspace") {
		t.Fatalf("offline check of a sandboxed scratch without a cache: %v\n%s", err, out)
	}
	overridden := `{"name":"scratch","executor":"runner","requires":{"toolbelt":"none"},"operations":[],"secret_names":[],"config":{"auto":true,"workspace_mode":"work"},"resource":{"provider":"notes-scratch","config":{"mode":"fresh"}}}`
	if err := os.WriteFile(bad, withBinding(overridden), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", bad).CombinedOutput(); err != nil || !strings.Contains(string(out), `the notes-scratch resource's mode "fresh" never applies, because executor runner requests mode "work"`) {
		t.Fatalf("offline check of an overridden scratch mode: %v\n%s", err, out)
	}
	if err := os.WriteFile(bad, withBinding(strings.Replace(overridden, `"workspace_mode":"work"`, `"workspace_mode":"fresh"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(daemonBinary(t), "config", "check", "--file", bad).CombinedOutput(); err != nil || strings.Contains(string(out), "never applies") {
		t.Fatalf("offline check of an agreeing scratch mode: %v\n%s", err, out)
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
	// The package proposes config through the host's handles (permission
	// config.read): it reads the base, edits, validates and traces a
	// candidate, and never receives reference text, the base's or its own.
	var proposed notes.ConfigRun
	var proposedRaw []byte
	d.until("the package's config task ran", 90*time.Second, func() bool {
		matches, _ := filepath.Glob(filepath.Join(d.state, "*", "notes", "config.json"))
		if len(matches) == 0 {
			matches, _ = filepath.Glob(filepath.Join(d.state, "*", "*", "notes", "config.json"))
		}
		if len(matches) == 0 {
			return false
		}
		raw, err := os.ReadFile(matches[0])
		proposedRaw = raw
		return err == nil && json.Unmarshal(raw, &proposed) == nil
	})
	if strings.Contains(string(proposedRaw), existingRef) || strings.Contains(string(proposedRaw), notes.ConfigRef) {
		t.Fatalf("the package received reference text: %s", proposedRaw)
	}
	if !strings.Contains(proposed.Base.Text, `"existing": "[redacted]"`) || proposed.Base.Handle == "" || proposed.Base.Generation != "1" {
		t.Fatalf("base %+v", proposed.Base)
	}
	if len(proposed.Edits) != 2 || string(proposed.Edits[0].Before) != `"Notes"` || len(proposed.Edits[1].Before) != 0 ||
		!strings.Contains(proposed.Edits[1].Text, `"notes_token": "[redacted]"`) || !strings.Contains(proposed.Edits[1].Text, `"Notes, proposed"`) {
		t.Fatalf("edits %+v", proposed.Edits)
	}
	if proposed.Validate.Hash == "" || len(proposed.Validate.Diagnostics) != 0 {
		t.Fatalf("the candidate did not validate: %+v", proposed.Validate)
	}
	if proposed.Samples.Error != "" {
		t.Fatalf("trace against the candidate: %+v", proposed.Samples)
	}
	if s, err := d.c.Scope(ctx, "notes-scope"); err != nil || s.Name != "Notes" {
		t.Fatalf("the package's candidate was applied: %+v %v", s, err)
	}
	// The package declares a page core draws, and its Work offers the
	// candidate for core's review (config.candidate): the text behind its
	// handle reaches the operator alone, with the reference the package
	// never saw.
	// The bundled setup package's page is listed beside it.
	pages, err := d.c.Pages(ctx)
	byID := map[string]int{}
	for i, p := range pages {
		byID[p.ID] = i
	}
	n, ok := byID["notes"]
	if _, setup := byID["setup"]; err != nil || len(pages) != 2 || !ok || !setup || pages[n].Extension != "notes" || pages[n].WorkKind != notes.Kind || pages[n].Compose[0] != "remark" {
		t.Fatalf("pages: %+v %v", pages, err)
	}
	works, _ := d.c.Works(ctx, url.Values{"scope_id": {"notes-scope"}})
	var cand client.ConfigCandidate
	d.until("the package's candidate is offered for review", 30*time.Second, func() bool {
		cand, err = d.c.WorkConfigCandidate(ctx, works.Items[0].ID)
		return err == nil && cand.CandidateHash == proposed.Validate.Hash
	})
	if len(cand.Changes) != 2 || cand.Changes[0].Pointer != "/scopes/notes-scope/name" || string(cand.Changes[0].Before) != `"Notes"` ||
		!strings.Contains(cand.ConfigText, `"Notes, proposed"`) || !strings.Contains(cand.ConfigText, notes.ConfigRef) || cand.BaseGeneration != "1" {
		t.Fatalf("the package's candidate: %+v", cand)
	}
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
	// The package's declared check is listed under its own name.
	if doc, err := d.c.Doctor(ctx, false); err != nil || !slices.ContainsFunc(doc.Checks, func(c client.Check) bool { return c.Name == "notes" && c.Status == "ok" && c.Detail == "ok" }) {
		t.Fatalf("doctor lists no notes check: %+v %v", doc, err)
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
	d.settle()
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
