package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
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
	Reason         string            `json:"reason"`
	ExitCode       *int              `json:"exit_code,omitempty"`
	Signal         string            `json:"signal,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty"`
	Log            string            `json:"log,omitempty"`
	Outputs        map[string]string `json:"outputs,omitempty"`
	ArtifactError  string            `json:"artifact_error,omitempty"`
}

func resultData(f resultFields) domain.Payload {
	b, _ := json.Marshal(f)
	return domain.Payload{Schema: ResultSchema, Data: b}
}

// outcome maps the recorded stop reason and exit status to a structured result.
func (e *Executor) outcome(rn *run) (domain.Outcome, resultFields, error) {
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
	switch {
	case stopped && stop.Reason == "timeout":
		f.Reason, f.TimeoutSeconds = "timeout", rn.timeout
		return domain.Outcome{Kind: domain.OutcomeFailed, Summary: "shell command timed out"}, f, nil
	case stopped:
		f.Reason = "interrupted"
		return domain.Outcome{Kind: domain.OutcomeFailed, Summary: "shell command interrupted"}, f, nil
	case f.Signal != "":
		f.Reason = "signal"
		return domain.Outcome{Kind: domain.OutcomeFailed, Summary: "shell command terminated by signal"}, f, nil
	case f.ExitCode == nil:
		return domain.Outcome{}, f, errNoExit
	case *f.ExitCode == 0:
		f.Reason = "exit"
		return domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "exit status 0"}, f, nil
	}
	f.Reason = "exit"
	return domain.Outcome{Kind: domain.OutcomeFailed, Summary: "exit status " + strconv.Itoa(*f.ExitCode)}, f, nil
}

func (e *Executor) finalize(ctx context.Context, rn *run) (extension.Result, error) {
	return e.finalizeWith(ctx, rn, nil)
}

// finalizeWith publishes artifacts and durably records the result exactly once
// across instances; a later Recover returns this identical result.
func (e *Executor) finalizeWith(ctx context.Context, rn *run, preset *domain.Outcome) (extension.Result, error) {
	unlock, err := lockFile(filepath.Join(rn.dir, "finalize.lock"))
	if err != nil {
		return extension.Result{}, err
	}
	defer unlock()
	if result, ok, err := readResult(rn.dir); err != nil || ok {
		return result, err
	}
	var outcome domain.Outcome
	var f resultFields
	if preset != nil {
		outcome = *preset
		_ = json.Unmarshal(preset.Data.Data, &f)
	} else if outcome, f, err = e.outcome(rn); err != nil {
		return extension.Result{}, err
	}
	var result extension.Result
	if e.o.Artifacts != nil {
		if ref, err := e.publish(ctx, filepath.Join(rn.dir, "output.log"), LogMediaType); err == nil {
			f.Log = ref.ID
			result.Artifacts = append(result.Artifacts, ref)
		} else if !errors.Is(err, os.ErrNotExist) {
			f.ArtifactError = "artifact store unavailable"
		}
		names, _ := outputNames(filepath.Join(rn.dir, "outputs"))
		for _, name := range names {
			media := mime.TypeByExtension(filepath.Ext(name))
			if media == "" {
				media = "application/octet-stream"
			}
			ref, err := e.publish(ctx, filepath.Join(rn.dir, "outputs", name), media)
			if err != nil {
				f.ArtifactError = "artifact store unavailable"
				continue
			}
			if f.Outputs == nil {
				f.Outputs = map[string]string{}
			}
			f.Outputs[name] = ref.ID
			result.Artifacts = append(result.Artifacts, ref)
		}
	}
	outcome.Data = resultData(f)
	result.Outcome = outcome
	if err = writeJSON(filepath.Join(rn.dir, "result.json"), result); err != nil {
		return extension.Result{}, err
	}
	_ = os.Remove(filepath.Join(rn.dir, "token"))
	result, _, err = readResult(rn.dir)
	return result, err
}

// outputNames lists regular files only (no symlinks or directories), bounded.
func outputNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && len(names) < maxOutputs {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (e *Executor) publish(ctx context.Context, path, media string) (domain.ArtifactRef, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() {
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

type inputEvent struct {
	ID             string                 `json:"id"`
	Type           string                 `json:"type"`
	Source         string                 `json:"source"`
	DeliveryID     string                 `json:"delivery_id"`
	OccurredAt     time.Time              `json:"occurred_at"`
	CorrelationKey *domain.CorrelationKey `json:"correlation_key,omitempty"`
	CausationID    string                 `json:"causation_id,omitempty"`
	Data           *inputPayload          `json:"data,omitempty"`
}
type inputPayload struct {
	Schema string          `json:"schema,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

func toInput(ev domain.Event) inputEvent {
	out := inputEvent{ID: string(ev.ID), Type: ev.Type, Source: ev.Source, DeliveryID: ev.DeliveryID, OccurredAt: ev.OccurredAt, CorrelationKey: ev.CorrelationKey, CausationID: string(ev.CausationID)}
	if ev.Data.Schema != "" || len(ev.Data.Data) > 0 {
		out.Data = &inputPayload{Schema: ev.Data.Schema, Data: ev.Data.Data}
	}
	return out
}

// writeInput exposes the trigger and acceptance-ordered input batch.
func writeInput(path string, r extension.RunRequest) error {
	doc := struct {
		Schema  string       `json:"schema"`
		Trigger inputEvent   `json:"trigger"`
		Inputs  []inputEvent `json:"inputs"`
	}{Schema: InputSchema, Trigger: toInput(r.Trigger), Inputs: []inputEvent{}}
	for _, ev := range r.Inputs {
		doc.Inputs = append(doc.Inputs, toInput(ev))
	}
	return writeJSON(path, doc)
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
