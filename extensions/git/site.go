package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// A repository config (Config.Repository) has no checkout on the host:
// its workspace is a clone made at the site the attempt's launch is
// placed at (extension.ResourceAcquisition.Site), through commands run
// there. Its path is the lease's path under the provider's directory, the
// same string at every site, so what the run records about it (a
// session's project key) holds wherever the Work runs next. The ledger
// stays on the host and records the site.
//
// Git there reaches the repository as the run's git does: the host gives
// each command of an acquisition the run's environment layer and the
// execution's toolbelt, so a GitHub App connection's transport signs the
// clone in and no credential crosses to the site. The provider passes on
// none of its own.

// siteTimeout bounds a site command other than setup.
const siteTimeout = 10 * time.Minute

// siteDistrust is the prelude of a script that runs git in a clone the
// agent may have written ($dir): the git directory pinned to its own .git,
// and the config that makes git run a program turned off, filter drivers
// included, as distrust does on the host. g runs git there.
const siteDistrust = `export GIT_DIR="$dir/.git" GIT_COMMON_DIR="$dir/.git" GIT_WORK_TREE="$dir" GIT_OPTIONAL_LOCKS=0 GIT_PROTOCOL_FROM_USER=0
over="-c core.fsmonitor=false -c core.hooksPath=/dev/null -c gc.auto=0 -c maintenance.auto=false -c protocol.allow=never -c protocol.file.allow=always"
names=$(git config --show-scope --name-only --get-regexp '^filter\.' 2>/dev/null | awk -F'\t' '$1!="global" && $1!="system" && $1!="command" {print $2}' | sed 's/^filter\.//; s/\.[^.]*$//' | sort -u)
nl='
'
old=$IFS; IFS=$nl
for name in $names; do
	case $name in *[!A-Za-z0-9._-]*) echo "the clone's config names a filter that can't be turned off" >&2; exit 3;; esac
	over="$over -c filter.$name.clean= -c filter.$name.smudge= -c filter.$name.process= -c filter.$name.required=false"
done
IFS=$old
g() { git -C "$dir" $over "$@"; }
`

// Scripts run as `sh -c <script> open-conduit <args>`.
const (
	// siteScriptCreate <dir> <url> <remote> <branch> <base>: a clone of
	// url at dir on branch, which follows remote's branch of that name
	// when it has one and otherwise starts at base (the remote's HEAD,
	// else main, when empty). It prints the base it used.
	siteScriptCreate = `set -eu
dir=$1 url=$2 remote=$3 branch=$4 base=$5
rm -rf -- "$dir"
mkdir -p -- "${dir%/*}"
git clone -q --no-checkout --origin "$remote" -- "$url" "$dir"
cd -- "$dir"
initial=$(git symbolic-ref -q --short HEAD || true)
if [ -z "$base" ]; then
	base=$(git symbolic-ref -q --short "refs/remotes/$remote/HEAD" || true)
	base=${base#"$remote/"}
	[ -n "$base" ] || base=main
fi
if git show-ref --verify -q "refs/remotes/$remote/$branch"; then
	git checkout -q -B "$branch" --track "$remote/$branch"
elif git show-ref --verify -q "refs/remotes/$remote/$base"; then
	git checkout -q -B "$branch" --no-track "$remote/$base"
	git config "branch.$branch.remote" "$remote"
	git config "branch.$branch.merge" "refs/heads/$branch"
else
	git checkout -q --orphan "$branch"
fi
if [ -n "$initial" ] && [ "$initial" != "$branch" ]; then git branch -q -D "$initial" 2>/dev/null || true; fi
mkdir -p .git/hooks .git/info
printf '%s\n' "$base"
`
	// siteScriptInfo <dir>: whether dir is still a clone of its own (exit
	// 2 when it isn't), and its head, branch, uncommitted paths and
	// commits no remote-tracking branch has.
	siteScriptInfo = `dir=$1
cd -- "$dir" 2>/dev/null || exit 2
[ -d .git ] && [ ! -L .git ] && [ ! -e .git/commondir ] && [ ! -e .git/objects/info/alternates ] || exit 2
` + siteDistrust + `mkdir -p .git/hooks .git/info
g rev-parse -q --verify HEAD >/dev/null 2>&1 && head=$(g rev-parse --short HEAD) || head=
branch=$(g symbolic-ref -q --short HEAD || true)
dirty=$(g status --porcelain --ignore-submodules=all -- . | wc -l | tr -d ' ') || exit 4
if [ -n "$head" ]; then unpushed=$(g rev-list --count HEAD --not --remotes) || exit 4; else unpushed=0; fi
printf 'head=%s\nbranch=%s\ndirty=%s\nunpushed=%s\n' "$head" "$branch" "$dirty" "$unpushed"
`
	// siteScriptRemove <dir>: the workspace gone.
	siteScriptRemove = `rm -rf -- "$1"`
	// siteScriptDisk <path>: the size and free space, in KiB, of the
	// filesystem path (or its nearest parent that exists) is on.
	siteScriptDisk = `p=$1
while [ ! -e "$p" ]; do p=${p%/*}; [ -n "$p" ] || p=/; done
df -Pk -- "$p" | awk 'NR==2 {print $2, $4}'
`
	// siteScriptSetupState <dir> <files>...: the setup stamp, then each
	// file of setup_when present in dir, NUL-separated from its blob ID.
	siteScriptSetupState = `dir=$1; shift
cat -- "$dir/.git/` + setupStamp + `" 2>/dev/null || echo
for f; do
	if [ -f "$dir/$f" ] && [ ! -L "$dir/$f" ]; then printf '%s\0%s\n' "$f" "$(git hash-object --no-filters -- "$dir/$f")"; fi
done
`
	// siteScriptSetup <dir> <command>: setup, in dir.
	siteScriptSetup = `cd -- "$1" && exec /bin/sh -c "$2"`
	// siteScriptStamp <dir> <sum>: the setup stamp written.
	siteScriptStamp = `printf '%s\n' "$2" > "$1/.git/` + setupStamp + `"`
	// siteScriptRead <dir> <file>: a regular file within dir (exit 1 when
	// it isn't one).
	siteScriptRead = `f="$1/$2"
[ -f "$f" ] && [ ! -L "$f" ] || exit 1
cat -- "$f"`
)

