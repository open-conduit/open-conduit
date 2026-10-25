package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestPaths(t *testing.T) {
	home := envOf(map[string]string{"HOME": "/home/u"})
	xdg := envOf(map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/x/c", "XDG_STATE_HOME": "/x/s"})
	relXDG := envOf(map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "rel"})
	set := envOf(map[string]string{"HOME": "/home/u", "OPEN_CONDUIT_CONFIG": "env/config.json", "OPEN_CONDUIT_DATA_DIR": "/data"})
	cases := []struct {
		fn   func() (string, error)
		want string
	}{
		{func() (string, error) { return DefaultConfigPath(home) }, "/home/u/.config/open-conduit/config.json"},
		{func() (string, error) { return DefaultStateDir(home) }, "/home/u/.local/state/open-conduit"},
		{func() (string, error) { return DefaultConfigPath(xdg) }, "/x/c/open-conduit/config.json"},
		{func() (string, error) { return DefaultStateDir(xdg) }, "/x/s/open-conduit"},
		{func() (string, error) { return DefaultConfigPath(relXDG) }, "/home/u/.config/open-conduit/config.json"},
		{func() (string, error) { return ResolveConfigPath("../flag.json", set, "/work/dir") }, "/work/flag.json"},
		{func() (string, error) { return ResolveConfigPath("", set, "/work") }, "/work/env/config.json"},
		{func() (string, error) { return ResolveConfigPath("", home, "/work") }, "/home/u/.config/open-conduit/config.json"},
		{func() (string, error) { return ResolveStateDir("/flag//state/", set, "/work") }, "/flag/state"},
		{func() (string, error) { return ResolveStateDir("", set, "/work") }, "/data"},
		{func() (string, error) { return ResolveStateDir("", xdg, "/work") }, "/x/s/open-conduit"},
	}
	for i, c := range cases {
		if got, err := c.fn(); err != nil || got != c.want {
			t.Fatalf("case %d: %q %v", i, got, err)
		}
	}
	if _, err := DefaultConfigPath(envOf(nil)); err == nil {
		t.Fatal("missing HOME accepted")
	}
	if _, err := ResolveConfigPath("rel", home, ""); err == nil {
		t.Fatal("relative path without startup directory accepted")
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadFile(path); err != nil || string(data) != "{}" {
		t.Fatal(string(data), err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(link); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		os.Chmod(path, mode)
		if _, err := ReadFile(path); !errors.Is(err, ErrUnsafeFile) {
			t.Fatalf("%o: %v", mode, err)
		}
	}
	if _, err := ReadFile(dir); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, make([]byte, MaxSize+1), 0o600)
	if _, err := ReadFile(big); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
	if _, err := ReadFile(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestSaveAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open-conduit")
	path := filepath.Join(dir, "config.json")
	v1, v2 := example(t, "shell-job"), example(t, "inheritance")
	if err := SaveAtomic(path, v1); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal(info.Mode(), err)
	}
	if _, err := os.Stat(path + ".last-good"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup without previous content")
	}
	if err := SaveAtomic(path, v2); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string][]byte{path: v2, path + ".last-good": v1} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal(p, err)
		}
		if data, err := ReadFile(p); err != nil || string(data) != string(want) {
			t.Fatal(p, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
	// An invalid disk edit fails validation and leaves last-good intact.
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(data, registry()); err == nil {
		t.Fatal("invalid edit validated")
	}
	good, err := ReadFile(path + ".last-good")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(good, registry()); err != nil || string(good) != string(v1) {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, make([]byte, MaxSize+1)); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
	// An unsafe existing file is never trusted as a backup source.
	os.Chmod(path, 0o644)
	if err := SaveAtomic(path, v2); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
}
