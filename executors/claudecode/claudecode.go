// Package claudecode is the restart-safe Claude Code executor. A segment is
// `claude -p` on a Work's private session, run detached as its own process
// group with file-backed stream-json output, so it outlives the daemon and a
// later epoch adopts it. The prompt goes in on stdin and stdin stays open while
// this instance holds the process, so accepted input can be injected mid-turn.
// The toolbelt is presented as an MCP server (`os mcp`) named in a per-segment
// --mcp-config whose credential file is replaced on adoption.
//
// Session identity, resume-versus-fresh, the handoff note and attachment are
// executor-private continuity in namespaced KV (see
// continuity.go); outcomes carry only structured, session-free metadata.
package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	// RecoverySchema identifies the opaque metadata persisted before launch.
	RecoverySchema = "open-conduit.claudecode.recovery.v1"
	// ResultSchema is the outcome data schema of a turn that could not start
	// on its binding's config (see Config.StatusSchema for the others).
	ResultSchema = "open-conduit.claudecode.result.v1"
	// marker prefixes the wrapper's $0 so process identity is checked by an
	// unguessable per-segment nonce in the argument vector, not by PID alone.
	marker = "open-conduit-claude:"
	// TranscriptMediaType is the turn's stream-json output artifact.
	TranscriptMediaType = "application/x-ndjson"
	// LogMediaType is the stderr artifact.
	LogMediaType = "text/plain; charset=utf-8"
	// MCPServerName is the toolbelt's MCP server name: tools appear to the
	// agent as mcp__open-conduit__<operation>.
	MCPServerName = "open-conduit"
)

// wrapper records its PID, runs claude with the group's termination signals
// trapped (so they reach it with default disposition and stdin passes
// through), then signals any remaining group members (the MCP server among
// them) and atomically records the exit status.
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
	// ErrUnknownExecution means this instance holds no live segment for the
	// execution; it never acts on unowned or unverified processes.
	ErrUnknownExecution = errors.New("no claude process owned for execution")
	// ErrNotAccepting (extension.ErrNotAccepting) means the segment's input
	// is closed: it was adopted (stdin went with the daemon that started it)
	// or its result arrived. Core keeps the input for the next attempt or wake.
	ErrNotAccepting = extension.ErrNotAccepting
	// ErrSuperseded means a newer lease epoch adopted the process.
	ErrSuperseded = errors.New("claude process adopted by a newer lease epoch")
	// ErrNotStopped means the process tree could not be verified stopped.
	ErrNotStopped = errors.New("claude process tree not verified stopped")
	// ErrLiveSegment refuses Forget while a segment's verified process runs.
	ErrLiveSegment = errors.New("claude code: segment still running")
	hexName        = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Options configure one executor registration.
type Options struct {
	// StateDir is a private absolute directory for segment state and logs.
	StateDir string
	// Store is coordination storage; continuity uses only its namespaced KV
	// (and reads Work state in Maintain).
	Store storage.Store
	// Namespace separates this registration's continuity (default
	// "executor.claudecode").
	Namespace string
	// Artifacts receives each turn's transcript and stderr. Optional.
	Artifacts storage.Artifacts
	// ToolbeltDir contains the `os` binary that serves MCP. Defaults to the
	// running executable's directory when it contains `os`.
	ToolbeltDir string
	// Environ supplies the process baseline before sanitization (os.Environ).
	Environ func() []string
	// Render turns input Events into prompt markdown (default RenderEvents).
	Render Renderer
	// PluginDir is the default --plugin-dir (the host-prepared playbook
	// plugin) for bindings that set no plugin_dir and use a playbook.
	PluginDir string
	// Report receives per-turn telemetry outside core.
	Report func(name string, attrs map[string]any)
	// Grace is the delay between SIGTERM and SIGKILL (default 10s).
	Grace time.Duration
	// PollInterval paces output following and observation (default 1s).
	PollInterval time.Duration
	// Now is the wall clock (default time.Now).
	Now func() time.Time
	// DryRun runs every turn as a stub on DryRunModel (default haiku) that
	// is told to do nothing and call no tools (v1 dry run). Continuity and
	// outcomes behave as usual.
	DryRun      bool
	DryRunModel string
}

// Executor implements extension.Executor with interrupt, inject and resume.
type Executor struct {
	o    Options
	mu   sync.Mutex
	runs map[domain.ID]*run
}