// UseSites gives the provider the host's site resolver
// (extension.ResourceSiteUser), through which Reconcile reaches the
// workspaces it made at sites.
func (p *Provider) UseSites(sites extension.SiteResolver) {
	p.mu.Lock()
	p.sites = sites
	p.mu.Unlock()
}

// NeedsSite is NeedsSite (extension.ResourceSiteUser).
func (*Provider) NeedsSite(raw json.RawMessage) bool { return NeedsSite(raw) }

var _ extension.ResourceSiteUser = (*Provider)(nil)

// resolve is the site a lease is at, through the host.
func (p *Provider) resolve(ctx context.Context, id string) (extension.Site, error) {
	p.mu.Lock()
	sites := p.sites
	p.mu.Unlock()
	if sites == nil {
		return nil, fmt.Errorf("%w: the host gave the git provider no site resolver", extension.ErrSiteUnavailable)
	}
	return sites(ctx, id)
}

// siteRun runs one script at a site and returns its output. A script that
// ran and failed is an error carrying the redacted first line it wrote.
func siteRun(ctx context.Context, site extension.Site, timeout time.Duration, env []string, script string, args ...string) (string, error) {
	cmd := extension.SiteCommand{Argv: append([]string{"/bin/sh", "-c", script, "open-conduit"}, args...), Env: append([]string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}, env...), Timeout: timeout}
	res, err := site.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := firstLine(redact(string(res.Stderr)))
		if msg == "" {
			msg = "exit status " + strconv.Itoa(res.ExitCode)
		}
		return "", &siteError{code: res.ExitCode, msg: msg}
	}
	return string(res.Stdout), nil
}

// siteError is a script that ran at a site and failed.
type siteError struct {
	code int
	msg  string
}

func (e *siteError) Error() string { return "at the site: " + e.msg }

func exitCode(err error) int {
	var e *siteError
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}

// siteInfo is what siteScriptInfo reports of a workspace at a site.
type siteInfo struct {
	head, branch      string
	dirty, unpushed   int
	valid, unreadable bool
}

