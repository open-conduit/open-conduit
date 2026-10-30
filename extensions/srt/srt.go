// Package srt is the process sandbox provider (B09) for Anthropic's
// sandbox-runtime, `srt`: Seatbelt on macOS, bubblewrap with a seccomp
// filter on Linux, and an allowlisting network proxy on both. It wraps an
// executor's launch in `srt --settings <policy> -- <argv>` with the
// operator's policy, after checking every need against it.
package srt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

const (
	// Type is the provider's name in a binding's sandbox.
	Type = "srt"
	// PolicySchema identifies the effective policy a plan records.
	PolicySchema = "open-conduit.sandbox.srt.policy.v1"
	// MinVersion is the oldest srt this provider runs: the one its
	// isolation tests ran on. TestedVersion is the newest.
	MinVersion    = "0.0.81"
	TestedVersion = "0.0.81"
)

// Options are host-trusted, never from binding config.
type Options struct {
	// Binary is srt, resolved on PATH when not absolute (default "srt").
	Binary string
	// GOOS selects the platform (runtime.GOOS when empty); for tests.
	GOOS string
	// LookPath resolves programs (exec.LookPath when nil).
	LookPath func(string) (string, error)
	// Environ is the host's environment, for the srt process's own PATH
	// (os.Environ when nil).
	Environ func() []string
	// Now is the clock for the plan's record (time.Now when nil).
	Now func() time.Time
}

// Provider implements extension.SandboxProvider.
type Provider struct{ o Options }

