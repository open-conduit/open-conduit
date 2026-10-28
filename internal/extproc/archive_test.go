package extproc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tarGz packs entries (name → contents; a trailing "/" is a directory, a
// "->" in contents makes a symlink) into a .tar.gz.
func tarGz(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		name, body := e[0], e[1]
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		switch {
		case strings.HasSuffix(name, "/"):
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		case strings.HasPrefix(body, "->"):
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, body[2:], 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
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

// packageArchive packs a package directory written by writePackage.
func packageArchive(t *testing.T, dir string) []byte {
	t.Helper()
	base := filepath.Dir(dir)
	var entries [][2]string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, [2]string{rel + "/", ""})
			return nil
		}
		b, err := os.ReadFile(p)
		entries = append(entries, [2]string{rel, string(b)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tarGz(t, append([][2]string{{"._" + filepath.Base(dir), "metadata"}}, entries...))
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestInstallSourceArchive(t *testing.T) {
	src, root := t.TempDir(), filepath.Join(t.TempDir(), Dir)
	v1 := packageArchive(t, writePackage(t, filepath.Join(src, "v1"), manifest("acme", "1.0.0", 1)))
	v2 := packageArchive(t, writePackage(t, filepath.Join(src, "v2"), manifest("acme", "2.0.0", 1)))
	file := filepath.Join(src, "acme-1.0.0.tar.gz")
	if err := os.WriteFile(file, v1, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := InstallSource(ctx, root, Source{Location: file, SHA256: digest(v2)}, InstallOptions{}); !errors.Is(err, ErrArchive) || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("a wrong checksum: %v", err)
	}
	res, err := InstallSource(ctx, root, Source{Location: file, SHA256: "sha256:" + digest(v1)}, InstallOptions{})
	if err != nil || res.Name != "acme" || res.Version != "1.0.0" {
		t.Fatalf("install from a path: %+v %v", res, err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(v2) }))
	defer srv.Close()
	res, err = InstallSource(ctx, root, Source{Location: srv.URL + "/acme.tar.gz", Client: srv.Client()}, InstallOptions{Update: true})
	if err != nil || res.Version != "2.0.0" || res.Previous != "1.0.0" {
		t.Fatalf("update over https without a checksum: %+v %v", res, err)
	}
	if found, errs := Discover(root); len(found) != 1 || len(errs) != 0 {
		t.Fatalf("discover sees %d (%v)", len(found), errs)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, stagingDir)); len(entries) != 0 {
		t.Fatalf("staging left %d entries", len(entries))
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(v1) }))
	defer plain.Close()
	if _, err := InstallSource(ctx, root, Source{Location: plain.URL + "/acme.tar.gz"}, InstallOptions{Update: true}); !errors.Is(err, ErrArchive) || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("plain http without a checksum: %v", err)
	}
	if res, err := InstallSource(ctx, root, Source{Location: plain.URL + "/acme.tar.gz", SHA256: digest(v1)}, InstallOptions{Update: true}); err != nil || res.Version != "1.0.0" {
		t.Fatalf("plain http with a checksum: %+v %v", res, err)
	}

	dir := writePackage(t, filepath.Join(src, "dir"), manifest("other", "1.0.0", 0))
	if _, err := InstallSource(ctx, root, Source{Location: dir, SHA256: digest(v1)}, InstallOptions{}); !errors.Is(err, ErrArchive) {
		t.Fatalf("a checksum for a directory: %v", err)
	}
	if res, err := InstallSource(ctx, root, Source{Location: dir}, InstallOptions{}); err != nil || res.Name != "other" {
		t.Fatalf("install from a directory: %+v %v", res, err)
	}
	if _, err := InstallSource(ctx, root, Source{Location: file, SHA256: "abc"}, InstallOptions{}); !errors.Is(err, ErrArchive) {
		t.Fatalf("a malformed checksum: %v", err)
	}
}

func TestInstallSourceRefusesUnsafeArchives(t *testing.T) {
	src := t.TempDir()
	pkg := writePackage(t, filepath.Join(src, "p"), manifest("acme", "1.0.0", 0))
	m, _ := os.ReadFile(filepath.Join(pkg, "open-conduit-extension.json"))
	cases := map[string][][2]string{
		"escape":   {{"acme/", ""}, {"acme/../evil", "x"}},
		"absolute": {{"/etc/evil", "x"}},
		"symlink":  {{"acme/", ""}, {"acme/open-conduit-extension.json", string(m)}, {"acme/acme-extension", "->/bin/sh"}},
		"two dirs": {{"acme/", ""}, {"acme/open-conduit-extension.json", string(m)}, {"other/x", "x"}},
		"loose":    {{"open-conduit-extension.json", string(m)}},
		"empty":    {},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), Dir)
			file := filepath.Join(t.TempDir(), "a.tar.gz")
			if err := os.WriteFile(file, tarGz(t, entries), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := InstallSource(context.Background(), root, Source{Location: file}, InstallOptions{})
			if !errors.Is(err, ErrArchive) {
				t.Fatalf("installed or wrong error: %v", err)
			}
			if d := Diagnose("acme", err, Paths{}); d.Code != "invalid_archive" {
				t.Fatalf("diagnosis %+v", d)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(src), "evil")); err == nil {
				t.Fatal("wrote outside the staging directory")
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Fatalf("a refused archive left %d entries", len(entries))
			}
		})
	}
}

// foreignELF is a minimal Linux ELF header for an architecture other than
// this host's.
func foreignELF() []byte {
	machine := elf.EM_X86_64
	if runtime.GOARCH == "amd64" {
		machine = elf.EM_AARCH64
	}
	h := make([]byte, 64)
	copy(h, elf.ELFMAG)
	h[elf.EI_CLASS], h[elf.EI_DATA], h[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(h[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(h[18:], uint16(machine))
	binary.LittleEndian.PutUint32(h[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(h[52:], 64)
	return h
}

func TestInstallRefusesAnotherPlatform(t *testing.T) {
	src, root := t.TempDir(), filepath.Join(t.TempDir(), Dir)
	pkg := writePackage(t, src, manifest("acme", "1.0.0", 0))
	if err := os.WriteFile(filepath.Join(pkg, "acme-extension"), foreignELF(), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Install(root, pkg, InstallOptions{})
	if !errors.Is(err, ErrPlatform) || !strings.Contains(err.Error(), runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("a foreign binary: %v", err)
	}
	if d := Diagnose("acme", err, Paths{}); d.Code != "wrong_platform" {
		t.Fatalf("diagnosis %+v", d)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if goos, arches := binaryPlatform(self); goos != runtime.GOOS || len(arches) == 0 || arches[0] != runtime.GOARCH {
		t.Fatalf("this test binary reads as %s/%v", goos, arches)
	}
}
