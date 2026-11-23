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