// New returns the provider.
func New(o Options) *Provider {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Binary == "" {
		o.Binary = "srt"
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Provider{o: o}
}

// settings is srt's own settings file. Every path is literal, so a
// directory name never reads as a pattern.
type settings struct {
	Network struct {
		AllowedDomains      []string `json:"allowedDomains"`
		DeniedDomains       []string `json:"deniedDomains"`
		StrictAllowlist     bool     `json:"strictAllowlist"`
		AllowUnixSockets    []string `json:"allowUnixSockets,omitempty"`
		AllowAllUnixSockets bool     `json:"allowAllUnixSockets,omitempty"`
		AllowLocalBinding   bool     `json:"allowLocalBinding"`
	} `json:"network"`
	Filesystem struct {
		DenyRead   []literal `json:"denyRead"`
		AllowRead  []literal `json:"allowRead"`
		AllowWrite []literal `json:"allowWrite"`
		DenyWrite  []literal `json:"denyWrite"`
	} `json:"filesystem"`
}

type literal struct {
	Path    string `json:"path"`
	Literal bool   `json:"literal"`
}

func literals(paths []string) []literal {
	out := []literal{}
	for _, p := range unique(paths) {
		out = append(out, literal{p, true})
	}
	return out
}

// record is the plan's Data: the version and the settings srt runs with.
type record struct {
	Version  string          `json:"version"`
	Settings json.RawMessage `json:"settings"`
	File     string          `json:"file"`
}

// knownSockets are local services a process could reach on Linux once every
// unix socket is allowed, hidden unless the policy lists them.
var knownSockets = []string{"/var/run/docker.sock", "/run/docker.sock", "~/.docker/run/docker.sock", "/run/containerd/containerd.sock", "/run/podman/podman.sock"}

func (p *Provider) Plan(ctx context.Context, _ extension.ResourceOwner, policy extension.SandboxPolicy, needs extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	o := p.o
	pol, err := ParseConfig(policy.Config.Data)
	if err != nil {
		return extension.SandboxPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	if o.GOOS != "darwin" && o.GOOS != "linux" {
		return extension.SandboxPlan{}, fmt.Errorf("%w: srt does not support %s", extension.ErrSandboxUnavailable, o.GOOS)
	}
	bin, version, err := p.binary(ctx)
	if err != nil {
		return extension.SandboxPlan{}, err
	}
	paths := policy.Paths
	home, _ := os.UserHomeDir()
	expand := func(list []string) []string {
		var out []string
		for _, s := range list {
			s = strings.NewReplacer("{workspace}", paths.Workspace, "{run}", paths.Run, "{toolbelt}", paths.Toolbelt).Replace(s)
			if s == "~" || strings.HasPrefix(s, "~/") {
				s = home + s[1:]
			}
			if s != "" && filepath.IsAbs(s) {
				out = append(out, extension.CanonicalPath(s))
			}
		}
		return out
	}
	run := extension.CanonicalPath(paths.Run)
	grants := append(expand(pol.Write), run)
	readDeny, readAllow := expand(pol.Read.Deny), expand(pol.Read.Allow)
	sockets := expand(pol.Sockets)

	// The host's denies are not the operator's to lift: a grant never
	// reaches into one except at its own exceptions (this attempt's run
	// directory, its workspace and what the host shares), which the host
	// passes as needs beneath it.
	exceptions := append(append(append([]string{run, extension.CanonicalPath(paths.Workspace)}, needs.Read...), needs.Write...), needs.ReadOnly...)
	writable := append(append([]string{run, extension.CanonicalPath(paths.Workspace)}, needs.Write...), grants...)
	var refused, denyWrite []string
	for _, d := range needs.Deny {
		for _, g := range grants {
			if extension.Within(d, []string{g}) {
				// srt's write denies take precedence over its grants, so a
				// deny inside a grant holds only if nothing beneath it must
				// stay writable.
				inner := false
				for _, w := range writable {
					if w != g && extension.Within(w, []string{d}) {
						inner = true
					}
				}
				if inner || d == g {
					refused = append(refused, fmt.Sprintf("write %s contains %s", g, d))
				} else {
					denyWrite = append(denyWrite, d)
				}
			} else if extension.Within(g, []string{d}) && !extension.Within(g, exceptions) {
				refused = append(refused, fmt.Sprintf("write %s is inside %s", g, d))
			}
		}
		for _, a := range readAllow {
			if extension.Within(a, []string{d}) && !extension.Within(a, exceptions) {
				refused = append(refused, fmt.Sprintf("read %s is inside %s", a, d))
			}
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return extension.SandboxPlan{}, fmt.Errorf("%w: the policy reaches into what Open Conduit protects: %s", extension.ErrSandboxDenied, strings.Join(refused, ", "))
	}

	// Needs are checked against the policy, and never widen it.
	var denied []string
	for _, w := range needs.Write {
		if !extension.Within(w, grants) {
			denied = append(denied, "write "+w)
		}
	}
	readable := append(append([]string{}, readAllow...), grants...)
	for _, r := range append(append([]string{}, needs.Read...), needs.ReadOnly...) {
		if extension.Within(r, readDeny) && !extension.Within(r, readable) {
			denied = append(denied, "read "+r)
		}
	}
	for _, h := range needs.Hosts {
		if !hostAllowed(h, pol.Network.Allow) {
			denied = append(denied, "host "+h)
		}
	}
	for _, s := range needs.Sockets {
		if !extension.Within(extension.CanonicalPath(s), sockets) {
			denied = append(denied, "socket "+s)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return extension.SandboxPlan{}, fmt.Errorf("%w: %s", extension.ErrSandboxDenied, strings.Join(denied, ", "))
	}

	var s settings
	s.Network.AllowedDomains = nonNil(pol.Network.Allow)
	s.Network.DeniedDomains = []string{}
	s.Network.StrictAllowlist = true
	deny := append(append([]string{}, readDeny...), needs.Deny...)
	// A read exception is listed only beneath a deny: listing a broader
	// path would lift the denies inside it.
	var allow []string
	for _, a := range append(append(append(append([]string{}, readAllow...), exceptions...), grants...), sockets...) {
		if extension.Within(a, deny) {
			for _, d := range deny {
				if d != a && extension.Within(d, []string{a}) && !extension.Within(a, []string{d}) {
					return extension.SandboxPlan{}, fmt.Errorf("%w: %s is readable but holds the denied %s", extension.ErrSandboxUnavailable, a, d)
				}
			}
			allow = append(allow, a)
		}
	}
	if o.GOOS == "linux" {
		// srt binds a read exception after, and over, the writable paths
		// beneath it, which would leave them read-only.
		for _, a := range allow {
			for _, g := range grants {
				if g != a && extension.Within(g, []string{a}) && !extension.Within(a, grants) {
					return extension.SandboxPlan{}, fmt.Errorf("%w: on Linux the writable %s can't be beneath the read exception %s", extension.ErrSandboxUnavailable, g, a)
				}
			}
		}
	}
	readOnly := append(append([]string{}, needs.ReadOnly...), denyWrite...)
	for _, r := range readOnly {
		for _, g := range grants {
			if g != r && extension.Within(g, []string{r}) {
				return extension.SandboxPlan{}, fmt.Errorf("%w: the writable %s is beneath the read-only %s", extension.ErrSandboxUnavailable, g, r)
			}
		}
	}
	if len(sockets) > 0 {
		if o.GOOS == "linux" {
			if pol.UnixSockets != "all" {
				return extension.SandboxPlan{}, fmt.Errorf(`%w: Linux can't allow a unix socket by path; set unix_sockets to "all" to accept every socket the filesystem policy leaves visible`, extension.ErrSandboxUnavailable)
			}
			s.Network.AllowAllUnixSockets = true
			for _, known := range expand(knownSockets) {
				if _, err := os.Lstat(known); err == nil && !extension.Within(known, sockets) {
					deny = append(deny, known)
				}
			}
		} else {
			s.Network.AllowUnixSockets = sockets
		}
	}
	s.Filesystem.DenyRead = literals(deny)
	s.Filesystem.AllowRead = literals(allow)
	s.Filesystem.AllowWrite = literals(grants)
	s.Filesystem.DenyWrite = literals(readOnly)
	doc, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return extension.SandboxPlan{}, err
	}
	file := filepath.Join(paths.Records, "srt.json")
	if err = writeReadOnly(file, append(doc, '\n')); err != nil {
		return extension.SandboxPlan{}, fmt.Errorf("%w: could not write the policy: %v", extension.ErrSandboxUnavailable, err)
	}
	wrapperPath := p.wrapperPath(bin, l.Env)
	if err = p.preflight(ctx, bin, file, l.Dir, wrapperPath); err != nil {
		return extension.SandboxPlan{}, err
	}
	argv := []string{"/usr/bin/env", "PATH=" + wrapperPath}
	argv, restore := wrapperEnv(argv, l)
	argv = append(append(argv, bin, "--settings", file, "--"), restore...)
	argv = append(argv, l.Argv...)
	data, _ := json.Marshal(record{Version: version, Settings: doc, File: file})
	signals := extension.SandboxTerm
	if o.GOOS == "linux" {
		// bubblewrap runs the command in a new session: a stop kills it.
		signals = extension.SandboxKill
	}
	return extension.SandboxPlan{
		Argv: argv, Env: l.Env, Signals: signals,
		Summary: fmt.Sprintf("srt %s: %d writable path(s), %d host(s), %d socket(s)", version, len(unique(grants)), len(pol.Network.Allow), len(sockets)),
		Data:    domain.Payload{Schema: PolicySchema, Data: data},
	}, nil
}

// wrapperEnv sets the launch's WrapperEnv for srt alone and restores the
// child's own values for those keys inside the sandbox.
func wrapperEnv(argv []string, l extension.SandboxLaunch) ([]string, []string) {
	child := map[string]string{}
	for _, kv := range l.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			child[k] = v
		}
	}
	keys := []string{"PATH"}
	for _, kv := range l.WrapperEnv {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			argv = append(argv, kv)
			keys = append(keys, k)
		}
	}
	restore := []string{"/usr/bin/env"}
	for _, k := range keys {
		if v, ok := child[k]; ok {
			restore = append(restore, k+"="+v)
		} else {
			restore = append(restore, "-u", k)
		}
	}
	return argv, restore
}

