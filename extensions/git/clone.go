package git

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// identityKeys are the checkout's own commit identity and signing settings,
// carried into a clone so its commits are made as the checkout's would be.
var identityKeys = []string{"user.name", "user.email", "user.signingkey", "gpg.format", "commit.gpgsign", "tag.gpgsign"}

// createClone makes a clone workspace: a local clone of the checkout with its
// objects copied, never hardlinked (a hardlink is the checkout's own file,
// which a process that can write the clone could rewrite) or borrowed
// through alternates. Nothing in it is the checkout's, so a gc or repack in
// the checkout can't take an object away, and nothing written in it reaches
// the checkout. Its remote is the checkout's configured remote (pushes go there, with
// the operator's own credential helpers), its remote-tracking branches are
// the checkout's, and it is on the Work's branch.
func (p *Provider) createClone(ctx context.Context, cfg Config, checkout string, l *lease, log io.Writer) error {
	defer p.lock("repo:" + checkout)()
	p.clearStale(ctx, checkout, l.Path)
	l.Base = p.base(ctx, cfg, checkout)
	var url, pushURL string
	if p.hasRemote(ctx, checkout, cfg.Remote) {
		if _, err := p.git(ctx, checkout, "fetch", "-q", cfg.Remote); err != nil {
			logf(log, "fetch failed, carrying on: %s", firstLine(err.Error()))
		}
		url, _ = p.git(ctx, checkout, "config", "--get", "remote."+cfg.Remote+".url")
		pushURL, _ = p.git(ctx, checkout, "config", "--get", "remote."+cfg.Remote+".pushurl")
		if u, pu := withoutUserinfo(url), withoutUserinfo(pushURL); u != url || pu != pushURL {
			url, pushURL = u, pu
			logf(log, "left the credentials in the remote URL out of the clone; it uses your credential helpers")
		}
	}
	// What the branch starts from, as in createWork; local is a branch of the
	// checkout the clone needs too.
	var start, local, tracking, done string
	switch {
	case p.refExists(ctx, checkout, "refs/heads/"+l.Branch):
		start, local = l.Branch, l.Branch
		done = fmt.Sprintf("cloned the checkout on its existing branch %s in %s", l.Branch, l.Path)
	case p.refExists(ctx, checkout, "refs/remotes/"+cfg.Remote+"/"+l.Branch):
		start = cfg.Remote + "/" + l.Branch
		tracking = start
		done = fmt.Sprintf("cloned the checkout on %s from %s in %s", l.Branch, cfg.Remote, l.Path)
	default:
		start = cfg.Remote + "/" + l.Base
		if !p.refExists(ctx, checkout, "refs/remotes/"+start) {
			start, local = l.Base, l.Base
		}
		done = fmt.Sprintf("cloned the checkout and created %s from %s in %s", l.Branch, start, l.Path)
	}
	if err := p.roomFor(ctx, cfg, l, start); err != nil {
		return err
	}
	if _, err := p.git(ctx, checkout, "clone", "-q", "--no-hardlinks", "--no-checkout", "--origin", cfg.Remote, "--", checkout, l.Path); err != nil {
		return err
	}
	// The clone's remote names the checkout; point it at the real remote and
	// take the checkout's view of that remote instead of its local branches.
	initial, _ := p.git(ctx, l.Path, "symbolic-ref", "-q", "--short", "HEAD")
	if _, err := p.git(ctx, l.Path, "remote", "remove", cfg.Remote); err != nil {
		return err
	}
	if url != "" {
		if _, err := p.git(ctx, l.Path, "remote", "add", cfg.Remote, url); err != nil {
			return err
		}
		if pushURL != "" {
			if _, err := p.git(ctx, l.Path, "remote", "set-url", "--push", cfg.Remote, pushURL); err != nil {
				return err
			}
		}
	}
	refspecs := []string{"+refs/remotes/" + cfg.Remote + "/*:refs/remotes/" + cfg.Remote + "/*", "+refs/tags/*:refs/tags/*"}
	if local != "" {
		refspecs = append(refspecs, "+refs/heads/"+local+":refs/heads/"+local)
	}
	if _, err := p.git(ctx, l.Path, append([]string{"fetch", "-q", "--no-tags", "--update-head-ok", checkout}, refspecs...)...); err != nil {
		return err
	}
	for _, key := range identityKeys {
		if v, err := p.git(ctx, checkout, "config", "--get", key); err == nil && v != "" {
			if _, err := p.git(ctx, l.Path, "config", key, v); err != nil {
				return err
			}
		}
	}
	args := []string{"checkout", "-q", "--no-track", "-b", l.Branch, start}
	if local == l.Branch {
		args = []string{"checkout", "-q", l.Branch}
	}
	if _, err := p.git(ctx, l.Path, args...); err != nil {
		return err
	}
	if tracking != "" {
		_, _ = p.git(ctx, l.Path, "branch", "-q", "--set-upstream-to", tracking)
	} else if url != "" {
		// A new branch's upstream is set now, as a first `git push -u`
		// would: a sandbox keeps the clone's config read-only (B09), so the
		// push can't record it later.
		_, _ = p.git(ctx, l.Path, "config", "branch."+l.Branch+".remote", cfg.Remote)
		_, _ = p.git(ctx, l.Path, "config", "branch."+l.Branch+".merge", "refs/heads/"+l.Branch)
	}
	if initial != "" && initial != l.Branch && initial != local {
		_, _ = p.git(ctx, l.Path, "branch", "-q", "-D", initial)
	}
	logf(log, "%s", done)
	return nil
}

