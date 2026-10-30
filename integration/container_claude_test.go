package integration_test

// Real Docker tests for claude-code in a container: a turn, a resumed turn
// and an operator's attach, with a stand-in claude in the test image (make
// container-test and the container CI job). They call no real service.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
)

// fakeContainerClaude stands in for claude in the test image. A turn (-p)
// records where it ran and what it was given, takes its first stream-json
// message, writes the session's transcript under its config directory and
// ends the turn. An attach (no -p) does what claude would with the config
// it finds in its home and the workspace: runs every hook, helper, status
// line and MCP server command there. Each planted command is a script that
// notes it ran in $FAKE_DIR and touches a file outside every mount, which
// only a run on the host could create.
const fakeContainerClaude = `#!/bin/sh
session=; resume=0; print=0
while [ $# -gt 0 ]; do
  case "$1" in
    -p) print=1 ;;
    --session-id) session=$2; shift ;;
    --resume) session=$2; resume=1; shift ;;
  esac
  shift
done
f=${FAKE_DIR:?}
where=host; [ -f /.dockerenv ] && where=container
printf 'print=%s session=%s resume=%s where=%s home=%s config=%s token=%s traffic=%s\n' "$print" "$session" "$resume" "$where" "$HOME" "$CLAUDE_CONFIG_DIR" "$CLAUDE_CODE_OAUTH_TOKEN" "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" >> "$f/invocations"
t="$CLAUDE_CONFIG_DIR/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"
if [ "$print" = 0 ]; then
  [ -f "$t/$session.jsonl" ] || { echo "no transcript at $t/$session.jsonl" >> "$f/invocations"; exit 1; }
  for c in "$HOME/settings.json" "$HOME/.claude.json" .claude/settings.json .claude/settings.local.json .mcp.json; do
    grep -o '[^" ]*/\.hostile/[a-z-]*\.sh' "$c" 2>/dev/null
  done | while read -r s; do sh "$s"; done
  exit 0
fi
read -r line
mkdir -p "$t"
echo '{"type":"user"}' >> "$t/$session.jsonl"
echo '{"type":"system","subtype":"init"}'
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s","num_turns":1,"total_cost_usd":0.01,"structured_output":{"state":"review","summary":"ok","handoff":"note"}}\n' "$session"
`

