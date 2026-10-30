package integration_test

// Real isolation tests for process sandboxes (B09): the shell and Claude
// Code executors launched through the host's sandbox binding and the srt
// provider, probed from inside. They need srt (and on Linux bubblewrap and
// socat); without it they skip, unless OPEN_CONDUIT_SANDBOX_TESTS=1 (the
// sandbox CI jobs), where a missing srt fails them. They call no real
// service: network checks are against a local listener and srt's proxy,
// which refuses a host before it resolves it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	gitws "github.com/open-conduit/open-conduit/extensions/git"
	"github.com/open-conduit/open-conduit/extensions/srt"
)

func srtBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("OPEN_CONDUIT_SRT")
	if bin == "" {
		bin, _ = exec.LookPath("srt")
	}
	if bin == "" {
		if os.Getenv("OPEN_CONDUIT_SANDBOX_TESTS") == "1" {
			t.Fatal("OPEN_CONDUIT_SANDBOX_TESTS=1, but srt is not installed")
		}
		t.Skip("srt is not installed; the sandbox CI jobs run these tests")
	}
	if runtime.GOOS == "linux" {
		for _, dep := range []string{"bwrap", "socat", "rg"} {
			if _, err := exec.LookPath(dep); err != nil {
				if os.Getenv("OPEN_CONDUIT_SANDBOX_TESTS") == "1" {
					t.Fatalf("OPEN_CONDUIT_SANDBOX_TESTS=1, but %s is not installed", dep)
				}
				t.Skipf("%s is not installed", dep)
			}
		}
	}
	return bin
}

// sandboxLab is a daemon's state directory, a home and a workspace laid out
// as the daemon lays them out, with something to find in each place a
// sandboxed process must not reach.
type sandboxLab struct {
	t                                      *testing.T
	srt                                    string
	root, home, state, config, ws, outside string
	claude                                 string
	run, other, sock, otherSock, loopback  string
	execState                              string
	pins                                   []extension.SandboxPlan
	mu                                     sync.Mutex
	listeners                              []net.Listener
}

func newSandboxLab(t *testing.T, executor string) *sandboxLab {
	t.Helper()
	bin := srtBinary(t)
	// A short root, so every socket path fits the platform's limit.
	root, err := os.MkdirTemp("/tmp", "ocsb")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", root).Run(); _ = os.RemoveAll(root) })
	l := &sandboxLab{t: t, srt: bin, root: root, home: filepath.Join(root, "home"), state: filepath.Join(root, "state"),
		config: filepath.Join(root, "config.json"), outside: filepath.Join(root, "outside")}
	l.ws = filepath.Join(l.state, "resources", "git", "workspaces", "mine")
	l.run = filepath.Join(l.state, "runs", "own")
	l.other = filepath.Join(l.state, "runs", "other")
	l.execState = filepath.Join(l.state, "executors", executor)
	for _, d := range []string{l.home + "/.ssh", l.home + "/.docker/run", l.state + "/private", l.run + "/token", l.other + "/token",
		l.state + "/resources/git/workspaces/theirs", l.outside, l.ws, l.execState} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		l.home + "/.ssh/id_test":                            "ssh-secret",
		l.state + "/private/operator.token":                 "operator-secret",
		l.other + "/token/token":                            "other-secret",
		l.run + "/token/token":                              "own-token",
		l.state + "/resources/git/workspaces/theirs/secret": "their-secret",
		l.config: "config-secret",
	} {
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", l.home)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", l.ws}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	_ = os.MkdirAll(filepath.Join(l.ws, ".git", "hooks"), 0o700)
	_ = os.MkdirAll(filepath.Join(l.ws, ".git", "info"), 0o700)
	// The operator's own claude config, which a sandboxed claude must not
	// reach, and the binding's own: srt on Linux can grant only a path that
	// exists.
	l.claude = filepath.Join(l.home, ".claude-sandboxed")
	_ = os.MkdirAll(l.claude, 0o700)
	_ = os.MkdirAll(filepath.Join(l.home, ".claude"), 0o700)
	for path, body := range map[string]string{".claude/settings.json": "{}", ".claude.json": "{}", ".claude/.credentials.json": "claude-secret"} {
		_ = os.WriteFile(filepath.Join(l.home, path), []byte(body+"\n"), 0o600)
	}
	_ = os.Symlink(l.home+"/.ssh", filepath.Join(l.ws, "link-ssh"))
	_ = os.Symlink(l.state, filepath.Join(l.ws, "link-state"))
	l.sock = filepath.Join(l.run, "toolbelt.sock")
	l.otherSock = filepath.Join(l.other, "toolbelt.sock")
	l.serve("unix", l.sock, "toolbelt-ok")
	l.serve("unix", l.otherSock, "other-toolbelt")
	l.serve("unix", filepath.Join(l.state, "management.sock"), "management")
	l.serve("unix", filepath.Join(l.home, ".docker", "run", "docker.sock"), "docker")
	l.loopback = l.serve("tcp", "127.0.0.1:0", "loopback")
	return l
}

