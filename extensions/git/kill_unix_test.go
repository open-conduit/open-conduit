//go:build !windows

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A process run from inside the workspace by a global binary names no path in
// its command line, and what it starts may move elsewhere: both have to go.
func TestKillWithinFindsProcessesByWorkingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	_ = os.MkdirAll(filepath.Join(path, "sub"), 0o755)
	outside := t.TempDir()
	other := exec.Command("sleep", "60")
	other.Dir = outside
	parent := exec.Command("sh", "-c", `(cd / && exec sleep 61) & echo $! > child.pid; wait`)
	parent.Dir = filepath.Join(path, "sub")
	for _, c := range []*exec.Cmd{other, parent} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = c.Wait() }()
	}
	defer func() { _ = other.Process.Kill() }()
	var child int
	for i := 0; i < 100 && child == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		data, _ := os.ReadFile(filepath.Join(path, "sub", "child.pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if child == 0 {
		t.Fatal("the child never started")
	}
	time.Sleep(100 * time.Millisecond) // let the child cd out
	killWithin(path)
	time.Sleep(100 * time.Millisecond) // the parent is reaped by its Wait
	if alive(parent.Process.Pid) {
		t.Error("the process sitting in the workspace survived")
	}
	if alive(child) {
		t.Error("its child, which had moved out of the workspace, survived")
	}
	if !alive(other.Process.Pid) {
		t.Error("a process outside the workspace was killed")
	}
}
