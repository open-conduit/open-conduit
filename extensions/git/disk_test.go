package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

func (f *fixture) sweep() extension.ResourceSweepReport {
	f.t.Helper()
	report, err := f.provider.Sweep(context.Background(), func(_ context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s, ok := f.status[o.ExecutionID]
		if !ok {
			return extension.ResourceOwnerStatus{Known: true}, nil
		}
		return s, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

func spares(items []extension.ResourceSpare) string {
	var out []string
	for _, s := range items {
		item := s.Ref + " " + s.Spare
		if s.Kept != "" {
			item += ": " + s.Kept
		}
		out = append(out, item)
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

func (f *fixture) leaseFiles() int {
	entries, _ := os.ReadDir(filepath.Join(f.dir, "state", "leases"))
	return len(entries)
}

// Port of v1 TestSetupRefusesAFullDisk: a disk with room takes a new
// workspace; one that would be left too full refuses it with how much is free,
// what the checkout needs and which of the scope's workspaces can go, and
// nothing is made or leased. Re-acquiring an existing workspace needs no room.
func TestRefusesAWorkspaceOnAFullDisk(t *testing.T) {
	f := newFixture(t)
	if got := f.provider.checkoutSize(context.Background(), f.root, "main"); got != 6 {
		t.Fatalf("checkoutSize = %d, want 6", got)
	}
	work := cfg(map[string]any{"mode": "work"})
	kept, err := f.acquire(owner("w1", "x1"), work, "", nil)
	if err != nil {
		t.Fatalf("a disk with room refused: %v", err)
	}
	// A failed fresh run is retained: finished, so it can go.
	failedRun, err := f.acquire(owner("w2", "x2"), cfg(map[string]any{"mode": "fresh"}), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.set("x2", failed)
	f.reconcile()
	f.mu.Lock()
	probed := append([]string(nil), f.probed...)
	f.mu.Unlock()
	if len(probed) != 2 || probed[0] != filepath.Join(f.dir, "state", "workspaces") {
		t.Fatalf("probed %v, want the workspace root (the nearest existing parent) once per creation", probed)
	}

	// 1 GB free of 100 GB keeps 10 GB (90%, capped at 10 GiB): refused.
	f.setDisk(100<<30, 1<<30)
	var log strings.Builder
	_, err = f.acquire(owner("w3", "x3"), work, "", &log)
	var noSpace *NoSpaceError
	if !errors.As(err, &noSpace) || !errors.Is(err, extension.ErrResourceExhausted) {
		t.Fatalf("acquire on a full disk = %v, want NoSpaceError wrapping ErrResourceExhausted", err)
	}
	if noSpace.Need != 6 || noSpace.Free != 1<<30 || noSpace.Keep != 10<<30 || noSpace.Workspaces != 2 {
		t.Fatalf("NoSpaceError = %+v", noSpace)
	}
	msg := err.Error()
	for _, want := range []string{"not enough disk space", "1.0 GB free", "10.0 GB kept free", failedRun.Ref + " (failed_run)", "Free space, then retry"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, kept.Ref) {
		t.Errorf("a workspace still in use was offered for removal: %q", msg)
	}
	if !strings.Contains(log.String(), "not enough disk space") {
		t.Errorf("the run log does not explain the refusal: %q", log.String())
	}
	ref := refFor(extension.ResourceWork, owner("w3", "x3"))
	if exists(filepath.Join(f.dir, "state", "workspaces", ref)) || exists(f.provider.leasePath(ref)) {
		t.Fatal("a refused workspace was made or leased anyway")
	}
	if _, err = f.acquire(owner("w4", "x4"), cfg(map[string]any{"mode": "fresh"}), "", nil); !errors.As(err, &noSpace) {
		t.Fatalf("fresh on a full disk = %v", err)
	}
	// The existing workspace is reused without a check; the check can be off.
	if again, err := f.acquire(owner("w1", "x1b"), work, "", nil); err != nil || again.Dir != kept.Dir {
		t.Fatalf("re-acquire on a full disk: %v", err)
	}
	if _, err = f.acquire(owner("w3", "x3"), cfg(map[string]any{"mode": "work", "disk_full_percent": 100}), "", nil); err != nil {
		t.Fatalf("disk_full_percent 100 still checked: %v", err)
	}
	// A small reserve fits: 1 GB free keeps at most 512 MiB.
	if _, err = f.acquire(owner("w5", "x5"), cfg(map[string]any{"mode": "work", "disk_reserve_bytes": 512 << 20}), "", nil); err != nil {
		t.Fatalf("a capped reserve refused: %v", err)
	}
	// With nothing finished the message says so.
	f.set("x2", running)
	f.setDisk(100<<30, 0)
	other := cfg(map[string]any{"checkout": "repo", "mode": "work"})
	_, err = extension.BindResources(f.provider, extension.ResourceOwner{ScopeID: "ops", WorkID: "w6", ExecutionID: "x6"}, other).Acquire(context.Background(), extension.ResourceRequest{})
	if err == nil || !strings.HasSuffix(err.Error(), "Free space, then retry") || strings.Contains(err.Error(), "can go") {
		t.Fatalf("refusal for a scope with no workspaces: %v", err)
	}
}

// Port of v1 TestSetupClearsAHalfMadeWorktree: whatever made git fail, no
// directory is left to be reused as if it were whole, the worktree is not
// registered and the lease is dropped.
func TestClearsAHalfMadeWorkspace(t *testing.T) {
	f := newFixture(t)
	var log strings.Builder
	o := owner("w", "x")
	if _, err := f.acquire(o, cfg(map[string]any{"mode": "work", "base_branch": "nope"}), "", &log); err == nil {
		t.Fatal("a work workspace from a missing base was made")
	}
	path := filepath.Join(f.dir, "state", "workspaces", refFor(extension.ResourceWork, o))
	if exists(path) || f.leaseFiles() != 0 {
		t.Fatal("the failed workspace or its lease was left behind")
	}
	if strings.Contains(runGit(t, f.root, "worktree", "list"), path) {
		t.Fatal("the failed workspace is still registered")
	}
	if !strings.Contains(log.String(), "the unfinished workspace was cleared") {
		t.Fatalf("log: %q", log.String())
	}
	// A partial checkout left by a crash in the same place is cleared before
	// the next creation, and the workspace then comes up whole.
	write(t, path, "half", "x")
	r, err := f.acquire(o, cfg(map[string]any{"mode": "work"}), "", nil)
	if err != nil || exists(filepath.Join(r.Dir, "half")) || read(t, filepath.Join(r.Dir, "a.txt")) != "v1" {
		t.Fatalf("after a half-made checkout: %v", err)
	}
}

// Port of v1 TestIssueWorktrees and the sweep: finished Work's workspaces and
// idle clean ones no lease or record holds are removed (branches kept); dirty,
// unpushed and in-use ones are kept and reported; live ones are untouched.
func TestSweepRemovesSpareWorkspacesOnly(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, ".env", "KEY=1\n")
	work := cfg(map[string]any{"mode": "work", "copy": []string{".env"}})
	acquire := func(w, x string) extension.Resource {
		t.Helper()
		r, err := f.acquire(owner(w, x), work, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	finished := extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, WorkDone: true}
	done := acquire("done", "x-done") // a cancelled Work's; only the copied .env differs
	dirty := acquire("dirty", "x-dirty")
	write(t, dirty.Dir, "notes.txt", "unsaved")
	unpushed := acquire("unpushed", "x-unpushed")
	write(t, unpushed.Dir, "a.txt", "local")
	runGit(t, unpushed.Dir, "commit", "-qam", "local only")
	busy := acquire("busy", "x-busy")
	live := acquire("live", "x-live")
	unknown := acquire("gone", "x-gone")
	freshFailed, err := f.acquire(owner("job", "x-job"), cfg(map[string]any{"mode": "fresh"}), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []string{"x-done", "x-dirty", "x-unpushed", "x-busy"} {
		f.set(x, finished)
	}
	f.set("x-job", failed) // a failed run of Work that goes on: retention applies
	f.set("x-gone", extension.ResourceOwnerStatus{})
	if report := f.reconcile(); report.Retained != 6 || report.Active != 1 {
		t.Fatalf("reconcile: %+v", report)
	}

	sleeper := exec.Command("sleep", "60")
	sleeper.Dir = busy.Dir
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()

	// Directories no lease holds: one made by hand and idle for two days, one
	// idle but dirty, one touched recently and junk git can't read.
	orphan := func(name string) string {
		path := filepath.Join(f.dir, "state", "workspaces", name)
		runGit(t, f.root, "worktree", "add", "-q", "--detach", path, "origin/main")
		return path
	}
	idle := f.clock.Add(-48 * time.Hour)
	clean := orphan("work-" + strings.Repeat("a", 32))
	stale := orphan("fresh-" + strings.Repeat("b", 32))
	write(t, stale, "scratch.txt", "x")
	recent := orphan("work-" + strings.Repeat("c", 32))
	junk := filepath.Join(f.dir, "state", "workspaces", "fresh-"+strings.Repeat("d", 32))
	write(t, junk, "x", "y")
	for _, p := range []string{clean, stale, junk, unknown.Dir} {
		if err := os.Chtimes(p, idle, idle); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(recent, f.clock, f.clock); err != nil {
		t.Fatal(err)
	}

	report := f.sweep()
	wantRemoved := []extension.ResourceSpare{
		{Ref: done.Ref, Spare: spareFinished}, {Ref: unknown.Ref, Spare: spareUnknown}, {Ref: filepath.Base(clean), Spare: spareOrphaned},
	}
	wantKept := []extension.ResourceSpare{
		{Ref: dirty.Ref, Spare: spareFinished, Kept: keptUncommitted}, {Ref: unpushed.Ref, Spare: spareFinished, Kept: keptUnpushed},
		{Ref: busy.Ref, Spare: spareFinished, Kept: keptInUse}, {Ref: filepath.Base(stale), Spare: spareOrphaned, Kept: keptUncommitted},
		{Ref: filepath.Base(junk), Spare: spareOrphaned, Kept: keptUnreadable},
	}
	if got, want := spares(report.Removed), spares(wantRemoved); got != want {
		t.Fatalf("removed:\n got %s\nwant %s", got, want)
	}
	if got, want := spares(report.Kept), spares(wantKept); got != want {
		t.Fatalf("kept:\n got %s\nwant %s", got, want)
	}
	for _, path := range []string{done.Dir, unknown.Dir, clean} {
		if exists(path) {
			t.Errorf("%s was not removed", path)
		}
	}
	for _, path := range []string{dirty.Dir, unpushed.Dir, busy.Dir, live.Dir, freshFailed.Dir, stale, recent, junk} {
		if !exists(path) {
			t.Errorf("%s was removed", path)
		}
	}
	list := runGit(t, f.root, "worktree", "list")
	if strings.Contains(list, done.Dir) || strings.Contains(list, clean) {
		t.Fatalf("swept worktrees still registered: %s", list)
	}
	runGit(t, f.root, "show-ref", "--verify", "-q", "refs/heads/agent/work-done")
	if exists(f.provider.leasePath(done.Ref)) || !exists(f.provider.leasePath(dirty.Ref)) {
		t.Fatal("the swept lease was kept or a kept one dropped")
	}
	for _, m := range report.Kept {
		if m.Mode == "" {
			t.Errorf("kept %s has no mode", m.Ref)
		}
	}

	// Once the unsaved work is gone, the next sweep takes it.
	if err := os.Remove(filepath.Join(dirty.Dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if again := f.sweep(); !strings.Contains(spares(again.Removed), dirty.Ref) {
		t.Fatalf("second sweep: %+v", again)
	}
}