func (l *sandboxLab) serve(network, addr, body string) string {
	ln, err := net.Listen(network, addr)
	if err != nil {
		l.t.Fatal(err)
	}
	l.listeners = append(l.listeners, ln)
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	}()
	l.t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

const sandboxPolicy = `{"write":["{workspace}","~/.claude-sandboxed"],"read":{"deny":["~/.ssh"]},"network":{"allow":["allowed.invalid","api.anthropic.com"]},"sockets":["{toolbelt}"],"unix_sockets":"all"}`

// binding is the host's sandbox for one attempt, as bootstrap binds it.
func (l *sandboxLab) binding(policy string, pinned string, o srt.Options) *extension.BoundSandbox {
	if o.Binary == "" {
		o.Binary = l.srt
	}
	return extension.BindSandbox(extension.SandboxBinding{
		Provider:  srt.New(o),
		Owner:     extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: "exec"},
		Config:    domain.Payload{Schema: "core.config.sandbox.v1", Data: json.RawMessage(policy)},
		Protected: []string{l.state, l.config},
		Shared:    []string{l.run},
		RunRoot:   filepath.Join(l.execState, "runs"),
		Toolbelt:  l.sock,
		Records:   filepath.Join(l.run, "sandbox"),
		Pin: func(p extension.SandboxPlan) error {
			l.mu.Lock()
			l.pins = append(l.pins, p)
			l.mu.Unlock()
			return nil
		},
		Pinned: pinned,
	})
}

func (l *sandboxLab) pinned() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pins) == 0 {
		l.t.Fatal("no plan was pinned")
	}
	return l.pins[len(l.pins)-1].Digest
}

// workspace is the binding's resource: the clone, with the git provider's
// own sandbox needs for it.
type labWorkspace struct {
	dir       string
	sandboxed bool
}

func (w labWorkspace) Acquire(_ context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	if r.Sandboxed != w.sandboxed {
		return extension.Resource{}, errors.New("acquired without the sandbox flag")
	}
	return extension.Resource{Kind: "workspace", Ref: "lease", Dir: w.dir, Sandbox: gitws.CloneSandboxNeeds(w.dir)}, nil
}
func (w labWorkspace) Lookup(ctx context.Context, _ string) (extension.Resource, error) {
	return w.Acquire(ctx, extension.ResourceRequest{Sandboxed: true})
}

func (l *sandboxLab) env() map[string]string {
	return map[string]string{"LAB_STATE": l.state, "LAB_CONFIG": l.config, "LAB_OUTSIDE": l.outside, "LAB_RUN": l.run,
		"LAB_LOOPBACK": l.loopback, "LAB_OTHER_SOCK": l.otherSock, "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}
}

// claudeEnv is a claude-code binding's env: the lab's, with a config
// directory of its own.
func (l *sandboxLab) claudeEnv() map[string]string {
	env := l.env()
	env["CLAUDE_CONFIG_DIR"] = l.claude
	return env
}

// sandboxProbes print "name: ALLOWED|DENIED" for each attempt from inside,
// and "name=<status>" for the network checks through srt's proxy.
const sandboxProbes = `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi; }
code() { curl -s -o /dev/null -w '%{http_code}' -m 10 "$@" 2>/dev/null; }
read() { [ "$(cat "$1" 2>/dev/null)" = "$2" ]; }
t read_ssh read "$HOME/.ssh/id_test" ssh-secret
t read_other_workspace read "$LAB_STATE/resources/git/workspaces/theirs/secret" their-secret
t read_operator_token read "$LAB_STATE/private/operator.token" operator-secret
t read_other_token read "$LAB_STATE/runs/other/token/token" other-secret
t read_config read "$LAB_CONFIG" config-secret
t read_own_token read "$LAB_RUN/token/token" own-token
t read_symlink_ssh read ./link-ssh/id_test ssh-secret
t read_symlink_state read ./link-state/private/operator.token operator-secret
t read_dotdot read ../../../../private/operator.token operator-secret
t read_proc_root read "/proc/self/root$LAB_STATE/private/operator.token" operator-secret
t read_case read "$(printf %s "$LAB_STATE/private/operator.token" | tr a-z A-Z)" operator-secret
t new_hardlink ln "$LAB_STATE/private/operator.token" ./hardlink
t write_workspace sh -c 'echo x > ./probe'
t git_commit sh -c 'git add probe && git -c user.name=t -c user.email=t@t commit -qm probe'
t write_outside sh -c 'echo x > "$LAB_OUTSIDE/probe"'
t write_home sh -c 'echo x > "$HOME/probe"'
t write_state sh -c 'echo x > "$LAB_STATE/probe"'
t write_records sh -c 'echo x > "$LAB_RUN/sandbox/probe"'
t write_git_config sh -c 'echo "[core]" >> .git/config'
t write_git_hook sh -c 'echo x > .git/hooks/post-commit'
t write_git_info sh -c 'echo x >> .git/info/exclude'
t rename_git mv .git .git-moved
t loopback_tcp curl -sf -m 3 --noproxy '*' "http://$LAB_LOOPBACK/"
t docker_socket curl -sf -m 3 --unix-socket "$HOME/.docker/run/docker.sock" http://x/
t management_socket curl -sf -m 3 --unix-socket "$LAB_STATE/management.sock" http://x/
t other_toolbelt curl -sf -m 3 --unix-socket "$LAB_OTHER_SOCK" http://x/
t own_toolbelt sh -c 'test "$(curl -sf -m 5 --unix-socket "$LAB_RUN/toolbelt.sock" http://x/)" = toolbelt-ok'
echo "net_disallowed=$(code http://blocked.invalid/)"
echo "net_allowed=$(code http://allowed.invalid/)"
`

// sandboxExpect is what the probes must report from inside a sandbox.
var sandboxExpect = map[string]string{
	"read_ssh": "DENIED", "read_other_workspace": "DENIED", "read_operator_token": "DENIED", "read_other_token": "DENIED",
	"read_config": "DENIED", "read_own_token": "ALLOWED", "read_symlink_ssh": "DENIED", "read_symlink_state": "DENIED",
	"read_dotdot": "DENIED", "read_proc_root": "DENIED", "read_case": "DENIED", "new_hardlink": "DENIED", "write_workspace": "ALLOWED", "git_commit": "ALLOWED",
	"loopback_tcp": "DENIED", "docker_socket": "DENIED", "management_socket": "DENIED", "other_toolbelt": "DENIED", "own_toolbelt": "ALLOWED",
}

func probeResults(out string) map[string]string {
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			got[k] = strings.TrimSpace(v)
		} else if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(k, "net_") {
			got[k] = strings.TrimSpace(v)
		}
	}
	return got
}

