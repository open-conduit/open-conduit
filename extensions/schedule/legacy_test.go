package schedule_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	sc "github.com/alsiola/open-conduit/extensions/schedule"
	"github.com/alsiola/open-conduit/storage"
)

// innerExec stands in for the shell executor: it records what it was asked
// to run, acquires the binding's resource like the shell does, and finishes
// with result.
type innerExec struct {
	mu     sync.Mutex
	last   extension.RunRequest
	env    map[string]string
	layer  map[string]string
	result extension.Result
	forgot []domain.ID
}

func (x *innerExec) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: extension.ToolbeltCLI, Interrupt: true, Workspace: true}
}
func (x *innerExec) run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	var cfg struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(r.Context.Config.Data, &cfg); err != nil {
		return nil, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.last, x.env = r, cfg.Env
	if r.Context.Resources != nil {
		res, err := r.Context.Resources.Acquire(ctx, extension.ResourceRequest{})
		if err != nil {
			return nil, err
		}
		x.layer = res.Env
	}
	return x, nil
}
func (x *innerExec) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	return x.run(ctx, r)
}
func (x *innerExec) Recover(_ context.Context, r extension.RunRequest, _ domain.Payload) (extension.RunHandle, error) {
	return x, nil
}
func (x *innerExec) Interrupt(context.Context, domain.ID) error { return nil }
func (x *innerExec) Forget(_ context.Context, id domain.ID) error {
	x.forgot = append(x.forgot, id)
	return nil
}
func (x *innerExec) Wait(context.Context) (extension.Result, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.result, nil
}
func (x *innerExec) ValidateConfig(raw json.RawMessage, _ []string) error {
	if !bytes.Contains(raw, []byte(`"command"`)) {
		return errors.New("command required")
	}
	return nil
}

type memArtifacts struct {
	mu    sync.Mutex
	items map[string][]byte
	n     int
}

type memWriter struct {
	a     *memArtifacts
	media string
	buf   bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memWriter) Close() error                { return nil }
func (w *memWriter) Commit(context.Context) (domain.ArtifactRef, error) {
	w.a.mu.Lock()
	defer w.a.mu.Unlock()
	w.a.n++
	id := "a" + string(rune('0'+w.a.n))
	if w.a.items == nil {
		w.a.items = map[string][]byte{}
	}
	w.a.items[id] = append([]byte(nil), w.buf.Bytes()...)
	return domain.ArtifactRef{ID: id, Size: int64(w.buf.Len()), MediaType: w.media}, nil
}
func (a *memArtifacts) Create(_ context.Context, media string) (storage.ArtifactWriter, error) {
	return &memWriter{a: a, media: media}, nil
}
func (a *memArtifacts) Open(_ context.Context, ref domain.ArtifactRef) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return io.NopCloser(bytes.NewReader(a.items[ref.ID])), nil
}
func (a *memArtifacts) Delete(context.Context, domain.ArtifactRef) error { return nil }

type fakeBroker struct{ dir string }

func (b fakeBroker) Acquire(context.Context, extension.ResourceRequest) (extension.Resource, error) {
	data, _ := json.Marshal(map[string]string{"mode": "fresh", "remote": "origin", "base": "main", "dir": b.dir})
	return extension.Resource{Kind: "workspace", Ref: "fresh-1", Dir: b.dir, Env: map[string]string{"FROM_DOTENV": "1"}, Data: domain.Payload{Schema: "open-conduit.git.workspace.v1", Data: data}}, nil
}
func (b fakeBroker) Lookup(ctx context.Context, _ string) (extension.Resource, error) {
	return b.Acquire(ctx, extension.ResourceRequest{})
}

func legacyRequest(t *testing.T, config string, trigger domain.Event) extension.RunRequest {
	t.Helper()
	return extension.RunRequest{
		Work:      domain.Work{ID: "w", ScopeID: "demo", Kind: sc.WorkKind, CorrelationKeys: []domain.CorrelationKey{{Source: "schedule/demo", ExternalID: sc.JobKey("post")}}},
		Execution: domain.Execution{ID: "w/attempt/1", WorkID: "w", Attempt: 1},
		Trigger:   trigger,
		Context:   extension.ExecutionContext{Config: domain.Payload{Schema: "core.config.binding.v1", Data: []byte(config)}},
	}
}

func triggerEvent(typ string, p sc.TriggerPayload) domain.Event {
	data, _ := json.Marshal(p)
	return domain.Event{ID: "t1", Type: typ, Source: "schedule/demo", Data: domain.Payload{Schema: sc.SchemaTrigger, Data: data}}
}

