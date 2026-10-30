package git

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func branchParams(branch string) domain.Payload {
	b, _ := json.Marshal(map[string]string{"branch": branch})
	return domain.Payload{Schema: ParamsSchema, Data: b}
}

// resolvesInto lists the paths under dir that resolve, through symlinks, to
// root or inside it.
func resolvesInto(t *testing.T, dir, root string) []string {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	err = filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if rel, err := filepath.Rel(root, real); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// A clone is private to its Work, reused by each attempt with its commits
// and edits, pushes to the real remote and shares nothing writable with the
// checkout. When the Work completes it goes, and a branch with commits no
// remote has is kept in the checkout.
func TestCloneIsReusedAndPushesToTheRealRemote(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "clone", "branch_template": "agent/issue-{work}", "copy": []string{".env"}})
	write(t, f.root, ".env", "A=1\n")
	runGit(t, f.root, "config", "user.email", "operator@example.com")
	var log strings.Builder
	r, err := f.acquire(owner("PROJ 1", "x1"), c, "", &log)
	if err != nil {
		t.Fatal(err)
	}
	w := syncOf(t, r)
	if w.Mode != ModeClone || !strings.HasPrefix(r.Ref, "clone-") || w.Branch != "agent/issue-PROJ-1" || runGit(t, r.Dir, "rev-parse", "--abbrev-ref", "HEAD") != "agent/issue-PROJ-1" {
		t.Fatalf("clone: %+v %s", w, r.Ref)
	}
	if r.Summary != "clone on branch agent/issue-PROJ-1" || !strings.Contains(log.String(), "cloned the checkout and created agent/issue-PROJ-1 from origin/main") {
		t.Fatalf("summary %q, log %s", r.Summary, log.String())
	}
	if got := runGit(t, r.Dir, "remote", "get-url", "origin"); got != f.origin {
		t.Fatalf("origin is %s, want the real remote %s", got, f.origin)
	}
	if got := runGit(t, r.Dir, "rev-parse", "origin/main"); got != runGit(t, f.root, "rev-parse", "origin/main") {
		t.Fatalf("origin/main is %s, not the checkout's", got)
	}
	if strings.Contains(runGit(t, r.Dir, "branch", "--list"), "main") {
		t.Fatal("the clone kept the checkout's main as a local branch")
	}
	if runGit(t, r.Dir, "config", "--file", filepath.Join(r.Dir, ".git", "config"), "user.email") != "operator@example.com" || read(t, filepath.Join(r.Dir, ".env")) != "A=1\n" {
		t.Fatal("the clone lost the checkout's identity or the copied .env")
	}

	// Nothing in the clone, followed through symlinks, is the checkout's .git.
	if hits := resolvesInto(t, r.Dir, filepath.Join(f.root, ".git")); len(hits) > 0 {
		t.Fatalf("paths in the clone resolve into the checkout's .git: %v", hits)
	}
	if exists(filepath.Join(r.Dir, ".git", "objects", "info", "alternates")) {
		t.Fatal("the clone borrows objects through alternates")
	}
	if info, err := os.Lstat(filepath.Join(r.Dir, ".git")); err != nil || !info.IsDir() {
		t.Fatal("the clone's .git is not a directory of its own")
	}

	// A commit and an edit survive the attempt; the next one reuses them.
	write(t, r.Dir, "work.txt", "committed")
	runGit(t, r.Dir, "add", "work.txt")
	runGit(t, r.Dir, "commit", "-qm", "work")
	write(t, r.Dir, "a.txt", "editing")
	f.set("x1", failed)
	f.reconcile()
	write(t, f.root, ".env", "A=1\nB=2\n")
	again, err := f.acquire(owner("PROJ 1", "x2"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref != r.Ref || again.Dir != r.Dir || !syncOf(t, again).Reused || syncOf(t, again).Dirty != 1 || again.Summary != "clone on branch agent/issue-PROJ-1, reused, 1 uncommitted path(s) inherited" {
		t.Fatalf("reuse: %+v %+v", again, syncOf(t, again))
	}
	if read(t, filepath.Join(r.Dir, "a.txt")) != "editing" || runGit(t, r.Dir, "log", "-1", "--format=%s") != "work" || read(t, filepath.Join(r.Dir, ".env")) != "A=1\nB=2\n" {
		t.Fatal("reuse reset the clone or didn't refresh copied files")
	}
	if _, err := extension.BindResources(f.provider, owner("PROJ 2", "x9"), c).Lookup(context.Background(), r.Ref); !errors.Is(err, extension.ErrResourceNotFound) {
		t.Fatalf("lookup by another Work: %v", err)
	}
	if _, err := extension.BindResources(f.provider, owner("PROJ 1", "x3"), c).Lookup(context.Background(), r.Ref); err != nil {
		t.Fatalf("lookup by the Work: %v", err)
	}

	// The branch pushes to the real remote, using the operator's own git.
	runGit(t, r.Dir, "push", "-q", "-u", "origin", "HEAD")
	if runGit(t, f.origin, "rev-parse", "agent/issue-PROJ-1") != runGit(t, r.Dir, "rev-parse", "HEAD") {
		t.Fatal("the push didn't reach the remote")
	}
	if strings.Contains(runGit(t, f.root, "branch", "--list", "agent/issue-PROJ-1"), "agent") {
		t.Fatal("the clone's branch appeared in the checkout")
	}

	// A commit nobody pushed is saved into the checkout when the clone goes.
	write(t, r.Dir, "a.txt", "final")
	runGit(t, r.Dir, "commit", "-qam", "unpushed")
	head := runGit(t, r.Dir, "rev-parse", "HEAD")
	f.set("x3", extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, ExecutionSucceeded: true, WorkDone: true, WorkSucceeded: true})
	if report := f.reconcile(); report.Removed != 1 || exists(r.Dir) || exists(f.provider.leasePath(r.Ref)) {
		t.Fatalf("after Work completed: %+v", report)
	}
	if got := runGit(t, f.root, "rev-parse", "refs/heads/agent/issue-PROJ-1"); got != head {
		t.Fatalf("the unpushed branch was not saved: %s, want %s", got, head)
	}
}

