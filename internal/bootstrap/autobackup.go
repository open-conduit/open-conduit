package bootstrap

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/storage"
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
	// backupStartGrace is how long an overdue backup waits after a start
	// for the daemon's first request, so waking to answer a delivery doesn't
	// begin with a full copy. It is counted from the first start that put
	// the backup off, so short runs can't put it off for ever.
	backupStartGrace = 10 * time.Minute
)

type backupState struct {
	// name is db's backend, which the bundle records.
	name    string
	db      storage.Database
	mu      sync.Mutex
	running bool
	next    time.Time
	// deferred is set while an overdue backup waits for the first request.
	deferred bool
	// shipDue is when the newest bundle is next offered to the destinations;
	// zero when no shipment is owed.
	shipDue time.Time
	// shipping is the bundle a shipment has in flight, which pruning leaves
	// alone. Shipping never shares running: a destination that hangs can't
	// hold up the local backup.
	shipping string
	// shipStall replaces shipStallLimit (tests).
	shipStall time.Duration
	// shipped is each destination's latest outcome since the daemon started.
	shipped map[string]shipResult
	// targets replaces the configured destinations (tests).
	targets func() []shipTarget
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
func (d *daemon) startBackups(name string, db storage.Database) error {
	d.backups.name, d.backups.db = name, db
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
		// A daemon that stopped mid-upload owes its newest bundle. A retry
		// still backing off keeps its time.
		d.backups.shipDue = d.now()
		r, err := d.loadDuty(context.Background(), dutyShipment, d.now(), min(backupRetry, d.o.BackupInterval))
		if err != nil {
			return err
		}
		//duty:backup_shipment
		if r.NextDue.After(d.now()) {
			d.backups.shipDue = r.NextDue
		}
		d.storeShipDue(context.Background())
	}
	return d.planBackup(context.Background(), len(times) > 0)
}

// storeShipDue records when a shipment is next owed (zero: none is), so a
// failed upload's retry survives a restart and shows in the quiescence answer.
func (d *daemon) storeShipDue(ctx context.Context) {
	d.backups.mu.Lock()
	due := d.backups.shipDue
	d.backups.mu.Unlock()
	d.setDuty(ctx, dutyShipment, dutyRecord{NextDue: due})
}

// planBackup records the backup's due time. One already overdue at a start
// is put off until the first request or backupStartGrace, whichever is
// first; once that has passed it runs at once, and the log says why.
func (d *daemon) planBackup(ctx context.Context, hasBundle bool) error {
	b := &d.backups
	r, err := d.loadDuty(ctx, dutyBackup, b.next, 0)
	if err != nil {
		return err
	}
	now := d.now()
	r.NextDue = b.next
	//duty:backup
	if !hasBundle || now.Before(b.next) {
		r.Deferred = time.Time{}
		d.setDuty(ctx, dutyBackup, r)
		return nil
	}
	if r.Deferred.IsZero() {
		r.Deferred = now
	}
	//duty:backup
	if until := r.Deferred.Add(backupStartGrace); now.Before(until) {
		b.next, b.deferred, r.NextDue = until, true, until
		d.log.Info("overdue backup waits for the first request", "until", until)
	} else {
		d.log.Warn("taking an overdue backup before the first request: it has already waited its grace", "waiting_since", r.Deferred)
	}
	d.setDuty(ctx, dutyBackup, r)
	return nil
}

// requestServed lets a backup put off at the start go ahead, now the daemon
// has done what it was woken for.
func (d *daemon) requestServed() {
	b := &d.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deferred {
		b.deferred, b.next = false, d.now()
	}
}

