package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// retire adds n Events, archived and then retired, to the state.
func (s archiveState) retire(t *testing.T, prefix string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		id := domain.ID(fmt.Sprintf("%s-%d", prefix, i))
		var record domain.AcceptedEvent
		if err := s.db.Transact(ctx, storage.Guard{Now: archiveNow}, func(tx storage.Tx) error {
			e := domain.Event{ID: id, Type: "fixture.seen", Source: "fixture", DeliveryID: string(id), OccurredAt: archiveNow, ReceivedAt: archiveNow, Data: domain.Payload{Schema: "fixture.v1", Data: []byte(archivedBody)}}
			if _, _, err := tx.AcceptEvent(e); err != nil {
				return err
			}
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
		if err := s.archive.PutEvents(ctx, []domain.AcceptedEvent{record}); err != nil {
			t.Fatal(err)
		}
		if err := s.db.Transact(ctx, storage.Guard{Now: archiveNow}, func(tx storage.Tx) error { return tx.RetireEvent(id) }); err != nil {
			t.Fatal(err)
		}
	}
}

func (s archiveState) createIn(t *testing.T, parent, name string, events storage.EventArchive) string {
	t.Helper()
	bundle := filepath.Join(parent, name)
	if err := CreateWithArchive(context.Background(), bundle, nil, DefaultBackend, s.db, s.files, events, "v1", kvRetention()); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func bundleSegments(t *testing.T, bundle string) []segmentEntry {
	t.Helper()
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Archive == nil {
		t.Fatal("the bundle names no archive")
	}
	return m.Archive.Segments
}

func restoredRecords(t *testing.T, destination string) uint64 {
	t.Helper()
	ctx := context.Background()
	restored, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(destination, ArchiveDir))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count, after uint64
	for {
		page, err := restored.Events(ctx, after, archivePage)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			count++
			after = a.Sequence
		}
		if len(page) < archivePage {
			return count
		}
	}
}

// A later bundle names the earlier one's segment and writes only what is new,
// and each still restores all of its own history.
func TestBundlesShareTheirSegments(t *testing.T) {
	s := newArchiveState(t)
	s.retire(t, "first", 7)
	parent := t.TempDir()
	first := s.createIn(t, parent, "first", s.archive)
	before := bundleSegments(t, first)
	if len(before) != 1 || before[0].Records != 8 {
		t.Fatalf("first bundle's segments = %+v", before)
	}
	s.retire(t, "second", 1)
	second := s.createIn(t, parent, "second", s.archive)
	after := bundleSegments(t, second)
	if len(after) != 2 || after[0] != before[0] || after[1].Records != 1 {
		t.Fatalf("second bundle's segments = %+v, want the first's and one record", after)
	}
	for bundle, want := range map[string]uint64{first: 8, second: 9} {
		destination, err := restoreArchive(t, bundle)
		if err != nil {
			t.Fatal(err)
		}
		if got := restoredRecords(t, destination); got != want {
			t.Fatalf("%s restored %d records, want %d", filepath.Base(bundle), got, want)
		}
	}
}

