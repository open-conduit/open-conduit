package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/internal/storage/sqlite"
	"github.com/alsiola/open-conduit/internal/telemetry"
	"github.com/alsiola/open-conduit/observability"
	"github.com/alsiola/open-conduit/storage"
)

func (h *shellHost) records() []observability.Record {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.pipeline.Flush(ctx); err != nil {
		h.t.Fatalf("flush telemetry: %v", err)
	}
	return h.memory.Records()
}

func (h *shellHost) diagnostics() []string {
	h.diagMu.Lock()
	defer h.diagMu.Unlock()
	return append([]string(nil), h.diagnosed...)
}

type recordSet []observability.Record

func (rs recordSet) where(name string, pred func(observability.Record) bool) recordSet {
	var out recordSet
	for _, r := range rs {
		if r.Name == name && (pred == nil || pred(r)) {
			out = append(out, r)
		}
	}
	return out
}
func (rs recordSet) one(t *testing.T, name string, pred func(observability.Record) bool) observability.Record {
	t.Helper()
	got := rs.where(name, pred)
	if len(got) != 1 {
		var names []string
		for _, r := range rs {
			names = append(names, r.Name+"("+string(r.Correlation.ExecutionID)+")")
		}
		t.Fatalf("want exactly one %s, got %d among %v", name, len(got), names)
	}
	return got[0]
}
func forExecution(id domain.ID) func(observability.Record) bool {
	return func(r observability.Record) bool { return r.Correlation.ExecutionID == id }
}

// checkRecordInvariants: every exported record is valid, identifiable for
// duplicate delivery, redacted and timestamped by the trusted clock.
func checkRecordInvariants(t *testing.T, rs []observability.Record, secrets ...string) {
	t.Helper()
	ids := map[string]string{}
	for _, r := range rs {
		if err := observability.Validate(r); err != nil {
			t.Fatalf("invalid record %+v: %v", r, err)
		}
		id := r.Attributes["record_id"]
		if id == "" {
			t.Fatalf("record without record_id: %+v", r)
		}
		if prior, ok := ids[id]; ok && prior != r.Name {
			t.Fatalf("record_id %s reused by %s and %s", id, prior, r.Name)
		}
		ids[id] = r.Name
		text := fmt.Sprintf("%+v", r)
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Fatalf("secret %q escaped into telemetry: %s", s, text)
			}
		}
	}
}

