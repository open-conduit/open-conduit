// Package runner is the example external executor of protocol version 1
// (07-02). Like the first-party shell executor, each attempt is a detached
// job process (its own session, so it survives the extension and the
// daemon) with a run directory under the extension's state directory; the
// recovery metadata names the run, so a restarted extension adopts a live
// job or its finished result through executor.recover, never by
// reattaching to the old extension process.
//
// Capabilities: interrupt and inject; controls: stop (epoch-fenced
// physical-stop evidence) and forget. A job finishes when its run directory
// gets a "finish" file (or at once with config {"auto":true}); it then
// takes in its inbox and proves consumption in the result.
//
// A binding with a resource provider gets the resource acquired before the
// job starts, in the mode config workspace_mode names ("" for the resource
// config's default), and the job runs in its directory.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

const (
	Name           = "runner"
	RecoverySchema = "runner.run.v1"
)

// Manifest is the runner extension's manifest.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: "0.1.0", Summary: "Example external executor: detached jobs with inject, interrupt, stop and forget.", Command: []string{"runner-extension"}, Provides: protocol.Provides{
		Executors: []protocol.ExecutorDecl{{Name: Name, Capabilities: protocol.Capabilities{Interrupt: true, Inject: true, Toolbelt: extension.ToolbeltNone}, Controls: protocol.Controls{Stop: true, Forget: true}, ResourceMode: true}},
		Checks:    []protocol.CheckDecl{{Name: Name}},
	}}
}

// Executor runs jobs. Job is the argv that starts one job; the run
// directory is appended.
type Executor struct {
	StateDir func() string
	Job      []string
}

// Build returns the served extension.
func Build(e *Executor) *sdk.Extension {
	return &sdk.Extension{Manifest: Manifest(), Executors: map[string]extension.Executor{Name: e}, Checks: map[string]func(context.Context, *sdk.Host, bool) (string, error){Name: func(context.Context, *sdk.Host, bool) (string, error) { return "ok", nil }}}
}

// RunDir is the run directory of an execution.
func RunDir(stateDir string, execution domain.ID) string {
	return filepath.Join(stateDir, "runs", strings.NewReplacer("/", "_", "..", "_").Replace(string(execution)))
}

type meta struct {
	Epoch uint64 `json:"epoch"`
	PID   int    `json:"pid"`
}

type jobConfig struct {
	Auto bool `json:"auto"`
	// WorkspaceMode is the resource mode the job asks for, "" for the
	// resource config's default.
	WorkspaceMode string `json:"workspace_mode,omitempty"`
	// Workspace is the acquired resource's directory, set by Run.
	Workspace string `json:"workspace,omitempty"`
	// Retries is an old setting the runner ignores; config check warns
	// about it (ConfigWarnings).
	Retries *int `json:"retries,omitempty"`
}

var (
	_ extension.ResourceModer = (*Executor)(nil)
	_ extension.ConfigWarner  = (*Executor)(nil)
)

// ConfigWarnings are config check's warnings about a binding config: the
// ignored retries setting.
func (e *Executor) ConfigWarnings(raw json.RawMessage) []string {
	var cfg jobConfig
	if json.Unmarshal(raw, &cfg) != nil || cfg.Retries == nil {
		return nil
	}
	return []string{"config.retries is ignored: the runner never retries a job; remove it"}
}

// ResourceMode is the resource mode a binding config requests.
func (e *Executor) ResourceMode(raw json.RawMessage) string {
	var cfg jobConfig
	_ = json.Unmarshal(raw, &cfg)
	return cfg.WorkspaceMode
}

func (e *Executor) dir(id domain.ID) string { return RunDir(e.StateDir(), id) }

func (e *Executor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Interrupt: true, Inject: true, Toolbelt: extension.ToolbeltNone}
}

func writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readMeta(dir string) (meta, error) {
	var m meta
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(raw, &m)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func recovery(id domain.ID) domain.Payload {
	data, _ := json.Marshal(map[string]string{"execution": string(id)})
	return domain.Payload{Schema: RecoverySchema, Data: data}
}

// Run persists the recovery metadata, then launches the detached job.
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil {
		return nil, extension.ErrInvalid
	}
	if err := r.PersistRecovery(recovery(r.Execution.ID)); err != nil {
		return nil, err
	}
	dir := e.dir(r.Execution.ID)
	if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o700); err != nil {
		return nil, err
	}
	var cfg jobConfig
	_ = json.Unmarshal(r.Context.Config.Data, &cfg)
	cfg.Workspace = ""
	if r.Context.Resources != nil {
		res, err := r.Context.Resources.Acquire(ctx, extension.ResourceRequest{Mode: cfg.WorkspaceMode})
		if err != nil {
			return nil, err
		}
		cfg.Workspace = res.Dir
	}
	if err := writeJSON(filepath.Join(dir, "job.json"), cfg); err != nil {
		return nil, err
	}
	argv := append(append([]string(nil), e.Job...), dir)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cfg.Workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }() // reap while this process lives
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta{Epoch: r.Execution.Lease.Epoch, PID: cmd.Process.Pid}); err != nil {
		return nil, err
	}
	return handle{dir}, nil
}

