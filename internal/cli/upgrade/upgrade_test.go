package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// release serves GitHub's latest-release API and one release's assets.
type release struct {
	tag     string
	archive []byte
	sum     string
	checks  atomic.Int32
}

func (r *release) handler(t *testing.T) http.Handler {
	name := "open-conduit_" + strings.TrimPrefix(r.tag, "v") + "_linux_arm64.tar.gz"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		r.checks.Add(1)
		_, _ = w.Write([]byte(`{"tag_name": "` + r.tag + `", "html_url": "https://github.com/` + Repo + `/releases/tag/` + r.tag + `"}`))
	})
	mux.HandleFunc("GET /"+Repo+"/releases/download/"+r.tag+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		sum := r.sum
		if sum == "" {
			s := sha256.Sum256(r.archive)
			sum = hex.EncodeToString(s[:])
		}
		_, _ = w.Write([]byte("0000  open-conduit_" + strings.TrimPrefix(r.tag, "v") + "_darwin_arm64.tar.gz\n" + sum + "  " + name + "\n"))
	})
	mux.HandleFunc("GET /"+Repo+"/releases/download/"+r.tag+"/"+name, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(r.archive)
	})
	return mux
}

func updater(t *testing.T, r *release) (*Updater, string) {
	t.Helper()
	srv := httptest.NewServer(r.handler(t))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	for _, name := range []string{"open-conduit", "os"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old "+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Updater{Executable: filepath.Join(dir, "open-conduit"), API: srv.URL, Download: srv.URL, GOOS: "linux", GOARCH: "arm64", HTTP: srv.Client()}, dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"1.8.0", "1.9.0", true},
		{"1.9.0", "1.9.0", false},
		{"1.10.0", "1.9.0", false},
		{"1.9.9", "2.0.0", true},
		{"v1.8.0", "1.8.1", true},
		{"1.9.0-rc.1", "1.9.0", true},
		{"1.9.1-snapshot.abc", "1.9.0", false},
		{"dev", "1.9.0", false},
		{"1.9.0", "", false},
	} {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.current, c.latest, got)
		}
	}
}

func TestLatestAndInstall(t *testing.T) {
	r := &release{tag: "v1.9.0"}
	r.archive = archive(t, map[string]string{"open-conduit": "new open-conduit", "os": "new os", "README.md": "readme", "docs/x.md": "doc"})
	u, dir := updater(t, r)
	rel, err := u.Latest(context.Background())
	if err != nil || rel.Version != "1.9.0" || !strings.HasSuffix(rel.URL, "/releases/tag/v1.9.0") {
		t.Fatalf("latest: %+v %v", rel, err)
	}
	if err := u.Install(context.Background(), rel.Version); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "open-conduit")) != "new open-conduit" || read(t, filepath.Join(dir, "os")) != "new os" {
		t.Fatal("binaries not replaced")
	}
	info, err := os.Stat(filepath.Join(dir, "os"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("os mode: %v %v", info.Mode(), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("left behind: %v", entries)
	}
}

// The bundled extensions/ directory is replaced as a whole with the
// release's, and removed when the release bundles none.
func TestInstallReplacesTheBundledExtensions(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"bundled": {"open-conduit": "new open-conduit", "os": "new os", "extensions/setup/setup-extension": "new setup", "extensions/setup/playbooks/setup.md": "guide"},
		"none":    {"open-conduit": "new open-conduit", "os": "new os"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &release{tag: "v1.9.0", archive: archive(t, files)}
			u, dir := updater(t, r)
			for _, f := range []string{"extensions/setup/setup-extension", "extensions/setup/old.md", "extensions/stale/x"} {
				path := filepath.Join(dir, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := u.Install(context.Background(), "1.9.0"); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(dir)
			if name == "none" {
				if _, err := os.Stat(filepath.Join(dir, "extensions")); !os.IsNotExist(err) || len(entries) != 2 {
					t.Fatalf("the old bundled extensions were kept: %v %v", err, entries)
				}
				return
			}
			if read(t, filepath.Join(dir, "extensions", "setup", "setup-extension")) != "new setup" || read(t, filepath.Join(dir, "extensions", "setup", "playbooks", "setup.md")) != "guide" {
				t.Fatal("bundled extensions not replaced")
			}
			if info, err := os.Stat(filepath.Join(dir, "extensions", "setup", "setup-extension")); err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("setup-extension mode: %v", err)
			}
			for _, gone := range []string{"extensions/setup/old.md", "extensions/stale"} {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(gone))); !os.IsNotExist(err) {
					t.Fatalf("%s was kept", gone)
				}
			}
			if len(entries) != 3 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}
}

func TestInstallRefusesABadArchive(t *testing.T) {
	for name, r := range map[string]*release{
		"checksum mismatch":  {tag: "v1.9.0", sum: strings.Repeat("ab", 32)},
		"missing os":         {tag: "v1.9.0"},
		"escaping extension": {tag: "v1.9.0"},
	} {
		t.Run(name, func(t *testing.T) {
			r.archive = archive(t, map[string]string{"open-conduit": "new open-conduit"})
			if name == "checksum mismatch" {
				r.archive = archive(t, map[string]string{"open-conduit": "new", "os": "new"})
			}
			if name == "escaping extension" {
				r.archive = archive(t, map[string]string{"open-conduit": "new", "os": "new", "extensions/../evil": "x"})
			}
			u, dir := updater(t, r)
			if err := u.Install(context.Background(), "1.9.0"); err == nil {
				t.Fatal("installed")
			}
			if read(t, filepath.Join(dir, "open-conduit")) != "old open-conduit" || read(t, filepath.Join(dir, "os")) != "old os" {
				t.Fatal("old binaries touched")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 2 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}
}

func TestInstallLeavesHomebrewToBrew(t *testing.T) {
	u := &Updater{Executable: "/opt/homebrew/Caskroom/open-conduit/1.8.0/open-conduit", API: "http://127.0.0.1:1", Download: "http://127.0.0.1:1"}
	if !u.Homebrew() {
		t.Fatal("not seen as Homebrew")
	}
	if err := u.Install(context.Background(), "1.9.0"); !errors.Is(err, ErrHomebrew) {
		t.Fatalf("install: %v", err)
	}
}

func TestCachedChecksInTheBackgroundOncePerTTL(t *testing.T) {
	r := &release{tag: "v1.9.0"}
	u, _ := updater(t, r)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var clock atomic.Pointer[time.Time]
	clock.Store(&now)
	u.Now = func() time.Time { return *clock.Load() }
	if _, ok := u.Cached(); ok {
		t.Fatal("a release before any check")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if rel, ok := u.Cached(); ok {
			if rel.Version != "1.9.0" {
				t.Fatalf("cached: %+v", rel)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background check never finished")
		}
		time.Sleep(time.Millisecond)
	}
	if n := r.checks.Load(); n != 1 {
		t.Fatalf("checked %d times within the TTL", n)
	}
	later := now.Add(7 * time.Hour)
	clock.Store(&later)
	// The first check's goroutine may still be finishing: ask until the
	// second one starts.
	for u.Cached(); r.checks.Load() < 2; u.Cached() {
		if time.Now().After(deadline) {
			t.Fatal("no check after the TTL")
		}
		time.Sleep(time.Millisecond)
	}
}