// checkIsolation asserts the probes and, for writes, the host side: a
// write into a denied region on Linux lands in a throwaway tmpfs, so only
// the host file says whether it got out.
func (l *sandboxLab) checkIsolation(t *testing.T, out string) {
	t.Helper()
	got := probeResults(out)
	for k, want := range sandboxExpect {
		if got[k] != want {
			t.Errorf("%s: %q from inside the sandbox, want %s\n%s", k, got[k], want, out)
		}
	}
	if got["net_disallowed"] != "403" {
		t.Errorf("a host outside the policy got %q from srt's proxy, want 403", got["net_disallowed"])
	}
	if got["net_allowed"] == "403" || got["net_allowed"] == "" {
		t.Errorf("a host in the policy got %q from srt's proxy, want it let through (not 403)", got["net_allowed"])
	}
	for _, p := range []string{l.outside + "/probe", l.home + "/probe", l.state + "/probe", l.run + "/sandbox/probe", l.ws + "/.git/hooks/post-commit", l.ws + "/.git-moved"} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written from inside the sandbox", p)
		}
	}
	if b, _ := os.ReadFile(l.ws + "/.git/config"); strings.Contains(string(b), "[core]\n[core]") || strings.HasSuffix(string(b), "[core]\n") {
		t.Errorf(".git/config was written from inside the sandbox:\n%s", b)
	}
	if b, _ := os.ReadFile(l.ws + "/.git/info/exclude"); strings.HasSuffix(string(b), "x\n") {
		t.Error(".git/info/exclude was written from inside the sandbox")
	}
	if _, err := os.Stat(l.ws + "/probe"); err != nil {
		t.Error("the workspace write did not reach the host")
	}
}