// TestLifecycleTelemetryExplainsRuns is the 02-08 trace/log acceptance on the
// real shell + os toolbelt workflow: records alone answer why each attempt
// started, who ran it, what it emitted and why it stopped, including the
// declared wait and its wakeup.
func TestLifecycleTelemetryExplainsRuns(t *testing.T) {
	ctx := context.Background()
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": toolbeltScript}))
	h := newShellHostWith(t, state, bin, conformance.NewClock(), "daemon", false, shellHostOptions{telemetry: true})
	c := h.operator()
	root := h.submit("operator.requested", "root", "thread-1")
	work := domain.ID(root.RoutedWorkID)
	first, err := h.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	h.observe(first)
	h.routeAll()
	w, _ := c.Work(ctx, string(work))
	wake, err := c.Wake(ctx, string(work), client.WakeRequest{ExpectedRevision: w.Revision, Type: "operator.reply", DeliveryID: "reply-1", CorrelationKey: &client.Key{Source: "ops", ExternalID: "thread-1"}}, "wake-1")
	if err != nil || !wake.Matched {
		t.Fatalf("wake: %+v %v", wake, err)
	}
	second, err := h.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	h.observe(second)

	rs := recordSet(h.records())
	checkRecordInvariants(t, rs, "shell-secret", first.Request.Context.ToolbeltToken, second.Request.Context.ToolbeltToken)
	x1, _ := c.Execution(ctx, string(first.Lease.ExecutionID))
	x2, _ := c.Execution(ctx, string(second.Lease.ExecutionID))

	// Why did it start? The accepted operator event was routed by a named
	// route to the binding, queued and claimed with that event as trigger.
	route := rs.one(t, "route.decided", func(r observability.Record) bool { return r.Correlation.EventID == domain.ID(root.ID) })
	if route.Attributes["route.name"] != "incoming" || route.Attributes["route.binding"] != "run" || route.To != "queue" || route.Correlation.WorkID != work {
		t.Fatalf("route decision: %+v", route)
	}
	rs.one(t, "work.created", func(r observability.Record) bool { return r.Correlation.WorkID == work })
	start1 := rs.one(t, "execution.started", forExecution(first.Lease.ExecutionID))
	if start1.Attributes["trigger.type"] != "operator.requested" || start1.Correlation.EventID != domain.ID(root.ID) || start1.Attributes["trigger.id"] != root.ID {
		t.Fatalf("first start: %+v", start1)
	}
	start2 := rs.one(t, "execution.started", forExecution(second.Lease.ExecutionID))
	if start2.Attributes["trigger.type"] != "operator.reply" || start2.Correlation.EventID != domain.ID(wake.Event.ID) {
		t.Fatalf("second start must be explained by the wake event: %+v", start2)
	}
	// Who ran it? Binding, executor registration, generation and lease owner.
	for _, r := range []observability.Record{start1, start2} {
		if r.Correlation.ExecutorBinding != "run" || r.Attributes["executor"] != "shell" || r.Attributes["config.generation"] != "1" || r.Attributes["lease.owner"] != "daemon" || r.Correlation.ScopeID != "ops" {
			t.Fatalf("executor identity: %+v", r)
		}
	}
	if l := rs.one(t, "execution.launched", forExecution(first.Lease.ExecutionID)); l.Attributes["launch.mode"] != "run" {
		t.Fatalf("launch acknowledgement: %+v", l)
	}
	// What did it emit? Emissions, child Work and toolbelt calls carry the
	// attempt's correlation and trace.
	emitted := rs.one(t, "event.accepted", func(r observability.Record) bool { return r.Attributes["event.type"] == "fixture.reported.v1" })
	if emitted.Correlation.ExecutionID != first.Lease.ExecutionID || emitted.Correlation.TraceID != x1.TraceID || emitted.Attributes["event.causation_id"] != root.ID {
		t.Fatalf("emission correlation: %+v (trace %s)", emitted, x1.TraceID)
	}
	child := rs.one(t, "work.created", func(r observability.Record) bool { return r.Attributes["work.parent_id"] == string(work) })
	if child.Attributes["work.kind"] != "fixture.child" {
		t.Fatalf("child work: %+v", child)
	}
	calls := map[string]bool{}
	for _, r := range rs.where("toolbelt.called", forExecution(first.Lease.ExecutionID)) {
		calls[r.Attributes["toolbelt.operation"]] = true
		if r.Correlation.TraceID != x1.TraceID {
			t.Fatalf("toolbelt call trace: %+v", r)
		}
	}
	for _, op := range []string{"read_context", "emit", "create_work", "wait"} {
		if !calls[op] {
			t.Fatalf("toolbelt call %s missing: %v", op, calls)
		}
	}
	// The post-wait read is refused and recorded as such.
	if len(rs.where("toolbelt.rejected", forExecution(first.Lease.ExecutionID))) == 0 {
		t.Fatal("refused toolbelt call after wait not recorded")
	}
	// Why did it stop? A declared wait parked the Work; the wake winner and the
	// declared outcome explain the rest.
	if d := rs.one(t, "toolbelt.declared", forExecution(first.Lease.ExecutionID)); d.Attributes["declaration.kind"] != "wait" {
		t.Fatalf("declaration: %+v", d)
	}
	stop1 := rs.one(t, "execution.waiting", forExecution(first.Lease.ExecutionID))
	if stop1.Attributes["outcome.kind"] != "waiting" || stop1.Attributes["execution.artifacts"] != "5" {
		t.Fatalf("first stop: %+v", stop1)
	}
	registered := rs.one(t, "wait.registered", forExecution(first.Lease.ExecutionID))
	if registered.Attributes["wait.types"] != "operator.reply" || registered.Attributes["wait.timeout_timer_id"] == "" {
		t.Fatalf("wait registration: %+v", registered)
	}
	rs.one(t, "timer.armed", func(r observability.Record) bool {
		return r.Attributes["timer.id"] == registered.Attributes["wait.timeout_timer_id"]
	})
	woken := rs.one(t, "wait.woken", nil)
	if woken.Correlation.EventID != domain.ID(wake.Event.ID) || woken.Attributes["wait.wake_event_type"] != "operator.reply" || woken.Correlation.ExecutionID != first.Lease.ExecutionID {
		t.Fatalf("wakeup: %+v", woken)
	}
	rs.one(t, "timer.cancelled", func(r observability.Record) bool {
		return r.Attributes["timer.id"] == registered.Attributes["wait.timeout_timer_id"]
	})
	stop2 := rs.one(t, "execution.completed", forExecution(second.Lease.ExecutionID))
	if stop2.Attributes["outcome.summary"] != "reply handled" || stop2.From != "running" || stop2.Attributes["execution.duration_ms"] == "" {
		t.Fatalf("final stop: %+v", stop2)
	}
	// Work lifecycle transitions are each reported once with their states.
	var states []string
	for _, r := range rs.where("work.transitioned", func(r observability.Record) bool { return r.Correlation.WorkID == work }) {
		states = append(states, r.From+">"+r.To)
	}
	if got := strings.Join(states, ","); got != "ready>running,running>waiting,waiting>ready,ready>running,running>completed" {
		t.Fatalf("work transitions %s", got)
	}
	// Execution records stay on the attempt trace; event chains join it.
	for _, r := range rs {
		if r.Correlation.ExecutionID == first.Lease.ExecutionID && strings.HasPrefix(r.Name, "execution.") && r.Correlation.TraceID != x1.TraceID {
			t.Fatalf("attempt record off trace: %+v", r)
		}
		if r.Correlation.ExecutionID == second.Lease.ExecutionID && strings.HasPrefix(r.Name, "execution.") && r.Correlation.TraceID != x2.TraceID {
			t.Fatalf("second attempt record off trace: %+v", r)
		}
	}
	for _, code := range h.diagnostics() {
		if code != "uncorrelated_record" {
			t.Fatalf("unexpected telemetry diagnostic %s", code)
		}
	}
}