// info reads a workspace at a site. An error means the site couldn't be
// reached; a workspace that isn't a clone of its own is !valid, and one
// git can't read is unreadable.
func (p *Provider) info(ctx context.Context, site extension.Site, path string) (siteInfo, error) {
	out, err := siteRun(ctx, site, siteTimeout, nil, siteScriptInfo, path)
	switch exitCode(err) {
	case 2:
		return siteInfo{}, nil
	case 0:
		if err != nil {
			return siteInfo{}, err
		}
	default:
		return siteInfo{valid: true, unreadable: true}, nil
	}
	got := siteInfo{valid: true}
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "head":
			got.head = v
		case "branch":
			got.branch = v
		case "dirty":
			got.dirty, _ = strconv.Atoi(v)
		case "unpushed":
			got.unpushed, _ = strconv.Atoi(v)
		}
	}
	return got, nil
}

// kept is why a workspace at a site must stay, as keep says of one on the
// host: uncommitted paths, commits no remote has, or git can't read it.
func (i siteInfo) kept() string {
	switch {
	case i.unreadable:
		return keptUnreadable
	case i.dirty > 0:
		return keptUncommitted
	case i.unpushed > 0:
		return keptUnpushed
	}
	return ""
}

// atSite makes or reuses the Work's clone of the repository at the
// acquisition's site. An existing one is never reset: local edits and
// commits stay, and the run is told about them. One the Work had at
// another site is left to Reconcile's rules there: removed when it holds
// nothing unpushed, otherwise kept and logged.
func (p *Provider) atSite(ctx context.Context, cfg Config, prm params, a extension.ResourceAcquisition) (extension.Resource, error) {
	site := a.Site
	ref := refFor(ModeClone, a.Owner)
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
	var info siteInfo
	if ok && l.Site == site.ID() && l.Path != "" {
		if info, err = p.info(ctx, site, l.Path); err != nil {
			return extension.Resource{}, err
		}
	}
	if !info.valid {
		// The Work is back at a site where its clone was left: take it
		// back, with its edits and commits.
		if back, got, err := p.takeBack(ctx, site, ref, log); err != nil {
			return extension.Resource{}, err
		} else if got.valid {
			if ok && l.Site != "" && l.Site != site.ID() {
				p.leaveAt(ctx, l, log)
			}
			l, info = back, got
		}
	}
	if info.valid {
		w.Reused = true
		if branch != l.Branch {
			logf(log, "keeping the existing branch %s (requested %s)", l.Branch, branch)
		}
		logf(log, "reusing %s on %s at %s", l.Path, l.Branch, site.ID())
		l.Owner = a.Owner
	} else {
		if ok && l.Site != "" && l.Site != site.ID() {
			p.leaveAt(ctx, l, log)
		}
		l = newLease(ref, ModeClone, cfg, "", a.Owner)
		l.Repository, l.Site, l.Branch = cfg.Repository, site.ID(), branch
		l.Path = filepath.Join(p.o.StateDir, "workspaces", ref)
		if err = p.save(&l); err != nil {
			return extension.Resource{}, err
		}
		if err = p.createAtSite(ctx, cfg, site, &l, log); err != nil {
			p.abandonAtSite(ctx, site, &l, log, err)
			return extension.Resource{}, err
		}
		if info, err = p.info(ctx, site, l.Path); err != nil || !info.valid {
			cause := errors.New("the new clone at the site couldn't be read")
			if err != nil {
				cause = err
			}
			p.abandonAtSite(ctx, site, &l, log, cause)
			return extension.Resource{}, cause
		}
	}
	if !slices.Contains(l.Holders, a.Owner.ExecutionID) {
		l.Holders = append(l.Holders, a.Owner.ExecutionID)
	}
	l.Sign, l.RetainSeconds = cfg.Sign, cfg.RetainFailedSeconds
	w.Dirty, w.Head = info.dirty, info.head
	if w.Dirty > 0 && w.Reused {
		logf(log, "the workspace has %d uncommitted path(s) from earlier runs; they were kept", w.Dirty)
	}
	ran, setupErr := p.setupAtSite(ctx, cfg, site, l.Path, log)
	w.SetupRan = ran
	if setupErr == nil {
		if info, err := p.info(ctx, site, l.Path); err == nil && info.valid {
			w.Head = info.head
		}
	}
	res := resource(l, w)
	res.Files = p.readAtSite(ctx, site, l.Path, a.Request.Files)
	if setupErr != nil {
		logf(log, "%s; the workspace is kept for inspection", setupErr)
		l.State, l.Code = stateSetupFailed, "setup_failed"
		if err := p.save(&l); err != nil {
			return extension.Resource{}, err
		}
		return res, fmt.Errorf("%w: %s", extension.ErrResourceSetup, setupErr)
	}
	l.State, l.Code = stateActive, ""
	if err := p.save(&l); err != nil {
		return extension.Resource{}, err
	}
	return res, nil
}