func (l *sandboxLab) shell(t *testing.T) *shell.Executor {
	t.Helper()
	e, err := shell.New(shell.Options{StateDir: l.execState, Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} },
		Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (l *sandboxLab) shellRequest(id domain.ID, command string, b *extension.BoundSandbox, saved *domain.Payload) extension.RunRequest {
	cfg, _ := json.Marshal(map[string]any{"command": command, "timeout_seconds": 120, "env": l.env()})
	r := extension.RunRequest{Work: domain.Work{ID: "work", ScopeID: "scope"}, Execution: domain.Execution{ID: id, Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Context: extension.ExecutionContext{Config: domain.Payload{Schema: "core.config.binding.v1", Data: cfg},
			ToolbeltEndpoint: "unix://" + l.sock, ToolbeltToken: "own-token", ToolbeltTokenFile: l.run + "/token/token"},
		PersistRecovery: func(p domain.Payload) error { *saved = p; return nil }}
	if b != nil {
		r.Context.Sandbox, r.Context.Resources = b, b.Resources(labWorkspace{l.ws, true})
	} else {
		r.Context.Resources = labWorkspace{l.ws, false}
	}
	return r
}

func waitResult(t *testing.T, h extension.RunHandle) extension.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type shellResult struct {
	Reason    string `json:"reason"`
	Interrupt string `json:"interrupt"`
	Log       string `json:"log"`
}

func TestSandboxShellIsolation(t *testing.T) {
	l := newSandboxLab(t, "shell")
	e := l.shell(t)
	var saved domain.Payload
	h, err := e.Run(context.Background(), l.shellRequest("exec", sandboxProbes+"cat ./probe-out 2>/dev/null; true", l.binding(sandboxPolicy, "", srt.Options{}), &saved))
	if err != nil {
		t.Fatal(err)
	}
	res := waitResult(t, h)
	out, _ := os.ReadFile(l.execOutput(t))
	if res.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("outcome %s %q\n%s", res.Outcome.Kind, res.Outcome.Summary, out)
	}
	l.checkIsolation(t, string(out))
	if t.Failed() {
		files, _ := filepath.Glob(filepath.Join(l.run, "sandbox", "*", "srt.json"))
		for _, f := range files {
			b, _ := os.ReadFile(f)
			t.Logf("%s:\n%s", f, b)
		}
	}
}

// TestSandboxUnsandboxedControl runs the same probes without a sandbox: they
// all reach what the sandbox denies, so the lab finds a real boundary.
func TestSandboxUnsandboxedControl(t *testing.T) {
	l := newSandboxLab(t, "shell")
	e := l.shell(t)
	var saved domain.Payload
	h, err := e.Run(context.Background(), l.shellRequest("exec", sandboxProbes, nil, &saved))
	if err != nil {
		t.Fatal(err)
	}
	waitResult(t, h)
	out, _ := os.ReadFile(l.execOutput(t))
	got := probeResults(string(out))
	keys := []string{"read_ssh", "read_other_workspace", "read_operator_token", "read_other_token", "read_config", "read_symlink_ssh", "read_dotdot", "management_socket", "loopback_tcp", "docker_socket"}
	if runtime.GOOS == "darwin" {
		keys = append(keys, "read_case") // APFS is case-insensitive by default
	}
	for _, k := range keys {
		if got[k] != "ALLOWED" {
			t.Errorf("unsandboxed %s: %q, want ALLOWED (the lab must have something to deny)\n%s", k, got[k], out)
		}
	}
}

// execOutput is the shell segment's combined log.
func (l *sandboxLab) execOutput(t *testing.T) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(l.execState, "runs", "*", "*", "output.log"))
	if len(matches) == 0 {
		t.Fatal("no shell output")
	}
	return matches[len(matches)-1]
}

// fakeSandboxClaude stands in for claude on its launch path: it takes its
// first stream-json message, runs the probes and the operator's claude
// config probes, writes its own config, and ends the turn.
const fakeSandboxClaude = `#!/bin/sh
read -r line
echo '{"type":"system","subtype":"init"}'
{ ` + "PROBES" + `
t write_operator_settings sh -c 'echo x >> "$HOME/.claude/settings.json"'
t write_operator_claude_json sh -c 'echo x >> "$HOME/.claude.json"'
t write_operator_plugin mkdir -p "$HOME/.claude/plugins/planted"
t read_operator_credentials read "$HOME/.claude/.credentials.json" claude-secret
} > ./probe-out 2>&1
mkdir -p "$CLAUDE_CONFIG_DIR/projects" && echo ok > "$CLAUDE_CONFIG_DIR/projects/written"
echo ok > "$TMPDIR/claude-tmp" && echo "tmp=$TMPDIR" >> ./probe-out
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s","num_turns":1,"total_cost_usd":0,"structured_output":{"state":"review","summary":"probed"}}'
`

