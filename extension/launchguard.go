package extension

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// LaunchGuard is what GuardLaunch checks a confined launch against: the
// host's protected set and its exceptions, and the launch the executor would
// make. Both the process sandbox broker (B09) and the container broker call
// it, so every confined launch passes the same path checks.
type LaunchGuard struct {
	// Protected are the host's protected roots; Shared the paths beneath
	// them a process may read; Records the host-owned record directory,
	// outside every grant.
	Protected, Shared []string
	Records           string
	// RunRoot is the executor's runs directory: RunDir must be a directory
	// beneath it, reached without symbolic links.
	RunRoot, Toolbelt string
	// Dir and RunDir are the launch's working and run directories. Home,
	// for a container, is the Work's own home: like them, an exception to
	// the protected set for this launch only.
	Dir, RunDir, Home string
	// Resource is the workspace the executor acquired for this attempt,
	// if any: the launch must be in it, and it must say what a confined
	// process needs from it.
	Resource *Resource
	Needs    SandboxNeeds
	// MaxScan bounds the hard-link walk of the workspace (default
	// 1,000,000 entries).
	MaxScan int
}

// GuardedLaunch is a launch that passed GuardLaunch: its canonical
// workspace and run directory, the protected set with its aliases, the
// shared paths and the acquired resource's needs.
type GuardedLaunch struct {
	Workspace, Run    string
	Home              string
	Protected, Shared []string
	Resource          SandboxNeeds
}

// GuardLaunch checks a confined launch, failing closed. The run directory
// must be the executor's own; neither it nor the workspace may contain
// anything the host protects; beneath the protected set a need may reach
// only those two directories (and read what the host shares, and the
// toolbelt socket); and the workspace may hold no hard link to a file
// outside it. A refusal is ErrSandboxDenied (sandbox_denied) and anything
// it can't check is ErrSandboxUnavailable.
func GuardLaunch(g LaunchGuard) (GuardedLaunch, error) {
	if g.MaxScan <= 0 {
		g.MaxScan = 1_000_000
	}
	protected := sandboxAliases(g.Protected)
	inside := func(p string) bool { return Within(p, protected) }
	workspace, run := CanonicalPath(g.Dir), CanonicalPath(g.RunDir)
	out := GuardedLaunch{Workspace: workspace, Run: run, Protected: protected}
	own := []string{workspace, run}
	if g.Home != "" {
		out.Home = CanonicalPath(g.Home)
		own = append(own, out.Home)
	}
	if res := g.Resource; res != nil {
		if CanonicalPath(res.Dir) != workspace {
			return GuardedLaunch{}, fmt.Errorf("%w: the launch is not in the acquired workspace", ErrSandboxUnavailable)
		}
		if res.Sandbox == nil {
			return GuardedLaunch{}, fmt.Errorf("%w: the %s workspace can't be used in a sandbox", ErrSandboxUnavailable, res.Kind)
		}
		out.Resource = *res.Sandbox
	}
	// The run directory is the executor's own, reached without links, so
	// it can't stand for a directory of other attempts' files.
	root := filepath.Clean(g.RunRoot)
	rel, err := filepath.Rel(root, filepath.Clean(g.RunDir))
	if g.RunRoot == "" || err != nil || rel == "." || strings.HasPrefix(rel, "..") || run != filepath.Join(CanonicalPath(root), rel) {
		return GuardedLaunch{}, fmt.Errorf("%w: the run directory is not the executor's own", ErrSandboxDenied)
	}
	// The exceptions to the protected set never contain what it protects.
	out.Shared = canonical(g.Shared)
	records := CanonicalPath(g.Records)
	for _, o := range own {
		for _, p := range append(append(append([]string{}, protected...), out.Shared...), records) {
			if p == o || Within(p, []string{o}) {
				return GuardedLaunch{}, fmt.Errorf("%w: %s would expose %s", ErrSandboxDenied, o, p)
			}
		}
	}
	// Beneath the protected set, an executor may only reach its own run
	// directory, workspace and home, and read what the host shares.
	var denied []string
	check := func(kind string, paths []string, extra []string) {
		for _, p := range paths {
			c := CanonicalPath(p)
			if inside(c) && !Within(c, append(append([]string{}, own...), extra...)) {
				denied = append(denied, kind+" "+p)
			}
		}
	}
	check("write", g.Needs.Write, nil)
	check("read", g.Needs.Read, out.Shared)
	check("read", g.Needs.ReadOnly, out.Shared)
	for _, sock := range g.Needs.Sockets {
		if c := CanonicalPath(sock); inside(c) && (g.Toolbelt == "" || c != CanonicalPath(g.Toolbelt)) {
			denied = append(denied, "socket "+sock)
		}
	}
	if len(denied) > 0 {
		return GuardedLaunch{}, fmt.Errorf("%w: inside Open Conduit's state: %s", ErrSandboxDenied, strings.Join(denied, ", "))
	}
	if err := hardLinks(workspace, g.MaxScan); err != nil {
		return GuardedLaunch{}, err
	}
	return out, nil
}

// CanonicalPath is p made absolute, cleaned and with every symbolic link in
// its longest existing prefix resolved (on macOS /var is /private/var), so
// paths compare as the kernel sees them.
func CanonicalPath(p string) string {
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Within reports whether p is one of roots or beneath one.
func Within(p string, roots []string) bool {
	for _, r := range roots {
		if r == "" {
			continue
		}
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func canonical(paths []string) []string {
	var out []string
	for _, p := range paths {
		if p != "" {
			out = append(out, CanonicalPath(p))
		}
	}
	return out
}

func canonicalNeeds(n SandboxNeeds) SandboxNeeds {
	n.Read, n.Write, n.ReadOnly, n.Deny = canonical(n.Read), canonical(n.Write), canonical(n.ReadOnly), sandboxAliases(n.Deny)
	return n
}

// sandboxAliases are paths canonicalised, with every other path the same
// directory is reachable by (a bind mount on Linux) added.
func sandboxAliases(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		c := CanonicalPath(p)
		add(c)
		for _, a := range mountAliases(c) {
			add(a)
		}
	}
	return out
}

// hardLinks refuses a workspace holding a hard link to a file outside it.
// A hard link is the same file under another path, and no path policy can
// tell the two apart: one to a protected file would read it from inside.
func hardLinks(root string, max int) error {
	type inode struct{ dev, ino uint64 }
	links := map[inode]uint64{}
	found := map[inode]uint64{}
	names := map[inode]string{}
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > max {
			return errTooMany
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		dev, ino, nlink, ok := fileIdentity(info)
		if !ok || nlink < 2 {
			return nil
		}
		k := inode{dev, ino}
		links[k], names[k] = nlink, path
		found[k]++
		return nil
	})
	if errors.Is(err, errTooMany) {
		return fmt.Errorf("%w: the workspace has more than %d entries to check for hard links", ErrSandboxUnavailable, max)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: could not check the workspace for hard links: %v", ErrSandboxUnavailable, err)
	}
	for k, nlink := range links {
		if found[k] < nlink {
			return fmt.Errorf("%w: %s is a hard link to a file outside the workspace; remove it", ErrSandboxDenied, names[k])
		}
	}
	return nil
}

var errTooMany = errors.New("too many entries")
