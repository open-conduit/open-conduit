// Package srt is a SPIKE (#161) sandbox provider: it wraps a launch in
// Anthropic's sandbox-runtime (`srt`), Seatbelt on macOS and bubblewrap on
// Linux. Evidence for B09, not for merge.
package srt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// PolicySchema identifies the effective policy a launch records.
const PolicySchema = "open-conduit.sandbox.srt.policy.v1"

// Options are host-trusted, never from binding config.
type Options struct {
	Binary    string   // srt; resolved on PATH when empty
	Protect   []string // always read-denied (the daemon's state dir); the attempt's own dirs are re-allowed
	Toolbelt  string   // the toolbelt's unix socket path
	GOOS      string   // for tests; runtime.GOOS when empty
	LookPath  func(string) (string, error)
	Preflight bool // run `srt -- true` under the effective policy before launch
}

// Policy is the operator's ceiling, from the binding's sandbox config. It is
// also exactly what is granted: needs never widen it.
// Placeholders: {workspace} (the launch dir), {run} (the segment's run dir),
// {toolbelt} (the toolbelt socket). ~ is the home directory.
type Policy struct {
	Network struct {
		Allow []string `json:"allow"`
	} `json:"network"`
	Read struct {
		Deny  []string `json:"deny"`
		Allow []string `json:"allow"`
	} `json:"read"`
	Write   []string `json:"write"`
	Sockets []string `json:"sockets"`
	// UnixSockets "all" accepts that Linux can't allow sockets by path
	// (seccomp blocks socket(AF_UNIX) wholesale or not at all); without it a
	// socket need on Linux is unenforceable and the launch fails closed.
	UnixSockets string `json:"unix_sockets"`
}

type Provider struct{ o Options }

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
	return &Provider{o: o}
}

// Broker binds the operator's policy to one attempt.
func (p *Provider) Broker(config json.RawMessage) (extension.SandboxBroker, error) {
	var pol Policy
	if len(config) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(config)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&pol); err != nil {
			return nil, fmt.Errorf("%w: invalid sandbox config: %v", extension.ErrSandboxUnavailable, err)
		}
	}
	return &broker{p: p, pol: pol}, nil
}

type broker struct {
	p   *Provider
	pol Policy
}

// settings is srt's own settings file.
type settings struct {
	Network struct {
		AllowedDomains      []string `json:"allowedDomains"`
		DeniedDomains       []string `json:"deniedDomains"`
		AllowUnixSockets    []string `json:"allowUnixSockets,omitempty"`
		AllowAllUnixSockets bool     `json:"allowAllUnixSockets,omitempty"`
		AllowLocalBinding   bool     `json:"allowLocalBinding"`
	} `json:"network"`
	Filesystem struct {
		DenyRead   []string `json:"denyRead"`
		AllowRead  []string `json:"allowRead"`
		AllowWrite []string `json:"allowWrite"`
		DenyWrite  []string `json:"denyWrite"`
	} `json:"filesystem"`
}

