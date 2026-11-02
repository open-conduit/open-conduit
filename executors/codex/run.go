package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// run is one launched segment. done/state exist only in the instance that
// started the process (it reaps it); adopted segments are observed by polling.
type run struct {
	id       domain.ID
	dir      string
	nonce    string
	mu       sync.Mutex
	epoch    uint64
	deadline time.Time
	timeout  int64
	pid      int
	done     chan struct{}
	state    *os.ProcessState
}

type intent struct {
	ExecutionID    string     `json:"execution_id"`
	Nonce          string     `json:"nonce"`
	StartedAt      time.Time  `json:"started_at"`
	TimeoutSeconds int64      `json:"timeout_seconds,omitempty"`
	Deadline       *time.Time `json:"deadline,omitempty"`
}
type processRecord struct {
	PID int `json:"pid"`
	// Signals is what the segment's sandbox lets a stop deliver: term or
	// kill (B09); empty without one.
	Signals string `json:"signals,omitempty"`
}
type stopRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// load restores a segment from its files: the daemon's process record, or the
// PID the wrapper itself wrote, or a scan for the nonce when neither exists.
func (rn *run) load() error {
	var in intent
	if err := readJSON(filepath.Join(rn.dir, "intent.json"), &in); err == nil {
		rn.timeout = in.TimeoutSeconds
		if in.Deadline != nil {
			rn.deadline = *in.Deadline
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if rn.pid != 0 {
		return nil
	}
	var p processRecord
	if err := readJSON(filepath.Join(rn.dir, "process.json"), &p); err == nil && p.PID > 1 {
		rn.pid = p.PID
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(rn.dir, "pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			rn.pid = pid
			return nil
		}
	}
	list, err := scanProcesses()
	if err != nil {
		return err
	}
	for _, p := range list {
		if rn.matches(p) {
			rn.pid = p.PID
		}
	}
	return nil
}

// matches checks identity beyond PID: a session/group leader whose argument
// vector carries this segment's nonce.
func (rn *run) matches(p procInfo) bool {
	return p.PID > 1 && p.PGID == p.PID && strings.Contains(p.Args, marker+rn.nonce)
}

type procInfo struct {
	PID, PGID int
	Args      string
}

func (e *Executor) leaderAlive(rn *run) (bool, error) {
	if rn.done != nil {
		select {
		case <-rn.done:
			return false, nil
		default:
			return true, nil
		}
	}
	if rn.pid <= 1 {
		return false, nil
	}
	p, ok, err := inspectProcess(rn.pid)
	if err != nil {
		return false, err
	}
	return ok && rn.matches(p), nil
}

// owner refuses process control once a newer epoch has adopted the segment.
func (e *Executor) owner(rn *run) error {
	current, err := readLease(rn.dir)
	if err != nil {
		return err
	}
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if current > rn.epoch {
		return ErrSuperseded
	}
	return nil
}

// verified returns the leader PID only after re-checking identity immediately
// before a signal.
func (e *Executor) verified(rn *run) (int, bool, error) {
	if err := e.owner(rn); err != nil {
		return 0, false, err
	}
	if rn.pid <= 1 {
		return 0, false, nil
	}
	p, ok, err := inspectProcess(rn.pid)
	if err != nil || !ok || !rn.matches(p) {
		return 0, false, err
	}
	return p.PID, true, nil
}

// requestStop records the first stop reason and sends SIGTERM to the verified
// process group.
func (e *Executor) requestStop(rn *run, reason string) error {
	pid, ok, err := e.verified(rn)
	if err != nil || !ok {
		return err
	}
	path := filepath.Join(rn.dir, "stop.json")
	if !exists(path) {
		if err = writeJSON(path, stopRecord{Reason: reason, At: e.o.Now().UTC()}); err != nil {
			return err
		}
	}
	return signalGroup(pid, sigTerm)
}

func (e *Executor) killVerified(rn *run) error {
	pid, ok, err := e.verified(rn)
	if err != nil || !ok {
		return err
	}
	return signalGroup(pid, sigKill)
}

// drainGroup waits for remaining group members after the leader has gone,
// escalating to SIGKILL after the grace period. Signals are only sent while the
// group is live, when its ID cannot have been reused.
func (e *Executor) drainGroup(ctx context.Context, rn *run) error {
	if rn.pid <= 1 {
		return nil
	}
	if err := e.owner(rn); err != nil {
		return err
	}
	// A live process holding the leader PID without this segment's identity
	// means the PID was reused, which is only possible after this group emptied.
	reused := func() (bool, error) {
		p, ok, err := inspectProcess(rn.pid)
		return ok && !rn.matches(p), err
	}
	if r, err := reused(); err != nil || r {
		return err
	}
	start := e.o.Now()
	killed := false
	for groupAlive(rn.pid) {
		elapsed := e.o.Now().Sub(start)
		if elapsed >= e.o.Grace && !killed {
			if r, err := reused(); err != nil || r {
				return err
			}
			if err := signalGroup(rn.pid, sigKill); err != nil {
				return err
			}
			killed = true
		}
		if killed && elapsed >= e.o.Grace+5*time.Second {
			return ErrNotStopped
		}
		if err := sleep(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

type handle struct {
	e *Executor
	r *run
}

// Wait observes the segment. Cancelling ctx stops observation only; it never
// terminates the detached process.
func (h *handle) Wait(ctx context.Context) (extension.Result, error) {
	e, rn := h.e, h.r
	ticker := time.NewTicker(e.o.PollInterval)
	defer ticker.Stop()
	for {
		if result, ok, err := readResult(rn.dir); err != nil || ok {
			return result, err
		}
		alive, err := e.leaderAlive(rn)
		if err != nil {
			return extension.Result{}, err
		}
		if !alive {
			break
		}
		if err = e.enforce(rn); err != nil && !errors.Is(err, ErrSuperseded) {
			return extension.Result{}, err
		}
		wake := e.o.PollInterval
		if rn.done == nil || !rn.deadline.IsZero() || exists(filepath.Join(rn.dir, "stop.json")) {
			wake = min(wake, 50*time.Millisecond)
		}
		timer := time.NewTimer(wake)
		select {
		case <-ctx.Done():
			timer.Stop()
			return extension.Result{}, ctx.Err()
		case <-rn.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	if err := e.drainGroup(ctx, rn); err != nil {
		return extension.Result{}, err
	}
	return e.finalize(ctx, rn)
}

// enforce applies the persisted deadline and SIGKILL escalation, so both hold
// across daemon restart.
func (e *Executor) enforce(rn *run) error {
	var stop stopRecord
	err := readJSON(filepath.Join(rn.dir, "stop.json"), &stop)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !rn.deadline.IsZero() && !e.o.Now().Before(rn.deadline) {
			return e.requestStop(rn, "timeout")
		}
		return nil
	case err != nil:
		return err
	case e.o.Now().Sub(stop.At) >= e.o.Grace:
		return e.killVerified(rn)
	}
	return nil
}

type resultFields struct {
	// Status is the turn's reported state (building, review, help, done,
	// finished) for projections; empty without a status.
	Status string `json:"status,omitempty"`
	Reason string `json:"reason"`
	// Report is the status's write-up for the operator.
	Report    string  `json:"report,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	Turns     int     `json:"turns,omitempty"`
	ToolCalls int     `json:"tool_calls,omitempty"`
	Tokens    *Tokens `json:"tokens,omitempty"`
	Limited   bool    `json:"limited,omitempty"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	Signal    string  `json:"signal,omitempty"`
	// Injected counts the events delivered to this segment and those a
	// follow-up turn took in.
	Injected       *injected `json:"injected,omitempty"`
	TimeoutSeconds int64     `json:"timeout_seconds,omitempty"`
	Transcript     string    `json:"transcript,omitempty"`
	Log            string    `json:"log,omitempty"`
	ArtifactError  string    `json:"artifact_error,omitempty"`
	// Workspace describes the resource the segment ran in, if any.
	Workspace *workspaceRecord `json:"workspace,omitempty"`
}

type injected struct {
	Accepted int `json:"accepted"`
	Consumed int `json:"consumed"`
}

// maxReport bounds a status report kept with the turn's outcome.
const maxReport = 20000

func resultData(schema string, f resultFields) domain.Payload {
	b, _ := json.Marshal(f)
	return domain.Payload{Schema: schema, Data: b}
}

// outcome maps the recorded stop reason, exit status, codex's events and
// the turn's status to a structured result: a finished status completes
// the Work, every other status hands it back for the next input, and an
// error, a missing status or an interrupt fails it.
func (e *Executor) outcome(rn *run) (domain.Outcome, resultFields, *extension.RetryHint, error) {
	var f resultFields
	var stop stopRecord
	stopped := readJSON(filepath.Join(rn.dir, "stop.json"), &stop) == nil
	if b, err := os.ReadFile(filepath.Join(rn.dir, "exit")); err == nil {
		if code, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			f.ExitCode = &code
		}
	}
	rn.mu.Lock()
	state := rn.state
	rn.mu.Unlock()
	if f.ExitCode == nil && state != nil {
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			f.Signal = ws.Signal().String()
		} else {
			code := state.ExitCode()
			f.ExitCode = &code
		}
	}
	p := parseEvents(filepath.Join(rn.dir, eventsFile))
	f.Turns, f.ToolCalls, f.Tokens = p.Turns, p.ToolCalls, p.Tokens
	var turn turnRecord
	_ = readJSON(filepath.Join(rn.dir, "turn.json"), &turn)
	f.Kind = turn.Kind
	failed := func(summary string) (domain.Outcome, resultFields, *extension.RetryHint, error) {
		return domain.Outcome{Kind: domain.OutcomeFailed, Summary: truncate(summary, 300)}, f, nil, nil
	}
	switch {
	case stopped && stop.Reason == "timeout":
		f.Reason, f.TimeoutSeconds = "timeout", rn.timeout
		return failed("codex turn timed out")
	case stopped:
		f.Reason = "interrupted"
		return failed("codex turn interrupted")
	case f.Signal != "":
		f.Reason = "signal"
		return failed("codex terminated by signal")
	case f.ExitCode == nil:
		return domain.Outcome{}, f, nil, errNoExit
	case *f.ExitCode == 127 && p.Turns == 0:
		f.Reason = "start_failed"
		return failed("codex could not be started: is it installed?")
	case *f.ExitCode != 0 || p.Failed:
		f.Reason = "error"
		msg := p.Error
		if msg == "" {
			msg = lastLine(filepath.Join(rn.dir, errFile))
		}
		if msg == "" {
			msg = "codex exited with status " + strconv.Itoa(*f.ExitCode)
		}
		if limited(msg) {
			f.Reason, f.Limited = "usage_limit", true
			out, f, _, _ := failed(msg)
			return out, f, &extension.RetryHint{}, nil
		}
		return failed(msg)
	}
	if !turn.Structured {
		f.Reason = "result"
		return domain.Outcome{Kind: domain.OutcomeCompleted, Summary: truncate(strings.TrimSpace(p.Text), 300)}, f, nil, nil
	}
	s := readStatus(filepath.Join(rn.dir, lastFile))
	if s == nil {
		f.Reason = "no_status"
		return failed("codex ended its turn without a status")
	}
	f.Reason, f.Status, f.Report = "status", s.State, truncate(s.Report, maxReport)
	kind := domain.OutcomeHandoff
	if s.State == StateFinished {
		kind = domain.OutcomeCompleted
	}
	return domain.Outcome{Kind: kind, Summary: truncate(s.Summary, 300)}, f, nil, nil
}

// lastLine is the last non-empty line of a file, for an error codex gave
// only on stderr.
func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (e *Executor) finalize(ctx context.Context, rn *run) (extension.Result, error) {
	return e.finalizeWith(ctx, rn, nil)
}

// finalizeWith records the Work's thread, publishes the transcript and log
// and durably records the result exactly once across instances; a later
// Recover returns this identical result.
func (e *Executor) finalizeWith(ctx context.Context, rn *run, preset *domain.Outcome) (extension.Result, error) {
	unlock, err := lockFile(filepath.Join(rn.dir, "finalize.lock"))
	if err != nil {
		return extension.Result{}, err
	}
	defer unlock()
	if result, ok, err := readResult(rn.dir); err != nil || ok {
		return result, err
	}
	var turn turnRecord
	_ = readJSON(filepath.Join(rn.dir, "turn.json"), &turn)
	schema := ResultSchema
	var outcome domain.Outcome
	var f resultFields
	var hint *extension.RetryHint
	var result extension.Result
	if preset != nil {
		outcome = *preset
		_ = json.Unmarshal(preset.Data.Data, &f)
	} else {
		if outcome, f, hint, err = e.outcome(rn); err != nil {
			return extension.Result{}, err
		}
		if f.Status != "" && turn.StatusSchema != "" {
			schema = turn.StatusSchema
		}
		if thread := threadOf(filepath.Join(rn.dir, eventsFile)); thread != "" && turn.Work != "" {
			if err := e.saveThread(turn.Work, thread); err != nil {
				return extension.Result{}, err
			}
		}
		u := extension.Usage{}
		if f.Tokens != nil {
			in, out, cached := uint64(max(f.Tokens.Input, 0)), uint64(max(f.Tokens.Output, 0)), uint64(max(f.Tokens.CacheRead, 0))
			u.InputTokens, u.OutputTokens, u.CachedTokens = &in, &out, &cached
		}
		result.Usage = u
	}
	if e.o.Artifacts != nil {
		for _, a := range []struct {
			file, media string
			field       *string
		}{{eventsFile, TranscriptMediaType, &f.Transcript}, {errFile, LogMediaType, &f.Log}} {
			ref, err := e.publish(ctx, filepath.Join(rn.dir, a.file), a.media)
			if err == nil {
				*a.field = ref.ID
				result.Artifacts = append(result.Artifacts, ref)
			} else if !errors.Is(err, os.ErrNotExist) {
				f.ArtifactError = "artifact store unavailable"
			}
		}
	}
	var ws workspaceRecord
	if readJSON(filepath.Join(rn.dir, "workspace.json"), &ws) == nil && ws.Ref != "" {
		f.Workspace = &ws
		result.Workspace = &extension.WorkspaceRecord{Kind: ws.Kind, Ref: ws.Ref, Dir: ws.Dir, Summary: ws.Summary, Data: domain.Payload{Schema: ws.Schema, Data: ws.Data}}
	}
	result.ConsumedInputs = consumedInputs(rn.dir)
	if entries, _ := inboxEntries(filepath.Join(rn.dir, inboxDir)); len(entries) > 0 {
		f.Injected = &injected{Accepted: len(entries), Consumed: len(result.ConsumedInputs)}
	}
	outcome.Data = resultData(schema, f)
	result.Outcome = outcome
	result.RetryHint = hint
	if err = writeJSON(filepath.Join(rn.dir, "result.json"), result); err != nil {
		return extension.Result{}, err
	}
	_ = os.Remove(filepath.Join(rn.dir, "token"))
	result, _, err = readResult(rn.dir)
	return result, err
}

func (e *Executor) publish(ctx context.Context, path, media string) (domain.ArtifactRef, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return domain.ArtifactRef{}, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	defer f.Close()
	w, err := e.o.Artifacts.Create(ctx, media)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	if _, err = io.Copy(w, f); err != nil {
		w.Close()
		return domain.ArtifactRef{}, err
	}
	return w.Commit(ctx)
}

func readResult(dir string) (extension.Result, bool, error) {
	var r extension.Result
	err := readJSON(filepath.Join(dir, "result.json"), &r)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	return r, err == nil, err
}

func readLease(dir string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(dir, "lease"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}
func writeLease(dir string, epoch uint64) error {
	return writeFile(filepath.Join(dir, "lease"), []byte(strconv.FormatUint(epoch, 10)+"\n"))
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err = json.Compact(&out, b); err != nil {
		return err
	}
	return writeFile(path, out.Bytes())
}

// writeFile atomically replaces an owner-only file.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
