// Package shell is the restart-safe shell executor. Each segment runs as its own
// process group with file-backed output, so it survives the daemon and can be
// adopted by a later epoch. It presents the CLI toolbelt through OS_ENDPOINT,
// OS_TOKEN (plus the replaceable OS_TOKEN_FILE) and an `os` binary on PATH.
package shell

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

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

const (
	// RecoverySchema identifies the opaque metadata persisted before launch.
	RecoverySchema = "open-conduit.shell.recovery.v1"
	// ResultSchema identifies structured outcome data.
	ResultSchema = "open-conduit.shell.result.v1"
	// InputSchema identifies the OS_INPUT_FILE document.
	InputSchema = "open-conduit.shell.input.v1"
	// marker prefixes the wrapper's $0 so process identity is checked by an
	// unguessable per-segment nonce in the argument vector, not by PID alone.
	marker = "open-conduit-shell:"
	// LogMediaType is the combined stdout/stderr artifact media type.
	LogMediaType = "text/plain; charset=utf-8"
	maxOutputs   = 64
)

// wrapper records its PID, runs the command with the group's termination
// signals trapped (so they reach the command with default disposition), then
// signals any remaining group members and atomically records the exit status.
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
	// ErrUnknownExecution means this executor instance holds no live segment for
	// the execution; it never acts on unowned or unverified processes.
	ErrUnknownExecution = errors.New("no shell process owned for execution")
	// ErrSuperseded means a newer lease epoch adopted the process.
	ErrSuperseded = errors.New("shell process adopted by a newer lease epoch")
	// ErrNotStopped means the process tree could not be verified stopped.
	ErrNotStopped = errors.New("shell process tree not verified stopped")
	errNoExit     = errors.New("shell process ended without a recorded exit status")
	hexName       = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Options configure one executor registration.
type Options struct {
	// StateDir is a private absolute directory for segment state and logs.
	StateDir string
	// Artifacts receives the combined log and OS_OUTPUT_DIR files. Optional.
	Artifacts storage.Artifacts
	// ToolbeltDir contains the `os` binary and is prepended to PATH. Defaults
	// to the running executable's directory when it contains `os`.
	ToolbeltDir string
	// Environ supplies the process baseline before sanitization (os.Environ).
	Environ func() []string
	// Resume advertises the resume capability: a woken parked attempt relaunches
	// the command as a new segment of the same attempt with the wake trigger.
	// Otherwise a toolbelt wait ends the attempt and the wake creates N+1.
	Resume bool
	// Grace is the delay between SIGTERM and SIGKILL (default 10s).
	Grace time.Duration
	// PollInterval paces observation of adopted processes (default 1s).
	PollInterval time.Duration
	// Now is the wall clock for process deadlines (default time.Now).
	Now func() time.Time
}

// Executor implements extension.Executor, InterruptExecutor and ResumeExecutor.
type Executor struct {
	o    Options
	mu   sync.Mutex
	runs map[domain.ID]*run
}

