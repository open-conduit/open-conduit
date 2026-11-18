package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// fakeSite runs a site's commands on this machine, with an environment of
// only what the command sets, a PATH and HOME, as a provider's SiteRun
// does. down makes it unreachable.
type fakeSite struct {
	id   string
	home string
	mu   sync.Mutex
	down bool
	// hang makes every command wait until it is cancelled.
	hang bool
	argv [][]string
	envs [][]string
}

func (s *fakeSite) ID() string { return s.id }

func (s *fakeSite) Run(ctx context.Context, cmd extension.SiteCommand) (extension.SiteResult, error) {
	s.mu.Lock()
	down, hang := s.down, s.hang
	s.argv, s.envs = append(s.argv, cmd.Argv), append(s.envs, cmd.Env)
	s.mu.Unlock()
	if hang {
		<-ctx.Done()
		return extension.SiteResult{}, errors.Join(extension.ErrSiteUnavailable, ctx.Err())
	}
	if down {
		return extension.SiteResult{}, extension.ErrSiteUnavailable
	}
	c := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	c.Dir = cmd.Dir
	c.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.home,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, cmd.Env...)
	var stdout, stderr bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(cmd.Stdin), &stdout, &stderr
	err := c.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return extension.SiteResult{}, errors.Join(extension.ErrSiteUnavailable, err)
	}
	return extension.SiteResult{ExitCode: c.ProcessState.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func (s *fakeSite) runs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.argv)
}

func (s *fakeSite) setDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
}

// siteFixture is the fixture's origin with a provider that has no git of
// its own (any host-side git fails), so everything it does at the site is
// done through the site, and no checkout.
type siteFixture struct {
	*fixture
	sites map[string]*fakeSite
}

