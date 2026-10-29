//go:build sandboxspike

// SPIKE #161: real srt, real toolbelt socket. Not for CI. Run with
//
//	SPIKE_SRT=/path/to/srt SPIKE_TOKEN_FILE=<this session's toolbelt token file> \
//	  go test -tags sandboxspike -run Spike -v ./executors/shell/
package shell_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/sandbox/srt"
)

var realState = envOr("SPIKE_REAL_STATE", "/Users/alexyoung/.local/state/open-conduit")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type lab struct {
	root, state, wsA, wsB, outside, sock, toolbeltDir, token string
	loopback                                                 net.Listener
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	l := &lab{root: root, state: filepath.Join(root, "state"), outside: filepath.Join(root, "outside"),
		sock: envOr("SPIKE_SOCK", filepath.Join(realState, "management.sock")), toolbeltDir: envOr("SPIKE_TOOLBELT_DIR", "/opt/homebrew/Caskroom/open-conduit/1.7.0")}
	l.wsA, l.wsB = filepath.Join(l.state, "workspaces", "a"), filepath.Join(l.state, "workspaces", "b")
	for _, d := range []string{l.wsA, l.wsB, l.outside, filepath.Join(l.state, "executors/shell/runs/other/0000")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(l.wsB, "secret"), []byte("b\n"), 0o600)
	os.WriteFile(filepath.Join(l.state, "executors/shell/runs/other/0000/token"), []byte("other\n"), 0o600)
	if b, err := os.ReadFile(os.Getenv("SPIKE_TOKEN_FILE")); err == nil {
		l.token = strings.TrimSpace(string(b))
	}
	var err error
	if l.loopback, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.loopback.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 1024)
			c.Read(buf)
			c.Write([]byte("HTTP/1.0 200 OK\r\nContent-Length: 9\r\n\r\nloopback\n"))
			c.Close()
		}
	}()
	t.Cleanup(func() { l.loopback.Close() })
	return l
}

func (l *lab) provider(extra ...func(*srt.Options)) *srt.Provider {
	o := srt.Options{Binary: os.Getenv("SPIKE_SRT"), Protect: []string{l.state, realState}, Toolbelt: l.sock, Preflight: true}
	for _, f := range extra {
		f(&o)
	}
	return srt.New(o)
}

var policy = envOr("SPIKE_POLICY", `{"network":{"allow":["registry.npmjs.org"]},"read":{"deny":["~/.ssh"]},"write":["{workspace}"],"sockets":["{toolbelt}"]}`)

func (l *lab) request(t *testing.T, id, command string, pol string, p *srt.Provider) (extension.RunRequest, *domain.Payload) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"dir": l.wsA, "command": command, "timeout_seconds": 120, "env": map[string]string{
		"LAB": l.root, "TOOLBELT_PROBE": os.Getenv("SPIKE_TOOLBELT_PROBE"), "LOOPBACK": l.loopback.Addr().String(), "REAL_STATE": realState, "npm_config_cache": filepath.Join(l.wsA, ".npm-cache")}})
	r, saved := request(id, 1, string(cfg))
	r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken = "unix://"+l.sock, l.token
	if p != nil {
		b, err := p.Broker(json.RawMessage(pol))
		if err != nil {
			t.Fatal(err)
		}
		r.Context.Sandbox = b
	}
	return r, saved
}

