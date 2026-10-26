// Package git is the optional Git workspace resource provider. It gives an
// execution the scope's main checkout (shared), a branch worktree private to
// one Work and reused by its attempts (work), or a detached worktree at the
// base branch's remote head private to one execution (fresh). It copies
// configured files (.env), runs setup when its inputs changed, supplies the
// provider environment layer (opted-in .env, signing policy) and cleans up
// from durable owner state. Core never imports it; executors reach it only
// through the public extension.ResourceBroker.
//
// Every lease is recorded in the provider's private ledger before anything is
// created, so a crash at any point leaves a lease Reconcile can release or
// remove. Shared checkouts are never reset, stashed or rebased.
package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// Lease states.
const (
	stateAcquiring     = "acquiring"
	stateActive        = "active"
	stateSetupFailed   = "setup_failed"
	stateRetained      = "retained"
	stateCleanupFailed = "cleanup_failed"
)

// Options configure one provider registration.
type Options struct {
	// StateDir is a private absolute directory for the ledger and workspaces.
	StateDir string
	// BaseDir resolves relative checkout paths (the config file's directory).
	BaseDir string
	// Now is the wall clock for retention (default time.Now).
	Now func() time.Time
	// Environ supplies the daemon environment (default os.Environ).
	Environ func() []string
	// Git is the git binary (default "git").
	Git string
	// DiskSpace measures free space before a work or fresh workspace is made
	// (default: the filesystem's own figures).
	DiskSpace DiskSpaceFunc
}