// A clone holds its own objects: a gc, prune or repack in the checkout never
// breaks it, even with the objects it was cloned with gone from the checkout.
func TestCloneSurvivesTheSourceBeingGarbageCollected(t *testing.T) {
	f := newFixture(t)
	// A branch the checkout drops later, so gc removes its objects there.
	runGit(t, f.root, "checkout", "-q", "-b", "agent/issue-9")
	write(t, f.root, "only-here.txt", "kept by the clone")
	runGit(t, f.root, "add", ".")
	runGit(t, f.root, "commit", "-qm", "only here")
	commit := runGit(t, f.root, "rev-parse", "HEAD")
	runGit(t, f.root, "checkout", "-q", "main")
	loose := filepath.Join(".git", "objects", commit[:2], commit[2:])
	if !exists(filepath.Join(f.root, loose)) {
		t.Fatal("fixture: the commit is not a loose object")
	}

	r, err := f.acquire(owner("9", "x1"), cfg(map[string]any{"mode": "clone", "branch_template": "agent/issue-{work}"}), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if runGit(t, r.Dir, "rev-parse", "HEAD") != commit {
		t.Fatal("the clone is not on the checkout's existing branch")
	}
	src, err1 := os.Stat(filepath.Join(f.root, loose))
	dst, err2 := os.Stat(filepath.Join(r.Dir, loose))
	if err1 != nil || err2 != nil || !os.SameFile(src, dst) {
		t.Fatalf("on one filesystem the clone's objects should be hardlinks of the checkout's: %v %v", err1, err2)
	}

	runGit(t, f.root, "branch", "-q", "-D", "agent/issue-9")
	runGit(t, f.root, "reflog", "expire", "--expire=now", "--all")
	runGit(t, f.root, "gc", "-q", "--prune=now")
	runGit(t, f.root, "repack", "-q", "-a", "-d", "-f")
	runGit(t, f.root, "prune", "--expire=now")
	if exists(filepath.Join(f.root, loose)) {
		t.Fatal("fixture: gc left the object in the checkout")
	}

	runGit(t, r.Dir, "fsck", "--full", "--strict")
	if read(t, filepath.Join(r.Dir, "only-here.txt")) != "kept by the clone" || runGit(t, r.Dir, "cat-file", "-t", commit) != "commit" {
		t.Fatal("the clone lost an object to the checkout's gc")
	}
	write(t, r.Dir, "more.txt", "x")
	runGit(t, r.Dir, "add", ".")
	runGit(t, r.Dir, "commit", "-qm", "after gc")
	runGit(t, r.Dir, "push", "-q", "origin", "HEAD")
	runGit(t, f.origin, "cat-file", "-e", commit)
	if _, err := f.acquire(owner("9", "x2"), cfg(map[string]any{"mode": "clone", "branch_template": "agent/issue-{work}"}), "", nil); err != nil {
		t.Fatalf("reuse after gc: %v", err)
	}
}

// A failed Work's clone is retained, then removed when retention ends; the
// sweep removes a finished clean clone and an idle orphan and keeps one with
// commits no remote has; a new Work on a retained clone's branch takes it
// over.
func TestCloneRetentionSweepAndTakeOver(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "clone", "retain_failed_seconds": 3600})
	acquire := func(w, x string) extension.Resource {
		t.Helper()
		r, err := f.acquire(owner(w, x), c, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	finished := extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, WorkDone: true}

	retained := acquire("retained", "x-retained")
	f.set("x-retained", finished)
	if report := f.reconcile(); report.Retained != 1 || !exists(retained.Dir) {
		t.Fatalf("failed Work: %+v", report)
	}
	f.advance(2 * time.Hour)
	if report := f.reconcile(); report.Removed != 1 || exists(retained.Dir) {
		t.Fatalf("retention expired: %+v", report)
	}

	clean := acquire("clean", "x-clean")
	unpushed := acquire("unpushed", "x-unpushed")
	write(t, unpushed.Dir, "a.txt", "local")
	runGit(t, unpushed.Dir, "commit", "-qam", "local only")
	f.set("x-clean", finished)
	f.set("x-unpushed", finished)
	f.reconcile()
	orphan := filepath.Join(f.dir, "state", "workspaces", "clone-"+strings.Repeat("e", 32))
	runGit(t, f.dir, "clone", "-q", f.origin, orphan)
	idle := f.clock.Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, idle, idle); err != nil {
		t.Fatal(err)
	}
	report := f.sweep()
	wantRemoved := []extension.ResourceSpare{{Ref: clean.Ref, Spare: spareFinished}, {Ref: filepath.Base(orphan), Spare: spareOrphaned}}
	wantKept := []extension.ResourceSpare{{Ref: unpushed.Ref, Spare: spareFinished, Kept: keptUnpushed}}
	if got, want := spares(report.Removed), spares(wantRemoved); got != want {
		t.Fatalf("removed:\n got %s\nwant %s", got, want)
	}
	if got, want := spares(report.Kept), spares(wantKept); got != want {
		t.Fatalf("kept:\n got %s\nwant %s", got, want)
	}
	if exists(clean.Dir) || exists(orphan) || !exists(unpushed.Dir) {
		t.Fatal("the sweep removed the wrong clones")
	}

	// The unpushed clone is retained on agent/work-unpushed; a new Work on
	// that branch takes it over with its commit.
	var log strings.Builder
	moved, err := extension.BindResources(f.provider, owner("again2", "y1"), c).Acquire(context.Background(), extension.ResourceRequest{Params: branchParams("agent/work-unpushed"), Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if !syncOf(t, moved).Reused || exists(unpushed.Dir) || runGit(t, moved.Dir, "log", "-1", "--format=%s") != "local only" || exists(f.provider.leasePath(unpushed.Ref)) {
		t.Fatalf("take-over: %+v, log %s", syncOf(t, moved), log.String())
	}
	f.set("y1", running)
	busy := extension.BindResources(f.provider, owner("again3", "z1"), c)
	if _, err := busy.Acquire(context.Background(), extension.ResourceRequest{Params: branchParams("agent/work-unpushed")}); err == nil || !strings.Contains(err.Error(), "again2") {
		t.Fatalf("a live Work's branch: %v", err)
	}
}

// A clone is sized for the disk check: on the checkout's filesystem its
// objects are hardlinks and count for nothing; on another they are copied
// and count in full.
func TestCloneDiskRefusal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	write(t, f.root, "big.bin", strings.Repeat("x", 64<<10))
	runGit(t, f.root, "add", ".")
	runGit(t, f.root, "commit", "-qm", "big")
	runGit(t, f.root, "push", "-q", "origin", "main")
	files := f.provider.checkoutSize(ctx, f.root, "origin/main")
	objects := func() uint64 {
		f.provider.sameDisk = func(string, string) bool { return false }
		defer func() { f.provider.sameDisk = sameDevice }()
		return f.provider.cloneObjects(ctx, f.root, filepath.Join(f.dir, "state", "workspaces", "x"))
	}()
	if got := f.provider.cloneObjects(ctx, f.root, filepath.Join(f.dir, "state", "workspaces", "x")); got != 0 || objects == 0 {
		t.Fatalf("objects counted %d hardlinked and %d copied", got, objects)
	}
	// Room for the files and what is kept free, but not for copied objects.
	keep := uint64(1 << 20)
	f.setDisk(keep*10, keep+files+objects/2)
	c := cfg(map[string]any{"mode": "clone"})
	r, err := f.acquire(owner("same", "x1"), c, "", nil)
	if err != nil {
		t.Fatalf("hardlinked clone refused: %v", err)
	}
	f.provider.sameDisk = func(string, string) bool { return false }
	_, err = f.acquire(owner("other", "x2"), c, "", nil)
	var full *NoSpaceError
	if !errors.As(err, &full) || !errors.Is(err, extension.ErrResourceExhausted) || full.Need != files+objects {
		t.Fatalf("copied clone: %v", err)
	}
	if exists(filepath.Join(f.dir, "state", "workspaces", refFor(ModeClone, owner("other", "x2")))) || exists(f.provider.leasePath(refFor(ModeClone, owner("other", "x2")))) {
		t.Fatal("a refused clone left a workspace or a lease")
	}
	if again, err := f.acquire(owner("same", "x3"), c, "", nil); err != nil || again.Dir != r.Dir {
		t.Fatalf("reuse needs no room: %v", err)
	}
}

