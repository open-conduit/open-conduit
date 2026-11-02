package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
)

// Scheduled backups: while it runs, the daemon publishes a backup bundle of
// its own state every BackupInterval into BackupDir and keeps the newest
// BackupsKept. The bundle is the one `backup create` makes, taken with
// SQLite's online backup API, so the daemon keeps running.

// bundlePrefix and bundleLayout name scheduled bundles; only names of this
// form are counted or pruned, so bundles an operator keeps beside them are
// never removed.
const (
	bundlePrefix = "conduit-"
	bundleLayout = "20060102T150405Z"
	// backupRetry is how soon a failed scheduled backup is tried again.
	backupRetry = 15 * time.Minute
)

type backupState struct {
	db      *sqlite.Store
	mu      sync.Mutex
	running bool
	next    time.Time
}

// scheduledBundles are the scheduled bundles in dir and their times, oldest
// first (the layout sorts by time).
func scheduledBundles(dir string) ([]string, []time.Time, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var names []string
	var times []time.Time
	for _, e := range entries { // ReadDir sorts by name
		stamp, ok := strings.CutPrefix(e.Name(), bundlePrefix)
		if !ok || !e.IsDir() {
			continue
		}
		if t, err := time.Parse(bundleLayout, stamp); err == nil {
			names, times = append(names, e.Name()), append(times, t)
		}
	}
	return names, times, nil
}

// startBackups schedules the first backup one interval after the newest
// scheduled bundle (now, when there is none) and removes the staging
// directories an interrupted backup left: the state lock is held, so no
// backup is running.
func (d *daemon) startBackups(db *sqlite.Store) error {
	d.backups.db = db
	if d.o.BackupInterval < 0 {
		return nil
	}
	if err := os.MkdirAll(d.o.BackupDir, 0o700); err != nil {
		return err
	}
	if stale, err := filepath.Glob(filepath.Join(d.o.BackupDir, ".conduit-snapshot-*")); err == nil {
		for _, p := range stale {
			_ = os.RemoveAll(p)
		}
	}
	_, times, err := scheduledBundles(d.o.BackupDir)
	if err != nil {
		return err
	}
	d.backups.next = d.now()
	if len(times) > 0 {
		d.backups.next = times[len(times)-1].Add(d.o.BackupInterval)
	}
	return nil
}

// scheduleBackup starts a scheduled backup in the background when one is due
// and none is running. The loop goes on meanwhile: the database snapshot
// briefly holds the store's connection, and the artifact copy holds nothing.
func (d *daemon) scheduleBackup(ctx context.Context) {
	b := &d.backups
	if d.o.BackupInterval < 0 || b.db == nil {
		return
	}
	now := d.now()
	b.mu.Lock()
	if b.running || now.Before(b.next) {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		err := d.backupNow(ctx, now)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.running = false
		if err != nil {
			b.next = now.Add(min(backupRetry, d.o.BackupInterval))
			if ctx.Err() == nil {
				d.report("backup", "", errorCode(err), err.Error())
			}
			return
		}
		b.next = now.Add(d.o.BackupInterval)
		d.clear("backup", "")
	}()
}

// backupNow publishes one scheduled bundle and prunes the oldest beyond
// BackupsKept.
func (d *daemon) backupNow(ctx context.Context, now time.Time) error {
	name := bundlePrefix + now.UTC().Format(bundleLayout)
	destination := filepath.Join(d.o.BackupDir, name)
	if _, err := os.Lstat(destination); err == nil {
		return nil
	}
	if err := backup.Create(ctx, destination, d.backups.db, d.artifacts, d.o.Version+" ("+d.o.Commit+")", retentionExpired); err != nil {
		return err
	}
	d.log.Info("backup published", "path", destination)
	names, _, err := scheduledBundles(d.o.BackupDir)
	if err != nil {
		return err
	}
	for len(names) > d.o.BackupsKept {
		if err := os.RemoveAll(filepath.Join(d.o.BackupDir, names[0])); err != nil {
			return err
		}
		d.log.Info("backup pruned", "path", filepath.Join(d.o.BackupDir, names[0]))
		names = names[1:]
	}
	return nil
}