// createAtSite clones the repository at the site, after checking its disk
// has room.
func (p *Provider) createAtSite(ctx context.Context, cfg Config, site extension.Site, l *lease, log io.Writer) error {
	if err := p.roomAtSite(ctx, cfg, site, l); err != nil {
		return err
	}
	out, err := siteRun(ctx, site, siteTimeout, nil, siteScriptCreate, l.Path, cfg.Repository, cfg.Remote, l.Branch, cfg.BaseBranch)
	if err != nil {
		return fmt.Errorf("couldn't clone the repository at %s: %w", site.ID(), err)
	}
	if base := strings.TrimSpace(out); validRef(base) {
		l.Base = base
	}
	logf(log, "cloned %s on %s at %s in %s", cfg.Repository, l.Branch, site.ID(), l.Path)
	return nil
}

// roomAtSite is roomFor at the site: with no checkout to measure, it
// refuses a clone only when the site's disk is already past its share.
func (p *Provider) roomAtSite(ctx context.Context, cfg Config, site extension.Site, l *lease) error {
	if cfg.DiskFullPercent <= 0 || cfg.DiskFullPercent >= 100 {
		return nil
	}
	out, err := siteRun(ctx, site, siteTimeout, nil, siteScriptDisk, l.Path)
	if err != nil {
		if errors.Is(err, extension.ErrSiteUnavailable) {
			return err
		}
		return nil // a disk the site can't read doesn't stop it
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return nil
	}
	totalK, err1 := strconv.ParseUint(f[0], 10, 64)
	freeK, err2 := strconv.ParseUint(f[1], 10, 64)
	if err1 != nil || err2 != nil || totalK == 0 {
		return nil
	}
	total, free := totalK<<10, freeK<<10
	keep := min(uint64(float64(total)*(100-cfg.DiskFullPercent)/100), cfg.DiskReserveBytes)
	if free >= keep {
		return nil
	}
	e := &NoSpaceError{Path: site.ID() + ":" + l.Path, Free: free, Keep: keep}
	e.Workspaces, e.Spare = p.spareFor(l.Owner.ScopeID, l.Ref)
	return e
}

// setupAtSite runs setup at the site when the command or a setup_when
// file changed since it last succeeded there, as runSetup does on the
// host. It runs where the agent runs, with the provider layer.
func (p *Provider) setupAtSite(ctx context.Context, cfg Config, site extension.Site, path string, log io.Writer) (bool, error) {
	if cfg.Setup == "" {
		return false, nil
	}
	out, err := siteRun(ctx, site, siteTimeout, nil, siteScriptSetupState, append([]string{path}, cfg.SetupWhen...)...)
	if err != nil {
		return false, fmt.Errorf("couldn't read the setup state: %w", err)
	}
	stamp, files, _ := strings.Cut(out, "\n")
	h := sha256.New()
	h.Write([]byte(cfg.Setup))
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(files, "\n"), "\n") {
		if name, oid, ok := strings.Cut(line, "\x00"); ok {
			h.Write([]byte{0})
			h.Write([]byte(name))
			h.Write([]byte{0})
			h.Write([]byte(oid))
			found = true
		}
	}
	if len(cfg.SetupWhen) > 0 && !found {
		logf(log, "skipping setup: none of %s is in the workspace", strings.Join(cfg.SetupWhen, ", "))
		return false, nil
	}
	sum := "site:" + hex.EncodeToString(h.Sum(nil))
	if strings.TrimSpace(stamp) == sum {
		return false, nil
	}
	logf(log, "running setup at %s", site.ID())
	var env []string
	for k, v := range environment("", false, cfg.Sign) {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	timeout := time.Duration(cfg.SetupTimeoutSeconds) * time.Second
	res, err := site.Run(ctx, extension.SiteCommand{Argv: []string{"/bin/sh", "-c", siteScriptSetup, "open-conduit", path, cfg.Setup}, Env: env, Timeout: timeout})
	if err != nil {
		return true, fmt.Errorf("setup couldn't run: %v", err)
	}
	if log != nil {
		_, _ = log.Write(res.Stdout)
		_, _ = log.Write(res.Stderr)
	}
	if res.ExitCode != 0 {
		return true, fmt.Errorf("setup command failed: exit status %d", res.ExitCode)
	}
	if _, err := siteRun(ctx, site, siteTimeout, nil, siteScriptStamp, path, sum); err != nil {
		return true, fmt.Errorf("couldn't record setup: %w", err)
	}
	return true, nil
}

