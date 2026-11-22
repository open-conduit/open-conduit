package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func backupDaemon(t *testing.T, now time.Time) *daemon {
	t.Helper()
	state := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	files, err := artifacts.Open(filepath.Join(state, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	d := &daemon{o: Options{StateDir: state, BackupDir: filepath.Join(state, "backups"), BackupInterval: time.Hour, BackupsKept: 2, Version: "test", Commit: "abc"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }, artifacts: files, reported: map[string]string{}}
	if err = d.startBackups(sqlite.Name, db); err != nil {
		t.Fatal(err)
	}
	return d
}

func bundleNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Scheduled backups publish restorable bundles, keep the newest BackupsKept,
// and leave bundles not named by the schedule alone.
func TestScheduledBackupsKeepTheNewest(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	if !d.backups.next.Equal(start) {
		t.Fatalf("first backup at %v, want at start", d.backups.next)
	}
	mine := filepath.Join(d.o.BackupDir, "before-upgrade")
	if err := os.Mkdir(mine, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := d.backupNow(ctx, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	got := bundleNames(t, d.o.BackupDir)
	want := []string{"before-upgrade", "conduit-20261102T100000Z", "conduit-20261102T110000Z"}
	if !slices.Equal(got, want) {
		t.Fatalf("bundles %v, want %v", got, want)
	}
	if err := backup.Restore(ctx, filepath.Join(d.o.BackupDir, want[2]), filepath.Join(t.TempDir(), "restored"), backup.Trust{}, start, retentionExpired, restoreBackends, backup.Target{Artifacts: artifacts.Backend}, recordChoice); err != nil {
		t.Fatalf("scheduled bundle does not restore: %v", err)
	}
}

// A restarted daemon backs up one interval after its newest scheduled
// bundle, not at every start, and clears an interrupted backup's staging.
func TestScheduledBackupsResumeFromTheNewestBundle(t *testing.T) {
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	if err := d.backupNow(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(d.o.BackupDir, ".conduit-snapshot-123")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	restarted := &daemon{o: d.o, log: d.log, now: func() time.Time { return start.Add(10 * time.Minute) }}
	if err := restarted.startBackups(d.backups.name, d.backups.db); err != nil {
		t.Fatal(err)
	}
	if want := start.Add(time.Hour); !restarted.backups.next.Equal(want) {
		t.Fatalf("next backup %v, want %v", restarted.backups.next, want)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("staging directory left: %v", err)
	}
}

// Off means no directory and no backup.
func TestScheduledBackupsOff(t *testing.T) {
	state := t.TempDir()
	d := &daemon{o: Options{BackupDir: filepath.Join(state, "backups"), BackupInterval: -1}, now: time.Now}
	if err := d.startBackups(sqlite.Name, nil); err != nil {
		t.Fatal(err)
	}
	d.scheduleBackup(context.Background())
	if _, err := os.Lstat(d.o.BackupDir); !os.IsNotExist(err) {
		t.Fatalf("backups off but directory made: %v", err)
	}
}

// Doctor reports what the backend states and the newest bundle, and never
// turns the answer into a warning: how much loss is acceptable is the
// operator's call.
func TestRecoveryCheckReportsTheBackendsStatement(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	d := backupDaemon(t, now)
	d.backups.source = coordination{name: sqlite.Name, backend: sqlite.Backend, location: filepath.Join(d.o.StateDir, "conduit.db")}
	check := d.recoveryCheck(ctx)
	if check.Status != runtime.CheckInfo || check.Code != "replication_unobserved" || !strings.Contains(check.Detail, "can't see") {
		t.Fatalf("check = %+v", check)
	}
	if len(check.Items) != 1 || check.Items[0] != "no scheduled bundle yet" {
		t.Fatalf("items = %v", check.Items)
	}
	if err := d.backupNow(ctx, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check = d.recoveryCheck(ctx)
	if len(check.Items) != 1 || !strings.Contains(check.Items[0], "conduit-20261122T100000Z, 2h0m0s old") {
		t.Fatalf("items = %v", check.Items)
	}
	d.backups.source.backend = statingBackend{Backend: sqlite.Backend, point: storage.RecoveryPoint{Replication: storage.ReplicationObserved, Lag: 3 * time.Second, Detail: "streaming to a standby"}}
	if check = d.recoveryCheck(ctx); check.Status != runtime.CheckInfo || check.Code != "replication_observed" || !strings.Contains(check.Detail, "3s behind") {
		t.Fatalf("observed: %+v", check)
	}
	d.backups.source.backend = statingBackend{Backend: sqlite.Backend, err: errors.New("server unreachable")}
	if check = d.recoveryCheck(ctx); check.Status != runtime.CheckWarn || check.Code != "unavailable" {
		t.Fatalf("failed: %+v", check)
	}
	d.backups.source.backend = statingBackend{Backend: sqlite.Backend, point: storage.RecoveryPoint{Replication: "unknown", Detail: "?"}}
	if check = d.recoveryCheck(ctx); check.Status != runtime.CheckWarn || check.Code != "invalid" {
		t.Fatalf("invalid: %+v", check)
	}
	d.backups.source.backend = struct{ storage.Backend }{sqlite.Backend}
	if check = d.recoveryCheck(ctx); check.Status != runtime.CheckInfo || check.Code != "unstated" {
		t.Fatalf("unstated: %+v", check)
	}
	d.o.BackupInterval = -1
	if check = d.recoveryCheck(ctx); len(check.Items) != 1 || check.Items[0] != "scheduled backups are off" {
		t.Fatalf("off: %+v", check)
	}
}

type statingBackend struct {
	storage.Backend
	point storage.RecoveryPoint
	err   error
}

func (b statingBackend) RecoveryPoint(context.Context, string) (storage.RecoveryPoint, error) {
	return b.point, b.err
}
