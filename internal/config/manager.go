package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ErrUnsafeFile rejects symlinked, non-regular, foreign-owned, group/world
// accessible or oversized private files.
var ErrUnsafeFile = errors.New("unsafe private file")

func homeDir(env func(string) string, xdg, fallback string) (string, error) {
	if dir := env(xdg); filepath.IsAbs(dir) {
		return filepath.Join(filepath.Clean(dir), "open-conduit"), nil
	}
	home := env("HOME")
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("HOME must be an absolute path")
	}
	return filepath.Join(home, fallback, "open-conduit"), nil
}

// DefaultConfigPath is ${XDG_CONFIG_HOME:-$HOME/.config}/open-conduit/config.json.
func DefaultConfigPath(env func(string) string) (string, error) {
	dir, err := homeDir(env, "XDG_CONFIG_HOME", ".config")
	return filepath.Join(dir, "config.json"), err
}

// DefaultStateDir is ${XDG_STATE_HOME:-$HOME/.local/state}/open-conduit.
func DefaultStateDir(env func(string) string) (string, error) {
	return homeDir(env, "XDG_STATE_HOME", filepath.Join(".local", "state"))
}
func explicit(path, cwd string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("relative path requires an absolute startup directory")
	}
	return filepath.Join(cwd, path), nil
}

// ResolveConfigPath applies --config → OPEN_CONDUIT_CONFIG → default.
func ResolveConfigPath(flag string, env func(string) string, cwd string) (string, error) {
	for _, p := range []string{flag, env("OPEN_CONDUIT_CONFIG")} {
		if p != "" {
			return explicit(p, cwd)
		}
	}
	return DefaultConfigPath(env)
}

// ResolveStateDir applies --data-dir → OPEN_CONDUIT_DATA_DIR → default.
func ResolveStateDir(flag string, env func(string) string, cwd string) (string, error) {
	for _, p := range []string{flag, env("OPEN_CONDUIT_DATA_DIR")} {
		if p != "" {
			return explicit(p, cwd)
		}
	}
	return DefaultStateDir(env)
}

func privateFile(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: must be a regular owner-only (0600) file", ErrUnsafeFile)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%w: owner mismatch", ErrUnsafeFile)
	}
	return nil
}

// ReadFile reads a private file once, bounded to MaxSize.
func ReadFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err = privateFile(before); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, info) {
		return nil, fmt.Errorf("%w: file changed while opening", ErrUnsafeFile)
	}
	if err = privateFile(info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err == nil && len(data) > MaxSize {
		return nil, fmt.Errorf("%w: larger than 1 MiB", ErrUnsafeFile)
	}
	return data, err
}

// SaveAtomic replaces path via a same-directory 0600 temporary file, fsync,
// rename and parent fsync. The previous content is kept as "<path>.last-good".
func SaveAtomic(path string, data []byte) error {
	if len(data) > MaxSize {
		return fmt.Errorf("%w: larger than 1 MiB", ErrUnsafeFile)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	previous, err := ReadFile(path)
	switch {
	case err == nil:
		if err = replace(path+".last-good", previous); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return replace(path, data)
}
func replace(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: refusing to replace a symlink", ErrUnsafeFile)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
