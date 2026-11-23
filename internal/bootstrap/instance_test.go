package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const sharedTestBackend = "shared-test"

// sharedSQLite stands in for a database server: its location is shared, so
// Open takes no exclusive ownership and only the lease keeps one writer.
type sharedSQLite struct{ storage.Backend }

func (sharedSQLite) SharesLocation(string) bool { return true }
func (sharedSQLite) Open(ctx context.Context, location string) (storage.Database, error) {
	return sqlite.Open(ctx, location)
}

func (sharedSQLite) Inspect(ctx context.Context, location string) (storage.Inspection, error) {
	return sqlite.InspectUnowned(ctx, location)
}

func (sharedSQLite) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	return sqlite.OpenPriorUnowned(ctx, location)
}

func (sharedSQLite) ViewInPlace(ctx context.Context, location string, read func(storage.Tx) error) error {
	return sqlite.ViewUnowned(ctx, location, read)
}

func init() { storage.RegisterBackend(sharedTestBackend, sharedSQLite{sqlite.Backend}) }

// sharedState is a state directory on the shared backend, and its database.
func sharedState(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storageFile), []byte(`{"backend":"`+sharedTestBackend+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, db, err := openState(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "conduit.db")
}

func putKey(ctx context.Context, s storage.Store) error {
	return s.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		return tx.Put("instance-test", "key", []byte("x"))
	})
}

// crash takes the lease as owner and closes without releasing it.
func crash(t *testing.T, path, owner string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.AcquireInstance(ctx, owner, time.Hour); err != nil {
		t.Fatal(err)
	}
}