func newSiteFixture(t *testing.T) *siteFixture {
	f := newFixture(t)
	p, err := New(Options{StateDir: filepath.Join(f.dir, "state"), Git: filepath.Join(f.dir, "no-git-on-the-host"), Now: func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.provider = p
	sf := &siteFixture{fixture: f, sites: map[string]*fakeSite{}}
	for _, id := range []string{"ssh/one", "ssh/two"} {
		sf.sites[id] = &fakeSite{id: id, home: t.TempDir()}
	}
	p.UseSites(func(_ context.Context, id string) (extension.Site, error) {
		if s, ok := sf.sites[id]; ok {
			return s, nil
		}
		return nil, extension.ErrSiteUnavailable
	})
	return sf
}

func repoCfg(origin string, extra map[string]any) domain.Payload {
	m := map[string]any{"repository": "file://" + origin}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return domain.Payload{Schema: "core.config.resource.v1", Data: b}
}

func (f *siteFixture) acquireAt(site string, o extension.ResourceOwner, config domain.Payload, req extension.ResourceRequest) (extension.Resource, error) {
	var s extension.Site
	if site != "" {
		s = f.sites[site]
	}
	return extension.BindResourcesAt(f.provider, o, config, nil, s).Acquire(context.Background(), req)
}

func TestRepositoryConfig(t *testing.T) {
	for _, tc := range []struct {
		config string
		ok     bool
	}{
		{`{"repository":"https://github.com/o/r"}`, true},
		{`{"repository":"git@github.com:o/r.git"}`, true},
		{`{"repository":"ssh://git@github.com/o/r.git","mode":"clone","setup":"make"}`, true},
		{`{"repository":"https://x-access-token:secret@github.com/o/r"}`, false},
		{`{"repository":"ssh://u:secret@host/r"}`, false},
		{`{"repository":"-uhelper"}`, false},
		{`{"repository":"https://github.com/o/r","checkout":"/srv/r"}`, false},
		{`{"repository":"https://github.com/o/r","mode":"work"}`, false},
		{`{"repository":"https://github.com/o/r","copy":[".env"]}`, false},
		{`{"repository":"https://github.com/o/r","dotenv":true}`, false},
		{`{}`, false},
	} {
		if err := ValidateConfig(json.RawMessage(tc.config)); (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok %v", tc.config, err, tc.ok)
		}
	}
	raw := json.RawMessage(`{"repository":"https://github.com/o/r","setup":"make deps"}`)
	if ModeDefault(raw) != ModeClone || !NeedsSite(raw) || NeedsSite(json.RawMessage(`{"checkout":"/srv/r","mode":"clone"}`)) {
		t.Error("a repository config defaults to clone and needs a site; a checkout doesn't")
	}
	// Setup at the site runs inside the boundary, so a sandboxed binding
	// may have it; a checkout's still may not.
	if err := ValidateSandbox(raw, ""); err != nil {
		t.Errorf("repository with setup refused for a sandbox: %v", err)
	}
	if err := ValidateSandbox(json.RawMessage(`{"checkout":"/srv/r","mode":"clone","setup":"make"}`), ""); !errors.Is(err, ErrSandboxSetup) {
		t.Errorf("checkout with setup: %v, want ErrSandboxSetup", err)
	}
}

// A repository config's workspace is a clone made at the site, reused
// there by the Work's next attempt with its edits and commits, and never
// made on the host: the provider has no git there.
func TestCloneAtSite(t *testing.T) {
	f := newSiteFixture(t)
	runGit(t, f.root, "checkout", "-q", "-b", "agent/work-W1")
	write(t, f.root, "branch.txt", "remote branch")
	runGit(t, f.root, "add", "branch.txt")
	runGit(t, f.root, "commit", "-qm", "on the branch")
	runGit(t, f.root, "push", "-q", "origin", "agent/work-W1")
	c := repoCfg(f.origin, nil)
	var log strings.Builder
	req := extension.ResourceRequest{Log: &log, Sandboxed: true, Files: []string{"branch.txt", "missing.md", "../escape"}}

	if _, err := f.acquireAt("", owner("W1", "x1"), c, req); !errors.Is(err, extension.ErrUnsupported) {
		t.Fatalf("a repository acquired with no site: %v, want unsupported", err)
	}
	r, err := f.acquireAt("ssh/one", owner("W1", "x1"), c, req)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	w := syncOf(t, r)
	if r.Site != "ssh/one" || w.Mode != ModeClone || w.Branch != "agent/work-W1" || w.Reused || w.Head == "" {
		t.Fatalf("resource %+v, workspace %+v", r, w)
	}
	if r.Dir != filepath.Join(f.dir, "state", "workspaces", r.Ref) || r.Summary != "clone on branch agent/work-W1 at ssh/one" {
		t.Fatalf("dir %s, summary %q", r.Dir, r.Summary)
	}
	if got := r.Files; len(got) != 1 || got["branch.txt"] != "remote branch" {
		t.Fatalf("files read at the site: %v", got)
	}
	if r.Sandbox == nil || len(r.Sandbox.Write) != 1 || r.Sandbox.Write[0] != r.Dir || len(r.Sandbox.ReadOnly) != 3 {
		t.Fatalf("sandbox needs %+v", r.Sandbox)
	}
	// It follows the remote's branch of the Work's name, its remote is the
	// repository and its hooks and info directories exist for the sandbox.
	if runGit(t, r.Dir, "rev-parse", "--abbrev-ref", "@{upstream}") != "origin/agent/work-W1" || runGit(t, r.Dir, "remote", "get-url", "origin") != "file://"+f.origin {
		t.Fatal("the clone doesn't track the remote branch")
	}
	for _, d := range []string{"hooks", "info"} {
		if !exists(filepath.Join(r.Dir, ".git", d)) {
			t.Fatalf(".git/%s missing", d)
		}
	}
	if strings.Contains(runGit(t, r.Dir, "branch", "--list"), " main") {
		t.Fatal("the clone kept the remote's default branch")
	}

	// An edit and a commit stay for the next attempt at the same site.
	write(t, r.Dir, "work.txt", "committed")
	runGit(t, r.Dir, "add", "work.txt")
	runGit(t, r.Dir, "commit", "-qm", "work")
	write(t, r.Dir, "a.txt", "editing")
	log.Reset()
	again, err := f.acquireAt("ssh/one", owner("W1", "x2"), c, extension.ResourceRequest{Log: &log, Sandboxed: true})
	if err != nil {
		t.Fatal(err)
	}
	if aw := syncOf(t, again); again.Ref != r.Ref || !aw.Reused || aw.Dirty != 1 || !strings.Contains(log.String(), "reusing") {
		t.Fatalf("reuse: %+v %s", aw, log.String())
	}
	if read(t, filepath.Join(r.Dir, "a.txt")) != "editing" || runGit(t, r.Dir, "log", "-1", "--format=%s") != "work" {
		t.Fatal("reuse reset the clone")
	}
	// Lookup names the site; the launch placed there checks it.
	got, err := extension.BindResources(f.provider, owner("W1", "x3"), c).Lookup(context.Background(), r.Ref)
	if err != nil || got.Site != "ssh/one" || got.Dir != r.Dir {
		t.Fatalf("lookup: %+v %v", got, err)
	}
}

// A new branch starts at the remote's HEAD and is set to push to its own
// name; setup runs at the site once per change of its inputs.
func TestSetupAtSite(t *testing.T) {
	f := newSiteFixture(t)
	write(t, f.root, "deps.lock", "v1")
	runGit(t, f.root, "add", "deps.lock")
	runGit(t, f.root, "commit", "-qm", "lock")
	runGit(t, f.root, "push", "-q", "origin", "main")
	c := repoCfg(f.origin, map[string]any{"setup": `echo ran >> setup.log; test "$GIT_CONFIG_VALUE_0" = false`, "setup_when": []string{"deps.lock"}})
	var log strings.Builder
	r, err := f.acquireAt("ssh/one", owner("W2", "x1"), c, extension.ResourceRequest{Log: &log, Sandboxed: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if w := syncOf(t, r); !w.SetupRan || w.Base != "main" || read(t, filepath.Join(r.Dir, "setup.log")) != "ran\n" {
		t.Fatalf("setup: %+v %s", w, log.String())
	}
	if runGit(t, r.Dir, "config", "branch.agent/work-W2.merge") != "refs/heads/agent/work-W2" {
		t.Fatal("a new branch has no upstream of its own name")
	}
	r2, err := f.acquireAt("ssh/one", owner("W2", "x2"), c, extension.ResourceRequest{Sandboxed: true})
	if err != nil || syncOf(t, r2).SetupRan {
		t.Fatalf("setup ran again with nothing changed: %v", err)
	}
	write(t, r.Dir, "deps.lock", "v2")
	r3, err := f.acquireAt("ssh/one", owner("W2", "x3"), c, extension.ResourceRequest{Sandboxed: true})
	if err != nil || !syncOf(t, r3).SetupRan || read(t, filepath.Join(r.Dir, "setup.log")) != "ran\nran\n" {
		t.Fatalf("setup didn't run after its input changed: %v", err)
	}

	failing := repoCfg(f.origin, map[string]any{"setup": "exit 3"})
	if _, err := f.acquireAt("ssh/one", owner("W3", "x1"), failing, extension.ResourceRequest{Sandboxed: true}); !errors.Is(err, extension.ErrResourceSetup) {
		t.Fatalf("failing setup: %v, want ErrResourceSetup", err)
	}
}

// When the Work completes, its workspace at the site goes unless it holds
// what no remote has: then it is kept for retain_failed_seconds as
// unpushed or uncommitted, and removed after. An unreachable site is
// retried.
func TestReconcileAtSite(t *testing.T) {
	f := newSiteFixture(t)
	c := repoCfg(f.origin, nil)
	done := extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, ExecutionSucceeded: true, WorkDone: true, WorkSucceeded: true}

	clean, err := f.acquireAt("ssh/one", owner("W1", "x1"), c, extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	unpushed, err := f.acquireAt("ssh/one", owner("W2", "y1"), c, extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	write(t, unpushed.Dir, "work.txt", "x")
	runGit(t, unpushed.Dir, "add", "work.txt")
	runGit(t, unpushed.Dir, "commit", "-qm", "unpushed")
	f.set("x1", done)
	f.set("y1", done)

	f.sites["ssh/one"].setDown(true)
	if _, err := f.provider.Reconcile(context.Background(), f.status1); err == nil {
		t.Fatal("reconcile with the site down reported no error")
	}
	if !exists(clean.Dir) || !exists(unpushed.Dir) {
		t.Fatal("a workspace went while its site was down")
	}
	// While it backs off, the down site isn't tried again, so it holds up
	// no other lease.
	tries := f.sites["ssh/one"].runs()
	if report := f.reconcile(); f.sites["ssh/one"].runs() != tries || len(report.Attention) != 2 {
		t.Fatalf("the site was tried again within its backoff: %+v", report)
	}
	f.sites["ssh/one"].setDown(false)
	f.advance(2 * time.Minute)
	report := f.reconcile()
	if exists(clean.Dir) || !exists(unpushed.Dir) || report.Removed != 1 || report.Retained != 1 {
		t.Fatalf("report %+v: clean %v, unpushed %v", report, exists(clean.Dir), exists(unpushed.Dir))
	}
	if a := report.Attention; len(a) != 1 || a[0].Code != keptUnpushed {
		t.Fatalf("attention %+v", a)
	}
	f.advance(25 * time.Hour)
	if report := f.reconcile(); exists(unpushed.Dir) || report.Removed != 1 {
		t.Fatalf("retention over: %+v, still there %v", report, exists(unpushed.Dir))
	}
	if all, _ := f.provider.leases(); len(all) != 0 {
		t.Fatalf("leases left: %+v", all)
	}
}

// A Work placed at another site gets a new clone there. The one it had
// is removed when it holds nothing a remote lacks; otherwise it keeps a
// lease of its own, which Reconcile reports and removes as any workspace
// at a site, and which the Work takes back if it returns there first.
func TestWorkMovesSite(t *testing.T) {
	f := newSiteFixture(t)
	// The two sites share this machine's disk, so the first site's clone
	// is moved aside while the second makes its own at the same path, and
	// swapped back when the first is asked again.
	c := repoCfg(f.origin, nil)
	r, err := f.acquireAt("ssh/one", owner("W1", "x1"), c, extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	write(t, r.Dir, "edit.txt", "x")
	aside := r.Dir + ".one"
	var log strings.Builder
	f.sites["ssh/one"].setDown(true)
	moved, err := f.acquireAtAside("ssh/two", owner("W1", "x2"), c, &log, r.Dir, aside)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Site != "ssh/two" || moved.Ref != r.Ref || syncOf(t, moved).Reused || !strings.Contains(log.String(), "couldn't be reached and is left there") {
		t.Fatalf("moved: %+v\n%s", moved, log.String())
	}
	left, ok, err := f.provider.load(leftRef(r.Ref, "ssh/one"))
	if err != nil || !ok || left.Site != "ssh/one" || left.State != stateCleanupFailed || left.Path != r.Dir {
		t.Fatalf("the left workspace's lease: %+v %v %v", left, ok, err)
	}
	// Once its site answers, Reconcile finds the edit and retains it.
	f.sites["ssh/one"].setDown(false)
	f.advance(2 * time.Minute)
	swap(t, r.Dir, aside)
	report := f.reconcile()
	if a := report.Attention; report.Retained != 1 || len(a) != 1 || a[0].Ref != left.Ref || a[0].Code != keptUncommitted {
		t.Fatalf("report %+v", report)
	}
	// Back at the first site, the Work takes its clone back with the edit.
	log.Reset()
	back, err := f.acquireAt("ssh/one", owner("W1", "x3"), c, extension.ResourceRequest{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if back.Site != "ssh/one" || !syncOf(t, back).Reused || read(t, filepath.Join(r.Dir, "edit.txt")) != "x" || !strings.Contains(log.String(), "took back") {
		t.Fatalf("back: %+v\n%s", back, log.String())
	}
	if _, ok, _ := f.provider.load(leftRef(r.Ref, "ssh/one")); ok {
		t.Fatal("the left lease outlived being taken back")
	}
}

// acquireAtAside acquires at site with the clone at path moved to aside
// for the call and its own moved back after: two sites on one disk.
func (f *siteFixture) acquireAtAside(site string, o extension.ResourceOwner, config domain.Payload, log *strings.Builder, path, aside string) (extension.Resource, error) {
	if err := os.Rename(path, aside); err != nil {
		f.t.Fatal(err)
	}
	return f.acquireAt(site, o, config, extension.ResourceRequest{Log: log})
}

// swap exchanges the directories at a and b.
func swap(t *testing.T, a, b string) {
	t.Helper()
	tmp := a + ".swap"
	for _, m := range [][2]string{{a, tmp}, {b, a}, {tmp, b}} {
		if err := os.Rename(m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *siteFixture) status1(_ context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[o.ExecutionID], nil
}

// A site that hangs costs Reconcile one bounded wait, then backs off, and
// holds up no other lease.
func TestReconcileAtAHangingSite(t *testing.T) {
	f := newSiteFixture(t)
	defer func(d time.Duration) { siteReconcileTimeout = d }(siteReconcileTimeout)
	siteReconcileTimeout = 200 * time.Millisecond
	c := repoCfg(f.origin, nil)
	done := extension.ResourceOwnerStatus{Known: true, ExecutionDone: true, ExecutionSucceeded: true, WorkDone: true, WorkSucceeded: true}
	stuck, err := f.acquireAt("ssh/one", owner("W1", "x1"), c, extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	fine, err := f.acquireAt("ssh/two", owner("W2", "y1"), c, extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	f.set("x1", done)
	f.set("y1", done)
	f.sites["ssh/one"].mu.Lock()
	f.sites["ssh/one"].hang = true
	f.sites["ssh/one"].mu.Unlock()
	start := time.Now()
	report, _ := f.provider.Reconcile(context.Background(), f.status1)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("reconcile took %s with one site hanging", took)
	}
	if exists(fine.Dir) || report.Removed != 1 || !exists(stuck.Dir) {
		t.Fatalf("report %+v: the other site's clone stayed %v", report, exists(fine.Dir))
	}
	tries := f.sites["ssh/one"].runs()
	if _, err := f.provider.Reconcile(context.Background(), f.status1); err != nil || f.sites["ssh/one"].runs() != tries {
		t.Fatalf("the hanging site was tried again within its backoff: %v", err)
	}
}
