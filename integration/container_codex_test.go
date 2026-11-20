package integration_test

// Real Docker tests for codex in a container: a turn that calls its
// toolbelt, a resumed turn in a new container, and a grant that doesn't
// allow the model's API, with a stand-in codex in the test image (make
// container-test and the container CI job). They call no real service.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/codex"
)

// fakeContainerCodex stands in for `codex exec --json` in the test image.
// Each turn records where it ran and what it was given, calls the
// toolbelt with `os context` from the environment codex hands its MCP
// server, tries to read the operator's token, starts thread-1 on a fresh
// turn, writes its session under CODEX_HOME and ends with a review status.
const fakeContainerCodex = `#!/bin/sh
f=${FAKE_DIR:?}
resume=0 last= prev=
for a in "$@"; do
  [ "$prev" = --output-last-message ] && last=$a
  [ "$a" = resume ] && resume=1
  prev=$a
done
cat >/dev/null
where=host; [ -f /.dockerenv ] && where=container
private=hidden; cat "$LAB_STATE/private/operator.token" >/dev/null 2>&1 && private=read
printf 'resume=%s where=%s home=%s codex_home=%s key=%s private=%s\n' "$resume" "$where" "$HOME" "$CODEX_HOME" "$CODEX_API_KEY" "$private" >> "$f/invocations"
os context >> "$f/context" 2>&1
[ $resume = 0 ] && echo '{"type":"thread.started","thread_id":"thread-1"}'
mkdir -p "$CODEX_HOME/sessions" && echo '{}' >> "$CODEX_HOME/sessions/rollout-thread-1.jsonl"
echo '{"type":"turn.started"}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
printf '{"state":"review","summary":"ok","handoff":"note","report":""}' > "$last"
`

// TestDockerCodex: a codex binding runs in a container with the Work's
// home as HOME and CODEX_HOME in it, its API key present inside but not in
// docker inspect, and its toolbelt reachable from what codex hands its MCP
// server; it can't read the state directory's private/. The next segment
// resumes the thread from the home in a new container. A grant whose
// network doesn't allow the model's API refuses the launch, and nothing
// runs.
func TestDockerCodex(t *testing.T) {
	l := newDockerLab(t)
	bin, fake := filepath.Join(l.root, "bin"), filepath.Join(l.root, "fake")
	execState := filepath.Join(l.state, "executors", "codex")
	home := filepath.Join(l.state, "homes", "work")
	for _, d := range []string{bin, fake, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fakeContainerCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := codex.New(codex.Options{StateDir: execState, Environ: func() []string { return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.home} }})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Capabilities().Container {
		t.Fatal("the codex executor does not advertise containers")
	}
	cfg, _ := json.Marshal(map[string]any{"binary": filepath.Join(bin, "codex"), "env": map[string]string{"FAKE_DIR": fake, "LAB_STATE": l.state},
		"secret_env": map[string]string{"openai": "CODEX_API_KEY"}})
	const key = "sk-test-secret"
	turn := func(id domain.ID, allow string) extension.Result {
		t.Helper()
		_, socket, tokenFile, token := l.labToolbelt(t, id)
		grant := `{"image":"` + dockerTestImage + `","write":["{workspace}","` + fake + `"],"read":["` + bin + `"],"network":{"allow":["` + allow + `"]},"provider":{"context":"` + l.dctx + `"}}`
		b := extension.BindContainer(extension.ContainerBinding{Provider: l.provider, Owner: extension.ResourceOwner{ScopeID: "scope", WorkID: "work", ExecutionID: id},
			Config: domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(grant)}, Host: l.host, Segment: 1,
			Protected: []string{l.state, l.config}, Shared: []string{filepath.Dir(socket)}, Toolbelt: socket,
			RunRoot: filepath.Join(execState, "runs"), Records: filepath.Join(l.state, "runs", string(id), "container"),
			Home: l.home, WorkHome: home,
			Pin: func(c extension.ContainerPin) error {
				l.mu.Lock()
				l.pins = append(l.pins, c)
				l.mu.Unlock()
				return nil
			}})
		r := extension.RunRequest{Work: domain.Work{ID: "work", ScopeID: "scope"}, Execution: domain.Execution{ID: id, WorkID: "work", Attempt: 1, Lease: domain.Lease{Epoch: 1}},
			Trigger: domain.Event{ID: domain.ID("ev-" + id), Type: "note"},
			Context: extension.ExecutionContext{Config: domain.Payload{Data: cfg}, Secrets: map[string]string{"openai": key},
				Container: b, ContainerHome: home, Resources: b.Resources(dockerWorkspace{l.ws}),
				ToolbeltEndpoint: "unix://" + socket, ToolbeltToken: token, ToolbeltTokenFile: tokenFile, ToolbeltDir: bin},
			PersistRecovery: func(domain.Payload) error { return nil }}
		h, err := e.Run(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		return b.Settle(waitResult(t, h))
	}
	invocations := func() []string {
		raw, _ := os.ReadFile(filepath.Join(fake, "invocations"))
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}

	if res := turn("codex-1", "api.openai.com"); res.Outcome.Kind != domain.OutcomeHandoff {
		t.Fatalf("the first turn: %+v %s\n%v", res.Outcome, res.Outcome.Data.Data, invocations())
	}
	if inv := invocations(); len(inv) != 1 || inv[0] != "resume=0 where=container home="+home+" codex_home="+home+"/.codex key="+key+" private=hidden" {
		t.Fatalf("the first turn ran as %v", inv)
	}
	if raw, _ := os.ReadFile(filepath.Join(fake, "context")); !strings.Contains(string(raw), "the forward") {
		t.Errorf("the toolbelt from codex's environment: %q", raw)
	}
	pin := l.pinned()
	if env, err := l.docker("inspect", "--format", "{{json .Config.Env}} {{json .Args}}", dockerID(pin.ID)); err != nil || strings.Contains(env, key) {
		t.Errorf("docker inspect shows the key (%v): %s", err, env)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "sessions", "rollout-thread-1.jsonl")); err != nil {
		t.Errorf("the session is not in the Work's home: %v", err)
	}

	if res := turn("codex-2", "api.openai.com"); res.Outcome.Kind != domain.OutcomeHandoff {
		t.Fatalf("the second turn: %+v %s\n%v", res.Outcome, res.Outcome.Data.Data, invocations())
	}
	if inv := invocations(); len(inv) != 2 || !strings.HasPrefix(inv[1], "resume=1 where=container") {
		t.Fatalf("the second turn did not resume: %v", inv)
	}
	if second := l.pinned(); second.ID == pin.ID {
		t.Error("the second turn ran in the first's container")
	}

	res := turn("codex-3", "allowed.test")
	var f struct{ Reason string }
	_ = json.Unmarshal(res.Outcome.Data.Data, &f)
	if res.Outcome.Kind != domain.OutcomeFailed || f.Reason != "sandbox_denied" {
		t.Fatalf("a grant without the model's API: %+v %s", res.Outcome, res.Outcome.Data.Data)
	}
	if inv := invocations(); len(inv) != 2 {
		t.Fatalf("codex ran under a grant without its API: %v", inv)
	}
}
