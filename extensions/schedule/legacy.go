package schedule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// Open Supervisor job compatibility (05-02). v1 jobs read their run through
// OPEN_SUPERVISOR_* variables, a delivery's body through a file, and v1's
// detached jobs reported through a status folder (running.json while they
// ran, last.json when they ended, "busy" asking to be tried again in ten
// minutes). Open Conduit's own contract is the shell executor's OS_*
// variables and OS_INPUT_FILE, and neither the shell executor nor core knows
// these legacy names. They live only here: the `legacy-job` executor wraps
// the shell executor, adds the legacy layer and translates the status
// folder, so a converted v1 job (06-02) keeps working unchanged.
//
// Environment precedence, lowest to highest: the shell's sanitized baseline,
// the resource provider's layer with the legacy workspace names added
// (OPEN_SUPERVISOR_BASE_BRANCH, _WORKTREE, _REMOTE), the legacy job and
// delivery names (only where the binding's env does not set the same name),
// the binding's env, its selected secrets, then the trusted OS_* variables.

// LegacyExecutorName is the executor registration of the compatibility
// wrapper.
const LegacyExecutorName = "legacy-job"

// legacyBusyRetry is v1's "busy" retry delay. Core adds two minutes of margin
// to an executor's retry hint, so the hint names a reset eight minutes out.
const (
	legacyBusyRetry  = 10 * time.Minute
	legacyHintMargin = 2 * time.Minute
)

// LegacyConfig is the strictly checked binding config of `legacy-job`.
type LegacyConfig struct {
	// Status is v1's status folder (absolute, or relative to the run's
	// working directory: its workspace, else the shell's dir). When set, the
	// run's outcome reads last.json, and "busy" asks to be tried again.
	Status string `json:"status,omitempty"`
	// Shell is the wrapped shell executor's own binding config.
	Shell json.RawMessage `json:"shell"`
}

// ParseLegacyConfig decodes the wrapper's config (the shell part is the
// wrapped executor's to check).
func ParseLegacyConfig(data []byte) (LegacyConfig, error) {
	var c LegacyConfig
	if err := strict(data, &c); err != nil {
		return c, errors.New("legacy-job: malformed config or unknown field")
	}
	if len(bytes.TrimSpace(c.Shell)) == 0 || bytes.TrimSpace(c.Shell)[0] != '{' {
		return c, errors.New("legacy-job: shell must be the shell binding config object")
	}
	if strings.ContainsRune(c.Status, 0) || c.Status != "" && filepath.Clean(c.Status) != c.Status {
		return c, errors.New("legacy-job: status must be a clean path")
	}
	return c, nil
}

// LegacyOptions configure the wrapper.
type LegacyOptions struct {
	// Inner is the shell executor registration it wraps.
	Inner extension.Executor
	// StateDir is a private absolute directory for delivery files.
	StateDir string
	// Artifacts receives the delivery payload and status file. Optional.
	Artifacts storage.Artifacts
	Now       func() time.Time
}

// LegacyExecutor is the `legacy-job` executor.
type LegacyExecutor struct{ o LegacyOptions }

