package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/storage"
)

// The artifact suite proves a storage.ArtifactBackend and the
// storage.ArtifactStore it opens (#695): commit and abort, the size and
// digest a ref carries and every read checks, no partial reads,
// idempotent delete, listing for collection, and the import a restore
// relies on. It needs nothing of a backend but the contract and a way to
// damage stored bytes, so an object-store backend runs it as the file
// store does.

// ArtifactHarness is what the artifact suite needs from a backend's own
// test beside the backend itself.
type ArtifactHarness struct {
	Backend storage.ArtifactBackend
	// Location names a fresh, empty store for the backend, using dir (a
	// private temporary directory) for any files.
	Location func(t *testing.T, dir string) string
	// Corrupt changes the stored bytes of ref at location without
	// changing their length. The store at location is closed.
	Corrupt func(t *testing.T, location string, ref storage.ArtifactRef)
}

// ArtifactCase is one artifact guarantee.
type ArtifactCase struct {
	Name  string
	Check func(*testing.T, ArtifactHarness) error
}

// RunArtifacts runs every artifact case against h.
func RunArtifacts(t *testing.T, h ArtifactHarness) {
	t.Helper()
	for _, c := range ArtifactCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(t, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func ArtifactCases() []ArtifactCase {
	return []ArtifactCase{
		{"commit_returns_size_media_type_and_digest", artifactCommitRef},
		{"abort_publishes_nothing", artifactAbort},
		{"uncommitted_content_is_never_read_or_listed", artifactNoPartialReads},
		{"commit_and_close_happen_once", artifactOnce},
		{"open_refuses_a_ref_that_does_not_match", artifactRefChecked},
		{"read_verifies_size_and_digest", artifactDigestVerified},
		{"committed_content_survives_reopen", artifactReopen},
		{"delete_is_idempotent_and_checks_the_ref", artifactDelete},
		{"list_pages_committed_content_once_in_id_order", artifactList},
		{"import_publishes_exactly_the_ref_or_nothing", artifactImport},
		{"identical_content_gets_a_new_id", artifactFreshID},
	}
}

var artifactCtx = context.Background()

func openArtifacts(t *testing.T, h ArtifactHarness) (storage.ArtifactStore, string, error) {
	location := h.Location(t, t.TempDir())
	s, err := h.Backend.Open(artifactCtx, location)
	if err != nil {
		return nil, location, fmt.Errorf("open an empty store: %w", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, location, nil
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func publishArtifact(s storage.Artifacts, mediaType, content string) (storage.ArtifactRef, error) {
	w, err := s.Create(artifactCtx, mediaType)
	if err != nil {
		return storage.ArtifactRef{}, fmt.Errorf("create: %w", err)
	}
	if _, err = io.WriteString(w, content); err != nil {
		_ = w.Close()
		return storage.ArtifactRef{}, fmt.Errorf("write: %w", err)
	}
	ref, err := w.Commit(artifactCtx)
	if err != nil {
		return ref, fmt.Errorf("commit: %w", err)
	}
	return ref, nil
}

// readArtifact reads ref to EOF, the only read that proves its content.
func readArtifact(s storage.Artifacts, ref storage.ArtifactRef) (string, error) {
	r, err := s.Open(artifactCtx, ref)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(r)
	return string(b), errors.Join(err, r.Close())
}

// listAll reads every page of s, limit entries at a time.
func listAll(s storage.ArtifactStore, limit int) ([]storage.StoredArtifact, error) {
	var out []storage.StoredArtifact
	after := ""
	for pages := 0; ; pages++ {
		if pages > 10000 {
			return out, fmt.Errorf("list never reached an empty page")
		}
		page, err := s.List(artifactCtx, after, limit)
		if err != nil {
			return out, fmt.Errorf("list after %q: %w", after, err)
		}
		if len(page) > limit {
			return out, fmt.Errorf("a page of %d entries for a limit of %d", len(page), limit)
		}
		if len(page) == 0 {
			return out, nil
		}
		for _, a := range page {
			if a.Ref.ID <= after {
				return out, fmt.Errorf("listed %q after %q: not in ID order", a.Ref.ID, after)
			}
			after = a.Ref.ID
		}
		out = append(out, page...)
	}
}

func listedIDs(s storage.ArtifactStore) (map[string]storage.StoredArtifact, error) {
	all, err := listAll(s, 1000)
	out := map[string]storage.StoredArtifact{}
	for _, a := range all {
		out[a.Ref.ID] = a
	}
	return out, err
}

// artifactCommitRef: Commit's ref carries the content's size, its media
// type and its SHA-256, and reads back exactly what was written, including
// empty content.
func artifactCommitRef(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	for _, content := range []string{"transcript line\n", ""} {
		ref, err := publishArtifact(s, "application/x-ndjson", content)
		if err != nil {
			return err
		}
		if ref.ID == "" || ref.Size != int64(len(content)) || ref.MediaType != "application/x-ndjson" {
			return fmt.Errorf("commit returned %+v for %d bytes of application/x-ndjson", ref, len(content))
		}
		if ref.SHA256 != digestOf(content) {
			return fmt.Errorf("commit returned digest %q, want the content's SHA-256 %q", ref.SHA256, digestOf(content))
		}
		got, err := readArtifact(s, ref)
		if err != nil || got != content {
			return fmt.Errorf("read back %q, %v; want %q", got, err, content)
		}
	}
	return nil
}

// artifactAbort: Close without Commit publishes nothing and leaves nothing
// listed.
func artifactAbort(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	w, err := s.Create(artifactCtx, "text/plain")
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err = io.WriteString(w, "abandoned"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	if _, err = w.Commit(artifactCtx); err == nil {
		return fmt.Errorf("an aborted writer committed")
	}
	listed, err := listedIDs(s)
	if err != nil {
		return err
	}
	if len(listed) != 0 {
		return fmt.Errorf("an aborted writer's content was listed: %v", listed)
	}
	return nil
}

// artifactNoPartialReads: while a writer is unfinished, nothing it wrote is
// listed, and the only way to name it, the ref, doesn't exist yet. Content
// another writer commits meanwhile is unaffected.
func artifactNoPartialReads(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	w, err := s.Create(artifactCtx, "text/plain")
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err = io.WriteString(w, "half of it"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	other, err := publishArtifact(s, "text/plain", "complete")
	if err != nil {
		return err
	}
	listed, err := listedIDs(s)
	if err != nil {
		return err
	}
	if len(listed) != 1 || listed[other.ID].Ref != other {
		return fmt.Errorf("with one writer unfinished and one committed, listed %v, want only %s", listed, other.ID)
	}
	if _, err = io.WriteString(w, ", then the rest"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	ref, err := w.Commit(artifactCtx)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if got, err := readArtifact(s, ref); err != nil || got != "half of it, then the rest" {
		return fmt.Errorf("read back %q, %v", got, err)
	}
	return nil
}

// artifactOnce: a writer commits once; a second Commit, a Close after it
// and a write after it all fail, and change nothing.
func artifactOnce(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	w, err := s.Create(artifactCtx, "text/plain")
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err = io.WriteString(w, "once"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	ref, err := w.Commit(artifactCtx)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if _, err = w.Commit(artifactCtx); err == nil {
		return fmt.Errorf("a second commit succeeded")
	}
	if _, err = w.Write([]byte("more")); err == nil {
		return fmt.Errorf("a write after commit succeeded")
	}
	if err = w.Close(); err == nil {
		return fmt.Errorf("a close after commit succeeded")
	}
	if got, err := readArtifact(s, ref); err != nil || got != "once" {
		return fmt.Errorf("after the extra calls, read %q, %v", got, err)
	}
	return nil
}

// artifactFreshID: every commit gets its own ID, even for the same bytes,
// and deleting one leaves the other. The host releases content by ID, so a
// store that deduplicated under one ID would lose content still in use.
func artifactFreshID(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	publish := func() (storage.ArtifactRef, error) {
		w, err := s.Create(artifactCtx, "text/plain")
		if err != nil {
			return storage.ArtifactRef{}, fmt.Errorf("create: %w", err)
		}
		if _, err = io.WriteString(w, "same bytes"); err != nil {
			return storage.ArtifactRef{}, fmt.Errorf("write: %w", err)
		}
		return w.Commit(artifactCtx)
	}
	a, err := publish()
	if err != nil {
		return err
	}
	b, err := publish()
	if err != nil {
		return err
	}
	if a.ID == b.ID {
		return fmt.Errorf("two commits of the same bytes share the ID %s", a.ID)
	}
	if err = s.Delete(artifactCtx, a); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if got, err := readArtifact(s, b); err != nil || got != "same bytes" {
		return fmt.Errorf("after deleting its twin, read %q, %v", got, err)
	}
	return nil
}

// artifactRefChecked: Open takes only the ref Commit returned. Another
// size, media type or digest is ErrInvalid; an unknown ID is ErrNotFound.
// A ref with no digest, as recorded before refs carried one, still opens
// and is verified by what the store recorded.
func artifactRefChecked(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	ref, err := publishArtifact(s, "text/plain", "checked")
	if err != nil {
		return err
	}
	size, media, digest := ref, ref, ref
	size.Size++
	media.MediaType = "application/octet-stream"
	digest.SHA256 = digestOf("something else")
	for name, wrong := range map[string]storage.ArtifactRef{"size": size, "media type": media, "digest": digest} {
		if r, err := s.Open(artifactCtx, wrong); !errors.Is(err, storage.ErrInvalid) {
			if err == nil {
				r.Close()
			}
			return fmt.Errorf("open with another %s: got %v, want ErrInvalid", name, err)
		}
	}
	unknown, err := publishArtifact(s, "text/plain", "to delete")
	if err != nil {
		return err
	}
	if err = s.Delete(artifactCtx, unknown); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if _, err = s.Open(artifactCtx, unknown); !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("open of deleted content: got %v, want ErrNotFound", err)
	}
	legacy := ref
	legacy.SHA256 = ""
	if got, err := readArtifact(s, legacy); err != nil || got != "checked" {
		return fmt.Errorf("open of a ref without a digest: read %q, %v", got, err)
	}
	return nil
}

// artifactDigestVerified: stored bytes changed in place, same length, are
// ErrCorrupt by the end of the read, whether the ref carries the digest
// or not.
func artifactDigestVerified(t *testing.T, h ArtifactHarness) error {
	s, location, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	ref, err := publishArtifact(s, "text/plain", "the original bytes")
	if err != nil {
		return err
	}
	if err = s.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	h.Corrupt(t, location, ref)
	s, err = h.Backend.Open(artifactCtx, location)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	legacy := ref
	legacy.SHA256 = ""
	for _, r := range []storage.ArtifactRef{ref, legacy} {
		got, err := readArtifact(s, r)
		if !errors.Is(err, storage.ErrCorrupt) {
			return fmt.Errorf("read of damaged content gave %q, %v; want ErrCorrupt", got, err)
		}
	}
	return nil
}

// artifactReopen: committed content outlives the store that wrote it, and
// a writer the closed store left unfinished leaves nothing behind.
func artifactReopen(t *testing.T, h ArtifactHarness) error {
	s, location, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	ref, err := publishArtifact(s, "text/plain", "durable")
	if err != nil {
		return err
	}
	w, err := s.Create(artifactCtx, "text/plain")
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err = io.WriteString(w, "never committed"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = s.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if _, err = w.Commit(artifactCtx); err == nil {
		return fmt.Errorf("a writer committed after its store closed")
	}
	s, err = h.Backend.Open(artifactCtx, location)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got, err := readArtifact(s, ref); err != nil || got != "durable" {
		return fmt.Errorf("after reopen read %q, %v", got, err)
	}
	listed, err := listedIDs(s)
	if err != nil {
		return err
	}
	if len(listed) != 1 || listed[ref.ID].Ref != ref {
		return fmt.Errorf("after reopen listed %v, want only %s", listed, ref.ID)
	}
	return nil
}

// artifactDelete: Delete removes content, is nil for content already gone
// or never there, and refuses a ref that doesn't match with ErrInvalid,
// removing nothing.
func artifactDelete(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	ref, err := publishArtifact(s, "text/plain", "deleted twice")
	if err != nil {
		return err
	}
	wrong := ref
	wrong.SHA256 = digestOf("not this")
	if err = s.Delete(artifactCtx, wrong); !errors.Is(err, storage.ErrInvalid) {
		return fmt.Errorf("delete with another digest: got %v, want ErrInvalid", err)
	}
	if got, err := readArtifact(s, ref); err != nil || got != "deleted twice" {
		return fmt.Errorf("a refused delete removed content: read %q, %v", got, err)
	}
	for i := 0; i < 2; i++ {
		if err = s.Delete(artifactCtx, ref); err != nil {
			return fmt.Errorf("delete %d: %w", i+1, err)
		}
	}
	if _, err = s.Open(artifactCtx, ref); !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("open after delete: got %v, want ErrNotFound", err)
	}
	listed, err := listedIDs(s)
	if err != nil {
		return err
	}
	if len(listed) != 0 {
		return fmt.Errorf("deleted content listed: %v", listed)
	}
	return nil
}

// artifactList: List pages every committed artifact once, in ID byte
// order, with its whole ref and a publication time; a page never exceeds
// its limit; a limit outside 1 to 1000 is ErrInvalid.
func artifactList(t *testing.T, h ArtifactHarness) error {
	s, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	want := map[string]storage.ArtifactRef{}
	for i := 0; i < 7; i++ {
		ref, err := publishArtifact(s, "text/plain", strings.Repeat("x", i))
		if err != nil {
			return err
		}
		want[ref.ID] = ref
	}
	for _, limit := range []int{0, 1001} {
		if _, err = s.List(artifactCtx, "", limit); !errors.Is(err, storage.ErrInvalid) {
			return fmt.Errorf("list with limit %d: got %v, want ErrInvalid", limit, err)
		}
	}
	for _, limit := range []int{1, 3, 1000} {
		all, err := listAll(s, limit)
		if err != nil {
			return err
		}
		if len(all) != len(want) {
			return fmt.Errorf("with limit %d listed %d artifacts, want %d", limit, len(all), len(want))
		}
		for i, a := range all {
			if i > 0 && a.Ref.ID <= all[i-1].Ref.ID {
				return fmt.Errorf("with limit %d listed %q after %q: not in ID order", limit, a.Ref.ID, all[i-1].Ref.ID)
			}
			if want[a.Ref.ID] != a.Ref {
				return fmt.Errorf("listed %+v, committed %+v", a.Ref, want[a.Ref.ID])
			}
			if a.PublishedAt.IsZero() {
				return fmt.Errorf("listed %s without a publication time", a.Ref.ID)
			}
		}
	}
	return nil
}

// artifactImport: Import publishes exactly the ref it is given into a
// store without that ID, and nothing at all when the source runs short or
// long, its digest differs, or the ID is already there. A ref without a
// digest imports, and the store then holds its digest.
func artifactImport(t *testing.T, h ArtifactHarness) error {
	source, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	ref, err := publishArtifact(source, "text/plain", "restored")
	if err != nil {
		return err
	}
	legacy, err := publishArtifact(source, "text/plain", "from before digests")
	if err != nil {
		return err
	}
	legacy.SHA256 = ""
	target, _, err := openArtifacts(t, h)
	if err != nil {
		return err
	}
	bad := map[string]string{"a short source": "restore", "a long source": "restored!", "another digest": "restorex"}
	for name, content := range bad {
		if err = target.Import(artifactCtx, ref, strings.NewReader(content)); !errors.Is(err, storage.ErrCorrupt) {
			return fmt.Errorf("import of %s: got %v, want ErrCorrupt", name, err)
		}
	}
	listed, err := listedIDs(target)
	if err != nil {
		return err
	}
	if len(listed) != 0 {
		return fmt.Errorf("failed imports left content listed: %v", listed)
	}
	if _, err = target.Open(artifactCtx, ref); !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("failed imports left content readable: %v", err)
	}
	if err = target.Import(artifactCtx, ref, strings.NewReader("restored")); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if err = target.Import(artifactCtx, ref, strings.NewReader("restored")); !errors.Is(err, storage.ErrConflict) {
		return fmt.Errorf("import of an ID the store holds: got %v, want ErrConflict", err)
	}
	if got, err := readArtifact(target, ref); err != nil || got != "restored" {
		return fmt.Errorf("imported content read %q, %v", got, err)
	}
	if err = target.Import(artifactCtx, legacy, bytes.NewReader([]byte("from before digests"))); err != nil {
		return fmt.Errorf("import of a ref without a digest: %w", err)
	}
	listed, err = listedIDs(target)
	if err != nil {
		return err
	}
	full := legacy
	full.SHA256 = digestOf("from before digests")
	if listed[ref.ID].Ref != ref || listed[legacy.ID].Ref != full {
		return fmt.Errorf("after import listed %v, want %+v and %+v", listed, ref, full)
	}
	return nil
}