// wrapperPath is srt's own PATH: its directory and node's first (srt is a
// node script), then the child's.
func (p *Provider) wrapperPath(bin string, env []string) string {
	dirs := []string{filepath.Dir(bin)}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		dirs = append(dirs, filepath.Dir(real))
	}
	if node, err := p.o.LookPath("node"); err == nil {
		dirs = append(dirs, filepath.Dir(node))
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			dirs = append(dirs, v)
		}
	}
	if len(dirs) == 0 {
		dirs = append(dirs, "/usr/bin:/bin")
	}
	return strings.Join(unique(dirs), string(os.PathListSeparator))
}

var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// binary resolves srt and its version, and on Linux what srt needs.
func (p *Provider) binary(ctx context.Context) (string, string, error) {
	o := p.o
	bin, err := o.LookPath(o.Binary)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s not found", extension.ErrSandboxUnavailable, o.Binary)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return "", "", fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	if o.GOOS == "linux" {
		for _, dep := range []string{"bwrap", "socat", "rg"} {
			if _, err := o.LookPath(dep); err != nil {
				return "", "", fmt.Errorf("%w: %s not found (srt needs it on Linux)", extension.ErrSandboxUnavailable, dep)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = []string{"PATH=" + p.wrapperPath(bin, o.Environ()), "HOME=" + os.Getenv("HOME")}
	out, err := cmd.Output()
	m := versionPattern.FindStringSubmatch(string(out))
	if err != nil || m == nil {
		return "", "", fmt.Errorf("%w: %s --version failed", extension.ErrSandboxUnavailable, bin)
	}
	if compareVersions(m[0], MinVersion) < 0 {
		return "", "", fmt.Errorf("%w: srt %s is older than %s", extension.ErrSandboxUnavailable, m[0], MinVersion)
	}
	return bin, m[0], nil
}

// preflight runs /usr/bin/true under the real policy, so a policy srt
// can't apply, or a platform where it would degrade, runs nothing.
func (p *Provider) preflight(ctx context.Context, bin, file, dir, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--settings", file, "--", "/usr/bin/true")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + path, "HOME=" + os.Getenv("HOME")}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: srt could not apply the policy: %s", extension.ErrSandboxUnavailable, firstLine(stderr.String(), err))
	}
	if degraded(stderr.String()) {
		return fmt.Errorf("%w: srt would run degraded: %s", extension.ErrSandboxUnavailable, firstLine(stderr.String(), nil))
	}
	return nil
}

