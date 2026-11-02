// Package codex is the Codex CLI executor. Each turn runs as a detached
// segment: a process group, restart-safe like the shell executor's, whose
// leader drives `codex exec --json` on the Work's thread. The agent reaches
// its toolbelt over MCP (`os mcp`, configured per segment), the Work's
// thread is resumed on its next turn (`codex exec resume`), and an Event
// injected while a turn runs becomes a follow-up turn on the same thread
// within the same segment. The package imports only public contracts; its
// process identity helpers are copied from executors/shell, because one
// executor may not import another.
package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	// Name is the executor's registration name.
	Name = "codex"
	// RecoverySchema identifies the opaque metadata persisted before launch.
	RecoverySchema = "open-conduit.codex.recovery.v1"
	// ResultSchema identifies outcome data without a turn status.
	ResultSchema = "open-conduit.codex.result.v1"
	// marker prefixes the wrapper's $0 so process identity is checked by an
	// unguessable per-segment nonce in the argument vector, not by PID alone.
	marker = "open-conduit-codex:"
	// LogMediaType is codex's stderr artifact; TranscriptMediaType its
	// JSONL events.
	LogMediaType        = "text/plain; charset=utf-8"
	TranscriptMediaType = "application/x-ndjson"
	// mcpServer is the toolbelt's MCP server name in codex's config.
	mcpServer = "open-conduit"
)

// wrapper records its PID, runs the driver with the group's termination
// signals trapped (so they reach it with default disposition), then
// signals any remaining group members and atomically records the exit
// status.
const wrapper = `d=$1; shift
printf '%s\n' "$$" > "$d/pid.tmp" && mv -f "$d/pid.tmp" "$d/pid"
trap ':' HUP INT TERM
"$@"
c=$?
trap '' HUP INT TERM
kill -TERM 0 2>/dev/null
printf '%s\n' "$c" > "$d/exit.tmp" && mv -f "$d/exit.tmp" "$d/exit"
exit "$c"`