func TestValidateSandbox(t *testing.T) {
	for _, tc := range []struct {
		config, mode string
		ok           bool
	}{
		{`{"checkout":"r","mode":"clone"}`, "", true},
		{`{"checkout":"r"}`, "clone", true},
		{`{"checkout":"r","mode":"clone"}`, "work", false},
		{`{"checkout":"r","mode":"work"}`, "", false},
		{`{"checkout":"r"}`, "", false}, // shared by default
		{`{"checkout":"r","mode":"fresh"}`, "", false},
	} {
		err := ValidateSandbox(json.RawMessage(tc.config), tc.mode)
		if tc.ok != (err == nil) || !tc.ok && (!errors.Is(err, ErrSandboxMode) || !strings.Contains(err.Error(), "use mode clone")) {
			t.Errorf("%s requesting %q: %v", tc.config, tc.mode, err)
		}
	}
	if err := ValidateSandbox(json.RawMessage(`{"mode":"clone"}`), ""); !errors.Is(err, ErrConfig) {
		t.Errorf("an invalid config: %v", err)
	}
}

func TestCloneLeavesCredentialsOutOfTheRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://x-access-token:ghs_secret@github.com/o/r.git": "https://github.com/o/r.git",
		"https://github.com/o/r.git":                           "https://github.com/o/r.git",
		"git@github.com:o/r.git":                               "git@github.com:o/r.git",
	} {
		if got := withoutUserinfo(in); got != want {
			t.Errorf("withoutUserinfo(%s) = %s", in, got)
		}
	}
}