// readAtSite reads the files an executor asked for (ResourceRequest.Files)
// from the workspace at the site: those that are regular files within it.
func (p *Provider) readAtSite(ctx context.Context, site extension.Site, path string, names []string) map[string]string {
	var out map[string]string
	for _, name := range names {
		if !relativeFile(name) {
			continue
		}
		res, err := site.Run(ctx, extension.SiteCommand{Argv: []string{"/bin/sh", "-c", siteScriptRead, "open-conduit", path, name}, Timeout: siteTimeout})
		if err != nil || res.ExitCode != 0 || res.Truncated || bytes.IndexByte(res.Stdout, 0) >= 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = string(res.Stdout)
	}
	return out
}

// abandonAtSite clears a clone that failed before it was usable, as
// abandon does on the host, and drops the lease; one it can't clear is
// recorded as cleanup_failed for Reconcile.
func (p *Provider) abandonAtSite(ctx context.Context, site extension.Site, l *lease, log io.Writer, cause error) {
	logf(log, "%s", cause)
	if _, err := siteRun(ctx, site, siteTimeout, nil, siteScriptRemove, l.Path); err != nil {
		l.State, l.Code = stateCleanupFailed, "cleanup_failed"
		_ = p.save(l)
		logf(log, "couldn't clear the unfinished workspace; it will be removed later")
		return
	}
	_ = p.drop(l.Ref)
}

// leaveAt deals with the workspace a lease had at another site before
// the Work's lease moves to a new one: removed when it holds nothing a
// remote lacks. Otherwise it keeps a lease of its own (leftRef), so
// Reconcile reports it and removes it as any other workspace at a site:
// retained for retain_failed_seconds under why it was kept, or, when its
// site couldn't be reached, cleanup_failed and retried.
func (p *Provider) leaveAt(ctx context.Context, l lease, log io.Writer) {
	kept, err := p.removeAtSite(ctx, l, false)
	if err == nil && kept == "" {
		logf(log, "removed the workspace at %s", l.Site)
		return
	}
	left := l
	left.Ref, left.Holders = leftRef(l.Ref, l.Site), nil
	defer p.lock(left.Ref)()
	now := p.o.Now()
	if err != nil {
		left.State, left.Code, left.Attempts, left.NextAttempt = stateCleanupFailed, "cleanup_failed", 1, now.Add(siteBackoff(1)).UTC()
		logf(log, "the workspace at %s couldn't be reached and is left there, to be removed later: %s", l.Site, firstLine(err.Error()))
	} else {
		left.State, left.Code = stateRetained, kept
		left.RetainUntil = now.Add(time.Duration(l.RetainSeconds) * time.Second).UTC()
		logf(log, "the workspace at %s is left there (%s) until %s", l.Site, kept, left.RetainUntil.Format(time.RFC3339))
	}
	if err := p.save(&left); err != nil {
		logf(log, "couldn't record the workspace left at %s: %s", l.Site, err)
	}
}

// leftRef is the ref of the lease of a workspace a Work's lease ref left
// at site.
func leftRef(ref, site string) string {
	sum := sha256.Sum256([]byte("left\x00" + ref + "\x00" + site))
	return ModeClone + "-" + hex.EncodeToString(sum[:16])
}