func TestSandboxClaudeCodeIsolation(t *testing.T) {
	l := newSandboxLab(t, "claude-code")
	bin := filepath.Join(l.root, "bin", "claude")
	_ = os.MkdirAll(filepath.Dir(bin), 0o700)
	if err := os.WriteFile(bin, []byte(strings.Replace(fakeSandboxClaude, "PROBES", sandboxProbes, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := claudecode.New(claudecode.Options{StateDir: l.execState, Store: reference.New(), Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} },
		Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond, Artifacts: nil})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"binary": bin, "playbook": "-", "env": l.claudeEnv()})
	b := l.binding(sandboxPolicy, "", srt.Options{})
	r := extension.RunRequest{Work: domain.Work{ID: "work", ScopeID: "scope"}, Execution: domain.Execution{ID: "exec", Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Trigger: domain.Event{ID: "ev", Type: "note"},
		Context: extension.ExecutionContext{Config: domain.Payload{Schema: "core.config.binding.v1", Data: cfg},
			ToolbeltEndpoint: "unix://" + l.sock, ToolbeltToken: "own-token", ToolbeltTokenFile: l.run + "/token/token",
			Sandbox: b, Resources: b.Resources(labWorkspace{l.ws, true})},
		PersistRecovery: func(domain.Payload) error { return nil }}
	h, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	res := waitResult(t, h)
	out, _ := os.ReadFile(filepath.Join(l.ws, "probe-out"))
	if res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("outcome %s %q\n%s", res.Outcome.Kind, res.Outcome.Summary, out)
	}
	l.checkIsolation(t, string(out))
	if _, err := os.Stat(filepath.Join(l.claude, "projects", "written")); err != nil {
		t.Error("claude could not write its own config dir")
	}
	got := probeResults(string(out))
	for _, k := range []string{"write_operator_settings", "write_operator_claude_json", "write_operator_plugin", "read_operator_credentials"} {
		if got[k] != "DENIED" {
			t.Errorf("%s: %q from inside the sandbox, want DENIED\n%s", k, got[k], out)
		}
	}
	for _, path := range []string{".claude/settings.json", ".claude.json"} {
		if b, _ := os.ReadFile(filepath.Join(l.home, path)); string(b) != "{}\n" {
			t.Errorf("the operator's ~/%s was written from inside the sandbox: %q", path, b)
		}
	}
	if _, err := os.Lstat(filepath.Join(l.home, ".claude", "plugins")); err == nil {
		t.Error("a plugin was planted in the operator's ~/.claude from inside the sandbox")
	}
	if !strings.Contains(string(out), "tmp="+l.execState) {
		t.Errorf("claude's temporary directory is not its segment's own:\n%s", out)
	}
}

// TestSandboxClaudeCodeOwnConfig: a sandboxed claude without a config
// directory of its own, or under a policy that still grants the operator's
// ~/.claude, is refused as sandbox_denied with nothing run.
func TestSandboxClaudeCodeOwnConfig(t *testing.T) {
	l := newSandboxLab(t, "claude-code")
	bin := filepath.Join(l.root, "bin", "claude")
	_ = os.MkdirAll(filepath.Dir(bin), 0o700)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch \"$LAB_OUTSIDE/claude-ran\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := claudecode.New(claudecode.Options{StateDir: l.execState, Store: reference.New(), Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} },
		Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	granting := strings.Replace(sandboxPolicy, `"~/.claude-sandboxed"`, `"~/.claude-sandboxed","~/.claude","~/.claude.json"`, 1)
	for i, c := range []struct {
		name, policy string
		env          map[string]string
	}{
		{"no_config_dir", sandboxPolicy, l.env()},
		{"policy_grants_operator_claude", granting, l.claudeEnv()},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := json.Marshal(map[string]any{"binary": bin, "playbook": "-", "env": c.env})
			b := l.binding(c.policy, "", srt.Options{})
			id := domain.ID(fmt.Sprintf("own-%d", i))
			r := extension.RunRequest{Work: domain.Work{ID: id, ScopeID: "scope"}, Execution: domain.Execution{ID: id, Attempt: 1, Lease: domain.Lease{Epoch: 1}},
				Trigger: domain.Event{ID: "ev", Type: "note"},
				Context: extension.ExecutionContext{Config: domain.Payload{Schema: "core.config.binding.v1", Data: cfg},
					ToolbeltEndpoint: "unix://" + l.sock, ToolbeltToken: "own-token", ToolbeltTokenFile: l.run + "/token/token",
					Sandbox: b, Resources: b.Resources(labWorkspace{l.ws, true})},
				PersistRecovery: func(domain.Payload) error { return nil }}
			h, err := e.Run(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			res := waitResult(t, h)
			var f struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(res.Outcome.Data.Data, &f)
			if res.Outcome.Kind != domain.OutcomeFailed || f.Reason != "sandbox_denied" {
				t.Errorf("outcome %s reason %q (%s), want failed sandbox_denied", res.Outcome.Kind, f.Reason, res.Outcome.Summary)
			}
			if _, err := os.Stat(filepath.Join(l.outside, "claude-ran")); err == nil {
				t.Fatal("claude ran")
			}
		})
	}
}

