package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
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
		CommitOutside: func(t *testing.T, path string) {
			raw(t, path, "PRAGMA busy_timeout=5000", "INSERT OR REPLACE INTO kv VALUES ('lifecycle','outside',x'00')")
		},
		ExpireInstance: func(t *testing.T, path string) {
			raw(t, path, "PRAGMA busy_timeout=5000", "UPDATE instance_lease SET expires_at=1 WHERE expires_at IS NOT NULL")
		},
	}
}

// staleInstance is a database that doesn't compare a writer's reference: every
// write runs as the latest holder, or as none when nameless is set.
type staleInstance struct {
	storage.Database
	leaser   storage.InstanceLeaser
	latest   *storage.InstanceRef
	nameless bool
}

func (s *staleInstance) AcquireInstance(ctx context.Context, owner string, ttl time.Duration) (storage.InstanceRef, error) {
	ref, err := s.leaser.AcquireInstance(ctx, owner, ttl)
	if err == nil {
		s.latest = &ref
	}
	return ref, err
}
func (s *staleInstance) RenewInstance(ctx context.Context, ref storage.InstanceRef, ttl time.Duration) error {
	return s.leaser.RenewInstance(ctx, ref, ttl)
}
func (s *staleInstance) ReleaseInstance(ctx context.Context, ref storage.InstanceRef) error {
	return s.leaser.ReleaseInstance(ctx, ref)
}
func (s *staleInstance) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	if (g.Instance != nil && !s.nameless) || (g.Instance == nil && s.nameless) {
		g.Instance = s.latest
	}
	return s.Database.Transact(ctx, g, fn)
}

// acquiresNewer takes state stored at newer versions than it writes.
type acquiresNewer struct {
	storage.Database
	leaser storage.InstanceLeaser
}

func (a acquiresNewer) AcquireInstance(ctx context.Context, owner string, ttl time.Duration) (storage.InstanceRef, error) {
	ref, err := a.leaser.AcquireInstance(ctx, owner, ttl)
	if errors.Is(err, storage.ErrRecordsNewer) || errors.Is(err, storage.ErrSchemaNewer) {
		return storage.InstanceRef{Owner: owner, Epoch: 99}, nil
	}
	return ref, err
}
func (a acquiresNewer) RenewInstance(ctx context.Context, ref storage.InstanceRef, ttl time.Duration) error {
	return a.leaser.RenewInstance(ctx, ref, ttl)
}
func (a acquiresNewer) ReleaseInstance(ctx context.Context, ref storage.InstanceRef) error {
	return a.leaser.ReleaseInstance(ctx, ref)
}

// priorIgnoresHolder preserves state a live instance is still changing.
type priorIgnoresHolder struct{ storage.Backend }

func (p priorIgnoresHolder) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	if db, err := existing(location); err == nil {
		db.Exec("UPDATE instance_lease SET expires_at=NULL")
		db.Close()
	}
	return p.Backend.OpenPrior(ctx, location)
}

// blindRaise raises what it preserved without comparing the write position.
type blindRaise struct{ storage.Backend }

type blindPrior struct {
	storage.Prior
	location string
}

func (b blindRaise) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	p, err := b.Backend.OpenPrior(ctx, location)
	if p == nil || err != nil {
		return p, err
	}
	return blindPrior{p, location}, nil
}

func (p blindPrior) Raise(context.Context) error {
	db, err := existing(p.location)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(fmt.Sprintf("PRAGMA busy_timeout=5000; UPDATE metadata SET value='%d' WHERE key='record_format'", sqlite.RecordFormat))
	return err
}

// A rewrite waits for a live holder as the format raise does: the holder would
// go on writing records in the shape the rewrite replaced.
func TestOpenLeavesRewritesToALiveHolder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, err := sqlite.Backend.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := db.(storage.InstanceLeaser).AcquireInstance(ctx, "old", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw(t, path, "DELETE FROM metadata WHERE key='rewrite_turn_schema'")
	before := fingerprint(t, path)
	if db, err := sqlite.Backend.Open(ctx, path); !errors.Is(err, storage.ErrInstanceHeld) {
		if err == nil {
			db.Close()
		}
		t.Fatalf("open with a rewrite pending under a live holder: %v", err)
	}
	if after := fingerprint(t, path); after != before {
		t.Fatal("refusing the rewrite changed the state")
	}
	raw(t, path, "UPDATE instance_lease SET expires_at=1")
	db, err = sqlite.Backend.Open(ctx, path)
	if err != nil {
		t.Fatalf("open after the holder expired: %v", err)
	}
	defer db.Close()
	if err := db.(storage.InstanceLeaser).RenewInstance(ctx, old, time.Hour); !errors.Is(err, storage.ErrInstanceFenced) {
		t.Fatalf("the expired holder renewed over the rewrite: %v", err)
	}
	var done int
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow("SELECT count(*) FROM metadata WHERE key='rewrite_turn_schema'").Scan(&done); err != nil || done != 1 {
		t.Fatalf("the rewrite did not run: %d, %v", done, err)
	}
}

