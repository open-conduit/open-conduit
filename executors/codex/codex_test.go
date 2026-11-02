package codex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/codex"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// The test binary is also the segment driver: the executor runs it as
// `<test binary> segment <run dir>`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == codex.SegmentCommand {
		os.Exit(codex.RunSegment(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// fakeCodex plays `codex exec --json`: it records each launch's arguments,
// stdin and environment as call-N, stdin-N and env-N, starts thread-1 on a
// fresh launch, waits on a fresh launch for FAKE_BARRIER when it is set,
// fails with FAKE_FAIL when set, and otherwise ends with FAKE_STATE (default
// review) written to --output-last-message.
const fakeCodex = `#!/bin/sh
d="$FAKE_DIR"
n=1; while [ -e "$d/call-$n" ]; do n=$((n+1)); done
printf '%s\n' "$@" > "$d/call-$n"
cat > "$d/stdin-$n"
env > "$d/env-$n"
last=; prev=; resume=0
for a in "$@"; do
  [ "$prev" = --output-last-message ] && last=$a
  [ "$a" = resume ] && resume=1
  prev=$a
done
[ $resume = 0 ] && echo '{"type":"thread.started","thread_id":"thread-1"}'
echo '{"type":"turn.started"}'
: > "$d/started-$n"
if [ $resume = 0 ] && [ -n "$FAKE_BARRIER" ]; then
  while [ ! -e "$FAKE_BARRIER" ]; do sleep 0.02; done
fi
if [ -n "$FAKE_FAIL" ]; then
  printf '{"type":"turn.failed","error":{"message":"%s"}}\n' "$FAKE_FAIL"
  echo "codex failed" >&2
  exit 1
fi
echo '{"type":"item.completed","item":{"type":"mcp_tool_call"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3}}'
printf '{"state":"%s","summary":"turn %s","handoff":"","report":""}' "${FAKE_STATE:-review}" "$n" > "$last"
`

type fixture struct {
	t     *testing.T
	state string
	fake  string
	bin   string
	art   *memoryArtifacts
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := os.MkdirTemp("", "oc-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := &fixture{t: t, state: filepath.Join(root, "state"), fake: filepath.Join(root, "fake"), art: &memoryArtifacts{data: map[string][]byte{}}}
	if err := os.MkdirAll(f.fake, 0o700); err != nil {
		t.Fatal(err)
	}
	f.bin = filepath.Join(root, "codex")
	if err := os.WriteFile(f.bin, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) executor() *codex.Executor {
	f.t.Helper()
	self, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := codex.New(codex.Options{StateDir: f.state, Artifacts: f.art, Driver: []string{self, codex.SegmentCommand},
		PollInterval: 20 * time.Millisecond, Grace: 200 * time.Millisecond,
		Environ: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "OPENAI_API_KEY=leak", "OS_TOKEN=leak"}
		}})
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

// config is a binding config running the fake, with env added.
func (f *fixture) config(extra map[string]any, env map[string]string) domain.Payload {
	vars := map[string]string{"FAKE_DIR": f.fake}
	for k, v := range env {
		vars[k] = v
	}
	c := map[string]any{"binary": f.bin, "env": vars}
	for k, v := range extra {
		c[k] = v
	}
	b, _ := json.Marshal(c)
	return domain.Payload{Data: b}
}

func request(work, execution string, config domain.Payload, inputs ...domain.Event) extension.RunRequest {
	w := domain.Work{ID: domain.ID(work), ScopeID: "scope", CorrelationKeys: []domain.CorrelationKey{{Source: "fixture", ExternalID: "issue-42"}}}
	return extension.RunRequest{Work: w, Execution: domain.Execution{ID: domain.ID(execution), Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Trigger: domain.Event{ID: "trigger"}, Inputs: inputs, Context: extension.ExecutionContext{Config: config},
		PersistRecovery: func(domain.Payload) error { return nil }}
}

func (f *fixture) read(name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.fake, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func run(t *testing.T, e *codex.Executor, r extension.RunRequest) extension.Result {
	t.Helper()
	h, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.ValidateResult(res); err != nil {
		t.Fatal(err)
	}
	return res
}

type fields struct {
	Status, Reason, Kind, Transcript, Log string
	Turns                                 int
	ToolCalls                             int `json:"tool_calls"`
	Limited                               bool
	Injected                              *struct{ Accepted, Consumed int }
}

func data(t *testing.T, res extension.Result) fields {
	t.Helper()
	var f fields
	if err := json.Unmarshal(res.Outcome.Data.Data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExecutorConformance(t *testing.T) {
	f := newFixture(t)
	conformance.RunExecutor(t, func() extension.Executor { return f.executor() }, request("work-c", "exec-c", f.config(nil, nil)))
}

// TestInjectConformance: an Event injected while the first turn runs is
// taken in by a follow-up `codex exec resume` turn on the same thread, in
// the same segment, and proved consumed.
func TestInjectConformance(t *testing.T) {
	f := newFixture(t)
	barrier := filepath.Join(f.fake, "go")
	conformance.RunInjectExecutor(t, func() conformance.InjectFixture {
		return conformance.InjectFixture{Executor: f.executor(), Request: request("work-i", "exec-i", f.config(nil, map[string]string{"FAKE_BARRIER": barrier})),
			Ready:  func() error { return waitFile(filepath.Join(f.fake, "started-1")) },
			Finish: func() error { return os.WriteFile(barrier, nil, 0o600) }}
	})
	follow := f.read("call-2")
	if !strings.HasPrefix(follow, "exec\nresume\n") || !strings.Contains(follow, "\nthread-1\n-\n") {
		t.Fatalf("follow-up turn:\n%s", follow)
	}
	if in := f.read("stdin-2"); !strings.Contains(in, "## Arrived during this turn") || !strings.Contains(in, "also this") {
		t.Fatalf("follow-up message:\n%s", in)
	}
	if _, err := os.Stat(filepath.Join(f.fake, "call-3")); err == nil {
		t.Fatal("the segment ran a third turn with nothing pending")
	}
}

func waitFile(path string) error {
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("%s never appeared", path)
}

// TestTurnsOnTheWorksThread: a Work's first turn starts a thread on the
// playbook, its status hands the Work back with core.turn.v1 data, and the
// next turn resumes that thread on the events since.
func TestTurnsOnTheWorksThread(t *testing.T) {
	f := newFixture(t)
	plugin := filepath.Join(f.fake, "plugin")
	if err := os.MkdirAll(filepath.Join(plugin, "skills", "work-issue"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "skills", "work-issue", "SKILL.md"), []byte("---\nname: work-issue\n---\n# Work an issue\n\nDo the work."), 0o600); err != nil {
		t.Fatal(err)
	}
	e := f.executor()
	cfg := f.config(map[string]any{"playbook": "work-issue", "plugin_dir": plugin, "model": "gpt-test"}, nil)
	comment := domain.Event{ID: "ev-1", Type: "github.issue.comment", Data: domain.Payload{Data: []byte(`{"author":"alex","body":"please fix"}`)}}
	res := run(t, e, request("work-1", "exec-1", cfg, comment))
	if res.Outcome.Kind != domain.OutcomeHandoff || res.Outcome.Summary != "turn 1" || res.Outcome.Data.Schema != domain.TurnSchema {
		t.Fatalf("first turn: %+v", res.Outcome)
	}
	d := data(t, res)
	if d.Status != "review" || d.Kind != "start" || d.Turns != 1 || d.ToolCalls != 1 || d.Transcript == "" {
		t.Fatalf("first turn data: %+v", d)
	}
	if res.Usage.InputTokens == nil || *res.Usage.InputTokens != 10 || *res.Usage.CachedTokens != 4 || *res.Usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	first := f.read("call-1")
	for _, want := range []string{"exec\n", "--model\ngpt-test\n", `sandbox_mode="danger-full-access"`, `approval_policy="never"`, "--json\n", "--output-schema\n", "\n-\n"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first launch lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "resume") || strings.Contains(first, "mcp_servers") {
		t.Fatalf("first launch resumed or had a toolbelt without one:\n%s", first)
	}
	in := f.read("stdin-1")
	if !strings.HasPrefix(in, "# Work an issue\n\nDo the work.\n\nARGUMENTS: 42\n\n## Already waiting\n\n### github.issue.comment by alex") || !strings.Contains(in, "please fix") {
		t.Fatalf("first message:\n%s", in)
	}

	res = run(t, e, request("work-1", "exec-2", cfg, domain.Event{ID: "ev-2", Type: "checks", Data: domain.Payload{Data: []byte(`{"state":"passed"}`)}}))
	if d := data(t, res); d.Kind != "events" || res.Outcome.Kind != domain.OutcomeHandoff {
		t.Fatalf("second turn: %+v %+v", res.Outcome, d)
	}
	if second := f.read("call-2"); !strings.HasPrefix(second, "exec\nresume\n") || !strings.HasSuffix(second, "\nthread-1\n-\n") {
		t.Fatalf("second launch:\n%s", second)
	}
	if in := f.read("stdin-2"); !strings.Contains(in, "ARGUMENTS: 42 events\n\n## Since your last turn") || !strings.Contains(in, "State: passed") {
		t.Fatalf("second message:\n%s", in)
	}

	// A finished status completes the Work.
	// A finished status completes the Work. Without plugin_dir the
	// playbook comes from the host's directory.
	finished := f.config(map[string]any{"playbook": "work-issue"}, map[string]string{"FAKE_STATE": "finished"})
	r := request("work-1", "exec-3", finished)
	r.Context.PlaybookDir = plugin
	if res = run(t, e, r); res.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("finished: %+v", res.Outcome)
	}
	if in := f.read("stdin-3"); !strings.HasPrefix(in, "# Work an issue") {
		t.Fatalf("host playbook:\n%s", in)
	}
	if res = run(t, e, request("work-2", "exec-4", finished)); res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != "invalid_config" {
		t.Fatalf("no playbook dir: %+v", res.Outcome)
	}
}

// TestToolbeltOverMCP: with a toolbelt, codex's config names `os mcp`
// with the endpoint and token file, and the token reaches neither codex's
// arguments nor its environment.
func TestToolbeltOverMCP(t *testing.T) {
	f := newFixture(t)
	r := request("work-t", "exec-t", f.config(map[string]any{"secret_env": map[string]string{"openai": "OPENAI_API_KEY"}}, nil))
	r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken, r.Context.ToolbeltDir = "unix:///tmp/toolbelt.sock", "secret-token", "/opt/oc"
	r.Context.Secrets = map[string]string{"openai": "sk-test"}
	run(t, f.executor(), r)
	args, env := f.read("call-1"), f.read("env-1")
	for _, want := range []string{`mcp_servers.open-conduit.command="/opt/oc/os"`, `mcp_servers.open-conduit.args=["mcp"]`, `OS_ENDPOINT="unix:///tmp/toolbelt.sock"`, `OS_TOKEN_FILE="`} {
		if !strings.Contains(args, want) {
			t.Fatalf("args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args+env, "secret-token") || strings.Contains(env, "leak") || strings.Contains(env, "OS_ENDPOINT") {
		t.Fatalf("credential leaked:\n%s\n%s", args, env)
	}
	if !strings.Contains(env, "OPENAI_API_KEY=sk-test") || !strings.Contains(env, "OS_WORK_ID=work-t") {
		t.Fatalf("environment:\n%s", env)
	}
}

func TestFailuresAndLimits(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	res := run(t, e, request("work-f", "exec-f", f.config(nil, map[string]string{"FAKE_FAIL": "model refused"})))
	if res.Outcome.Kind != domain.OutcomeFailed || res.Outcome.Summary != "model refused" || res.RetryHint != nil || data(t, res).Log == "" {
		t.Fatalf("failure: %+v %+v", res.Outcome, res.RetryHint)
	}
	res = run(t, e, request("work-l", "exec-l", f.config(nil, map[string]string{"FAKE_FAIL": "You've hit your usage limit"})))
	if res.Outcome.Kind != domain.OutcomeFailed || res.RetryHint == nil || !data(t, res).Limited {
		t.Fatalf("limit: %+v %+v", res.Outcome, res.RetryHint)
	}
	res = run(t, e, request("work-m", "exec-m", f.config(map[string]any{"binary": filepath.Join(f.fake, "missing")}, nil)))
	if res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != "start_failed" {
		t.Fatalf("missing binary: %+v", res.Outcome)
	}
	res = run(t, e, request("work-s", "exec-s", f.config(map[string]any{"structured": true}, map[string]string{"FAKE_STATE": "bogus"})))
	if res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != "no_status" {
		t.Fatalf("no status: %+v", res.Outcome)
	}
	res = run(t, e, request("work-u", "exec-u", f.config(map[string]any{"structured": false}, nil)))
	if res.Outcome.Kind != domain.OutcomeCompleted || res.Outcome.Summary != "done" || res.Outcome.Data.Schema != codex.ResultSchema {
		t.Fatalf("unstructured: %+v", res.Outcome)
	}
}

// TestInterruptAndAdoption: a second executor instance adopts a running
// segment without launching again, and an interrupt fails the turn.
func TestInterruptAndAdoption(t *testing.T) {
	f := newFixture(t)
	barrier := filepath.Join(f.fake, "never")
	r := request("work-a", "exec-a", f.config(nil, map[string]string{"FAKE_BARRIER": barrier}))
	var state domain.Payload
	r.PersistRecovery = func(p domain.Payload) error { state = p; return nil }
	if _, err := f.executor().Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := waitFile(filepath.Join(f.fake, "started-1")); err != nil {
		t.Fatal(err)
	}
	e2 := f.executor()
	r.Execution.Lease.Epoch = 2
	h, err := e2.Recover(context.Background(), r, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.Interrupt(context.Background(), "exec-a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != "interrupted" {
		t.Fatalf("interrupted: %+v", res.Outcome)
	}
	if _, err := os.Stat(filepath.Join(f.fake, "call-2")); err == nil {
		t.Fatal("adoption launched codex again")
	}
	r.Execution.Lease.Epoch = 1
	if _, err := f.executor().Recover(context.Background(), r, state); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("lower epoch recovered: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		config string
		ok     bool
	}{
		{`{}`, true},
		{`{"playbook":"work-issue","plugin_dir":"/p","sandbox":"workspace-write","extra_args":["--search"]}`, true},
		{`{"sandbox":"anything"}`, false},
		{`{"playbook":"Work Issue"}`, false},
		{`{"extra_args":["--json"]}`, false},
		{`{"extra_args":["-c","mcp_servers.open-conduit.command=\"x\""]}`, false},
		{`{"env":{"OS_TOKEN":"x"}}`, false},
		{`{"dir":"relative"}`, false},
		{`{"unknown":1}`, false},
	} {
		err := codex.ValidateConfig(json.RawMessage(tc.config), nil)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.config, err)
		}
	}
	if codex.ResourceMode(json.RawMessage(`{}`)) != "work" || codex.ResourceMode(json.RawMessage(`{"workspace_mode":"clone"}`)) != "clone" {
		t.Fatal("resource mode")
	}
}

// memoryArtifacts is a minimal public-contract artifact store.
type memoryArtifacts struct {
	mu   sync.Mutex
	n    int
	data map[string][]byte
}
type memoryWriter struct {
	s     *memoryArtifacts
	media string
	buf   bytes.Buffer
}

func (w *memoryWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memoryWriter) Close() error                { return nil }
func (w *memoryWriter) Commit(context.Context) (storage.ArtifactRef, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.n++
	id := fmt.Sprintf("artifact-%d", w.s.n)
	w.s.data[id] = append([]byte(nil), w.buf.Bytes()...)
	return storage.ArtifactRef{ID: id, Size: int64(w.buf.Len()), MediaType: w.media}, nil
}
func (s *memoryArtifacts) Create(_ context.Context, media string) (storage.ArtifactWriter, error) {
	return &memoryWriter{s: s, media: media}, nil
}
func (s *memoryArtifacts) Open(_ context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[ref.ID]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (s *memoryArtifacts) Delete(context.Context, storage.ArtifactRef) error { return nil }
