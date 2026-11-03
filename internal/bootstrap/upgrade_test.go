package bootstrap

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// adoptLegacyKV moves claude-code's in-process continuity into the
// package's namespace once: a record already there wins, the old namespace
// ends empty, and a second run moves nothing.
func TestAdoptLegacyKV(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	put := func(ns, key, value string) {
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.Put(ns, key, []byte(value)) }); err != nil {
			t.Fatal(err)
		}
	}
	put("executor.claudecode", "work/a", "old-a")
	put("executor.claudecode", "work/b", "old-b")
	put("claude-code", "work/b", "new-b")
	put("executor.other", "work/c", "untouched")
	moved, err := adoptLegacyKV(ctx, store, now)
	if err != nil || moved != 1 {
		t.Fatalf("moved %d: %v, want 1", moved, err)
	}
	read := func(ns string) map[string]string {
		out := map[string]string{}
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			entries, err := tx.List(ns, "")
			for _, e := range entries {
				out[e.Key] = string(e.Value)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read("claude-code"); len(got) != 2 || got["work/a"] != "old-a" || got["work/b"] != "new-b" {
		t.Errorf("claude-code holds %v", got)
	}
	if got := read("executor.claudecode"); len(got) != 0 {
		t.Errorf("executor.claudecode still holds %v", got)
	}
	if got := read("executor.other"); got["work/c"] != "untouched" {
		t.Errorf("another namespace changed: %v", got)
	}
	if moved, err := adoptLegacyKV(ctx, store, now); err != nil || moved != 0 {
		t.Fatalf("second run moved %d: %v", moved, err)
	}
}
