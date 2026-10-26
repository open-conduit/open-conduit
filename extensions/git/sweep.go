package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/extension"
)

// Spare reasons and the reasons a sweep keeps a spare workspace.
const (
	spareFinished = "finished_work"
	spareUnknown  = "owner_unknown"
	spareOrphaned = "orphaned"

	keptUncommitted = "uncommitted"
	keptUnpushed    = "unpushed"
	keptInUse       = "in_use"
	keptUnreadable  = "unreadable"
	keptFailed      = "remove_failed"
)

// spareIdle is how long a workspace no lease or record holds is left before
// the sweep takes it, so one made by hand isn't taken from under whoever made
// it (v1 spareAge).
const spareIdle = 24 * time.Hour

// Sweep removes spare workspaces that Reconcile's owner-driven release does
// not reach (v1 #63's six-hourly worktree sweep): those of finished Work that
// are still kept (a failed or cancelled Work's, inside its retention), those
// whose owner no record holds, and directories under the provider's root no
// lease holds at all. The last two must have been idle for a day. It never
// removes a workspace with uncommitted changes (copied files aside), with
// commits no remote has, that git can't read, or that a running process
// uses; those are reported as kept. Branches are always kept. Shared leases
// (the main checkout) are never touched.
func (p *Provider) Sweep(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceSweepReport, error) {
	var report extension.ResourceSweepReport
	all, err := p.leases()
	if err != nil {
		return report, err
	}
	var errs []error
	held := map[string]bool{}
	for _, l := range all {
		held[l.Ref] = true
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if err := p.sweepLease(ctx, l.Ref, status, &report); err != nil {
			errs = append(errs, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(p.o.StateDir, "workspaces"))
	if err != nil {
		return report, errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if e.IsDir() && refName.MatchString(e.Name()) && !held[e.Name()] {
			if err := p.sweepOrphan(ctx, e.Name(), &report); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return report, errors.Join(errs...)
}

func (p *Provider) sweepLease(ctx context.Context, ref string, status extension.ResourceStatusFunc, report *extension.ResourceSweepReport) error {
	defer p.lock(ref)()
	l, ok, err := p.load(ref)
	if err != nil || !ok {
		return err
	}
	// Acquiring and failed-cleanup leases are Reconcile's; shared checkouts
	// are never removed.
	if l.Mode == extension.ResourceShared || l.State == stateAcquiring || l.State == stateCleanupFailed || !p.owned(l.Path) || !exists(l.Path) {
		return nil
	}
	st, err := status(ctx, l.Owner)
	if err != nil {
		return err
	}
	var spare string
	switch {
	case !st.Known:
		if !p.idle(l.Path) {
			return nil
		}
		spare = spareUnknown
	case st.WorkDone && (l.Mode == extension.ResourceWork || st.ExecutionDone):
		spare = spareFinished
	default:
		return nil
	}
	item := extension.ResourceSpare{Ref: l.Ref, Mode: l.Mode, Spare: spare}
	if item.Kept = p.keep(ctx, l.Path, l.Copy); item.Kept != "" {
		report.Kept = append(report.Kept, item)
		return nil
	}
	if err := p.removeWorkspace(ctx, l); err != nil {
		l.State, l.Code = stateCleanupFailed, "cleanup_failed"
		l.Attempts++
		item.Kept = keptFailed
		report.Kept = append(report.Kept, item)
		return errors.Join(err, p.save(&l))
	}
	report.Removed = append(report.Removed, item)
	return p.drop(l.Ref)
}

// sweepOrphan removes a workspace directory no lease holds (its lease was
// lost, or it was made by hand) once it has been idle for a day.
func (p *Provider) sweepOrphan(ctx context.Context, ref string, report *extension.ResourceSweepReport) error {
	defer p.lock(ref)()
	path := filepath.Join(p.o.StateDir, "workspaces", ref)
	// A lease written since the listing, or one that can't be read, holds it.
	if exists(p.leasePath(ref)) || !exists(path) || !p.idle(path) {
		return nil
	}
	mode, _, _ := strings.Cut(ref, "-")
	item := extension.ResourceSpare{Ref: ref, Mode: mode, Spare: spareOrphaned}
	checkout := p.checkoutOf(ctx, path)
	if checkout == "" {
		item.Kept = keptUnreadable
	} else {
		item.Kept = p.keep(ctx, path, nil)
	}
	if item.Kept != "" {
		report.Kept = append(report.Kept, item)
		return nil
	}
	if err := p.removeWorkspace(ctx, lease{Ref: ref, Path: path, Checkout: checkout}); err != nil {
		item.Kept = keptFailed
		report.Kept = append(report.Kept, item)
		return err
	}
	report.Removed = append(report.Removed, item)
	return nil
}

// keep says why a spare workspace must stay, or "" when it can go: it has
// uncommitted changes (files the provider copied in aside), commits no
// remote has, git can't read it, or a process runs from it.
func (p *Provider) keep(ctx context.Context, path string, copied []string) string {
	if _, err := p.toplevel(ctx, path); err != nil {
		return keptUnreadable
	}
	args := []string{"status", "--porcelain", "--", "."}
	for _, name := range copied {
		args = append(args, ":(exclude,literal)"+name)
	}
	out, err := p.git(ctx, path, args...)
	switch {
	case err != nil:
		return keptUnreadable
	case out != "":
		return keptUncommitted
	}
	// Commits on HEAD that no remote-tracking branch has (with no remote at
	// all, every commit counts, so nothing is lost).
	count, err := p.git(ctx, path, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if n, cerr := strconv.Atoi(count); err != nil || cerr != nil {
		return keptUnreadable
	} else if n > 0 {
		return keptUnpushed
	}
	if inUse(path) {
		return keptInUse
	}
	return ""
}

// idle is a workspace whose directory has not changed for spareIdle.
func (p *Provider) idle(path string) bool {
	info, err := os.Stat(path)
	return err == nil && p.o.Now().Sub(info.ModTime()) >= spareIdle
}

// checkoutOf is the main checkout a workspace belongs to, from its git
// common directory, or "" when git can't say.
func (p *Provider) checkoutOf(ctx context.Context, path string) string {
	common, err := p.git(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Base(common) != ".git" {
		return ""
	}
	checkout := filepath.Dir(common)
	if real, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = real
	}
	if checkout == path || !exists(checkout) {
		return ""
	}
	return checkout
}

var _ extension.ResourceSweeper = (*Provider)(nil)