// Provider implements extension.ResourceProvider.
type Provider struct {
	o     Options
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var refName = regexp.MustCompile(`^(shared|work|fresh)-[0-9a-f]{32}$`)

// New validates options and creates the private state directories.
func New(o Options) (*Provider, error) {
	if !filepath.IsAbs(o.StateDir) {
		return nil, errors.New("git provider state directory must be absolute")
	}
	if o.BaseDir != "" && !filepath.IsAbs(o.BaseDir) {
		return nil, errors.New("git provider base directory must be absolute")
	}
	for _, dir := range []string{o.StateDir, filepath.Join(o.StateDir, "leases"), filepath.Join(o.StateDir, "workspaces")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Git == "" {
		o.Git = "git"
	}
	if o.DiskSpace == nil {
		o.DiskSpace = diskSpace
	}
	return &Provider{o: o, locks: map[string]*sync.Mutex{}}, nil
}

// lock serializes work on one key (a lease or a repository).
func (p *Provider) lock(key string) func() {
	p.mu.Lock()
	m := p.locks[key]
	if m == nil {
		m = &sync.Mutex{}
		p.locks[key] = m
	}
	p.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// lease is one ledger record. Env values are never stored.
type lease struct {
	Ref           string                  `json:"ref"`
	Mode          string                  `json:"mode"`
	State         string                  `json:"state"`
	Owner         extension.ResourceOwner `json:"owner"`
	Holders       []domain.ID             `json:"holders,omitempty"`
	Checkout      string                  `json:"checkout"`
	Path          string                  `json:"path"`
	Remote        string                  `json:"remote"`
	Base          string                  `json:"base,omitempty"`
	Branch        string                  `json:"branch,omitempty"`
	Sign          bool                    `json:"sign,omitempty"`
	DotEnv        bool                    `json:"dotenv,omitempty"`
	Copy          []string                `json:"copy,omitempty"`
	RetainSeconds int64                   `json:"retain_seconds"`
	Code          string                  `json:"code,omitempty"`
	Attempts      int                     `json:"cleanup_attempts,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	RetainUntil   time.Time               `json:"retain_until,omitempty"`
}

// workspace is Resource.Data (WorkspaceSchema).
type workspace struct {
	Mode     string `json:"mode"`
	Dir      string `json:"dir"`
	Remote   string `json:"remote"`
	Base     string `json:"base,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Head     string `json:"head,omitempty"`
	Reused   bool   `json:"reused,omitempty"`
	Dirty    int    `json:"dirty,omitempty"`
	SetupRan bool   `json:"setup_ran,omitempty"`
	Sync
}

func refFor(mode string, o extension.ResourceOwner) string {
	key := string(o.ScopeID) + "\x00" + string(o.ExecutionID)
	if mode == extension.ResourceWork {
		key = string(o.ScopeID) + "\x00" + string(o.WorkID)
	}
	sum := sha256.Sum256([]byte(mode + "\x00" + key))
	return mode + "-" + hex.EncodeToString(sum[:16])
}

func (p *Provider) leasePath(ref string) string {
	return filepath.Join(p.o.StateDir, "leases", ref+".json")
}

func (p *Provider) load(ref string) (lease, bool, error) {
	var l lease
	b, err := os.ReadFile(p.leasePath(ref))
	if errors.Is(err, os.ErrNotExist) {
		return l, false, nil
	} else if err != nil {
		return l, false, err
	}
	if err = json.Unmarshal(b, &l); err != nil || l.Ref != ref {
		return l, false, fmt.Errorf("git provider: unreadable lease %s", ref)
	}
	return l, true, nil
}

// save writes a lease atomically and durably.
func (p *Provider) save(l *lease) error {
	l.UpdatedAt = p.o.Now().UTC()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = l.UpdatedAt
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	path := p.leasePath(l.Ref)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (p *Provider) drop(ref string) error {
	if err := os.Remove(p.leasePath(ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Join(p.o.StateDir, "leases"))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

func (p *Provider) leases() ([]lease, error) {
	entries, err := os.ReadDir(filepath.Join(p.o.StateDir, "leases"))
	if err != nil {
		return nil, err
	}
	var out []lease
	for _, e := range entries {
		ref, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !refName.MatchString(ref) {
			continue
		}
		if l, ok, err := p.load(ref); err == nil && ok {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (p *Provider) checkout(ctx context.Context, cfg Config) (string, error) {
	dir := cfg.Checkout
	if !filepath.IsAbs(dir) {
		if p.o.BaseDir == "" {
			return "", fmt.Errorf("%w: relative checkout without a base directory", extension.ErrInvalid)
		}
		dir = filepath.Join(p.o.BaseDir, dir)
	}
	top, err := p.toplevel(ctx, filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("%w: checkout is not the root of a git work tree", extension.ErrInvalid)
	}
	return top, nil
}

// Acquire returns the owner's lease for the requested mode, creating the
// workspace only when none exists. See extension.ResourceProvider.
func (p *Provider) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	cfg, err := ParseConfig(a.Config.Data)
	if err != nil {
		return extension.Resource{}, errors.Join(extension.ErrInvalid, err)
	}
	mode := a.Request.Mode
	if mode == "" {
		mode = cfg.Mode
	}
	o := a.Owner
	if o.ScopeID == "" || o.WorkID == "" || o.ExecutionID == "" {
		return extension.Resource{}, fmt.Errorf("%w: resource owner", extension.ErrInvalid)
	}
	prm, err := parseParams(a.Request.Params.Data)
	if err != nil {
		return extension.Resource{}, err
	}
	checkout, err := p.checkout(ctx, cfg)
	if err != nil {
		return extension.Resource{}, err
	}
	switch mode {
	case extension.ResourceShared:
		return p.shared(ctx, cfg, checkout, a)
	case extension.ResourceWork:
		return p.work(ctx, cfg, checkout, prm, a)
	case extension.ResourceFresh:
		return p.fresh(ctx, cfg, checkout, a)
	}
	return extension.Resource{}, fmt.Errorf("%w: git resource mode", extension.ErrUnsupported)
}

func newLease(ref, mode string, cfg Config, checkout string, o extension.ResourceOwner) lease {
	return lease{Ref: ref, Mode: mode, State: stateAcquiring, Owner: o, Checkout: checkout, Remote: cfg.Remote, Sign: cfg.Sign, DotEnv: cfg.DotEnv, Copy: cfg.Copy, RetainSeconds: cfg.RetainFailedSeconds}
}

func resource(l lease, w workspace) extension.Resource {
	w.Mode, w.Dir, w.Remote, w.Base, w.Branch = l.Mode, l.Path, l.Remote, l.Base, l.Branch
	data, _ := json.Marshal(w)
	return extension.Resource{Kind: "workspace", Ref: l.Ref, Dir: l.Path, Env: environment(l.Checkout, l.DotEnv, l.Sign), Data: domain.Payload{Schema: WorkspaceSchema, Data: data}, Summary: summary(w)}
}

// summary is the workspace in one line for run history (05-02).
func summary(w workspace) string {
	remote := w.Remote + "/" + w.Base
	switch w.Mode {
	case extension.ResourceShared:
		s := "main checkout"
		switch {
		case w.Updated > 0:
			s += fmt.Sprintf(", fast-forwarded %d commit(s) to %s", w.Updated, remote)
		case w.Behind > 0:
			s += fmt.Sprintf(", %d commit(s) behind %s: %s", w.Behind, remote, w.Stale)
		case w.Stale != "":
			s += ", " + w.Stale
		default:
			s += ", up to date with " + remote
		}
		if w.Dirty > 0 {
			s += fmt.Sprintf(", %d uncommitted path(s)", w.Dirty)
		}
		return s
	case extension.ResourceFresh:
		return "fresh worktree at " + remote + " " + w.Head[:min(7, len(w.Head))]
	}
	s := "worktree on branch " + w.Branch
	if w.Reused {
		s += ", reused"
	}
	if w.Dirty > 0 {
		s += fmt.Sprintf(", %d uncommitted path(s) inherited", w.Dirty)
	}
	return s
}

// shared hands out the main checkout. The first user catches it up with a
// fast-forward only; while another execution holds it, it is only compared,
// and the run is told how far behind it is and why.
func (p *Provider) shared(ctx context.Context, cfg Config, checkout string, a extension.ResourceAcquisition) (extension.Resource, error) {
	ref := refFor(extension.ResourceShared, a.Owner)
	defer p.lock(ref)()
	l, ok, err := p.load(ref)
	if err != nil {
		return extension.Resource{}, err
	}
	if !ok {
		l = newLease(ref, extension.ResourceShared, cfg, checkout, a.Owner)
	}
	l.State, l.Path, l.Base = stateActive, checkout, p.base(ctx, cfg, checkout)
	if err = p.save(&l); err != nil {
		return extension.Resource{}, err
	}
	defer p.lock("repo:" + checkout)()
	var others []string
	all, err := p.leases()
	if err != nil {
		return extension.Resource{}, err
	}
	for _, x := range all {
		if x.Mode == extension.ResourceShared && x.Ref != ref && x.Checkout == checkout && x.State == stateActive {
			others = append(others, x.Ref)
		}
	}
	var w workspace
	if len(others) > 0 {
		w.Sync = p.behind(ctx, checkout, cfg.Remote, l.Base)
		if w.Behind > 0 && w.Stale == "" {
			w.Stale = "another execution is using the checkout"
		}
	} else {
		w.Sync = p.catchUp(ctx, checkout, cfg.Remote, l.Base)
	}
	w.Head, w.Dirty = p.head(ctx, checkout), p.dirty(ctx, checkout, nil)
	log := a.Request.Log
	switch {
	case w.Updated > 0:
		logf(log, "fast-forwarded the checkout %d commit(s) to %s/%s", w.Updated, cfg.Remote, l.Base)
	case w.Behind > 0:
		logf(log, "running %d commit(s) behind %s/%s: %s", w.Behind, cfg.Remote, l.Base, w.Stale)
	case w.Stale != "":
		logf(log, "%s", w.Stale)
	}
	if w.Dirty > 0 {
		logf(log, "the checkout has %d uncommitted path(s); they were left untouched", w.Dirty)
	}
	return resource(l, w), nil
}

// fresh creates a detached worktree at the base branch's remote head for one
// execution; acquiring again (a resumed segment) reuses it.
func (p *Provider) fresh(ctx context.Context, cfg Config, checkout string, a extension.ResourceAcquisition) (extension.Resource, error) {
	ref := refFor(extension.ResourceFresh, a.Owner)
	defer p.lock(ref)()
	log := a.Request.Log
	l, ok, err := p.load(ref)
	if err != nil {
		return extension.Resource{}, err
	}
	var w workspace
	if ok && l.Path != "" && p.validWorktree(ctx, checkout, l.Path) {
		w.Reused = true
		logf(log, "reusing %s", l.Path)
	} else {
		l = newLease(ref, extension.ResourceFresh, cfg, checkout, a.Owner)
		l.Path = filepath.Join(p.o.StateDir, "workspaces", ref)
		if err = p.save(&l); err != nil {
			return extension.Resource{}, err
		}
		if err = p.createFresh(ctx, cfg, checkout, &l); err != nil {
			p.abandon(ctx, &l, log, err)
			return extension.Resource{}, err
		}
		logf(log, "checked out %s/%s at %s in %s", cfg.Remote, l.Base, p.head(ctx, l.Path), l.Path)
	}
	return p.prepare(ctx, cfg, &l, w, log)
}

func (p *Provider) createFresh(ctx context.Context, cfg Config, checkout string, l *lease) error {
	defer p.lock("repo:" + checkout)()
	p.clearStale(ctx, checkout, l.Path)
	l.Base = p.base(ctx, cfg, checkout)
	if _, err := p.git(ctx, checkout, "fetch", "-q", cfg.Remote, l.Base); err != nil {
		return err
	}
	start := cfg.Remote + "/" + l.Base
	if err := p.roomFor(ctx, cfg, l, start); err != nil {
		return err
	}
	_, err := p.git(ctx, checkout, "worktree", "add", "-q", "--detach", l.Path, start)
	return err
}

// work creates or reuses the Work's branch worktree. An existing worktree is
// never reset: local edits and commits stay, and the run is told about them.
func (p *Provider) work(ctx context.Context, cfg Config, checkout string, prm params, a extension.ResourceAcquisition) (extension.Resource, error) {
	ref := refFor(extension.ResourceWork, a.Owner)
	defer p.lock(ref)()
	log := a.Request.Log
	l, ok, err := p.load(ref)
	if err != nil {
		return extension.Resource{}, err
	}
	branch := prm.Branch
	if branch == "" {
		branch = strings.NewReplacer("{work}", sanitize(string(a.Owner.WorkID)), "{scope}", sanitize(string(a.Owner.ScopeID))).Replace(cfg.BranchTemplate)
	}
	var w workspace
	if ok && l.Path != "" && p.validWorktree(ctx, checkout, l.Path) {
		w.Reused = true
		if branch != l.Branch {
			logf(log, "keeping the existing branch %s (requested %s)", l.Branch, branch)
		}
		logf(log, "reusing %s on %s", l.Path, l.Branch)
		l.Owner = a.Owner
	} else {
		if !ok {
			l = newLease(ref, extension.ResourceWork, cfg, checkout, a.Owner)
		}
		l.State, l.Owner, l.Branch = stateAcquiring, a.Owner, branch
		l.Path = filepath.Join(p.o.StateDir, "workspaces", ref)
		if err = p.save(&l); err != nil {
			return extension.Resource{}, err
		}
		if err = p.createWork(ctx, cfg, checkout, &l, log); err != nil {
			p.abandon(ctx, &l, log, err)
			return extension.Resource{}, err
		}
	}
	if !slices.Contains(l.Holders, a.Owner.ExecutionID) {
		l.Holders = append(l.Holders, a.Owner.ExecutionID)
	}
	l.Sign, l.DotEnv, l.Copy, l.RetainSeconds = cfg.Sign, cfg.DotEnv, cfg.Copy, cfg.RetainFailedSeconds
	w.Dirty = p.dirty(ctx, l.Path, cfg.Copy)
	if w.Dirty > 0 {
		logf(log, "the workspace has %d uncommitted path(s) from earlier runs; they were kept", w.Dirty)
	}
	return p.prepare(ctx, cfg, &l, w, log)
}

func (p *Provider) createWork(ctx context.Context, cfg Config, checkout string, l *lease, log io.Writer) error {
	defer p.lock("repo:" + checkout)()
	p.clearStale(ctx, checkout, l.Path)
	l.Base = p.base(ctx, cfg, checkout)
	if p.hasRemote(ctx, checkout, cfg.Remote) {
		if _, err := p.git(ctx, checkout, "fetch", "-q", cfg.Remote); err != nil {
			logf(log, "fetch failed, carrying on: %s", firstLine(err.Error()))
		}
	}
	// start is what the checkout is made from; tracking, a remote branch it
	// follows.
	var start, tracking, done string
	var args []string
	switch {
	case p.refExists(ctx, checkout, "refs/heads/"+l.Branch):
		start, args = l.Branch, []string{l.Path, l.Branch}
		done = fmt.Sprintf("checked out the existing branch %s in %s", l.Branch, l.Path)
	case p.refExists(ctx, checkout, "refs/remotes/"+cfg.Remote+"/"+l.Branch):
		start = cfg.Remote + "/" + l.Branch
		tracking, args = start, []string{"-b", l.Branch, l.Path, start}
		done = fmt.Sprintf("checked out %s from %s in %s", l.Branch, cfg.Remote, l.Path)
	default:
		start = cfg.Remote + "/" + l.Base
		if !p.refExists(ctx, checkout, "refs/remotes/"+start) {
			start = l.Base
		}
		args = []string{"--no-track", "-b", l.Branch, l.Path, start}
		done = fmt.Sprintf("created %s from %s in %s", l.Branch, start, l.Path)
	}
	if err := p.roomFor(ctx, cfg, l, start); err != nil {
		return err
	}
	if _, err := p.git(ctx, checkout, append([]string{"worktree", "add", "-q"}, args...)...); err != nil {
		return err
	}
	if tracking != "" {
		_, _ = p.git(ctx, l.Path, "branch", "-q", "--set-upstream-to", tracking)
	}
	logf(log, "%s", done)
	return nil
}

// prepare copies files and runs setup, then records the lease's outcome. A
// setup failure keeps the workspace (retained under the failure policy once
// its owner finishes) and returns it with ErrResourceSetup.
func (p *Provider) prepare(ctx context.Context, cfg Config, l *lease, w workspace, log io.Writer) (extension.Resource, error) {
	copyFiles(log, l.Checkout, l.Path, cfg.Copy)
	ran, err := p.runSetup(ctx, log, cfg, l.Path, environment(l.Checkout, l.DotEnv, l.Sign))
	w.SetupRan, w.Head = ran, p.head(ctx, l.Path)
	if err != nil {
		logf(log, "%s; the workspace is kept for inspection", err)
		l.State, l.Code = stateSetupFailed, "setup_failed"
		if serr := p.save(l); serr != nil {
			return extension.Resource{}, serr
		}
		return resource(*l, w), fmt.Errorf("%w: %s", extension.ErrResourceSetup, err)
	}
	l.State, l.Code = stateActive, ""
	if err = p.save(l); err != nil {
		return extension.Resource{}, err
	}
	return resource(*l, w), nil
}

// clearStale removes a leftover directory at a workspace path that is not a
// valid worktree (a crash between ledger write and creation), inside the
// provider's own workspace root only.
func (p *Provider) clearStale(ctx context.Context, checkout, path string) {
	if !exists(path) || !p.owned(path) {
		return
	}
	_, _ = p.git(ctx, checkout, "worktree", "remove", "--force", path)
	_ = os.RemoveAll(path)
	_, _ = p.git(ctx, checkout, "worktree", "prune")
}

// abandon cleans up a creation that failed before the workspace was usable
// (refused for disk space, or a checkout git left half-made by a full disk),
// so it is never reused as if it were whole, and drops the lease. If that
// cleanup fails the lease is recorded as cleanup_failed for Reconcile.
func (p *Provider) abandon(ctx context.Context, l *lease, log io.Writer, cause error) {
	logf(log, "%s", cause)
	if err := p.removeWorkspace(ctx, *l); err != nil {
		l.State, l.Code = stateCleanupFailed, "cleanup_failed"
		_ = p.save(l)
		logf(log, "couldn't clear the unfinished workspace; it will be removed later")
		return
	}
	if !errors.Is(cause, extension.ErrResourceExhausted) {
		logf(log, "the unfinished workspace was cleared")
	}
	_ = p.drop(l.Ref)
}

func (p *Provider) owned(path string) bool {
	root := filepath.Join(p.o.StateDir, "workspaces")
	rel, err := filepath.Rel(root, path)
	return err == nil && refName.MatchString(rel)
}

// removeWorkspace stops stray processes in the workspace, removes the
// worktree and prunes it. The branch is kept, so nothing committed is lost.
func (p *Provider) removeWorkspace(ctx context.Context, l lease) error {
	if l.Path == "" || !p.owned(l.Path) {
		return nil // Shared leases (the main checkout) are never removed.
	}
	if exists(l.Path) {
		killWithin(l.Path)
	}
	defer p.lock("repo:" + l.Checkout)()
	if exists(l.Path) {
		_, _ = p.git(ctx, l.Checkout, "worktree", "remove", "--force", l.Path)
		if err := os.RemoveAll(l.Path); err != nil {
			return err
		}
	}
	if exists(l.Checkout) {
		_, _ = p.git(ctx, l.Checkout, "worktree", "prune")
	}
	return nil
}

// Lookup returns an existing lease owned by the caller without creating,
// fetching or running setup. A work lease is owned by its Work; other leases
// by their execution.
func (p *Provider) Lookup(ctx context.Context, o extension.ResourceOwner, ref string) (extension.Resource, error) {
	if !refName.MatchString(ref) {
		return extension.Resource{}, extension.ErrResourceNotFound
	}
	defer p.lock(ref)()
	l, ok, err := p.load(ref)
	if err != nil {
		return extension.Resource{}, err
	}
	owned := ok && l.Owner.ScopeID == o.ScopeID && l.Owner.WorkID == o.WorkID && (l.Mode == extension.ResourceWork || l.Owner.ExecutionID == o.ExecutionID)
	if !owned || (l.State != stateActive && l.State != stateSetupFailed) || !exists(l.Path) {
		return extension.Resource{}, extension.ErrResourceNotFound
	}
	w := workspace{Reused: true, Head: p.head(ctx, l.Path)}
	if l.Mode == extension.ResourceWork && !slices.Contains(l.Holders, o.ExecutionID) {
		l.Owner = o
		l.Holders = append(l.Holders, o.ExecutionID)
		if err = p.save(&l); err != nil {
			return extension.Resource{}, err
		}
	}
	return resource(l, w), nil
}

// Reconcile releases leases whose owners finished (fresh: removed after
// success, retained after failure; work: when the Work is terminal; shared:
// released), removes workspaces whose retention expired and retries failed
// cleanup. It is safe to repeat and after restart.
func (p *Provider) Reconcile(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceReport, error) {
	var report extension.ResourceReport
	all, err := p.leases()
	if err != nil {
		return report, err
	}
	var errs []error
	for _, l := range all {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if err := p.reconcileOne(ctx, l.Ref, status, &report); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

func (p *Provider) reconcileOne(ctx context.Context, ref string, status extension.ResourceStatusFunc, report *extension.ResourceReport) error {
	defer p.lock(ref)()
	l, ok, err := p.load(ref)
	if err != nil || !ok {
		return err
	}
	now := p.o.Now()
	retain := func(code string) error {
		l.State, l.Code = stateRetained, code
		l.RetainUntil = now.Add(time.Duration(l.RetainSeconds) * time.Second).UTC()
		report.Retained++
		report.Attention = append(report.Attention, extension.ResourceLeaseReport{Ref: l.Ref, Mode: l.Mode, State: l.State, Code: code, RetainUntil: l.RetainUntil})
		return p.save(&l)
	}
	remove := func() error {
		if err := p.removeWorkspace(ctx, l); err != nil {
			l.State, l.Code = stateCleanupFailed, "cleanup_failed"
			l.Attempts++
			report.Attention = append(report.Attention, extension.ResourceLeaseReport{Ref: l.Ref, Mode: l.Mode, State: l.State, Code: l.Code})
			return errors.Join(err, p.save(&l))
		}
		report.Removed++
		return p.drop(l.Ref)
	}
	switch l.State {
	case stateRetained:
		if !now.Before(l.RetainUntil) {
			return remove()
		}
		report.Attention = append(report.Attention, extension.ResourceLeaseReport{Ref: l.Ref, Mode: l.Mode, State: l.State, Code: l.Code, RetainUntil: l.RetainUntil})
		return nil
	case stateCleanupFailed:
		return remove()
	}
	st, err := status(ctx, l.Owner)
	if err != nil {
		return err
	}
	switch l.Mode {
	case extension.ResourceShared:
		if !st.Known || st.ExecutionDone {
			report.Released++
			return p.drop(l.Ref)
		}
	case extension.ResourceFresh:
		switch {
		case !st.Known:
			return retain("owner_unknown")
		case st.ExecutionDone && st.ExecutionSucceeded && l.State == stateActive:
			report.Released++
			return remove()
		case st.ExecutionDone:
			return retain("failed_run")
		}
	case extension.ResourceWork:
		switch {
		case !st.Known:
			return retain("owner_unknown")
		case st.WorkDone && st.WorkSucceeded && l.State != stateAcquiring:
			report.Released++
			return remove()
		case st.WorkDone:
			return retain("failed_work")
		}
	}
	report.Active++
	return nil
}

var _ extension.ResourceProvider = (*Provider)(nil)
