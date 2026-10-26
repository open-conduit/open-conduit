package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/tui"
	"github.com/open-conduit/open-conduit/storage"
)

// historyExecutor is a fake executor whose result is chosen per request. It
// publishes a log artifact like any real executor and records Forget calls.
type historyExecutor struct {
	caps      domain.Capabilities
	artifacts storage.Artifacts
	result    func(extension.RunRequest) extension.Result
	block     chan struct{}
	mu        sync.Mutex
	forgotten []domain.ID
}

type historyHandle struct {
	result extension.Result
	block  chan struct{}
}

func (h historyHandle) Wait(ctx context.Context) (extension.Result, error) {
	if h.block != nil {
		select {
		case <-h.block:
		case <-ctx.Done():
			return extension.Result{}, ctx.Err()
		}
	}
	return h.result, nil
}

func (e *historyExecutor) Capabilities() domain.Capabilities { return e.caps }
func (e *historyExecutor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.history.v1", Data: json.RawMessage(`{}`)}); err != nil {
		return nil, err
	}
	res := e.result(r)
	w, err := e.artifacts.Create(ctx, "text/plain; charset=utf-8")
	if err != nil {
		return nil, err
	}
	if _, err = w.Write([]byte("log of " + string(r.Execution.ID) + " triggered by " + r.Trigger.Type + "\n")); err != nil {
		return nil, err
	}
	ref, err := w.Commit(ctx)
	if err != nil {
		return nil, err
	}
	res.Artifacts = append([]domain.ArtifactRef{ref}, res.Artifacts...)
	return historyHandle{result: res, block: e.block}, nil
}
func (e *historyExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}
func (e *historyExecutor) Forget(_ context.Context, id domain.ID) error {
	e.mu.Lock()
	e.forgotten = append(e.forgotten, id)
	e.mu.Unlock()
	return nil
}

func typeRoute(typ, kind string) routingHandlerFunc {
	return func(in extension.RouteInput) (extension.RouteDecision, error) {
		ev := in.Event.Event
		if ev.Type != typ || ev.CorrelationKey == nil {
			return extension.RouteDecision{}, nil
		}
		return extension.RouteDecision{Match: true, CreateID: domain.ID("work/" + ev.CorrelationKey.ExternalID), Kind: kind}, nil
	}
}

func mixedConfig() string {
	return `{
  "version": 1, "settings": {}, "connections": [],
  "scopes": [{
    "id": "ops", "name": "Operations",
    "authorization": {"connections": [], "operations": ["emit", "read_context", "declare_outcome", "create_work", "wait"], "read_scopes": []},
    "bindings": [
      {"name": "run", "executor": "shell", "requires": {"toolbelt": "cli"}, "operations": ["read_context"], "secret_names": ["api"], "config": {"command": "echo shell-run-output; exit 0"}},
      {"name": "agent", "executor": "agent", "requires": {"toolbelt": "mcp"}, "operations": ["read_context"], "secret_names": [], "config": {}},
      {"name": "job", "executor": "job", "requires": {}, "operations": [], "secret_names": [], "config": {}}
    ],
    "routes": [
      {"name": "conversations", "handler": "agent", "binding": "agent", "active_policy": "queue", "config": {}},
      {"name": "jobs", "handler": "job", "binding": "job", "active_policy": "queue", "config": {}},
      {"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}
    ],
    "secret_refs": {"api": "cmd:printf shell-secret"}
  }]
}`
}

func mixedHost(t *testing.T, kept int) (*shellHost, *historyExecutor, *historyExecutor, *conformance.Clock) {
	t.Helper()
	state, bin := shellState(t, mixedConfig())
	tokens, cost := uint64(1200), 0.042
	out := uint64(300)
	agent := &historyExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltMCP}, result: func(extension.RunRequest) extension.Result {
		return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "answered"}, Usage: extension.Usage{InputTokens: &tokens, OutputTokens: &out, Cost: &cost, Currency: "USD"}}
	}}
	job := &historyExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltNone}, result: func(r extension.RunRequest) extension.Result {
		if r.Execution.Attempt == 1 {
			return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "job exit 2"}}
		}
		return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "job ok"}}
	}}
	clock := conformance.NewClock()
	h := newShellHostWith(t, state, bin, clock, "daemon", false, shellHostOptions{telemetry: true, kept: kept, ingress: true,
		executors: map[string]extension.Executor{"agent": agent, "job": job},
		handlers:  map[string]extension.RouteHandler{"agent": typeRoute("operator.agent", "fixture.conversation"), "job": typeRoute("operator.job", "fixture.job")}})
	agent.artifacts, job.artifacts = h.artifacts, h.artifacts
	return h, agent, job, clock
}

