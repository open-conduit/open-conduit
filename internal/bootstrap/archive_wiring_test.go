package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/backup"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

const wiredBody = `{"message":"only the archive holds this"}`

// retireInto accepts an Event in db, copies it to the archive and retires it.
func retireInto(t *testing.T, db storage.Store, events storage.EventArchive, id domain.ID, now time.Time) {
	t.Helper()
	ctx := context.Background()
	var record domain.AcceptedEvent
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if _, err := tx.Scope("archive-scope"); errors.Is(err, storage.ErrNotFound) {
			if err := tx.PutScope(domain.Scope{ID: "archive-scope", Name: "archive-scope"}, 0); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		e := domain.Event{ID: id, Type: "fixture.seen", Source: "fixture", DeliveryID: string(id), OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "fixture.v1", Data: []byte(wiredBody)}}
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		if err := tx.MarkRouted(id, "", "unmatched"); err != nil {
			return err
		}
		if err := tx.SetEventScope(id, "archive-scope"); err != nil {
			return err
		}
		var err error
		record, err = tx.Event(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := events.PutEvents(ctx, []domain.AcceptedEvent{record}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error { return tx.RetireEvent(id) }); err != nil {
		t.Fatal(err)
	}
}

func restoredPayload(t *testing.T, bundle string, id domain.ID, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	restored := filepath.Join(t.TempDir(), "restored")
	if err := backup.Restore(ctx, bundle, restored, backup.Trust{AcceptUnsigned: true}, now, retentionExpired, restoreBackends, backup.Target{Artifacts: artifacts.Backend, EventArchive: sqlite.ArchiveBackend}, recordChoice); err != nil {
		t.Fatal(err)
	}
	events, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(restored, backup.ArchiveDir))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	a, err := events.Event(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return string(a.Event.Data.Data)
}

// A scheduled backup of a daemon that has archived Events holds their
// payloads, and rotating bundles out leaves the segments the rest name.
func TestScheduledBackupCarriesTheDaemonsArchive(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := backupDaemon(t, start)
	events, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(d.o.StateDir, backup.ArchiveDir))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	d.events = events
	var newest string
	for i := range 4 {
		at := start.Add(time.Duration(i) * time.Hour)
		retireInto(t, d.backups.db, events, domain.ID("gone-"+at.Format("15")), at)
		if err := d.backupNow(ctx, at); err != nil {
			t.Fatal(err)
		}
		newest = filepath.Join(d.o.BackupDir, bundlePrefix+at.Format(bundleLayout))
	}
	if got := restoredPayload(t, newest, "gone-09", start); got != wiredBody {
		t.Fatalf("restored payload %q", got)
	}
	names, _, err := scheduledBundles(d.o.BackupDir)
	if err != nil || len(names) != 2 {
		t.Fatalf("kept %v, %v", names, err)
	}
	needed := map[string]bool{}
	for _, n := range names {
		segments, err := backup.SegmentNames(filepath.Join(d.o.BackupDir, n))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range segments {
			needed[s] = true
		}
	}
	for _, e := range bundleNames(t, d.o.BackupDir) {
		if strings.HasPrefix(e, "event-segment-") && !needed[e] {
			t.Fatalf("segment %s outlived every bundle naming it", e)
		}
	}
}

// The first upgrade after archiving must still get its bundle, with the
// payloads the older database only has tombstones for.
func TestUpgradeBundleCarriesTheArchive(t *testing.T) {
	ctx := context.Background()
	state := olderState(t)
	func() {
		defer sqlite.SimulateRecordFormat(olderFormat)()
		db, err := sqlite.Backend.Open(ctx, filepath.Join(state, "conduit.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		events, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(state, backup.ArchiveDir))
		if err != nil {
			t.Fatal(err)
		}
		defer events.Close()
		retireInto(t, db, events, "gone", upgradeStart)
	}()
	// Negative control: without the archive the bundle would lose the
	// payload, so the start is refused and the database left as it was.
	aside := filepath.Join(t.TempDir(), "archive")
	if err := os.Rename(filepath.Join(state, backup.ArchiveDir), aside); err != nil {
		t.Fatal(err)
	}
	if err := upgradeDaemon(state, upgradeStart).preserveBeforeUpgrade(ctx); err == nil || !strings.Contains(err.Error(), "not in the event archive") {
		t.Fatalf("upgrade without the archive: %v", err)
	}
	if err := os.Rename(aside, filepath.Join(state, backup.ArchiveDir)); err != nil {
		t.Fatal(err)
	}
	d := upgradeDaemon(state, upgradeStart)
	if err := d.preserveBeforeUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(d.o.BackupDir, upgradeBundlePrefix+upgradeStart.Format(bundleLayout))
	defer sqlite.SimulateRecordFormat(olderFormat)()
	if got := restoredPayload(t, bundle, "gone", upgradeStart); got != wiredBody {
		t.Fatalf("restored payload %q", got)
	}
}

func TestArchivePolicyFollowsTheSettings(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	if _, on := archivePolicy(config.Settings{}, now); on {
		t.Fatal("archiving is on with no horizon set")
	}
	p, on := archivePolicy(config.Settings{ArchiveFinishedDays: 30}, now)
	if !on || !p.FinishedBefore.Equal(now.AddDate(0, 0, -30)) || !p.UnmatchedBefore.Equal(p.FinishedBefore) {
		t.Fatalf("one horizon: %+v %v", p, on)
	}
	p, _ = archivePolicy(config.Settings{ArchiveFinishedDays: 30, ArchiveUnmatchedDays: 7}, now)
	if !p.UnmatchedBefore.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("shorter horizon: %+v", p)
	}
}

type countingArchiver struct {
	calls  int
	wrapAt int
	err    error
}

func (a *countingArchiver) ArchiveEvents(context.Context, runtime.ArchivePolicy, int) (runtime.ArchivePass, error) {
	a.calls++
	if a.err != nil {
		return runtime.ArchivePass{}, a.err
	}
	return runtime.ArchivePass{Scanned: passLimit, Archived: 1, Wrapped: a.calls == a.wrapAt}, nil
}

// A pass stops at the end of the log or its page budget, runs nothing when
// archiving is off, and reports a failure until one succeeds.
func TestArchivePassIsBoundedAndReported(t *testing.T) {
	ctx := context.Background()
	d := backupDaemon(t, time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	d.reportedAt = map[string]time.Time{}
	on := config.Settings{ArchiveFinishedDays: 30}
	off := &countingArchiver{}
	d.runArchivePass(ctx, off, config.Settings{})
	if off.calls != 0 {
		t.Fatalf("ran %d pages with archiving off", off.calls)
	}
	wraps := &countingArchiver{wrapAt: 3}
	d.runArchivePass(ctx, wraps, on)
	if wraps.calls != 3 {
		t.Fatalf("ran %d pages past the end of the log", wraps.calls)
	}
	backlog := &countingArchiver{}
	d.runArchivePass(ctx, backlog, on)
	if backlog.calls != archivePages {
		t.Fatalf("ran %d pages, budget %d", backlog.calls, archivePages)
	}
	failing := &countingArchiver{err: storage.ErrCorrupt}
	d.runArchivePass(ctx, failing, on)
	if failing.calls != 1 || d.reportedCode("event_archive", "") == "" {
		t.Fatalf("failure: %d calls, code %q", failing.calls, d.reportedCode("event_archive", ""))
	}
	d.runArchivePass(ctx, &countingArchiver{wrapAt: 1}, on)
	if code := d.reportedCode("event_archive", ""); code != "" {
		t.Fatalf("still reported after a good pass: %q", code)
	}
}
