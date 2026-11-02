package extensiontest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// What extensiontest builds (open-conduit, os and the packages under test)
// it keeps in a cache directory rather than a new temporary directory per
// test binary, which nothing could remove afterwards: this package can't
// own its importers' TestMain. A later run reuses an entry, and an entry no
// run has used for cacheTTL is removed by the next run that looks (#445).

// CacheEnv names a directory to keep the cache in instead of
// <user cache directory>/open-conduit/extensiontest.
const CacheEnv = "OPEN_CONDUIT_EXTENSIONTEST_CACHE"

// cacheTTL is how long an unused entry is kept.
const cacheTTL = 24 * time.Hour

// cacheRoot is the cache directory.
func cacheRoot() (string, error) {
	if dir := os.Getenv(CacheEnv); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("extensiontest: no cache directory for what it builds (set %s): %w", CacheEnv, err)
	}
	return filepath.Join(base, "open-conduit", "extensiontest"), nil
}

// openCache makes the cache directory and removes its stale entries.
func openCache() (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("extensiontest: cache: %w", err)
	}
	pruneCache(root, time.Now())
	return root, nil
}

// pruneCache removes every entry (and every half-built one) under root
// last used before now less cacheTTL.
func pruneCache(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		if now.Sub(info.ModTime()) > cacheTTL {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// cacheEntry is root/key, made by fill on a miss. fill writes into a new
// directory, which becomes the entry whole by a rename, so a concurrent
// test binary sees no entry or a complete one. A hit marks the entry used.
func cacheEntry(root, key string, fill func(dir string) error) (string, error) {
	entry := filepath.Join(root, key)
	if _, err := os.Stat(entry); err == nil {
		now := time.Now()
		_ = os.Chtimes(entry, now, now)
		return entry, nil
	}
	tmp, err := os.MkdirTemp(root, ".building-")
	if err != nil {
		return "", fmt.Errorf("extensiontest: cache: %w", err)
	}
	if err := fill(tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return settleEntry(tmp, entry)
}

// settleEntry renames the built directory tmp to entry; when entry is
// already there (another test binary got there first, or built the same
// package), it keeps that one and marks it used.
func settleEntry(tmp, entry string) (string, error) {
	if err := os.Rename(tmp, entry); err != nil {
		_ = os.RemoveAll(tmp)
		if _, serr := os.Stat(entry); serr != nil {
			return "", fmt.Errorf("extensiontest: cache: %w", err)
		}
		now := time.Now()
		_ = os.Chtimes(entry, now, now)
	}
	return entry, nil
}

// buildKey identifies what `go build` makes of each package with args:
// go list's build IDs change with the source of the package and of
// everything it imports, and with the flags.
func buildKey(args []string, pkgs ...string) (string, error) {
	list := append(append([]string{"list", "-export", "-f", "{{.ImportPath}} {{.BuildID}}"}, args...), pkgs...)
	out, err := exec.Command("go", list...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("go list %s: %v\n%s", strings.Join(pkgs, " "), err, exit.Stderr)
		}
		return "", fmt.Errorf("go list %s: %v", strings.Join(pkgs, " "), err)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s/%s %q\n%s", runtime.GOOS, runtime.GOARCH, args, out)
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

// treeKey is a digest of every file under dir: its path, mode and content.
func treeKey(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %o\n", filepath.ToSlash(rel), info.Mode())
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}
