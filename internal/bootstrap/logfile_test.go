package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log rotates at its size limit, keeps the newest files, never splits a
// line, and appends to an existing file across a reopen.
func TestLogRotatesBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "lifecycle.jsonl")
	l, err := openRotatingLog(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := func(i int) []byte {
		return []byte(fmt.Sprintf("{\"n\":%02d,\"pad\":\"%s\"}\n", i, strings.Repeat("x", 25)))
	} // 40 bytes
	for i := range 4 {
		if _, err = l.Write(line(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening appends rather than truncates, and counts the bytes there.
	if l, err = openRotatingLog(path, 100, 2); err != nil {
		t.Fatal(err)
	}
	for i := 4; i < 10; i++ {
		if _, err = l.Write(line(i)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", p, info.Mode())
		}
		return string(b)
	}
	// Two lines fit in 100 bytes: 0-1, 2-3, 4-5, 6-7, 8-9; .1 and .2 are kept.
	for p, want := range map[string]string{path: string(line(8)) + string(line(9)), path + ".1": string(line(6)) + string(line(7)), path + ".2": string(line(4)) + string(line(5))} {
		if got := read(p); got != want {
			t.Fatalf("%s:\n%s\nwant\n%s", filepath.Base(p), got, want)
		}
	}
	if _, err = os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("more than the kept files: %v", err)
	}
}

// A line longer than the limit is written whole to its own file.
func TestLogKeepsAnOversizedLineWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	l, err := openRotatingLog(path, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	long := strings.Repeat("y", 30) + "\n"
	for range 2 {
		if _, err = l.Write([]byte(long)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{path, path + ".1"} {
		if b, _ := os.ReadFile(p); string(b) != long {
			t.Fatalf("%s holds %q", p, b)
		}
	}
}
