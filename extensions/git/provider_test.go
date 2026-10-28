package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fixture is a bare origin, the scope's main checkout cloned from it and a
// second clone that pushes changes "from elsewhere".
type fixture struct {
	t                    *testing.T
	dir                  string
	origin, root, other  string
	clock                time.Time
	provider             *Provider
	status               map[domain.ID]extension.ResourceOwnerStatus
	mu                   sync.Mutex
	workDone, workFailed map[domain.ID]bool
	// total and free are what the injected disk probe reports; probed lists
	// the paths it was asked about.
	total, free uint64
	probed      []string
}

// setDisk changes what the injected free-space probe reports.
func (f *fixture) setDisk(total, free uint64) {
	f.mu.Lock()
	f.total, f.free = total, free
	f.mu.Unlock()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	f := &fixture{t: t, dir: dir, origin: filepath.Join(dir, "origin.git"), root: filepath.Join(dir, "repo"), other: filepath.Join(dir, "other"),
		clock: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), status: map[domain.ID]extension.ResourceOwnerStatus{},
		total: 1 << 40, free: 1 << 39}
	runGit(t, dir, "init", "-q", "--bare", "-b", "main", f.origin)
	runGit(t, dir, "clone", "-q", f.origin, f.root)
	runGit(t, f.root, "checkout", "-q", "-B", "main")
	write(t, f.root, "a.txt", "v1")
	write(t, f.root, "b.txt", "v1")
	write(t, f.root, "lock.txt", "l1")
	runGit(t, f.root, "add", ".")
	runGit(t, f.root, "commit", "-qm", "one")
	runGit(t, f.root, "push", "-q", "origin", "main")
	runGit(t, dir, "clone", "-q", f.origin, f.other)
	f.provider = f.newProvider()
	return f
}

func (f *fixture) newProvider() *Provider {
	p, err := New(Options{StateDir: filepath.Join(f.dir, "state"), BaseDir: f.dir, Now: func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}, DiskSpace: func(path string) (uint64, uint64, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.probed = append(f.probed, path)
		return f.total, f.free, nil
	}})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

// push commits a change in the other clone and pushes it to origin main.
func (f *fixture) push(name, body string) {
	f.t.Helper()
	runGit(f.t, f.other, "pull", "-q", "--ff-only", "origin", "main")
	write(f.t, f.other, name, body)
	runGit(f.t, f.other, "add", name)
	runGit(f.t, f.other, "commit", "-qm", "change "+name)
	runGit(f.t, f.other, "push", "-q", "origin", "HEAD:main")
}

