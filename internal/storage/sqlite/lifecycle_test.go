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
		Backend:  b,
		Location: func(t *testing.T, dir string) string { return filepath.Join(dir, "conduit.db") },
		Age:      age,
		Older: func(t *testing.T, path string) {
			raw(t, path, fmt.Sprintf("UPDATE metadata SET value='%d' WHERE key='record_format'", sqlite.RecordFormat-1))
		},
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

// unowned opens the database without owning its location.
func unowned(ctx context.Context, location string) (storage.Database, error) {
	return sqlite.Open(ctx, location)
}

type eager struct{ storage.Backend }

// Present says yes to anything.
func (eager) Present(context.Context, string) (bool, error) { return true, nil }

// blindRead reads an empty database instead of the snapshot.
type blindRead struct{ storage.Backend }

func (b blindRead) ReadSnapshot(ctx context.Context, _ string, _ time.Time, read func(storage.Tx) error) error {
	dir, err := os.MkdirTemp("", "oc-blind-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	s, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	return s.View(ctx, read)
}

// touchingRead reads the snapshot and then writes to it.
type touchingRead struct{ storage.Backend }

func (b touchingRead) ReadSnapshot(ctx context.Context, snapshot string, now time.Time, read func(storage.Tx) error) error {
	if err := b.Backend.ReadSnapshot(ctx, snapshot, now, read); err != nil {
		return err
	}
	f, err := os.OpenFile(snapshot, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte{0})
	return err
}

type unfenced struct{ storage.Database }

func (unfenced) Fence(_ context.Context, now time.Time) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	return nil
}

type unchecked struct{ storage.Database }

// raising opens the state first, as a daemon without the guard would, so
// the format is already raised when it is asked what to preserve.
type raising struct{ storage.Backend }

func (r raising) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	if present, err := r.Backend.Present(ctx, location); err != nil || !present {
		return nil, err
	}
	db, err := r.Backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return r.Backend.OpenPrior(ctx, location)
}

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
		name: "open_accepts_newer_records", failing: "newer_records_refused_before_any_change", want: "open of newer state",
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
		// Open shares the location with any other opener.
		name: "open_shares_its_location", failing: "one_owner_at_a_time", want: "a second owner opened",
		backend: weakened{Backend: sqlite.Backend, open: unowned},
	}, {
		// Present reports state that isn't there.
		name: "present_reports_absent_state", failing: "present_reports_state_without_creating_it", want: "absent state reported present",
		backend: eager{sqlite.Backend},
	}, {
		// OpenPrior raises the format before it looks.
		name: "open_prior_raises_the_format", failing: "open_prior_preserves_older_state_unchanged", want: "did not report a prior",
		backend: raising{sqlite.Backend},
	}, {
		// ReadSnapshot reads some other state.
		name: "read_snapshot_reads_other_state", failing: "read_snapshot_sees_state_and_publishes_nothing", want: "was not there",
		backend: blindRead{sqlite.Backend},
	}, {
		// ReadSnapshot changes the snapshot it reads.
		name: "read_snapshot_changes_the_snapshot", failing: "read_snapshot_sees_state_and_publishes_nothing", want: "changed the snapshot",
		backend: touchingRead{sqlite.Backend},
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
