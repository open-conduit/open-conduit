package extensiontest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A miss fills a new entry whole; a hit reuses it without filling again.
func TestCacheEntryBuildsOnceAndReuses(t *testing.T) {
	root := t.TempDir()
	fills := 0
	fill := func(dir string) error {
		fills++
		return os.WriteFile(filepath.Join(dir, "bin"), []byte("built"), 0o755)
	}
	first, err := cacheEntry(root, "bin-k", fill)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cacheEntry(root, "bin-k", fill)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != filepath.Join(root, "bin-k") || fills != 1 {
		t.Fatalf("entries %q, %q after %d fills; want one fill of %s", first, second, fills, filepath.Join(root, "bin-k"))
	}
	if b, err := os.ReadFile(filepath.Join(first, "bin")); err != nil || string(b) != "built" {
		t.Fatalf("entry holds %q, %v", b, err)
	}
	assertOnly(t, root, "bin-k")
}

// A failed fill leaves nothing behind.
func TestCacheEntryFailedFillLeavesNothing(t *testing.T) {
	root := t.TempDir()
	if _, err := cacheEntry(root, "bin-k", func(string) error { return errors.New("no compiler") }); err == nil {
		t.Fatal("a failed fill made an entry")
	}
	assertOnly(t, root)
}

// When another test binary settled the same entry first, the second build
// is dropped and the first kept.
func TestSettleEntryKeepsTheFirst(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "pkg-k")
	if err := os.MkdirAll(filepath.Join(entry, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(root, "oc-devscope-pkg-1")
	if err := os.MkdirAll(filepath.Join(late, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := settleEntry(late, entry)
	if err != nil || got != entry {
		t.Fatalf("settled %q, %v; want %s", got, err, entry)
	}
	assertOnly(t, root, "pkg-k")
}

// Pruning removes entries (and half-built ones) unused for cacheTTL, and
// keeps the rest; using an entry renews it.
func TestPruneCacheRemovesUnused(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{
		"bin-old":      cacheTTL + time.Hour,
		".building-1":  cacheTTL + time.Minute,
		"bin-recent":   cacheTTL - time.Hour,
		"pkg-just-now": 0,
	} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	pruneCache(root, now)
	assertOnly(t, root, "bin-recent", "pkg-just-now")

	if _, err := cacheEntry(root, "bin-recent", func(string) error { t.Fatal("a hit filled"); return nil }); err != nil {
		t.Fatal(err)
	}
	// Unused, bin-recent would be past cacheTTL by then.
	pruneCache(root, now.Add(cacheTTL-30*time.Minute))
	assertOnly(t, root, "bin-recent", "pkg-just-now")
}

// Beyond the keptBins most recently used, pruning removes bin- entries
// whatever their age, except one used within inUse; pkg- entries are
// bounded by cacheTTL alone.
func TestPruneCacheCapsBinaries(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	ages := map[string]time.Duration{
		"bin-1": 2 * inUse, "bin-2": 3 * inUse, "bin-3": 4 * inUse, "bin-4": 5 * inUse,
		"bin-5": 6 * inUse,
		"bin-6": 7 * inUse,
		"pkg-1": 8 * inUse, "pkg-2": 9 * inUse, "pkg-3": 10 * inUse, "pkg-4": 11 * inUse, "pkg-5": 12 * inUse,
	}
	for name, age := range ages {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	pruneCache(root, now)
	assertOnly(t, root, "bin-1", "bin-2", "bin-3", "bin-4", "pkg-1", "pkg-2", "pkg-3", "pkg-4", "pkg-5")

	// Seven entries in use: none is removed, and the older ones go.
	for i := 1; i <= 7; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("bin-new-%d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pruneCache(root, now)
	assertOnly(t, root, "bin-new-1", "bin-new-2", "bin-new-3", "bin-new-4", "bin-new-5", "bin-new-6", "bin-new-7", "pkg-1", "pkg-2", "pkg-3", "pkg-4", "pkg-5")
}

// treeKey changes with a file's content and name, and not otherwise.
func TestTreeKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	key := func() string {
		t.Helper()
		k, err := treeKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	write("manifest.json", "{}")
	a := key()
	if key() != a {
		t.Fatal("treeKey differs for the same tree")
	}
	write("manifest.json", `{"name":"x"}`)
	b := key()
	if err := os.Rename(filepath.Join(dir, "manifest.json"), filepath.Join(dir, "other.json")); err != nil {
		t.Fatal(err)
	}
	c := key()
	if a == b || b == c || a == c {
		t.Fatalf("keys %s, %s, %s; want each change to give a new key", a, b, c)
	}
}

func assertOnly(t *testing.T, root string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("cache holds %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("cache holds %v; want %v", got, want)
		}
	}
}
