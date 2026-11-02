package srt_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/srt"
)

func TestParseConfig(t *testing.T) {
	ok := `{"write":["{workspace}","~/.npm","/tmp/x"],"read":{"deny":["~/.ssh"],"allow":["~/.ssh/known_hosts"]},"network":{"allow":["github.com","*.githubusercontent.com"]},"sockets":["{toolbelt}"],"unix_sockets":"all"}`
	if _, err := srt.ParseConfig(json.RawMessage(ok)); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for _, bad := range []string{
		`{"write":["relative/path"]}`,
		`{"write":["/tmp/*"]}`,
		`{"network":{"allow":["https://github.com"]}}`,
		`{"network":{"allow":"*"}}`,
		`{"unix_sockets":"some"}`,
		`{"extra":true}`,
		`{"read":{"deny":["{elsewhere}/x"]}}`,
	} {
		if err := srt.ValidateConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// lab is a state directory with an attempt's paths, and a stand-in srt that
// reports a version and runs its command.
type lab struct {
	state, ws, run, records, sock, bin string
}

func newLab(t *testing.T, srtScript string) *lab {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	l := &lab{state: filepath.Join(root, "state")}
	l.ws, l.run = filepath.Join(l.state, "ws"), filepath.Join(l.state, "executors", "shell", "runs", "k", "n")
	l.records, l.sock = filepath.Join(l.state, "runs", "k", "sandbox", "r"), filepath.Join(l.state, "runs", "k", "toolbelt.sock")
	for _, d := range []string{l.ws, l.run, l.records} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if srtScript == "" {
		srtScript = `[ "$1" = --version ] && { echo 0.0.81; exit 0; }; exit 0`
	}
	l.bin = filepath.Join(root, "srt")
	if err := os.WriteFile(l.bin, []byte("#!/bin/sh\n"+srtScript+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *lab) provider(goos string) *srt.Provider {
	return srt.New(srt.Options{Binary: l.bin, GOOS: goos, LookPath: func(s string) (string, error) {
		if s == l.bin {
			return l.bin, nil
		}
		return exec.LookPath(s)
	}})
}

func (l *lab) policy(cfg string) extension.SandboxPolicy {
	return extension.SandboxPolicy{Config: domain.Payload{Data: json.RawMessage(cfg)}, Paths: extension.SandboxPaths{Workspace: l.ws, Run: l.run, Toolbelt: l.sock, Records: l.records}}
}

// needs are what the host would pass: the executor's, and the protected
// state directory with this attempt's exceptions beneath it.
func (l *lab) needs() extension.SandboxNeeds {
	return extension.SandboxNeeds{Write: []string{l.ws, filepath.Join(l.run, "outputs")}, Read: []string{filepath.Dir(l.sock)},
		ReadOnly: []string{filepath.Join(l.ws, ".git", "config")}, Deny: []string{l.state}, Hosts: []string{"api.anthropic.com"}, Sockets: []string{l.sock}}
}

func (l *lab) launch() extension.SandboxLaunch {
	return extension.SandboxLaunch{Argv: []string{"/bin/echo", "hi"}, Dir: l.ws, Env: []string{"PATH=/usr/bin:/bin", "TMPDIR=/long/child/tmp", "SECRET=s"}, WrapperEnv: []string{"TMPDIR=/tmp"}, RunDir: l.run}
}

const policy = `{"write":["{workspace}"],"read":{"deny":["~/.ssh"]},"network":{"allow":["api.anthropic.com"]},"sockets":["{toolbelt}"],"unix_sockets":"all"}`

type settings struct {
	Network struct {
		AllowedDomains      []string `json:"allowedDomains"`
		StrictAllowlist     bool     `json:"strictAllowlist"`
		AllowUnixSockets    []string `json:"allowUnixSockets"`
		AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	} `json:"network"`
	Filesystem struct {
		DenyRead, AllowRead, AllowWrite, DenyWrite []struct {
			Path    string `json:"path"`
			Literal bool   `json:"literal"`
		}
	} `json:"filesystem"`
}

func paths(list []struct {
	Path    string `json:"path"`
	Literal bool   `json:"literal"`
}) []string {
	var out []string
	for _, p := range list {
		if !p.Literal {
			out = append(out, "pattern:"+p.Path)
		}
		out = append(out, p.Path)
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestPlan(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			if goos == "linux" {
				for _, dep := range []string{"bwrap", "socat", "rg"} {
					if _, err := exec.LookPath(dep); err != nil {
						t.Skip("the Linux plan looks for bwrap, socat and rg")
					}
				}
			}
			l := newLab(t, "")
			plan, err := l.provider(goos).Plan(context.Background(), extension.ResourceOwner{}, l.policy(policy), l.needs(), l.launch())
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(l.records, "srt.json"))
			if err != nil {
				t.Fatal(err)
			}
			var s settings
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			deny, allow, write, ro := paths(s.Filesystem.DenyRead), paths(s.Filesystem.AllowRead), paths(s.Filesystem.AllowWrite), paths(s.Filesystem.DenyWrite)
			home, _ := os.UserHomeDir()
			if !has(deny, l.state) || !has(deny, extension.CanonicalPath(home+"/.ssh")) {
				t.Errorf("denyRead %q, want the state dir and the policy's denies", deny)
			}
			for _, p := range []string{l.ws, l.run, filepath.Dir(l.sock)} {
				if !has(allow, p) {
					t.Errorf("allowRead %q lacks the exception %s", allow, p)
				}
			}
			if !has(write, l.ws) || !has(write, l.run) || len(write) != 2 {
				t.Errorf("allowWrite %q, want exactly the workspace and the run dir", write)
			}
			if !has(ro, filepath.Join(l.ws, ".git", "config")) {
				t.Errorf("denyWrite %q, want the read-only needs", ro)
			}
			if !s.Network.StrictAllowlist || !has(s.Network.AllowedDomains, "api.anthropic.com") || len(s.Network.AllowedDomains) != 1 {
				t.Errorf("network %+v", s.Network)
			}
			if goos == "linux" {
				if !s.Network.AllowAllUnixSockets || plan.Signals != extension.SandboxKill {
					t.Errorf("linux: sockets %+v signals %s", s.Network, plan.Signals)
				}
			} else if !has(s.Network.AllowUnixSockets, l.sock) || plan.Signals != extension.SandboxTerm {
				t.Errorf("darwin: sockets %+v signals %s", s.Network, plan.Signals)
			}
			argv := strings.Join(plan.Argv, " ")
			if !strings.Contains(argv, "TMPDIR=/tmp "+l.bin+" --settings ") || !strings.Contains(argv, "-- /usr/bin/env PATH=/usr/bin:/bin TMPDIR=/long/child/tmp /bin/echo hi") {
				t.Errorf("argv %q, want srt's own TMPDIR and the child's restored inside", argv)
			}
			if strings.Contains(argv, "SECRET") || !has(plan.Env, "SECRET=s") {
				t.Errorf("the child's environment goes in the environment, never the argv: %q %q", argv, plan.Env)
			}
			if plan.Data.Schema != srt.PolicySchema || !strings.Contains(plan.Summary, "srt 0.0.81") {
				t.Errorf("data %s summary %q", plan.Data.Schema, plan.Summary)
			}
		})
	}
}

func TestPlanRefuses(t *testing.T) {
	cases := []struct {
		name, policy, goos, script string
		needs                      func(l *lab, n *extension.SandboxNeeds)
		want                       error
	}{
		{"workspace_not_granted", `{"sockets":["{toolbelt}"]}`, "darwin", "", nil, extension.ErrSandboxDenied},
		{"host_not_granted", `{"write":["{workspace}"],"sockets":["{toolbelt}"]}`, "darwin", "", nil, extension.ErrSandboxDenied},
		{"socket_not_granted", `{"write":["{workspace}"],"network":{"allow":["api.anthropic.com"]}}`, "darwin", "", nil, extension.ErrSandboxDenied},
		{"read_denied_by_policy", `{"write":["{workspace}"],"read":{"deny":["{toolbelt}/.."]},"network":{"allow":["api.anthropic.com"]},"sockets":["{toolbelt}"]}`, "darwin", "", nil, extension.ErrSandboxDenied},
		{"grant_holds_the_state", `{"write":["{workspace}/../.."],"network":{"allow":["api.anthropic.com"]},"sockets":["{toolbelt}"]}`, "darwin", "", nil, extension.ErrSandboxDenied},
		{"grant_inside_the_state", `{"write":["{workspace}","{toolbelt}/.."],"network":{"allow":["api.anthropic.com"]},"sockets":["{toolbelt}"]}`, "darwin", "", func(l *lab, n *extension.SandboxNeeds) {
			n.Read = nil
		}, extension.ErrSandboxDenied},
		{"read_allow_inside_the_state", `{"write":["{workspace}"],"read":{"allow":["{run}/../.."]},"network":{"allow":["api.anthropic.com"]},"sockets":["{toolbelt}"]}`, "darwin", "", nil, extension.ErrSandboxDenied},
		{"read_only_holding_a_grant", policy, "darwin", "", func(l *lab, n *extension.SandboxNeeds) {
			n.ReadOnly = append(n.ReadOnly, filepath.Dir(l.ws))
		}, extension.ErrSandboxUnavailable},
		{"linux_socket_by_path", strings.Replace(policy, `,"unix_sockets":"all"`, "", 1), "linux", "", nil, extension.ErrSandboxUnavailable},
		{"unsupported_platform", policy, "windows", "", nil, extension.ErrSandboxUnavailable},
		{"old_srt", policy, "darwin", `echo 0.0.12`, nil, extension.ErrSandboxUnavailable},
		{"preflight_fails", policy, "darwin", `[ "$1" = --version ] && { echo 0.0.81; exit 0; }; exit 1`, nil, extension.ErrSandboxUnavailable},
		{"degraded", policy, "darwin", `[ "$1" = --version ] && { echo 0.0.81; exit 0; }; echo "seccomp not available - unix socket access not restricted" >&2`, nil, extension.ErrSandboxUnavailable},
		{"bad_config", `{"write":"nope"}`, "darwin", "", nil, extension.ErrSandboxUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t, c.script)
			n := l.needs()
			if c.needs != nil {
				c.needs(l, &n)
			}
			p := l.provider(c.goos)
			if c.goos == "linux" {
				p = srt.New(srt.Options{Binary: l.bin, GOOS: "linux", LookPath: func(s string) (string, error) {
					if s == l.bin {
						return l.bin, nil
					}
					return "/bin/sh", nil
				}})
			}
			if _, err := p.Plan(context.Background(), extension.ResourceOwner{}, l.policy(c.policy), n, l.launch()); !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
		})
	}
}

