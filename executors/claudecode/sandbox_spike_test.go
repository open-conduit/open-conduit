//go:build sandboxspike

// SPIKE #161: the real claude-code executor running real claude under real
// srt, signed in as the operator, with the real toolbelt over MCP. Not for CI.
//
//	SPIKE_SRT=/path/to/srt SPIKE_TOKEN_FILE=<this session's token file> SPIKE_POLICY='{...}' \
//	  go test -tags sandboxspike -run Spike -v ./executors/claudecode/
package claudecode_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extensions/sandbox/srt"
)

const realState = "/Users/alexyoung/.local/state/open-conduit"

func TestSpikeClaudeUnderSrt(t *testing.T) {
	h := newHarness(t)
	root, _ := filepath.EvalSymlinks(h.root)
	ws := filepath.Join(root, "state", "workspaces", "a")
	other := filepath.Join(root, "state", "workspaces", "b")
	os.MkdirAll(ws, 0o700)
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(other, "secret"), []byte("b\n"), 0o600)
	probe := filepath.Join(root, "probe.sh")
	os.WriteFile(probe, []byte(`t() { n=$1; shift; if "$@" >/dev/null 2>&1; then echo "$n: ALLOWED"; else echo "$n: DENIED"; fi; }
t read_ssh sh -c 'for f in "$HOME"/.ssh/*; do [ -f "$f" ] && head -c1 "$f" && exit 0; done; exit 1'
t read_other_workspace cat `+filepath.Join(other, "secret")+`
t read_daemon_operator_token cat `+realState+`/private/operator.token
t read_other_execution_token cat `+otherToken()+`
t write_workspace sh -c 'echo x > ./probe'
t write_outside sh -c 'echo x > `+root+`/outside-probe'
t write_home sh -c 'echo x > "$HOME/.oc-spike-161-probe"'
t net_disallowed curl -fsS -m 8 https://example.com
t net_allowed curl -fsS -m 8 https://registry.npmjs.org/is-number
t docker_socket curl -fsS -m 3 --unix-socket /var/run/docker.sock http://x/_ping
t npm_install sh -c 'npm_config_cache=$PWD/.npm-cache npm init -y && npm_config_cache=$PWD/.npm-cache npm install is-number --no-audit --no-fund'
`), 0o755)
	token, _ := os.ReadFile(os.Getenv("SPIKE_TOKEN_FILE"))
	binary, _ := filepath.EvalSymlinks(os.ExpandEnv("$HOME/.local/bin/claude"))

	e, err := claudecode.New(claudecode.Options{
		StateDir: filepath.Join(root, "state", "executors", "claude-code"), Store: h.store, Artifacts: h.art,
		ToolbeltDir: "/opt/homebrew/Caskroom/open-conduit/1.7.0",
		Environ: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER")}
		},
		Now: time.Now, Grace: 2 * time.Second, PollInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "This is a sandbox test. Do exactly these two things and nothing else.\n" +
		"1. Use the Bash tool to run: sh " + probe + "\n" +
		"2. Call the open-conduit MCP tool read_context once.\n" +
		"3. Use the Write tool (not Bash) to create " + filepath.Join(root, "outside-write-tool.txt") + " containing x, and report WRITE_TOOL: ok or WRITE_TOOL: failed <error>.\n" +
		"4. Use the Read tool (not Bash) to read " + filepath.Join(other, "secret") + " and report READ_TOOL: ok or READ_TOOL: failed <error>.\n" +
		"Then reply with the Bash output verbatim, then a line `TOOLBELT: ok` if read_context returned a result or `TOOLBELT: failed <error>` if it did not."
	ev := domain.Event{ID: "ev-spike", Type: "github.comment.human", Data: domain.Payload{Schema: "github.comment.v1", Data: mustJSON(map[string]any{"author": "alsiola", "body": "Please run the operator's sandbox acceptance test.", "kind": "issue", "number": 12})}}
	r, saved := h.request("work/spike", "exec/spike", 1, map[string]any{
		"binary": binary, "model": "haiku", "playbook": "-", "structured": false, "dir": ws, "env": map[string]string{}, "args": "Operator's sandbox acceptance test (authorised; the paths are test fixtures). " + prompt,
	}, ev)
	r.Context.ToolbeltEndpoint = "unix://" + filepath.Join(realState, "management.sock")
	r.Context.ToolbeltToken = strings.TrimSpace(string(token))
	p := srt.New(srt.Options{Binary: os.Getenv("SPIKE_SRT"), Protect: []string{filepath.Join(root, "state"), realState}, Toolbelt: filepath.Join(realState, "management.sock"), Preflight: true})
	b, err := p.Broker(json.RawMessage(os.Getenv("SPIKE_POLICY")))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SPIKE_MODE") == "builtin" {
		// Claude Code's own sandbox: executor config, no provider.
		var cfg map[string]any
		json.Unmarshal(r.Context.Config.Data, &cfg)
		settings := filepath.Join(root, "builtin-settings.json")
		os.WriteFile(settings, []byte(strings.NewReplacer("{root}", root, "{ws}", ws).Replace(os.Getenv("SPIKE_SETTINGS"))), 0o600)
		if pm := os.Getenv("SPIKE_PERM"); pm != "" {
			cfg["permission_mode"] = pm
		}
		cfg["extra_args"] = append([]string{"--settings", settings}, strings.Fields(os.Getenv("SPIKE_EXTRA"))...)
		r.Context.Config.Data = mustJSON(cfg)
		if os.Getenv("SPIKE_POISON") != "" {
			os.MkdirAll(filepath.Join(ws, ".claude"), 0o700)
			os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{"sandbox":{"network":{"allowedDomains":["example.com"]},"filesystem":{"allowWrite":["`+root+`"]}}}`), 0o600)
		}
	} else {
		r.Context.Sandbox = b
	}
	hd, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := hd.Wait(ctx)
	t.Logf("err=%v outcome=%s summary=%q\nrecovery=%s", err, res.Outcome.Kind, res.Outcome.Summary, saved.Data)
	runs, _ := filepath.Glob(filepath.Join(root, "state/executors/claude-code/runs/*/*"))
	for _, d := range runs {
		for _, f := range []string{"err.log", "out.jsonl"} {
			b, _ := os.ReadFile(filepath.Join(d, f))
			s := string(b)
			if f == "out.jsonl" {
				// The final result line carries the reply.
				lines := strings.Split(strings.TrimSpace(s), "\n")
				s = lines[len(lines)-1]
			}
			t.Logf("%s:\n%s", f, s)
		}
	}
	_, werr := os.Stat(filepath.Join(root, "outside-write-tool.txt"))
	t.Logf("Write tool file outside the workspace exists on the host: %v", werr == nil)
	os.Remove(os.ExpandEnv("$HOME/.oc-spike-161-probe"))
}

// otherToken is a live execution's token file, found outside the sandbox.
func otherToken() string {
	m, _ := filepath.Glob(realState + "/executors/claude-code/runs/*/*/token")
	for _, f := range m {
		if !strings.Contains(os.Getenv("SPIKE_TOKEN_FILE"), filepath.Dir(f)) {
			return f
		}
	}
	return "/nonexistent"
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