var (
	// ErrUnknownExecution means this executor instance holds no live
	// segment for the execution.
	ErrUnknownExecution = errors.New("no codex process owned for execution")
	// ErrSuperseded means a newer lease epoch adopted the process.
	ErrSuperseded = errors.New("codex process adopted by a newer lease epoch")
	// ErrNotStopped means the process tree could not be verified stopped.
	ErrNotStopped = errors.New("codex process tree not verified stopped")
	errNoExit     = errors.New("codex process ended without a recorded exit status")
	hexName       = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Options configure one executor registration.
type Options struct {
	// StateDir is a private absolute directory for segments and the
	// Works' threads.
	StateDir string
	// Artifacts receives each segment's transcript and log. Optional.
	Artifacts storage.Artifacts
	// Driver is the segment driver's argv; the run directory is appended.
	// Default: this executable's `segment` subcommand (RunSegment).
	Driver []string
	// ToolbeltDir holds `os` when the request names none
	// (ExecutionContext.ToolbeltDir).
	ToolbeltDir string
	// Environ supplies the process baseline before sanitization.
	Environ func() []string
	// Grace is the delay between SIGTERM and SIGKILL (default 10s).
	Grace time.Duration
	// PollInterval paces observation of adopted segments (default 1s).
	PollInterval time.Duration
	// Now is the wall clock (default time.Now).
	Now func() time.Time
}

// Executor implements extension.Executor with interrupt, inject, resume,
// stop and forget.
type Executor struct {
	o    Options
	mu   sync.Mutex
	runs map[domain.ID]*run
}

// New validates options and creates the private state directory.
func New(o Options) (*Executor, error) {
	if !processSupported {
		return nil, errors.New("codex executor requires a Unix platform")
	}
	if !filepath.IsAbs(o.StateDir) {
		return nil, errors.New("codex executor state directory must be absolute")
	}
	for _, d := range []string{"runs", "work"} {
		if err := os.MkdirAll(filepath.Join(o.StateDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	if info, err := os.Stat(o.StateDir); err != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("codex executor state directory must be owner-only (0700)")
	}
	if len(o.Driver) == 0 {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		o.Driver = []string{self, SegmentCommand}
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Grace <= 0 {
		o.Grace = 10 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Executor{o: o, runs: map[domain.ID]*run{}}, nil
}

// Capabilities are what the executor implements. Resume relaunches a
// woken attempt as a new segment on the same thread.
func Capabilities() domain.Capabilities {
	return domain.Capabilities{Interrupt: true, Inject: true, Resume: true, Toolbelt: extension.ToolbeltMCP, Workspace: true}
}

func (e *Executor) Capabilities() domain.Capabilities { return Capabilities() }

func executionKey(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type recovery struct {
	Run string `json:"run"`
}

// turnRecord is what finishing a segment needs to know about its turn.
type turnRecord struct {
	Work         domain.ID `json:"work"`
	Kind         string    `json:"kind"`
	Structured   bool      `json:"structured"`
	StatusSchema string    `json:"status_schema,omitempty"`
}

// workspaceRecord is the acquired resource's opaque lease reference and
// description, kept with the segment and reported in its result.
type workspaceRecord struct {
	Ref     string          `json:"ref"`
	Schema  string          `json:"schema,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Dir     string          `json:"dir,omitempty"`
	Summary string          `json:"summary,omitempty"`
}

// Run starts a new attempt segment.
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	return e.launch(ctx, r)
}

// Resume relaunches a parked attempt as a new segment on the Work's thread.
func (e *Executor) Resume(ctx context.Context, r extension.RunRequest, _ domain.Payload) (extension.RunHandle, error) {
	return e.launch(ctx, r)
}

// launch persists recovery intent before starting any process, so a crash
// can never leave an unacknowledged launch that recovery cannot find.
func (e *Executor) launch(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil || r.Execution.ID == "" {
		return nil, extension.ErrInvalid
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	key := executionKey(r.Execution.ID)
	dir := filepath.Join(e.o.StateDir, "runs", key, nonce)
	if err = os.MkdirAll(filepath.Join(dir, inboxDir, "consumed"), 0o700); err != nil {
		return nil, err
	}
	rn := &run{id: r.Execution.ID, dir: dir, nonce: nonce, epoch: r.Execution.Lease.Epoch}
	if err = writeLease(dir, rn.epoch); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	data, _ := json.Marshal(recovery{Run: key + "/" + nonce})
	if err = r.PersistRecovery(domain.Payload{Schema: RecoverySchema, Data: data}); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	cfg, err := ParseConfig(r.Context.Config.Data)
	if err != nil {
		return e.preset(ctx, rn, "invalid_config", "invalid codex binding config")
	}
	log, err := os.OpenFile(filepath.Join(dir, errFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare codex")
	}
	defer log.Close()
	workdir := cfg.Dir
	var layer map[string]string
	if r.Context.Resources != nil {
		if cfg.Dir != "" {
			return e.preset(ctx, rn, "invalid_config", "codex dir conflicts with the binding's workspace")
		}
		req := extension.ResourceRequest{Mode: ResourceMode(r.Context.Config.Data), Log: log}
		if cfg.Branch != "" {
			params, _ := json.Marshal(map[string]string{"branch": expand(cfg.Branch, r.Work)})
			req.Params = domain.Payload{Schema: "open-conduit.git.request.v1", Data: params}
		}
		res, err := r.Context.Resources.Acquire(ctx, req)
		if res.Ref != "" {
			_ = writeJSON(filepath.Join(dir, "workspace.json"), workspaceRecord{Ref: res.Ref, Schema: res.Data.Schema, Data: res.Data.Data, Kind: res.Kind, Dir: res.Dir, Summary: res.Summary})
		}
		switch {
		case errors.Is(err, extension.ErrResourceSetup):
			return e.preset(ctx, rn, "workspace_setup_failed", "workspace setup failed")
		case errors.Is(err, extension.ErrResourceExhausted):
			return e.preset(ctx, rn, "workspace_no_space", err.Error())
		case err != nil || res.Dir == "":
			return e.preset(ctx, rn, "workspace_unavailable", unavailable(err))
		}
		workdir, layer = res.Dir, res.Env
	} else if workdir != "" {
		if info, err := os.Stat(workdir); err != nil || !info.IsDir() {
			return e.preset(ctx, rn, "workspace_unavailable", "the binding's dir is not there")
		}
	} else {
		// A workspace-free binding's sessions live in a folder that stays
		// the same for the Work.
		workdir = filepath.Join(e.o.StateDir, "work", executionKey(r.Work.ID))
		if err = os.MkdirAll(workdir, 0o700); err != nil {
			return e.preset(ctx, rn, "start_failed", "could not prepare codex")
		}
	}
	rn.timeout = cfg.TimeoutSeconds
	if cfg.TimeoutSeconds > 0 {
		rn.deadline = e.o.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second).UTC()
	}
	in := intent{ExecutionID: string(r.Execution.ID), Nonce: nonce, StartedAt: e.o.Now().UTC(), TimeoutSeconds: cfg.TimeoutSeconds}
	if !rn.deadline.IsZero() {
		in.Deadline = &rn.deadline
	}
	if err = writeJSON(filepath.Join(dir, "intent.json"), in); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare codex")
	}
	// The turn: a Work with a thread resumes it on the events since its
	// last turn; one without starts a new thread.
	thread, err := e.loadThread(r.Work.ID)
	if err != nil {
		return e.preset(ctx, rn, "start_failed", "could not read the Work's thread")
	}
	kind := kindStart
	if thread != "" {
		kind = kindEvents
	}
	plugins := cfg.PluginDir
	if plugins == "" {
		plugins = r.Context.PlaybookDir
	}
	playbook, err := readPlaybook(plugins, cfg.Playbook)
	if err != nil {
		return e.preset(ctx, rn, "invalid_config", "the binding's playbook "+cfg.Playbook+" was not found")
	}
	args, text := body(kind, expand(cfg.Args, r.Work), RenderEvents(r.Inputs))
	text = withAdditions(text, projectAdditions(workdir, cfg.Playbook))
	if err = writeFile(filepath.Join(dir, promptFile), []byte(prompt(playbook, args, text))); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare codex")
	}
	spec := segmentSpec{Binary: cfg.Binary, Thread: thread}
	if *cfg.Structured {
		schema, _ := json.Marshal(StatusSchema)
		spec.Schema = filepath.Join(dir, "status-schema.json")
		if err = writeFile(spec.Schema, schema); err != nil {
			return e.preset(ctx, rn, "start_failed", "could not prepare codex")
		}
	}
	if spec.Args, err = e.codexArgs(cfg, r, dir); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare the toolbelt")
	}
	if err = writeJSON(filepath.Join(dir, specFile), spec); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare codex")
	}
	if err = writeJSON(filepath.Join(dir, "turn.json"), turnRecord{Work: r.Work.ID, Kind: string(kind), Structured: *cfg.Structured, StatusSchema: domain.TurnSchema}); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare codex")
	}
	env, err := e.environment(cfg, r, dir, layer)
	if err != nil {
		return e.preset(ctx, rn, "invalid_config", "invalid codex binding config")
	}
	argv := append(append([]string{}, e.o.Driver...), dir)
	cmd := exec.Command("/bin/sh", append([]string{"-c", wrapper, marker + nonce, dir}, argv...)...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = workdir, env, log, log
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not start codex")
	}
	rn.pid, rn.done = cmd.Process.Pid, make(chan struct{})
	_ = writeJSON(filepath.Join(dir, "process.json"), processRecord{PID: rn.pid})
	go func() {
		state, _ := cmd.Process.Wait()
		rn.mu.Lock()
		rn.state = state
		rn.mu.Unlock()
		close(rn.done)
	}()
	e.register(rn)
	return &handle{e: e, r: rn}, nil
}

// codexArgs are the options every turn of the segment passes: the
// sandbox, no approvals, the model, the toolbelt's MCP server and the
// binding's extra arguments.
func (e *Executor) codexArgs(cfg Config, r extension.RunRequest, dir string) ([]string, error) {
	args := []string{"--skip-git-repo-check", "-c", "sandbox_mode=" + tomlString(cfg.Sandbox), "-c", `approval_policy="never"`}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	toolbelt := r.Context.ToolbeltDir
	if toolbelt == "" {
		toolbelt = e.o.ToolbeltDir
	}
	if r.Context.ToolbeltToken != "" && r.Context.ToolbeltEndpoint != "" && toolbelt != "" {
		token := r.Context.ToolbeltTokenFile
		if token == "" {
			token = filepath.Join(dir, "token")
			if err := writeFile(token, []byte(r.Context.ToolbeltToken+"\n")); err != nil {
				return nil, err
			}
		}
		// The token itself never appears in codex's config or environment:
		// `os mcp` reads the file, which adoption replaces.
		prefix := "mcp_servers." + mcpServer + "."
		args = append(args,
			"-c", prefix+"command="+tomlString(filepath.Join(toolbelt, "os")),
			"-c", prefix+`args=["mcp"]`,
			"-c", prefix+"env={OS_ENDPOINT="+tomlString(r.Context.ToolbeltEndpoint)+",OS_TOKEN_FILE="+tomlString(token)+"}")
	}
	return append(args, cfg.ExtraArgs...), nil
}

// tomlString quotes a value as a TOML basic string (JSON's escapes are
// TOML's).
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// preset finishes a segment that never started a process with a structured
// failed result, so no uncertain resource hold is left behind.
func (e *Executor) preset(ctx context.Context, rn *run, reason, summary string) (extension.RunHandle, error) {
	out := domain.Outcome{Kind: domain.OutcomeFailed, Summary: summary}
	out.Data = resultData(ResultSchema, resultFields{Reason: reason})
	if _, err := e.finalizeWith(ctx, rn, &out); err != nil {
		return nil, err
	}
	e.register(rn)
	return &handle{e: e, r: rn}, nil
}

func (e *Executor) register(rn *run) {
	e.mu.Lock()
	e.runs[rn.id] = rn
	e.mu.Unlock()
}

// environment applies the B05 order: sanitized baseline and pass_env →
// resource-provider layer → the extensions' run layer → binding env →
// selected secrets → execution identity. codex never gets the toolbelt
// credential; only its MCP server does. A run layer gets the toolbelt
// client (OS_ENDPOINT, OS_TOKEN_FILE, `os` on PATH) so it can call `os`.
func (e *Executor) environment(cfg Config, r extension.RunRequest, dir string, layer map[string]string) ([]string, error) {
	vars := map[string]string{}
	for _, kv := range e.o.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if ok && (baseline(name) || contains(cfg.PassEnv, name)) && !reserved(name) {
			vars[name] = value
		}
	}
	if vars["PATH"] == "" {
		vars["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	extension.ApplyEnv(vars, layer)
	extension.ApplyEnv(vars, r.Context.Env)
	toolbelt := r.Context.ToolbeltDir
	if toolbelt == "" {
		toolbelt = e.o.ToolbeltDir
	}
	if len(r.Context.Env) > 0 && r.Context.ToolbeltToken != "" && r.Context.ToolbeltEndpoint != "" {
		vars["OS_ENDPOINT"] = r.Context.ToolbeltEndpoint
		vars["OS_TOKEN_FILE"] = r.Context.ToolbeltTokenFile
		if vars["OS_TOKEN_FILE"] == "" {
			vars["OS_TOKEN_FILE"] = filepath.Join(dir, "token")
		}
		if toolbelt != "" {
			vars["PATH"] = toolbelt + string(os.PathListSeparator) + vars["PATH"]
		}
	}
	for name, value := range cfg.Env {
		vars[name] = value
	}
	for secret, value := range r.Context.Secrets {
		name := cfg.secretVariable(secret)
		if reserved(name) || strings.ContainsRune(value, 0) {
			return nil, configError("secret maps to a reserved variable")
		}
		vars[name] = value
	}
	vars["OS_SCOPE_ID"] = string(r.Work.ScopeID)
	vars["OS_WORK_ID"] = string(r.Work.ID)
	vars["OS_EXECUTION_ID"] = string(r.Execution.ID)
	vars["OS_ATTEMPT"] = strconv.FormatUint(uint64(r.Execution.Attempt), 10)
	out := make([]string, 0, len(vars))
	for name, value := range vars {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// baseline admits only safe process variables: unrelated credentials,
// toolbelt variables and Open Conduit settings never reach codex.
func baseline(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TMPDIR", "TERM", "CODEX_HOME":
		return true
	}
	return strings.HasPrefix(name, "LC_")
}

// thread is a Work's private continuity: the Codex session its turns
// resume. Core never reads it.
type thread struct {
	Thread string    `json:"thread"`
	At     time.Time `json:"at"`
}

func (e *Executor) threadPath(work domain.ID) string {
	return filepath.Join(e.o.StateDir, "work", executionKey(work)+".json")
}

func (e *Executor) loadThread(work domain.ID) (string, error) {
	var t thread
	err := readJSON(e.threadPath(work), &t)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return t.Thread, err
}

func (e *Executor) saveThread(work domain.ID, id string) error {
	return writeJSON(e.threadPath(work), thread{Thread: id, At: e.o.Now().UTC()})
}

// Recover adopts a verified live segment or returns its finished result. It
// never launches a process; unknown, dead or unverifiable metadata returns
// ErrNotAdoptable.
func (e *Executor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := executionKey(r.Execution.ID)
	if state.Schema != RecoverySchema {
		// No acknowledged launch: clean up anything this execution may have
		// left, then let core decide retry after verified stop.
		_ = e.Stop(ctx, r.Execution.ID, r.Execution.Lease.Epoch)
		return nil, extension.ErrNotAdoptable
	}
	var meta recovery
	if json.Unmarshal(state.Data, &meta) != nil {
		return nil, extension.ErrNotAdoptable
	}
	k, nonce, ok := strings.Cut(meta.Run, "/")
	if !ok || k != key || !hexName.MatchString(nonce) {
		return nil, extension.ErrNotAdoptable
	}
	dir := filepath.Join(e.o.StateDir, "runs", key, nonce)
	if _, err := os.Stat(filepath.Join(dir, "lease")); err != nil {
		return nil, extension.ErrNotAdoptable
	}
	epoch := r.Execution.Lease.Epoch
	if current, err := readLease(dir); err != nil || current > epoch {
		return nil, extension.ErrNotAdoptable
	}
	if err := writeLease(dir, epoch); err != nil {
		return nil, err
	}
	e.mu.Lock()
	rn := e.runs[r.Execution.ID]
	fresh := rn == nil || rn.nonce != nonce
	if fresh {
		rn = &run{id: r.Execution.ID, dir: dir, nonce: nonce}
	}
	rn.mu.Lock()
	rn.epoch = epoch
	rn.mu.Unlock()
	e.mu.Unlock()
	// adopt re-acknowledges the identical metadata under the new lease (the
	// scheduler requires it) and never records replacement metadata.
	adopt := func() (extension.RunHandle, error) {
		if r.PersistRecovery == nil {
			return nil, extension.ErrInvalid
		}
		if err := r.PersistRecovery(state); err != nil {
			return nil, err
		}
		e.register(rn)
		return &handle{e: e, r: rn}, nil
	}
	if _, ok, err := readResult(dir); err != nil {
		return nil, err
	} else if ok {
		return adopt()
	}
	if fresh {
		if err := rn.load(); err != nil {
			return nil, extension.ErrNotAdoptable
		}
	}
	alive, err := e.leaderAlive(rn)
	if err != nil {
		return nil, err
	}
	if !alive {
		if !exists(filepath.Join(dir, "exit")) && !exists(filepath.Join(dir, "stop.json")) {
			return nil, extension.ErrNotAdoptable
		}
		return adopt()
	}
	// Replace the process credential before acknowledging adoption. Runtime has
	// already revoked lower-epoch tokens.
	// With a host-kept token file the host has rotated it already; a segment
	// launched with its own file (before the host kept one) keeps that current.
	token := filepath.Join(dir, "token")
	if r.Context.ToolbeltToken != "" {
		if r.Context.ToolbeltTokenFile == "" || exists(token) {
			err = writeFile(token, []byte(r.Context.ToolbeltToken+"\n"))
		}
	} else {
		err = os.Remove(token)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	return adopt()
}

// Interrupt requests termination of the segment this instance holds for the
// execution. It verifies process identity and lease epoch first; success means
// the request was delivered, not that the tree has stopped.
func (e *Executor) Interrupt(ctx context.Context, id domain.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		return ErrUnknownExecution
	}
	if _, ok, _ := readResult(rn.dir); ok {
		return nil
	}
	return e.requestStop(rn, "interrupted")
}

// Stop is trusted host cleanup for a fenced attempt: it terminates any verified
// live segment of the execution and returns nil only once every segment's
// process tree is verified stopped, so the host may call ConfirmStopped with
// the same epoch. A newer adopting epoch makes it fail with ErrSuperseded.
func (e *Executor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	base := filepath.Join(e.o.StateDir, "runs", executionKey(id))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !hexName.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		if current, err := readLease(dir); err != nil {
			return err
		} else if current > epoch {
			return ErrSuperseded
		}
		if _, ok, _ := readResult(dir); ok {
			continue
		}
		rn := &run{id: id, dir: dir, nonce: entry.Name(), epoch: epoch}
		e.mu.Lock()
		if held := e.runs[id]; held != nil && held.dir == dir {
			rn.pid, rn.done, rn.timeout, rn.deadline = held.pid, held.done, held.timeout, held.deadline
		}
		e.mu.Unlock()
		if err := rn.load(); err != nil {
			return err
		}
		if err := e.stopTree(ctx, rn); err != nil {
			return err
		}
	}
	return nil
}

// ErrLiveSegment refuses Forget while a segment's verified process still runs.
var ErrLiveSegment = errors.New("codex: segment still running")

// Forget removes every run directory of an execution once runtime retention
// expired it. The Work's thread is kept: it belongs to the Work, not the
// execution. It never signals: a segment without a recorded result whose
// verified leader is still alive is refused, leaving its state intact.
func (e *Executor) Forget(ctx context.Context, id domain.ID) error {
	base := filepath.Join(e.o.StateDir, "runs", executionKey(id))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !hexName.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		if _, ok, _ := readResult(dir); ok {
			continue
		}
		rn := &run{id: id, dir: dir, nonce: entry.Name()}
		if err := rn.load(); err != nil {
			continue // Unreadable metadata names no process to protect.
		}
		if alive, err := e.leaderAlive(rn); err != nil {
			return err
		} else if alive {
			return ErrLiveSegment
		}
	}
	e.mu.Lock()
	if held := e.runs[id]; held != nil {
		delete(e.runs, id)
	}
	e.mu.Unlock()
	return os.RemoveAll(base)
}

// stopTree terminates and waits for one segment's process tree.
func (e *Executor) stopTree(ctx context.Context, rn *run) error {
	if alive, err := e.leaderAlive(rn); err != nil {
		return err
	} else if alive {
		if err = e.requestStop(rn, "interrupted"); err != nil && !errors.Is(err, ErrSuperseded) {
			return err
		}
		killAt := e.o.Now().Add(e.o.Grace)
		for {
			alive, err = e.leaderAlive(rn)
			if err != nil {
				return err
			}
			if !alive {
				break
			}
			if !e.o.Now().Before(killAt) {
				if err = e.killVerified(rn); err != nil {
					return err
				}
			}
			if err = sleep(ctx, 20*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return e.drainGroup(ctx, rn)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

var _ interface {
	extension.Executor
	extension.InterruptExecutor
	extension.InjectExecutor
	extension.ResumeExecutor
	extension.StopExecutor
	extension.ForgetExecutor
} = (*Executor)(nil)

// unavailable is the workspace_unavailable summary: the provider's reason in
// one line, so the run's history and tracker say why without the run's log.
func unavailable(err error) string {
	if err == nil {
		return "workspace acquisition failed"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return "workspace acquisition failed: " + line
}