// A grant may hold a protected path with nothing writable beneath it (the
// fallback toolbelt socket root under /tmp): it is denied inside the grant.
func TestPlanDeniesInsideAGrant(t *testing.T) {
	l := newLab(t, "")
	n := l.needs()
	short := filepath.Join(filepath.Dir(l.state), "short")
	n.Deny = append(n.Deny, short)
	pol := strings.Replace(policy, `"write":["{workspace}"]`, `"write":["{workspace}","`+filepath.Dir(l.state)+`/other","`+filepath.Dir(short)+`"]`, 1)
	// The broad grant holds the state directory too, which has writable
	// paths beneath it: refused.
	if _, err := l.provider("darwin").Plan(context.Background(), extension.ResourceOwner{}, l.policy(pol), n, l.launch()); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Fatalf("a grant holding the state directory: %v, want ErrSandboxDenied", err)
	}
	n.Deny = []string{short}
	if _, err := l.provider("darwin").Plan(context.Background(), extension.ResourceOwner{}, l.policy(pol), n, l.launch()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(l.records, "srt.json"))
	var s settings
	_ = json.Unmarshal(raw, &s)
	if !has(paths(s.Filesystem.DenyWrite), short) || !has(paths(s.Filesystem.DenyRead), short) {
		t.Fatalf("the protected path inside the grant is not denied: %s", raw)
	}
}

