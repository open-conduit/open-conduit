package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	if _, err := holdInstance(ctx, noLease{db}, "me", nil); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("shared state without a lease: %v", err)
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
	crash(t, path, instanceOwner(dir))
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
	held, err := holdInstance(ctx, db, "me", func(context.Context) error {
		waits++
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

// A write refused as superseded tells the daemon, which must stop.
func TestHeldDatabaseReportsBeingSuperseded(t *testing.T) {
	ctx := context.Background()
	_, path := sharedState(t)
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	held, err := holdInstance(ctx, db, "me", nil)
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
	h := &heldDatabase{leaser: leaser}
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
