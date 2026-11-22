package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// A start on a database an older binary left raises its stored versions, and
// the older binary then refuses it, so going back a release needs a bundle
// from before the raise. The daemon takes that bundle itself, before it opens
// the database. It is named outside the scheduled conduit-<time> set, so
// rotation never counts or prunes it.

const (
	upgradeBundlePrefix = "pre-upgrade-"
	// upgradeVerifyPrefix names a verification restore's directory. It is a
	// stage by the name the backup directory's sweep and an in-place
	// restore's both clear.
	upgradeVerifyPrefix       = ".conduit-snapshot-verify-"
	defaultUpgradeBundlesKept = 3
	// buildNamespace and buildKey hold the build that last ran on the state,
	// so the next upgrade's bundle can say which build it came from.
	buildNamespace = "core.build"
	buildKey       = "last"
)

// preserveBeforeUpgrade takes a verified bundle of a database an older binary
// left, before anything rewrites it. If it can't, the daemon doesn't start
// and the database stays at its old versions.
func (d *daemon) preserveBeforeUpgrade(ctx context.Context) error {
	// The daemon lock is held, so a verification stage here is a crash's.
	if stale, err := filepath.Glob(filepath.Join(d.o.StateDir, upgradeVerifyPrefix+"*")); err == nil {
		for _, p := range stale {
			_ = os.RemoveAll(p)
		}
	}
	c, err := coordinationOf(d.o.StateDir)
	if err != nil {
		return err
	}
	prior, err := c.backend.OpenPrior(ctx, c.location)
	if err != nil {
		return err
	}
	if prior == nil {
		d.pruneUpgradeBundles()
		return nil
	}
	defer prior.Close()
	stored := prior.Stored()
	if d.o.NoUpgradeBundle {
		d.log.Warn("starting on an older database without a pre-upgrade bundle, as --no-upgrade-bundle asks: going back to the older binary will need a bundle taken earlier", "record_format", stored.Records, "schema", stored.Schema)
		return prior.Close()
	}
	path, err := d.takeUpgradeBundle(ctx, c.name, prior)
	if err != nil {
		return fmt.Errorf("not starting: this binary would upgrade the database from record format %d, schema %d, and the bundle that makes going back possible could not be taken (backup directory %s): %w. The database is unchanged. Fix that, or run serve --no-upgrade-bundle once to go ahead without a way back", stored.Records, stored.Schema, d.o.BackupDir, err)
	}
	d.log.Info("pre-upgrade bundle kept", "path", path, "record_format", stored.Records, "schema", stored.Schema, "bytes", treeSize(path))
	d.pruneUpgradeBundles()
	return prior.Close()
}

