package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

func archiveHarness(b storage.EventArchiveBackend) conformance.EventArchiveHarness {
	return conformance.EventArchiveHarness{Backend: b, Location: func(_ *testing.T, dir string) string { return filepath.Join(dir, "archive") }}
}

func TestEventArchiveConformance(t *testing.T) {
	conformance.RunEventArchive(t, archiveHarness(ArchiveBackend))
}

// weakenedArchive changes one guarantee of the real archive.
type weakenedArchive struct {
	storage.EventArchive
	mode string
}

type weakenedArchiveBackend struct {
	mode string
	dirs *[]string
}

func (b weakenedArchiveBackend) Open(ctx context.Context, location string) (storage.EventArchive, error) {
	if b.mode == "forgets" {
		*b.dirs = append(*b.dirs, location)
		location += "-" + strings.Repeat("x", len(*b.dirs))
	}
	a, err := ArchiveBackend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return weakenedArchive{a, b.mode}, nil
}

func (a weakenedArchive) PutEvents(ctx context.Context, events []domain.AcceptedEvent) error {
	if a.mode == "encoding" {
		for _, e := range events {
			held, err := a.EventArchive.Event(ctx, e.Event.ID)
			if err != nil {
				continue
			}
			x, _ := json.Marshal(held)
			y, _ := json.Marshal(e)
			if !bytes.Equal(x, y) {
				return storage.ErrConflict
			}
		}
	}
	events = slices.Clone(events)
	for i := range events {
		switch a.mode {
		case "payload":
			events[i].Event.Data = domain.Payload{}
		case "tombstone":
			events[i].Retired = false
		}
	}
	err := a.EventArchive.PutEvents(ctx, events)
	if a.mode == "overwrite" && errors.Is(err, storage.ErrConflict) {
		return nil
	}
	return err
}

func (a weakenedArchive) Events(ctx context.Context, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if a.mode != "time" {
		return a.EventArchive.Events(ctx, after, limit)
	}
	out, err := a.EventArchive.Events(ctx, after, 0)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(x, y domain.AcceptedEvent) int { return x.Event.ReceivedAt.Compare(y.Event.ReceivedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (a weakenedArchive) WorkEvents(ctx context.Context, work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	out, err := a.EventArchive.WorkEvents(ctx, work, after, limit)
	if a.mode == "named" {
		out = slices.DeleteFunc(out, func(e domain.AcceptedEvent) bool { return e.Event.WorkID != work })
	}
	return out, err
}

func (a weakenedArchive) ScopeEvents(ctx context.Context, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if a.mode == "scope" {
		return a.EventArchive.Events(ctx, after, limit)
	}
	return a.EventArchive.ScopeEvents(ctx, scopes, after, limit)
}

func (a weakenedArchive) TypeEvents(ctx context.Context, eventType string, scopes []domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if a.mode == "type" {
		return a.EventArchive.ScopeEvents(ctx, append(scopes, "scope", "other", ""), after, limit)
	}
	return a.EventArchive.TypeEvents(ctx, eventType, scopes, after, limit)
}

func TestWeakenedEventArchivesAreRejected(t *testing.T) {
	for _, negative := range []struct{ mode, name, diagnostic string }{
		{"payload", "keeps_the_whole_record", "archived record changed"},
		{"encoding", "put_again_is_nil_and_a_different_record_conflicts", "the same record in another zone"},
		{"overwrite", "put_again_is_nil_and_a_different_record_conflicts", "a different record under a held ID"},
		{"tombstone", "refuses_a_tombstone_or_unrouted_event_and_stores_none_of_the_batch", "archiving a tombstone"},
		{"time", "reads_merge_periods_in_sequence_order", "out of sequence order"},
		{"named", "work_events_follow_naming_and_routing", "work events"},
		{"scope", "scope_and_type_reads_select_by_the_record", "scope events"},
		{"type", "scope_and_type_reads_select_by_the_record", "type events"},
		{"forgets", "records_survive_reopen", "records after reopen"},
	} {
		t.Run(negative.mode, func(t *testing.T) {
			var dirs []string
			for _, c := range conformance.EventArchiveCases() {
				if c.Name != negative.name {
					continue
				}
				err := c.Check(t, archiveHarness(weakenedArchiveBackend{negative.mode, &dirs}))
				if err == nil || !strings.Contains(err.Error(), negative.diagnostic) {
					t.Fatalf("wanted rejection containing %q, got %v", negative.diagnostic, err)
				}
				return
			}
			t.Fatal("negative control did not select a real case")
		})
	}
}

func TestEventArchiveHasOneOwner(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "archive")
	a, err := ArchiveBackend.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = ArchiveBackend.Open(ctx, dir); !errors.Is(err, storage.ErrInUse) {
		t.Fatalf("second owner: got %v, want in use", err)
	}
	if _, err = ArchiveBackend.Open(ctx, "relative/archive"); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("relative location: got %v, want invalid", err)
	}
}

func archiveRecord(id domain.ID, sequence uint64, at time.Time) domain.AcceptedEvent {
	return domain.AcceptedEvent{Event: domain.Event{ID: id, Type: "test.archived.v1", Source: "test", DeliveryID: string(id), OccurredAt: at, ReceivedAt: at}, Sequence: sequence, Routed: true, Disposition: "unmatched"}
}

func TestEventArchiveRefusesANewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "archive")
	a, err := ArchiveBackend.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err = a.PutEvents(ctx, []domain.AcceptedEvent{archiveRecord("a", 1, at)}); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events-2026-03.db")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("archive file mode %v, want 0600", info.Mode().Perm())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if _, err = ArchiveBackend.Open(ctx, dir); !errors.Is(err, storage.ErrSchemaNewer) {
		t.Fatalf("newer archive file: got %v, want schema newer", err)
	}
}