func newLegacy(t *testing.T, inner *innerExec, arts storage.Artifacts, now func() time.Time) *sc.LegacyExecutor {
	t.Helper()
	x, err := sc.NewLegacyExecutor(sc.LegacyOptions{Inner: inner, StateDir: filepath.Join(t.TempDir(), "legacy"), Artifacts: arts, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// TestLegacyJobEnvironment ports the env half of v1 TestJobRunsOnEvent,
// TestJobWebhookURL and TestJobFreshCheckout: the OPEN_SUPERVISOR_* names
// live only in the wrapper, the binding's env wins over them, the workspace
// names join the provider's layer, and the delivery body is a file and a
// payload artifact.
func TestLegacyJobEnvironment(t *testing.T) {
	ctx := context.Background()
	inner := &innerExec{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "exit status 0"}}}
	arts := &memArtifacts{}
	x := newLegacy(t, inner, arts, nil)
	if got := x.Capabilities(); got != inner.Capabilities() {
		t.Fatalf("capabilities %+v", got)
	}
	cfg := `{"shell":{"command":"true","env":{"OPEN_SUPERVISOR_EVENT_REF":"pinned","PLAIN":"x"}}}`
	r := legacyRequest(t, cfg, triggerEvent(sc.TypeWebhook, sc.TriggerPayload{Job: "post", Kind: sc.KindWebhook, Trigger: "webhook", Body: json.RawMessage(`{"hi":1}`)}))
	r.Context.Resources = fakeBroker{dir: t.TempDir()}
	h, err := x.Run(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"OPEN_SUPERVISOR_JOB": "post", "OPEN_SUPERVISOR_JOB_TRIGGER": "webhook", "OPEN_SUPERVISOR_JOB_RUN": "w/attempt/1", "OPEN_SUPERVISOR_PROJECT": "demo",
		"OPEN_SUPERVISOR_EVENT": "webhook", "OPEN_SUPERVISOR_EVENT_REF": "pinned", "PLAIN": "x"}
	for k, v := range want {
		if inner.env[k] != v {
			t.Errorf("%s = %q; want %q (env %v)", k, inner.env[k], v, inner.env)
		}
	}
	file := inner.env["OPEN_SUPERVISOR_EVENT_FILE"]
	if b, err := os.ReadFile(file); err != nil || string(b) != `{"hi":1}` {
		t.Fatalf("event file %q: %q %v", file, b, err)
	}
	if inner.layer["OPEN_SUPERVISOR_BASE_BRANCH"] != "main" || inner.layer["OPEN_SUPERVISOR_REMOTE"] != "origin" || inner.layer["OPEN_SUPERVISOR_WORKTREE"] == "" || inner.layer["FROM_DOTENV"] != "1" {
		t.Fatalf("provider layer %v", inner.layer)
	}
	res, err := h.Wait(ctx)
	if err != nil || res.Outcome.Kind != domain.OutcomeCompleted || len(res.Artifacts) != 1 || res.Artifacts[0].MediaType != "application/json" {
		t.Fatalf("result %+v %v", res, err)
	}
	if string(arts.items[res.Artifacts[0].ID]) != `{"hi":1}` {
		t.Fatalf("payload artifact %q", arts.items[res.Artifacts[0].ID])
	}
	// Observing again (after adoption) answers identically and publishes
	// nothing twice.
	h2, err := x.Recover(ctx, r, domain.Payload{})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := h2.Wait(ctx)
	if len(again.Artifacts) != 1 || again.Artifacts[0] != res.Artifacts[0] || arts.n != 1 {
		t.Fatalf("second observation %+v (published %d)", again, arts.n)
	}
	if err := x.Forget(ctx, r.Execution.ID); err != nil || len(inner.forgot) != 1 {
		t.Fatalf("forget %v %v", err, inner.forgot)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("forget kept the delivery file")
	}

	// A timer slot carries no delivery names; a manual run is "by hand".
	slot := legacyRequest(t, `{"shell":{"command":"true"}}`, triggerEvent(sc.TypeSlot, sc.TriggerPayload{Job: "post", Kind: sc.KindTimer, Trigger: "cron 2026-10-25 09:00 UTC"}))
	slot.Execution.ID = "w/attempt/2"
	if _, err := x.Run(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if inner.env["OPEN_SUPERVISOR_JOB_TRIGGER"] != "cron 2026-10-25 09:00 UTC" || inner.env["OPEN_SUPERVISOR_EVENT"] != "" || inner.env["OPEN_SUPERVISOR_EVENT_FILE"] != "" {
		t.Fatalf("slot env %v", inner.env)
	}
	manual := legacyRequest(t, `{"shell":{"command":"true"}}`, domain.Event{ID: "m", Type: "schedule.run", Source: sc.ManualSource})
	manual.Execution.ID = "w/attempt/3"
	if _, err := x.Run(ctx, manual); err != nil {
		t.Fatal(err)
	}
	if inner.env["OPEN_SUPERVISOR_JOB_TRIGGER"] != "by hand" {
		t.Fatalf("manual env %v", inner.env)
	}
}

// TestLegacyStatusFolder ports v1 TestDetachedJobIsRecorded and the busy
// retry: last.json becomes the run's outcome summary and an artifact; "busy"
// fails the run with a ten-minute retry hint within the slot's six hours.
func TestLegacyStatusFolder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 5, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	inner := &innerExec{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "exit status 0"}}}
	arts := &memArtifacts{}
	x := newLegacy(t, inner, arts, clock)
	work := t.TempDir()
	status := filepath.Join(work, ".local", "refresh")
	if err := os.MkdirAll(status, 0o700); err != nil {
		t.Fatal(err)
	}
	slotAt := time.Date(2026, 10, 21, 5, 17, 0, 0, time.UTC)
	trig := triggerEvent(sc.TypeSlot, sc.TriggerPayload{Job: "refresh", Kind: sc.KindTimer, Trigger: "at 2026-10-21 05:17 UTC", At: &slotAt})
	cfg := `{"status":".local/refresh","shell":{"command":"true"}}`
	run := func(id string, last string) extension.Result {
		t.Helper()
		r := legacyRequest(t, cfg, trig)
		r.Execution.ID = domain.ID(id)
		r.Context.Resources = fakeBroker{dir: work}
		h, err := x.Run(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(status, "last.json"))
		if last != "" {
			if err := os.WriteFile(filepath.Join(status, "last.json"), []byte(last), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		res, err := h.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := run("r/1", `{"slot":"x","outcome":"done","finishedAt":"2026-10-21T05:30:00Z","sha":"abc1234"}`)
	if res.Outcome.Kind != domain.OutcomeCompleted || !strings.Contains(res.Outcome.Summary, "status done abc1234") || res.RetryHint != nil || len(res.Artifacts) != 1 {
		t.Fatalf("done: %+v", res)
	}
	res = run("r/2", `{"slot":"x","outcome":"busy","finishedAt":"2026-10-21T05:30:00Z"}`)
	if res.Outcome.Kind != domain.OutcomeFailed || res.RetryHint == nil || res.RetryHint.ResetAt == nil || !res.RetryHint.ResetAt.Equal(now.Add(8*time.Minute)) {
		t.Fatalf("busy: %+v", res)
	}
	if err := extension.ValidateResult(res); err != nil {
		t.Fatal(err)
	}
	res = run("r/3", "")
	if res.Outcome.Kind != domain.OutcomeCompleted || !strings.Contains(res.Outcome.Summary, "no status result") {
		t.Fatalf("no result: %+v", res)
	}
	// Busy too late in the slot's six hours: no retry, the next slot runs.
	now = slotAt.Add(5*time.Hour + 55*time.Minute)
	res = run("r/4", `{"slot":"x","outcome":"busy","finishedAt":"2026-10-21T11:12:00Z"}`)
	if res.Outcome.Kind != domain.OutcomeFailed || res.RetryHint != nil || !strings.Contains(res.Outcome.Summary, "six hours") {
		t.Fatalf("late busy: %+v", res)
	}
}

func TestLegacyConfig(t *testing.T) {
	x := newLegacy(t, &innerExec{}, nil, nil)
	if err := x.ValidateConfig(json.RawMessage(`{"status":"/tmp/s","shell":{"command":"true"}}`), nil); err != nil {
		t.Fatal(err)
	}
	for i, bad := range []string{
		`{"shell":{"argv":["true"]}}`,                 // the shell's own check
		`{"status":"x","shell":"true"}`,               // shell must be an object
		`{"status":"a/../b","shell":{"command":"x"}}`, // unclean
		`{"shell":{"command":"x"},"env":{}}`,          // unknown field
		`{}`,
	} {
		if x.ValidateConfig(json.RawMessage(bad), nil) == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// The daemon proves physical stops through the public optional interface.
var _ extension.StopExecutor = (*sc.LegacyExecutor)(nil)