func (h *shellHost) runOnce(work string) domain.ID {
	h.t.Helper()
	a, err := h.scheduler.Start(context.Background(), domain.ID(work))
	if err != nil {
		h.t.Fatal(err)
	}
	h.observe(a)
	return a.Lease.ExecutionID
}

func runKeys(t *testing.T, c *client.Client, ctx context.Context, q url.Values) []map[string]json.RawMessage {
	t.Helper()
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := c.Do(ctx, "GET", "runs", q, nil, "", &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// TestUnifiedHistoryMixedExecutors is the 02-08 history/TUI contract: a real
// shell attempt and fake agent/job attempts are queried and rendered through
// one record shape and one view, with trigger, state, outcome, attempts,
// usage, artifact access and a single retention policy.
func TestUnifiedHistoryMixedExecutors(t *testing.T) {
	ctx := context.Background()
	h, agent, job, clock := mixedHost(t, 1)
	c := h.operator()
	agentWork := h.submit("operator.agent", "agent-1", "agent-1").RoutedWorkID
	jobWork := h.submit("operator.job", "job-1", "job-1").RoutedWorkID
	shellWork := h.submit("operator.requested", "shell-1", "shell-1").RoutedWorkID
	agentRun := h.runOnce(agentWork)
	jobRun1 := h.runOnce(jobWork)
	shellRun := h.runOnce(shellWork)

	// Retry status is visible on the failed attempt before the retry runs.
	failed, err := c.Run(ctx, string(jobRun1))
	if err != nil || failed.Retry == nil || failed.Retry.FailureCount != 1 || !failed.Retry.Blocked || failed.Retry.EligibleAt == "" {
		t.Fatalf("failed job run retry status: %+v %v", failed.Retry, err)
	}
	clock.Advance(6 * time.Minute)
	if _, err = h.scheduler.Poll(ctx, h.timers, 20); err != nil {
		t.Fatal(err)
	}
	jobRun2 := h.runOnce(jobWork)

	page, err := c.Runs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 {
		t.Fatalf("history has %d runs", len(page.Items))
	}
	byID := map[string]client.Run{}
	for _, r := range page.Items {
		byID[r.ID] = r
		if r.Trigger == nil || r.Trigger.Type == "" || r.State == "" || r.Outcome == nil || r.Attempt == 0 || r.Executor == "" || r.ConfigGeneration != "1" || len(r.Artifacts) == 0 || r.Retention.State != "retained" || r.StartedAt == "" || r.FinishedAt == "" || !r.ResourceExcluded {
			t.Fatalf("incomplete run record: %+v", r)
		}
	}
	a := byID[string(agentRun)]
	if a.Executor != "agent" || a.WorkKind != "fixture.conversation" || a.Usage == nil || *a.Usage.InputTokens != 1200 || *a.Usage.Cost != 0.042 || a.Usage.Currency != "USD" || a.Outcome.Summary != "answered" {
		t.Fatalf("agent run: %+v usage %+v", a, a.Usage)
	}
	s := byID[string(shellRun)]
	if s.Executor != "shell" || s.Usage != nil || s.Outcome.Summary != "exit status 0" || s.Trigger.Type != "operator.requested" {
		t.Fatalf("shell run: %+v", s)
	}
	j2 := byID[string(jobRun2)]
	if j2.Attempt != 2 || j2.Trigger.Type != "core.retry.due" || j2.Outcome.Summary != "job ok" || j2.Retry != nil {
		t.Fatalf("retried job run: %+v", j2)
	}
	// One schema for every executor: identical top-level keys modulo
	// optional fields that only appear when present.
	optional := map[string]bool{"usage": true, "retry": true, "current_trigger": true, "deadline": true, "remaining_timeout_seconds": true, "wait_id": true, "wake_event_id": true}
	var reference []string
	for _, item := range runKeys(t, c, ctx, nil) {
		var keys []string
		for k := range item {
			if !optional[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if reference == nil {
			reference = keys
		} else if strings.Join(keys, ",") != strings.Join(reference, ",") {
			t.Fatalf("run schema differs by executor: %v vs %v", keys, reference)
		}
	}
	for q, want := range map[string]int{"executor=agent": 1, "executor=shell": 1, "work_kind=fixture.job": 2, "outcome=failed": 1, "executor_binding=job": 2, "state=completed": 3} {
		k, v, _ := strings.Cut(q, "=")
		got, err := c.Runs(ctx, url.Values{k: {v}})
		if err != nil || len(got.Items) != want {
			t.Fatalf("filter %s: %d %v", q, len(got.Items), err)
		}
	}

	// The same row/detail code renders every executor's attempt.
	now := time.Now()
	for _, r := range page.Items {
		row := runview.Row(r, now)
		if !strings.Contains(row, r.State) || !strings.Contains(row, r.ExecutorBinding) || !strings.Contains(row, runview.TriggerText(r)) || !strings.Contains(row, r.Outcome.Kind) {
			t.Fatalf("row for %s: %q", r.ID, row)
		}
	}
	detail := runview.Detail(a, []client.Run{a}, now)
	for _, want := range []string{"operator.agent", "agent", "answered", "in 1200", "cost 0.0420 USD", "Attempts for this work", a.Artifacts[0].ID} {
		if !strings.Contains(detail, want) {
			t.Fatalf("agent detail lacks %q:\n%s", want, detail)
		}
	}

	// Interactive model over the real management API: list, detail with the
	// Work's attempts, and log access.
	m := tui.New(c, time.Hour)
	batch := m.Init()().(tea.BatchMsg)
	model, _ := m.Update(batch[0]())
	view := model.View()
	for _, id := range []domain.ID{agentRun, jobRun1, jobRun2, shellRun} {
		r := byID[string(id)]
		if !strings.Contains(view, r.Executor) || !strings.Contains(view, r.WorkKind) {
			t.Fatalf("list view lacks %s:\n%s", id, view)
		}
	}
	// Select the first job attempt (the list is newest first).
	runs, _ := tui.FetchRuns(c, "")
	target := -1
	for i, r := range runs {
		if r.ID == string(jobRun1) {
			target = i
		}
	}
	for i := 0; i < target; i++ {
		model, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	model, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model, _ = model.Update(cmd())
	view = model.View()
	for _, want := range []string{"Run " + string(jobRun1), "job exit 2", "Attempts for this work", "core.retry.due", "job ok"} {
		if !strings.Contains(view, want) {
			t.Fatalf("detail view lacks %q:\n%s", want, view)
		}
	}
	model, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	model, _ = model.Update(cmd())
	if view = model.View(); !strings.Contains(view, "log of "+string(jobRun1)+" triggered by operator.job") {
		t.Fatalf("log view:\n%s", view)
	}

	// One retention policy for every executor (kept_executions=1 per Work):
	// the older job attempt expires, metadata stays, content and executor
	// state are released, and the view reports it.
	result, err := h.scheduler.SweepRetention(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Expired) != 1 || result.Expired[0] != jobRun1 || len(result.Released) != 1 || result.Pending != 0 {
		t.Fatalf("retention sweep: %+v", result)
	}
	if job.forgotten == nil || job.forgotten[0] != jobRun1 || agent.forgotten != nil {
		t.Fatalf("executor Forget: job %v agent %v", job.forgotten, agent.forgotten)
	}
	expired, err := c.Run(ctx, string(jobRun1))
	if err != nil || expired.Retention.State != "expired" || !expired.Retention.ContentReleased || expired.Outcome == nil || expired.Outcome.Summary != "job exit 2" || len(expired.Artifacts) != 1 {
		t.Fatalf("expired run keeps metadata: %+v %v", expired, err)
	}
	var b strings.Builder
	var api *client.Error
	if err = c.ArtifactContent(ctx, expired.Artifacts[0].ID, "", &b); !errors.As(err, &api) || !strings.Contains(api.Error(), "artifact_expired") {
		t.Fatalf("expired artifact content: %v", err)
	}
	if again, err := h.scheduler.SweepRetention(ctx, 100); err != nil || len(again.Expired)+len(again.Released) != 0 {
		t.Fatalf("second sweep not idempotent: %+v %v", again, err)
	}
	rs := recordSet(h.records())
	rs.one(t, "execution.retention_expired", forExecution(jobRun1))
	agentDone := rs.one(t, "execution.completed", forExecution(agentRun))
	if agentDone.Usage == nil || agentDone.Usage.InputTokens == nil || *agentDone.Usage.InputTokens != 1200 || agentDone.Usage.Cost == nil || agentDone.Usage.Currency != "USD" {
		t.Fatalf("executor usage report not on outcome telemetry: %+v", agentDone)
	}

	// The same history is available through the operator CLI.
	var out, errOut strings.Builder
	args := []string{"--endpoint", h.endpoint, "--token-file", filepath.Join(h.state, "private", "operator.token")}
	if code := cli.RunOperator("t", "c", append(args, "runs", "table", "--work-id", jobWork), cli.Env{Getenv: func(string) string { return "" }}, &out, &errOut); code != 0 || !strings.Contains(out.String(), runview.Header) || strings.Count(out.String(), "fixture.job") != 2 {
		t.Fatalf("runs table: %d %q %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := cli.RunOperator("t", "c", append(args, "runs", "log", string(shellRun)), cli.Env{Getenv: func(string) string { return "" }}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "shell-run-output") {
		t.Fatalf("runs log: %d %q %q", code, out.String(), errOut.String())
	}
}

func shellRunDir(state string, id domain.ID) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(state, "executors", "shell", "runs", hex.EncodeToString(sum[:16]))
}

// TestShellRetentionReleasesRunDirectories applies the shared policy to real
// shell attempts: the expired attempt's run directory and artifacts are
// released, the kept attempt is untouched.
func TestShellRetentionReleasesRunDirectories(t *testing.T) {
	ctx := context.Background()
	script := `case "$OS_TRIGGER_TYPE" in
operator.flaky) echo first-failure; exit 3;;
*) echo retried-ok;;
esac`
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": script}))
	clock := conformance.NewClock()
	h := newShellHostWith(t, state, bin, clock, "daemon", false, shellHostOptions{telemetry: true, kept: 1})
	work := h.submit("operator.flaky", "flaky", "flaky").RoutedWorkID
	first := h.runOnce(work)
	clock.Advance(6 * time.Minute)
	if _, err := h.scheduler.Poll(ctx, h.timers, 20); err != nil {
		t.Fatal(err)
	}
	second := h.runOnce(work)
	for _, id := range []domain.ID{first, second} {
		if _, err := os.Stat(shellRunDir(state, id)); err != nil {
			t.Fatalf("run directory for %s missing before sweep: %v", id, err)
		}
	}
	x1, _ := h.operator().Execution(ctx, string(first))
	result, err := h.scheduler.SweepRetention(ctx, 10)
	if err != nil || len(result.Released) != 1 || result.Released[0] != first {
		t.Fatalf("sweep: %+v %v", result, err)
	}
	if _, err = os.Stat(shellRunDir(state, first)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired run directory kept: %v", err)
	}
	if _, err = os.Stat(shellRunDir(state, second)); err != nil {
		t.Fatalf("kept run directory removed: %v", err)
	}
	var b strings.Builder
	for _, a := range x1.Artifacts {
		if err = h.operator().ArtifactContent(ctx, a.ID, "", &b); err == nil {
			t.Fatalf("expired artifact %s still served", a.ID)
		}
	}
	x2, _ := h.operator().Execution(ctx, string(second))
	if err = h.operator().ArtifactContent(ctx, x2.Artifacts[0].ID, "", &b); err != nil || !strings.Contains(b.String(), "retried-ok") {
		t.Fatalf("kept artifact: %v %q", err, b.String())
	}
}

// TestDoctorChecks covers storage, config, bindings, connections, secrets,
// reconcile lag/failure, declared non-re-derivable families, lease recovery,
// pinned executors and the CLI.
func TestDoctorChecks(t *testing.T) {
	ctx := context.Background()
	h, _, job, clock := mixedHost(t, 20)
	c := h.operator()
	source := extension.Source{Namespace: "fixture/doctor", ScopeID: "ops"}
	adapter := newFixtureIngress()
	adapter.pages[""] = extension.Snapshot{ScanID: "scan-1", Complete: true, Checkpoint: "c1"}
	if err := h.ingress.Register(ctx, "doctor_source", source, adapter, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ingress.Reconcile(ctx, "doctor_source"); err != nil {
		t.Fatal(err)
	}
	checks := func(secrets bool) (client.Doctor, map[string]client.Check) {
		t.Helper()
		d, err := c.Doctor(ctx, secrets)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]client.Check{}
		for _, ch := range d.Checks {
			m[ch.Name] = ch
		}
		return d, m
	}
	d, m := checks(false)
	for name, status := range map[string]string{"storage": "ok", "lease_recovery": "ok", "resource_holds": "ok", "retention": "ok", "reconcile_lag": "ok", "non_rederivable": "info", "config": "ok", "bindings": "ok", "connections": "ok", "secrets": "info", "pinned_executors": "ok"} {
		if m[name].Status != status {
			t.Fatalf("check %s = %+v, want %s (all %+v)", name, m[name], status, d.Checks)
		}
	}
	if d.State != "ok" || len(m["non_rederivable"].Items) != 1 || !strings.Contains(m["non_rederivable"].Items[0], oneShotFamily) {
		t.Fatalf("doctor baseline: %+v", d)
	}
	if _, m = checks(true); m["secrets"].Status != "ok" || m["secrets"].Code != "resolved" {
		t.Fatalf("check_secrets: %+v", m["secrets"])
	}
	raw := map[string]any{}
	if err := c.Do(ctx, "GET", "doctor", url.Values{"check_secrets": {"true"}}, nil, "", &raw); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(raw); strings.Contains(string(b), "shell-secret") || strings.Contains(string(b), "cmd:") {
		t.Fatalf("doctor leaked secret material: %s", b)
	}

	// Degradations: a failing scan, an unrecovered expired lease and an
	// unapplied config edit.
	adapter.failure = errors.New("fixture scan unavailable: token=abc")
	clock.Advance(2 * time.Minute)
	if _, err := h.ingress.Reconcile(ctx, "doctor_source"); err == nil {
		t.Fatal("expected scan failure")
	}
	job.block = make(chan struct{})
	defer close(job.block)
	running := h.submit("operator.job", "job-held", "job-held").RoutedWorkID
	attempt, err := h.scheduler.Start(ctx, domain.ID(running))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	if err = config.SaveAtomic(filepath.Join(h.state, "config", "config.json"), []byte(strings.Replace(mixedConfig(), `"Operations"`, `"Operations edited"`, 1))); err != nil {
		t.Fatal(err)
	}
	d, m = checks(false)
	if d.State != "warn" || m["reconcile_lag"].Code != "scan_failing" || m["lease_recovery"].Code != "expired_leases" || m["lease_recovery"].Items[0] != string(attempt.Lease.ExecutionID) || m["config"].Code != "pending_edit" {
		t.Fatalf("degraded doctor: %+v", d.Checks)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "token=abc") {
		t.Fatalf("doctor echoed provider error text: %s", b)
	}
	rs := recordSet(h.records())
	rs.one(t, "ingress.scan_started", nil)
	completed := rs.one(t, "ingress.scan_completed", nil)
	failedScan := rs.one(t, "ingress.scan_failed", nil)
	if completed.Attributes["ingress.source"] != source.Namespace || completed.Correlation.ScopeID != "ops" || failedScan.Attributes["ingress.error"] != "provider_failed" || strings.Contains(failedScan.Attributes["ingress.error"], "token") {
		t.Fatalf("ingress telemetry: %+v %+v", completed, failedScan)
	}

	var out, errOut strings.Builder
	args := []string{"--endpoint", h.endpoint, "--token-file", filepath.Join(h.state, "private", "operator.token")}
	if code := cli.RunOperator("t", "c", append(args, "doctor"), cli.Env{Getenv: func(string) string { return "" }}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "overall: warn") || !strings.Contains(out.String(), "expired_leases") {
		t.Fatalf("doctor CLI: %d %q %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := cli.RunOperator("t", "c", append(args, "doctor", "--json"), cli.Env{Getenv: func(string) string { return "" }}, &out, &errOut); code != 0 || json.Unmarshal([]byte(out.String()), &client.Doctor{}) != nil {
		t.Fatalf("doctor --json: %d %q", code, out.String())
	}
}
