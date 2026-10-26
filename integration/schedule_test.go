package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// Scheduled work (05-01) over the production router, scheduler and durable
// timers on SQLite with a fake clock, composed as bootstrap composes them:
// timer recurrence is the schedule extension's RecurrenceFunc, the `schedule`
// route makes triggers input of one standing job Work, and the scheduler's
// job kind policy takes the whole pending batch as one run.

const (
	scNS     = "schedule/demo"
	scScope  = "demo"
	scSecret = "s3cret"
	scSource = "schedule/demo/jobs"
)

type jobRun struct {
	Execution domain.ID
	Trigger   domain.Event
	Inputs    []domain.ID
}

// jobExecutor records each run. A run stays "going" until the test observes
// its attempt; outcome selects how it ends.
type jobExecutor struct {
	mu      sync.Mutex
	runs    []jobRun
	outcome domain.OutcomeKind
	// hint, when set, rides a failed outcome as an executor retry hint.
	hint *time.Time
}

func (x *jobExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: "none"}
}
func (x *jobExecutor) Run(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
		return nil, err
	}
	run := jobRun{Execution: r.Execution.ID, Trigger: r.Trigger}
	for _, in := range r.Inputs {
		run.Inputs = append(run.Inputs, in.ID)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.runs = append(x.runs, run)
	kind := x.outcome
	if kind == "" {
		kind = domain.OutcomeCompleted
	}
	res := extension.Result{Outcome: domain.Outcome{Kind: kind, Summary: "job ran"}}
	if x.hint != nil && kind == domain.OutcomeFailed {
		res.RetryHint = &extension.RetryHint{ResetAt: x.hint}
	}
	return &schedulerHandle{result: res}, nil
}
func (x *jobExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}
func (x *jobExecutor) all() []jobRun {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]jobRun(nil), x.runs...)
}

type scEnv struct {
	t        *testing.T
	clock    *ghClock
	path     string
	entry    string
	enabled  bool
	x        *jobExecutor
	store    *sqlite.Store
	ingress  *runtime.Ingress
	router   *runtime.Router
	sched    *runtime.Scheduler
	timers   *runtime.Timers
	adapter  *sc.Ingress
	attempts map[domain.ID]runtime.Attempt
	fired    []runtime.TimerFire
	// policy overrides the job kind policy (contract tests); routes and
	// bindings override the single `jobs` route.
	policy   *runtime.WorkKindPolicy
	routes   []domain.Route
	bindings []domain.Binding
}

func scEntry(jobs string) string {
	return `{"namespace":"` + scNS + `","scope":"` + scScope + `","webhook_secret":"hook","jobs":` + jobs + `}`
}

func newScheduleEnv(t *testing.T, jobs string, start time.Time) *scEnv {
	t.Helper()
	e := &scEnv{t: t, clock: &ghClock{t: start}, path: filepath.Join(t.TempDir(), "conduit.db"), entry: scEntry(jobs), enabled: true, x: &jobExecutor{}, attempts: map[domain.ID]runtime.Attempt{}}
	e.open()
	e.putScope()
	e.register()
	return e
}

func (e *scEnv) settings() (sc.Settings, bool) {
	s, err := sc.ParseExtension([]byte(e.entry), []string{"hook"})
	return s, err == nil
}

func (e *scEnv) open() {
	t := e.t
	store, err := sqlite.Open(context.Background(), e.path)
	if err != nil {
		t.Fatal(err)
	}
	e.store = store
	t.Cleanup(func() { _ = store.Close() })
	if e.ingress, err = runtime.NewIngress(store, e.clock.Now, runtime.IngressOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", Handlers: map[string]extension.RouteHandler{sc.Type: sc.Route{}},
		Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: e.x, Capabilities: e.x.Capabilities()}}}); err != nil {
		t.Fatal(err)
	}
	if e.sched, err = runtime.NewScheduler(store, e.clock.Now, e.router, runtime.SchedulerOptions{Owner: "schedule-test", LeaseDuration: 24 * time.Hour, Generation: "1",
		Executors: map[string]extension.Executor{"fixture": e.x}, Timeout: 24 * time.Hour,
		WorkKinds: map[string]runtime.WorkKindPolicy{sc.WorkKind: e.kindPolicy()}}); err != nil {
		t.Fatal(err)
	}
	if e.timers, err = runtime.NewTimers(store, e.clock.Now, runtime.TimerOptions{Recurrences: map[string]runtime.RecurrenceFunc{sc.RecurrenceKind: sc.Recurrence}}); err != nil {
		t.Fatal(err)
	}
	e.adapter = sc.NewIngress(scNS, e.settings, e.clock.Now)
}