// degraded reports srt warning that part of the policy isn't enforced
// (seccomp missing leaves unix sockets open).
func degraded(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "seccomp") || strings.Contains(low, "not restricted") || strings.Contains(low, "warning")
}

func firstLine(s string, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if line == "" && err != nil {
		line = err.Error()
	}
	if len(line) > 300 {
		line = line[:300]
	}
	return line
}

// Adopt proves the recovered segment's process tree is the sandbox it was
// launched in: the wrapper runs nothing but the recorded root, which is
// the srt process started with this plan's policy file after the plan was
// recorded; on Linux its sandboxed descendants are also in namespaces of
// their own.
func (p *Provider) Adopt(_ context.Context, plan extension.SandboxPlan, parent, root int) error {
	var rec record
	if json.Unmarshal(plan.Data.Data, &rec) != nil || rec.File == "" {
		return errors.New("unreadable srt plan")
	}
	procs, err := processes()
	if err != nil {
		return err
	}
	byPID := map[int]process{}
	for _, pr := range procs {
		byPID[pr.PID] = pr
	}
	for _, pr := range procs {
		if pr.PPID == parent && pr.PID != root {
			return fmt.Errorf("the wrapper runs process %d outside the sandbox", pr.PID)
		}
	}
	if root == 0 {
		return nil // nothing started, or it has exited: nothing runs outside
	}
	r, ok := byPID[root]
	if !ok {
		return nil // the sandbox has exited; the wrapper only records its exit
	}
	if r.PPID != parent {
		return fmt.Errorf("process %d is not the sandbox the wrapper started", root)
	}
	if !strings.Contains(r.Args, "--settings "+rec.File+" --") {
		return fmt.Errorf("process %d does not run the recorded policy", root)
	}
	info, err := os.Stat(rec.File)
	if err != nil {
		return fmt.Errorf("the recorded policy is gone: %w", err)
	}
	if start, err := startTime(root); err != nil {
		return err
	} else if start.Before(info.ModTime().Add(-2 * time.Second)) {
		return fmt.Errorf("process %d started before the plan", root)
	}
	if p.o.GOOS == "linux" {
		return namespaced(root, procs)
	}
	return nil
}

func writeReadOnly(path string, data []byte) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o400); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hostAllowed(h string, allow []string) bool {
	host := h
	if i := strings.LastIndex(h, ":"); i > 0 {
		host = h[:i]
	}
	for _, a := range allow {
		if a == host || strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
	}
	return false
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func unique(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
