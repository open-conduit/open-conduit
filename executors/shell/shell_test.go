package shell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/executors/shell"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// memoryArtifacts is a minimal public-contract artifact store for unit tests.
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
func (s *memoryArtifacts) content(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.data[id])
}

func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shell-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newExecutor(t *testing.T, dir string, art storage.Artifacts, environ ...string) *shell.Executor {
	t.Helper()
	e, err := shell.New(shell.Options{StateDir: dir, Artifacts: art, Environ: func() []string { return environ }, Grace: 300 * time.Millisecond, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func request(id string, epoch uint64, config string) (extension.RunRequest, *domain.Payload) {
	saved := &domain.Payload{}
	r := extension.RunRequest{
		Work:      domain.Work{ID: "work/1", ScopeID: "ops"},
		Execution: domain.Execution{ID: domain.ID(id), WorkID: "work/1", Attempt: 1, Lease: domain.Lease{Owner: "test", Epoch: epoch}},
		Trigger:   domain.Event{ID: "trigger", Type: "fixture.requested", Data: domain.Payload{Schema: "fixture.v1", Data: json.RawMessage(`{"n":1}`)}},
		Context:   extension.ExecutionContext{Config: domain.Payload{Schema: "open-conduit.binding.config.v1", Data: json.RawMessage(config)}},
	}
	r.Inputs = []domain.Event{r.Trigger}
	r.PersistRecovery = func(p domain.Payload) error { *saved = p; return nil }
	return r, saved
}

func run(t *testing.T, e *shell.Executor, r extension.RunRequest) extension.Result {
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
	return res
}

type fields struct {
	Reason   string            `json:"reason"`
	ExitCode *int              `json:"exit_code"`
	Log      string            `json:"log"`
	Outputs  map[string]string `json:"outputs"`
}

func data(t *testing.T, res extension.Result) fields {
	t.Helper()
	var f fields
	if res.Outcome.Data.Schema != shell.ResultSchema || json.Unmarshal(res.Outcome.Data.Data, &f) != nil {
		t.Fatalf("unstructured result: %+v", res.Outcome)
	}
	return f
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 && bytes.HasSuffix(b, []byte("\n")) {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never written", filepath.Base(path))
	return ""
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("process %d survived cleanup", pid)
	}
}

func TestSharedExecutorConformance(t *testing.T) {
	dir := stateDir(t)
	art := &memoryArtifacts{data: map[string][]byte{}}
	r, _ := request("exec-conformance", 1, `{"command":"printf hello"}`)
	conformance.RunExecutor(t, func() extension.Executor { return newExecutor(t, dir, art) }, r)
}

func TestCompletedResultLogAndOutputs(t *testing.T) {
	art := &memoryArtifacts{data: map[string][]byte{}}
	e := newExecutor(t, stateDir(t), art)
	r, saved := request("exec-ok", 1, `{"command":"echo out; echo err >&2; printf '{\"ok\":true}' > \"$OS_OUTPUT_DIR/report.json\"; cat \"$OS_INPUT_FILE\" > \"$OS_OUTPUT_DIR/input.json\"; pwd > \"$OS_OUTPUT_DIR/pwd.txt\""}`)
	res := run(t, e, r)
	if saved.Schema != shell.RecoverySchema {
		t.Fatalf("recovery metadata not persisted: %+v", saved)
	}
	f := data(t, res)
	if res.Outcome.Kind != domain.OutcomeCompleted || f.Reason != "exit" || f.ExitCode == nil || *f.ExitCode != 0 {
		t.Fatalf("result: %+v %+v", res.Outcome, f)
	}
	if got := art.content(f.Log); got != "out\nerr\n" {
		t.Fatalf("combined log %q", got)
	}
	if len(res.Artifacts) != 4 || art.content(f.Outputs["report.json"]) != `{"ok":true}` {
		t.Fatalf("outputs: %+v %+v", res.Artifacts, f.Outputs)
	}
	var input struct {
		Schema  string
		Trigger struct{ ID, Type string }
		Inputs  []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(art.content(f.Outputs["input.json"])), &input); err != nil || input.Schema != shell.InputSchema || input.Trigger.Type != "fixture.requested" || len(input.Inputs) != 1 {
		t.Fatalf("input file: %v %+v", err, input)
	}
	if pwd := strings.TrimSpace(art.content(f.Outputs["pwd.txt"])); !strings.HasSuffix(pwd, "/work") {
		t.Fatalf("no-workspace scratch dir: %s", pwd)
	}
}

func TestEnvironmentLayers(t *testing.T) {
	art := &memoryArtifacts{data: map[string][]byte{}}
	e := newExecutor(t, stateDir(t), art, "PATH=/usr/bin:/bin", "HOME=/home/x", "LC_ALL=C", "AWS_SECRET_ACCESS_KEY=leak", "OPEN_CONDUIT_TOKEN_FILE=/x", "OPEN_SUPERVISOR_TOKEN=y", "OS_TOKEN=forged", "OS_ENDPOINT=unix:///forged")
	r, _ := request("exec-env", 1, `{"command":"env > \"$OS_OUTPUT_DIR/env.txt\"","env":{"MODE":"fixture","HOME":"/override"},"secret_env":{"api":"API_KEY"}}`)
	r.Context.Secrets = map[string]string{"api": "s1", "support-key": "s2"}
	r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken = "unix:///tmp/oc/management.sock", "token-1"
	res := run(t, e, r)
	env := map[string]string{}
	for _, line := range strings.Split(art.content(data(t, res).Outputs["env.txt"]), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	for k, want := range map[string]string{"HOME": "/override", "LC_ALL": "C", "MODE": "fixture", "API_KEY": "s1", "SUPPORT_KEY": "s2", "OS_ENDPOINT": "unix:///tmp/oc/management.sock", "OS_TOKEN": "token-1", "OS_WORK_ID": "work/1", "OS_EXECUTION_ID": "exec-env", "OS_SCOPE_ID": "ops", "OS_TRIGGER_TYPE": "fixture.requested", "OS_ATTEMPT": "1"} {
		if env[k] != want {
			t.Fatalf("%s = %q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{"AWS_SECRET_ACCESS_KEY", "OPEN_CONDUIT_TOKEN_FILE", "OPEN_SUPERVISOR_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Fatalf("baseline leaked %s", k)
		}
	}
	if env["OS_TOKEN_FILE"] == "" || !strings.HasPrefix(env["PATH"], "/usr/bin") {
		t.Fatalf("toolbelt/path: %q %q", env["OS_TOKEN_FILE"], env["PATH"])
	}
	// Without toolbelt credentials none of the transport variables appear.
	r2, _ := request("exec-env-none", 1, `{"command":"env > \"$OS_OUTPUT_DIR/env.txt\""}`)
	if out := art.content(data(t, run(t, e, r2)).Outputs["env.txt"]); strings.Contains(out, "OS_TOKEN") || strings.Contains(out, "OS_ENDPOINT") {
		t.Fatal("transport variables without a toolbelt token")
	}
}

func TestConfigValidation(t *testing.T) {
	e := newExecutor(t, stateDir(t), nil)
	for _, bad := range []string{`{}`, `{"command":"a","argv":["b"]}`, `{"command":"a","extra":1}`, `{"argv":[""]}`, `{"command":"a","dir":"rel"}`, `{"command":"a","env":{"OS_TOKEN":"x"}}`, `{"command":"a","env":{"1BAD":"x"}}`, `{"command":"a","secret_env":{"api":"OS_X"}}`, `{"command":"a","timeout_seconds":-1}`, `{"command":"a"} {}`} {
		if err := e.ValidateConfig(json.RawMessage(bad), []string{"api"}); !errors.Is(err, shell.ErrConfig) {
			t.Fatalf("accepted %s: %v", bad, err)
		}
	}
	if err := e.ValidateConfig(json.RawMessage(`{"command":"a","secret_env":{"other":"X"}}`), []string{"api"}); err == nil {
		t.Fatal("secret_env for an unselected secret accepted")
	}
	if err := e.ValidateConfig(json.RawMessage(`{"command":"a"}`), []string{"os_token"}); err == nil {
		t.Fatal("secret defaulting to a reserved variable accepted")
	}
	if err := e.ValidateConfig(json.RawMessage(`{"argv":["/bin/echo","x"],"dir":"/tmp","env":{"A":"1"},"timeout_seconds":5}`), nil); err != nil {
		t.Fatal(err)
	}
	// A config that bypassed validation fails structurally without a process.
	r, saved := request("exec-badconfig", 1, `{"command":""}`)
	res := run(t, e, r)
	if res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != "invalid_config" || saved.Schema == "" {
		t.Fatalf("invalid config result: %+v", res.Outcome)
	}
	r, _ = request("exec-nostart", 1, `{"argv":["/nonexistent/program"]}`)
	if res = run(t, e, r); res.Outcome.Kind != domain.OutcomeFailed {
		t.Fatalf("missing program: %+v", res.Outcome)
	}
}

func TestExitFailureAndTreeCleanup(t *testing.T) {
	dir := stateDir(t)
	e := newExecutor(t, dir, nil)
	child := filepath.Join(dir, "child.pid")
	r, _ := request("exec-exit", 1, `{"command":"sleep 30 & echo $! > `+child+`; exit 3"}`)
	res := run(t, e, r)
	f := data(t, res)
	if res.Outcome.Kind != domain.OutcomeFailed || f.Reason != "exit" || f.ExitCode == nil || *f.ExitCode != 3 || res.Outcome.Summary != "exit status 3" {
		t.Fatalf("exit failure: %+v %+v", res.Outcome, f)
	}
	pid, _ := strconv.Atoi(waitFile(t, child))
	waitDead(t, pid)
}

func TestTimeoutKillsTree(t *testing.T) {
	dir := stateDir(t)
	e := newExecutor(t, dir, nil)
	child := filepath.Join(dir, "child.pid")
	// The background child ignores TERM, forcing SIGKILL escalation.
	r, _ := request("exec-timeout", 1, `{"command":"(trap '' TERM; sleep 30) & echo $! > `+child+`; sleep 30","timeout_seconds":1}`)
	start := time.Now()
	res := run(t, e, r)
	f := data(t, res)
	if res.Outcome.Kind != domain.OutcomeFailed || f.Reason != "timeout" || res.Outcome.Summary != "shell command timed out" {
		t.Fatalf("timeout: %+v %+v", res.Outcome, f)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout not enforced promptly")
	}
	pid, _ := strconv.Atoi(waitFile(t, child))
	waitDead(t, pid)
}

func TestInterruptStructuredResult(t *testing.T) {
	dir := stateDir(t)
	e := newExecutor(t, dir, nil)
	child := filepath.Join(dir, "child.pid")
	r, _ := request("exec-int", 1, `{"command":"sleep 30 & echo $! > `+child+`; wait"}`)
	h, err := e.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(waitFile(t, child))
	if err = e.Interrupt(context.Background(), "exec-int"); err != nil {
		t.Fatal(err)
	}
	if err = e.Interrupt(context.Background(), "unknown"); !errors.Is(err, shell.ErrUnknownExecution) {
		t.Fatalf("interrupt of unowned execution: %v", err)
	}
	res, err := h.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != "interrupted" {
		t.Fatalf("interrupt: %+v", res.Outcome)
	}
	waitDead(t, pid)
	if err = e.Stop(context.Background(), "exec-int", 1); err != nil {
		t.Fatalf("stop after finish: %v", err)
	}
}

// TestAdoptionAcrossInstances launches with one executor instance and adopts
// with another sharing only the state directory, as after a daemon restart.
func TestAdoptionAcrossInstances(t *testing.T) {
	dir := stateDir(t)
	art := &memoryArtifacts{data: map[string][]byte{}}
	first := newExecutor(t, dir, art)
	release, launches := filepath.Join(dir, "release"), filepath.Join(dir, "launches")
	r, saved := request("exec-adopt", 1, `{"command":"echo launch >> `+launches+`; while [ ! -e `+release+` ]; do sleep 0.02; done; cat \"$OS_TOKEN_FILE\" > \"$OS_OUTPUT_DIR/token.txt\""}`)
	r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken = "unix:///tmp/x.sock", "old-token"
	if _, err := first.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	waitFile(t, launches)
	second := newExecutor(t, dir, art)
	stale := r
	stale.Execution.Lease.Epoch = 0
	if _, err := second.Recover(context.Background(), stale, *saved); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("lower epoch adopted: %v", err)
	}
	r.Execution.Lease.Epoch = 2
	r.Context.ToolbeltToken = "new-token"
	r.PersistRecovery = func(p domain.Payload) error {
		if !reflect.DeepEqual(p, *saved) {
			return errors.New("recovery must not relaunch")
		}
		return nil
	}
	h, err := second.Recover(context.Background(), r, *saved)
	if err != nil {
		t.Fatal(err)
	}
	// The superseded instance can no longer terminate the adopted process.
	if err = first.Interrupt(context.Background(), "exec-adopt"); !errors.Is(err, shell.ErrSuperseded) {
		t.Fatalf("superseded interrupt: %v", err)
	}
	if err = first.Stop(context.Background(), "exec-adopt", 1); !errors.Is(err, shell.ErrSuperseded) {
		t.Fatalf("superseded stop: %v", err)
	}
	if err = os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := h.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := data(t, res)
	if res.Outcome.Kind != domain.OutcomeCompleted || strings.TrimSpace(art.content(f.Outputs["token.txt"])) != "new-token" {
		t.Fatalf("adopted result: %+v token=%q", res.Outcome, art.content(f.Outputs["token.txt"]))
	}
	if b, _ := os.ReadFile(launches); string(b) != "launch\n" {
		t.Fatalf("duplicate launch: %q", b)
	}
	// A third instance returns the identical finished result.
	again, err := newExecutor(t, dir, art).Recover(context.Background(), r, *saved)
	if err != nil {
		t.Fatal(err)
	}
	if res2, err := again.Wait(context.Background()); err != nil || !reflect.DeepEqual(res2, res) {
		t.Fatal("recovered result changed")
	}
}

// TestIdentityBeyondPID never adopts or signals a live process that merely
// reuses a recorded PID.
func TestIdentityBeyondPID(t *testing.T) {
	dir := stateDir(t)
	e := newExecutor(t, dir, nil)
	r, saved := request("exec-identity", 1, `{"command":"exit 0"}`)
	if res := run(t, e, r); res.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatal(res.Outcome)
	}
	var meta struct{ Run string }
	_ = json.Unmarshal(saved.Data, &meta)
	seg := filepath.Join(dir, "runs", meta.Run)
	// Simulate a crash before the exit was recorded and PID reuse by an
	// unrelated process group leader.
	unrelated := exec.Command("sleep", "30")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { unrelated.Process.Kill(); unrelated.Wait() }()
	for _, name := range []string{"result.json", "exit"} {
		os.Remove(filepath.Join(seg, name))
	}
	pid := []byte(strconv.Itoa(unrelated.Process.Pid) + "\n")
	os.WriteFile(filepath.Join(seg, "pid"), pid, 0o600)
	os.WriteFile(filepath.Join(seg, "process.json"), []byte(`{"pid":`+strconv.Itoa(unrelated.Process.Pid)+`}`), 0o600)
	r.Execution.Lease.Epoch = 2
	fresh := newExecutor(t, dir, nil)
	if _, err := fresh.Recover(context.Background(), r, *saved); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Fatalf("PID-only identity adopted: %v", err)
	}
	if err := fresh.Stop(context.Background(), "exec-identity", 2); err != nil {
		t.Fatalf("stop of dead segment: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !alive(unrelated.Process.Pid) {
		t.Fatal("unrelated process was signaled")
	}
	// Tampered metadata and unknown state are never adopted.
	for _, p := range []domain.Payload{{}, {Schema: shell.RecoverySchema, Data: []byte(`{"run":"../../x/y"}`)}, {Schema: "other.v1", Data: []byte(`{}`)}} {
		if _, err := fresh.Recover(context.Background(), r, p); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopted %+v: %v", p, err)
		}
	}
}

func TestResumeIsExplicit(t *testing.T) {
	e := newExecutor(t, stateDir(t), nil)
	if e.Capabilities().Resume {
		t.Fatal("resume advertised by default")
	}
	r, _ := request("exec-resume", 1, `{"command":"true"}`)
	if _, err := e.Resume(context.Background(), r, domain.Payload{}); !errors.Is(err, extension.ErrUnsupported) {
		t.Fatalf("resume without capability: %v", err)
	}
	resumable, err := shell.New(shell.Options{StateDir: stateDir(t), Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := resumable.Capabilities(); !c.Resume || !c.Interrupt || c.Toolbelt != extension.ToolbeltCLI || c.Workspace || c.Inject {
		t.Fatalf("capabilities: %+v", c)
	}
	if err = extension.ValidateBinding(domain.Binding{Requires: domain.Capabilities{Resume: true, Interrupt: true, Toolbelt: "cli"}}, resumable); err != nil {
		t.Fatal(err)
	}
}
