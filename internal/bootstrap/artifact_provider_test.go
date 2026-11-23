package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/internal/artifacts"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol/artifactstore"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	providerEnv    = "OPEN_CONDUIT_TEST_ARTIFACT_PROVIDER"
	crashMarkerEnv = "OPEN_CONDUIT_TEST_ARTIFACT_CRASH_MARKER"
)

// TestMain lets this test binary act as a provider executable, so the suite
// crosses a real process boundary.
func TestMain(m *testing.M) {
	switch os.Getenv(providerEnv) {
	case "":
		os.Exit(m.Run())
	case "serve":
		if err := artifactstore.Serve(context.Background(), os.Stdin, os.Stdout, artifacts.Backend); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "crash":
		if err := artifactstore.Serve(context.Background(), os.Stdin, os.Stdout, crashBackend{os.Getenv(crashMarkerEnv)}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "stutter", "stuck":
		if err := artifactstore.Serve(context.Background(), os.Stdin, os.Stdout, emptyReadsBackend{stuck: os.Getenv(providerEnv) == "stuck"}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "lying":
		if err := artifactstore.Serve(context.Background(), os.Stdin, os.Stdout, lyingBackend{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func provider(t *testing.T, mode string) storage.ArtifactBackend {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(providerEnv, mode)
	return artifactstore.Command(exe)
}

// What a store says about its reach decides doctor's account on shared state,
// including across a provider's process boundary, where one that says nothing
// is reported as unstated and never as shared.
func TestArtifactReachOnSharedState(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T, b storage.ArtifactBackend) storage.ArtifactStore {
		t.Helper()
		store, err := b.Open(ctx, filepath.Join(t.TempDir(), "artifacts"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	}
	for _, tc := range []struct {
		name   string
		store  func(*testing.T) storage.ArtifactStore
		status string
		code   string
	}{
		{"file store", func(t *testing.T) storage.ArtifactStore { return open(t, artifacts.Backend) }, runtime.CheckWarn, artifactsOnThisHost},
		{"file store through a provider", func(t *testing.T) storage.ArtifactStore { return open(t, provider(t, "serve")) }, runtime.CheckWarn, artifactsOnThisHost},
		{"provider that says nothing", func(t *testing.T) storage.ArtifactStore { return open(t, provider(t, "stutter")) }, runtime.CheckInfo, "unstated"},
		{"store that can't say", func(t *testing.T) storage.ArtifactStore { return reachStore{stated: false} }, runtime.CheckInfo, "unstated"},
		{"shared store", func(t *testing.T) storage.ArtifactStore { return reachStore{shared: true, stated: true} }, runtime.CheckOK, "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := artifactReach(tc.store(t)); c.Status != tc.status || c.Code != tc.code {
				t.Fatalf("doctor: %+v", c)
			}
		})
	}
}

type reachStore struct {
	storage.ArtifactStore
	shared, stated bool
}

func (r reachStore) SharesLocation() (bool, bool) { return r.shared, r.stated }

// A store in another process meets the same contract as the file store.
func TestProviderConformance(t *testing.T) {
	conformance.RunArtifacts(t, conformance.ArtifactHarness{
		Backend:  provider(t, "serve"),
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
	})
}

func TestProviderCommandMustBeAbsolute(t *testing.T) {
	if _, err := artifactstore.Command("relative/provider").Open(context.Background(), t.TempDir()); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

func TestProviderThatCannotStartFailsOpen(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := artifactstore.Command(missing).Open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("opened a store with no provider")
	}
}

// The host checks content itself: a provider returning other bytes, or a
// ref that doesn't describe what was written, is refused.
func TestHostVerifiesWhatAProviderReturns(t *testing.T) {
	ctx := context.Background()
	location := filepath.Join(t.TempDir(), "store")
	honest, err := provider(t, "serve").Open(ctx, location)
	if err != nil {
		t.Fatal(err)
	}
	w, err := honest.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, "the real content"); err != nil {
		t.Fatal(err)
	}
	ref, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = honest.Close(); err != nil {
		t.Fatal(err)
	}

	lying, err := provider(t, "lying").Open(ctx, location)
	if err != nil {
		t.Fatal(err)
	}
	defer lying.Close()
	r, err := lying.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = io.ReadAll(r); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("read of altered content: got %v, want ErrCorrupt", err)
	}
	w, err = lying.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, "written"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(ctx); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("commit with a false ref: got %v, want ErrCorrupt", err)
	}
	// What the refused commit stored is taken back.
	listed, err := lying.List(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Ref.ID != ref.ID {
		t.Fatalf("after the refused commit the store lists %+v, want only %s", listed, ref.ID)
	}
}

// A provider that dies is started again; the store keeps working.
func TestProviderRestartsAfterItDies(t *testing.T) {
	ctx := context.Background()
	t.Setenv(crashMarkerEnv, filepath.Join(t.TempDir(), "crashed"))
	s, err := provider(t, "crash").Open(ctx, filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.List(ctx, "", 10); err == nil {
		t.Fatal("the call the provider died during succeeded")
	}
	if _, err = s.List(ctx, "", 10); err != nil {
		t.Fatalf("store did not recover: %v", err)
	}
}

// crashBackend's process exits during its first List, once per marker.
type crashBackend struct{ marker string }

func (b crashBackend) Open(ctx context.Context, location string) (storage.ArtifactStore, error) {
	s, err := artifacts.Backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return crashStore{s, b.marker}, nil
}

type crashStore struct {
	storage.ArtifactStore
	marker string
}

func (c crashStore) List(ctx context.Context, after string, limit int) ([]storage.StoredArtifact, error) {
	if _, err := os.Stat(c.marker); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(c.marker, nil, 0o600)
		os.Exit(3)
	}
	return c.ArtifactStore.List(ctx, after, limit)
}

// storage.json can name a provider, and the state directory then opens
// through it; a choice it can't honour is refused, never a fallback.
func TestStorageChoiceNamesAProvider(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(providerEnv, "serve")
	dir, location := t.TempDir(), filepath.Join(t.TempDir(), "store")
	writeChoice(t, dir, `{"backend":"sqlite","artifacts":{"backend":"test-provider","command":"`+exe+`","location":"`+location+`"}}`)
	a, s, err := openArtifacts(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if a.name != "test-provider" || a.location != location {
		t.Fatalf("resolved %+v", a)
	}
	w, err := s.Create(context.Background(), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, "through the provider"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"relative_command": `{"backend":"sqlite","artifacts":{"backend":"p","command":"bin/p","location":"x"}}`,
		"no_location":      `{"backend":"sqlite","artifacts":{"backend":"p","command":"` + exe + `"}}`,
		"built_in_name":    `{"backend":"sqlite","artifacts":{"backend":"files","command":"` + exe + `","location":"x"}}`,
	} {
		bad := t.TempDir()
		writeChoice(t, bad, body)
		if _, err := artifactsOf(bad); !errors.Is(err, storage.ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

// io.Reader allows a read of nothing with no error. The SDK reads past it,
// and reports a reader that never gets anywhere rather than spinning.
func TestProviderHandlesEmptyReads(t *testing.T) {
	ctx := context.Background()
	location := filepath.Join(t.TempDir(), "store")
	honest, err := provider(t, "serve").Open(ctx, location)
	if err != nil {
		t.Fatal(err)
	}
	w, err := honest.Create(ctx, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, "content after empty reads"); err != nil {
		t.Fatal(err)
	}
	ref, err := w.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = honest.Close(); err != nil {
		t.Fatal(err)
	}
	read := func(mode string) (string, error) {
		s, err := provider(t, mode).Open(ctx, location)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		r, err := s.Open(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		return string(b), err
	}
	if got, err := read("stutter"); err != nil || got != "content after empty reads" {
		t.Fatalf("read past empty reads: %q, %v", got, err)
	}
	if got, err := read("stuck"); err == nil {
		t.Fatalf("a stuck reader read cleanly: %q", got)
	}
}

// emptyReadsBackend's readers return (0, nil) before each read, or for ever.
type emptyReadsBackend struct{ stuck bool }

func (b emptyReadsBackend) Open(ctx context.Context, location string) (storage.ArtifactStore, error) {
	s, err := artifacts.Backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return emptyReadsStore{s, b.stuck}, nil
}

type emptyReadsStore struct {
	storage.ArtifactStore
	stuck bool
}

func (s emptyReadsStore) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	r, err := s.ArtifactStore.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &emptyReader{ReadCloser: r, stuck: s.stuck}, nil
}

type emptyReader struct {
	io.ReadCloser
	stuck, primed bool
}

func (r *emptyReader) Read(b []byte) (int, error) {
	if r.stuck || !r.primed {
		r.primed = true
		return 0, nil
	}
	r.primed = false
	return r.ReadCloser.Read(b)
}

// lyingBackend alters what it reads and misreports what it commits.
type lyingBackend struct{}

func (lyingBackend) Open(ctx context.Context, location string) (storage.ArtifactStore, error) {
	s, err := artifacts.Backend.Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return lyingStore{s}, nil
}

type lyingStore struct{ storage.ArtifactStore }

func (l lyingStore) Open(_ context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Repeat("x", int(ref.Size)))), nil
}

func (l lyingStore) Create(ctx context.Context, mediaType string) (storage.ArtifactWriter, error) {
	w, err := l.ArtifactStore.Create(ctx, mediaType)
	if err != nil {
		return nil, err
	}
	return lyingWriter{w}, nil
}

type lyingWriter struct{ storage.ArtifactWriter }

func (l lyingWriter) Commit(ctx context.Context) (storage.ArtifactRef, error) {
	ref, err := l.ArtifactWriter.Commit(ctx)
	ref.Size++
	return ref, err
}
