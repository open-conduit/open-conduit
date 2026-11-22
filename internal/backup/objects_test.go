package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/storage"
)

// attach commits content and references it from a new Execution.
func (s archiveState) attach(t *testing.T, id, content string) domain.ArtifactRef {
	t.Helper()
	ctx := context.Background()
	w, err := s.files.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatal(err)
	}
	ref, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transact(ctx, storage.Guard{Now: archiveNow}, func(tx storage.Tx) error {
		work := domain.ID("work-" + id)
		if err := tx.PutWork(domain.Work{ID: work, Kind: "fixture.work", State: domain.WorkReady, ScopeID: "scope"}, 0); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: domain.ID("run-" + id), WorkID: work, ExecutorBinding: "shell", State: domain.ExecutionPending, Attempt: 1, TriggerID: "kept", Artifacts: []domain.ArtifactRef{ref}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	return ref
}

func (s archiveState) scheduled(t *testing.T, parent, name string) string {
	t.Helper()
	bundle := filepath.Join(parent, name)
	if err := CreateScheduled(context.Background(), bundle, nil, DefaultBackend, s.db, s.files, s.archive, "v1", kvRetention()); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func objectPath(parent string, ref domain.ArtifactRef) string {
	return filepath.Join(parent, objectPrefix+ref.SHA256, ObjectFile)
}

func restoredArtifact(t *testing.T, destination string, ref domain.ArtifactRef) string {
	t.Helper()
	store, err := artifacts.OpenReadOnly(filepath.Join(destination, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err := store.Open(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A later scheduled bundle stores only what is new, and each restores all of
// its own artifacts.
func TestScheduledBundlesShareTheirArtifacts(t *testing.T) {
	s := newArchiveState(t)
	old := s.attach(t, "old", "an artifact every bundle needs")
	parent := t.TempDir()
	first := s.scheduled(t, parent, "first")
	before, err := os.Stat(objectPath(parent, old))
	if err != nil {
		t.Fatal(err)
	}
	fresh := s.attach(t, "new", "an artifact only the second needs")
	second := s.scheduled(t, parent, "second")
	after, err := os.Stat(objectPath(parent, old))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the second bundle wrote the object it could reuse")
	}
	if held, err := os.ReadDir(filepath.Join(second, "artifacts", "objects")); err != nil || len(held) != 0 {
		t.Fatalf("the bundle holds its own artifacts: %v, %v", held, err)
	}
	info, err := Inspect(second)
	if err != nil || info.Artifacts != 2 {
		t.Fatalf("inspect = %+v, %v", info, err)
	}
	destination, err := restoreArchive(t, first)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoredArtifact(t, destination, old); got != "an artifact every bundle needs" {
		t.Fatalf("first restored %q", got)
	}
	destination, err = restoreArchive(t, second)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoredArtifact(t, destination, fresh); got != "an artifact only the second needs" {
		t.Fatalf("second restored %q", got)
	}
}

// A damaged object is never reused: the next bundle replaces it from the live
// store, which repairs the earlier bundle too. Until then a restore refuses.
func TestDamagedObjectIsRefusedAndReplaced(t *testing.T) {
	s := newArchiveState(t)
	ref := s.attach(t, "one", "content worth keeping")
	parent := t.TempDir()
	first := s.scheduled(t, parent, "first")
	if err := os.WriteFile(objectPath(parent, ref), []byte("content worth k33ping"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreArchive(t, first); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("restored a damaged object: %v", err)
	}
	s.scheduled(t, parent, "second")
	destination, err := restoreArchive(t, first)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoredArtifact(t, destination, ref); got != "content worth keeping" {
		t.Fatalf("restored %q", got)
	}
	if err := os.RemoveAll(filepath.Dir(objectPath(parent, ref))); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreArchive(t, first); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("restored without its object: %v", err)
	}
}

// A held object doesn't hide content that has gone from the live store.
func TestScheduledBundleRefusesMissingLiveContent(t *testing.T) {
	s := newArchiveState(t)
	ref := s.attach(t, "one", "content")
	parent := t.TempDir()
	s.scheduled(t, parent, "first")
	if err := s.files.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	err := CreateScheduled(context.Background(), filepath.Join(parent, "second"), nil, DefaultBackend, s.db, s.files, s.archive, "v1", kvRetention())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// An object stays until no bundle names it, and an unreadable bundle stops
// pruning, since it might need any of them.
func TestPruneKeepsObjectsABundleNames(t *testing.T) {
	s := newArchiveState(t)
	ref := s.attach(t, "one", "content")
	parent := t.TempDir()
	first := s.scheduled(t, parent, "first")
	second := s.scheduled(t, parent, "second")
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneShared(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(objectPath(parent, ref)); err != nil {
		t.Fatalf("pruned an object a kept bundle names: %v", err)
	}
	manifest := filepath.Join(second, "manifest.json")
	good, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := PruneShared(parent); err == nil || len(removed) != 0 {
		t.Fatalf("pruned past an unreadable bundle: %v, %v", removed, err)
	}
	if err := os.WriteFile(manifest, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(second); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneShared(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(removed), objectPrefix+ref.SHA256) {
		t.Fatalf("removed = %v", removed)
	}
}

// A bundle made to be carried elsewhere holds its artifacts, so it restores
// after moving without anything beside it and names no objects.
func TestCarriedBundleHoldsItsArtifacts(t *testing.T) {
	s := newArchiveState(t)
	ref := s.attach(t, "one", "portable content")
	parent := t.TempDir()
	s.scheduled(t, parent, "scheduled")
	bundle := s.createIn(t, parent, "by-hand", s.archive)
	m, err := readManifest(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.SharedArtifacts) != 0 || len(m.Artifacts) != 1 {
		t.Fatalf("manifest artifacts = %d held, %d shared", len(m.Artifacts), len(m.SharedArtifacts))
	}
	if err := os.RemoveAll(filepath.Dir(objectPath(parent, ref))); err != nil {
		t.Fatal(err)
	}
	destination, err := restoreArchive(t, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if got := restoredArtifact(t, destination, ref); got != "portable content" {
		t.Fatalf("restored %q", got)
	}
}

// A digest becomes a path, and an unsigned manifest is whatever someone wrote.
func TestObjectDigestCannotLeaveTheDirectory(t *testing.T) {
	for _, digest := range []string{"../../secret", strings.Repeat("A", 64), "", strings.Repeat("a", 63)} {
		if err := checkObjects([]entry{{SHA256: digest}}); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("digest %q: %v", digest, err)
		}
	}
	if err := checkObjects([]entry{{SHA256: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
}