// takeBack is the workspace the Work's lease ref left at site, when it is
// still there: its lease, as the Work's again, and what it holds. The
// left lease is dropped once taken back.
func (p *Provider) takeBack(ctx context.Context, site extension.Site, ref string, log io.Writer) (lease, siteInfo, error) {
	left := leftRef(ref, site.ID())
	defer p.lock(left)()
	l, ok, err := p.load(left)
	if err != nil || !ok || l.Site != site.ID() {
		return lease{}, siteInfo{}, err
	}
	info, err := p.info(ctx, site, l.Path)
	if err != nil {
		return lease{}, siteInfo{}, err
	}
	if !info.valid {
		return lease{}, siteInfo{}, p.drop(left)
	}
	l.Ref, l.State, l.Code, l.Attempts, l.NextAttempt, l.RetentionOver, l.RetainUntil = ref, stateAcquiring, "", 0, time.Time{}, false, time.Time{}
	if err := p.save(&l); err != nil {
		return lease{}, siteInfo{}, err
	}
	logf(log, "took back the workspace left at %s", site.ID())
	return l, info, p.drop(left)
}

// siteReconcileTimeout bounds each site command Reconcile runs for one
// lease, so a site that hangs doesn't hold up the others for long.
var siteReconcileTimeout = time.Minute

// siteBackoff is how long a site lease whose cleanup failed for the nth
// time waits before the next try: a minute, doubling to an hour, so a site
// that is down doesn't cost every pass a connection attempt.
func siteBackoff(n int) time.Duration {
	d := time.Minute
	for i := 1; i < n && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// removeAtSite removes a lease's workspace at its site. Unless force, one
// with uncommitted paths or unpushed commits stays, and kept says why.
func (p *Provider) removeAtSite(ctx context.Context, l lease, force bool) (string, error) {
	if !p.owned(l.Path) {
		return "", nil
	}
	site, err := p.resolve(ctx, l.Site)
	if err != nil {
		return "", err
	}
	if !force {
		info, err := p.info(ctx, site, l.Path)
		if err != nil {
			return "", err
		}
		if !info.valid {
			// Nothing of the provider's is there any more.
			return "", nil
		}
		if kept := info.kept(); kept != "" {
			return kept, nil
		}
	}
	_, err = siteRun(ctx, site, siteTimeout, nil, siteScriptRemove, l.Path)
	return "", err
}

// siteSandboxNeeds is CloneSandboxNeeds for a clone at a site, whose
// hooks and info directories the site's own scripts made.
func siteSandboxNeeds(dir string) *extension.SandboxNeeds {
	git := filepath.Join(dir, ".git")
	return &extension.SandboxNeeds{Write: []string{dir}, ReadOnly: []string{
		filepath.Join(git, "config"), filepath.Join(git, "hooks"), filepath.Join(git, "info")}}
}

// reconcileAtSite removes a lease's workspace at its site when Reconcile
// would remove it on the host, unless it holds uncommitted paths or
// unpushed commits: then it is retained for retain_failed_seconds under
// that reason, and removed when that is over.
func (p *Provider) reconcileAtSite(ctx context.Context, l *lease, retain func(string) error, report *extension.ResourceReport) error {
	return p.removeLeaseAtSite(ctx, l, false, retain, report)
}

func (p *Provider) removeLeaseAtSite(ctx context.Context, l *lease, force bool, retain func(string) error, report *extension.ResourceReport) error {
	ctx, cancel := context.WithTimeout(ctx, siteReconcileTimeout)
	defer cancel()
	kept, err := p.removeAtSite(ctx, *l, force)
	if err != nil {
		l.State, l.Code = stateCleanupFailed, "cleanup_failed"
		l.Attempts++
		// A removal that retention ended stays forced when retried.
		l.RetentionOver = l.RetentionOver || force
		l.NextAttempt = p.o.Now().Add(siteBackoff(l.Attempts)).UTC()
		report.Attention = append(report.Attention, extension.ResourceLeaseReport{Ref: l.Ref, Mode: l.Mode, State: l.State, Code: l.Code})
		return errors.Join(err, p.save(l))
	}
	if kept != "" {
		l.Attempts, l.NextAttempt = 0, time.Time{}
		return retain(kept)
	}
	report.Removed++
	return p.drop(l.Ref)
}
