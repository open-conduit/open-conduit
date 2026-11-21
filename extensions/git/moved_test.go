package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// moveState renames the provider's state directory and opens a provider on
// its new place, as a daemon started after a move does.
func (f *fixture) moveState() {
	f.t.Helper()
	from := filepath.Join(f.dir, "state")
	to := filepath.Join(f.dir, "elsewhere", "state")
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		f.t.Fatal(err)
	}
	p, err := New(Options{StateDir: to, BaseDir: f.dir, Now: func() time.Time { return f.clock }, DiskSpace: func(string) (uint64, uint64, error) { return f.total, f.free, nil }})
	if err != nil {
		f.t.Fatal(err)
	}
	f.provider = p
}

// A workspace that moved with the state directory keeps its work: a clone
// and a worktree whose checkout stayed are picked up where they now are, with
// their commit and edit. Before this, the old path read as a crash's leftover
// and the workspace at the new path was deleted.
func TestMovedWorkspaceKeepsItsWork(t *testing.T) {
	for _, mode := range []string{"work", "clone"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			c := cfg(map[string]any{"mode": mode, "branch_template": "agent/issue-{work}"})
			r, err := f.acquire(owner("w", "x1"), c, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			write(t, r.Dir, "work.txt", "committed")
			runGit(t, r.Dir, "add", "work.txt")
			runGit(t, r.Dir, "commit", "-qm", "unpushed work")
			write(t, r.Dir, "a.txt", "editing")
			f.moveState()
			again, err := f.acquire(owner("w", "x2"), c, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if again.Ref != r.Ref || again.Dir == r.Dir || !strings.Contains(again.Dir, "elsewhere") || !syncOf(t, again).Reused {
				t.Fatalf("after the move: %+v", again)
			}
			if read(t, filepath.Join(again.Dir, "a.txt")) != "editing" || runGit(t, again.Dir, "log", "-1", "--format=%s") != "unpushed work" {
				t.Fatal("the move lost the workspace's work")
			}
		})
	}
}

// A worktree whose checkout no longer has its record can't be reattached: it
// is set aside whole, a new workspace is made, and Reconcile never removes
// one it can't reattach.
func TestMovedWorktreeThatCantReattachIsKept(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "work", "branch_template": "agent/issue-{work}"})
	r, err := f.acquire(owner("w", "x1"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, r.Dir, "a.txt", "only copy")
	admin := strings.TrimSpace(runGit(t, r.Dir, "rev-parse", "--absolute-git-dir"))
	f.moveState()
	if err := os.RemoveAll(admin); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(f.dir, "elsewhere", "state", "workspaces", r.Ref)
	f.set("x1", failed)
	f.reconcile()
	if read(t, filepath.Join(moved, "a.txt")) != "only copy" {
		t.Fatal("reconcile removed a moved workspace it couldn't reattach")
	}
	again, err := f.acquire(owner("w", "x2"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(again.Dir, "a.txt")) == "only copy" {
		t.Fatal("the new workspace is the broken one")
	}
	kept, err := filepath.Glob(filepath.Join(f.dir, "elsewhere", "state", "moved", r.Ref+"-*", "a.txt"))
	if err != nil || len(kept) != 1 || read(t, kept[0]) != "only copy" {
		t.Fatalf("set aside: %v, %v", kept, err)
	}
}
