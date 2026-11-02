package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
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
	if err = d.startBackups(db); err != nil {
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
	if err := backup.Restore(ctx, filepath.Join(d.o.BackupDir, want[2]), filepath.Join(t.TempDir(), "restored"), start, retentionExpired); err != nil {
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
	if err := restarted.startBackups(d.backups.db); err != nil {
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
	if err := d.startBackups(nil); err != nil {
		t.Fatal(err)
	}
	d.scheduleBackup(context.Background())
	if _, err := os.Lstat(d.o.BackupDir); !os.IsNotExist(err) {
		t.Fatalf("backups off but directory made: %v", err)
	}
}

// While a backup copies artifacts (the store pinned), a due retention pass
// is skipped rather than queued on the artifact gate, which would stall the
// loop and every artifact read and write behind it. Artifacts are still
// written meanwhile, and the sweep stays due for the first pass after.
func TestRetentionWaitsForARunningBackup(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	d.o.RetentionInterval = 10 * time.Minute
	pin, err := d.artifacts.Pin() // the backup's artifact copy
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	d.backups.running = true
	var lastSweep time.Time
	// c is nil: a pass that went ahead would panic rather than pass.
	if d.retentionPass(ctx, nil, &lastSweep) {
		t.Fatal("retention ran during a backup")
	}
	if !lastSweep.IsZero() {
		t.Fatal("skipped sweep counted as done")
	}
	w, err := d.artifacts.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("run output")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(ctx); err != nil {
		t.Fatalf("artifact write blocked during a backup: %v", err)
	}
}