// However many backups run, a restore reads a logarithmic number of segments.
func TestSegmentsStayFewAsBackupsAccumulate(t *testing.T) {
	s := newArchiveState(t)
	parent := t.TempDir()
	var bundle string
	const backups = 33
	for i := range backups {
		s.retire(t, fmt.Sprintf("round%d", i), 1)
		bundle = s.createIn(t, parent, fmt.Sprintf("b%02d", i), s.archive)
		segments := bundleSegments(t, bundle)
		var records uint64
		for _, seg := range segments {
			records += seg.Records
		}
		if records != uint64(i+2) {
			t.Fatalf("backup %d names %d records", i, records)
		}
		if limit := bits.Len64(records); len(segments) > limit {
			t.Fatalf("backup %d names %d segments for %d records", i, len(segments), records)
		}
	}
	destination, err := restoreArchive(t, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoredRecords(t, destination); got != backups+1 {
		t.Fatalf("restored %d records", got)
	}
}

// After a restore to an earlier point, a kept segment can hold sequences the
// archive no longer has, which the daemon may issue to different Events.
func TestASegmentTheArchiveNoLongerMatchesIsNotReused(t *testing.T) {
	ctx := context.Background()
	s := newArchiveState(t)
	var kept domain.AcceptedEvent
	if err := s.db.Transact(ctx, storage.Guard{Now: archiveNow}, func(tx storage.Tx) error {
		var err error
		kept, err = tx.Event("kept")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.archive.PutEvents(ctx, []domain.AcceptedEvent{kept}); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	earlier := bundleSegments(t, s.createIn(t, parent, "earlier", s.archive))
	rewound, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "rewound"))
	if err != nil {
		t.Fatal(err)
	}
	defer rewound.Close()
	if err := rewound.PutEvents(ctx, []domain.AcceptedEvent{s.whole}); err != nil {
		t.Fatal(err)
	}
	bundle := s.createIn(t, parent, "later", rewound)
	segments := bundleSegments(t, bundle)
	if len(segments) != 1 || segments[0] == earlier[0] || segments[0].Records != 1 {
		t.Fatalf("segments = %+v, reused %+v", segments, earlier)
	}
	destination, err := restoreArchive(t, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoredRecords(t, destination); got != 1 {
		t.Fatalf("restored %d records, want the rewound archive's 1", got)
	}
}

// The same sequence can hold a different record once a daemon has gone on
// from an earlier restore, so a segment is compared and not just counted.
func TestASegmentWithADifferentRecordAtItsSequenceIsNotReused(t *testing.T) {
	ctx := context.Background()
	s := newArchiveState(t)
	parent := t.TempDir()
	earlier := bundleSegments(t, s.createIn(t, parent, "earlier", s.archive))
	changed := s.whole
	changed.Disposition = "reissued"
	other, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.PutEvents(ctx, []domain.AcceptedEvent{changed}); err != nil {
		t.Fatal(err)
	}
	bundle := s.createIn(t, parent, "later", other)
	if segments := bundleSegments(t, bundle); len(segments) != 1 || segments[0] == earlier[0] {
		t.Fatalf("segments = %+v, reused %+v", segments, earlier)
	}
	destination, err := restoreArchive(t, bundle)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(destination, ArchiveDir))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, err := restored.Event(ctx, "gone"); err != nil || got.Disposition != "reissued" {
		t.Fatalf("restored %+v, %v", got, err)
	}
}

// A damaged segment isn't vouched for again: its records are written afresh.
func TestADamagedSegmentIsWrittenAgain(t *testing.T) {
	s := newArchiveState(t)
	parent := t.TempDir()
	first := s.createIn(t, parent, "first", s.archive)
	path := filepath.Join(parent, bundleSegments(t, first)[0].name(), ArchiveFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "only", "ONLY", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreArchive(t, first); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("restore of the damaged segment: %v", err)
	}
	second := s.createIn(t, parent, "second", s.archive)
	if _, err := restoreArchive(t, second); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRefusesASegmentItCannotTrust(t *testing.T) {
	s := newArchiveState(t)
	t.Run("missing", func(t *testing.T) {
		parent := t.TempDir()
		bundle := s.createIn(t, parent, "bundle", s.archive)
		if err := os.RemoveAll(filepath.Join(parent, bundleSegments(t, bundle)[0].name())); err != nil {
			t.Fatal(err)
		}
		destination, err := restoreArchive(t, bundle)
		if err == nil || !strings.Contains(err.Error(), "is missing") {
			t.Fatalf("got %v", err)
		}
		if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("published a restore with part of its history")
		}
	})
	for name, digest := range map[string]string{
		"a path":     "../../escape",
		"not a hash": "abc",
	} {
		t.Run(name, func(t *testing.T) {
			bundle := s.createIn(t, t.TempDir(), "bundle", s.archive)
			path := filepath.Join(bundle, "manifest.json")
			m, err := readManifest(path)
			if err != nil {
				t.Fatal(err)
			}
			m.Archive.Segments[0].SHA256 = digest
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := restoreArchive(t, bundle); !errors.Is(err, storage.ErrInvalid) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPruneKeepsWhatABundleNames(t *testing.T) {
	s := newArchiveState(t)
	parent := t.TempDir()
	first := s.createIn(t, parent, "first", s.archive)
	// Equal sizes merge, so the second bundle names a new segment alone and
	// the one-record segment written on the way to it is named by nothing.
	s.retire(t, "more", 1)
	second := s.createIn(t, parent, "second", s.archive)
	onlyFirst := bundleSegments(t, first)[0].name()
	if slices.ContainsFunc(bundleSegments(t, second), func(e segmentEntry) bool { return e.name() == onlyFirst }) {
		t.Fatal("fixture: the bundles should not share this segment")
	}
	removed, err := PruneShared(parent)
	if err != nil || len(removed) != 1 || removed[0] == onlyFirst || removed[0] == bundleSegments(t, second)[0].name() {
		t.Fatalf("pruned %v, %v, want only the segment no bundle names", removed, err)
	}
	// One unreadable manifest might name anything, so nothing goes.
	if err := os.WriteFile(filepath.Join(first, "manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := PruneShared(parent); err == nil || len(removed) != 0 {
		t.Fatalf("pruned %v, %v past an unreadable manifest", removed, err)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	removed, err = PruneShared(parent)
	if err != nil || !slices.Equal(removed, []string{onlyFirst}) {
		t.Fatalf("pruned %v, %v, want %s", removed, err, onlyFirst)
	}
	if _, err := restoreArchive(t, second); err != nil {
		t.Fatal(err)
	}
}