// New validates options and creates the private state directory.
func New(o Options) (*Executor, error) {
	if !processSupported {
		return nil, errors.New("claude code executor requires a Unix platform")
	}
	if !filepath.IsAbs(o.StateDir) {
		return nil, errors.New("claude code executor state directory must be absolute")
	}
	if o.Store == nil {
		return nil, errors.New("claude code executor needs coordination storage for continuity")
	}
	for _, sub := range []string{"runs", "work"} {
		if err := os.MkdirAll(filepath.Join(o.StateDir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if info, err := os.Stat(o.StateDir); err != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("claude code executor state directory must be owner-only (0700)")
	}
	if o.Namespace == "" {
		o.Namespace = "executor.claudecode"
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
	if o.Render == nil {
		o.Render = RenderEvents
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

// Capabilities advertises interrupt, inject, resume, a workspace and the MCP
// toolbelt transport.
func (e *Executor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Interrupt: true, Inject: true, Resume: true, Toolbelt: extension.ToolbeltMCP, Workspace: true}
}

func hashKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

type recovery struct {
	Run     string `json:"run"`
	Sandbox string `json:"sandbox,omitempty"` // SPIKE #161: the pinned policy digest
}

// Run starts a new attempt segment.
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	return e.launch(ctx, r, domain.Payload{})
}

// Resume continues a parked attempt as a new segment on the same session,
// with the accepted wake event as its input.
func (e *Executor) Resume(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	return e.launch(ctx, r, state)
}

// Recover adopts a verified live segment or returns its finished result. It
// never launches a process; unknown, dead or unverifiable metadata returns
// ErrNotAdoptable.
func (e *Executor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Schema != RecoverySchema {
		// No acknowledged launch: clean up anything this execution may have
		// left, then let core decide retry after verified stop.
		_ = e.Stop(ctx, r.Execution.ID, r.Execution.Lease.Epoch)
		return nil, extension.ErrNotAdoptable
	}
	dir, nonce, ok := e.segmentDir(r.Execution.ID, state)
	if !ok {
		return nil, extension.ErrNotAdoptable
	}
	if _, err := os.Stat(filepath.Join(dir, "lease")); err != nil {
		return nil, extension.ErrNotAdoptable
	}
	// SPIKE #161: adopt only a segment launched under the pinned policy.
	var meta recovery
	_ = json.Unmarshal(state.Data, &meta)
	if extension.VerifySandboxRecord(dir, meta.Sandbox) != nil {
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
	// Replace the MCP server's credential before acknowledging adoption.
	// Runtime has already revoked lower-epoch tokens.
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

// segmentDir resolves recovery metadata to this execution's segment.
func (e *Executor) segmentDir(id domain.ID, state domain.Payload) (string, string, bool) {
	var meta recovery
	if state.Schema != RecoverySchema || json.Unmarshal(state.Data, &meta) != nil {
		return "", "", false
	}
	key := hashKey(string(id))
	k, nonce, ok := strings.Cut(meta.Run, "/")
	if !ok || k != key || !hexName.MatchString(nonce) {
		return "", "", false
	}
	return filepath.Join(e.o.StateDir, "runs", key, nonce), nonce, true
}

// Interrupt requests termination of the segment this instance holds. Success
// means the request was delivered, not that the tree has stopped.
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

// Inject hands an accepted Event to the running turn as a user message.
// claude takes it in after the tool call in progress, before its next model
// call, and echoes it back; the echo is recorded as consumption in the
// result's injected counts and reported in Result.ConsumedInputs, so core
// consumes it with the turn instead of presenting it to the next attempt.
// Success itself means accepted for delivery only: an input that was never
// echoed stays eligible for the next attempt.
func (e *Executor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	rn := e.runs[id]
	e.mu.Unlock()
	if rn == nil {
		// Nothing this instance holds can take the input: definitively not
		// delivered.
		return fmt.Errorf("%w: %w", ErrNotAccepting, ErrUnknownExecution)
	}
	return rn.send(ev.ID, interjection(e.o.Render, ev))
}

// Stop is trusted host cleanup for a fenced attempt: it terminates any
// verified live segment of the execution and returns nil only once every
// segment's process tree is verified stopped. A newer adopting epoch makes it
// fail with ErrSuperseded.
func (e *Executor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	base := filepath.Join(e.o.StateDir, "runs", hashKey(string(id)))
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
			rn.pid, rn.done = held.pid, held.done
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

// Forget removes every segment directory of an execution once runtime
// retention expired it. It never signals: a segment without a recorded result
// whose verified leader is alive is refused. Work continuity is released by
// Maintain when the Work is terminal.
func (e *Executor) Forget(ctx context.Context, id domain.ID) error {
	base := filepath.Join(e.o.StateDir, "runs", hashKey(string(id)))
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
			continue
		}
		if alive, err := e.leaderAlive(rn); err != nil {
			return err
		} else if alive {
			return ErrLiveSegment
		}
	}
	e.mu.Lock()
	delete(e.runs, id)
	e.mu.Unlock()
	return os.RemoveAll(base)
}

func (e *Executor) register(rn *run) {
	e.mu.Lock()
	e.runs[rn.id] = rn
	e.mu.Unlock()
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
	extension.ForgetExecutor
	extension.LiveExecutor
} = (*Executor)(nil)
