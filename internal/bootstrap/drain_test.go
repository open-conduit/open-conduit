package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// TestDrainSurvivesAStartAnywhere: a copy of the state can't know the daemon
// it came from has stopped, so reopening it at another path keeps the drain.
func TestDrainSurvivesAStartAnywhere(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, moved := filepath.Join(root, "state"), filepath.Join(root, "elsewhere")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		_, err := runtime.BeginDrainInTx(tx, now, drainOrigin(state), "moving")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(state, moved); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, filepath.Join(moved, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(ctx, func(tx storage.Tx) error {
		d, on, err := runtime.DrainInTx(tx)
		if err == nil && (!on || d.Origin != state) {
			t.Errorf("after the move: draining %v, origin %q", on, d.Origin)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
