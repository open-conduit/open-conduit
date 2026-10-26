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
	"time"

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
			for _, dir := range []string{"pending", "objects"} {
				entries, e := os.ReadDir(filepath.Join(s.root, dir))
				if e != nil || len(entries) != 0 {
					t.Fatalf("leaked %s: %v %v", dir, entries, e)
				}
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
	if e = os.WriteFile(filepath.Join(s.root, "objects", ref.ID, "data"), []byte("changed"), 0600); e != nil {
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
func TestRetentionRecoveryAndPinnedBackup(t *testing.T) {
	s := store(t)
	live := publish(t, s, "retained")
	orphan := publish(t, s, "orphaned")
	active, e := s.Create(ctx, "text/plain")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(active, "still writing")
	if e = s.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = active.Write([]byte(" more")); e != nil {
		t.Fatal("recovery removed active writer:", e)
	}
	if e = s.Collect(ctx, time.Now().Add(time.Hour), map[string]bool{live.ID: true}); e != nil {
		t.Fatal(e)
	}
	if content(t, s, live) != "retained" {
		t.Fatal("live removed")
	}
	if _, e = s.Open(ctx, orphan); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal(e)
	}
	pin, e := s.Pin()
	if e != nil {
		t.Fatal(e)
	}
	target := store(t)
	r, e := pin.Open(ctx, live)
	if e != nil {
		t.Fatal(e)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); done <- s.Delete(ctx, live) }()
	<-started
	// Import while the deletion writer waits proves a pinned Open does not
	// recursively acquire RWMutex and deadlock behind that writer.
	if e = target.Import(ctx, live, r); e != nil {
		t.Fatal(e)
	}
	r.Close()
	if e = pin.Close(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if content(t, target, live) != "retained" {
		t.Fatal("backup bytes differ")
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
	path := filepath.Join(s.root, "objects", ref.ID, "data")
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
			defer s.Close()
			if e = s.Recover(ctx); e != nil {
				t.Fatal(e)
			}
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