func cfg(extra map[string]any) domain.Payload {
	m := map[string]any{"checkout": "repo"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return domain.Payload{Schema: "core.config.resource.v1", Data: b}
}

func owner(work, execution string) extension.ResourceOwner {
	return extension.ResourceOwner{ScopeID: "eng", WorkID: domain.ID(work), ExecutionID: domain.ID(execution)}
}

func (f *fixture) acquire(o extension.ResourceOwner, config domain.Payload, mode string, log *strings.Builder) (extension.Resource, error) {
	r := extension.ResourceRequest{Mode: mode}
	if log != nil {
		r.Log = log
	}
	return extension.BindResources(f.provider, o, config).Acquire(context.Background(), r)
}

func (f *fixture) set(execution string, s extension.ResourceOwnerStatus) {
	f.mu.Lock()
	f.status[domain.ID(execution)] = s
	f.mu.Unlock()
}

func (f *fixture) reconcile() extension.ResourceReport {
	f.t.Helper()
	report, err := f.provider.Reconcile(context.Background(), func(_ context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
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

var (
	running   = extension.ResourceOwnerStatus{Known: true}
	succeeded = extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, ExecutionSucceeded: true}
	failed    = extension.ResourceOwnerStatus{Known: true, ExecutionDone: true}
)

func syncOf(t *testing.T, r extension.Resource) workspace {
	t.Helper()
	if r.Data.Schema != WorkspaceSchema {
		t.Fatalf("resource data schema %q", r.Data.Schema)
	}
	var w workspace
	if err := json.Unmarshal(r.Data.Data, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSharedCatchUpPreservesLocalEdits(t *testing.T) {
	f := newFixture(t)
	c := cfg(nil)
	r, err := f.acquire(owner("w1", "x1"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != f.root || r.Kind != "workspace" || r.Ref == "" {
		t.Fatalf("shared resource: %+v", r)
	}
	if w := syncOf(t, r); w.Mode != extension.ResourceShared || w.Sync != (Sync{}) {
		t.Fatalf("up to date: %+v", w)
	}
	f.set("x1", succeeded)
	f.reconcile()

	// An uncommitted change the update doesn't touch stays; the checkout catches up.
	f.push("a.txt", "v2")
	f.push("a.txt", "v3")
	write(t, f.root, "b.txt", "mine")
	var log strings.Builder
	r, err = f.acquire(owner("w2", "x2"), c, extension.ResourceShared, &log)
	if err != nil {
		t.Fatal(err)
	}
	if w := syncOf(t, r); w.Updated != 2 || w.Behind != 0 || w.Dirty != 1 {
		t.Fatalf("dirty elsewhere: %+v", w)
	}
	if read(t, filepath.Join(f.root, "a.txt")) != "v3" || read(t, filepath.Join(f.root, "b.txt")) != "mine" {
		t.Fatal("catch-up lost the update or the local edit")
	}
	if !strings.Contains(log.String(), "fast-forwarded the checkout 2 commit(s)") || !strings.Contains(log.String(), "left untouched") {
		t.Fatalf("log: %s", log.String())
	}
	f.set("x2", succeeded)
	f.reconcile()

	// One it touches stops it, and the run is told why. Nothing is stashed or reset.
	f.push("b.txt", "v2")
	log.Reset()
	r, err = f.acquire(owner("w3", "x3"), c, "", &log)
	if err != nil {
		t.Fatal(err)
	}
	if w := syncOf(t, r); w.Updated != 0 || w.Behind != 1 || !strings.Contains(w.Stale, "uncommitted") {
		t.Fatalf("dirty in the way: %+v", w)
	}
	if read(t, filepath.Join(f.root, "b.txt")) != "mine" {
		t.Fatal("the uncommitted change went")
	}
	if !strings.Contains(log.String(), "running 1 commit(s) behind origin/main: uncommitted changes") {
		t.Fatalf("log: %s", log.String())
	}
	if out := runGit(t, f.root, "stash", "list"); out != "" {
		t.Fatalf("something was stashed: %s", out)
	}
	f.set("x3", succeeded)
	f.reconcile()
	runGit(t, f.root, "checkout", "-q", "--", "b.txt")

	// Another branch is left be.
	runGit(t, f.root, "checkout", "-q", "-b", "feature")
	r, _ = f.acquire(owner("w4", "x4"), c, "", nil)
	if w := syncOf(t, r); w.Behind != 1 || w.Stale != "on feature, not main" {
		t.Fatalf("on a branch: %+v", w)
	}
	f.set("x4", succeeded)
	f.reconcile()
	runGit(t, f.root, "checkout", "-q", "main")

	// Nor is a branch with commits of its own.
	write(t, f.root, "c.txt", "local")
	runGit(t, f.root, "add", "c.txt")
	runGit(t, f.root, "commit", "-qm", "local")
	f.push("a.txt", "v4")
	r, _ = f.acquire(owner("w5", "x5"), c, "", nil)
	if w := syncOf(t, r); w.Behind < 1 || w.Stale != "main has commits origin/main doesn't" {
		t.Fatalf("diverged: %+v", w)
	}
	if runGit(t, f.root, "log", "-1", "--format=%s") != "local" {
		t.Fatal("the local commit was rewritten")
	}
	f.set("x5", succeeded)
	f.reconcile()

	// No such remote: nothing to catch up with.
	r, err = f.acquire(owner("w6", "x6"), cfg(map[string]any{"remote": "upstream"}), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := syncOf(t, r); w.Sync != (Sync{}) {
		t.Fatalf("no remote: %+v", w)
	}
}

func TestSharedConcurrentUsersOnlyCompare(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"base_branch": "main"})
	if _, err := f.acquire(owner("w1", "x1"), c, "", nil); err != nil {
		t.Fatal(err)
	}
	f.push("a.txt", "v2")
	var log strings.Builder
	r, err := f.acquire(owner("w2", "x2"), c, "", &log)
	if err != nil {
		t.Fatal(err)
	}
	if w := syncOf(t, r); w.Updated != 0 || w.Behind != 1 || w.Stale != "another execution is using the checkout" {
		t.Fatalf("second user: %+v", w)
	}
	if read(t, filepath.Join(f.root, "a.txt")) != "v1" {
		t.Fatal("the checkout moved under a running user")
	}
	// Once both finish and are released, the next run catches up.
	f.set("x1", succeeded)
	f.set("x2", failed)
	if report := f.reconcile(); report.Released != 2 {
		t.Fatalf("released %+v", report)
	}
	r, _ = f.acquire(owner("w3", "x3"), c, "", nil)
	if w := syncOf(t, r); w.Updated != 1 {
		t.Fatalf("after release: %+v", w)
	}
	// A shared lease never removes the main checkout.
	f.set("x3", failed)
	f.reconcile()
	if !exists(filepath.Join(f.root, "a.txt")) {
		t.Fatal("the main checkout was removed")
	}
}

func TestFreshAtRemoteHeadIsolatedAndCleanedUp(t *testing.T) {
	f := newFixture(t)
	f.push("a.txt", "v2") // the fresh checkout has it though the main checkout doesn't
	write(t, f.root, ".env", "A=1\n")
	c := cfg(map[string]any{"copy": []string{".env"}, "setup": "echo ran >> setup.log"})
	var log strings.Builder
	r1, err := f.acquire(owner("w1", "x1"), c, extension.ResourceFresh, &log)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if read(t, filepath.Join(r1.Dir, "a.txt")) != "v2" {
		t.Error("fresh checkout is not at the remote head")
	}
	if read(t, filepath.Join(r1.Dir, ".env")) != "A=1\n" {
		t.Error(".env wasn't copied")
	}
	if read(t, filepath.Join(r1.Dir, "setup.log")) != "ran\n" {
		t.Error("setup did not run once")
	}
	if read(t, filepath.Join(f.root, "a.txt")) != "v1" {
		t.Error("the main checkout moved")
	}
	if w := syncOf(t, r1); w.Head == "" || w.Base != "main" || !w.SetupRan || w.Reused {
		t.Errorf("fresh data: %+v", w)
	}
	if b := runGit(t, r1.Dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
		t.Errorf("fresh checkout is on branch %q, want detached", b)
	}

	// Another execution gets its own workspace; the same one reuses its own.
	r2, err := f.acquire(owner("w2", "x2"), c, extension.ResourceFresh, nil)
	if err != nil || r2.Dir == r1.Dir || r2.Ref == r1.Ref {
		t.Fatalf("second fresh: %+v %v", r2, err)
	}
	write(t, r1.Dir, "scratch.txt", "mine")
	again, err := f.acquire(owner("w1", "x1"), c, extension.ResourceFresh, nil)
	if err != nil || again.Ref != r1.Ref || again.Dir != r1.Dir || !syncOf(t, again).Reused {
		t.Fatalf("re-acquire: %+v %v", again, err)
	}
	if read(t, filepath.Join(r1.Dir, "setup.log")) != "ran\n" || !exists(filepath.Join(r1.Dir, "scratch.txt")) {
		t.Error("re-acquire reran setup or reset the workspace")
	}
	if _, err := os.Stat(filepath.Join(r2.Dir, "scratch.txt")); !os.IsNotExist(err) {
		t.Error("fresh workspaces are not isolated")
	}

	// Lookup never creates and refuses another owner.
	if got, err := extension.BindResources(f.provider, owner("w1", "x1"), c).Lookup(context.Background(), r1.Ref); err != nil || got.Dir != r1.Dir {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	if _, err := extension.BindResources(f.provider, owner("w2", "x2"), c).Lookup(context.Background(), r1.Ref); !errors.Is(err, extension.ErrResourceNotFound) {
		t.Fatalf("lookup by another owner: %v", err)
	}

	// Running owners keep their workspaces.
	f.set("x1", running)
	if report := f.reconcile(); report.Active != 2 || !exists(r1.Dir) {
		t.Fatalf("reconcile while running: %+v", report)
	}
	// Success removes; failure retains for a day, then removes.
	f.set("x1", succeeded)
	f.set("x2", failed)
	report := f.reconcile()
	if report.Removed != 1 || report.Retained != 1 || exists(r1.Dir) || !exists(r2.Dir) {
		t.Fatalf("after finish: %+v", report)
	}
	if out := runGit(t, f.root, "worktree", "list"); strings.Contains(out, r1.Ref) {
		t.Fatalf("worktree still listed: %s", out)
	}
	f.advance(23 * time.Hour)
	if f.reconcile(); !exists(r2.Dir) {
		t.Fatal("failed run's workspace removed before its retention")
	}
	f.advance(2 * time.Hour)
	if report = f.reconcile(); report.Removed != 1 || exists(r2.Dir) {
		t.Fatalf("after retention: %+v", report)
	}
	if out := runGit(t, f.root, "worktree", "list"); strings.Contains(out, "fresh-") {
		t.Fatalf("worktrees remain: %s", out)
	}
}

func TestFreshConcurrentAcquisitions(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "fresh"})
	var wg sync.WaitGroup
	results := make([]extension.Resource, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Two goroutines per execution: the same lease must come back.
			results[i], errs[i] = f.acquire(owner("w", fmt.Sprintf("x%d", i/2)), c, "", nil)
		}(i)
	}
	wg.Wait()
	dirs := map[string]bool{}
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("acquire %d: %v", i, errs[i])
		}
		if i%2 == 1 && r.Dir != results[i-1].Dir {
			t.Fatalf("same execution got two workspaces: %s %s", r.Dir, results[i-1].Dir)
		}
		dirs[r.Dir] = true
	}
	if len(dirs) != 4 {
		t.Fatalf("got %d workspaces for 4 executions", len(dirs))
	}
	if n := strings.Count(runGit(t, f.root, "worktree", "list"), "fresh-"); n != 4 {
		t.Fatalf("%d worktrees registered", n)
	}
}

func TestWorkBranchKeepsEditsAndCommits(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "work", "branch_template": "agent/issue-{work}", "copy": []string{".env"}})
	write(t, f.root, ".env", "A=1\n")
	r, err := f.acquire(owner("PROJ 1", "x1"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := syncOf(t, r)
	if w.Branch != "agent/issue-PROJ-1" || runGit(t, r.Dir, "rev-parse", "--abbrev-ref", "HEAD") != "agent/issue-PROJ-1" {
		t.Fatalf("work branch: %+v", w)
	}
	// A dirty branch: a local commit and an uncommitted edit from the first attempt.
	write(t, r.Dir, "work.txt", "committed")
	runGit(t, r.Dir, "add", "work.txt")
	runGit(t, r.Dir, "commit", "-qm", "work")
	write(t, r.Dir, "a.txt", "editing")
	f.set("x1", failed) // the attempt failed; the Work continues
	f.reconcile()
	if !exists(r.Dir) {
		t.Fatal("a work workspace went with its failed attempt")
	}
	f.push("a.txt", "v2")
	write(t, f.root, ".env", "A=1\nB=2\n")
	var log strings.Builder
	again, err := f.acquire(owner("PROJ 1", "x2"), c, "", &log)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref != r.Ref || again.Dir != r.Dir || !syncOf(t, again).Reused || syncOf(t, again).Dirty != 1 {
		t.Fatalf("reuse: %+v %+v", again, syncOf(t, again))
	}
	if read(t, filepath.Join(r.Dir, "a.txt")) != "editing" || runGit(t, r.Dir, "log", "-1", "--format=%s") != "work" {
		t.Fatal("reuse reset the branch or dropped the edit")
	}
	if read(t, filepath.Join(r.Dir, ".env")) != "A=1\nB=2\n" {
		t.Fatal("copied files were not refreshed on reuse")
	}
	if !strings.Contains(log.String(), "uncommitted path(s) from earlier runs; they were kept") {
		t.Fatalf("log: %s", log.String())
	}
	// Another Work cannot look it up; a later attempt of the same Work can.
	if _, err := extension.BindResources(f.provider, owner("PROJ 2", "x9"), c).Lookup(context.Background(), r.Ref); !errors.Is(err, extension.ErrResourceNotFound) {
		t.Fatalf("lookup by another Work: %v", err)
	}
	if _, err := extension.BindResources(f.provider, owner("PROJ 1", "x3"), c).Lookup(context.Background(), r.Ref); err != nil {
		t.Fatalf("lookup by the Work: %v", err)
	}
	// Completion of the Work removes the worktree and keeps the branch.
	f.set("x3", extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, ExecutionSucceeded: true, WorkDone: true, WorkSucceeded: true})
	if report := f.reconcile(); report.Removed != 1 || exists(r.Dir) {
		t.Fatalf("after Work completed: %+v", report)
	}
	if !strings.Contains(runGit(t, f.root, "branch", "--list", "agent/issue-PROJ-1"), "agent/issue-PROJ-1") {
		t.Fatal("the branch went with the worktree")
	}

	// A failed Work's workspace is retained, and a branch checked out elsewhere is refused clearly.
	r3, err := f.acquire(owner("PROJ 3", "x4"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.set("x4", extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, WorkDone: true})
	if report := f.reconcile(); report.Retained != 1 || !exists(r3.Dir) {
		t.Fatalf("after Work failed: %+v", report)
	}
	runGit(t, f.root, "checkout", "-q", "-b", "agent/issue-busy")
	_, err = f.acquire(owner("busy", "x5"), c, "", nil)
	if err == nil || !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("branch checked out in the main checkout: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dir, "state", "leases")); len(entries) != 1 {
		t.Fatalf("failed creation left a lease: %d leases", len(entries))
	}
}

// A Work started again on the branch of a cancelled one (an issue closed and
// reopened) takes over the retained workspace with what was left in it; a
// branch still held by a live Work is refused naming that Work.
func TestWorkTakesOverRetainedWorkspaceOnItsBranch(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "work", "branch_template": "agent/issue-7"})
	a, err := f.acquire(owner("A", "a1"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, a.Dir, "work.txt", "committed")
	runGit(t, a.Dir, "add", "work.txt")
	runGit(t, a.Dir, "commit", "-qm", "work")
	write(t, a.Dir, "a.txt", "editing")
	f.set("a1", extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, WorkDone: true})
	if report := f.reconcile(); report.Retained != 1 {
		t.Fatalf("after Work A was cancelled: %+v", report)
	}
	var log strings.Builder
	b, err := f.acquire(owner("B", "b1"), c, "", &log)
	if err != nil {
		t.Fatalf("Work B on the retained branch: %v (%s)", err, log.String())
	}
	if b.Ref == a.Ref || b.Dir != filepath.Join(f.dir, "state", "workspaces", b.Ref) || exists(a.Dir) {
		t.Fatalf("takeover: %+v from %+v", b, a)
	}
	if w := syncOf(t, b); !w.Reused || w.Dirty != 1 || w.Branch != "agent/issue-7" {
		t.Fatalf("takeover workspace: %+v", w)
	}
	if read(t, filepath.Join(b.Dir, "a.txt")) != "editing" || runGit(t, b.Dir, "log", "-1", "--format=%s") != "work" {
		t.Fatal("the takeover lost Work A's edit or commit")
	}
	if !strings.Contains(log.String(), "took over the retained workspace of work A") {
		t.Fatalf("log: %s", log.String())
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dir, "state", "leases")); len(entries) != 1 {
		t.Fatalf("Work A's lease outlived the takeover: %d leases", len(entries))
	}
	// A's retention running out takes nothing from B.
	f.advance(48 * time.Hour)
	f.reconcile()
	if !exists(b.Dir) {
		t.Fatal("Work B's workspace went when Work A's retention ran out")
	}
	// While B is live, another Work on the branch is refused naming B.
	if _, err = f.acquire(owner("C", "c1"), c, "", nil); err == nil || !strings.Contains(err.Error(), "work B") {
		t.Fatalf("branch held by a live Work: %v", err)
	}
	if read(t, filepath.Join(b.Dir, "a.txt")) != "editing" {
		t.Fatal("the refused acquisition touched Work B's workspace")
	}
}

