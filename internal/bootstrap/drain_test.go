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

// Only the state directory's path is evidence of a move. A daemon recreated
// with a new hostname (a container, a rescheduled pod) has the same origin, as
// does one reaching the directory through a symlink, so both stay drained. A
// start from another path ends the drain, the negative control.
func TestDrainOriginIsTheStatePathAlone(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	if got := drainOrigin(state); got != state {
		t.Fatalf("origin %q holds more than the state path %q", got, state)
	}
	store, err := sqlite.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		_, err := runtime.BeginDrainInTx(tx, now, drainOrigin(state), "moving")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, same := range []string{state, link} {
		if ended, err := runtime.EndMovedDrain(ctx, store, now, drainOrigin(same)); err != nil || ended != nil {
			t.Fatalf("start at %s ended the drain: %+v, %v", same, ended, err)
		}
	}
	if ended, err := runtime.EndMovedDrain(ctx, store, now, drainOrigin(filepath.Join(root, "elsewhere"))); err != nil || ended == nil {
		t.Fatalf("a start from another path: %+v, %v", ended, err)
	}
}