// State only this host can reach keeps Open's single owner: holding the lease
// there would make an idle daemon write for nothing.
func TestUnsharedStateIsNotHeld(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	_, db, err := openState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := db.(*heldDatabase); held {
		t.Fatal("unshared state was held under the instance lease")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	direct, err := sqlite.Open(ctx, filepath.Join(dir, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if err := putKey(ctx, direct); err != nil {
		t.Fatalf("a write naming no holder: %v", err)
	}
}

// noLease is a shared Database without an instance lease.
type noLease struct{ storage.Database }

// Nothing would keep two instances apart on shared state with no lease.
func TestSharedStateWithoutALeaseIsRefused(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := holdInstance(ctx, noLease{db}, "me", instanceTTL, nil); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("shared state without a lease: %v", err)
	}
}

// The owner survives a restart, and never follows the directory to a host
// that shares its name and path, or to another host at all.
func TestInstanceOwnerIsThisCopysOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := ownerOn(dir, "node")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := ownerOn(dir, "node"); err != nil || again != first {
		t.Fatalf("restart: owner %q, want %q (%v)", again, first, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "private", instanceFile))
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(copied, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copied, "private", instanceFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if other, err := ownerOn(copied, "node"); err != nil || other == first {
		t.Fatalf("a copy on a host of the same name kept the owner %q (%v)", other, err)
	}
	if other, err := ownerOn(dir, "renamed"); err != nil || strings.HasSuffix(other, strings.TrimPrefix(first, "node")) {
		t.Fatalf("another host kept the ID: %q (%v)", other, err)
	}
}

// viewCounting records reads through View and refuses any Transact.
type viewCounting struct {
	storage.Database
	views int
}

func (v *viewCounting) View(ctx context.Context, fn func(storage.Tx) error) error {
	v.views++
	return v.Database.View(ctx, fn)
}
func (v *viewCounting) Transact(context.Context, storage.Guard, func(storage.Tx) error) error {
	return errors.New("a read became a write transaction")
}

// A held database reads through the backend's View, never a write transaction.
func TestHeldDatabaseReadsThroughView(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spy := &viewCounting{Database: db}
	held := &heldDatabase{Database: spy}
	if err := storage.View(ctx, held, time.Now(), func(storage.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if spy.views != 1 {
		t.Fatalf("reached the backend's View %d times", spy.views)
	}
}

// Closing gives the lease up, so unheld state takes a write naming no holder.
func TestOpenStateReleasesOnClose(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := putKey(ctx, db); err != nil {
		t.Fatalf("write after a clean close: %v", err)
	}
}

// An offline command after this host's daemon crashed takes over its tenure.
func TestOpenStateTakesOverOwnCrashedTenure(t *testing.T) {
	ctx := context.Background()
	dir, path := sharedState(t)
	owner, err := instanceOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	crash(t, path, owner)
	_, db, err := openState(ctx, dir)
	if err != nil {
		t.Fatalf("open after a crash: %v", err)
	}
	defer db.Close()
	if err := putKey(ctx, db); err != nil {
		t.Fatalf("write after a crash: %v", err)
	}
}

// Another instance's live lease refuses an offline command.
func TestOpenStateRefusesAnotherHolder(t *testing.T) {
	dir, path := sharedState(t)
	crash(t, path, "another-host:/state")
	if _, _, err := openState(context.Background(), dir); !errors.Is(err, storage.ErrInstanceHeld) {
		t.Fatalf("open under another holder: %v", err)
	}
}

// A daemon waits while the state is held, and holds it once it is free.
func TestHoldInstanceWaitsForTheHolder(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := db.AcquireInstance(ctx, "other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	waits := 0
	held, err := holdInstance(ctx, db, "me", instanceTTL, func(_ context.Context, cause error) error {
		waits++
		if !errors.Is(cause, storage.ErrInstanceHeld) {
			t.Fatalf("the wait heard %v", cause)
		}
		return db.ReleaseInstance(ctx, other)
	})
	if err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Fatalf("waited %d times", waits)
	}
	if err := putKey(ctx, held); err != nil {
		t.Fatalf("the new holder's write: %v", err)
	}
}

// flakyAcquire fails its first acquire as the backend's busy database would.
type flakyAcquire struct {
	*sqlite.Store
	failed bool
}

func (f *flakyAcquire) AcquireInstance(ctx context.Context, owner string, ttl time.Duration) (storage.InstanceRef, error) {
	if !f.failed {
		f.failed = true
		return storage.InstanceRef{}, fmt.Errorf("database busy: %w", storage.ErrTransient)
	}
	return f.Store.AcquireInstance(ctx, owner, ttl)
}

// A transient failure leaves a standby waiting, since it says nothing about
// the holder. Without a wait it is the caller's error.
func TestHoldInstanceSurvivesATransientFailure(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := holdInstance(ctx, &flakyAcquire{Store: db}, "me", instanceTTL, nil); !errors.Is(err, storage.ErrTransient) {
		t.Fatalf("with no wait: %v", err)
	}
	waits := 0
	held, err := holdInstance(ctx, &flakyAcquire{Store: db}, "me", instanceTTL, func(_ context.Context, cause error) error {
		waits++
		if !errors.Is(cause, storage.ErrTransient) {
			t.Fatalf("the wait heard %v", cause)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a standby gave up on a transient failure: %v", err)
	}
	if waits != 1 || held == nil {
		t.Fatalf("waited %d times, held %v", waits, held)
	}
}

// A standby's log tells a storage failure from a live holder, and says each
// only when there is something new to say.
func TestStandbyLogsWhyItIsWaiting(t *testing.T) {
	var out bytes.Buffer
	wait := standby(slog.New(slog.NewTextHandler(&out, nil)), time.Nanosecond)
	busy := fmt.Errorf("database busy: %w", storage.ErrTransient)
	gone := fmt.Errorf("connection refused: %w", storage.ErrTransient)
	for _, cause := range []error{busy, busy, gone, gone, storage.ErrInstanceHeld, storage.ErrInstanceHeld} {
		if err := wait(context.Background(), cause); err != nil {
			t.Fatal(err)
		}
	}
	log := out.String()
	for want, n := range map[string]int{"database busy": 1, "connection refused": 1, "another instance holds this state": 1} {
		if got := strings.Count(log, want); got != n {
			t.Errorf("%q logged %d times, want %d:\n%s", want, got, n, log)
		}
	}
	if first := strings.SplitN(log, "\n", 2)[0]; strings.Contains(first, "another instance") {
		t.Errorf("a storage failure was logged as a live holder: %s", first)
	}
}

// A write refused as superseded tells the daemon, which must stop.
func TestHeldDatabaseReportsBeingSuperseded(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	held, err := holdInstance(ctx, db, "me", instanceTTL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lost error
	held.(*heldDatabase).onLost(func(err error) { lost = err })
	if _, err := db.AcquireInstance(ctx, "me", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := putKey(ctx, held); !errors.Is(err, storage.ErrInstanceFenced) {
		t.Fatalf("superseded write: %v", err)
	}
	if !errors.Is(lost, storage.ErrInstanceFenced) {
		t.Fatalf("not told it was superseded: %v", lost)
	}
}

// countingCompactor records what reaches the backend's compaction.
type countingCompactor struct {
	storage.Compactor
	reclaims, rebuilds int
}

func (c *countingCompactor) Reclaim(context.Context, int64) (int64, error) {
	c.reclaims++
	return 0, nil
}

func (c *countingCompactor) Rebuild(context.Context) error {
	c.rebuilds++
	return nil
}

// A holder reclaims while it holds the state. Once superseded it touches
// nothing and is told so, and it never rebuilds, in or out of the start path.
func TestHeldCompactionNeedsTheTenure(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := holdInstance(ctx, db, "me", instanceTTL, nil)
	if err != nil {
		t.Fatal(err)
	}
	held := got.(*heldDatabase)
	var lost error
	held.onLost(func(err error) { lost = err })
	if _, ok := compactorOf(held).(heldCompactor); !ok {
		t.Fatalf("a held database's compactor: %T", compactorOf(held))
	}
	if _, ok := compactorOf(db).(heldCompactor); ok {
		t.Fatal("an unheld database's compactor is under a lease")
	}
	inner := &countingCompactor{}
	c := heldCompactor{Compactor: inner, held: held}
	if _, err := c.Reclaim(ctx, 1); err != nil || inner.reclaims != 1 {
		t.Fatalf("holder's reclaim: %v, %d reached the backend", err, inner.reclaims)
	}
	if err := c.Rebuild(ctx); !errors.Is(err, errRebuildHeld) || inner.rebuilds != 0 {
		t.Fatalf("holder's rebuild: %v, %d reached the backend", err, inner.rebuilds)
	}
	if _, err := db.AcquireInstance(ctx, "me", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reclaim(ctx, 1); !errors.Is(err, storage.ErrInstanceFenced) || inner.reclaims != 1 {
		t.Fatalf("superseded reclaim: %v, %d reached the backend", err, inner.reclaims)
	}
	if !errors.Is(lost, storage.ErrInstanceFenced) {
		t.Fatalf("not told it was superseded: %v", lost)
	}
}

// scriptedLeaser answers renewals in order and reports each one.
type scriptedLeaser struct {
	storage.InstanceLeaser
	answers []error
	renewed chan struct{}
}

func (s *scriptedLeaser) RenewInstance(context.Context, storage.InstanceRef, time.Duration) error {
	err := s.answers[0]
	s.answers = s.answers[1:]
	s.renewed <- struct{}{}
	return err
}

// A renewal that fails for any other reason is retried. Only being superseded
// ends the tenure.
func TestKeepRetriesUntilSuperseded(t *testing.T) {
	leaser := &scriptedLeaser{answers: []error{errors.New("database unavailable"), nil, storage.ErrInstanceFenced}, renewed: make(chan struct{}, 3)}
	h := &heldDatabase{leaser: leaser, ttl: instanceTTL}
	lost := make(chan error, 1)
	h.onLost(func(err error) { lost <- err })
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.keep(context.Background(), ticks, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	for range 3 {
		ticks <- time.Time{}
		<-leaser.renewed
	}
	<-done
	if err := <-lost; !errors.Is(err, storage.ErrInstanceFenced) {
		t.Fatalf("lost with %v", err)
	}
}
