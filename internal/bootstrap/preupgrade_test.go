package bootstrap

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

var upgradeStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

const olderFormat = sqlite.RecordFormat - 1

// olderState is a state directory an older binary, at olderFormat, left.
func olderState(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	defer sqlite.SimulateRecordFormat(olderFormat)()
	db, err := sqlite.Backend.Open(ctx, filepath.Join(state, "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Transact(ctx, storage.Guard{Now: upgradeStart}, func(tx storage.Tx) error {
		if err := tx.Put(buildNamespace, buildKey, []byte("v1 (old)")); err != nil {
			return err
		}
		return tx.PutScope(domain.Scope{ID: "scope", Name: "before"}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func upgradeDaemon(state string, now time.Time) *daemon {
	return &daemon{o: Options{StateDir: state, ConfigPath: filepath.Join(state, "absent.json"), BackupDir: filepath.Join(state, "backups"), BackupInterval: time.Hour, BackupsKept: 2, UpgradeBundlesKept: defaultUpgradeBundlesKept, Version: "v2", Commit: "new"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }, reported: map[string]string{}}
}

func storedFormat(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow("SELECT value FROM metadata WHERE key='record_format'").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The first start on a newer format leaves a labelled bundle the older binary
// restores and opens, with the live database then raised.
func TestUpgradeBundleRestoresOnTheOlderBinary(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	d := upgradeDaemon(state, upgradeStart)
	if err := d.preserveBeforeUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedFormat(t, filepath.Join(state, "conduit.db")); got != fmt.Sprint(olderFormat) {
		t.Fatalf("taking the bundle changed the format to %s", got)
	}
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if got := storedFormat(t, filepath.Join(state, "conduit.db")); got != fmt.Sprint(sqlite.RecordFormat) {
		t.Fatalf("live format %s after opening", got)
	}
	bundle := filepath.Join(d.o.BackupDir, "pre-upgrade-20261102T090000Z")
	info, err := bundleInfo(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if p := info.PreUpgrade; p == nil || p.RecordFormat != olderFormat || p.Schema != sqlite.SchemaVersion || p.FromBuild != "v1 (old)" || info.Build != "v2 (new)" {
		t.Fatalf("label: %+v %+v", info, info.PreUpgrade)
	}
	if got := storedFormat(t, filepath.Join(bundle, "conduit.db")); got != fmt.Sprint(olderFormat) {
		t.Fatalf("bundle holds format %s", got)
	}
	defer sqlite.SimulateRecordFormat(olderFormat)()
	restored := filepath.Join(t.TempDir(), "restored")
	if err := backup.Restore(ctx, bundle, restored, backup.Trust{}, upgradeStart, retentionExpired, restoreBackends, backup.Target{Artifacts: artifacts.Backend}, recordChoice); err != nil {
		t.Fatalf("the older binary can't restore it: %v", err)
	}
	_, again, err := openState(ctx, restored)
	if err != nil {
		t.Fatalf("the older binary can't open it: %v", err)
	}
	defer again.Close()
	ids, err := scopeIDs(ctx, again)
	if err != nil || len(ids) != 1 || ids[0] != "scope" {
		t.Fatalf("restored scopes %v, %v", ids, err)
	}
	// Negative control: the live database, already raised, is what the older
	// binary could not have opened.
	if _, err := sqlite.Backend.Open(ctx, filepath.Join(state, "conduit.db")); !errors.Is(err, storage.ErrRecordsNewer) {
		t.Fatalf("older binary on the raised database: %v", err)
	}
}

// The label fits a manifest a binary from before it reads: no new field.
func TestUpgradeBundleManifestHasNoNewField(t *testing.T) {
	state := olderState(t)
	d := upgradeDaemon(state, upgradeStart)
	if err := d.preserveBeforeUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(d.o.BackupDir, "pre-upgrade-20261102T090000Z", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("PreUpgrade")) || bytes.Contains(raw, []byte("RecordFormat")) || !bytes.Contains(raw, []byte("pre-upgrade records=")) {
		t.Fatalf("manifest: %s", raw)
	}
}

// Seven days of scheduled bundles rotate around it.
func TestScheduledRotationKeepsTheUpgradeBundle(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	d := upgradeDaemon(state, upgradeStart)
	if err := d.preserveBeforeUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files, err := artifacts.Open(filepath.Join(state, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	d.artifacts = files
	d.o.BackupsKept = 7
	if err := d.startBackups(sqlite.Name, store); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := d.backupNow(ctx, upgradeStart.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	scheduled, _, err := scheduledBundles(d.o.BackupDir)
	if err != nil || len(scheduled) != 7 {
		t.Fatalf("scheduled %v, %v", scheduled, err)
	}
	if names, _ := upgradeBundles(d.o.BackupDir); len(names) != 1 {
		t.Fatalf("pre-upgrade bundles %v", names)
	}
}

// With a backup directory it can't write to, the daemon refuses, says how to
// go ahead, and leaves the database at its old format.
func TestUpgradeRefusesWithoutABundle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	state := olderState(t)
	d := upgradeDaemon(state, upgradeStart)
	if err := os.Mkdir(d.o.BackupDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d.o.BackupDir, 0o700) })
	err := d.preserveBeforeUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--no-upgrade-bundle") {
		t.Fatalf("refusal: %v", err)
	}
	if got := storedFormat(t, filepath.Join(state, "conduit.db")); got != fmt.Sprint(olderFormat) {
		t.Fatalf("refused start left format %s", got)
	}
	// Negative control: the deliberate way past.
	d.o.NoUpgradeBundle = true
	if err := d.preserveBeforeUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A current database gets no bundle.
func TestNoUpgradeBundleWithoutAnUpgrade(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	_, store, err := openState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	d := upgradeDaemon(state, upgradeStart)
	if err := d.preserveBeforeUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if names, _ := upgradeBundles(d.o.BackupDir); len(names) != 0 {
		t.Fatalf("bundles %v", names)
	}
}

// A start that stopped before raising the format doesn't add a second copy
// of the same state, so a crash loop can't rotate older bundles out.
func TestUpgradeBundleIsNotRepeatedForTheSameState(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	for i := range 2 {
		d := upgradeDaemon(state, upgradeStart.Add(time.Duration(i)*time.Minute))
		if err := d.preserveBeforeUpgrade(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if names, _ := upgradeBundles(filepath.Join(state, "backups")); len(names) != 1 {
		t.Fatalf("bundles %v", names)
	}
}

// The operator's count and age apply, and the newest always stays.
func TestUpgradeBundlesPrune(t *testing.T) {
	dir := t.TempDir()
	d := upgradeDaemon(dir, upgradeStart)
	d.o.BackupDir = dir
	for i := range 5 {
		name := upgradeBundlePrefix + upgradeStart.Add(-time.Duration(i)*24*time.Hour).Format(bundleLayout)
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	d.pruneUpgradeBundles()
	if names, _ := upgradeBundles(dir); len(names) != defaultUpgradeBundlesKept {
		t.Fatalf("by count: %v", names)
	}
	d.o.UpgradeBundlesMaxAge = time.Hour
	d.now = func() time.Time { return upgradeStart.Add(365 * 24 * time.Hour) }
	d.pruneUpgradeBundles()
	names, _ := upgradeBundles(dir)
	if len(names) != 1 || names[0] != upgradeBundlePrefix+upgradeStart.Format(bundleLayout) {
		t.Fatalf("by age: %v", names)
	}
}

// Offline commands don't raise the format past the daemon's bundle: backup
// create keeps it as it stands, and auth recover refuses.
func TestOfflineCommandsLeaveAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	bundle := filepath.Join(t.TempDir(), "by-hand")
	info, err := BackupState(ctx, state, bundle, "v2 (new)", cli.BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.PreUpgrade == nil || info.PreUpgrade.RecordFormat != olderFormat {
		t.Fatalf("by-hand bundle: %+v", info)
	}
	err = RecoverOperator(ctx, state, filepath.Join(state, "private", "operator.token"))
	if err == nil || !strings.Contains(err.Error(), "older binary") {
		t.Fatalf("auth recover: %v", err)
	}
	if got := storedFormat(t, filepath.Join(state, "conduit.db")); got != fmt.Sprint(olderFormat) {
		t.Fatalf("offline commands left format %s", got)
	}
}

// The verification restore stays off the system temporary directory, which
// is often a small tmpfs: a start must not depend on it.
func TestUpgradeBundleVerifiesBesideTheBundle(t *testing.T) {
	state := olderState(t)
	missing := filepath.Join(t.TempDir(), "no-such-tmp")
	t.Setenv("TMPDIR", missing)
	d := upgradeDaemon(state, upgradeStart)
	if err := d.preserveBeforeUpgrade(context.Background()); err != nil {
		t.Fatalf("start depended on the temporary directory: %v", err)
	}
	if names, _ := upgradeBundles(d.o.BackupDir); len(names) != 1 {
		t.Fatalf("bundles %v", names)
	}
	if left, _ := filepath.Glob(filepath.Join(d.o.BackupDir, ".conduit-snapshot-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}

// A backup directory on a network mount is allowed but can't hold the live
// database a verification restore opens, so that restore uses the state
// directory, and clears what a crash left there.
func TestUpgradeBundleVerifiesOffANetworkBackupDir(t *testing.T) {
	state := olderState(t)
	d := upgradeDaemon(state, upgradeStart)
	stale := filepath.Join(state, upgradeVerifyPrefix+"crashed")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	defer localfs.SwapProbe(func(path string) (localfs.Info, error) {
		if strings.HasPrefix(path, d.o.BackupDir) {
			return localfs.Info{Name: "nfs", Network: true}, nil
		}
		return localfs.Info{Name: "apfs"}, nil
	})()
	if err := d.preserveBeforeUpgrade(context.Background()); err != nil {
		t.Fatalf("a network backup directory stopped the start: %v", err)
	}
	if names, _ := upgradeBundles(d.o.BackupDir); len(names) != 1 {
		t.Fatalf("bundles %v", names)
	}
	if left, _ := filepath.Glob(filepath.Join(state, upgradeVerifyPrefix+"*")); len(left) != 0 {
		t.Fatalf("verification stages left in the state directory: %v", left)
	}
}