// Recover adopts a live job or its finished result. It never launches.
func (e *Executor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var s struct {
		Execution string `json:"execution"`
	}
	if state.Schema != RecoverySchema || json.Unmarshal(state.Data, &s) != nil || s.Execution != string(r.Execution.ID) {
		return nil, extension.ErrNotAdoptable
	}
	dir := e.dir(r.Execution.ID)
	m, err := readMeta(dir)
	if err != nil {
		return nil, extension.ErrNotAdoptable
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil && !alive(m.PID) {
		return nil, extension.ErrNotAdoptable
	}
	if r.Execution.Lease.Epoch > m.Epoch {
		m.Epoch = r.Execution.Lease.Epoch
		if err := writeJSON(filepath.Join(dir, "meta.json"), m); err != nil {
			return nil, err
		}
	}
	// Re-acknowledge the identical metadata under the adopting lease.
	if r.PersistRecovery != nil {
		if err := r.PersistRecovery(state); err != nil {
			return nil, err
		}
	}
	return handle{dir}, nil
}

type handle struct{ dir string }

func (h handle) Wait(ctx context.Context) (extension.Result, error) {
	vanished := 0
	for {
		raw, err := os.ReadFile(filepath.Join(h.dir, "result.json"))
		if err == nil {
			var r protocol.Result
			if err := json.Unmarshal(raw, &r); err != nil {
				return extension.Result{}, err
			}
			return r.Extension(), nil
		}
		if m, err := readMeta(h.dir); err == nil && !alive(m.PID) {
			if vanished++; vanished > 20 {
				return extension.Result{}, errors.New("runner: job exited without a result")
			}
		}
		select {
		case <-ctx.Done():
			return extension.Result{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (e *Executor) live(id domain.ID) (string, meta, bool) {
	dir := e.dir(id)
	m, err := readMeta(dir)
	if err != nil {
		return dir, m, false
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err == nil {
		return dir, m, false
	}
	return dir, m, alive(m.PID)
}

// Inject drops the event in the live job's inbox (idempotent per Event).
func (e *Executor) Inject(_ context.Context, id domain.ID, ev domain.Event) error {
	dir, _, ok := e.live(id)
	if !ok {
		return extension.ErrNotAccepting
	}
	name := filepath.Join(dir, "inbox", strings.ReplaceAll(string(ev.ID), "/", "_")+".json")
	if _, err := os.Stat(name); err == nil {
		return nil
	}
	return writeJSON(name, protocol.FromEvent(ev))
}

// Interrupt asks the live job to stop; it records a failed outcome.
func (e *Executor) Interrupt(_ context.Context, id domain.ID) error {
	_, m, ok := e.live(id)
	if !ok {
		return extension.ErrNotAccepting
	}
	return syscall.Kill(m.PID, syscall.SIGINT)
}

// Stop kills the job at this epoch and returns once nothing of it runs. A
// newer adopting epoch fences an older caller.
func (e *Executor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	m, err := readMeta(e.dir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Epoch > epoch {
		return fmt.Errorf("runner: stop fenced: epoch %d adopted by %d", epoch, m.Epoch)
	}
	if alive(m.PID) {
		_ = syscall.Kill(-m.PID, syscall.SIGKILL)
	}
	for alive(m.PID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}

// Forget removes the run directory of a terminal execution.
func (e *Executor) Forget(_ context.Context, id domain.ID) error {
	return os.RemoveAll(e.dir(id))
}

// Job is the detached job's main loop (runner-extension job <dir>).
func Job(dir string) error {
	var cfg jobConfig
	if raw, err := os.ReadFile(filepath.Join(dir, "job.json")); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	interrupted := make(chan os.Signal, 1)
	notify(interrupted)
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	for {
		select {
		case <-interrupted:
			return writeJSON(filepath.Join(dir, "result.json"), protocol.FromResult(extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "interrupted"}}))
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := os.Stat(filepath.Join(dir, "finish")); err != nil && !cfg.Auto {
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(dir, "inbox"))
		var consumed []domain.ID
		for _, en := range entries {
			if !strings.HasSuffix(en.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, "inbox", en.Name()))
			var ev protocol.Event
			if err == nil && json.Unmarshal(raw, &ev) == nil {
				consumed = append(consumed, domain.ID(ev.ID))
			}
		}
		sort.Slice(consumed, func(i, j int) bool { return consumed[i] < consumed[j] })
		return writeJSON(filepath.Join(dir, "result.json"), protocol.FromResult(extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "finished"}, ConsumedInputs: consumed}))
	}
}

// Certification is the package's certification.json (07-03): a static
// fixture. The executor case finishes at once ({"auto":true}); the inject
// case runs until the runner creates its finish file.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Fixture: &protocol.Fixture{
		Executor: &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Schema: "runner.config.v1", Data: []byte(`{"auto":true}`)}},
		Inject: &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Schema: "runner.config.v1", Data: []byte(`{}`)},
			ReadyFile: "{state}/runs/certify-inject/ready", FinishFile: "{state}/runs/certify-inject/finish"},
		ExecutorModes: []protocol.ExecutorModeCase{
			{Executor: Name, Config: json.RawMessage(`{"workspace_mode":"fresh"}`), Mode: extension.ResourceFresh},
			{Executor: Name, Config: json.RawMessage(`{"auto":true}`)},
		},
	}}
}
