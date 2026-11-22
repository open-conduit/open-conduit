package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

var archiveNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

const archivedBody = `{"message":"only the archive holds this"}`

// archiveState is a live database with one retired Event ("gone"), copied to
// its archive first, and one still live ("kept").
type archiveState struct {
	root    string
	db      storage.Database
	files   storage.ArtifactStore
	archive storage.EventArchive
	whole   domain.AcceptedEvent
}

func newArchiveState(t *testing.T) archiveState {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	files, err := artifacts.Open(filepath.Join(root, "live-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	archive, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(root, "live-archive"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { archive.Close() })
	s := archiveState{root: root, db: db, files: files, archive: archive}
	put := func(fn func(storage.Tx) error) {
		t.Helper()
		if err := db.Transact(ctx, storage.Guard{Now: archiveNow}, fn); err != nil {
			t.Fatal(err)
		}
	}
	put(func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		for _, id := range []domain.ID{"gone", "kept"} {
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
		}
		var err error
		s.whole, err = tx.Event("gone")
		return err
	})
	if err := archive.PutEvents(ctx, []domain.AcceptedEvent{s.whole}); err != nil {
		t.Fatal(err)
	}
	put(func(tx storage.Tx) error { return tx.RetireEvent("gone") })
	return s
}

func (s archiveState) create(t *testing.T, events storage.EventArchive) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := CreateWithArchive(context.Background(), bundle, nil, DefaultBackend, s.db, s.files, events, "v1", kvRetention()); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func restoreArchive(t *testing.T, bundle string) (string, error) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "restored")
	err := Restore(context.Background(), bundle, destination, Trust{AcceptUnsigned: true}, archiveNow, nil, lookup, Target{Artifacts: artifacts.Backend, EventArchive: sqlite.ArchiveBackend}, nil)
	return destination, err
}

func TestBundleCarriesTheArchiveThroughTheSnapshot(t *testing.T) {
	ctx := context.Background()
	s := newArchiveState(t)
	// Archived after the snapshot's last sequence, as a pass running during
	// a backup would be. A restored daemon will issue this sequence again.
	later := s.whole
	later.Event.ID, later.Event.DeliveryID, later.Sequence = "later", "later", s.whole.Sequence+100
	if err := s.archive.PutEvents(ctx, []domain.AcceptedEvent{later}); err != nil {
		t.Fatal(err)
	}
	destination, err := restoreArchive(t, s.create(t, s.archive))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(destination, ArchiveDir))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.Event(ctx, "gone")
	if err != nil || !storage.SameAcceptedEvent(got, s.whole) {
		t.Fatalf("restored record = %+v, %v, want %+v", got, err, s.whole)
	}
	if _, err := restored.Event(ctx, "later"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a record past the snapshot was restored: %v", err)
	}
}

func TestBackupRefusesTombstonesItCannotBack(t *testing.T) {
	ctx := context.Background()
	s := newArchiveState(t)
	empty, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "empty"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	for name, events := range map[string]storage.EventArchive{"no archive": nil, "an archive without the record": empty} {
		bundle := filepath.Join(t.TempDir(), "bundle")
		err := CreateWithArchive(ctx, bundle, nil, DefaultBackend, s.db, s.files, events, "v1", kvRetention())
		if err == nil || !strings.Contains(err.Error(), "not in the event archive") {
			t.Fatalf("%s: got %v", name, err)
		}
		if _, statErr := os.Lstat(bundle); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s: published a bundle that lost payloads", name)
		}
	}
}

// The walk has to notice a gap with later records after it, and a record
// that isn't the Event the database retired at that sequence.
func TestBackupRefusesAnArchiveThatMissesATombstoneMidway(t *testing.T) {
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
	other := s.whole
	other.Event.ID, other.Event.DeliveryID = "other", "other"
	for name, c := range map[string]struct {
		held []domain.AcceptedEvent
		want string
	}{
		"gap before a later record": {[]domain.AcceptedEvent{kept}, "not in the event archive"},
		"another Event there":       {[]domain.AcceptedEvent{other, kept}, "different Event"},
	} {
		t.Run(name, func(t *testing.T) {
			archive, err := sqlite.ArchiveBackend.Open(ctx, filepath.Join(t.TempDir(), "archive"))
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			if err := archive.PutEvents(ctx, c.held); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(t.TempDir(), "bundle")
			err = CreateWithArchive(ctx, bundle, nil, DefaultBackend, s.db, s.files, archive, "v1", kvRetention())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if _, statErr := os.Lstat(bundle); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("published a bundle that lost a payload")
			}
		})
	}
}

func TestRestoreRefusesABundleWhoseArchiveDoesNotStandUp(t *testing.T) {
	s := newArchiveState(t)
	edit := func(t *testing.T, bundle string, change func(*manifest)) {
		t.Helper()
		path := filepath.Join(bundle, "manifest.json")
		m, err := readManifest(path)
		if err != nil {
			t.Fatal(err)
		}
		change(&m)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("altered", func(t *testing.T) {
		bundle := s.create(t, s.archive)
		path := filepath.Join(bundle, ArchiveFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Replace(data, []byte("only"), []byte("ONLY"), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := restoreArchive(t, bundle); err == nil || !strings.Contains(err.Error(), "event archive digest mismatch") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("removed_from_the_manifest", func(t *testing.T) {
		bundle := s.create(t, s.archive)
		edit(t, bundle, func(m *manifest) { m.Archive = nil })
		if _, err := restoreArchive(t, bundle); err == nil || !strings.Contains(err.Error(), "not in the event archive") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("from_another_snapshot", func(t *testing.T) {
		bundle := s.create(t, s.archive)
		edit(t, bundle, func(m *manifest) { m.Archive.Through++ })
		if _, err := restoreArchive(t, bundle); err == nil || !strings.Contains(err.Error(), "taken through sequence") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("nowhere_to_put_it", func(t *testing.T) {
		bundle := s.create(t, s.archive)
		destination := filepath.Join(t.TempDir(), "restored")
		err := Restore(context.Background(), bundle, destination, Trust{AcceptUnsigned: true}, archiveNow, nil, lookup, Target{Artifacts: artifacts.Backend}, nil)
		if !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("got %v", err)
		}
		if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("published a restore without its archive")
		}
	})
}

// A bundle with no archive names none, so a binary from before archives,
// which refuses unknown manifest fields, still reads it.
func TestABundleWithoutAnArchiveStaysReadableByOlderBinaries(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(formatBundle(t, "v1"), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"Archive"`)) {
		t.Fatalf("manifest names an archive: %s", data)
	}
}
