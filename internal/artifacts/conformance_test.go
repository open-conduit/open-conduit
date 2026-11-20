package artifacts_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/storage"
)

func harness(b storage.ArtifactBackend) conformance.ArtifactHarness {
	return conformance.ArtifactHarness{
		Backend:  b,
		Location: func(t *testing.T, dir string) string { return filepath.Join(dir, "artifacts") },
		Corrupt: func(t *testing.T, location string, ref storage.ArtifactRef) {
			path := filepath.Join(location, "objects", ref.ID[:2], ref.ID, "data")
			b, err := os.ReadFile(path)
			if err != nil || len(b) == 0 {
				t.Fatalf("corrupt %s: %v", path, err)
			}
			b[0] ^= 0xff
			if err = os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
}

func TestFileArtifactConformance(t *testing.T) {
	conformance.RunArtifacts(t, harness(artifacts.Backend))
}

func TestFileBackendIsRegistered(t *testing.T) {
	b, err := storage.LookupArtifactBackend(artifacts.BackendName)
	if err != nil || b != artifacts.Backend {
		t.Fatalf("lookup %q: %v, %v", artifacts.BackendName, b, err)
	}
	if _, err = b.Open(context.Background(), "relative/artifacts"); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("relative location: got %v, want ErrInvalid", err)
	}
}

// Negative controls: each weakens one guarantee of the real file store,
// and the case it breaks must fail for that reason.

type weakBackend struct {
	wrap func(storage.ArtifactStore) storage.ArtifactStore
}

func (b weakBackend) Open(ctx context.Context, location string) (storage.ArtifactStore, error) {
	s, err := artifacts.Backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return b.wrap(s), nil
}

// weakStore overrides whichever of the store's methods a control sets.
type weakStore struct {
	storage.ArtifactStore
	create func(storage.ArtifactWriter) storage.ArtifactWriter
	open   func(context.Context, storage.ArtifactRef) (io.ReadCloser, error)
	delete func(context.Context, storage.ArtifactRef) error
	list   func(context.Context, string, int) ([]storage.StoredArtifact, error)
	imp    func(context.Context, storage.ArtifactRef, io.Reader) error
}

func (s weakStore) Create(ctx context.Context, mediaType string) (storage.ArtifactWriter, error) {
	w, err := s.ArtifactStore.Create(ctx, mediaType)
	if err != nil || s.create == nil {
		return w, err
	}
	return s.create(w), nil
}
func (s weakStore) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	if s.open != nil {
		return s.open(ctx, ref)
	}
	return s.ArtifactStore.Open(ctx, ref)
}
func (s weakStore) Delete(ctx context.Context, ref storage.ArtifactRef) error {
	if s.delete != nil {
		return s.delete(ctx, ref)
	}
	return s.ArtifactStore.Delete(ctx, ref)
}
func (s weakStore) List(ctx context.Context, after string, limit int) ([]storage.StoredArtifact, error) {
	if s.list != nil {
		return s.list(ctx, after, limit)
	}
	return s.ArtifactStore.List(ctx, after, limit)
}
func (s weakStore) Import(ctx context.Context, ref storage.ArtifactRef, source io.Reader) error {
	if s.imp != nil {
		return s.imp(ctx, ref, source)
	}
	return s.ArtifactStore.Import(ctx, ref, source)
}

type undigested struct{ storage.ArtifactWriter }

func (w undigested) Commit(ctx context.Context) (storage.ArtifactRef, error) {
	ref, err := w.ArtifactWriter.Commit(ctx)
	ref.SHA256 = ""
	return ref, err
}

type committingClose struct{ storage.ArtifactWriter }

func (w committingClose) Close() error {
	_, err := w.ArtifactWriter.Commit(context.Background())
	return err
}

// trusting turns the reader's corruption report into a clean end.
type trusting struct{ io.ReadCloser }

func (r trusting) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, storage.ErrCorrupt) {
		err = io.EOF
	}
	return n, err
}

func TestWeakenedArtifactsAreRejected(t *testing.T) {
	controls := []struct {
		name, failing, want string
		wrap                func(storage.ArtifactStore) storage.ArtifactStore
	}{{
		// Commit returns a ref without the digest.
		name: "commit_drops_the_digest", failing: "commit_returns_size_media_type_and_digest", want: "want the content's SHA-256",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, create: func(w storage.ArtifactWriter) storage.ArtifactWriter { return undigested{w} }}
		},
	}, {
		// Close publishes what was written instead of aborting.
		name: "abort_publishes", failing: "abort_publishes_nothing", want: "aborted writer's content was listed",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, create: func(w storage.ArtifactWriter) storage.ArtifactWriter { return committingClose{w} }}
		},
	}, {
		// Open ignores the digest the ref names.
		name: "open_ignores_the_digest", failing: "open_refuses_a_ref_that_does_not_match", want: "another digest",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, open: func(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
				ref.SHA256 = ""
				return s.Open(ctx, ref)
			}}
		},
	}, {
		// The reader doesn't report content that fails its digest.
		name: "read_trusts_the_bytes", failing: "read_verifies_size_and_digest", want: "want ErrCorrupt",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, open: func(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
				r, err := s.Open(ctx, ref)
				if err != nil {
					return nil, err
				}
				return trusting{r}, nil
			}}
		},
	}, {
		// Deleting content already gone is an error.
		name: "delete_reports_missing_content", failing: "delete_is_idempotent_and_checks_the_ref", want: "delete 2",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, delete: func(ctx context.Context, ref storage.ArtifactRef) error {
				if r, err := s.Open(ctx, ref); errors.Is(err, storage.ErrNotFound) {
					return err
				} else if err == nil {
					r.Close()
				}
				return s.Delete(ctx, ref)
			}}
		},
	}, {
		// A page comes back in reverse.
		name: "list_out_of_order", failing: "list_pages_committed_content_once_in_id_order", want: "not in ID order",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, list: func(ctx context.Context, after string, limit int) ([]storage.StoredArtifact, error) {
				page, err := s.List(ctx, after, limit)
				slices.Reverse(page)
				return page, err
			}}
		},
	}, {
		// Import takes whatever bytes come, whatever the ref's digest.
		name: "import_ignores_the_digest", failing: "import_publishes_exactly_the_ref_or_nothing", want: "another digest",
		wrap: func(s storage.ArtifactStore) storage.ArtifactStore {
			return weakStore{ArtifactStore: s, imp: func(ctx context.Context, ref storage.ArtifactRef, source io.Reader) error {
				ref.SHA256 = ""
				return s.Import(ctx, ref, source)
			}}
		},
	}}
	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			h := harness(weakBackend{wrap: control.wrap})
			for _, c := range conformance.ArtifactCases() {
				if c.Name != control.failing {
					continue
				}
				err := c.Check(t, h)
				if err == nil || !strings.Contains(err.Error(), control.want) {
					t.Fatalf("%s: got %v, want an error containing %q", c.Name, err, control.want)
				}
				return
			}
			t.Fatalf("no case %s", control.failing)
		})
	}
}