// TestDockerClaudeCode: a claude-code binding runs in a container with the
// Work's home as HOME and CLAUDE_CONFIG_DIR, its sign-in only in the
// secrets file (not in docker inspect, and gone once read), and its
// nonessential traffic off; the next segment resumes the session from the
// home. An attach runs `claude --resume` in a container of its own on a
// terminal, and every hook, apiKeyHelper, statusLine and MCP server planted
// in the home's settings.json and .claude.json and the workspace's
// .claude/settings.json, .claude/settings.local.json and .mcp.json runs
// only inside it. The attach container goes on detach.
func TestDockerClaudeCode(t *testing.T) {
	l := newDockerLab(t)
	ctx := context.Background()
	bin, fake := filepath.Join(l.root, "bin"), filepath.Join(l.root, "fake")
	execState := filepath.Join(l.state, "executors", "claude-code")
	home := filepath.Join(l.state, "homes", "work")
	for _, d := range []string{bin, fake, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeContainerClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := claudecode.New(claudecode.Options{StateDir: execState, Store: reference.New(), Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} },
		Grace: 2 * time.Second, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	grant := `{"image":"` + dockerTestImage + `","write":["{workspace}","` + fake + `"],"read":["` + bin + `"],"network":{"allow":["api.anthropic.com"]},"provider":{"context":"` + l.dctx + `"}}`
	cfg, _ := json.Marshal(map[string]any{"binary": filepath.Join(bin, "claude"), "playbook": "-", "env": map[string]string{"FAKE_DIR": fake},
		"secret_env": map[string]string{"claude": "CLAUDE_CODE_OAUTH_TOKEN"}})
	const token = "sk-ant-oat-test-secret"
	work := domain.Work{ID: "work", ScopeID: "scope"}
	bind := func(exec domain.ID, segment int, attach bool) *extension.BoundContainer {
		return extension.BindContainer(extension.ContainerBinding{Provider: l.provider, Owner: extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: exec},
			Config: domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(grant)}, Host: l.host, Segment: segment, Attach: attach,
			Protected: []string{l.state, l.config}, RunRoot: filepath.Join(execState, "runs"), Records: filepath.Join(l.state, "runs", string(exec), "container"),
			Home: l.home, WorkHome: home,
			Pin: func(c extension.ContainerPin) error {
				l.mu.Lock()
				l.pins = append(l.pins, c)
				l.mu.Unlock()
				return nil
			}})
	}
	turn := func(exec domain.ID) extension.Result {
		t.Helper()
		b := bind(exec, 1, false)
		r := extension.RunRequest{Work: work, Execution: domain.Execution{ID: exec, WorkID: work.ID, Attempt: 1, Lease: domain.Lease{Epoch: 1}},
			Trigger: domain.Event{ID: domain.ID("ev-" + exec), Type: "note"},
			Context: extension.ExecutionContext{Config: domain.Payload{Data: cfg}, Secrets: map[string]string{"claude": token},
				Container: b, ContainerHome: home, Resources: b.Resources(dockerWorkspace{l.ws})},
			PersistRecovery: func(domain.Payload) error { return nil }}
		h, err := e.Run(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return waitResult(t, h)
	}
	invocations := func() []string {
		raw, _ := os.ReadFile(filepath.Join(fake, "invocations"))
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}

	if res := turn("exec-1"); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("the first turn failed: %q\n%v", res.Outcome.Summary, invocations())
	}
	inv := invocations()
	if want := "print=1 session="; len(inv) != 1 || !strings.HasPrefix(inv[0], want) || !strings.Contains(inv[0], "resume=0 where=container home="+home+" config="+home+" token="+token+" traffic=1") {
		t.Fatalf("the first turn ran as %v", inv)
	}
	pin := l.pinned()
	if env, err := l.docker("inspect", "--format", "{{json .Config.Env}} {{json .Args}}", dockerID(pin.ID)); err != nil || strings.Contains(env, token) {
		t.Errorf("docker inspect shows the token (%v): %s", err, env)
	}
	if left, _ := filepath.Glob(filepath.Join(execState, "runs", "*", "*", ".open-conduit-secrets")); len(left) != 0 {
		t.Errorf("the secrets file outlived the container's start: %v", left)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Errorf("the home was not seeded: %v", err)
	}

	if res := turn("exec-2"); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("the second turn failed: %q\n%v", res.Outcome.Summary, invocations())
	}
	if inv := invocations(); len(inv) != 2 || !strings.Contains(inv[1], "resume=1 where=container") {
		t.Fatalf("the second turn did not resume: %v", inv)
	}

	// The agent plants what a host-side claude would run, in its home and
	// in the workspace.
	planted := []string{"home-pretooluse", "home-sessionstart", "home-apikeyhelper", "home-statusline", "home-mcp",
		"ws-pretooluse", "ws-sessionstart", "ws-apikeyhelper", "ws-statusline", "local-sessionstart", "ws-mcp"}
	_ = os.MkdirAll(filepath.Join(l.ws, ".hostile"), 0o700)
	_ = os.MkdirAll(filepath.Join(l.ws, ".claude"), 0o700)
	for _, name := range planted {
		script := "#!/bin/sh\necho " + name + ` >> "$FAKE_DIR/ran"` + "\ntouch " + l.outside + "/" + name + "\n"
		if err := os.WriteFile(filepath.Join(l.ws, ".hostile", name+".sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := func(name string) string { return filepath.Join(l.ws, ".hostile", name+".sh") }
	settings := func(prefix string) string {
		return `{"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "` + s(prefix+"-pretooluse") + `"}]}], ` +
			`"SessionStart": [{"hooks": [{"type": "command", "command": "` + s(prefix+"-sessionstart") + `"}]}]}, ` +
			`"apiKeyHelper": "` + s(prefix+"-apikeyhelper") + `", "statusLine": {"type": "command", "command": "` + s(prefix+"-statusline") + `"}}`
	}
	mcp := func(name string) string {
		return `{"hasCompletedOnboarding": true, "mcpServers": {"x": {"type": "stdio", "command": "` + s(name) + `"}}}`
	}
	for path, body := range map[string]string{
		filepath.Join(home, "settings.json"):                  settings("home"),
		filepath.Join(home, ".claude.json"):                   mcp("home-mcp"),
		filepath.Join(l.ws, ".claude", "settings.json"):       settings("ws"),
		filepath.Join(l.ws, ".claude", "settings.local.json"): `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "` + s("local-sessionstart") + `"}]}]}}`,
		filepath.Join(l.ws, ".mcp.json"):                      `{"mcpServers": {"x": {"type": "stdio", "command": "` + s("ws-mcp") + `"}}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The attach, as the daemon makes it for `open-conduit container attach`.
	spec, err := e.ContainerAttach(ctx, extension.ContainerAttachRequest{Work: work, Config: domain.Payload{Data: cfg}, Secrets: map[string]string{"claude": token}, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	b := bind("exec-2", 0, true)
	plan, err := b.Launch(ctx, spec.Needs, spec.Launch)
	if err != nil {
		t.Fatal(err)
	}
	attach := l.pinned()
	if labels, _ := l.docker("inspect", "--format", "{{index .Config.Labels \""+extension.ContainerLabelSegment+"\"}} {{.Config.Tty}}", dockerID(attach.ID)); labels != "attach true" {
		t.Errorf("the attach container is %q, want labelled attach with a terminal", labels)
	}
	if out, err := runOnTerminal(plan.Argv, plan.Env); err != nil {
		t.Fatalf("the attach exited with %v:\n%s\n%v", err, out, invocations())
	}
	if inv := invocations(); len(inv) != 3 || !strings.Contains(inv[2], "print=0") || !strings.Contains(inv[2], "resume=1 where=container") || !strings.Contains(inv[2], "token="+token) {
		t.Errorf("the attach ran as %v", inv)
	}
	raw, _ := os.ReadFile(filepath.Join(fake, "ran"))
	ran := strings.Fields(string(raw))
	slices.Sort(ran)
	want := slices.Clone(planted)
	slices.Sort(want)
	if !slices.Equal(ran, want) {
		t.Errorf("inside the attach container ran %v, want %v", ran, want)
	}
	if left, _ := os.ReadDir(l.outside); len(left) != 0 {
		t.Errorf("planted commands ran on the host: %v", left)
	}
	if err := l.provider.Remove(ctx, extension.ContainerRef{ID: attach.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.docker("inspect", dockerID(attach.ID)); err == nil {
		t.Error("the attach container outlived its detach")
	}
	if gw, _ := l.docker("ps", "-aq", "--filter", "label="+extension.ContainerLabelSegment+"="+extension.ContainerSegmentAttach, "--filter", "label="+extension.ContainerLabelHost+"="+l.host); gw != "" {
		t.Errorf("the attach's gateway outlived its detach: %s", gw)
	}
}

// runOnTerminal runs argv on a pseudo-terminal, as an operator's attach
// runs on theirs, and returns what it wrote.
func runOnTerminal(argv, env []string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("script", append([]string{"-q", "/dev/null"}, argv...)...)
	} else {
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		cmd = exec.Command("script", "-qefc", strings.Join(quoted, " "), "/dev/null")
	}
	cmd.Env = append(append([]string{}, env...), "TERM=xterm")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