// takeUpgradeBundle composes and verifies the bundle under a staging name a
// later start clears, and publishes it only once it restores. When a start
// that stopped before raising the format already kept this same state, that
// bundle stands, so a crash loop can't rotate older ones out.
func (d *daemon) takeUpgradeBundle(ctx context.Context, backendName string, prior storage.Prior) (string, error) {
	if err := os.MkdirAll(d.o.BackupDir, 0o700); err != nil {
		return "", err
	}
	if stale, err := filepath.Glob(filepath.Join(d.o.BackupDir, ".conduit-snapshot-*")); err == nil {
		for _, p := range stale {
			_ = os.RemoveAll(p)
		}
	}
	_, files, err := openArtifacts(ctx, d.o.StateDir)
	if err != nil {
		return "", fmt.Errorf("artifacts: %w", err)
	}
	defer files.Close()
	events, closeEvents, err := openEventArchive(ctx, d.o.StateDir)
	if err != nil {
		return "", fmt.Errorf("event archive: %w", err)
	}
	defer closeEvents()
	// The key is read as backup create reads it, since packages aren't
	// running yet. One that can't be had doesn't cost the way back: a restore
	// can accept the unsigned bundle explicitly.
	key, err := offlineSigningKey(ctx, cli.BackupOptions{ConfigPath: d.o.ConfigPath})
	if err != nil {
		d.log.Warn("taking the pre-upgrade bundle unsigned: a restore with trust keys needs --accept-unsigned", "error", err)
		key = nil
	}
	now := d.now().UTC()
	stamp := now.Format(bundleLayout)
	stage := filepath.Join(d.o.BackupDir, ".conduit-snapshot-upgrade-"+stamp)
	defer os.RemoveAll(stage)
	build := d.o.Version + " (" + d.o.Commit + ")"
	if err := backup.CreatePrior(ctx, stage, key, backendName, prior, files, events, build, retentionExpired, now, lastBuild); err != nil {
		return "", err
	}
	within := d.upgradeVerifyDir()
	if err := verifyUpgradeBundle(ctx, stage, within, now); err != nil {
		return "", fmt.Errorf("verifying it by restoring it into %s: %w", within, err)
	}
	staged, err := backup.Inspect(stage)
	if err != nil {
		return "", err
	}
	names, _ := upgradeBundles(d.o.BackupDir)
	if len(names) > 0 {
		newest := filepath.Join(d.o.BackupDir, names[len(names)-1])
		if held, err := backup.Inspect(newest); err == nil && held.DatabaseSHA256 == staged.DatabaseSHA256 {
			return newest, nil
		}
	}
	path := filepath.Join(d.o.BackupDir, upgradeBundlePrefix+stamp)
	if err := os.Rename(stage, path); err != nil {
		return "", err
	}
	dir, err := os.Open(d.o.BackupDir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	return path, dir.Sync()
}

// upgradeVerifyDir is where the verification restore goes: the backup
// directory, the disk sized for bundles, rather than a system temporary
// directory that is often a small tmpfs. A restore opens a live database,
// which a network filesystem can't hold, so a backup directory on one (or one
// that can't be classified) gives way to the state directory, which is
// always local.
func (d *daemon) upgradeVerifyDir() string {
	if info, err := localfs.Detect(d.o.BackupDir); err != nil || info.Network {
		return d.o.StateDir
	}
	return d.o.BackupDir
}

// verifyUpgradeBundle restores the bundle into a private directory within
// dir, as backup verify does. The bundle was made here a moment ago, so it is
// checked for being whole, not for who signed it.
func verifyUpgradeBundle(ctx context.Context, bundle, dir string, now time.Time) error {
	tmp, err := os.MkdirTemp(dir, upgradeVerifyPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	return backup.Restore(ctx, bundle, filepath.Join(tmp, "state"), backup.Trust{}, now, retentionExpired, restoreBackends, backup.Target{Artifacts: artifacts.Backend, EventArchive: sqlite.ArchiveBackend}, recordChoice)
}

// upgradeBundles are the pre-upgrade bundles in dir, oldest first, with their
// times.
func upgradeBundles(dir string) ([]string, []time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var names []string
	var times []time.Time
	for _, e := range entries { // ReadDir sorts by name
		stamp, ok := strings.CutPrefix(e.Name(), upgradeBundlePrefix)
		if !ok || !e.IsDir() {
			continue
		}
		if t, err := time.Parse(bundleLayout, stamp); err == nil {
			names, times = append(names, e.Name()), append(times, t)
		}
	}
	return names, times
}

// pruneUpgradeBundles applies the operator's count and age to the pre-upgrade
// bundles. The newest always stays: it may be the only way back. A failure is
// logged and never stops a start.
func (d *daemon) pruneUpgradeBundles() {
	names, times := upgradeBundles(d.o.BackupDir)
	for i := 0; i < len(names)-1; i++ {
		over := len(names)-i > d.o.UpgradeBundlesKept
		old := d.o.UpgradeBundlesMaxAge > 0 && d.now().Sub(times[i]) > d.o.UpgradeBundlesMaxAge
		if !over && !old {
			continue
		}
		path := filepath.Join(d.o.BackupDir, names[i])
		if err := os.RemoveAll(path); err != nil {
			d.log.Warn("could not prune a pre-upgrade bundle", "path", path, "error", err)
			continue
		}
		d.log.Info("pre-upgrade bundle pruned", "path", path)
	}
	// The state lock is held and the loop hasn't started, so no bundle is
	// being written. With backups off this is the only prune there is.
	if _, err := backup.PruneSegments(d.o.BackupDir); err != nil {
		d.log.Warn("backup segments not pruned", "error", err)
	}
}

// lastBuild reads the build that last ran on the state, "" if none recorded
// itself.
func lastBuild(tx storage.Tx) (string, error) {
	b, err := tx.Get(buildNamespace, buildKey)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	return string(b), err
}

// recordBuild notes this build on the state, for the next upgrade's label.
// It is only a label, so a failure is logged and the start goes on.
func (d *daemon) recordBuild(ctx context.Context, store storage.Database) {
	build := d.o.Version + " (" + d.o.Commit + ")"
	err := store.Transact(ctx, storage.Guard{Now: d.now().UTC()}, func(tx storage.Tx) error {
		if held, err := lastBuild(tx); err != nil || held == build {
			return err
		}
		return tx.Put(buildNamespace, buildKey, []byte(build))
	})
	if err != nil {
		d.log.Warn("could not record this build on the state", "error", err)
	}
}

// refuseBeforeUpgrade stops an offline command from raising the stored
// versions, which would skip the daemon's pre-upgrade bundle.
func refuseBeforeUpgrade(ctx context.Context, stateDir string) error {
	c, err := coordinationOf(stateDir)
	if err != nil {
		return err
	}
	prior, err := c.backend.OpenPrior(ctx, c.location)
	if err != nil || prior == nil {
		return err
	}
	stored := prior.Stored()
	if err := prior.Close(); err != nil {
		return err
	}
	return fmt.Errorf("this database is at record format %d, schema %d, from an older binary: start the daemon once with this binary first, which keeps a pre-upgrade bundle, or run this command with the older binary", stored.Records, stored.Schema)
}