// withoutUserinfo drops a token or password embedded in a remote URL.
func withoutUserinfo(url string) string { return credentialURL.ReplaceAllString(url, "${1}") }

// validClone reports whether path is a repository of its own: the root of
// its work tree, with its git directory inside it and no alternates, so
// nothing it writes lands in another repository.
func (p *Provider) validClone(ctx context.Context, path string) bool {
	if _, err := p.toplevel(ctx, path); err != nil {
		return false
	}
	common, err := p.git(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false
	}
	got, err1 := filepath.EvalSymlinks(common)
	want, err2 := filepath.EvalSymlinks(filepath.Join(path, ".git"))
	return err1 == nil && err2 == nil && got == want && !exists(filepath.Join(want, "objects", "info", "alternates"))
}

func isClone(l lease) bool {
	return l.Mode == ModeClone || strings.HasPrefix(l.Ref, ModeClone+"-")
}

// saveBranch keeps a clone's branch when the clone goes, as a worktree's
// branch is kept: when it has commits no remote-tracking branch has, they
// are fetched into the checkout's branch of that name, or, when that can't
// take them (it has moved on, or is checked out), into <branch>-<ref>.
func (p *Provider) saveBranch(ctx context.Context, l lease) error {
	if l.Branch == "" || l.Checkout == "" || !exists(l.Checkout) || !p.refExists(ctx, l.Path, "refs/heads/"+l.Branch) {
		return nil
	}
	count, err := p.git(ctx, l.Path, "rev-list", "--count", "refs/heads/"+l.Branch, "--not", "--remotes")
	if n, cerr := strconv.Atoi(count); err != nil || cerr != nil || n == 0 {
		return nil
	}
	src := "refs/heads/" + l.Branch
	if _, err := p.git(ctx, l.Checkout, "fetch", "-q", "--no-tags", l.Path, src+":"+src); err == nil {
		return nil
	}
	_, err = p.git(ctx, l.Checkout, "fetch", "-q", "--no-tags", l.Path, "+"+src+":"+src+"-"+l.Ref)
	return err
}

// cloneObjects is what a clone of the checkout adds to the workspace's disk
// besides its files: a copy of all the checkout's objects.
func (p *Provider) cloneObjects(ctx context.Context, checkout string) uint64 {
	out, err := p.git(ctx, checkout, "count-objects", "-v")
	if err != nil {
		return 0
	}
	var kib uint64
	for _, line := range strings.Split(out, "\n") {
		name, value, _ := strings.Cut(line, ": ")
		if name == "size" || name == "size-pack" {
			n, _ := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			kib += n
		}
	}
	return kib << 10
}