func (e *scEnv) kindPolicy() runtime.WorkKindPolicy {
	if e.policy != nil {
		return *e.policy
	}
	return runtime.WorkKindPolicy{Standing: true, Trigger: sc.SelectTrigger}
}

func (e *scEnv) register() {
	if err := e.ingress.Register(context.Background(), scSource, extension.Source{Namespace: scNS, ScopeID: scScope}, e.adapter, time.Hour); err != nil {
		e.t.Fatal(err)
	}
}

// reopen is a daemon restart: every component is rebuilt over the same file.
// In-memory observation handles are lost, as they are in a crash.
func (e *scEnv) reopen() {
	_ = e.store.Close()
	e.attempts = map[domain.ID]runtime.Attempt{}
	e.open()
	e.register()
}

func (e *scEnv) read(fn func(storage.Tx) error) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *scEnv) putScope() {
	e.t.Helper()
	meta, _ := json.Marshal(map[string]any{"enabled": e.enabled, "extensions": []map[string]any{
		{"name": "jobs", "type": "schedule", "config": json.RawMessage(e.entry), "secret_names": []string{"hook"}},
	}})
	e.read(func(tx storage.Tx) error {
		var rev domain.Revision
		if cur, err := tx.Scope(scScope); err == nil {
			rev = cur.Revision
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		bindings, routes := e.bindings, e.routes
		if bindings == nil {
			bindings = []domain.Binding{{Name: "job", Executor: "fixture"}}
		}
		if routes == nil {
			routes = []domain.Route{{Name: "jobs", Handler: sc.Type, Binding: "job", ActivePolicy: "queue"}}
		}
		return tx.PutScope(domain.Scope{ID: scScope, Name: "demo", Bindings: bindings, Routes: routes,
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, rev)
	})
}

// configure applies a config generation: the scope's effective metadata and
// the schedule timers' reconciliation, as bootstrap's OnApply does.
func (e *scEnv) configure(jobs string, enabled bool) sc.EnsureResult {
	e.t.Helper()
	e.entry, e.enabled = scEntry(jobs), enabled
	e.putScope()
	return e.ensure()
}

func (e *scEnv) ensure() sc.EnsureResult {
	e.t.Helper()
	var entries []sc.Entry
	if s, ok := e.settings(); ok {
		entries = append(entries, sc.Entry{Scope: scScope, Enabled: e.enabled, Settings: s})
	}
	res, err := sc.Ensure(context.Background(), e.store, e.clock.Now(), e.timers, entries)
	if err != nil || len(res.Codes) > 0 {
		e.t.Fatalf("ensure: %v %v", err, res.Codes)
	}
	return res
}

func (e *scEnv) route() {
	e.t.Helper()
	var after uint64
	for {
		page, err := e.ingress.Pending(context.Background(), after, 200)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, p := range page.Items {
			if p.Diagnostic != "" {
				e.t.Fatalf("pending diagnostic %s", p.Diagnostic)
			}
			if err := e.router.Route(context.Background(), p.Event.Event.ID); err != nil {
				e.t.Fatalf("route %s: %v", p.Event.Event.Type, err)
			}
		}
		if !page.More {
			return
		}
		after = page.NextSequence
	}
}

// pass is one daemon loop pass: route, poll timers, route what they
// accepted, then start every ready Work with pending input (a Work with a
// run going is not ready, so it starts nothing).
func (e *scEnv) pass() {
	t := e.t
	t.Helper()
	// The daemon renews the lease of every run it observes.
	for _, a := range e.attempts {
		if err := e.sched.Renew(context.Background(), a.Lease); err != nil {
			t.Fatalf("renew %s: %v", a.Lease.ExecutionID, err)
		}
	}
	e.route()
	poll, err := e.sched.Poll(context.Background(), e.timers, 100)
	if err != nil {
		t.Fatal(err)
	}
	e.fired = append(e.fired, poll.Timers...)
	e.route()
	var ready []domain.ID
	e.read(func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		for _, w := range works {
			if w.State != domain.WorkReady {
				continue
			}
			in, er := runtime.PendingInputsInTx(tx, w.ID)
			if er != nil {
				return er
			}
			if len(in) > 0 {
				ready = append(ready, w.ID)
			}
		}
		return err
	})
	for _, id := range ready {
		a, err := e.sched.Start(context.Background(), id)
		if err != nil {
			var xs []domain.Execution
			e.read(func(tx storage.Tx) error { var er error; xs, er = tx.ListExecutions(id); return er })
			for _, x := range xs {
				st, _ := e.sched.InspectAttempt(context.Background(), x.ID)
				t.Logf("%s %s excluded=%v launch=%s", x.ID, x.State, st.Excluded, st.LaunchState)
			}
			t.Fatalf("start %s: %v", id, err)
		}
		e.attempts[id] = a
	}
}

// finish ends the run going on a job's Work.
func (e *scEnv) finish(job string) {
	e.t.Helper()
	id := domain.ID(sc.WorkID(scNS, job))
	a, ok := e.attempts[id]
	if !ok {
		e.t.Fatalf("no run going for %s", job)
	}
	delete(e.attempts, id)
	if err := e.sched.Observe(context.Background(), a); err != nil {
		e.t.Fatalf("observe %s: %v", job, err)
	}
}

func (e *scEnv) at(t time.Time) { e.clock.mu.Lock(); e.clock.t = t; e.clock.mu.Unlock() }

func (e *scEnv) work(job string) (domain.Work, bool) {
	var w domain.Work
	found := false
	e.read(func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(domain.ID(sc.WorkID(scNS, job)))
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return w, found
}

func (e *scEnv) timer(job string) (domain.Timer, bool) {
	var timer domain.Timer
	found := false
	e.read(func(tx storage.Tx) error {
		var err error
		timer, err = tx.Timer(domain.ID(sc.TimerID(scNS, job)))
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return timer, found
}

// slots lists the published timed slots of a job.
func (e *scEnv) slots(job string) []string {
	var out []string
	e.read(func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if p, ok := sc.Payload(a.Event); ok && a.Event.Type == sc.TypeSlot && p.Job == job {
				out = append(out, p.Slot)
			}
		}
		return err
	})
	return out
}

func (e *scEnv) runsOf(job string) []jobRun {
	var out []jobRun
	for _, r := range e.x.all() {
		if strings.HasPrefix(string(r.Execution), sc.WorkID(scNS, job)+"/") {
			out = append(out, r)
		}
	}
	return out
}

func label(ev domain.Event) string {
	p, _ := sc.Payload(ev)
	if ev.Source == sc.ManualSource {
		return "by hand"
	}
	return p.Trigger
}

// pathChecked proves every run went event → route → Work → Execution: its
// trigger and inputs are accepted Events routed by the `jobs` route to the
// job's standing Work, and nothing else ran.
func (e *scEnv) pathChecked() {
	e.t.Helper()
	for _, r := range e.x.all() {
		e.read(func(tx storage.Tx) error {
			x, err := tx.Execution(r.Execution)
			if err != nil {
				return err
			}
			w, err := tx.Work(x.WorkID)
			if err != nil {
				return err
			}
			if w.Kind != sc.WorkKind || x.TriggerID != r.Trigger.ID {
				e.t.Errorf("run %s: work %s kind %s trigger %s/%s", r.Execution, w.ID, w.Kind, x.TriggerID, r.Trigger.ID)
			}
			return nil
		})
		for _, id := range append([]domain.ID{r.Trigger.ID}, r.Inputs...) {
			rec, err := e.router.Decision(context.Background(), id)
			if err != nil || rec.Policy != "queue" || e.routes == nil && rec.Route != "jobs" || !strings.HasPrefix(rec.Route, "jobs") {
				e.t.Errorf("run %s input %s: %+v %v", r.Execution, id, rec, err)
			}
		}
	}
}

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// TestScheduleSixHourCatchUp: the latest missed slot within six hours runs
// once; exactly six hours or more late is skipped (v1's exclusive boundary),
// across restarts, on the durable timer alone.
func TestScheduleSixHourCatchUp(t *testing.T) {
	e := newScheduleEnv(t, `[{"name":"report","cron":"0 9 * * *"}]`, utc(2026, 10, 21, 8, 0))
	if r := e.ensure(); r.Armed != 1 {
		t.Fatalf("ensure %+v", r)
	}
	if tm, _ := e.timer("report"); !tm.FireAt.Equal(utc(2026, 10, 21, 9, 0)) {
		t.Fatalf("first start waits for the next slot: %v", tm.FireAt)
	}
	e.at(utc(2026, 10, 21, 8, 59))
	e.pass()
	// Down from 08:59 until exactly six hours after the slot.
	e.at(utc(2026, 10, 21, 15, 0))
	e.reopen()
	if r := e.ensure(); r.Kept != 1 {
		t.Fatalf("restart rearmed: %+v", r)
	}
	e.pass()
	if got := e.slots("report"); len(got) != 0 {
		t.Fatalf("exactly six hours late ran %v", got)
	}
	if len(e.fired) != 1 || !e.fired[0].Skipped {
		t.Fatalf("firing %+v", e.fired)
	}
	if tm, _ := e.timer("report"); !tm.FireAt.Equal(utc(2026, 10, 22, 9, 0)) {
		t.Fatalf("rearmed %v", tm.FireAt)
	}
	// Down again; back one second inside six hours: the slot runs once.
	e.at(utc(2026, 10, 22, 14, 59).Add(59 * time.Second))
	e.reopen()
	e.pass()
	if got := e.slots("report"); len(got) != 1 || got[0] != "2026-10-22 09:00 UTC" {
		t.Fatalf("just inside: %v", got)
	}
	runs := e.runsOf("report")
	if len(runs) != 1 || label(runs[0].Trigger) != "cron 2026-10-22 09:00 UTC" {
		t.Fatalf("runs %+v", runs)
	}
	e.finish("report")
	if w, _ := e.work("report"); w.State != domain.WorkReady {
		t.Fatalf("a job's Work is standing: %s", w.State)
	}
	// Just outside the next day.
	e.at(utc(2026, 10, 23, 15, 1))
	e.reopen()
	e.pass()
	e.pass()
	if got := e.slots("report"); len(got) != 1 || len(e.runsOf("report")) != 1 {
		t.Fatalf("just outside ran: %v", got)
	}
	e.pathChecked()
}

// TestScheduleMissedSlotsAndRestart: several slots missed in one outage run
// once for the latest; a crash between the atomic fire and routing neither
// loses nor repeats the occurrence.
func TestScheduleMissedSlotsAndRestart(t *testing.T) {
	e := newScheduleEnv(t, `[{"name":"report","cron":"0 9,13,18 * * *"},{"name":"half","cron":"*/30 * * * *"}]`, utc(2026, 10, 21, 8, 50))
	e.ensure()
	e.at(utc(2026, 10, 21, 9, 0))
	e.pass()
	e.finish("report")
	e.finish("half")
	// Down 09:05 → 20:10: report missed 13:00 and 18:00, half missed many.
	e.at(utc(2026, 10, 21, 20, 10))
	e.reopen()
	poll, err := e.sched.Poll(context.Background(), e.timers, 100)
	if err != nil || len(poll.Timers) != 2 {
		t.Fatalf("poll %+v %v", poll, err)
	}
	// Crash after the firing committed and before anything was routed.
	e.reopen()
	e.pass()
	e.pass()
	if got := e.slots("report"); strings.Join(got, "|") != "2026-10-21 09:00 UTC|2026-10-21 18:00 UTC" {
		t.Fatalf("report slots %v", got)
	}
	if got := e.slots("half"); strings.Join(got, "|") != "2026-10-21 09:00 UTC|2026-10-21 20:00 UTC" {
		t.Fatalf("half slots %v", got)
	}
	if len(e.runsOf("report")) != 2 || len(e.runsOf("half")) != 2 {
		t.Fatalf("runs %+v", e.x.all())
	}
	if tm, _ := e.timer("report"); !tm.FireAt.Equal(utc(2026, 10, 22, 9, 0)) {
		t.Fatalf("report rearmed %v", tm.FireAt)
	}
	if tm, _ := e.timer("half"); !tm.FireAt.Equal(utc(2026, 10, 21, 20, 30)) {
		t.Fatalf("half rearmed %v", tm.FireAt)
	}
	e.pathChecked()
}

// TestScheduleClockChanges: on the night the clocks go back the repeated
// 01:30 runs once; on the night they go forward the skipped 01:30 has no run
// and the next night runs.
func TestScheduleClockChanges(t *testing.T) {
	e := newScheduleEnv(t, `[{"name":"night","cron":"30 1 * * *","time_zone":"Europe/London"}]`, utc(2026, 10, 24, 12, 0))
	e.ensure()
	for _, step := range []time.Time{
		utc(2026, 10, 25, 0, 30), // 01:30 BST
		utc(2026, 10, 25, 1, 30), // 01:30 GMT, the same name
		utc(2026, 10, 25, 2, 0),
		utc(2026, 10, 26, 1, 30),
	} {
		e.at(step)
		e.pass()
		for job := range map[string]bool{"night": true} {
			if _, ok := e.attempts[domain.ID(sc.WorkID(scNS, job))]; ok {
				e.finish(job)
			}
		}
	}
	if got := e.slots("night"); strings.Join(got, "|") != "2026-10-25 01:30 Europe/London|2026-10-26 01:30 Europe/London" {
		t.Fatalf("autumn %v", got)
	}
	s := newScheduleEnv(t, `[{"name":"night","cron":"30 1 * * *","time_zone":"Europe/London"}]`, utc(2026, 3, 28, 12, 0))
	s.ensure()
	for _, step := range []time.Time{utc(2026, 3, 29, 0, 30), utc(2026, 3, 29, 1, 0), utc(2026, 3, 29, 1, 30), utc(2026, 3, 29, 2, 0), utc(2026, 3, 30, 0, 30)} {
		s.at(step)
		s.pass()
		if _, ok := s.attempts[domain.ID(sc.WorkID(scNS, "night"))]; ok {
			s.finish("night")
		}
	}
	if got := s.slots("night"); strings.Join(got, "|") != "2026-03-30 01:30 Europe/London" {
		t.Fatalf("spring %v", got)
	}
	e.pathChecked()
	s.pathChecked()
}

// TestScheduleConfigEditsAndRemoval: reload rearms an edited timing (v1 kept
// the last slot across a timer restart), leaves other edits alone, cancels a
// removed job's timer while its running attempt carries on, ignores late
// triggers for it and keeps a disabled scope's timers durable.
func TestScheduleConfigEditsAndRemoval(t *testing.T) {
	jobs := `[{"name":"report","cron":"0 9 * * *"},{"name":"tick","every_minutes":60}]`
	e := newScheduleEnv(t, jobs, utc(2026, 10, 21, 10, 0))
	if r := e.ensure(); r.Armed != 2 {
		t.Fatalf("ensure %+v", r)
	}
	e.pass() // an interval job runs at once
	if len(e.runsOf("tick")) != 1 || len(e.runsOf("report")) != 0 {
		t.Fatalf("runs %+v", e.x.all())
	}
	e.finish("tick")
	// Edit report's timing to 08:00 at 10:00: today's 08:00 is within six
	// hours and is not the slot that ran last, so it runs now. Editing
	// tick's other triggers does not rearm it.
	if r := e.configure(`[{"name":"report","cron":"0 8 * * *"},{"name":"tick","every_minutes":60,"on":{"webhook":true}}]`, true); r.Rearmed != 1 || r.Kept != 1 {
		t.Fatalf("edit %+v", r)
	}
	e.pass()
	if got := e.slots("report"); len(got) != 1 || got[0] != "2026-10-21 08:00 UTC" {
		t.Fatalf("edited slots %v", got)
	}
	e.finish("report")
	// tick runs at 11:00; remove it while that run is going.
	e.at(utc(2026, 10, 21, 11, 0))
	e.pass()
	if len(e.runsOf("tick")) != 2 {
		t.Fatalf("tick runs %+v", e.runsOf("tick"))
	}
	if r := e.configure(`[{"name":"report","cron":"0 8 * * *"}]`, true); r.Cancelled != 1 {
		t.Fatalf("removal %+v", r)
	}
	if _, ok := e.timer("tick"); ok {
		t.Fatal("removed job kept its timer")
	}
	e.finish("tick") // a run going carries on
	// A late manual trigger for the removed job matches no route.
	ev := e.manual("tick", "late")
	e.pass()
	if rec, _ := e.router.Decision(context.Background(), ev); rec.Policy != "unmatched" {
		t.Fatalf("late trigger %+v", rec)
	}
	if len(e.runsOf("tick")) != 2 {
		t.Fatal("removed job ran")
	}
	// Disabling the scope keeps report's timer durable and untouched.
	before, _ := e.timer("report")
	if r := e.configure(`[{"name":"report","cron":"0 8 * * *"}]`, false); r.Kept != 1 || r.Cancelled != 0 {
		t.Fatalf("disable %+v", r)
	}
	if after, ok := e.timer("report"); !ok || after.Revision != before.Revision {
		t.Fatal("disabled scope's timer changed")
	}
	// Removing the entry (scope enabled) cancels it.
	e.entry, e.enabled = `{}`, true
	e.putScope()
	if r := e.ensure(); r.Cancelled != 1 {
		t.Fatalf("entry removal %+v", r)
	}
	e.pathChecked()
}

func (e *scEnv) manual(job, delivery string) domain.ID {
	e.t.Helper()
	ev := domain.Event{ID: domain.ID("manual-" + job + "-" + delivery), Type: "schedule.run", Source: runtime.ManualSource, DeliveryID: delivery,
		OccurredAt: e.clock.Now(), ReceivedAt: e.clock.Now(), CorrelationKey: &domain.CorrelationKey{Source: scNS, ExternalID: sc.JobKey(job)}}
	e.read(func(tx storage.Tx) error { _, _, err := runtime.AcceptInTx(tx, scScope, ev); return err })
	return ev.ID
}

func (e *scEnv) post(handler http.Handler, job, token, key string) int {
	r := httptest.NewRequest(http.MethodPost, "/"+job+"/"+token, strings.NewReader(`{"n":"`+key+`"}`))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w.Code
}

// TestScheduleTriggersCoalesce: webhook, manual and timer triggers reach the
// one job Work; while a run goes every later trigger waits and the next run
// takes them all (one pending run): the newest delivery wins, a timer never
// displaces a pending timer, a duplicate delivery is one Event, and a failed
// run arms no retry because the next trigger is the retry.
func TestScheduleTriggersCoalesce(t *testing.T) {
	e := newScheduleEnv(t, `[{"name":"post","at":["09:00","09:30","10:00"],"on":{"webhook":true}}]`, utc(2026, 10, 21, 8, 0))
	e.ensure()
	webhook := sc.WebhookHandler(scNS, func(context.Context) (string, error) { return scSecret, nil }, e.settings, func(ctx context.Context, d extension.Delivery) error {
		_, err := e.ingress.Push(ctx, scSource, d)
		return err
	}, e.clock.Now)
	token := sc.Token(scSecret, scNS, "post")
	if code := e.post(webhook, "post", "wrong", "k0"); code != http.StatusNotFound {
		t.Fatalf("bad token %d", code)
	}
	if code := e.post(webhook, "post", token, "k1"); code != http.StatusAccepted {
		t.Fatalf("webhook %d", code)
	}
	e.pass()
	if runs := e.runsOf("post"); len(runs) != 1 || label(runs[0].Trigger) != "webhook" {
		t.Fatalf("first run %+v", runs)
	}
	// While it runs: a duplicate of k1, two more deliveries, a timer slot and
	// a manual run.
	for _, k := range []string{"k1", "k2", "k3"} {
		if code := e.post(webhook, "post", token, k); code != http.StatusAccepted {
			t.Fatalf("webhook %s %d", k, code)
		}
	}
	e.at(utc(2026, 10, 21, 9, 0))
	e.manual("post", "hand-1")
	e.pass()
	e.pass()
	if len(e.runsOf("post")) != 1 {
		t.Fatal("a second run started while one was going")
	}
	pending, err := e.router.PendingInputs(context.Background(), domain.ID(sc.WorkID(scNS, "post")))
	if err != nil || len(pending) != 4 {
		t.Fatalf("pending %d %v (the duplicate k1 must be one Event)", len(pending), err)
	}
	e.finish("post")
	e.pass()
	runs := e.runsOf("post")
	if len(runs) != 2 || len(runs[1].Inputs) != 4 {
		t.Fatalf("one pending run takes the batch: %+v", runs)
	}
	if p, _ := sc.Payload(runs[1].Trigger); string(p.Body) != `{"n":"k3"}` {
		t.Fatalf("the newest delivery wins: %s", p.Body)
	}
	e.finish("post")
	e.pass()
	if len(e.runsOf("post")) != 2 {
		t.Fatal("ran again with nothing pending")
	}
	if w, _ := e.work("post"); w.State != domain.WorkReady {
		t.Fatalf("standing Work %s", w.State)
	}
	// Two timer slots while a manual run goes: the earliest stands.
	e.at(utc(2026, 10, 21, 9, 10))
	e.manual("post", "hand-2")
	e.pass()
	e.at(utc(2026, 10, 21, 9, 30))
	e.pass()
	e.at(utc(2026, 10, 21, 10, 0))
	e.pass()
	e.finish("post")
	e.pass()
	runs = e.runsOf("post")
	if len(runs) != 4 || label(runs[3].Trigger) != "at 2026-10-21 09:30 UTC" || len(runs[3].Inputs) != 2 {
		t.Fatalf("timer precedence %+v", runs[len(runs)-1])
	}
	e.finish("post")
	// A failed run arms no automatic retry; the next trigger runs.
	e.x.mu.Lock()
	e.x.outcome = domain.OutcomeFailed
	e.x.mu.Unlock()
	e.manual("post", "hand-3")
	e.pass()
	e.finish("post")
	e.at(utc(2026, 10, 21, 12, 0))
	e.pass()
	var timers []string
	e.read(func(tx storage.Tx) error {
		all, err := tx.ListTimers()
		for _, tm := range all {
			timers = append(timers, string(tm.ID))
		}
		return err
	})
	sort.Strings(timers)
	if len(timers) != 1 || timers[0] != sc.TimerID(scNS, "post") {
		t.Fatalf("timers after a failure %v", timers)
	}
	e.x.mu.Lock()
	e.x.outcome = ""
	e.x.mu.Unlock()
	e.manual("post", "hand-4")
	e.pass()
	if len(e.runsOf("post")) != 6 {
		t.Fatalf("the next trigger did not run after a failure: %d", len(e.runsOf("post")))
	}
	e.finish("post")
	e.pathChecked()
}

// TestScheduleWebhookOff: with no webhook listener at all, timed and manual
// runs are unaffected; the only declared loss is the one-shot webhook family.
func TestScheduleWebhookOff(t *testing.T) {
	e := newScheduleEnv(t, `[{"name":"tick","every_minutes":5,"on":{"webhook":true}}]`, utc(2026, 10, 21, 8, 0))
	e.ensure()
	e.pass()
	e.finish("tick")
	e.manual("tick", "hand")
	e.pass()
	e.finish("tick")
	e.at(utc(2026, 10, 21, 8, 5))
	e.pass()
	e.finish("tick")
	if n := len(e.runsOf("tick")); n != 3 {
		t.Fatalf("runs %d", n)
	}
	statuses, err := e.ingress.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Name != scSource {
			continue
		}
		for _, f := range s.Families {
			if f.Type != sc.TypeWebhook || f.ReDerivable || f.Loss == "" {
				t.Fatalf("family %+v", f)
			}
		}
	}
	e.pathChecked()
}

// TestScheduleRoutePerBinding: one `schedule` handler serves several routes,
// each selecting its jobs through its own route config, so each job runs its
// own binding (the router passes the evaluated route to the handler).
func TestScheduleRoutePerBinding(t *testing.T) {
	e := &scEnv{t: t, clock: &ghClock{t: utc(2026, 10, 21, 8, 0)}, path: filepath.Join(t.TempDir(), "conduit.db"), enabled: true, x: &jobExecutor{}, attempts: map[domain.ID]runtime.Attempt{},
		entry:    scEntry(`[{"name":"report"},{"name":"backup"}]`),
		bindings: []domain.Binding{{Name: "report-cmd", Executor: "fixture"}, {Name: "backup-cmd", Executor: "fixture"}},
		routes: []domain.Route{
			{Name: "jobs-report", Handler: sc.Type, Binding: "report-cmd", ActivePolicy: "queue", Config: domain.Payload{Data: []byte(`{"jobs":["report"]}`)}},
			{Name: "jobs-rest", Handler: sc.Type, Binding: "backup-cmd", ActivePolicy: "queue"},
		}}
	e.open()
	e.putScope()
	e.register()
	r := e.manual("report", "r1")
	b := e.manual("backup", "b1")
	e.pass()
	for ev, want := range map[domain.ID]string{r: "report-cmd", b: "backup-cmd"} {
		rec, err := e.router.Decision(context.Background(), ev)
		if err != nil || rec.Binding != want {
			t.Fatalf("%s routed %+v %v", ev, rec, err)
		}
	}
	for _, run := range e.x.all() {
		var x domain.Execution
		e.read(func(tx storage.Tx) error { var er error; x, er = tx.Execution(run.Execution); return er })
		if strings.Contains(string(x.WorkID), "/report") != (x.ExecutorBinding == "report-cmd") {
			t.Fatalf("execution %s bound %s", x.ID, x.ExecutorBinding)
		}
	}
	e.pathChecked()
}

// TestWorkKindPolicyContract: without the standing policy a job Work
// completes after its run (the default lifecycle), and a trigger selection
// outside the batch falls back to the first input.
func TestWorkKindPolicyContract(t *testing.T) {
	e := &scEnv{t: t, clock: &ghClock{t: utc(2026, 10, 21, 8, 0)}, path: filepath.Join(t.TempDir(), "conduit.db"), enabled: true, x: &jobExecutor{}, attempts: map[domain.ID]runtime.Attempt{},
		entry: scEntry(`[{"name":"post"}]`), policy: &runtime.WorkKindPolicy{Trigger: func([]domain.Event) int { return 99 }}}
	e.open()
	e.putScope()
	e.register()
	e.manual("post", "first")
	e.pass()
	e.manual("post", "second")
	e.manual("post", "third")
	e.route() // completion never overtakes accepted, unrouted input
	e.finish("post")
	e.pass()
	runs := e.runsOf("post")
	if len(runs) != 2 || runs[1].Trigger.ID != "manual-post-second" || len(runs[1].Inputs) != 2 {
		t.Fatalf("runs %+v", runs)
	}
	e.finish("post")
	if w, _ := e.work("post"); w.State != domain.WorkCompleted {
		t.Fatalf("default lifecycle %s", w.State)
	}
	// Terminal job Work takes no more triggers: a late one is audited.
	late := e.manual("post", "late")
	e.pass()
	if rec, _ := e.router.Decision(context.Background(), late); rec.Policy != "terminal" {
		t.Fatalf("late %+v", rec)
	}
}
