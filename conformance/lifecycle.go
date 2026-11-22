package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// The lifecycle suite proves a storage.Backend's lifecycle: open, one owner
// at a time, refusal
// of state a newer binary wrote before anything changes, a consistent
// snapshot, a restore that fences every lease, a fence in place, an
// integrity check and an inspection that changes nothing (#767). The
// storage suite (RunStorage) proves what an open Store does; this one
// proves how a backend gets one.

// Newer is what a lifecycle harness ages stored state to claim.
type Newer int

const (
	// NewerSchema claims a schema this binary does not know.
	NewerSchema Newer = iota
	// NewerRecords claims records a newer binary wrote.
	NewerRecords
)

// LifecycleHarness is what the lifecycle suite needs from a backend's own
// test beside the Backend itself: facilities only the backend knows how
// to provide.
type LifecycleHarness struct {
	Backend storage.Backend
	// Location names fresh, absent state for the backend, using dir (a
	// private temporary directory) for any files.
	Location func(t *testing.T, dir string) string
	// Age makes the closed state at location claim to have been written
	// by a newer binary, as NewerSchema or NewerRecords says.
	Age func(t *testing.T, location string, newer Newer)
	// Older makes the closed state at location claim to have been written
	// by an older binary, at a lower record format.
	Older func(t *testing.T, location string)
	// Fingerprint changes whenever anything stored at location changes.
	Fingerprint func(t *testing.T, location string) string
	// Corrupt damages the closed state at location in a way that a full
	// integrity check finds, even if opening it does not.
	Corrupt func(t *testing.T, location string)
}

// LifecycleCase is one lifecycle guarantee.
type LifecycleCase struct {
	Name  string
	Check func(*testing.T, LifecycleHarness) error
}

