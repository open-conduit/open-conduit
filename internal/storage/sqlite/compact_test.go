package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// fillAndFree writes n large KV records and deletes them, leaving free pages.
func fillAndFree(t *testing.T, s *Store, n int) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	value := []byte(strings.Repeat("x", 64<<10))
	key := func(i int) string { return "key-" + time.Duration(i).String() }
	for i := range n {
		if err := s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.Put("fixture", key(i), value) }); err != nil {
			t.Fatal(err)
		}
	}
	for i := range n {
		if err := s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.Delete("fixture", key(i)) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.checkpoint(ctx, "TRUNCATE"); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestANewDatabaseReturnsFreeSpaceWhileInUse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillAndFree(t, s, 64)
	before, err := s.Space(ctx)
	if err != nil || !before.Incremental || before.FreeBytes < 3<<20 {
		t.Fatalf("space = %+v, %v", before, err)
	}
	size := fileSize(t, path)
	const ask = 1 << 20
	returned, err := s.Reclaim(ctx, ask)
	if err != nil || returned < ask-4096 || returned > ask {
		t.Fatalf("returned %d, %v, asked %d", returned, err, ask)
	}
	if got := fileSize(t, path); got > size-ask+4096*8 {
		t.Fatalf("file went from %d to %d", size, got)
	}
	if _, err := s.Reclaim(ctx, before.FreeBytes); err != nil {
		t.Fatal(err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

// A database from before incremental mode can't return space until a
// rebuild, which keeps its records.
func TestRebuildMakesAnOlderDatabaseIncremental(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.ExecContext(ctx, "PRAGMA auto_vacuum=0"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "VACUUM"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	if err := s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.Put("fixture", "kept", []byte("record")) }); err != nil {
		t.Fatal(err)
	}
	fillAndFree(t, s, 64)
	before, err := s.Space(ctx)
	if err != nil || before.Incremental || before.FreeBytes < 3<<20 {
		t.Fatalf("older database: %+v, %v", before, err)
	}
	// Negative control: without the mode, Reclaim returns nothing.
	if returned, err := s.Reclaim(ctx, before.FreeBytes); err != nil || returned != 0 {
		t.Fatalf("reclaim on an older database: %d, %v", returned, err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.Space(ctx)
	if err != nil || !after.Incremental || after.FreeBytes != 0 || after.TotalBytes >= before.TotalBytes {
		t.Fatalf("after rebuild: %+v, %v (before %+v)", after, err, before)
	}
	if got := fileSize(t, path); got >= before.TotalBytes {
		t.Fatalf("file is %d, was %d", got, before.TotalBytes)
	}
	if err := s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		v, err := tx.Get("fixture", "kept")
		if err == nil && string(v) != "record" {
			t.Fatalf("record = %q", v)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
}
