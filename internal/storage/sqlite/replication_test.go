package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// replicatorView is what a WAL shipper needs of the database beside the
// daemon: its own connection, holding a read transaction so that no
// checkpoint can restart the WAL past frames it hasn't copied.
type replicatorView struct {
	db *sql.DB
	tx *sql.Tx
}

func watchAsReplicator(ctx context.Context, path string) (*replicatorView, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err == nil {
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err == nil {
			return &replicatorView{db: db, tx: tx}, nil
		}
		_ = tx.Rollback()
	}
	db.Close()
	return nil, err
}

func (r *replicatorView) close() {
	_ = r.tx.Rollback()
	r.db.Close()
}

func walSalts(t *testing.T, path string) [8]byte {
	t.Helper()
	f, err := os.Open(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var salts [8]byte
	if _, err = f.ReadAt(salts[:], 16); err != nil {
		t.Fatal(err)
	}
	return salts
}

// churn runs everything the daemon does to the file while serving: commits,
// freeing pages and returning them to the disk.
func churn(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	guard := storage.Guard{Now: time.Unix(1, 0)}
	value := make([]byte, 4<<10)
	if err := s.Transact(ctx, guard, func(tx storage.Tx) error {
		for i := range 128 {
			if err := tx.Put("test.replication", fmt.Sprint(i), value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, guard, func(tx storage.Tx) error {
		for i := range 96 {
			if err := tx.Delete("test.replication", fmt.Sprint(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reclaim(ctx, 256<<10); err != nil {
		t.Fatal(err)
	}
}

// TestLiveDatabaseKeepsTheWALForAReplicator pins what a WAL shipper relies
// on: with its read transaction open, the daemon's writes, Reclaim and Rebuild
// all go on and the WAL is never restarted under it.
func TestLiveDatabaseKeepsTheWALForAReplicator(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(tx storage.Tx) error {
		return tx.Put("test.replication", "first", []byte("x"))
	}); err != nil {
		t.Fatal(err)
	}
	r, err := watchAsReplicator(ctx, path)
	if err != nil {
		t.Fatalf("a replicator can't read beside the daemon: %v", err)
	}
	defer r.close()
	before := walSalts(t, path)
	churn(t, ctx, s)
	if err = s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if after := walSalts(t, path); after != before {
		t.Fatalf("the WAL restarted under an open reader: salts %x, then %x", before, after)
	}
}

// TestRebuildLeavesTheWALInPlace fails on a TRUNCATE checkpoint, which empties
// the file when no reader happens to be in its way. Running one is the
// negative control.
func TestRebuildLeavesTheWALInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	churn(t, ctx, s)
	if err = s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("Rebuild truncated the live WAL")
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(path + "-wal"); err != nil || info.Size() != 0 {
		t.Fatalf("negative control: a TRUNCATE checkpoint left the WAL at %v (%v)", info.Size(), err)
	}
}

// TestExclusiveLockingShutsOutAReplicator is the negative control for the
// replicator's view: a store holding its file exclusively must fail it.
func TestExclusiveLockingShutsOutAReplicator(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	if err = s.db.QueryRowContext(ctx, "PRAGMA locking_mode=EXCLUSIVE").Scan(&mode); err != nil || mode != "exclusive" {
		t.Fatalf("locking_mode %q: %v", mode, err)
	}
	if err = s.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(tx storage.Tx) error {
		return tx.Put("test.replication", "first", []byte("x"))
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := watchAsReplicator(ctx, path); err == nil {
		r.close()
		t.Fatal("a replicator could read a database held exclusively")
	}
}

// TestNewDatabaseIsNeverVisibleEmpty stands in for a replicator that adds its
// tables to any database it finds: the file only appears with the schema in
// it, and a later open accepts tables it doesn't own beside it. An empty file
// given those tables is the negative control, which Open refuses.
func TestNewDatabaseIsNeverVisibleEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "conduit.db")
	if err := createDatabase(ctx, path); err != nil {
		t.Fatal(err)
	}
	addForeignTable := func(p string) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err = db.ExecContext(ctx, "CREATE TABLE replicator_bookkeeping (id INTEGER)"); err != nil {
			t.Fatal(err)
		}
	}
	addForeignTable(path)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("a published database with a replicator's table: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".conduit-new-*")); len(left) != 0 {
		t.Fatalf("staging files left: %v", left)
	}
	empty := filepath.Join(dir, "empty.db")
	if err = os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	addForeignTable(empty)
	if s, err := Open(ctx, empty); err == nil {
		s.Close()
		t.Fatal("negative control: Open accepted an unversioned database with tables")
	}
}