// New validates options and creates the private state directory.
func New(o Options) (*Executor, error) {
	if !processSupported {
		return nil, errors.New("shell executor requires a Unix platform")
	}
	if !filepath.IsAbs(o.StateDir) {
		return nil, errors.New("shell executor state directory must be absolute")
	}
	if err := os.MkdirAll(filepath.Join(o.StateDir, "runs"), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(o.StateDir); err != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("shell executor state directory must be owner-only (0700)")
	}
	if o.ToolbeltDir == "" {
		if self, err := os.Executable(); err == nil {
			dir := filepath.Dir(self)
			if info, err := os.Stat(filepath.Join(dir, "os")); err == nil && info.Mode().IsRegular() {
				o.ToolbeltDir = dir
			}
		}
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

func (e *Executor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Interrupt: true, Resume: e.o.Resume, Toolbelt: extension.ToolbeltCLI, Workspace: true}
}

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

// workspaceRecord is the acquired resource's opaque lease reference and
// description, kept with the segment and reported in its result.
type workspaceRecord struct {
	Ref    string          `json:"ref"`
	Schema string          `json:"schema,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Run starts a new attempt segment.
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	return e.launch(ctx, r)
}

// Resume relaunches a parked attempt as a new segment with the wake trigger.
func (e *Executor) Resume(ctx context.Context, r extension.RunRequest, _ domain.Payload) (extension.RunHandle, error) {
	if !e.o.Resume {
		return nil, extension.ErrUnsupported
	}
	return e.launch(ctx, r)
}

// launch persists recovery intent before starting any process, so a crash can
// never leave an unacknowledged launch that recovery cannot find.
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
	if err = os.MkdirAll(filepath.Join(dir, "outputs"), 0o700); err != nil {
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
		return e.preset(ctx, rn, "invalid_config", "invalid shell binding config")
	}
	log, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare shell process")
	}
	defer log.Close()
	workdir := cfg.Dir
	var layer map[string]string
	if r.Context.Resources != nil {
		// The binding declares a resource provider: run in its workspace with
		// its environment layer. The provider records the lease before creating
		// anything, so a crash here is cleaned up from durable owner state.
		if cfg.Dir != "" {
			return e.preset(ctx, rn, "invalid_config", "shell dir conflicts with the binding's workspace")
		}
		res, err := r.Context.Resources.Acquire(ctx, extension.ResourceRequest{Log: log})
		if res.Ref != "" {
			_ = writeJSON(filepath.Join(dir, "workspace.json"), workspaceRecord{Ref: res.Ref, Schema: res.Data.Schema, Data: res.Data.Data})
		}
		switch {
		case errors.Is(err, extension.ErrResourceSetup):
			return e.preset(ctx, rn, "workspace_setup_failed", "workspace setup failed")
		case err != nil || res.Dir == "":
			return e.preset(ctx, rn, "workspace_unavailable", "workspace acquisition failed")
		}
		workdir, layer = res.Dir, res.Env
	}
	// The process deadline starts after workspace preparation (setup has its
	// own provider timeout).
	rn.timeout = cfg.TimeoutSeconds
	if cfg.TimeoutSeconds > 0 {
		rn.deadline = e.o.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second).UTC()
	}
	in := intent{ExecutionID: string(r.Execution.ID), Nonce: nonce, StartedAt: e.o.Now().UTC(), TimeoutSeconds: cfg.TimeoutSeconds}
	if !rn.deadline.IsZero() {
		in.Deadline = &rn.deadline
	}
	if err = writeJSON(filepath.Join(dir, "intent.json"), in); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare shell process")
	}
	env, err := e.environment(cfg, r, dir, layer)
	if err != nil {
		return e.preset(ctx, rn, "invalid_config", "invalid shell binding config")
	}
	if err = writeInput(filepath.Join(dir, "input.json"), r); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not prepare shell process")
	}
	if r.Context.ToolbeltToken != "" {
		if err = writeFile(filepath.Join(dir, "token"), []byte(r.Context.ToolbeltToken+"\n")); err != nil {
			return e.preset(ctx, rn, "start_failed", "could not prepare shell process")
		}
	}
	if workdir == "" {
		workdir = filepath.Join(dir, "work")
		if err = os.Mkdir(workdir, 0o700); err != nil {
			return e.preset(ctx, rn, "start_failed", "could not prepare shell process")
		}
	}
	argv := cfg.Argv
	if cfg.Command != "" {
		argv = []string{"/bin/sh", "-c", cfg.Command}
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", wrapper, marker + nonce, dir}, argv...)...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = workdir, env, log, log
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return e.preset(ctx, rn, "start_failed", "could not start shell process")
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

// preset finishes a segment that never started a process with a structured
// failed result, so no uncertain resource hold is left behind.
func (e *Executor) preset(ctx context.Context, rn *run, reason, summary string) (extension.RunHandle, error) {
	out := domain.Outcome{Kind: domain.OutcomeFailed, Summary: summary}
	out.Data = resultData(resultFields{Reason: reason})
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

// environment applies the B05 order: sanitized baseline → resource-provider
// layer → binding config.env → selected secrets → trusted toolbelt and
// execution variables, which no earlier layer can override.
func (e *Executor) environment(cfg Config, r extension.RunRequest, dir string, layer map[string]string) ([]string, error) {
	vars := map[string]string{}
	for _, kv := range e.o.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if ok && baseline(name) {
			vars[name] = value
		}
	}
	if vars["PATH"] == "" {
		vars["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	for name, value := range layer {
		// A provider layer (such as a checkout .env) can never supply toolbelt
		// variables, even where the transport is none.
		if envName.MatchString(name) && !reserved(name) && !strings.ContainsRune(value, 0) {
			vars[name] = value
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
	if e.o.ToolbeltDir != "" {
		vars["PATH"] = e.o.ToolbeltDir + string(os.PathListSeparator) + vars["PATH"]
	}
	if r.Context.ToolbeltToken != "" && r.Context.ToolbeltEndpoint != "" {
		vars["OS_ENDPOINT"] = r.Context.ToolbeltEndpoint
		vars["OS_TOKEN"] = r.Context.ToolbeltToken
		vars["OS_TOKEN_FILE"] = filepath.Join(dir, "token")
	}
	vars["OS_SCOPE_ID"] = string(r.Work.ScopeID)
	vars["OS_WORK_ID"] = string(r.Work.ID)
	vars["OS_EXECUTION_ID"] = string(r.Execution.ID)
	vars["OS_ATTEMPT"] = strconv.FormatUint(uint64(r.Execution.Attempt), 10)
	vars["OS_TRIGGER_ID"] = string(r.Trigger.ID)
	vars["OS_TRIGGER_TYPE"] = r.Trigger.Type
	vars["OS_INPUT_FILE"] = filepath.Join(dir, "input.json")
	vars["OS_OUTPUT_DIR"] = filepath.Join(dir, "outputs")
	out := make([]string, 0, len(vars))
	for name, value := range vars {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out, nil
}

// baseline admits only safe process variables: unrelated credentials, toolbelt
// variables and Open Conduit/Open Supervisor settings never reach the command.
func baseline(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TZ", "TMPDIR", "TERM":
		return true
	}
	return strings.HasPrefix(name, "LC_")
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
	token := filepath.Join(dir, "token")
	if r.Context.ToolbeltToken != "" {
		err = writeFile(token, []byte(r.Context.ToolbeltToken+"\n"))
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
var ErrLiveSegment = errors.New("shell: segment still running")

// Forget removes every run directory of an execution once runtime retention
// expired it. It never signals: a segment without a recorded result whose
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
	extension.ResumeExecutor
	extension.ForgetExecutor
} = (*Executor)(nil)