// TestSandboxFailClosed: every way a declared sandbox can't be enforced
// ends the segment as a structured failure with nothing run.
func TestSandboxFailClosed(t *testing.T) {
	l := newSandboxLab(t, "shell")
	fake := func(name, script string) string {
		p := filepath.Join(l.root, "bin", name)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, policy, reason string
		o                    srt.Options
		hardlink             bool
	}{
		{"missing_binary", sandboxPolicy, "sandbox_unavailable", srt.Options{Binary: "/nonexistent/srt"}, false},
		{"unsupported_platform", sandboxPolicy, "sandbox_unavailable", srt.Options{GOOS: "windows"}, false},
		{"old_version", sandboxPolicy, "sandbox_unavailable", srt.Options{Binary: fake("old-srt", `echo 0.0.1`)}, false},
		{"failed_preflight", sandboxPolicy, "sandbox_unavailable", srt.Options{Binary: fake("broken-srt", `[ "$1" = --version ] && { echo 0.0.81; exit 0; }; echo "bwrap: setting up uid map: Permission denied" >&2; exit 1`)}, false},
		{"degraded", sandboxPolicy, "sandbox_unavailable", srt.Options{Binary: fake("degraded-srt", `[ "$1" = --version ] && { echo 0.0.81; exit 0; }; echo "Warning: seccomp not available - unix socket access not restricted" >&2; exit 0`)}, false},
		{"linux_socket_by_path", strings.Replace(sandboxPolicy, `,"unix_sockets":"all"`, "", 1), "sandbox_unavailable", srt.Options{GOOS: "linux", LookPath: func(s string) (string, error) {
			if s == "srt" {
				return l.srt, nil
			}
			return "/bin/sh", nil
		}, Binary: "srt"}, false},
		{"workspace_outside_policy", `{"write":[],"sockets":["{toolbelt}"],"unix_sockets":"all"}`, "sandbox_denied", srt.Options{}, false},
		{"socket_outside_policy", `{"write":["{workspace}"]}`, "sandbox_denied", srt.Options{}, false},
		{"policy_reaches_state", `{"write":["{workspace}","` + l.root + `"],"sockets":["{toolbelt}"],"unix_sockets":"all"}`, "sandbox_denied", srt.Options{}, false},
		{"policy_rereads_state", `{"write":["{workspace}"],"read":{"allow":["` + l.state + `/private"]},"sockets":["{toolbelt}"],"unix_sockets":"all"}`, "sandbox_denied", srt.Options{}, false},
		{"preexisting_hardlink", sandboxPolicy, "sandbox_denied", srt.Options{}, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.hardlink {
				link := filepath.Join(l.ws, "hardlink-token")
				if err := os.Link(l.state+"/private/operator.token", link); err != nil {
					t.Skipf("can't make a hard link here: %v", err)
				}
				defer os.Remove(link)
			}
			marker := filepath.Join(l.outside, c.name)
			e := l.shell(t)
			var saved domain.Payload
			h, err := e.Run(context.Background(), l.shellRequest(domain.ID(fmt.Sprintf("fc-%d", i)), "touch "+marker+" ./ran-"+c.name, l.binding(c.policy, "", c.o), &saved))
			if err != nil {
				t.Fatal(err)
			}
			res := waitResult(t, h)
			var f shellResult
			_ = json.Unmarshal(res.Outcome.Data.Data, &f)
			if res.Outcome.Kind != domain.OutcomeFailed || f.Reason != c.reason {
				t.Errorf("outcome %s reason %q (%s), want failed %s", res.Outcome.Kind, f.Reason, res.Outcome.Summary, c.reason)
			}
			for _, p := range []string{marker, filepath.Join(l.ws, "ran-"+c.name)} {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("the command ran: %s exists", p)
				}
			}
		})
	}
}