func TestWorkTracksBranchFromConfiguredRemote(t *testing.T) {
	dir := t.TempDir()
	upstream, root := filepath.Join(dir, "upstream.git"), filepath.Join(dir, "repo")
	runGit(t, dir, "init", "-q", "--bare", "-b", "main", upstream)
	runGit(t, dir, "clone", "-q", "-o", "upstream", upstream, root)
	runGit(t, root, "checkout", "-q", "-B", "main")
	write(t, root, "a.txt", "v1")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "one")
	runGit(t, root, "push", "-q", "upstream", "HEAD:main", "HEAD:refs/heads/agent/issue-2")
	p, err := New(Options{StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"checkout": root, "remote": "upstream", "mode": "work"})
	params, _ := json.Marshal(map[string]any{"branch": "agent/issue-2"})
	var log strings.Builder
	r, err := extension.BindResources(p, owner("2", "x1"), domain.Payload{Data: b}).Acquire(context.Background(), extension.ResourceRequest{Params: domain.Payload{Schema: ParamsSchema, Data: params}, Log: &log})
	if err != nil {
		t.Fatal(err, log.String())
	}
	if strings.Contains(log.String(), "fetch failed") {
		t.Fatalf("fetched from origin: %s", log.String())
	}
	if got := runGit(t, r.Dir, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "upstream/agent/issue-2" {
		t.Fatalf("tracks %q", got)
	}
}