// TestLifecycleTelemetryRetriesAndRecovery covers retry backoff, timer firing,
// the retried attempt's trigger, lease recovery and adoption.
func TestLifecycleTelemetryRetriesAndRecovery(t *testing.T) {
	ctx := context.Background()
	script := `case "$OS_TRIGGER_TYPE" in
operator.flaky) echo first-failure >&2; exit 3;;
core.retry.due) echo retried-ok;;
*) echo $$ > "$HOME/pid"; while [ ! -e "$HOME/release" ]; do sleep 0.02; done; echo adopted-ok;;
esac`
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": script}))
	clock := conformance.NewClock()
	h := newShellHostWith(t, state, bin, clock, "daemon", false, shellHostOptions{telemetry: true})
	root := h.submit("operator.flaky", "flaky", "flaky")
	work := domain.ID(root.RoutedWorkID)
	a1, err := h.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	h.observe(a1)
	clock.Advance(6 * time.Minute)
	if _, err = h.scheduler.Poll(ctx, h.timers, 20); err != nil {
		t.Fatal(err)
	}
	a2, err := h.scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	h.observe(a2)

	rs := recordSet(h.records())
	failed := rs.one(t, "execution.failed", forExecution(a1.Lease.ExecutionID))
	if failed.Attributes["outcome.summary"] != "exit status 3" || failed.Attributes["execution.resource_excluded"] != "true" {
		t.Fatalf("failure: %+v", failed)
	}
	retry := rs.one(t, "retry.scheduled", forExecution(a1.Lease.ExecutionID))
	if retry.Attributes["retry.failure_count"] != "1" || retry.Attributes["retry.eligible_at"] == "" || retry.Attributes["retry.exhausted"] != "false" {
		t.Fatalf("retry: %+v", retry)
	}
	armed := rs.one(t, "timer.armed", func(r observability.Record) bool { return r.Attributes["event.type"] == "core.retry.due" })
	fired := rs.one(t, "timer.fired", func(r observability.Record) bool { return r.Attributes["event.type"] == "core.retry.due" })
	if armed.Attributes["timer.id"] != fired.Attributes["timer.id"] || fired.Correlation.WorkID != work {
		t.Fatalf("retry timer: %+v %+v", armed, fired)
	}
	rs.one(t, "route.decided", func(r observability.Record) bool {
		return r.Correlation.EventID == fired.Correlation.EventID && r.To == "retry"
	})
	start2 := rs.one(t, "execution.started", forExecution(a2.Lease.ExecutionID))
	if start2.Attributes["trigger.type"] != "core.retry.due" || start2.Attributes["execution.attempt"] != "2" || start2.Correlation.EventID != fired.Correlation.EventID {
		t.Fatalf("retried attempt start: %+v", start2)
	}
	if done := rs.one(t, "execution.completed", forExecution(a2.Lease.ExecutionID)); done.Attributes["outcome.summary"] != "exit status 0" {
		t.Fatalf("retried outcome: %+v", done)
	}

	// Lease recovery and adoption of a live process.
	adopt := h.submit("operator.adopt", "adopt", "adopt")
	a3, err := h.scheduler.Start(ctx, domain.ID(adopt.RoutedWorkID))
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(state, "pid")
	for deadline := time.Now().Add(10 * time.Second); !fileHasLine(pidFile) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	clock.Advance(2 * time.Minute)
	poll, err := h.scheduler.Poll(ctx, h.timers, 20)
	if err != nil || len(poll.Recovered) != 1 {
		t.Fatalf("poll recovery: %+v %v", poll, err)
	}
	if err = os.WriteFile(filepath.Join(state, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.observe(poll.Recovered[0])
	rs = recordSet(h.records())
	checkRecordInvariants(t, rs, "shell-secret")
	recovered := rs.one(t, "execution.recovered", forExecution(a3.Lease.ExecutionID))
	if recovered.Attributes["lease.previous_epoch"] != "1" || recovered.Attributes["lease.epoch"] != "2" || recovered.From != "running" {
		t.Fatalf("recovery: %+v", recovered)
	}
	launches := rs.where("execution.launched", forExecution(a3.Lease.ExecutionID))
	if len(launches) != 2 || launches[1].Attributes["launch.mode"] != "recover" {
		t.Fatalf("adoption acknowledgement: %+v", launches)
	}
	x3, _ := h.operator().Execution(ctx, string(a3.Lease.ExecutionID))
	if recovered.Correlation.TraceID != x3.TraceID {
		t.Fatal("recovery left the original attempt trace")
	}
	if done := rs.one(t, "execution.completed", forExecution(a3.Lease.ExecutionID)); done.Attributes["lease.epoch"] != "2" {
		t.Fatalf("adopted outcome: %+v", done)
	}
}

// hungExporter blocks every export until released, ignoring its context.
type hungExporter struct {
	release chan struct{}
	once    sync.Once
}

func (e *hungExporter) Export(context.Context, []observability.Record) error {
	<-e.release
	return nil
}
func (e *hungExporter) Shutdown(context.Context) error {
	e.once.Do(func() { close(e.release) })
	return nil
}

type failingExporter struct{}

func (failingExporter) Export(context.Context, []observability.Record) error {
	return errors.New("collector unavailable: secret-collector-detail")
}
func (failingExporter) Shutdown(context.Context) error { return nil }

// TestTelemetryExportFailureDoesNotBlockProgress runs the full workflow with
// a hung and a failing exporter beside the healthy one. Durable progress is
// unaffected, the healthy exporter is isolated from both, and losses are
// counted and surfaced through doctor without exporter error text.
func TestTelemetryExportFailureDoesNotBlockProgress(t *testing.T) {
	ctx := context.Background()
	hung := &hungExporter{release: make(chan struct{})}
	t.Cleanup(func() { hung.once.Do(func() { close(hung.release) }) })
	var pipeline *telemetry.Pipeline
	check := func(context.Context) runtime.Check {
		status, code, detail := pipeline.Health()
		return runtime.Check{Name: "telemetry", Status: status, Code: code, Detail: detail}
	}
	state, bin := shellState(t, shellConfig(t, map[string]any{"command": toolbeltScript}))
	h := newShellHostWith(t, state, bin, conformance.NewClock(), "daemon", false, shellHostOptions{telemetry: true, exporters: []observability.Exporter{hung, failingExporter{}}, buffer: 8, checks: []func(context.Context) runtime.Check{check}})
	pipeline = h.pipeline
	c := h.operator()
	root := h.submit("operator.requested", "root", "thread-1")
	work := domain.ID(root.RoutedWorkID)
	done := make(chan error, 1)
	go func() {
		first, err := h.scheduler.Start(ctx, work)
		if err != nil {
			done <- err
			return
		}
		h.observe(first)
		h.routeAll()
		w, _ := c.Work(ctx, string(work))
		if _, err = c.Wake(ctx, string(work), client.WakeRequest{ExpectedRevision: w.Revision, Type: "operator.reply", DeliveryID: "reply-1", CorrelationKey: &client.Key{Source: "ops", ExternalID: "thread-1"}}, "wake-1"); err != nil {
			done <- err
			return
		}
		second, err := h.scheduler.Start(ctx, work)
		if err == nil {
			h.observe(second)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("durable progress blocked behind telemetry export")
	}
	if w, err := c.Work(ctx, string(work)); err != nil || w.State != "completed" {
		t.Fatalf("work did not complete: %+v %v", w, err)
	}
	// The healthy exporter still has the full history despite its siblings.
	rs := recordSet(h.memory.Records())
	deadline := time.Now().Add(5 * time.Second)
	for len(rs.where("execution.completed", nil)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		rs = recordSet(h.memory.Records())
	}
	if len(rs.where("execution.completed", nil)) != 1 || len(rs.where("execution.started", nil)) != 2 {
		t.Fatalf("healthy exporter lost records: %d", len(rs))
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := pipeline.Stats(); st.Dropped > 0 && st.ExportFailures > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := pipeline.Stats()
	if st.Dropped == 0 || st.ExportFailures == 0 || st.Observed == 0 {
		t.Fatalf("losses not accounted: %+v", st)
	}
	d, err := c.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range d.Checks {
		if ch.Name == "telemetry" {
			found = true
			if ch.Status != "warn" || strings.Contains(ch.Detail, "secret-collector-detail") {
				t.Fatalf("telemetry doctor check: %+v", ch)
			}
		}
	}
	if !found {
		t.Fatal("extension telemetry check missing from doctor")
	}
}

// TestInstrumentedRuntimeSharedCases runs all shared runtime cases through
// production components behind lifecycle instrumentation: semantics are
// unchanged and every emitted record validates.
func TestInstrumentedRuntimeSharedCases(t *testing.T) {
	conformance.RunRuntime(t, func(o conformance.RuntimeOptions) conformance.RuntimeFixture {
		return waitFixture(t, "instrumented", o)
	})
}

// validatingObserver fails the test on any invalid record and counts them.
type validatingObserver struct {
	t     *testing.T
	mu    sync.Mutex
	count int
}

func (v *validatingObserver) Observe(_ context.Context, r observability.Record) error {
	if err := observability.Validate(r); err != nil {
		v.t.Errorf("invalid lifecycle record %+v: %v", r, err)
		return err
	}
	if r.Attributes["record_id"] == "" {
		v.t.Errorf("record without identity %+v", r)
	}
	v.mu.Lock()
	v.count++
	v.mu.Unlock()
	return nil
}

func instrumentedBackend(t *testing.T, path string) storage.Store {
	t.Helper()
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	o := &validatingObserver{t: t}
	store, err := runtime.Instrument(db, runtime.TelemetryOptions{Observer: o, Diagnostic: func(code string) {
		if code != "uncorrelated_record" {
			t.Errorf("telemetry diagnostic %s", code)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestInstrumentationRollbackEmitsNothing: a rolled-back transaction must not
// produce telemetry for state that never committed.
func TestInstrumentationRollbackEmitsNothing(t *testing.T) {
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	memory := &telemetry.Memory{}
	p, _ := telemetry.New(telemetry.Options{Exporters: []observability.Exporter{memory}})
	defer p.Shutdown(context.Background())
	store, _ := runtime.Instrument(db, runtime.TelemetryOptions{Observer: p})
	now := conformance.NewClock().Now()
	accept := func(fail bool) error {
		return store.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
			if _, err := tx.Scope("s"); errors.Is(err, storage.ErrNotFound) {
				if err = tx.PutScope(domain.Scope{ID: "s", Name: "s"}, 0); err != nil {
					return err
				}
			}
			id := domain.ID(fmt.Sprintf("e-%v", fail))
			if _, _, err := runtime.AcceptInTx(tx, "s", domain.Event{ID: id, Type: "fixture.x", Source: "fixture", DeliveryID: string(id), OccurredAt: now, ReceivedAt: now}); err != nil {
				return err
			}
			if fail {
				return errors.New("abort")
			}
			return nil
		})
	}
	if err = accept(true); err == nil {
		t.Fatal("expected rollback")
	}
	if err = accept(false); err != nil {
		t.Fatal(err)
	}
	if err = p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := memory.Records()
	if len(got) != 1 || got[0].Correlation.EventID != "e-false" || got[0].Name != "event.accepted" {
		t.Fatalf("records after rollback: %+v", got)
	}
}
