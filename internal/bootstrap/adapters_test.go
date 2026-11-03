package bootstrap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
)

func catalogAt(t *testing.T, configPath string) []cli.Adapter {
	t.Helper()
	catalog, errs := Adapters(configPath)
	if len(errs) != 0 {
		t.Fatalf("refused packages: %v", errs)
	}
	return catalog
}

func adapter(catalog []cli.Adapter, typ string) (cli.Adapter, bool) {
	i := slices.IndexFunc(catalog, func(a cli.Adapter) bool { return a.Type == typ })
	if i < 0 {
		return cli.Adapter{}, false
	}
	return catalog[i], true
}

// An installed package describes its own config types (#494): the
// catalogue `open-conduit adapters` and the config editor read is built
// from the manifest of the notes example installed beside the config, with
// no entry for it in the core.
func TestAdaptersFromInstalledManifest(t *testing.T) {
	withBundled(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(notes.Manifest(notes.Options{Release: 1}))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(extensionsRoot(configPath), notes.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := adapter(catalogAt(t, ""), notes.Name); ok {
		t.Fatal("notes is in the catalogue without being installed")
	}
	a, ok := adapter(catalogAt(t, configPath), notes.Name)
	if !ok {
		t.Fatal("the installed notes package is not in the catalogue")
	}
	if a.Title != "Notes" || a.Description != "Example external extension: a note service." || a.Setup != notes.Name {
		t.Errorf("notes entry: %+v", a)
	}
	want := []string{"extension", "route", "resource:" + notes.Scratch, "sandbox:" + notes.Jail}
	if !slices.Equal(a.Kinds, want) {
		t.Errorf("kinds %v, want %v", a.Kinds, want)
	}
	// watch's default is its setting's, declared once.
	if len(a.Entry) != 3 || !reflect.DeepEqual(a.Entry[0], cli.Field{Name: "service", Help: "the note service's JSON file", Required: true}) ||
		a.Entry[1].Default != "true" || !slices.Equal(a.Entry[1].Options, []string{"true", "false"}) {
		t.Errorf("entry fields %+v", a.Entry)
	}
	if !slices.Equal(a.Operations, []string{"notes.annotate", "notes.lookup"}) {
		t.Errorf("operations %v", a.Operations)
	}
	// An installed package's tunnel type is one settings.webhooks can
	// select, its config fields found under its own name.
	hooks, ok := adapter(catalogAt(t, configPath), "tunnel")
	if !ok || len(hooks.Binding) < 2 || hooks.Binding[1].Name != "tunnel" || !strings.Contains(hooks.Binding[1].Help, notes.Relay) {
		t.Errorf("the webhook listener's entry: %+v", hooks)
	}
	if relay, ok := adapter(catalogAt(t, configPath), notes.Relay); !ok || !slices.Equal(relay.Kinds, []string{"tunnel"}) || len(relay.Tunnel) != 1 || relay.Tunnel[0].Name != "region" {
		t.Errorf("the notes-relay tunnel entry: %+v", relay)
	}
}

// A connection's secret_refs keys are its fields marked secret; a type
// named other than its package is listed by kind under the package, unless
// it describes fields, when it gets an entry of its own so the config
// editor finds them by its name.
func TestManifestAdapters(t *testing.T) {
	m := protocol.Manifest{Name: "acme", Title: "Acme", Summary: "Tickets.", Provides: protocol.Provides{
		Connections: []protocol.TypeDecl{{Type: "acme", Fields: []protocol.FieldDecl{{Name: "url", Required: true}, {Name: "token", Secret: true, Help: "an API token"}}}},
		Extensions:  []protocol.ExtensionDecl{{Type: "acme"}},
		Routes:      []protocol.RouteDecl{{Handler: "acme-hold"}},
		Executors:   []protocol.ExecutorDecl{{Name: "acme-run", Fields: []protocol.FieldDecl{{Name: "command"}}}},
	}}
	got := manifestAdapters(m)
	if len(got) != 2 {
		t.Fatalf("entries %+v", got)
	}
	own, run := got[0], got[1]
	if own.Type != "acme" || !slices.Equal(own.Kinds, []string{"connection", "extension", "route:acme-hold"}) {
		t.Errorf("own entry %+v", own)
	}
	if len(own.Connection) != 1 || own.Connection[0].Name != "url" || len(own.Secrets) != 1 || !reflect.DeepEqual(own.Secrets[0], cli.Field{Name: "token", Help: "an API token"}) {
		t.Errorf("connection %+v, secrets %+v", own.Connection, own.Secrets)
	}
	if run.Type != "acme-run" || run.Title != "Acme (acme-run)" || !slices.Equal(run.Kinds, []string{"executor"}) || len(run.Binding) != 1 {
		t.Errorf("acme-run entry %+v", run)
	}
	// A package of operations alone has no config type to describe.
	if got := manifestAdapters(protocol.Manifest{Name: "ops", Provides: protocol.Provides{Operations: []protocol.OperationDecl{{Name: "ops.x"}}}}); len(got) != 0 {
		t.Errorf("an operations-only package: %+v", got)
	}
}

// A package discovery refuses is missing from the catalogue, and Adapters
// says so, so `open-conduit adapters` can report the gap: here an installed
// package built for a newer Open Conduit, which also shadows the bundled
// one of its name.
func TestAdaptersReportRefusedPackages(t *testing.T) {
	bundled := withBundled(t)
	writeManifest(t, bundled, "acme", protocol.Provides{Extensions: []protocol.ExtensionDecl{{Type: "acme"}}})
	configPath := filepath.Join(t.TempDir(), "config.json")
	if _, ok := adapter(catalogAt(t, configPath), "acme"); !ok {
		t.Fatal("the bundled acme is not in the catalogue")
	}
	dir := filepath.Join(extensionsRoot(configPath), "acme")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), []byte(`{"protocol":[1],"name":"acme","version":"9","command":["./a"],"provides":{},"from_the_future":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, errs := Adapters(configPath)
	if _, ok := adapter(catalog, "acme"); ok || len(errs) != 1 || !errors.Is(errs[0], protocol.ErrNewerManifest) {
		t.Fatalf("catalogue lists acme %v, refusals %v", ok, errs)
	}
}