func TestSetupWhenAndSetupFailure(t *testing.T) {
	f := newFixture(t)
	// Skipped when none of its watched files exist.
	c := cfg(map[string]any{"mode": "work", "setup": "exit 1", "setup_when": []string{"pnpm-lock.yaml"}})
	if _, err := f.acquire(owner("w0", "x0"), c, "", nil); err != nil {
		t.Fatalf("setup ran with none of its watched files: %v", err)
	}
	// Runs once while the watched file is unchanged, again when it changes.
	c = cfg(map[string]any{"mode": "work", "setup": "echo ran >> setup.log", "setup_when": []string{"lock.txt"}})
	r, err := f.acquire(owner("w1", "x1"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.acquire(owner("w1", "x2"), c, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(read(t, filepath.Join(r.Dir, "setup.log")), "ran"); got != 1 {
		t.Fatalf("setup ran %d times; want once while lock.txt is unchanged", got)
	}
	write(t, r.Dir, "lock.txt", "l2")
	if _, err = f.acquire(owner("w1", "x3"), c, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(read(t, filepath.Join(r.Dir, "setup.log")), "ran"); got != 2 {
		t.Fatalf("setup ran %d times; want again after lock.txt changed", got)
	}

	// A failing setup returns the workspace for inspection and retains it even
	// after its execution "succeeds".
	var log strings.Builder
	bad := cfg(map[string]any{"setup": "echo broken; exit 3"})
	fr, err := f.acquire(owner("w2", "x4"), bad, extension.ResourceFresh, &log)
	if !errors.Is(err, extension.ErrResourceSetup) || fr.Dir == "" || !exists(fr.Dir) {
		t.Fatalf("setup failure: %+v %v", fr, err)
	}
	if !strings.Contains(log.String(), "broken") || !strings.Contains(log.String(), "kept for inspection") {
		t.Fatalf("setup log: %s", log.String())
	}
	f.set("x4", succeeded)
	if report := f.reconcile(); report.Retained != 1 || !exists(fr.Dir) {
		t.Fatalf("setup-failed workspace: %+v", report)
	}

	// A setup that outlives its timeout is stopped with its process group.
	slow := cfg(map[string]any{"setup": "sleep 30 & echo $! > child.pid; wait", "setup_timeout_seconds": 1})
	start := time.Now()
	sr, err := f.acquire(owner("w3", "x5"), slow, extension.ResourceFresh, nil)
	if !errors.Is(err, extension.ErrResourceSetup) || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("setup timeout: %v after %s", err, time.Since(start))
	}
	time.Sleep(100 * time.Millisecond)
	if pid := strings.TrimSpace(read(t, filepath.Join(sr.Dir, "child.pid"))); pid != "" {
		if out, _ := exec.Command("kill", "-0", pid).CombinedOutput(); len(out) == 0 {
			t.Fatalf("setup child %s survived the timeout", pid)
		}
	}
}

func TestEnvironmentLayer(t *testing.T) {
	f := newFixture(t)
	write(t, f.root, ".env", "# c\nA=1\nexport B=\"two\"\nC='3'\nbad\nOS_TOKEN=stolen\nGIT_CONFIG_COUNT=9\n")
	r, err := f.acquire(owner("w", "x1"), cfg(map[string]any{"dotenv": true}), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two", "C": "3", "GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false", "GIT_CONFIG_KEY_1": "tag.gpgsign", "GIT_CONFIG_VALUE_1": "false"}
	if len(r.Env) != len(want) {
		t.Fatalf("env %v", r.Env)
	}
	for k, v := range want {
		if r.Env[k] != v {
			t.Fatalf("env %s=%q, want %q (%v)", k, r.Env[k], v, r.Env)
		}
	}
	// Signing kept when asked for; no .env without the opt-in.
	r, _ = f.acquire(owner("w", "x2"), cfg(map[string]any{"sign": true}), "", nil)
	if len(r.Env) != 0 {
		t.Fatalf("env with sign and no dotenv: %v", r.Env)
	}
	// The ledger never stores environment values.
	entries, _ := filepath.Glob(filepath.Join(f.dir, "state", "leases", "*.json"))
	for _, e := range entries {
		if strings.Contains(read(t, e), "two") {
			t.Fatal("an environment value was persisted in the ledger")
		}
	}
}

func TestRestartAndFailedCleanupAreRecovered(t *testing.T) {
	f := newFixture(t)
	c := cfg(map[string]any{"mode": "fresh"})
	r, err := f.acquire(owner("w1", "x1"), c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A crash between the ledger write and worktree creation: the lease is
	// recorded as acquiring with a partial directory and nothing registered.
	crashed := owner("w2", "x2")
	ref := refFor(extension.ResourceFresh, crashed)
	partial := filepath.Join(f.dir, "state", "workspaces", ref)
	write(t, partial, "junk", "x")
	l := lease{Ref: ref, Mode: extension.ResourceFresh, State: stateAcquiring, Owner: crashed, Checkout: f.root, Path: partial, Remote: "origin", RetainSeconds: 60}
	if err = f.provider.save(&l); err != nil {
		t.Fatal(err)
	}

	// A new provider instance (daemon restart) finds both leases.
	f.provider = f.newProvider()
	if got, err := extension.BindResources(f.provider, owner("w1", "x1"), c).Lookup(context.Background(), r.Ref); err != nil || got.Dir != r.Dir {
		t.Fatalf("lookup after restart: %v", err)
	}
	f.set("x2", failed) // recovery found the launch was never acknowledged
	if report := f.reconcile(); report.Retained != 1 || report.Active != 1 {
		t.Fatalf("after restart: %+v", report)
	}
	f.advance(time.Minute)
	f.reconcile()
	if exists(partial) {
		t.Fatal("the crashed acquisition's partial directory remained")
	}
	// The same execution acquiring again after a crash reuses the lease.
	again, err := f.acquire(owner("w1", "x1"), c, "", nil)
	if err != nil || again.Dir != r.Dir {
		t.Fatalf("re-acquire after restart: %v", err)
	}

	// Cleanup that fails is recorded, reported and retried.
	locked := filepath.Join(r.Dir, "locked")
	write(t, locked, "f", "x")
	if err = os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o700)
	f.set("x1", succeeded)
	report, rerr := f.provider.Reconcile(context.Background(), func(context.Context, extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
		return succeeded, nil
	})
	if rerr == nil || len(report.Attention) != 1 || report.Attention[0].Code != "cleanup_failed" {
		t.Fatalf("failed cleanup: %+v %v", report, rerr)
	}
	_ = os.Chmod(locked, 0o700)
	f.provider = f.newProvider()
	if report = f.reconcile(); report.Removed != 1 || exists(r.Dir) {
		t.Fatalf("cleanup retry: %+v", report)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dir, "state", "leases")); len(entries) != 0 {
		t.Fatalf("%d leases left", len(entries))
	}
}

func TestUnknownOwnerIsRetainedNotDeleted(t *testing.T) {
	f := newFixture(t)
	r, err := f.acquire(owner("w", "x1"), cfg(map[string]any{"mode": "fresh"}), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.set("x1", extension.ResourceOwnerStatus{})
	if report := f.reconcile(); report.Retained != 1 || !exists(r.Dir) {
		t.Fatalf("unknown owner: %+v", report)
	}
}

func TestConfigValidation(t *testing.T) {
	good, err := ParseConfig([]byte(`{"checkout":"repo"}`))
	if err != nil || good.Mode != "shared" || good.Remote != "origin" || good.RetainFailedSeconds != 86400 || good.SetupTimeoutSeconds != 1800 || good.BranchTemplate != "agent/work-{work}" ||
		good.DiskFullPercent != 90 || good.DiskReserveBytes != 10<<30 {
		t.Fatalf("defaults: %+v %v", good, err)
	}
	for _, bad := range []string{
		``, `{}`, `{"checkout":"r","extra":1}`, `{"checkout":"r"} {}`, `{"checkout":"r","mode":"clone"}`,
		`{"checkout":"r","remote":"../x"}`, `{"checkout":"r","base_branch":"-x"}`, `{"checkout":"r","base_branch":"a..b"}`,
		`{"checkout":"r","branch_template":"agent/{key}"}`, `{"checkout":"r","copy":["/etc/passwd"]}`, `{"checkout":"r","copy":["../x"]}`,
		`{"checkout":"r","copy":[".git/config"]}`, `{"checkout":"r","setup_when":["a/../../b"]}`, `{"checkout":"r","retain_failed_seconds":-1}`,
		`{"checkout":"r","disk_full_percent":-1}`, `{"checkout":"r","disk_full_percent":100.5}`, `{"checkout":"r","disk_reserve_bytes":-1}`,
	} {
		if _, err := ParseConfig([]byte(bad)); !errors.Is(err, ErrConfig) {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := parseParams([]byte(`{"branch":"x y"}`)); err == nil {
		t.Error("accepted an invalid branch param")
	}
	f := newFixture(t)
	if _, err := f.acquire(owner("w", "x"), cfg(nil), "clone", nil); !errors.Is(err, extension.ErrUnsupported) {
		t.Errorf("unknown mode: %v", err)
	}
	if _, err := f.acquire(owner("w", "x"), cfg(map[string]any{"checkout": "missing"}), "", nil); !errors.Is(err, extension.ErrInvalid) {
		t.Errorf("missing checkout: %v", err)
	}
	if _, err := f.acquire(extension.ResourceOwner{ScopeID: "eng"}, cfg(nil), "", nil); !errors.Is(err, extension.ErrInvalid) {
		t.Errorf("incomplete owner: %v", err)
	}
}

func TestCopySkipsSymlinkEscapes(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(f.root, "envlink")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.root, "add", "linked", "envlink")
	runGit(t, f.root, "commit", "-qm", "links")
	runGit(t, f.root, "push", "-q", "origin", "main")
	runGit(t, f.root, "rm", "-q", "--cached", "envlink")
	os.Remove(filepath.Join(f.root, "envlink"))
	write(t, f.root, "envlink", "SECRET=1")
	write(t, filepath.Join(f.dir, "realdir"), ".env", "A=1")
	os.Remove(filepath.Join(f.root, "linked"))
	write(t, f.root, "linked/.env", "A=1")
	var log strings.Builder
	_, err := f.acquire(owner("w", "x"), cfg(map[string]any{"mode": "fresh", "copy": []string{"envlink", "linked/.env"}}), "", &log)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if exists(filepath.Join(outside, "target")) || exists(filepath.Join(outside, ".env")) {
		t.Fatalf("a copy escaped the workspace: %s", log.String())
	}
	if strings.Count(log.String(), "not copying") != 2 {
		t.Fatalf("log: %s", log.String())
	}
}

func TestRedactsCredentialURLs(t *testing.T) {
	got := redact("fatal: unable to access 'https://x-access-token:ghs_secret@github.com/o/r.git/': 403")
	if strings.Contains(got, "ghs_secret") || !strings.Contains(got, "https://[redacted]@github.com") {
		t.Fatalf("redact: %s", got)
	}
}