// RunLifecycle runs every lifecycle case against h.
func RunLifecycle(t *testing.T, h LifecycleHarness) {
	t.Helper()
	for _, c := range LifecycleCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(t, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func LifecycleCases() []LifecycleCase {
	return []LifecycleCase{
		{"open_creates_then_reopens_state", openReopens},
		{"present_reports_state_without_creating_it", presentCreatesNothing},
		{"one_owner_at_a_time", oneOwner},
		{"newer_schema_refused_before_any_change", func(t *testing.T, h LifecycleHarness) error { return refusesNewer(t, h, NewerSchema) }},
		{"newer_records_refused_before_any_change", func(t *testing.T, h LifecycleHarness) error { return refusesNewer(t, h, NewerRecords) }},
		{"inspect_reads_without_changing_state", inspectChangesNothing},
		{"open_prior_preserves_older_state_unchanged", priorUnchanged},
		{"snapshot_is_consistent_and_restores", snapshotConsistent},
		{"read_snapshot_sees_state_and_publishes_nothing", readSnapshot},
		{"restore_fences_every_lease", restoreFences},
		{"fence_ends_live_lease_authority", fenceInPlace},
		{"integrity_check_finds_corruption", integrityCheck},
		{"states_its_recovery_point", statesRecoveryPoint},
	}
}

var lifecycleNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func openSeeded(t *testing.T, h LifecycleHarness) (string, error) {
	location := h.Location(t, t.TempDir())
	db, err := h.Backend.Open(context.Background(), location)
	if err != nil {
		return location, fmt.Errorf("open absent state: %w", err)
	}
	err = seed(db, NewClock())
	return location, errors.Join(err, db.Close())
}

func hasWork(tx storage.Tx, id domain.ID) (bool, error) {
	_, err := tx.Work(id)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func viewHas(db storage.Viewer, id domain.ID) (bool, error) {
	var ok bool
	err := db.View(context.Background(), func(tx storage.Tx) (err error) { ok, err = hasWork(tx, id); return err })
	return ok, err
}

// openReopens: Open creates absent state, and the state outlives the
// Database that wrote it.
func openReopens(t *testing.T, h LifecycleHarness) error {
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	db, err := h.Backend.Open(context.Background(), location)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	defer db.Close()
	if db.SchemaVersion() == 0 {
		return fmt.Errorf("open state reports schema 0")
	}
	if ok, err := viewHas(db, "work"); err != nil || !ok {
		return fmt.Errorf("reopened state lost its records: %v", err)
	}
	return nil
}

// presentCreatesNothing: Present reports absent state as absent and leaves
// it absent, and reports state once Open has made it.
func presentCreatesNothing(t *testing.T, h LifecycleHarness) error {
	ctx := context.Background()
	location := h.Location(t, t.TempDir())
	for range 2 {
		if present, err := h.Backend.Present(ctx, location); err != nil || present {
			return fmt.Errorf("absent state reported present (%v)", err)
		}
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if present, err := h.Backend.Present(ctx, location); err != nil || !present {
		return fmt.Errorf("opened state reported absent (%v)", err)
	}
	return nil
}

// oneOwner: while a Database or an Inspection owns state, neither Open nor
// Inspect can open it again, in this process or any other; once it closes,
// they can.
func oneOwner(t *testing.T, h LifecycleHarness) error {
	ctx := context.Background()
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	second := func(holder string) error {
		if db, err := h.Backend.Open(ctx, location); !errors.Is(err, storage.ErrInUse) {
			if err == nil {
				db.Close()
			}
			return fmt.Errorf("a second owner opened state %s holds: got %v, want %v", holder, err, storage.ErrInUse)
		}
		if i, err := h.Backend.Inspect(ctx, location); !errors.Is(err, storage.ErrInUse) {
			if err == nil {
				i.Close()
			}
			return fmt.Errorf("a second owner inspected state %s holds: got %v, want %v", holder, err, storage.ErrInUse)
		}
		return nil
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	err = second("an open Database")
	if err := errors.Join(err, db.Close()); err != nil {
		return err
	}
	i, err := h.Backend.Inspect(ctx, location)
	if err != nil {
		return fmt.Errorf("inspect once the Database closed: %w", err)
	}
	err = second("an Inspection")
	if err := errors.Join(err, i.Close()); err != nil {
		return err
	}
	db, err = h.Backend.Open(ctx, location)
	if err != nil {
		return fmt.Errorf("open once every owner closed: %w", err)
	}
	return db.Close()
}

// refusesNewer: state a newer binary wrote is refused with the contract's
// error by Open and by Inspect, and neither changes it.
func refusesNewer(t *testing.T, h LifecycleHarness, newer Newer) error {
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	h.Age(t, location, newer)
	want := map[Newer]error{NewerSchema: storage.ErrSchemaNewer, NewerRecords: storage.ErrRecordsNewer}[newer]
	before := h.Fingerprint(t, location)
	if db, err := h.Backend.Open(context.Background(), location); !errors.Is(err, want) {
		if err == nil {
			db.Close()
		}
		return fmt.Errorf("open of newer state: got %v, want %v", err, want)
	}
	if after := h.Fingerprint(t, location); after != before {
		return fmt.Errorf("open changed the newer state it refused")
	}
	if i, err := h.Backend.Inspect(context.Background(), location); !errors.Is(err, want) {
		if err == nil {
			i.Close()
		}
		return fmt.Errorf("inspect of newer state: got %v, want %v", err, want)
	}
	if after := h.Fingerprint(t, location); after != before {
		return fmt.Errorf("inspect changed the newer state it refused")
	}
	return nil
}

// inspectChangesNothing: an inspection reads the records, refuses writes
// and leaves the state as it found it. Absent state is refused, not made.
func inspectChangesNothing(t *testing.T, h LifecycleHarness) error {
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	before := h.Fingerprint(t, location)
	i, err := h.Backend.Inspect(context.Background(), location)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if ok, err := viewHas(i, "work"); err != nil || !ok {
		i.Close()
		return fmt.Errorf("inspection does not read the records: %v", err)
	}
	werr := i.View(context.Background(), func(tx storage.Tx) error { return tx.Put("test", "inspected", []byte("x")) })
	if err := errors.Join(i.IntegrityCheck(context.Background()), i.Close()); err != nil {
		return err
	}
	if !errors.Is(werr, storage.ErrReadOnly) {
		return fmt.Errorf("an inspection wrote: %v", werr)
	}
	if after := h.Fingerprint(t, location); after != before {
		return fmt.Errorf("inspection changed the state")
	}
	absent := h.Location(t, t.TempDir())
	if i, err := h.Backend.Inspect(context.Background(), absent); err == nil {
		i.Close()
		return fmt.Errorf("inspection of absent state succeeded")
	}
	if db, err := h.Backend.Inspect(context.Background(), absent); err == nil {
		db.Close()
		return fmt.Errorf("inspection of absent state created it")
	}
	return nil
}

// priorUnchanged: OpenPrior finds nothing to preserve in absent or current
// state; in older state it owns the location, and snapshots it without
// changing it, until Open raises it.
func priorUnchanged(t *testing.T, h LifecycleHarness) error {
	ctx := context.Background()
	absent := h.Location(t, t.TempDir())
	if p, err := h.Backend.OpenPrior(ctx, absent); err != nil || p != nil {
		return fmt.Errorf("absent state has a prior: %v, %v", p, err)
	}
	if present, err := h.Backend.Present(ctx, absent); err != nil || present {
		return fmt.Errorf("OpenPrior created absent state (%v)", err)
	}
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	if p, err := h.Backend.OpenPrior(ctx, location); err != nil || p != nil {
		return fmt.Errorf("current state has a prior: %v, %v", p, err)
	}
	current, err := h.Backend.Inspect(ctx, location)
	if err != nil {
		return fmt.Errorf("inspect current state: %w", err)
	}
	stored, err := current.Stored(ctx)
	if cerr := current.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("stored versions: %w", err)
	}
	if writes := h.Backend.Writes(); stored != writes || writes.Records == 0 {
		return fmt.Errorf("current state is stored at %+v, and the backend writes %+v", stored, writes)
	}
	h.Older(t, location)
	before := h.Fingerprint(t, location)
	p, err := h.Backend.OpenPrior(ctx, location)
	if err != nil {
		return fmt.Errorf("open prior: %w", err)
	}
	if p == nil {
		return fmt.Errorf("older state did not report a prior")
	}
	defer p.Close()
	if db, err := h.Backend.Open(ctx, location); !errors.Is(err, storage.ErrInUse) {
		if err == nil {
			db.Close()
		}
		return fmt.Errorf("a second owner opened state a Prior holds: %v", err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	var read bool
	if err := p.Snapshot(ctx, snapshot, func(tx storage.Tx) (err error) { read, err = hasWork(tx, "work"); return err }); err != nil {
		return fmt.Errorf("prior snapshot: %w", err)
	}
	if !read {
		return fmt.Errorf("prior snapshot's read did not see the state")
	}
	if err := p.Close(); err != nil {
		return err
	}
	if after := h.Fingerprint(t, location); after != before {
		return fmt.Errorf("preserving older state changed it")
	}
	restored := h.Location(t, t.TempDir())
	if err := h.Backend.Restore(ctx, snapshot, restored, lifecycleNow); err != nil {
		return fmt.Errorf("restore prior snapshot: %w", err)
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if again, err := h.Backend.OpenPrior(ctx, location); err != nil || again != nil {
		return fmt.Errorf("state Open raised still has a prior: %v, %v", again, err)
	}
	return nil
}

// snapshotConsistent: read sees exactly the snapshot's state, a write
// committed after it is in neither, the snapshot restores to that state,
// and neither Snapshot nor Restore overwrites anything.
func snapshotConsistent(t *testing.T, h LifecycleHarness) error {
	ctx, c := context.Background(), NewClock()
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	defer db.Close()
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	var read bool
	var writeErr error
	if err := db.Snapshot(ctx, snapshot, func(tx storage.Tx) (err error) {
		read, err = hasWork(tx, "work")
		_, _, writeErr = tx.AcceptEvent(event("from-snapshot", c.Now()))
		return err
	}); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if !read {
		return fmt.Errorf("snapshot's read did not see committed state")
	}
	if !errors.Is(writeErr, storage.ErrReadOnly) {
		return fmt.Errorf("snapshot's read wrote: %v", writeErr)
	}
	if err := transact(db, c, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "later", Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady}, 0)
	}); err != nil {
		return err
	}
	if err := wantError(db.Snapshot(ctx, snapshot, nil), fs.ErrExist); err != nil {
		return fmt.Errorf("snapshot over an existing path: %w", err)
	}
	restored := h.Location(t, t.TempDir())
	if err := h.Backend.Restore(ctx, snapshot, restored, lifecycleNow); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	again, err := h.Backend.Open(ctx, restored)
	if err != nil {
		return fmt.Errorf("open restored state: %w", err)
	}
	defer again.Close()
	if ok, err := viewHas(again, "work"); err != nil || !ok {
		return fmt.Errorf("restored state lost the snapshot's records: %v", err)
	}
	if ok, err := viewHas(again, "later"); err != nil || ok {
		return fmt.Errorf("restored state holds a write committed after the snapshot: %v", err)
	}
	if err := again.View(ctx, func(tx storage.Tx) error {
		_, err := tx.Event("from-snapshot")
		return wantError(err, storage.ErrNotFound)
	}); err != nil {
		return fmt.Errorf("restored state holds the snapshot read's write: %w", err)
	}
	before := h.Fingerprint(t, location)
	if err := wantError(h.Backend.Restore(ctx, snapshot, location, lifecycleNow), fs.ErrExist); err != nil {
		return fmt.Errorf("restore over existing state: %w", err)
	}
	if h.Fingerprint(t, location) != before {
		return fmt.Errorf("a refused restore changed the existing state")
	}
	return nil
}

func leased(t *testing.T, h LifecycleHarness) (storage.Database, domain.LeaseRef, *Clock, error) {
	location, err := openSeeded(t, h)
	if err != nil {
		return nil, domain.LeaseRef{}, nil, err
	}
	db, err := h.Backend.Open(context.Background(), location)
	if err != nil {
		return nil, domain.LeaseRef{}, nil, err
	}
	c := NewClock()
	ref, err := claim(db, c)
	if err != nil {
		db.Close()
		return nil, domain.LeaseRef{}, nil, err
	}
	return db, ref, c, nil
}

// fenced: an old lease holder can no longer commit, its lease carries no
// owner or expiry, and the next Claim advances the epoch.
func fenced(db storage.Store, old domain.LeaseRef, c *Clock) error {
	var x domain.Execution
	if err := transact(db, c, func(tx storage.Tx) (err error) { x, err = tx.Execution(old.ExecutionID); return err }); err != nil {
		return err
	}
	if x.Lease.Owner != "" || !x.Lease.ExpiresAt.IsZero() {
		return fmt.Errorf("lease still held by %q until %v", x.Lease.Owner, x.Lease.ExpiresAt)
	}
	if x.Lease.Epoch != old.Epoch {
		return fmt.Errorf("fence changed the epoch from %d to %d", old.Epoch, x.Lease.Epoch)
	}
	if err := wantError(db.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &old}, func(tx storage.Tx) error { return tx.Put("test", "stale", nil) }), storage.ErrLeaseLost); err != nil {
		return fmt.Errorf("old holder after the fence: %w", err)
	}
	fresh, err := claim(db, c)
	if err != nil {
		return fmt.Errorf("claim after the fence: %w", err)
	}
	if fresh.Epoch <= old.Epoch {
		return fmt.Errorf("claim after the fence reused epoch %d", fresh.Epoch)
	}
	return nil
}

// restoreFences: a lease live when the snapshot was taken is fenced in
// the restored state.
func restoreFences(t *testing.T, h LifecycleHarness) error {
	db, ref, c, err := leased(t, h)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if err := db.Snapshot(ctx, snapshot, nil); err != nil {
		return err
	}
	restored := h.Location(t, t.TempDir())
	if err := h.Backend.Restore(ctx, snapshot, restored, lifecycleNow); err != nil {
		return err
	}
	again, err := h.Backend.Open(ctx, restored)
	if err != nil {
		return err
	}
	defer again.Close()
	return fenced(again, ref, c)
}

// readSnapshot: ReadSnapshot reads exactly the state the snapshot holds,
// passes on read's error, and leaves the snapshot as it was.
func readSnapshot(t *testing.T, h LifecycleHarness) error {
	ctx := context.Background()
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	defer db.Close()
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if err := db.Snapshot(ctx, snapshot, nil); err != nil {
		return err
	}
	before, err := os.ReadFile(snapshot)
	if err != nil {
		return err
	}
	var seen bool
	if err := h.Backend.ReadSnapshot(ctx, snapshot, lifecycleNow, func(tx storage.Tx) error {
		var err error
		seen, err = hasWork(tx, "work")
		return err
	}); err != nil {
		return err
	}
	if !seen {
		return errors.New("read snapshot: the snapshot's Work was not there")
	}
	refused := errors.New("refused by read")
	if err := h.Backend.ReadSnapshot(ctx, snapshot, lifecycleNow, func(storage.Tx) error { return refused }); !errors.Is(err, refused) {
		return fmt.Errorf("read snapshot: read's error came back as %v", err)
	}
	after, err := os.ReadFile(snapshot)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return errors.New("read snapshot: changed the snapshot")
	}
	return nil
}

// fenceInPlace: Fence ends the authority of a live lease in open state,
// as a standby taking over must.
func fenceInPlace(t *testing.T, h LifecycleHarness) error {
	db, ref, c, err := leased(t, h)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := wantError(db.Fence(context.Background(), time.Time{}), storage.ErrInvalid); err != nil {
		return fmt.Errorf("fence without a time: %w", err)
	}
	if err := db.Fence(context.Background(), c.Now()); err != nil {
		return err
	}
	return fenced(db, ref, c)
}

// statesRecoveryPoint: a backend says what it knows of its recovery point, in
// a form the contract allows, whether the state is open or closed, and saying
// so changes nothing.
func statesRecoveryPoint(t *testing.T, h LifecycleHarness) error {
	ctx := context.Background()
	stater, ok := h.Backend.(storage.RecoveryStater)
	if !ok {
		return fmt.Errorf("the backend doesn't state its recovery point")
	}
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	before := h.Fingerprint(t, location)
	point, err := stater.RecoveryPoint(ctx, location)
	if err != nil {
		return fmt.Errorf("stating the recovery point of closed state: %w", err)
	}
	if !point.Valid() {
		return fmt.Errorf("the backend stated an invalid recovery point: %+v", point)
	}
	if h.Fingerprint(t, location) != before {
		return fmt.Errorf("stating the recovery point changed the state")
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	defer db.Close()
	if point, err = stater.RecoveryPoint(ctx, location); err != nil {
		return fmt.Errorf("stating the recovery point of open state: %w", err)
	}
	if !point.Valid() {
		return fmt.Errorf("the backend stated an invalid recovery point: %+v", point)
	}
	return nil
}

// integrityCheck: healthy state passes, and once the state is damaged
// either opening it fails or the full check reports it, for an open
// Database and an inspection alike.
func integrityCheck(t *testing.T, h LifecycleHarness) error {
	ctx := context.Background()
	location, err := openSeeded(t, h)
	if err != nil {
		return err
	}
	db, err := h.Backend.Open(ctx, location)
	if err != nil {
		return err
	}
	err = db.IntegrityCheck(ctx)
	if err := errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("healthy state failed its integrity check: %w", err)
	}
	h.Corrupt(t, location)
	if i, err := h.Backend.Inspect(ctx, location); err == nil {
		err = i.IntegrityCheck(ctx)
		i.Close()
		if err == nil {
			return fmt.Errorf("an inspection's integrity check passed damaged state")
		}
	}
	if db, err := h.Backend.Open(ctx, location); err == nil {
		err = db.IntegrityCheck(ctx)
		db.Close()
		if err == nil {
			return fmt.Errorf("the integrity check passed damaged state")
		}
	}
	return nil
}
