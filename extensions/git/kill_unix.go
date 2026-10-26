//go:build !windows

package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killGrace is how long a process gets after SIGTERM before SIGKILL.
var killGrace = 3 * time.Second

// runGroup runs cmd as its own process group and kills the whole group when
// ctx ends, so a timed-out setup leaves nothing behind.
func runGroup(ctx context.Context, cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(killGrace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		return ctx.Err()
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// killWithin stops processes left running from a workspace being removed (a
// dev server keeps writing into it after the delete), with their descendants.
// A process counts when the path is in its command line or its working
// directory is inside the workspace. The caller and its ancestors are spared.
// Removal only follows a confirmed stop of the owning execution; this is the
// backstop for processes that escaped its process group.
func killWithin(path string) {
	roots := map[int]bool{}
	for _, pid := range pgrep(path + string(filepath.Separator)) {
		roots[pid] = true
	}
	for _, pid := range cwdWithin(path) {
		roots[pid] = true
	}
	if len(roots) == 0 {
		return
	}
	parents := processTable()
	spared := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !spared[pid]; pid = parents[pid] {
		spared[pid] = true
	}
	var pids []int
	for _, pid := range withDescendants(roots, parents) {
		if !spared[pid] && pid > 1 {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(killGrace)
	for _, pid := range pids {
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func pgrep(pattern string) []int {
	out, _ := exec.Command("pgrep", "-f", pattern).Output()
	return pidList(strings.Fields(string(out)))
}

// cwdWithin lists processes whose working directory is the path or below it,
// from /proc where available (Linux) and lsof otherwise (macOS).
func cwdWithin(path string) []int {
	prefixes := []string{filepath.Clean(path)}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != prefixes[0] {
		prefixes = append(prefixes, real)
	}
	in := func(dir string) bool {
		for _, p := range prefixes {
			if dir == p || strings.HasPrefix(dir, p+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	var pids []int
	if entries, err := os.ReadDir("/proc"); err == nil && len(entries) > 0 {
		if _, err := os.Readlink("/proc/self/cwd"); err == nil {
			for _, e := range entries {
				pid, err := strconv.Atoi(e.Name())
				if err != nil {
					continue
				}
				if dir, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd")); err == nil && in(dir) {
					pids = append(pids, pid)
				}
			}
			return pids
		}
	}
	// -Fpn prints "p<pid>", then "fcwd" and "n<dir>" for each process.
	out, _ := exec.Command("lsof", "-w", "-d", "cwd", "-Fpn").Output()
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0 && in(line[1:]):
			pids = append(pids, pid)
		}
	}
	return pids
}

// processTable maps every running process to its parent.
func processTable() map[int]int {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			parents[pid] = ppid
		}
	}
	return parents
}

// withDescendants is the roots and everything below them, parents first.
func withDescendants(roots map[int]bool, parents map[int]int) []int {
	children := map[int][]int{}
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	seen := map[int]bool{}
	var all []int
	var walk func(int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		all = append(all, pid)
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for pid := range roots {
		walk(pid)
	}
	return all
}

func pidList(fields []string) []int {
	var pids []int
	for _, f := range fields {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// inUse reports whether a process other than this one and its ancestors runs
// from the workspace: its command line names the path or its working
// directory is inside it.
func inUse(path string) bool {
	parents := processTable()
	spared := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !spared[pid]; pid = parents[pid] {
		spared[pid] = true
	}
	for _, pid := range append(pgrep(path+string(filepath.Separator)), cwdWithin(path)...) {
		if !spared[pid] && pid > 1 {
			return true
		}
	}
	return false
}