// stealing takes a held lease by expiring it first.
type stealing struct {
	storage.Database
	leaser storage.InstanceLeaser
	expire func() error
}

func (s stealing) AcquireInstance(ctx context.Context, owner string, ttl time.Duration) (storage.InstanceRef, error) {
	if err := s.expire(); err != nil {
		return storage.InstanceRef{}, err
	}
	return s.leaser.AcquireInstance(ctx, owner, ttl)
}
func (s stealing) RenewInstance(ctx context.Context, ref storage.InstanceRef, ttl time.Duration) error {
	return s.leaser.RenewInstance(ctx, ref, ttl)
}
func (s stealing) ReleaseInstance(ctx context.Context, ref storage.InstanceRef) error {
	return s.leaser.ReleaseInstance(ctx, ref)
}

// holderBlindFence ends whatever tenure is recorded before it fences, as a
// Fence that doesn't check the lease would.
type holderBlindFence struct {
	storage.Database
	storage.InstanceLeaser
	location string
}

func (f holderBlindFence) Fence(ctx context.Context, now time.Time) error {
	conn, err := sql.Open("sqlite", f.location)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.Exec("UPDATE instance_lease SET expires_at=NULL"); err != nil {
		return err
	}
	return f.Database.Fence(ctx, now)
}

// sharedOpen poses as a backend whose location several hosts reach: it opens
// without taking ownership.
type sharedOpen struct{ storage.Backend }

func (sharedOpen) SharesLocation(string) bool { return true }
func (sharedOpen) Open(ctx context.Context, location string) (storage.Database, error) {
	return sqlite.Open(ctx, location)
}
func (sharedOpen) Inspect(ctx context.Context, location string) (storage.Inspection, error) {
	return sqlite.Open(ctx, location)
}
func (sharedOpen) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	return sqlite.OpenPriorUnowned(ctx, location)
}

// sharedButOwned says its location is shared and still refuses a second opener.
type sharedButOwned struct{ storage.Backend }

func (sharedButOwned) SharesLocation(string) bool { return true }

// sharedNoLease is shared, with a database that has no instance lease.
type sharedNoLease struct{ sharedOpen }

func (b sharedNoLease) Open(ctx context.Context, location string) (storage.Database, error) {
	db, err := b.sharedOpen.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return unchecked{db}, nil
}

