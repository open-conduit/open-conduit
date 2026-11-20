package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func lifecycleHarness(b storage.Backend) conformance.LifecycleHarness {
	return conformance.LifecycleHarness{
		Backend:     b,
		Location:    func(t *testing.T, dir string) string { return filepath.Join(dir, "conduit.db") },
		Age:         age,
		Fingerprint: fingerprint,
		Corrupt:     corrupt,
	}
}

func raw(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func age(t *testing.T, path string, newer conformance.Newer) {
	switch newer {
	case conformance.NewerSchema:
		raw(t, path, fmt.Sprintf("PRAGMA user_version=%d", sqlite.SchemaVersion+1))
	case conformance.NewerRecords:
		raw(t, path, fmt.Sprintf("UPDATE metadata SET value='%d' WHERE key='record_format'", sqlite.RecordFormat+1))
	}
}

func fingerprint(t *testing.T, path string) string {
	h := sha256.New()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(path + suffix)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s:%d:", suffix, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// corrupt moves a correlation to another Work behind the back of its
// index: the index is hidden from the schema while the row changes, then
// put back. Every page is still in use, the row counts match and the
// foreign key holds, so Open's quick_check passes, but the full integrity
// check finds the index pointing at the old value.
func corrupt(t *testing.T, path string) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var name, table, statement string
	var root int
	if err := db.QueryRow("SELECT name, tbl_name, rootpage, sql FROM sqlite_schema WHERE name='correlation_work'").Scan(&name, &table, &root, &statement); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw(t, path, "INSERT INTO works SELECT 'other', scope_id, parent_id, record FROM works WHERE id='work'")
	raw(t, path, "PRAGMA writable_schema=ON", "DELETE FROM sqlite_schema WHERE name='correlation_work'")
	raw(t, path, "UPDATE correlations SET work_id='other'")
	raw(t, path, "PRAGMA writable_schema=ON", fmt.Sprintf("INSERT INTO sqlite_schema VALUES ('index','%s','%s',%d,'%s')", name, table, root, strings.ReplaceAll(statement, "'", "''")))
}

func TestSQLiteLifecycleConformance(t *testing.T) {
	conformance.RunLifecycle(t, lifecycleHarness(sqlite.Backend))
}

// The registry offers SQLite by name, so the composition root can select it.
func TestSQLiteIsRegistered(t *testing.T) {
	if b, err := storage.LookupBackend(sqlite.Name); err != nil || b != sqlite.Backend {
		t.Fatalf("sqlite not registered: %v", err)
	}
	if _, err := storage.LookupBackend("absent"); err == nil || !strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("unknown backend: %v", err)
	}
}

// Negative controls: each weakens one lifecycle guarantee of the real
// backend, and the suite must reject it for that reason.

type weakened struct {
	storage.Backend
	open    func(ctx context.Context, location string) (storage.Database, error)
	restore func(ctx context.Context, snapshot, destination string, now time.Time) error
	wrap    func(storage.Database) storage.Database
}

func (w weakened) Open(ctx context.Context, location string) (storage.Database, error) {
	open := w.Backend.Open
	if w.open != nil {
		open = w.open
	}
	db, err := open(ctx, location)
	if err == nil && w.wrap != nil {
		db = w.wrap(db)
	}
	return db, err
}
func (w weakened) Restore(ctx context.Context, snapshot, destination string, now time.Time) error {
	if w.restore != nil {
		return w.restore(ctx, snapshot, destination, now)
	}
	return w.Backend.Restore(ctx, snapshot, destination, now)
}

type unfenced struct{ storage.Database }

func (unfenced) Fence(_ context.Context, now time.Time) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	return nil
}

type unchecked struct{ storage.Database }

func (unchecked) IntegrityCheck(context.Context) error { return nil }

// existing opens a database file only if it is there, so a control never
// creates one.
func existing(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", path)
}

func TestWeakenedLifecycleIsRejected(t *testing.T) {
	controls := []struct {
		name, failing, want string
		backend             storage.Backend
	}{{
		// Restore copies the snapshot and leaves its leases live.
		name: "restore_leaves_a_lease_live", failing: "restore_fences_every_lease", want: "still held",
		backend: weakened{Backend: sqlite.Backend, restore: func(_ context.Context, snapshot, destination string, _ time.Time) error {
			b, err := os.ReadFile(snapshot)
			if err != nil {
				return err
			}
			if _, err := os.Lstat(destination); err == nil {
				return os.ErrExist
			}
			return os.WriteFile(destination, b, 0o600)
		}},
	}, {
		// Open lowers the stored record format to its own, and so accepts
		// records a newer binary wrote.
		name: "open_accepts_newer_records", failing: "newer_records_refused_before_any_change", want: "Open of newer state",
		backend: weakened{Backend: sqlite.Backend, open: func(ctx context.Context, location string) (storage.Database, error) {
			if db, err := existing(location); err == nil {
				db.Exec(fmt.Sprintf("UPDATE metadata SET value='%d' WHERE key='record_format'", sqlite.RecordFormat))
				db.Close()
			}
			return sqlite.Backend.Open(ctx, location)
		}},
	}, {
		// Open writes before it checks, so the state it refuses has changed.
		name: "open_changes_state_before_refusing", failing: "newer_schema_refused_before_any_change", want: "changed the newer state",
		backend: weakened{Backend: sqlite.Backend, open: func(ctx context.Context, location string) (storage.Database, error) {
			if db, err := existing(location); err == nil {
				db.Exec("INSERT OR REPLACE INTO metadata VALUES ('opened_by','older')")
				db.Close()
			}
			return sqlite.Backend.Open(ctx, location)
		}},
	}, {
		// Fence does nothing.
		name: "fence_leaves_a_lease_live", failing: "fence_ends_live_lease_authority", want: "still held",
		backend: weakened{Backend: sqlite.Backend, wrap: func(db storage.Database) storage.Database { return unfenced{db} }},
	}, {
		// The integrity check trusts what Open's quick check found.
		name: "integrity_check_passes_anything", failing: "integrity_check_finds_corruption", want: "passed damaged state",
		backend: weakened{Backend: sqlite.Backend, wrap: func(db storage.Database) storage.Database { return unchecked{db} }},
	}}
	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			h := lifecycleHarness(control.backend)
			for _, c := range conformance.LifecycleCases() {
				if c.Name != control.failing {
					continue
				}
				err := c.Check(t, h)
				if err == nil || !strings.Contains(err.Error(), control.want) {
					t.Fatalf("%s: got %v, want an error containing %q", c.Name, err, control.want)
				}
				return
			}
			t.Fatalf("no case %s", control.failing)
		})
	}
}