func TestAdoptRefusesAnotherProcess(t *testing.T) {
	l := newLab(t, "")
	plan, err := l.provider("darwin").Plan(context.Background(), extension.ResourceOwner{}, l.policy(policy), l.needs(), l.launch())
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper (parent) runs nothing: nothing runs outside a sandbox.
	parent := exec.Command("sleep", "30")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer parent.Process.Kill()
	p := l.provider("")
	if err := p.Adopt(context.Background(), plan, parent.Process.Pid, 0); err != nil {
		t.Fatalf("a wrapper with nothing running: %v", err)
	}
	// A root that isn't the wrapper's child.
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill()
	if err := p.Adopt(context.Background(), plan, parent.Process.Pid, other.Process.Pid); err == nil {
		t.Fatal("adopted a root that is not the wrapper's child")
	}
	// A child of the wrapper that isn't the recorded root.
	wrapper := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	defer wrapper.Process.Kill()
	if err := p.Adopt(context.Background(), plan, wrapper.Process.Pid, other.Process.Pid); err == nil {
		t.Fatal("adopted a wrapper running a process outside the sandbox")
	}
}

// The credential paths are denied by default, the keychain on macOS, and on
// Linux, where every unix socket is allowed, the operator's runtime
// directory and ssh agent; a policy lifts one through read.allow or sockets.
func TestPlanDefaultDenies(t *testing.T) {
	l := newLab(t, "")
	home := filepath.Join(filepath.Dir(l.state), "home")
	xdg, agent := filepath.Join(filepath.Dir(l.state), "xdg"), filepath.Join(filepath.Dir(l.state), "agent.sock")
	for _, d := range []string{home + "/.config", xdg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(agent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	plan := func(goos, pol string) settings {
		t.Helper()
		p := srt.New(srt.Options{Binary: l.bin, GOOS: goos, Environ: func() []string {
			return []string{"XDG_RUNTIME_DIR=" + xdg, "SSH_AUTH_SOCK=" + agent, "PATH=/usr/bin:/bin"}
		}, LookPath: func(s string) (string, error) {
			if s == l.bin {
				return l.bin, nil
			}
			return "/bin/sh", nil
		}})
		if _, err := p.Plan(context.Background(), extension.ResourceOwner{}, l.policy(pol), l.needs(), l.launch()); err != nil {
			t.Fatalf("%s %s: %v", goos, pol, err)
		}
		raw, _ := os.ReadFile(filepath.Join(l.records, "srt.json"))
		var s settings
		_ = json.Unmarshal(raw, &s)
		return s
	}
	creds := []string{"/.ssh", "/.aws", "/.config/gh", "/.docker/config.json", "/.netrc", "/.gnupg"}
	for _, goos := range []string{"darwin", "linux"} {
		deny := paths(plan(goos, policy).Filesystem.DenyRead)
		for _, c := range creds {
			if !has(deny, home+c) {
				t.Errorf("%s: denyRead %q lacks %s", goos, deny, home+c)
			}
		}
		if has(deny, home+"/Library/Keychains") != (goos == "darwin") {
			t.Errorf("%s: denyRead %q, want the keychain denied on macOS only", goos, deny)
		}
		// macOS allows sockets by path; Linux hides those it can't.
		if has(deny, xdg) != (goos == "linux") || has(deny, agent) != (goos == "linux") {
			t.Errorf("%s: denyRead %q, want the runtime dir and agent hidden on Linux only", goos, deny)
		}
	}
	// A policy lifts a default by listing it, or a directory holding it.
	lifted := strings.Replace(policy, `"read":{"deny":["~/.ssh"]}`, `"read":{"allow":["~/.aws","~/.config"]}`, 1)
	lifted = strings.Replace(lifted, `"sockets":["{toolbelt}"]`, `"sockets":["{toolbelt}","`+agent+`"]`, 1)
	s := plan("linux", lifted)
	deny, allow := paths(s.Filesystem.DenyRead), paths(s.Filesystem.AllowRead)
	if has(deny, home+"/.aws") || has(deny, home+"/.config/gh") || has(deny, agent) || !has(deny, home+"/.ssh") || !has(deny, xdg) {
		t.Errorf("denyRead %q, want ~/.aws, ~/.config/gh and the listed agent lifted, the rest kept", deny)
	}
	if has(allow, home+"/.aws") {
		t.Errorf("allowRead %q lists a lifted path that nothing denies", allow)
	}
	// A socket inside the runtime directory is an exception beneath it.
	inside := filepath.Join(xdg, "tool.sock")
	s = plan("linux", strings.Replace(policy, `"sockets":["{toolbelt}"]`, `"sockets":["{toolbelt}","`+inside+`"]`, 1))
	if !has(paths(s.Filesystem.DenyRead), xdg) || !has(paths(s.Filesystem.AllowRead), inside) {
		t.Errorf("a socket in the runtime dir: deny %q allow %q", paths(s.Filesystem.DenyRead), paths(s.Filesystem.AllowRead))
	}
	// A default inside a write grant stays unreadable and unwritable there.
	s = plan("darwin", strings.Replace(policy, `"write":["{workspace}"]`, `"write":["{workspace}","~/.config"]`, 1))
	if !has(paths(s.Filesystem.DenyRead), home+"/.config/gh") || !has(paths(s.Filesystem.DenyWrite), home+"/.config/gh") {
		t.Errorf("~/.config/gh inside the ~/.config grant: deny %q denyWrite %q", paths(s.Filesystem.DenyRead), paths(s.Filesystem.DenyWrite))
	}
	// A need beneath a default is refused, as one beneath the policy's own denies.
	n := l.needs()
	n.Read = append(n.Read, home+"/.aws/config")
	p := srt.New(srt.Options{Binary: l.bin, GOOS: "darwin"})
	if _, err := p.Plan(context.Background(), extension.ResourceOwner{}, l.policy(policy), n, l.launch()); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Errorf("a need beneath ~/.aws: %v, want ErrSandboxDenied", err)
	}
}
