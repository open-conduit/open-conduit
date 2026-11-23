package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// sharedStateUsing is shared state whose stored generation uses an extension
// type no bundled package provides.
func sharedStateUsing(t *testing.T, extensionType string) string {
	t.Helper()
	ctx := context.Background()
	state, path := sharedState(t)
	doc, err := json.Marshal(map[string]any{"version": 1, "settings": map[string]any{"retry_delays_seconds": []int{1}}, "connections": []any{},
		"scopes": []any{map[string]any{"id": "s", "name": "s",
			"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}},
			"bindings":      []any{}, "routes": []any{}, "secret_refs": map[string]any{},
			"extensions": []any{map[string]any{"name": "x", "type": extensionType, "secret_names": []any{}, "config": map[string]any{}}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		if err := tx.Put("core.config", "active", []byte(`{"Generation":1}`)); err != nil {
			return err
		}
		return tx.Put("core.config", "generation/1", doc)
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

// A package that is here but refused needs repairing on this host, so the
// refusal says why and doesn't offer to replace the config for every host. A
// package that is simply absent, the control, is offered that way out.
func TestRefusalTellsARefusedPackageFromAMissingOne(t *testing.T) {
	ctx := context.Background()
	absent := upgradeDaemon(sharedStateUsing(t, "notes"), time.Now())
	absent.o.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	err := absent.refuseUnservableConfig(ctx)
	if !errors.Is(err, management.ErrStoredConfigRefused) || !strings.Contains(err.Error(), "it uses notes") || !strings.Contains(err.Error(), "--adopt-config 1") {
		t.Fatalf("a missing package: %v", err)
	}

	broken := upgradeDaemon(sharedStateUsing(t, "notes"), time.Now())
	broken.o.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	dir := filepath.Join(extensionsRoot(broken.o.ConfigPath), "notes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocol.ManifestFile), []byte("not a manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = broken.refuseUnservableConfig(ctx)
	if !errors.Is(err, management.ErrStoredConfigRefused) {
		t.Fatalf("a refused package: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "refused") || !strings.Contains(msg, "notes: ") || strings.Contains(msg, "doesn't have") || strings.Contains(msg, "--adopt-config") {
		t.Fatalf("the refusal of a package that is here: %s", msg)
	}
}

// installPackage writes a package providing an extension type beside config.
func installPackage(t *testing.T, config, name, extensionType string) {
	t.Helper()
	dir := filepath.Join(extensionsRoot(config), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(protocol.Manifest{Protocol: []int{protocol.Version}, Name: name, Version: "1.0.0", Command: []string{name + "-extension"},
		Provides: protocol.Provides{Extensions: []protocol.ExtensionDecl{{Type: extensionType}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, protocol.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, name+"-extension"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// Packages that conflict are all refused when the daemon catalogues them, so
// the early check must not count them as providing what the config uses. One
// package alone, the control, serves the same generation.
func TestRefusalOfConflictingPackages(t *testing.T) {
	ctx := context.Background()
	alone := upgradeDaemon(sharedStateUsing(t, "alpha-x"), time.Now())
	alone.o.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	installPackage(t, alone.o.ConfigPath, "alpha", "alpha-x")
	if err := alone.refuseUnservableConfig(ctx); err != nil {
		t.Fatalf("control: one package providing the type: %v", err)
	}

	both := upgradeDaemon(sharedStateUsing(t, "alpha-x"), time.Now())
	both.o.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	installPackage(t, both.o.ConfigPath, "alpha", "alpha-x")
	// A type must start with its package's name, and this one does for both.
	installPackage(t, both.o.ConfigPath, "alpha-x", "alpha-x")
	err := both.refuseUnservableConfig(ctx)
	if !errors.Is(err, management.ErrStoredConfigRefused) {
		t.Fatalf("conflicting packages: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "registration_conflict") || !strings.Contains(msg, "alpha: ") || !strings.Contains(msg, "alpha-x: ") || strings.Contains(msg, "--adopt-config") {
		t.Fatalf("the refusal of conflicting packages: %s", msg)
	}
}