// TestSandboxProcessControl: Stop, Interrupt and Inject reach the sandboxed
// command, and Recover adopts only a process inside the original boundary.
func TestSandboxProcessControl(t *testing.T) {
	cmd := `trap 'echo term > ./got-term; exit 3' TERM
while :; do
  for f in "$OS_INBOX"/*.json; do [ -e "$f" ] && cp "$f" ./inbox-seen; done
  sleep 0.1
done`
	start := func(t *testing.T, l *sandboxLab, e *shell.Executor, id domain.ID, b *extension.BoundSandbox) (extension.RunRequest, *domain.Payload, extension.RunHandle) {
		t.Helper()
		saved := new(domain.Payload)
		r := l.shellRequest(id, cmd, b, saved)
		h, err := e.Run(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			if procs, _ := exec.Command("pgrep", "-f", "sleep 0.1").Output(); len(procs) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the sandboxed command never started")
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
		return r, saved, h
	}
	t.Run("inject_and_interrupt", func(t *testing.T) {
		l := newSandboxLab(t, "shell")
		e := l.shell(t)
		r, _, h := start(t, l, e, "pc-int", l.binding(sandboxPolicy, "", srt.Options{}))
		if err := e.Inject(context.Background(), r.Execution.ID, domain.Event{ID: "inj-1", Type: "note", Data: domain.Payload{Schema: "x.v1", Data: json.RawMessage(`{"hello":"sandbox"}`)}}); err != nil {
			t.Fatal(err)
		}
		waitFile(t, filepath.Join(l.ws, "inbox-seen"))
		if err := e.Interrupt(context.Background(), r.Execution.ID); err != nil {
			t.Fatal(err)
		}
		res := waitResult(t, h)
		var f shellResult
		_ = json.Unmarshal(res.Outcome.Data.Data, &f)
		want := extension.SandboxTerm
		if runtime.GOOS == "linux" {
			want = extension.SandboxKill
		}
		if f.Reason != "interrupted" || f.Interrupt != want {
			t.Errorf("interrupt: reason %q interrupt %q (%s), want interrupted %s", f.Reason, f.Interrupt, res.Outcome.Summary, want)
		}
		_, termErr := os.Stat(filepath.Join(l.ws, "got-term"))
		if want == extension.SandboxTerm && termErr != nil {
			t.Error("the sandboxed command's TERM handler did not run")
		}
		noneLeft(t)
	})
	t.Run("stop", func(t *testing.T) {
		l := newSandboxLab(t, "shell")
		e := l.shell(t)
		r, _, _ := start(t, l, e, "pc-stop", l.binding(sandboxPolicy, "", srt.Options{}))
		if err := e.Stop(context.Background(), r.Execution.ID, 1); err != nil {
			t.Fatal(err)
		}
		noneLeft(t)
	})
	t.Run("recover_adopts_the_same_sandbox", func(t *testing.T) {
		l := newSandboxLab(t, "shell")
		e := l.shell(t)
		r, saved, _ := start(t, l, e, "pc-rec", l.binding(sandboxPolicy, "", srt.Options{}))
		defer func() { _ = l.shell(t).Stop(context.Background(), r.Execution.ID, 3) }()
		// A restarted daemon: a new executor instance and a new binding
		// that knows only the pinned digest.
		r2 := r
		r2.Execution.Lease.Epoch = 2
		b2 := l.binding(sandboxPolicy, l.pinned(), srt.Options{})
		r2.Context.Sandbox, r2.Context.Resources = b2, b2.Resources(labWorkspace{l.ws, true})
		h2, err := l.shell(t).Recover(context.Background(), r2, *saved)
		if err != nil {
			t.Fatalf("recover refused the segment's own sandbox: %v", err)
		}
		_ = h2
	})
	t.Run("recover_refuses_a_changed_record", func(t *testing.T) {
		l := newSandboxLab(t, "shell")
		e := l.shell(t)
		r, saved, _ := start(t, l, e, "pc-tamper", l.binding(sandboxPolicy, "", srt.Options{}))
		defer func() { _ = l.shell(t).Stop(context.Background(), r.Execution.ID, 3) }()
		plans, _ := filepath.Glob(filepath.Join(l.run, "sandbox", "*", "plan.json"))
		if len(plans) != 1 {
			t.Fatalf("plans: %v", plans)
		}
		raw, _ := os.ReadFile(plans[0])
		_ = os.Chmod(plans[0], 0o600)
		_ = os.WriteFile(plans[0], []byte(strings.Replace(string(raw), "allowed.invalid", "example.com", 1)), 0o600)
		r2 := r
		r2.Execution.Lease.Epoch = 2
		b2 := l.binding(sandboxPolicy, l.pinned(), srt.Options{})
		r2.Context.Sandbox = b2
		if _, err := l.shell(t).Recover(context.Background(), r2, *saved); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("recover after the record changed: %v, want ErrNotAdoptable", err)
		}
	})
	t.Run("recover_refuses_a_root_outside_the_sandbox", func(t *testing.T) {
		l := newSandboxLab(t, "shell")
		e := l.shell(t)
		r, saved, _ := start(t, l, e, "pc-root", l.binding(sandboxPolicy, "", srt.Options{}))
		defer func() { _ = l.shell(t).Stop(context.Background(), r.Execution.ID, 3) }()
		// The record's root now names a process outside the sandbox.
		outside := exec.Command("sleep", "30")
		if err := outside.Start(); err != nil {
			t.Fatal(err)
		}
		defer outside.Process.Kill()
		roots, _ := filepath.Glob(filepath.Join(l.run, "sandbox", "*", "root"))
		if len(roots) != 1 {
			t.Fatalf("roots: %v", roots)
		}
		_ = os.WriteFile(roots[0], []byte(fmt.Sprintf("%d\n", outside.Process.Pid)), 0o600)
		r2 := r
		r2.Execution.Lease.Epoch = 2
		r2.Context.Sandbox = l.binding(sandboxPolicy, l.pinned(), srt.Options{})
		if _, err := l.shell(t).Recover(context.Background(), r2, *saved); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("recover with a root outside the sandbox: %v, want ErrNotAdoptable", err)
		}
	})
	t.Run("recover_refuses_an_unsandboxed_segment", func(t *testing.T) {
		l := newSandboxLab(t, "shell")
		e := l.shell(t)
		r, saved, _ := start(t, l, e, "pc-plain", nil)
		defer func() { _ = l.shell(t).Stop(context.Background(), r.Execution.ID, 3) }()
		// The binding now declares a sandbox; the running segment has none.
		r2 := r
		r2.Execution.Lease.Epoch = 2
		r2.Context.Sandbox = l.binding(sandboxPolicy, "", srt.Options{})
		if _, err := l.shell(t).Recover(context.Background(), r2, *saved); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("recover of an unsandboxed segment under a sandboxed binding: %v, want ErrNotAdoptable", err)
		}
	})
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// noneLeft: nothing of the sandboxed command still runs.
func noneLeft(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", "sleep 0.1").Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes left running: %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// fakeWaitingClaude takes stream-json messages until it is stopped, keeping
// each in the workspace, so injected input can be seen arriving inside.
const fakeWaitingClaude = `#!/bin/sh
trap 'echo term > ./got-term; exit 3' TERM
echo '{"type":"system","subtype":"init"}'
while IFS= read -r line; do printf '%s\n' "$line" >> ./stdin-seen; done
sleep 300
`

// TestSandboxClaudeCodeProcessControl: on Claude Code's launch path, Inject
// reaches the sandboxed claude through its stdin, Recover adopts it only
// inside its own sandbox, and Interrupt and Stop end it.
func TestSandboxClaudeCodeProcessControl(t *testing.T) {
	l := newSandboxLab(t, "claude-code")
	bin := filepath.Join(l.root, "bin", "claude")
	_ = os.MkdirAll(filepath.Dir(bin), 0o700)
	if err := os.WriteFile(bin, []byte(fakeWaitingClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	store := reference.New()
	newExec := func() *claudecode.Executor {
		e, err := claudecode.New(claudecode.Options{StateDir: l.execState, Store: store, Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} },
			Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	cfg, _ := json.Marshal(map[string]any{"binary": bin, "playbook": "-", "env": l.claudeEnv()})
	var saved domain.Payload
	request := func(epoch uint64, b *extension.BoundSandbox) extension.RunRequest {
		return extension.RunRequest{Work: domain.Work{ID: "work", ScopeID: "scope"}, Execution: domain.Execution{ID: "exec", Attempt: 1, Lease: domain.Lease{Epoch: epoch}},
			Trigger: domain.Event{ID: "ev", Type: "note"},
			Context: extension.ExecutionContext{Config: domain.Payload{Schema: "core.config.binding.v1", Data: cfg},
				ToolbeltEndpoint: "unix://" + l.sock, ToolbeltToken: "own-token", ToolbeltTokenFile: l.run + "/token/token",
				Sandbox: b, Resources: b.Resources(labWorkspace{l.ws, true})},
			PersistRecovery: func(p domain.Payload) error { saved = p; return nil }}
	}
	e := newExec()
	h, err := e.Run(context.Background(), request(1, l.binding(sandboxPolicy, "", srt.Options{})))
	if err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(l.ws, "stdin-seen"))
	if err := e.Inject(context.Background(), "exec", domain.Event{ID: "inj-1", Type: "note", Data: domain.Payload{Schema: "x.v1", Data: json.RawMessage(`{"text":"injected-into-sandbox"}`)}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(l.ws, "stdin-seen"))
		if strings.Count(string(b), "\n") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the injected message never reached claude inside the sandbox:\n%s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A restarted daemon recovers it under the pinned plan, and refuses it
	// when the binding's sandbox is a new one it wasn't launched in.
	if _, err := newExec().Recover(context.Background(), request(2, l.binding(sandboxPolicy, "sha256:another", srt.Options{})), saved); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("recover under another pin: %v, want ErrNotAdoptable", err)
	}
	adopter := newExec()
	h2, err := adopter.Recover(context.Background(), request(3, l.binding(sandboxPolicy, l.pinned(), srt.Options{})), saved)
	if err != nil {
		t.Fatalf("recover its own sandbox: %v", err)
	}
	_ = h
	if err := adopter.Interrupt(context.Background(), "exec"); err != nil {
		t.Fatal(err)
	}
	res := waitResult(t, h2)
	var f struct {
		Reason    string `json:"reason"`
		Interrupt string `json:"interrupt"`
	}
	_ = json.Unmarshal(res.Outcome.Data.Data, &f)
	want := extension.SandboxTerm
	if runtime.GOOS == "linux" {
		want = extension.SandboxKill
	}
	if f.Interrupt != want {
		t.Errorf("interrupted turn: %+v (%s), want interrupt %s", f, res.Outcome.Summary, want)
	}
	if err := adopter.Stop(context.Background(), "exec", 3); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("pgrep", "-f", "sleep 300").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("claude left processes running: %s", out)
	}
}
