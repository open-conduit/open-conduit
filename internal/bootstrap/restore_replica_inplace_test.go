package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// emptyVolume is an existing, empty, owner-only state directory.
func emptyVolume(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return state
}

func replicaOnce(f replicaFixture) cli.ReplicaOptions {
	return cli.ReplicaOptions{RestoreOptions: cli.RestoreOptions{Once: true}, ArtifactsFrom: f.artifacts, ArchiveFrom: f.archive}
}

func assertDrained(t *testing.T, state string, want bool) {
	t.Helper()
	ctx := context.Background()
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(ctx, func(tx storage.Tx) error {
		_, on, err := runtime.DrainInTx(tx)
		if err == nil && on != want {
			t.Errorf("drained = %v, want %v", on, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A container's empty volume takes the replica, drained and fenced, with a
// fresh credential and its content back.
func TestReplicaRestoresIntoAnEmptyVolume(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	state := emptyVolume(t)
	info, err := RestoreReplicaInPlace(ctx, f.database, state, replicaOnce(f))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Drained || info.Artifacts != 2 || info.ArchivedEvents != 1 || info.InPlace == nil || info.InPlace.AlreadyRestored {
		t.Fatalf("report %+v", info)
	}
	if info.InPlace.Check == nil || !info.InPlace.Check.OK {
		t.Fatalf("state check: %+v", info.InPlace.Check)
	}
	if got, err := os.Stat(info.TokenFile); err != nil || got.Mode().Perm() != 0o600 {
		t.Fatalf("token: %v, %v", got, err)
	}
	if got, err := os.Stat(f.database); err != nil || got.Mode().Perm() != 0o644 {
		t.Fatalf("the replica changed: %v, %v", got, err)
	}
	assertDrained(t, state, true)
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(ctx, func(tx storage.Tx) error {
		x, err := tx.Execution("run")
		if err == nil && x.Lease.Owner != "" {
			t.Errorf("lease not fenced: %+v", x.Lease)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A restart must keep what the daemon committed after the restore, whatever
// has become of the replica. Without Once the same directory is refused.
func TestReplicaNeverReplacesADatabase(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	state := emptyVolume(t)
	if _, err := RestoreReplicaInPlace(ctx, f.database, state, replicaOnce(f)); err != nil {
		t.Fatal(err)
	}
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		return tx.Put("fixture", "after-restore", []byte("kept"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.database); err != nil {
		t.Fatal(err)
	}
	info, err := RestoreReplicaInPlace(ctx, f.database, state, replicaOnce(f))
	if err != nil {
		t.Fatalf("a restart with the replica gone: %v", err)
	}
	if !info.InPlace.AlreadyRestored {
		t.Fatalf("report %+v", info.InPlace)
	}
	_, err = RestoreReplicaInPlace(ctx, f.database, state, cli.ReplicaOptions{})
	if !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "never replaces") {
		t.Fatalf("without Once: %v", err)
	}
	_, store, err = openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(ctx, func(tx storage.Tx) error {
		_, err := tx.Get("fixture", "after-restore")
		return err
	}); err != nil {
		t.Fatalf("the commit after the restore is gone: %v", err)
	}
}

// A swap that stops part way is finished by the next start, drained, and not
// restored a second time.
func TestReplicaInPlaceFinishesAnInterruptedSwap(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	state := emptyVolume(t)
	if err := os.WriteFile(filepath.Join(state, "conduit.db-wal"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapInterrupted = func(int) error { return errors.New("power cut") }
	_, err := RestoreReplicaInPlace(ctx, f.database, state, replicaOnce(f))
	swapInterrupted = func(int) error { return nil }
	if err == nil || !strings.Contains(err.Error(), "stopped part way") {
		t.Fatalf("err = %v", err)
	}
	if err := restoreInterrupted(state); err == nil {
		t.Fatal("negative control: the interrupted directory wasn't refused")
	}
	info, err := RestoreReplicaInPlace(ctx, f.database, state, replicaOnce(f))
	if err != nil {
		t.Fatal(err)
	}
	if !info.InPlace.AlreadyRestored || info.InPlace.MovedAside == "" {
		t.Fatalf("report %+v", info.InPlace)
	}
	if err := restoreInterrupted(state); err != nil {
		t.Fatal(err)
	}
	assertDrained(t, state, true)
}

// A volume that lost only its database still holds artifacts and the archive:
// they come back rather than being moved aside and recorded as lost.
func TestReplicaKeepsCopiesAlreadyInTheDirectory(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	state := emptyVolume(t)
	for _, move := range [][2]string{{f.artifacts, filepath.Join(state, "artifacts")}, {f.archive, filepath.Join(state, "archive")}} {
		if err := os.Rename(move[0], move[1]); err != nil {
			t.Fatal(err)
		}
	}
	info, err := RestoreReplicaInPlace(ctx, f.database, state, cli.ReplicaOptions{RestoreOptions: cli.RestoreOptions{Once: true}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Artifacts != 2 || len(info.MissingArtifacts) != 0 || info.ArchivedEvents != 1 || len(info.MissingEvents) != 0 {
		t.Fatalf("report %+v", info)
	}
}