// NewLegacyExecutor validates options.
func NewLegacyExecutor(o LegacyOptions) (*LegacyExecutor, error) {
	if o.Inner == nil {
		return nil, errors.New("legacy-job: an inner executor is required")
	}
	if o.StateDir != "" {
		if !filepath.IsAbs(o.StateDir) {
			return nil, errors.New("legacy-job: state directory must be absolute")
		}
		if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
			return nil, err
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &LegacyExecutor{o: o}, nil
}

// Capabilities are the wrapped executor's.
func (x *LegacyExecutor) Capabilities() domain.Capabilities { return x.o.Inner.Capabilities() }

// ValidateConfig checks the wrapper's fields and lets the wrapped executor
// check its own.
func (x *LegacyExecutor) ValidateConfig(raw json.RawMessage, secretNames []string) error {
	c, err := ParseLegacyConfig(raw)
	if err != nil {
		return err
	}
	if v, ok := x.o.Inner.(interface {
		ValidateConfig(json.RawMessage, []string) error
	}); ok {
		return v.ValidateConfig(c.Shell, secretNames)
	}
	return nil
}

func (x *LegacyExecutor) runDir(id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(x.o.StateDir, hex.EncodeToString(sum[:16]))
}

// legacyRun is what one attempt's wrapper keeps beside the shell's state.
type legacyRun struct {
	StartedAt time.Time `json:"started_at"`
	EventFile string    `json:"event_file,omitempty"`
	Workdir   string    `json:"workdir,omitempty"`
}

// jobName is the job a request runs: its Work's key, else the trigger's.
func jobName(r extension.RunRequest) string {
	for _, k := range r.Work.CorrelationKeys {
		if name, ok := ParseJobKey(k.ExternalID); ok {
			return name
		}
	}
	if p, ok := Payload(r.Trigger); ok {
		return p.Job
	}
	return ""
}

// legacyLabel is v1's OPEN_SUPERVISOR_JOB_TRIGGER for the run's trigger.
func legacyLabel(e domain.Event) string {
	if p, ok := Payload(e); ok && p.Trigger != "" {
		return p.Trigger
	}
	switch {
	case e.Source == ManualSource:
		return "by hand"
	case strings.HasPrefix(e.Type, "core.retry."):
		return "retry"
	}
	return e.Type
}

// prepare builds the request the wrapped executor runs: the legacy layer
// merged into its config and a broker that adds the workspace names.
func (x *LegacyExecutor) prepare(r extension.RunRequest, launch bool) (extension.RunRequest, LegacyConfig, error) {
	cfg, err := ParseLegacyConfig(r.Context.Config.Data)
	if err != nil {
		// The shell then fails the attempt with a structured invalid_config
		// result and no process.
		r.Context.Config = domain.Payload{Schema: r.Context.Config.Schema, Data: []byte(`{}`)}
		return r, cfg, err
	}
	dir := x.runDir(r.Execution.ID)
	state := legacyRun{StartedAt: x.o.Now().UTC()}
	_ = readJSONFile(filepath.Join(dir, "run.json"), &state)
	env := map[string]string{
		"OPEN_SUPERVISOR_JOB":         jobName(r),
		"OPEN_SUPERVISOR_JOB_TRIGGER": legacyLabel(r.Trigger),
		"OPEN_SUPERVISOR_JOB_RUN":     string(r.Execution.ID),
		"OPEN_SUPERVISOR_PROJECT":     string(r.Work.ScopeID),
	}
	if p, ok := Payload(r.Trigger); ok && (r.Trigger.Type == TypeWebhook || r.Trigger.Type == TypeEvent) {
		env["OPEN_SUPERVISOR_EVENT"] = p.Event
		if r.Trigger.Type == TypeWebhook {
			env["OPEN_SUPERVISOR_EVENT"] = "webhook"
		}
		env["OPEN_SUPERVISOR_EVENT_ACTION"] = p.Action
		env["OPEN_SUPERVISOR_EVENT_REF"] = p.Ref
		env["OPEN_SUPERVISOR_EVENT_SHA"] = p.SHA
		if launch && x.o.StateDir != "" && state.EventFile == "" {
			if err := os.MkdirAll(dir, 0o700); err == nil {
				file := filepath.Join(dir, "event.json")
				if writeFileAtomic(file, deliveryBody(r.Trigger.Type, p, r.Trigger.Data.Data)) == nil {
					state.EventFile = file
				}
			}
		}
		if state.EventFile != "" {
			env["OPEN_SUPERVISOR_EVENT_FILE"] = state.EventFile
		}
	}
	var shell map[string]json.RawMessage
	if err := json.Unmarshal(cfg.Shell, &shell); err != nil {
		r.Context.Config = domain.Payload{Schema: r.Context.Config.Schema, Data: []byte(`{}`)}
		return r, cfg, err
	}
	own := map[string]string{}
	if raw, ok := shell["env"]; ok {
		_ = json.Unmarshal(raw, &own)
	}
	for k, v := range env {
		if _, set := own[k]; !set {
			own[k] = v
		}
	}
	shell["env"], _ = json.Marshal(own)
	if state.Workdir == "" {
		var d string
		if raw, ok := shell["dir"]; ok {
			_ = json.Unmarshal(raw, &d)
		}
		state.Workdir = d
	}
	data, _ := json.Marshal(shell)
	r.Context.Config = domain.Payload{Schema: r.Context.Config.Schema, Data: data}
	if launch && x.o.StateDir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			_ = writeJSONFile(filepath.Join(dir, "run.json"), state)
		}
	}
	if r.Context.Resources != nil {
		r.Context.Resources = legacyBroker{inner: r.Context.Resources, save: func(workdir string) {
			if x.o.StateDir == "" {
				return
			}
			state.Workdir = workdir
			if err := os.MkdirAll(dir, 0o700); err == nil {
				_ = writeJSONFile(filepath.Join(dir, "run.json"), state)
			}
		}}
	}
	return r, cfg, nil
}

