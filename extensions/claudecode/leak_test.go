package claudecode_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/claudecode"
)

// Turns and tasks run in detached process groups of their own, which only a
// host's sweep bounds. A test has no sweep coming back, so the harness owns
// every group it spawns: its cleanup kills whatever still runs under its
// root, and TestMain fails the run if anything survived (#654).

// rootPrefix names a harness root after the test binary's PID, so a later
// run can tell a dead binary's leftovers (a -timeout panic skips every
// cleanup) from a live one's.
const rootPrefix = "occ-test-"

var (
	rootsMu sync.Mutex
	roots   []string
	deadRun = regexp.MustCompile(`/` + rootPrefix + `([0-9]+)-`)
)

// newRoot makes a harness root and makes sure nothing outlives it: the
// cleanup it registers runs before any registered after it, the root's
// removal among them.
func newRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", rootPrefix, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	rootsMu.Lock()
	roots = append(roots, root)
	rootsMu.Unlock()
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Cleanup(func() {
		if err := reap(root); err != nil {
			t.Error(err)
		}
	})
	return root
}

// under lists the processes whose arguments name a path under one of roots:
// every turn and task wrapper does, since each is given its own directory.
func under(roots ...string) ([]claudecode.Process, error) {
	all, err := claudecode.Processes()
	if err != nil {
		return nil, err
	}
	var out []claudecode.Process
	for _, p := range all {
		if p.PID == os.Getpid() {
			continue
		}
		for _, r := range roots {
			if strings.Contains(p.Args, r+string(filepath.Separator)) || strings.HasSuffix(p.Args, r) {
				out = append(out, p)
				break
			}
		}
	}
	return out, nil
}

// kill SIGKILLs p, and its whole group when it leads one other than ours.
func kill(p claudecode.Process) {
	if p.PGID == p.PID && p.PGID != syscall.Getpgrp() {
		_ = syscall.Kill(-p.PGID, syscall.SIGKILL)
	}
	_ = syscall.Kill(p.PID, syscall.SIGKILL)
}

// reap kills every process under roots until none is left; a wrapper being
// started as it looks is caught on a later pass.
func reap(roots ...string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		left, err := under(roots...)
		if err != nil || len(left) == 0 {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes survived SIGKILL under %v: %v", roots, left)
		}
		for _, p := range left {
			kill(p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reapDeadRuns kills the wrapper groups a dead test binary left: a group
// leader carrying an executor's marker under a root named for a PID that no
// longer runs.
func reapDeadRuns() {
	all, err := claudecode.Processes()
	if err != nil {
		return
	}
	for _, p := range all {
		m := deadRun.FindStringSubmatch(p.Args)
		if m == nil || p.PGID != p.PID || !strings.Contains(p.Args, "open-conduit-") {
			continue
		}
		owner, _ := strconv.Atoi(m[1])
		if owner <= 1 || owner == os.Getpid() || !errors.Is(syscall.Kill(owner, 0), syscall.ESRCH) {
			continue
		}
		fmt.Fprintf(os.Stderr, "reaping process group %d a dead test run (%d) left: %.200s\n", p.PGID, owner, p.Args)
		kill(p)
	}
}

// TestMain fails the run when any process a harness spawned is still alive
// after every test and its cleanups have run, and kills it.
func TestMain(m *testing.M) {
	reapDeadRuns()
	code := m.Run()
	rootsMu.Lock()
	all := append([]string(nil), roots...)
	rootsMu.Unlock()
	if left, err := under(all...); err != nil {
		fmt.Fprintf(os.Stderr, "leak guard: listing processes: %v\n", err)
		code = 1
	} else if len(left) > 0 {
		fmt.Fprintf(os.Stderr, "leak guard: the run left %d processes behind:\n", len(left))
		for _, p := range left {
			fmt.Fprintf(os.Stderr, "  pid %d pgid %d: %.200s\n", p.PID, p.PGID, p.Args)
		}
		_ = reap(all...)
		code = 1
	}
	os.Exit(code)
}

// TestLeakGuardCatchesALeakedTask is the guard's negative control: a task
// left running when its test stops caring (no TaskStop, no sweep) is found
// under its harness's root, and reaping kills its whole group.
func TestLeakGuardCatchesALeakedTask(t *testing.T) {
	th, _ := liveTurn(t, nil)
	task := th.start(t.Context(), map[string]any{"command": "while :; do sleep 0.05; done", "name": "leak"})
	pidFile := filepath.Join(th.state, "tasks", task["task"].(string), "pid")
	waitFor(t, "the task's wrapper", func() bool { _, err := os.Stat(pidFile); return err == nil })
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	left, err := under(th.root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range left {
		found = found || p.PID == pid
	}
	if !found {
		t.Fatalf("the guard did not find the leaked task's wrapper %d among %v", pid, left)
	}
	if err := reap(th.root); err != nil {
		t.Fatal(err)
	}
	if left, _ := under(th.root); len(left) > 0 {
		t.Fatalf("reaping left %v", left)
	}
	if !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
		t.Fatalf("the task's group %d survived reaping", pid)
	}
}
