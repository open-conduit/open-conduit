package artifacts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/storage"
)

var ctx = context.Background()

func store(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func publish(t *testing.T, s *Store, data string) storage.ArtifactRef {
	t.Helper()
	w, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.WriteString(w, data); e != nil {
		t.Fatal(e)
	}
	ref, e := w.Commit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return ref
}
func content(t *testing.T, s *Store, ref storage.ArtifactRef) string {
	t.Helper()
	r, e := s.Open(ctx, ref)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	b, e := io.ReadAll(r)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func TestPublicationAbortAndReopen(t *testing.T) {
	s := store(t)
	w, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.WriteString(w, "streaming bytes"); e != nil {
		t.Fatal(e)
	}
	draft := w.(*writer).ref
	if _, e = s.Open(ctx, draft); !errors.Is(e, storage.ErrNotFound) {
		t.Fatalf("partial stream visible: %v", e)
	}
	ref, e := w.Commit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if ref.Size != 15 || content(t, s, ref) != "streaming bytes" {
		t.Fatal(ref)
	}
	if _, e = w.Write([]byte("late")); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	if _, e = w.Commit(ctx); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	aborted, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	id := aborted.(*writer).ref.ID
	if e = aborted.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Open(ctx, storage.ArtifactRef{ID: id}); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	again, e := Open(s.root)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	if content(t, again, ref) != "streaming bytes" {
		t.Fatal("reopen changed bytes")
	}
	if e = again.Delete(ctx, ref); e != nil {
		t.Fatal(e)
	}
	if e = again.Delete(ctx, ref); e != nil {
		t.Fatal(e)
	}
	if _, e = again.Open(ctx, ref); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal(e)
	}
}
func TestFaultsNeverYieldReference(t *testing.T) {
	for _, stage := range []string{"data_sync", "publish", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			s := store(t)
			w, e := s.Create(ctx, "text/plain")
			if e != nil {
				t.Fatal(e)
			}
			io.WriteString(w, "payload")
			id := w.(*writer).ref.ID
			s.fault = func(at string) error {
				if at == stage {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			ref, e := w.Commit(ctx)
			if !errors.Is(e, io.ErrUnexpectedEOF) || ref.ID != "" {
				t.Fatalf("ref=%v err=%v", ref, e)
			}
			if _, e = s.Open(ctx, storage.ArtifactRef{ID: id}); !errors.Is(e, storage.ErrNotFound) {
				t.Fatalf("failed publication visible: %v", e)
			}
			entries, e := os.ReadDir(filepath.Join(s.root, "pending"))
			if e != nil || len(entries) != 0 {
				t.Fatalf("leaked pending: %v %v", entries, e)
			}
			prefixes, e := s.prefixes()
			for _, prefix := range prefixes {
				entries, e := os.ReadDir(filepath.Join(s.objects(), prefix))
				if e != nil || len(entries) != 0 {
					t.Fatalf("leaked objects/%s: %v %v", prefix, entries, e)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestCancellationAndCorruption(t *testing.T) {
	s := store(t)
	w, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(w, "payload")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	ref, e := w.Commit(cancelled)
	if !errors.Is(e, context.Canceled) || ref.ID != "" {
		t.Fatal(ref, e)
	}
	ref = publish(t, s, "payload")
	forged := ref
	forged.Size++
	if _, e = s.Open(ctx, forged); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.objectDir(ref.ID), "data"), []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	r, e := s.Open(ctx, ref)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if _, e = io.ReadAll(r); !errors.Is(e, ErrCorrupt) {
		t.Fatalf("corrupt same-size bytes accepted: %v", e)
	}
	if _, e = s.Open(ctx, storage.ArtifactRef{ID: "../../private"}); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
}

// Recovery, as Open runs it, removes what a writer left in pending/ but
// keeps this store's own active writers and every committed object.
func TestRecoveryKeepsActiveWritersAndObjects(t *testing.T) {
	s := store(t)
	live := publish(t, s, "retained")
	active, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(active, "still writing")
	left := filepath.Join(s.root, "pending", strings.Repeat("b", 64))
	if e = os.Mkdir(left, 0o700); e != nil {
		t.Fatal(e)
	}
	if e = s.recover(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = active.Write([]byte(" more")); e != nil {
		t.Fatal("recovery removed active writer:", e)
	}
	if _, e = os.Lstat(left); !os.IsNotExist(e) {
		t.Fatalf("recovery kept a stream no writer owns: %v", e)
	}
	if content(t, s, live) != "retained" {
		t.Fatal("recovery removed a committed object")
	}
	active.Close()
}
func TestImportRejectsPartialAndSourceErrors(t *testing.T) {
	s := store(t)
	ref := storage.ArtifactRef{ID: strings.Repeat("a", 64), Size: 3, MediaType: "text/plain"}
	for _, source := range []io.Reader{strings.NewReader("ab"), strings.NewReader("abcd"), io.MultiReader(strings.NewReader("abc"), errorReader{})} {
		if e := s.Import(ctx, ref, source); e == nil {
			t.Fatal("bad import accepted")
		}
		if _, e := s.Open(ctx, ref); !errors.Is(e, storage.ErrNotFound) {
			t.Fatal(e)
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestPrivatePathsAndExclusiveOwnership(t *testing.T) {
	s := store(t)
	if other, e := Open(s.root); e == nil {
		other.Close()
		t.Fatal("second owner accepted")
	}
	dir := filepath.Join(t.TempDir(), "unsafe")
	if e := os.Mkdir(dir, 0755); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(dir); e == nil {
		t.Fatal("unsafe mode accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(s.root, link); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(link); e == nil {
		t.Fatal("symlink root accepted")
	}
	ref := publish(t, s, "hello")
	path := filepath.Join(s.objectDir(ref.ID), "data")
	if e := os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Open(ctx, ref); e == nil {
		t.Fatal("symlink content accepted")
	}
}
func TestConcurrentWriters(t *testing.T) {
	s := store(t)
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, e := s.Create(ctx, "application/octet-stream")
			if e != nil {
				errors <- e
				return
			}
			if _, e = io.Copy(w, bytes.NewReader(bytes.Repeat([]byte("data"), 1024))); e != nil {
				errors <- e
				return
			}
			ref, e := w.Commit(ctx)
			if e != nil {
				errors <- e
				return
			}
			r, e := s.Open(ctx, ref)
			if e != nil {
				errors <- e
				return
			}
			_, e = io.Copy(io.Discard, r)
			r.Close()
			errors <- e
		}()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		if e != nil {
			t.Fatal(e)
		}
	}
}
func TestCrashRecovery(t *testing.T) {
	for _, phase := range []string{"stream", "committed"} {
		t.Run(phase, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "artifacts")
			cmd := exec.Command(os.Args[0], "-test.run=^TestArtifactCrashHelper$")
			input, e := cmd.StdinPipe()
			if e != nil {
				t.Fatal(e)
			}
			defer input.Close()
			cmd.Env = append(os.Environ(), "CONDUIT_ARTIFACT_HELPER="+phase, "CONDUIT_ARTIFACT_ROOT="+dir)
			out, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			scanner := bufio.NewScanner(out)
			if !scanner.Scan() {
				cmd.Wait()
				t.Fatalf("helper no barrier: %s", stderr.String())
			}
			var ref storage.ArtifactRef
			if e = jsonDecode(scanner.Text(), &ref); e != nil {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal(e)
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			cmd.Wait()
			s, e := Open(dir)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close() // Open has removed the killed process's pending stream
			if phase == "committed" {
				if content(t, s, ref) != "durable payload" {
					t.Fatal("lost committed content")
				}
			} else {
				if _, e = s.Open(ctx, ref); !errors.Is(e, storage.ErrNotFound) {
					t.Fatalf("partial crash stream visible %v", e)
				}
				entries, e := os.ReadDir(filepath.Join(dir, "pending"))
				if e != nil || len(entries) != 0 {
					t.Fatal(entries, e)
				}
			}
		})
	}
}
func jsonDecode(s string, v any) error { return json.Unmarshal([]byte(s), v) }
func TestArtifactCrashHelper(t *testing.T) {
	phase := os.Getenv("CONDUIT_ARTIFACT_HELPER")
	if phase == "" {
		return
	}
	s, e := Open(os.Getenv("CONDUIT_ARTIFACT_ROOT"))
	if e != nil {
		t.Fatal(e)
	}
	w, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(w, "durable payload")
	ref := w.(*writer).ref
	if phase == "committed" {
		ref, e = w.Commit(ctx)
		if e != nil {
			t.Fatal(e)
		}
	}
	b, _ := json.Marshal(ref)
	fmt.Println(string(b))
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// TestOpenFansOutTheFlatLayout: objects a store published at objects/<ID>
// (before the fan-out layout) move to objects/<prefix>/<ID> when it is
// opened, and read and list as before, digest or not.
func TestOpenFansOutTheFlatLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	ref := publish(t, s, "published flat")
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	// Put it back where an older binary kept it, with its meta.json as one
	// wrote it: no digest in the ref.
	if e = os.Rename(filepath.Join(dir, "objects", ref.ID[:2], ref.ID), filepath.Join(dir, "objects", ref.ID)); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(dir, "objects", ref.ID[:2])); e != nil {
		t.Fatal(e)
	}
	metaPath := filepath.Join(dir, "objects", ref.ID, "meta.json")
	b, e := os.ReadFile(metaPath)
	if e != nil {
		t.Fatal(e)
	}
	var m metadata
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	m.Ref.SHA256 = ""
	if b, e = json.Marshal(m); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(metaPath, b, 0o600); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = os.Stat(filepath.Join(dir, "objects", ref.ID)); !os.IsNotExist(e) {
		t.Fatalf("the flat object stayed: %v", e)
	}
	if content(t, s, ref) != "published flat" {
		t.Fatal("moved object reads differently")
	}
	page, e := s.List(ctx, "", 10)
	if e != nil || len(page) != 1 || page[0].Ref != ref {
		t.Fatalf("listed %+v, %v; want %+v with its digest", page, e, ref)
	}
}

// TestListPagesAcrossPrefixes: with enough objects that prefixes are
// shared, pages that start and end inside a prefix directory still list
// every object once, in ID order.
func TestListPagesAcrossPrefixes(t *testing.T) {
	s := store(t)
	want := map[string]bool{}
	for i := 0; i < 120; i++ { // many of the 256 prefixes then hold several
		want[publish(t, s, fmt.Sprint(i)).ID] = true
	}
	after, seen := "", 0
	for {
		page, e := s.List(ctx, after, 7)
		if e != nil {
			t.Fatal(e)
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			if a.Ref.ID <= after || !want[a.Ref.ID] {
				t.Fatalf("listed %s after %s", a.Ref.ID, after)
			}
			after = a.Ref.ID
			seen++
		}
	}
	if seen != len(want) {
		t.Fatalf("listed %d of %d", seen, len(want))
	}
}

// TestOpenReadOnlyChangesNothing: a read-only store reads an object in the
// flat layout where it is, leaving it there and leaving pending streams
// alone, and refuses every write.
func TestOpenReadOnlyChangesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	ref := publish(t, s, "bundled")
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	flat := filepath.Join(dir, "objects", ref.ID)
	if e = os.Rename(filepath.Join(dir, "objects", ref.ID[:2], ref.ID), flat); e != nil {
		t.Fatal(e)
	}
	stale := filepath.Join(dir, "pending", strings.Repeat("b", 64))
	if e = os.Mkdir(stale, 0o700); e != nil {
		t.Fatal(e)
	}
	r, e := OpenReadOnly(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if content(t, r, ref) != "bundled" {
		t.Fatal("read-only store read differently")
	}
	for _, path := range []string{flat, stale} {
		if _, e = os.Stat(path); e != nil {
			t.Fatalf("read-only open changed the store: %v", e)
		}
	}
	if _, e = r.Create(ctx, "text/plain"); !errors.Is(e, storage.ErrReadOnly) {
		t.Fatalf("create: %v", e)
	}
	if e = r.Delete(ctx, ref); !errors.Is(e, storage.ErrReadOnly) {
		t.Fatalf("delete: %v", e)
	}
	if e = r.Import(ctx, storage.ArtifactRef{ID: strings.Repeat("c", 64), Size: 1, MediaType: "text/plain"}, strings.NewReader("x")); !errors.Is(e, storage.ErrReadOnly) {
		t.Fatalf("import: %v", e)
	}
	if content(t, r, ref) != "bundled" {
		t.Fatal("a refused delete removed content")
	}
}
