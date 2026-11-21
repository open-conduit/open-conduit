package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// TestWALPolicyIsSetOnEveryConnection reads the policy back from the writer
// and from a reader, since both pragmas belong to the connection.
func TestWALPolicyIsSetOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, read := range map[string]func(string, any) error{
		"writer": func(q string, v any) error { return s.db.QueryRowContext(ctx, q).Scan(v) },
		"reader": func(q string, v any) error { return s.readers.QueryRowContext(ctx, q).Scan(v) },
	} {
		var pages, limit int64
		if err := read("PRAGMA wal_autocheckpoint", &pages); err != nil {
			t.Fatal(err)
		}
		if err := read("PRAGMA journal_size_limit", &limit); err != nil {
			t.Fatal(err)
		}
		if pages != walAutocheckpoint || limit != walSizeLimit {
			t.Errorf("%s: wal_autocheckpoint=%d journal_size_limit=%d, want %d and %d", name, pages, limit, walAutocheckpoint, walSizeLimit)
		}
	}
}

// TestWALSizeLimitReclaimsALargeWrite shows the limit cutting the file back
// after a large transaction. No limit is the negative control: the same
// writes leave the WAL at its largest.
func TestWALSizeLimitReclaimsALargeWrite(t *testing.T) {
	const limit = 64 << 10
	limited := walAfterLargeWrite(t, limit)
	unlimited := walAfterLargeWrite(t, -1)
	if limited > limit {
		t.Errorf("WAL is %d bytes after reuse, want at most the %d limit", limited, limit)
	}
	if unlimited <= limit {
		t.Errorf("negative control: WAL with no limit is %d bytes, want more than %d", unlimited, limit)
	}
}

// walAfterLargeWrite commits about 2 MB in one transaction, checkpoints, and
// commits once more so the WAL is reused, then returns the WAL file's size.
func walAfterLargeWrite(t *testing.T, limit int64) int64 {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := open(ctx, path, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	guard := storage.Guard{Now: time.Unix(1, 0)}
	value := make([]byte, 8<<10)
	if err = s.Transact(ctx, guard, func(tx storage.Tx) error {
		for i := range 256 {
			if e := tx.Put("test.wal", fmt.Sprint(i), value); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(ctx, guard, func(tx storage.Tx) error { return tx.Put("test.wal", "next", []byte("x")) }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
