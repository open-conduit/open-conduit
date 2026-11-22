package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// replicaFixture is a stopped daemon's state as a replicator would leave it:
// a world-readable file, a table of the replicator's own, a committed Execution
// with two artifacts and one retired Event whose record is in the archive.
type replicaFixture struct {
	dir, database, artifacts, archive string
	kept, gone                        domain.ArtifactRef
	retired                           domain.ID
}

func newReplicaFixture(t *testing.T) replicaFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := replicaFixture{dir: dir, database: filepath.Join(dir, "replica.db"), artifacts: filepath.Join(dir, "artifacts"), archive: filepath.Join(dir, "archive"), retired: "retired"}
	db, err := sqlite.Open(ctx, f.database)
	if err != nil {
		t.Fatal(err)
	}
	files, err := artifacts.Open(f.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	write := func(text string) domain.ArtifactRef {
		w, err := files.Create(ctx, "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, text); err != nil {
			t.Fatal(err)
		}
		ref, err := w.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	f.kept, f.gone = write("kept"), write("gone")
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "fixture.work", State: domain.WorkReady, ScopeID: "scope"}, 0); err != nil {
			return err
		}
		if _, _, err := tx.AcceptEvent(domain.Event{ID: "trigger", Type: "fixture.start", Source: "fixture", DeliveryID: "trigger", OccurredAt: now, ReceivedAt: now}); err != nil {
			return err
		}
		if _, _, err := tx.AcceptEvent(domain.Event{ID: f.retired, Type: "fixture.old", Source: "fixture", DeliveryID: "retired", OccurredAt: now, ReceivedAt: now}); err != nil {
			return err
		}
		if err := tx.PutExecution(domain.Execution{ID: "run", WorkID: "work", ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: "trigger", Artifacts: []domain.ArtifactRef{f.kept, f.gone}}, 0); err != nil {
			return err
		}
		_, err := tx.Claim("run", "old-host", time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retireForTest(t, ctx, db, f.archive, f.retired, now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+f.database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE replicator_bookkeeping (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.database, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// retireForTest routes the Event, archives its record and retires it.
func retireForTest(t *testing.T, ctx context.Context, db storage.Database, location string, id domain.ID, now time.Time) {
	t.Helper()
	var record domain.AcceptedEvent
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if err := tx.MarkRouted(id, "", "unmatched"); err != nil {
			return err
		}
		if err := tx.SetEventScope(id, "scope"); err != nil {
			return err
		}
		var err error
		record, err = tx.Event(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	archive, err := sqlite.ArchiveBackend.Open(ctx, location)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.PutEvents(ctx, []domain.AcceptedEvent{record}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.RetireEvent(id) }); err != nil {
		t.Fatal(err)
	}
}

// TestReplicaRestoresWhatSurvived brings everything back: the lease is fenced,
// the directory starts drained, and a bundle of it restores.
func TestReplicaRestoresWhatSurvived(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")
	info, err := RestoreReplica(ctx, f.database, dest, cli.ReplicaOptions{ArtifactsFrom: f.artifacts, ArchiveFrom: f.archive})
	if err != nil {
		t.Fatal(err)
	}
	if info.Artifacts != 2 || len(info.MissingArtifacts) != 0 || info.ArchivedEvents != 1 || len(info.MissingEvents) != 0 || !info.Drained {
		t.Fatalf("report %+v", info)
	}
	if got, err := os.Stat(f.database); err != nil || got.Mode().Perm() != 0o644 {
		t.Fatalf("the replica changed: %v, %v", got, err)
	}
	_, store, err := openState(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(tx storage.Tx) error {
		x, err := tx.Execution("run")
		if err != nil {
			return err
		}
		if x.Lease.Owner != "" || x.Lease.Epoch != 1 {
			t.Errorf("lease not fenced: %+v", x.Lease)
		}
		d, on, err := runtime.DrainInTx(tx)
		if err != nil || !on || !strings.Contains(d.Reason, "replica") {
			t.Errorf("drain %+v on=%v: %v", d, on, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	bundleAndRestore(t, dest)
}

// TestReplicaReportsWhatIsMissing covers the disk that took the artifacts and
// archive with it: the restore goes through and says so, and later backups
// still work. Removing the lost record is the negative control: the same state
// then can't be backed up.
func TestReplicaReportsWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	dest := filepath.Join(t.TempDir(), "restored")
	info, err := RestoreReplica(ctx, f.database, dest, cli.ReplicaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Artifacts != 0 || len(info.MissingArtifacts) != 2 || len(info.MissingEvents) != 1 || info.MissingEvents[0] != string(f.retired) {
		t.Fatalf("report %+v", info)
	}
	check, err := CheckState(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if !check.OK {
		t.Fatalf("state check: %+v", check)
	}
	bundleAndRestore(t, dest)

	_, store, err := openState(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
		for _, key := range []string{"event/" + string(f.retired), "artifact/" + f.kept.ID, "artifact/" + f.gone.ID} {
			if err := tx.Delete("core.host.restore.lost", key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupState(ctx, dest, filepath.Join(t.TempDir(), "bundle"), "test", cli.BackupOptions{}); err == nil {
		t.Fatal("negative control: state with unrecorded missing content was backed up")
	}
}

// TestReplicaIsDrainedBeforeItIsPublished stops the restore just after the
// directory appears, as a crash would: what is there must already be drained.
func TestReplicaIsDrainedBeforeItIsPublished(t *testing.T) {
	ctx := context.Background()
	f := newReplicaFixture(t)
	saved := replicaPublished
	replicaPublished = func() error { return errors.New("stopped after publishing") }
	defer func() { replicaPublished = saved }()
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreReplica(ctx, f.database, dest, cli.ReplicaOptions{ArtifactsFrom: f.artifacts, ArchiveFrom: f.archive}); err == nil {
		t.Fatal("the seam didn't stop the restore")
	}
	_, store, err := openState(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(ctx, func(tx storage.Tx) error {
		d, on, err := runtime.DrainInTx(tx)
		if err != nil {
			return err
		}
		if !on {
			t.Error("a published replica isn't drained")
		}
		if d.Origin != drainOrigin(dest) {
			t.Errorf("drain origin %q, want %q", d.Origin, drainOrigin(dest))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReplicaAppliesTheWALBesideIt commits a row only to the WAL: a restore
// reading the file alone would lose it.
func TestReplicaAppliesTheWALBesideIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "replica.db")
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(tx storage.Tx) error {
		return tx.Put("test.replica", "in-wal", []byte("x"))
	}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "copy.db"+suffix), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreReplica(ctx, filepath.Join(dir, "copy.db"), dest, cli.ReplicaOptions{}); err != nil {
		t.Fatal(err)
	}
	_, store, err := openState(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.View(ctx, func(tx storage.Tx) error {
		_, err := tx.Get("test.replica", "in-wal")
		return err
	}); err != nil {
		t.Fatalf("the WAL's commit was lost: %v", err)
	}
}

func TestReplicaRefusesWhatIsNotADatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "restored")
	if _, err := RestoreReplica(ctx, junk, dest, cli.ReplicaOptions{}); err == nil {
		t.Fatal("restored a file that isn't a database")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused restore published %v", err)
	}
}

// bundleAndRestore proves a restored directory can be protected again.
func bundleAndRestore(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	bundle := filepath.Join(t.TempDir(), "bundle")
	if _, err := BackupState(ctx, dir, bundle, "test", cli.BackupOptions{}); err != nil {
		t.Fatalf("backing up the restored directory: %v", err)
	}
	if _, err := VerifyBundle(ctx, bundle, cli.RestoreOptions{}); err != nil {
		t.Fatalf("verifying its bundle: %v", err)
	}
	if _, err := backup.Inspect(bundle); err != nil {
		t.Fatal(err)
	}
}