// deliveryBody is the file v1 named in OPEN_SUPERVISOR_EVENT_FILE: a
// webhook's body as posted, or for a code-host event the trigger's delivery
// fields (Open Conduit does not keep the code host's raw body).
func deliveryBody(typ string, p TriggerPayload, payload []byte) []byte {
	if typ == TypeWebhook {
		if len(p.Body) > 0 {
			return p.Body
		}
		return []byte(p.Text)
	}
	return payload
}

// legacyBroker adds v1's workspace names to the provider's layer.
type legacyBroker struct {
	inner extension.ResourceBroker
	save  func(string)
}

func (b legacyBroker) Acquire(ctx context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	res, err := b.inner.Acquire(ctx, r)
	return b.layer(res), err
}
func (b legacyBroker) Lookup(ctx context.Context, ref string) (extension.Resource, error) {
	res, err := b.inner.Lookup(ctx, ref)
	return b.layer(res), err
}
func (b legacyBroker) layer(res extension.Resource) extension.Resource {
	if res.Ref == "" {
		return res
	}
	var d struct {
		Remote string `json:"remote"`
		Base   string `json:"base"`
	}
	_ = json.Unmarshal(res.Data.Data, &d)
	env := make(map[string]string, len(res.Env)+3)
	for k, v := range res.Env {
		env[k] = v
	}
	set := func(k, v string) {
		if _, ok := env[k]; !ok && v != "" {
			env[k] = v
		}
	}
	set("OPEN_SUPERVISOR_BASE_BRANCH", d.Base)
	set("OPEN_SUPERVISOR_WORKTREE", res.Dir)
	set("OPEN_SUPERVISOR_REMOTE", d.Remote)
	res.Env = env
	if b.save != nil && res.Dir != "" {
		b.save(res.Dir)
	}
	return res
}

// Run launches the wrapped executor with the legacy layer.
func (x *LegacyExecutor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	req, cfg, _ := x.prepare(r, true)
	h, err := x.o.Inner.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	return &legacyHandle{x: x, inner: h, r: r, cfg: cfg}, nil
}

// Recover adopts through the wrapped executor; it never launches.
func (x *LegacyExecutor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	req, cfg, _ := x.prepare(r, false)
	h, err := x.o.Inner.Recover(ctx, req, state)
	if err != nil {
		return nil, err
	}
	return &legacyHandle{x: x, inner: h, r: r, cfg: cfg}, nil
}

// Resume relaunches through the wrapped executor with the legacy layer.
func (x *LegacyExecutor) Resume(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	inner, ok := x.o.Inner.(extension.ResumeExecutor)
	if !ok {
		return nil, extension.ErrUnsupported
	}
	req, cfg, _ := x.prepare(r, true)
	h, err := inner.Resume(ctx, req, state)
	if err != nil {
		return nil, err
	}
	return &legacyHandle{x: x, inner: h, r: r, cfg: cfg}, nil
}

func (x *LegacyExecutor) Interrupt(ctx context.Context, id domain.ID) error {
	if inner, ok := x.o.Inner.(extension.InterruptExecutor); ok {
		return inner.Interrupt(ctx, id)
	}
	return extension.ErrUnsupported
}

func (x *LegacyExecutor) Inject(ctx context.Context, id domain.ID, e domain.Event) error {
	if inner, ok := x.o.Inner.(extension.InjectExecutor); ok {
		return inner.Inject(ctx, id, e)
	}
	return extension.ErrNotAccepting
}

// Stop and Forget pass through, so the host's stop confirmation and the
// shared retention reach the wrapped executor's state.
func (x *LegacyExecutor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	if inner, ok := x.o.Inner.(interface {
		Stop(context.Context, domain.ID, uint64) error
	}); ok {
		return inner.Stop(ctx, id, epoch)
	}
	return nil
}

func (x *LegacyExecutor) Forget(ctx context.Context, id domain.ID) error {
	if inner, ok := x.o.Inner.(extension.ForgetExecutor); ok {
		if err := inner.Forget(ctx, id); err != nil {
			return err
		}
	}
	if x.o.StateDir == "" {
		return nil
	}
	return os.RemoveAll(x.runDir(id))
}

type legacyHandle struct {
	x     *LegacyExecutor
	inner extension.RunHandle
	r     extension.RunRequest
	cfg   LegacyConfig
}

// legacyResult is the translated result, kept so a later observation (after
// adoption) answers identically and publishes nothing twice.
type legacyResult struct {
	Inner  json.RawMessage  `json:"inner"`
	Result extension.Result `json:"result"`
}

