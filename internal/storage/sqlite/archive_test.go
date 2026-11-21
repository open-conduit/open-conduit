package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestWeakenedEventArchivesAreRejected(t *testing.T) {
	for _, negative := range []struct{ mode, name, diagnostic string }{
		{"payload", "keeps_the_whole_record", "archived record changed"},
		{"overwrite", "put_again_is_nil_and_a_different_record_conflicts", "a different record under a held ID"},
		{"tombstone", "refuses_a_tombstone_or_unrouted_event_and_stores_none_of_the_batch", "archiving a tombstone"},
		{"time", "reads_merge_periods_in_sequence_order", "out of sequence order"},
		{"named", "work_events_follow_naming_and_routing", "work events"},
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

func TestEventArchiveRefusesANewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "archive")
	a, err := ArchiveBackend.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	impl := a.(*eventArchive)
	db, err := impl.period(ctx, "2026-03")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = ArchiveBackend.Open(ctx, dir); !errors.Is(err, storage.ErrSchemaNewer) {
		t.Fatalf("newer archive file: got %v, want schema newer", err)
	}
	info, err := os.Stat(filepath.Join(dir, "events-2026-03.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("archive file mode %v, want 0600", info.Mode().Perm())
	}
}