// A shared location admits a standby where any other refuses a second owner.
func TestSharedLocationAdmitsAStandby(t *testing.T) {
	h := lifecycleHarness(sharedOpen{sqlite.Backend})
	for _, c := range conformance.LifecycleCases() {
		if c.Name == "one_owner_at_a_time" {
			if err := c.Check(t, h); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("no case one_owner_at_a_time")
}

// stealingBackend opens a database whose acquisition ignores a live holder.
func stealingBackend() storage.Backend {
	return weakened{Backend: sqlite.Backend, open: func(ctx context.Context, location string) (storage.Database, error) {
		db, err := sqlite.Backend.Open(ctx, location)
		if err != nil {
			return nil, err
		}
		return stealing{Database: db, leaser: db.(storage.InstanceLeaser), expire: func() error {
			conn, err := sql.Open("sqlite", location)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = conn.Exec("UPDATE instance_lease SET expires_at=1 WHERE expires_at IS NOT NULL")
			return err
		}}, nil
	}}
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

// silent has every Backend method and states no recovery point.
type silent struct{ storage.Backend }

// overclaiming says it sees a copy that is a negative time behind.
type overclaiming struct{ storage.Backend }

func (overclaiming) RecoveryPoint(context.Context, string) (storage.RecoveryPoint, error) {
	return storage.RecoveryPoint{Replication: storage.ReplicationObserved, Lag: -time.Second, Detail: "a replica"}, nil
}

// scribbling writes to the state it is asked about.
type scribbling struct{ storage.Backend }

func (scribbling) RecoveryPoint(_ context.Context, location string) (storage.RecoveryPoint, error) {
	db, err := existing(location)
	if err != nil {
		return storage.RecoveryPoint{}, err
	}
	defer db.Close()
	if _, err := db.Exec("INSERT OR REPLACE INTO metadata VALUES ('asked','yes')"); err != nil {
		return storage.RecoveryPoint{}, err
	}
	return storage.RecoveryPoint{Replication: storage.ReplicationNone, Detail: "none"}, nil
}

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
	}, {
		// The backend says nothing about its recovery point.
		name: "recovery_point_unstated", failing: "states_its_recovery_point", want: "doesn't state",
		backend: silent{sqlite.Backend},
	}, {
		// The statement isn't one the contract allows.
		name: "recovery_point_invalid", failing: "states_its_recovery_point", want: "invalid recovery point",
		backend: overclaiming{sqlite.Backend},
	}, {
		// Asking changes the state.
		name: "recovery_point_changes_state", failing: "states_its_recovery_point", want: "changed the state",
		backend: scribbling{sqlite.Backend},
	}, {
		// The database has no instance lease at all.
		name: "instance_lease_missing", failing: "instance_lease_admits_one_holder", want: "no instance lease",
		backend: weakened{Backend: sqlite.Backend, wrap: func(db storage.Database) storage.Database { return unchecked{db} }},
	}, {
		// A shared location that still refuses a standby at Open.
		name: "shared_location_refuses_a_standby", failing: "one_owner_at_a_time", want: "a standby could not open",
		backend: sharedButOwned{sqlite.Backend},
	}, {
		// A shared location with nothing to keep one writer.
		name: "shared_location_without_a_lease", failing: "one_owner_at_a_time", want: "no instance lease",
		backend: sharedNoLease{sharedOpen{sqlite.Backend}},
	}, {
		// Open raises the stored format under a live holder.
		name: "upgrade_under_a_live_holder", failing: "instance_upgrade_waits_for_the_holder", want: "raised under a live holder",
		backend: weakened{Backend: sqlite.Backend, open: func(ctx context.Context, location string) (storage.Database, error) {
			if db, err := existing(location); err == nil {
				db.Exec("UPDATE instance_lease SET expires_at=NULL WHERE (SELECT CAST(value AS INTEGER) FROM metadata WHERE key='record_format')<" + fmt.Sprint(sqlite.RecordFormat))
				db.Close()
			}
			return sqlite.Backend.Open(ctx, location)
		}},
	}, {
		// OpenPrior preserves state a live holder is still changing.
		name: "prior_under_a_live_holder", failing: "instance_upgrade_waits_for_the_holder", want: "preserved under a live holder",
		backend: priorIgnoresHolder{sqlite.Backend},
	}, {
		// Raise doesn't look for commits since the state was preserved.
		name: "raise_over_a_commit", failing: "instance_raise_refuses_moved_state", want: "raised over a commit",
		backend: blindRaise{sqlite.Backend},
	}, {
		// A standby takes state a newer binary has raised.
		name: "instance_acquires_newer_state", failing: "instance_refuses_newer_state", want: "acquired state",
		backend: weakened{Backend: sqlite.Backend, wrap: func(db storage.Database) storage.Database {
			return acquiresNewer{Database: db, leaser: db.(storage.InstanceLeaser)}
		}},
	}, {
		// A second instance takes a lease that hasn't expired.
		name: "instance_lease_stolen", failing: "instance_lease_admits_one_holder", want: "acquired a held lease",
		backend: stealingBackend(),
	}, {
		// A write that names no holder is let through as the holder.
		name: "instance_write_unnamed", failing: "instance_lease_admits_one_holder", want: "a write naming no holder",
		backend: weakened{Backend: sqlite.Backend, wrap: func(db storage.Database) storage.Database {
			return &staleInstance{Database: db, leaser: db.(storage.InstanceLeaser), nameless: true}
		}},
	}, {
		// Fence ends a live successor's tenure without naming it.
		name: "instance_fence_ignores_the_holder", failing: "instance_fence_refused_under_a_holder", want: "a Fence under a live holder",
		backend: weakened{Backend: sqlite.Backend, open: func(ctx context.Context, location string) (storage.Database, error) {
			db, err := sqlite.Backend.Open(ctx, location)
			if err != nil {
				return nil, err
			}
			return holderBlindFence{Database: db, InstanceLeaser: db.(storage.InstanceLeaser), location: location}, nil
		}},
	}, {
		// Restore copies the snapshot with its holder still recorded.
		name: "instance_restore_keeps_the_tenure", failing: "instance_restore_ends_the_tenure", want: "still holds the snapshot's tenure",
		backend: weakened{Backend: sqlite.Backend, restore: func(_ context.Context, snapshot, destination string, _ time.Time) error {
			b, err := os.ReadFile(snapshot)
			if err != nil {
				return err
			}
			return os.WriteFile(destination, b, 0o600)
		}},
	}, {
		// A stale epoch commits on a control-plane transaction.
		name: "instance_stale_epoch_commits", failing: "instance_takeover_fences_the_old_holder", want: "the old holder's write",
		backend: weakened{Backend: sqlite.Backend, wrap: func(db storage.Database) storage.Database {
			return &staleInstance{Database: db, leaser: db.(storage.InstanceLeaser)}
		}},
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