// TestEventArchiveReadsOnlyThePeriodsItNeeds holds the cost of a read and of
// the conflict check to the months the records are in, not the archive's age.
func TestEventArchiveReadsOnlyThePeriodsItNeeds(t *testing.T) {
	ctx := context.Background()
	opened, err := ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	a := opened.(*eventArchive)
	start := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	var all []domain.AcceptedEvent
	for i := range 24 {
		all = append(all, archiveRecord(domain.ID("month-"+string(rune('a'+i))), uint64(i+1), start.AddDate(0, i, 0)))
	}
	if err = a.PutEvents(ctx, all); err != nil {
		t.Fatal(err)
	}
	reads := func(name string, want int64, fn func() error) {
		t.Helper()
		before := a.periodReads.Load()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := a.periodReads.Load() - before; got != want {
			t.Errorf("%s opened %d periods, want %d", name, got, want)
		}
	}
	reads("one event", 1, func() error { _, e := a.Event(ctx, all[5].Event.ID); return e })
	reads("a page of one", 1, func() error { _, e := a.Events(ctx, 3, 1); return e })
	reads("an unknown event", 0, func() error {
		if _, e := a.Event(ctx, "missing"); !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		return nil
	})
	reads("a new record", 0, func() error {
		return a.PutEvents(ctx, []domain.AcceptedEvent{archiveRecord("new", 100, start)})
	})
	reads("a held record again", 1, func() error { return a.PutEvents(ctx, all[7:8]) })
}

// TestEventArchiveCommitsIndexAndRecordTogether makes the insert into the
// month's file fail after the index took the batch's first entry: neither
// file may keep any of it.
func TestEventArchiveCommitsIndexAndRecordTogether(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "archive")
	a, err := ArchiveBackend.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err = a.PutEvents(ctx, []domain.AcceptedEvent{archiveRecord("first", 1, at)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events-2026-03.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "INSERT INTO events(sequence,id,record) VALUES(9,'stray','{}')"); err != nil {
		t.Fatal(err)
	}
	batch := []domain.AcceptedEvent{archiveRecord("kept-out", 8, at), archiveRecord("clashes", 9, at)}
	if err = a.PutEvents(ctx, batch); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("clashing insert: got %v, want conflict", err)
	}
	if _, err = a.Event(ctx, "kept-out"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the index kept part of a failed batch: %v", err)
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE id='kept-out'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the month kept part of a failed batch: %d, %v", n, err)
	}
}

// TestEventArchiveReadsAlongsideWrites reads while batches commit across two
// months: every entry a read finds must have its record.
func TestEventArchiveReadsAlongsideWrites(t *testing.T) {
	ctx := context.Background()
	a, err := ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	at := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	const batches, size = 20, 5
	done := make(chan error, 1)
	go func() {
		for b := range batches {
			var batch []domain.AcceptedEvent
			for i := range size {
				sequence := uint64(b*size + i + 1)
				batch = append(batch, archiveRecord(domain.ID(fmt.Sprint("event-", sequence)), sequence, at.AddDate(0, i%2, 0)))
			}
			if err := a.PutEvents(ctx, batch); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for finished := false; !finished; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			finished = true
		default:
		}
		page, err := a.Events(ctx, 0, 0)
		if err != nil {
			t.Fatalf("read during a write: %v", err)
		}
		for i := 1; i < len(page); i++ {
			if page[i-1].Sequence >= page[i].Sequence {
				t.Fatalf("page out of order at %d", i)
			}
		}
	}
	page, err := a.Events(ctx, 0, 0)
	if err != nil || len(page) != batches*size {
		t.Fatalf("after the writes: %d records, %v", len(page), err)
	}
}