// scheduleBackup starts a scheduled backup in the background when one is due
// and none is running. The loop goes on meanwhile: the database snapshot
// briefly holds the store's connection, and the artifact copy pins the
// artifact store against deletion, so the retention pass waits for it
// (retentionPass) while artifact reads and writes go on.
func (d *daemon) scheduleBackup(ctx context.Context) {
	b := &d.backups
	if d.o.BackupInterval < 0 || b.db == nil {
		return
	}
	now := d.now()
	if bundle, ok := d.claimShipment(now); ok {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.shipBundle(ctx, bundle)
		}()
	}
	b.mu.Lock()
	//duty:backup
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
		b.running, b.deferred = false, false
		if err != nil {
			b.next = now.Add(min(backupRetry, d.o.BackupInterval))
		} else {
			b.next = now.Add(d.o.BackupInterval)
		}
		next := b.next
		b.mu.Unlock()
		if err != nil {
			// The grace already spent stays spent.
			d.duties.mu.Lock()
			r := d.duties.records[dutyBackup]
			d.duties.mu.Unlock()
			r.NextDue = next
			d.setDuty(ctx, dutyBackup, r)
			if ctx.Err() == nil {
				d.report("backup", "", errorCode(err), err.Error())
			}
			return
		}
		d.dutyRan(ctx, dutyBackup, next)
		d.clear("backup", "")
	}()
}

// backupNow publishes one scheduled bundle, marks it owed to the destinations
// and prunes the oldest beyond BackupsKept, except one still being shipped.
func (d *daemon) backupNow(ctx context.Context, now time.Time) error {
	name := bundlePrefix + now.UTC().Format(bundleLayout)
	destination := filepath.Join(d.o.BackupDir, name)
	if _, err := os.Lstat(destination); err == nil {
		return nil
	}
	if err := backup.CreateWithArchive(ctx, destination, d.signingKey(ctx), d.backups.name, d.backups.db, d.artifacts, d.eventArchive(), d.o.Version+" ("+d.o.Commit+")", retention(d.now)); err != nil {
		return err
	}
	d.log.Info("backup published", "path", destination, "bytes", treeSize(destination))
	// The shipment runs on its own (scheduleBackup), so a destination never
	// delays or fails the backup.
	d.backups.mu.Lock()
	d.backups.shipDue = now
	d.backups.mu.Unlock()
	d.storeShipDue(ctx)
	names, _, err := scheduledBundles(d.o.BackupDir)
	if err != nil {
		return err
	}
	for len(names) > d.o.BackupsKept {
		oldest := names[0]
		names = names[1:]
		d.backups.mu.Lock()
		inFlight := d.backups.shipping == oldest
		d.backups.mu.Unlock()
		if inFlight {
			continue // the next backup prunes it
		}
		if err := os.RemoveAll(filepath.Join(d.o.BackupDir, oldest)); err != nil {
			return err
		}
		d.log.Info("backup pruned", "path", filepath.Join(d.o.BackupDir, oldest))
	}
	// Every bundle in the directory counts, pre-upgrade ones included. One
	// that can't be read might need any segment, so nothing is removed then,
	// and the bundle just published stands either way.
	removed, err := backup.PruneSegments(d.o.BackupDir)
	if err != nil {
		d.log.Warn("backup segments not pruned", "error", err)
	}
	for _, name := range removed {
		d.log.Info("backup segment pruned", "segment", name)
	}
	return nil
}

// signingKey is the key a scheduled bundle is signed with, nil when none is
// configured. A key that can't be had doesn't cost the backup: an unsigned
// bundle a restore must explicitly accept beats no bundle when the disk goes.
// The diagnostic says so until signing works again.
func (d *daemon) signingKey(ctx context.Context) ed25519.PrivateKey {
	if d.configs == nil {
		return nil
	}
	s, _ := d.configs.Current()
	if s == nil {
		return nil
	}
	return d.signingKeyFrom(ctx, s.Settings.Backup.SigningKey)
}

// signingKeyFrom resolves the reference, reporting what stops it.
func (d *daemon) signingKeyFrom(ctx context.Context, ref string) ed25519.PrivateKey {
	if ref == "" {
		d.clear("backup_signing", "")
		return nil
	}
	value, err := d.resolver.ResolveNamed(ctx, "backup_signing_key", ref)
	if err != nil {
		d.report("backup_signing", "", "signing_key_unavailable", "publishing this bundle unsigned: "+err.Error())
		return nil
	}
	key, err := backup.ParsePrivateKey(value)
	if err != nil {
		d.report("backup_signing", "", "signing_key_invalid", "publishing this bundle unsigned: "+err.Error())
		return nil
	}
	d.clear("backup_signing", "")
	return key
}

// treeSize is the total size of the regular files under root, for the log.
func treeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, e os.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
