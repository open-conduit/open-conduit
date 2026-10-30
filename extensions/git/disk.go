package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// DiskSpaceFunc reports the size of the filesystem path is on and what an
// unprivileged process can still write to it, in bytes. The provider asks it
// before making a work, fresh or clone workspace; tests inject one so a full disk
// needs no full disk.
type DiskSpaceFunc func(path string) (total, free uint64, err error)

// NoSpaceError refuses a new workspace whose checkout would leave its disk
// too full (v1 #63). Nothing was created and the lease was dropped. It wraps
// extension.ErrResourceExhausted: trying again is refused the same way until
// space is freed, and the message says which of the scope's workspaces can go.
type NoSpaceError struct {
	// Path is where the workspace would have been made.
	Path string
	// Free is what the disk has, Need what the checkout takes and Keep what
	// must be left after it, in bytes.
	Free, Need, Keep uint64
	// Workspaces is how many other work and fresh workspaces the scope has.
	Workspaces int
	// Spare names the ones that could be removed: the scope's workspaces kept
	// after their owner finished, and workspaces no lease holds.
	Spare []string
}

func (e *NoSpaceError) Error() string {
	msg := fmt.Sprintf("not enough disk space for a new workspace at %s: the disk has %s free, and the checkout needs about %s on top of the %s kept free (disk_full_percent, disk_reserve_bytes)",
		e.Path, Bytes(e.Free), Bytes(e.Need), Bytes(e.Keep))
	switch {
	case len(e.Spare) > 0:
		return fmt.Sprintf("%s. The scope has %d other workspace(s); these are finished or no longer held and can go: %s. Free space, then retry", msg, e.Workspaces, strings.Join(e.Spare, ", "))
	case e.Workspaces > 0:
		return fmt.Sprintf("%s. The scope has %d other workspace(s), all still in use; free space elsewhere, then retry", msg, e.Workspaces)
	}
	return msg + ". Free space, then retry"
}

func (e *NoSpaceError) Unwrap() error { return extension.ErrResourceExhausted }

// maxSpareNames bounds the refusal message.
const maxSpareNames = 10

// roomFor checks the workspace's disk has room for a checkout of start (and,
// for a clone, the objects it copies), keeping the configured share
// free. A disk it can't read doesn't stop it, as
// in v1.
func (p *Provider) roomFor(ctx context.Context, cfg Config, l *lease, start string) error {
	if cfg.DiskFullPercent <= 0 || cfg.DiskFullPercent >= 100 {
		return nil
	}
	total, free, err := p.o.DiskSpace(existingParent(l.Path))
	if err != nil || total == 0 {
		return nil
	}
	need := p.checkoutSize(ctx, l.Checkout, start)
	if l.Mode == ModeClone {
		need += p.cloneObjects(ctx, l.Checkout)
	}
	keep := min(uint64(float64(total)*(100-cfg.DiskFullPercent)/100), cfg.DiskReserveBytes)
	if free >= need && free-need >= keep {
		return nil
	}
	e := &NoSpaceError{Path: l.Path, Free: free, Need: need, Keep: keep}
	e.Workspaces, e.Spare = p.spareFor(l.Owner.ScopeID, l.Ref)
	return e
}

// spareFor counts the scope's other work and fresh workspaces and names those
// that could go: kept after their owner finished (retained or failed
// cleanup), or present with no lease at all.
func (p *Provider) spareFor(scope domain.ID, self string) (int, []string) {
	all, _ := p.leases()
	held := map[string]bool{self: true}
	count := 0
	var spare []string
	for _, l := range all {
		held[l.Ref] = true
		if l.Ref == self || l.Mode == extension.ResourceShared || l.Owner.ScopeID != scope {
			continue
		}
		count++
		if l.State != stateRetained && l.State != stateCleanupFailed {
			continue
		}
		name := l.Ref
		if l.Branch != "" {
			name += " (branch " + l.Branch + ", " + l.Code + ")"
		} else {
			name += " (" + l.Code + ")"
		}
		spare = append(spare, name)
	}
	entries, _ := os.ReadDir(filepath.Join(p.o.StateDir, "workspaces"))
	for _, e := range entries {
		if e.IsDir() && refName.MatchString(e.Name()) && !held[e.Name()] && !exists(p.leasePath(e.Name())) {
			spare = append(spare, e.Name()+" (no lease)")
		}
	}
	if len(spare) > maxSpareNames {
		spare = append(spare[:maxSpareNames], fmt.Sprintf("and %d more", len(spare)-maxSpareNames))
	}
	return count, spare
}

// checkoutSize is how many bytes ref's files take, 0 when git can't say.
func (p *Provider) checkoutSize(ctx context.Context, checkout, ref string) uint64 {
	out, err := p.git(ctx, checkout, "ls-tree", "-r", "-l", "-z", ref)
	if err != nil {
		return 0
	}
	var total uint64
	for _, entry := range strings.Split(out, "\x00") {
		meta, _, _ := strings.Cut(entry, "\t")
		if f := strings.Fields(meta); len(f) == 4 {
			n, _ := strconv.ParseUint(f[3], 10, 64)
			total += n
		}
	}
	return total
}

// existingParent is path, or its nearest parent that exists.
func existingParent(path string) string {
	for {
		if _, err := os.Stat(path); err == nil || errors.Is(err, os.ErrPermission) {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// Bytes is a size for people to read: "1.4 GB".
func Bytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}