func (h *legacyHandle) Wait(ctx context.Context) (extension.Result, error) {
	res, err := h.inner.Wait(ctx)
	if err != nil {
		return res, err
	}
	x := h.x
	if x.o.StateDir == "" {
		return x.translate(ctx, h.r, h.cfg, legacyRun{}, res), nil
	}
	dir := x.runDir(h.r.Execution.ID)
	innerJSON, _ := json.Marshal(res)
	var cached legacyResult
	if readJSONFile(filepath.Join(dir, "result.json"), &cached) == nil && bytes.Equal(cached.Inner, innerJSON) {
		return cached.Result, nil
	}
	var state legacyRun
	_ = readJSONFile(filepath.Join(dir, "run.json"), &state)
	out := x.translate(ctx, h.r, h.cfg, state, res)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = writeJSONFile(filepath.Join(dir, "result.json"), legacyResult{Inner: innerJSON, Result: out})
	}
	return out, nil
}

// LegacyStatus is v1's last.json (engine/jobs.go JobResult).
type LegacyStatus struct {
	Slot       string    `json:"slot"`
	Outcome    string    `json:"outcome"`
	FinishedAt time.Time `json:"finishedAt"`
	StoppedAt  string    `json:"stoppedAt,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Issues     []any     `json:"issues,omitempty"`
}

// translate adds the delivery payload artifact and, for a status job, reads
// last.json into the outcome.
func (x *LegacyExecutor) translate(ctx context.Context, r extension.RunRequest, cfg LegacyConfig, state legacyRun, res extension.Result) extension.Result {
	if state.EventFile != "" {
		if ref, err := x.publish(ctx, state.EventFile, "application/json"); err == nil {
			res.Artifacts = append(res.Artifacts, ref)
		}
	}
	if cfg.Status == "" || res.Outcome.Kind == domain.OutcomeWaiting {
		return res
	}
	statusDir := cfg.Status
	if !filepath.IsAbs(statusDir) {
		if state.Workdir == "" {
			res.Outcome.Summary = strings.TrimSpace(res.Outcome.Summary + "; status folder is relative but the run had no working directory")
			return res
		}
		statusDir = filepath.Join(state.Workdir, statusDir)
	}
	last := filepath.Join(statusDir, "last.json")
	info, err := os.Stat(last)
	var st LegacyStatus
	if err != nil || info.ModTime().Before(state.StartedAt.Add(-time.Second)) || readJSONFile(last, &st) != nil || st.Outcome == "" {
		// v1: a detached run that ended without a result.
		res.Outcome.Summary = strings.TrimSpace(res.Outcome.Summary + "; no status result")
		return res
	}
	if ref, err := x.publish(ctx, last, "application/json"); err == nil {
		res.Artifacts = append(res.Artifacts, ref)
	}
	text := "status " + st.Outcome
	if st.SHA != "" {
		text += " " + st.SHA
	}
	if st.StoppedAt != "" {
		text += " at " + st.StoppedAt
	}
	if len(st.Issues) > 0 {
		text += fmt.Sprintf(", issues %v", st.Issues)
	}
	res.Outcome.Summary = strings.TrimSpace(res.Outcome.Summary + "; " + text)
	if st.Outcome != "busy" {
		return res
	}
	// Another run had what this one needed: v1 tried again in ten minutes,
	// within the slot's six hours.
	res.Outcome.Kind = domain.OutcomeFailed
	now := x.o.Now()
	if p, ok := Payload(r.Trigger); ok && r.Trigger.Type == TypeSlot && p.At != nil && now.Add(legacyBusyRetry).Sub(*p.At) >= CatchUp {
		res.Outcome.Summary += "; the slot is past its six hours, so the job waits for its next"
		return res
	}
	reset := now.Add(legacyBusyRetry - legacyHintMargin)
	res.RetryHint = &extension.RetryHint{ResetAt: &reset}
	return res
}

func (x *LegacyExecutor) publish(ctx context.Context, path, media string) (domain.ArtifactRef, error) {
	if x.o.Artifacts == nil {
		return domain.ArtifactRef{}, errors.New("no artifact store")
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	defer f.Close()
	w, err := x.o.Artifacts.Create(ctx, media)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	if _, err = io.Copy(w, io.LimitReader(f, MaxWebhookBody+1)); err != nil {
		w.Close()
		return domain.ArtifactRef{}, err
	}
	return w.Commit(ctx)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, data []byte) error {
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

var _ interface {
	extension.Executor
	extension.InterruptExecutor
	extension.InjectExecutor
	extension.ResumeExecutor
	extension.ForgetExecutor
} = (*LegacyExecutor)(nil)