func (b *broker) Wrap(ctx context.Context, r extension.SandboxRequest) (extension.SandboxLaunch, error) {
	o := b.p.o
	if o.GOOS != "darwin" && o.GOOS != "linux" {
		return extension.SandboxLaunch{}, fmt.Errorf("%w: srt does not support %s", extension.ErrSandboxUnavailable, o.GOOS)
	}
	bin, err := o.LookPath(o.Binary)
	if err != nil {
		return extension.SandboxLaunch{}, fmt.Errorf("%w: %s not found", extension.ErrSandboxUnavailable, o.Binary)
	}
	if o.GOOS == "linux" {
		for _, dep := range []string{"bwrap", "socat", "rg"} {
			if _, err := o.LookPath(dep); err != nil {
				return extension.SandboxLaunch{}, fmt.Errorf("%w: %s not found", extension.ErrSandboxUnavailable, dep)
			}
		}
	}
	home, _ := os.UserHomeDir()
	expand := func(list []string) []string {
		var out []string
		for _, s := range list {
			s = strings.NewReplacer("{workspace}", r.Dir, "{run}", r.RunDir, "{toolbelt}", o.Toolbelt).Replace(s)
			if s == "~" || strings.HasPrefix(s, "~/") {
				s = home + s[1:]
			}
			if s != "" {
				out = append(out, canon(s))
			}
		}
		return out
	}
	write := expand(b.pol.Write)
	allowRead := expand(b.pol.Read.Allow)
	denyRead := append(expand(b.pol.Read.Deny), expand(o.Protect)...)
	sockets := expand(b.pol.Sockets)
	hosts := b.pol.Network.Allow
	// The segment's run dir is the executor's own private directory: reading
	// it never exposes another execution, so it is always re-allowed. The
	// policy file under it is never writable.
	// On Linux a read re-allow is bound after, and over, the write binds
	// beneath it, so the run dir can't be re-allowed wholesale there: only the
	// read needs under it are (they must be siblings of the write needs).
	if o.GOOS == "linux" {
		allowRead = append(allowRead, needsUnder(r.Needs.Read, canon(r.RunDir))...)
	} else {
		allowRead = append(allowRead, canon(r.RunDir))
	}
	policyDir := filepath.Join(r.RunDir, "sandbox")

	// Needs are checked against the policy; any need outside it fails the
	// launch. Nothing is granted that the policy doesn't already grant.
	var denied []string
	for _, w := range r.Needs.Write {
		c := canon(w)
		if !(under(c, write) || under(c, []string{canon(r.RunDir)}) && !under(c, []string{canon(policyDir)})) {
			denied = append(denied, "write "+w)
		}
	}
	for _, rd := range r.Needs.Read {
		c := canon(rd)
		if under(c, denyRead) && !under(c, allowRead) && !under(c, write) && !under(c, []string{canon(r.RunDir)}) {
			denied = append(denied, "read "+rd)
		}
	}
	for _, h := range r.Needs.Hosts {
		if !hostAllowed(h, hosts) {
			denied = append(denied, "host "+h)
		}
	}
	for _, s := range r.Needs.Sockets {
		if !under(canon(s), sockets) {
			denied = append(denied, "socket "+s)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return extension.SandboxLaunch{}, fmt.Errorf("%w: outside the policy: %s", extension.ErrSandboxDenied, strings.Join(denied, ", "))
	}

	var s settings
	s.Network.AllowedDomains = nonNil(hosts)
	s.Network.DeniedDomains = []string{}
	s.Filesystem.DenyRead = nonNil(denyRead)
	// A write grant is readable too: a granted workspace under a protected
	// root (the state dir) is otherwise write-only.
	// Write needs under the run dir are listed for read too: on Linux a read
	// re-allow of the run dir is bound after, and over, the write binds below it.
	s.Filesystem.AllowRead = nonNil(append(append(allowRead, write...), needsUnder(r.Needs.Write, canon(r.RunDir))...))
	s.Filesystem.AllowWrite = nonNil(append(write, needsUnder(r.Needs.Write, canon(r.RunDir))...))
	s.Filesystem.DenyWrite = []string{canon(policyDir)}
	if len(sockets) > 0 {
		if o.GOOS == "linux" {
			if b.pol.UnixSockets != "all" {
				return extension.SandboxLaunch{}, fmt.Errorf("%w: Linux can't allow unix sockets by path; set unix_sockets to \"all\" to accept every socket the filesystem policy leaves reachable", extension.ErrSandboxUnavailable)
			}
			s.Network.AllowAllUnixSockets = true
			// A socket under a read-denied dir is hidden by its tmpfs; bind it back.
			s.Filesystem.AllowRead = append(s.Filesystem.AllowRead, sockets...)
			// Every socket the policy didn't list that we know of is hidden.
			for _, known := range []string{"/var/run/docker.sock", "/run/docker.sock", home + "/.docker/run/docker.sock"} {
				if _, err := os.Stat(known); err == nil {
					s.Filesystem.DenyRead = append(s.Filesystem.DenyRead, known)
				}
			}
		} else {
			s.Network.AllowUnixSockets = sockets
		}
	}
	doc, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return extension.SandboxLaunch{}, err
	}
	if err = os.MkdirAll(policyDir, 0o700); err != nil {
		return extension.SandboxLaunch{}, err
	}
	file := filepath.Join(policyDir, "srt.json")
	if err = os.WriteFile(file, append(doc, '\n'), 0o400); err != nil && !os.IsPermission(err) {
		return extension.SandboxLaunch{}, err
	}
	if o.Preflight {
		cmd := exec.CommandContext(ctx, bin, "-s", file, "--", "/usr/bin/true")
		cmd.Dir = r.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return extension.SandboxLaunch{}, fmt.Errorf("%w: srt could not apply the policy: %s", extension.ErrSandboxUnavailable, strings.TrimSpace(string(out)))
		}
	}
	argv := append([]string{bin, "-s", file, "--"}, r.Argv...)
	return extension.SandboxLaunch{
		Argv:    argv,
		Env:     r.Env,
		Summary: fmt.Sprintf("srt: write %d paths, %d hosts, %d sockets", len(s.Filesystem.AllowWrite), len(hosts), len(sockets)),
		Policy:  domain.Payload{Schema: PolicySchema, Data: doc},
	}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func needsUnder(needs []string, root string) []string {
	var out []string
	for _, n := range needs {
		if c := canon(n); under(c, []string{root}) {
			out = append(out, c)
		}
	}
	return out
}

// canon resolves symlinks in the longest existing prefix (macOS /var is
// /private/var), so containment compares real paths.
func canon(p string) string {
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

func under(p string, roots []string) bool {
	for _, r := range roots {
		if ok, _ := filepath.Match(r, p); ok && strings.ContainsAny(r, "*?[") {
			return true
		}
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func hostAllowed(h string, allow []string) bool {
	host := h
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		host = h[:i]
	}
	for _, a := range allow {
		if a == h || a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
	}
	return false
}