func (l *lab) executor(t *testing.T) *shell.Executor {
	t.Helper()
	e, err := shell.New(shell.Options{StateDir: filepath.Join(l.state, "executors/shell"), Artifacts: &memoryArtifacts{data: map[string][]byte{}}, ToolbeltDir: l.toolbeltDir,
		Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")} }, Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// probes print "name: ALLOWED|DENIED".
var probes = `t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi; }
other=$(ls -d "$REAL_STATE"/executors/claude-code/runs/*/*/token 2>/dev/null | grep -v "$OS_TOKEN_FILE" | head -1)
t read_ssh sh -c 'for f in "$HOME"/.ssh/*; do [ -f "$f" ] && head -c1 "$f" && exit 0; done; exit 1'
t read_other_workspace cat "$LAB/state/workspaces/b/secret"
t read_other_run_token cat "$LAB/state/executors/shell/runs/other/0000/token"
t read_daemon_operator_token cat "$REAL_STATE/private/operator.token"
t read_daemon_db sh -c 'test -n "$(ls -A "$REAL_STATE")"'
t read_own_input cat "$OS_INPUT_FILE"
t write_workspace sh -c 'echo x > ./probe'
t write_outputs sh -c 'echo x > "$OS_OUTPUT_DIR/probe"'
t write_outside sh -c 'echo x > "$LAB/outside/probe"'
t write_home sh -c 'echo x > "$HOME/.oc-spike-161-probe"'
t write_policy_file sh -c 'echo x > "$(dirname "$OS_INPUT_FILE")/sandbox/srt.json"'
t write_run_record sh -c 'echo x > "$(dirname "$OS_INPUT_FILE")/sandbox.json"'
t net_disallowed curl -fsS -m 8 https://example.com
t net_allowed curl -fsS -m 8 https://registry.npmjs.org/is-number
t loopback_tcp curl -fsS -m 3 "http://$LOOPBACK/"
t docker_socket curl -fsS -m 3 --unix-socket /var/run/docker.sock http://x/_ping
t toolbelt_own ${TOOLBELT_PROBE:-os call github.context}
t npm_install sh -c 'npm init -y && npm install is-number --no-audit --no-fund'
`

func TestSpikeIsolation(t *testing.T) {
	l := newLab(t)
	art := &memoryArtifacts{data: map[string][]byte{}}
	for _, sandboxed := range []bool{false, true} {
		e, err := shell.New(shell.Options{StateDir: filepath.Join(l.state, "executors/shell"), Artifacts: art, ToolbeltDir: l.toolbeltDir,
			Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")} }, Grace: time.Second, PollInterval: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		var p *srt.Provider
		if sandboxed {
			p = l.provider()
		}
		r, saved := l.request(t, "exec/iso-"+map[bool]string{false: "plain", true: "srt"}[sandboxed], probes, policy, p)
		res := run(t, e, r)
		f := data(t, res)
		t.Logf("sandboxed=%v outcome=%s reason=%s recovery=%s\n%s", sandboxed, res.Outcome.Kind, f.Reason, saved.Data, art.content(f.Log))
		os.Remove(filepath.Join(os.Getenv("HOME"), ".oc-spike-161-probe"))
	}
}

// Fail closed: each case is a structured failure with no process started.
func TestSpikeFailClosed(t *testing.T) {
	l := newLab(t)
	cases := []struct {
		name, pol string
		p         *srt.Provider
	}{
		{"missing_binary", policy, l.provider(func(o *srt.Options) { o.Binary = "/nonexistent/srt" })},
		{"unsupported_platform", policy, l.provider(func(o *srt.Options) { o.GOOS = "windows" })},
		{"need_outside_policy_workspace", `{"write":[]}`, l.provider()},
		{"need_outside_policy_socket", `{"write":["{workspace}"]}`, l.provider()},
		{"linux_socket_by_path", policy, l.provider(func(o *srt.Options) {
			o.GOOS = "linux"
			o.LookPath = func(s string) (string, error) { return "/usr/bin/" + s, nil }
		})},
		{"invalid_policy", `{"network":{"allow":"*"}}`, l.provider()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := l.executor(t)
			marker := filepath.Join(l.outside, c.name)
			b, err := c.p.Broker(json.RawMessage(c.pol))
			if err != nil {
				t.Logf("refused at bind: %v", err)
				return
			}
			r, _ := l.request(t, "exec/fc-"+c.name, "touch "+marker, c.pol, nil)
			r.Context.Sandbox = b
			res := run(t, e, r)
			f := data(t, res)
			_, statErr := os.Stat(marker)
			t.Logf("outcome=%s reason=%s summary=%q process_ran=%v", res.Outcome.Kind, f.Reason, res.Outcome.Summary, statErr == nil)
			if res.Outcome.Kind != domain.OutcomeFailed || statErr == nil || !strings.HasPrefix(f.Reason, "sandbox_") {
				t.Fatalf("not fail-closed")
			}
		})
	}
}

// Stop, Interrupt and Inject unchanged; Recover adopts only the pinned
// sandbox and the replaced toolbelt token is visible inside it.
func TestSpikeProcessControl(t *testing.T) {
	l := newLab(t)
	cmd := `trap 'echo got-TERM > "$OS_OUTPUT_DIR/term"; exit 3' TERM
echo "$OS_TOKEN_FILE" > ./tokenfile
while :; do
  if os inbox > ./inbox.out 2>&1 && [ -s ./inbox.out ]; then cp ./inbox.out "$OS_OUTPUT_DIR/inbox"; fi
  cat "$OS_TOKEN_FILE" > ./token.seen 2>/dev/null
  sleep 0.2
done`
	t.Run("interrupt", func(t *testing.T) {
		e := l.executor(t)
		r, saved := l.request(t, "exec/pc-int", cmd, policy, l.provider())
		h, err := e.Run(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		if err := e.Inject(context.Background(), r.Execution.ID, domain.Event{ID: "inj-1", Type: "note", Data: domain.Payload{Schema: "x.v1", Data: json.RawMessage(`{"hello":"sandbox"}`)}}); err != nil {
			t.Fatalf("inject: %v", err)
		}
		time.Sleep(2 * time.Second)
		// A daemon restart: a new executor instance adopts the live process.
		e2 := l.executor(t)
		r2 := r
		r2.Execution.Lease.Epoch = 2
		r2.Context.ToolbeltToken = l.token + "-epoch2"
		h2, err := e2.Recover(context.Background(), r2, *saved)
		t.Logf("recover: err=%v digest pinned=%s", err, saved.Data)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		seen, _ := os.ReadFile(filepath.Join(l.wsA, "token.seen"))
		t.Logf("token visible inside sandbox after recover rotated: %v", strings.TrimSpace(string(seen)) == l.token+"-epoch2")
		if err := e2.Interrupt(context.Background(), r.Execution.ID); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := h2.Wait(ctx)
		f := data(t, res)
		t.Logf("after interrupt: outcome=%s reason=%s exit=%v outputs=%v err=%v", res.Outcome.Kind, f.Reason, f.ExitCode, f.Outputs, err)
		_ = h
	})
	t.Run("recover_refuses_changed_policy", func(t *testing.T) {
		e := l.executor(t)
		r, saved := l.request(t, "exec/pc-tamper", cmd, policy, l.provider())
		if _, err := e.Run(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		var meta struct{ Run, Sandbox string }
		json.Unmarshal(saved.Data, &meta)
		rec := filepath.Join(l.state, "executors/shell/runs", meta.Run, "sandbox.json")
		b, _ := os.ReadFile(rec)
		os.WriteFile(rec, []byte(strings.Replace(string(b), "registry.npmjs.org", "example.com", 1)), 0o600)
		e2 := l.executor(t)
		r2 := r
		r2.Execution.Lease.Epoch = 2
		_, err := e2.Recover(context.Background(), r2, *saved)
		t.Logf("recover after the record changed: %v (adoptable=%v)", err, !errors.Is(err, extension.ErrNotAdoptable))
		if err := e2.Stop(context.Background(), r.Execution.ID, 2); err != nil {
			t.Logf("stop: %v", err)
		}
		if err := e.Stop(context.Background(), r.Execution.ID, 2); err != nil {
			t.Logf("stop (original instance): %v", err)
		}
	})
	t.Run("stop", func(t *testing.T) {
		e := l.executor(t)
		r, _ := l.request(t, "exec/pc-stop", cmd, policy, l.provider())
		if _, err := e.Run(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		start := time.Now()
		err := e.Stop(context.Background(), r.Execution.ID, 1)
		left, _ := pgrepF(l.wsA)
		t.Logf("stop: err=%v in %s; processes left under the workspace: %v", err, time.Since(start).Round(time.Millisecond), left)
	})
}

func pgrepF(path string) ([]string, error) {
	out, err := exec.Command("pgrep", "-f", path).Output()
	return strings.Fields(string(out)), err
}
