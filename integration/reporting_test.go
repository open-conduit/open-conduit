package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// Issue reporting through public extension hooks (03-08), through the
// production router, scheduler and toolbelt over SQLite, exactly as bootstrap
// composes them: an engineering execution reports a defect as a translated
// toolbelt emission; the user-bound `introspection` route gives each report
// Work of its own; triage reads and resolves reports as ordinary Work. Core
// chain-depth and per-chain rate limits stop report-about-a-report loops with
// no cooperation from the extension, and its operations exist only while it
// is enabled.

const (
	repScope  domain.ID = "eng"
	repSecond domain.ID = "ops"
	repBare   domain.ID = "bare"
)

type repEnv struct {
	t        *testing.T
	clock    *ghClock
	path     string
	limits   runtime.CausalLimits
	disabled bool // compose without registering the extension (entry removed)
	extra    *extension.Registration
	handlers map[string]extension.RouteHandler // route handlers besides introspection's
	// register, when set, adds a registration built over the open store.
	register func(storage.Store) extension.Registration

	store     *sqlite.Store
	ingress   *runtime.Ingress
	router    *runtime.Router
	belt      *runtime.Toolbelt
	scheduler *runtime.Scheduler
	control   *runtime.Control
	timers    *runtime.Timers
	exec      *schedulerExecutor

	mu     sync.Mutex
	tokens map[domain.ID]string
	grants map[string][]string
	seq    int
}

func newRepEnv(t *testing.T, limits runtime.CausalLimits) *repEnv {
	t.Helper()
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"), limits: limits,
		tokens: map[domain.ID]string{}, grants: map[string][]string{
			"agent":     {is.OpReport, is.OpCorrection, is.OpProblem, extension.ReadContext},
			"processor": {is.OpReport},
			"triage":    {is.OpList, is.OpResolve},
		}}
	e.open()
	e.putScope(repScope, true)
	e.putScope(repSecond, true)
	e.putScope(repBare, false)
	return e
}

func (e *repEnv) open() {
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
	e.exec = &schedulerExecutor{caps: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}
	e.exec.onRun = func(_ context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		e.mu.Lock()
		e.tokens[r.Execution.ID] = r.Context.ToolbeltToken
		e.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		return &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}, nil
	}
	if e.router, err = runtime.NewRouter(store, e.clock.Now, runtime.RouterOptions{Generation: "1", CausalLimits: e.limits,
		Handlers: e.routeHandlers(),
		Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: e.exec}}}); err != nil {
		t.Fatal(err)
	}
	var belt *runtime.Toolbelt
	belt, err = runtime.NewToolbelt(store, e.clock.Now, e.router, runtime.ToolbeltOptions{CausalLimits: e.limits,
		Grant: func(_ context.Context, s runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			// As bootstrap: only operations registered on this toolbelt.
			var ops []string
			for _, op := range e.grants[s.Binding.Name] {
				if slices.Contains(belt.Operations(), op) {
					ops = append(ops, op)
				}
			}
			return runtime.ToolbeltGrant{Operations: ops, EventTypes: belt.EventTypes()}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := is.NewTools(is.ToolsOptions{Store: store, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if !e.disabled {
		if err = belt.Register(tools.Registration()); err != nil {
			t.Fatal(err)
		}
	}
	if e.extra != nil {
		if err = belt.Register(*e.extra); err != nil {
			t.Fatal(err)
		}
	}
	if e.register != nil {
		if err = belt.Register(e.register(store)); err != nil {
			t.Fatal(err)
		}
	}
	e.belt = belt
	// As bootstrap: the standing sweep/triage policy and the extension's
	// calendar recurrence are generic host options.
	if e.scheduler, err = runtime.NewScheduler(store, e.clock.Now, e.router, runtime.SchedulerOptions{Owner: "test", LeaseDuration: time.Hour, Generation: "1",
		CausalLimits: e.limits, Executors: map[string]extension.Executor{"fixture": e.exec}, Toolbelt: belt, ToolbeltEndpoints: fixtureEndpoint("unix:///fixture.sock"),
		WorkKinds: map[string]runtime.WorkKindPolicy{is.WorkKindSweep: {Standing: true, RunOnce: true}, is.WorkKindTriage: {Standing: true, RunOnce: true}, is.WorkKindFix: {RunOnce: true}}}); err != nil {
		t.Fatal(err)
	}
	if e.timers, err = runtime.NewTimers(store, e.clock.Now, runtime.TimerOptions{CausalLimits: e.limits, Recurrences: map[string]runtime.RecurrenceFunc{is.RecurrenceKind: is.Recurrence}}); err != nil {
		t.Fatal(err)
	}
	if e.control, err = runtime.NewControl(store, e.clock.Now, e.router, e.scheduler, e.limits); err != nil {
		t.Fatal(err)
	}
}

// reopen simulates a daemon restart on the same database.
func (e *repEnv) reopen() {
	_ = e.store.Close()
	e.open()
}

func (e *repEnv) putScope(id domain.ID, enabled bool) {
	extensions := []map[string]any{}
	if enabled {
		extensions = append(extensions, map[string]any{"name": "reports", "type": is.Type, "config": map[string]any{}, "secret_names": []string{}})
	}
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": extensions})
	binding := func(name string) domain.Binding {
		return domain.Binding{Name: name, Executor: "fixture", Requires: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}}
	}
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		old, err := tx.Scope(id)
		var revision domain.Revision
		if err == nil {
			revision = old.Revision
		}
		return tx.PutScope(domain.Scope{ID: id, Name: string(id), Bindings: []domain.Binding{binding("agent"), binding("processor"), binding("triage")},
			Routes: []domain.Route{{Name: "reports", Handler: is.Type, Binding: "processor", ActivePolicy: "queue"}},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, revision)
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *repEnv) read(fn func(storage.Tx) error) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, fn); err != nil {
		e.t.Fatal(err)
	}
}

// manual creates ordinary manual Work bound to binding (how an operator runs
// the agent or triage by hand) and starts its attempt.
func (e *repEnv) manual(scope domain.ID, kind, binding string) runtime.Attempt {
	e.t.Helper()
	e.seq++
	w, _, err := e.control.CreateWork(context.Background(), runtime.ManualWork{ScopeID: scope, Kind: kind, Binding: binding, DeliveryID: "manual-" + strconv.Itoa(e.seq)})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.start(w.ID)
}

func (e *repEnv) start(work domain.ID) runtime.Attempt {
	e.t.Helper()
	a, err := e.scheduler.Start(context.Background(), work)
	if err != nil {
		e.t.Fatalf("start %s: %v", work, err)
	}
	return a
}

func (e *repEnv) finish(a runtime.Attempt) {
	e.t.Helper()
	res, err := a.Handle.Wait(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.scheduler.Complete(context.Background(), a.Lease, res); err != nil {
		e.t.Fatalf("complete %s: %v", a.Lease.ExecutionID, err)
	}
	e.route()
}

func (e *repEnv) route() {
	e.t.Helper()
	for {
		page, err := e.ingress.Pending(context.Background(), 0, 200)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(page.Items) == 0 {
			return
		}
		for _, p := range page.Items {
			if err := e.router.Route(context.Background(), p.Event.Event.ID); err != nil {
				e.t.Fatalf("route %s: %v", p.Event.Event.ID, err)
			}
		}
	}
}

func (e *repEnv) call(a runtime.Attempt, op, request string, input any) (extension.ToolResult, error) {
	e.mu.Lock()
	token := e.tokens[a.Lease.ExecutionID]
	e.mu.Unlock()
	data, _ := json.Marshal(input)
	return e.belt.CallRequest(context.Background(), token, request, extension.ToolCall{Operation: op, Input: domain.Payload{Data: data}})
}

func (e *repEnv) must(a runtime.Attempt, op, request string, input any) map[string]any {
	e.t.Helper()
	out, err := e.call(a, op, request, input)
	if err != nil {
		e.t.Fatalf("%s: %v", op, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Output.Data, &m); err != nil {
		e.t.Fatalf("%s output: %s", op, out.Output.Data)
	}
	return m
}

func (e *repEnv) reports(scope domain.ID) []is.Report {
	var out []is.Report
	e.read(func(tx storage.Tx) error { var err error; out, err = is.Reports(tx, scope); return err })
	return out
}

func (e *repEnv) event(id domain.ID) domain.AcceptedEvent {
	var a domain.AcceptedEvent
	e.read(func(tx storage.Tx) error { var err error; a, err = tx.Event(id); return err })
	return a
}

func (e *repEnv) countType(typ string) int {
	n := 0
	e.read(func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if a.Event.Type == typ {
				n++
			}
		}
		return err
	})
	return n
}

func (e *repEnv) work(id domain.ID) (domain.Work, bool) {
	var w domain.Work
	found := false
	e.read(func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(id)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return w, found
}

// chain walks causation from an Event to its root.
func (e *repEnv) chain(id domain.ID) []domain.Event {
	var out []domain.Event
	for id != "" {
		a := e.event(id)
		out = append(out, a.Event)
		id = a.Event.CausationID
	}
	return out
}

func defect(title string) map[string]any {
	return map[string]any{"kind": "runtime", "title": title, "body": "Tried os call; got 503 unavailable.\nWorkaround: none."}
}

func TestReportIssueFromAnEngineeringExecution(t *testing.T) {
	e := newRepEnv(t, runtime.CausalLimits{})
	a := e.manual(repScope, "engineering.issue", "agent")

	out := e.must(a, is.OpReport, "r1", defect("toolbelt call failed"))
	if out["reported"] == "" || out["event"] == "" || out["duplicate"] != false || out["depth"] != float64(1) {
		t.Fatalf("report output: %v", out)
	}
	// A retried call (same request) replays; the same input without a
	// request ID is one report too.
	if again := e.must(a, is.OpReport, "r1", defect("toolbelt call failed")); again["reported"] != out["reported"] {
		t.Fatalf("replay: %v", again)
	}
	first := e.must(a, is.OpReport, "", defect("second defect"))
	if dup := e.must(a, is.OpReport, "", defect("second defect")); dup["reported"] != first["reported"] || dup["duplicate"] != true {
		t.Fatalf("input dedup: %v / %v", first, dup)
	}
	if _, err := e.call(a, is.OpReport, "r1", defect("changed body")); !errors.Is(err, runtime.ErrRequestConflict) {
		t.Fatalf("reused request with another input: %v", err)
	}
	for _, bad := range []map[string]any{{"kind": "bogus", "title": "x", "body": "y"}, {"kind": "env", "title": "", "body": "y"}, {"kind": "env", "title": "a\nb", "body": "y"}, {"kind": "env", "title": "x", "body": ""}, {"kind": "env", "title": "x", "body": "y", "extra": 1}} {
		if _, err := e.call(a, is.OpReport, "", bad); err == nil {
			t.Fatalf("invalid report accepted: %v", bad)
		}
	}

	// The report is a durable derived Event of the execution's trigger,
	// addressed to its own key rather than to the reporting Work, with a
	// trusted source.
	ev := e.event(domain.ID(out["event"].(string)))
	x := a.Lease.ExecutionID
	if ev.Event.Type != is.TypeReported || ev.Event.Source != "execution/"+string(x) || ev.Event.WorkID != "" || ev.Event.CorrelationKey == nil || ev.Event.CorrelationKey.Source != is.SourceNamespace(repScope) {
		t.Fatalf("report event: %+v", ev.Event)
	}
	chain := e.chain(ev.Event.ID)
	root := chain[len(chain)-1]
	if len(chain) != 2 || root.Type != "core.work.created" || string(root.ID) != out["root"] {
		t.Fatalf("causal trace: %+v (root %v)", chain, out["root"])
	}

	reports := e.reports(repScope)
	if len(reports) != 2 {
		t.Fatalf("indexed reports: %+v", reports)
	}
	r := reports[0]
	if r.ID != out["reported"] || r.State != is.StateOpen || r.Execution != string(x) || r.Work != string(a.Request.Work.ID) || r.Trigger != string(root.ID) || r.Event != string(ev.Event.ID) || r.Depth != 1 || r.Kind != "runtime" {
		t.Fatalf("report record: %+v", r)
	}
	e.finish(a)

	// The user-bound route gives each report Work of its own, on the binding
	// configuration names; its trigger is the report.
	w, ok := e.work(is.WorkID(repScope, r.ID))
	if !ok || w.Kind != is.WorkKind || w.State != domain.WorkReady {
		t.Fatalf("processing Work: %+v %v", w, ok)
	}
	p := e.start(w.ID)
	var x2 domain.Execution
	e.read(func(tx storage.Tx) error { var err error; x2, err = tx.Execution(p.Lease.ExecutionID); return err })
	if x2.ExecutorBinding != "processor" || x2.TriggerID != ev.Event.ID {
		t.Fatalf("processing execution: %+v", x2)
	}
	if text, ok := is.RenderEvent(ev.Event); !ok || !strings.Contains(text, "toolbelt call failed") || !strings.Contains(text, is.OpResolve) {
		t.Fatalf("rendered: %q", text)
	}
	e.finish(p)

	// Triage runs by hand as ordinary Work: it lists and resolves.
	tr := e.manual(repScope, "manual.triage", "triage")
	list := e.must(tr, is.OpList, "", map[string]any{})
	if n := len(list["reports"].([]any)); n != 2 {
		t.Fatalf("open reports: %v", list)
	}
	res := e.must(tr, is.OpResolve, "", map[string]any{"id": r.ID, "summary": "Filed as #12", "link": "https://example.test/issues/12"})
	if res["resolved"] != r.ID {
		t.Fatalf("resolve: %v", res)
	}
	if open := e.must(tr, is.OpList, "", map[string]any{}); len(open["reports"].([]any)) != 1 {
		t.Fatalf("open after resolve: %v", open)
	}
	_, err := e.call(tr, is.OpResolve, "", map[string]any{"id": "nope", "summary": "x"})
	refused(t, err, "not_found")
	e.finish(tr)

	// Reports, their Events and the resolution survive a restart.
	e.reopen()
	after := e.reports(repScope)
	if len(after) != 2 || after[0].State != is.StateResolved || after[0].Resolution == nil || after[0].Resolution.Link == "" || after[1].State != is.StateOpen {
		t.Fatalf("after restart: %+v", after)
	}
	if got := e.event(ev.Event.ID); got.Event.CausationID != root.ID {
		t.Fatalf("event after restart: %+v", got.Event)
	}
	tr2 := e.manual(repScope, "manual.triage", "triage")
	if open := e.must(tr2, is.OpList, "", map[string]any{"state": "all"}); len(open["reports"].([]any)) != 2 {
		t.Fatalf("triage after restart: %v", open)
	}
}

func TestReportAboutAReportStopsAtTheChainDepth(t *testing.T) {
	const depth = 5
	e := newRepEnv(t, runtime.CausalLimits{MaxDepth: depth})
	a := e.manual(repScope, "engineering.issue", "agent")
	e.must(a, is.OpReport, "r", defect("first"))
	e.finish(a)

	// Each processing execution reports about its report (as a misbehaving
	// handler would); the extension does nothing to stop it.
	var refusal error
	for i := 0; i < 3*depth && refusal == nil; i++ {
		reports := e.reports(repScope)
		last := reports[len(reports)-1]
		p := e.start(is.WorkID(repScope, last.ID))
		_, refusal = e.call(p, is.OpReport, "loop", defect("report about "+last.ID))
		e.finish(p)
	}
	if !errors.Is(refusal, runtime.ErrCausalRejected) {
		t.Fatalf("the loop was not stopped: %v", refusal)
	}
	reports := e.reports(repScope)
	if len(reports) != depth || reports[len(reports)-1].Depth != depth {
		t.Fatalf("reports before the limit: %d (last depth %d)", len(reports), reports[len(reports)-1].Depth)
	}
	for i, r := range reports {
		if r.Depth != i+1 || r.Root != reports[0].Root {
			t.Fatalf("report %d chain position: %+v", i, r)
		}
	}
	causal, err := runtime.NewCausal(e.store, e.clock.Now, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	rejections, err := causal.Rejections(context.Background())
	if err != nil || len(rejections) != 1 || rejections[0].Reason != "depth_limit" || rejections[0].RootID != domain.ID(reports[0].Root) {
		t.Fatalf("rejection diagnostic: %+v %v", rejections, err)
	}
	// No processing Work exists beyond the limit.
	if n := e.countType(is.TypeReported); n != depth {
		t.Fatalf("report events: %d", n)
	}
}

func TestReportBurstHitsTheChainRate(t *testing.T) {
	const perMinute = 4
	e := newRepEnv(t, runtime.CausalLimits{PerMinute: perMinute})
	a := e.manual(repScope, "engineering.issue", "agent")
	accepted := 0
	var refusal error
	for i := 0; i < 2*perMinute; i++ {
		_, err := e.call(a, is.OpReport, "", defect("burst "+strconv.Itoa(i)))
		if err != nil {
			refusal = err
			break
		}
		accepted++
	}
	if !errors.Is(refusal, runtime.ErrCausalRejected) || accepted == 0 || accepted > perMinute {
		t.Fatalf("burst: accepted %d, refusal %v", accepted, refusal)
	}
	if n := len(e.reports(repScope)); n != accepted {
		t.Fatalf("a refused report left a record: %d records for %d accepted", n, accepted)
	}
	// The next minute restores rate capacity on the same chain.
	e.clock.Add(time.Minute)
	if _, err := e.call(a, is.OpReport, "", defect("next minute")); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestReportingPermissionBoundaries(t *testing.T) {
	e := newRepEnv(t, runtime.CausalLimits{})
	a := e.manual(repScope, "engineering.issue", "agent")
	out := e.must(a, is.OpReport, "r", defect("seen in eng"))
	e.finish(a)

	// Report-processing work reports only when its binding grants it.
	e.mu.Lock()
	e.grants["processor"] = nil
	e.mu.Unlock()
	p := e.start(is.WorkID(repScope, out["reported"].(string)))
	if _, err := e.call(p, is.OpReport, "loop", defect("about a report")); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("ungranted report from processing work: %v", err)
	}
	// Nor can it read or resolve reports (triage's grant), or emit the
	// family with an operation it was not given.
	if _, err := e.call(p, is.OpResolve, "", map[string]any{"id": out["reported"], "summary": "x"}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("ungranted resolve: %v", err)
	}
	e.finish(p)

	// A scope without an entry cannot report, and a triage in another
	// scope sees none of this scope's reports.
	bare := e.manual(repBare, "engineering.issue", "agent")
	_, err := e.call(bare, is.OpReport, "", defect("no entry"))
	refused(t, err, "not_enabled")
	e.finish(bare)
	other := e.manual(repSecond, "manual.triage", "triage")
	if list := e.must(other, is.OpList, "", map[string]any{"state": "all"}); len(list["reports"].([]any)) != 0 {
		t.Fatalf("cross-scope read: %v", list)
	}
	_, err = e.call(other, is.OpResolve, "", map[string]any{"id": out["reported"], "summary": "x"})
	refused(t, err, "not_found")
	e.finish(other)
	if r := e.reports(repScope); len(r) != 1 || r[0].State != is.StateOpen {
		t.Fatalf("reports changed across scopes: %+v", r)
	}

	// Removing the entry stops the route even for a report in flight.
	a2 := e.manual(repScope, "engineering.issue", "agent")
	second := e.must(a2, is.OpReport, "r2", defect("in flight"))
	e.putScope(repScope, false)
	e.finish(a2)
	if _, ok := e.work(is.WorkID(repScope, second["reported"].(string))); ok {
		t.Fatal("a report was processed after the entry was removed")
	}
}

// An agent logs corrections and problems as introspection's own notes,
// keyed by its Work's correlation key: once per request, validated, and
// refused in a scope without an entry. Sweep and triage evidence reads them
// beside the legacy GitHub notes.
func TestAgentNotes(t *testing.T) {
	e := newRepEnv(t, runtime.CausalLimits{})
	key := domain.CorrelationKey{Source: "tracker/eng", ExternalID: "issue/ENG-7"}
	w, _, err := e.control.CreateWork(context.Background(), runtime.ManualWork{ScopeID: repScope, Kind: "engineering.issue", Binding: "agent", DeliveryID: "notes-1", Keys: []domain.CorrelationKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	// The Work's source describes it (who asked, its ref and link); each
	// note records that and the logging execution's trigger as its origin.
	shown := &domain.WorkDisplay{Title: "Fix the widget", Ref: "ENG-7", Requester: "outsider", Links: []domain.Link{{Label: "ENG-7", URL: "https://tracker.example/ENG-7"}}}
	e.read(func(tx storage.Tx) error {
		cur, err := tx.Work(w.ID)
		if err != nil {
			return err
		}
		cur.Display = shown
		return tx.PutWork(cur, cur.Revision)
	})
	a := e.start(w.ID)
	c := e.must(a, is.OpCorrection, "c1", map[string]any{"area": "writing", "body": "Summarised the page in the lede.\n\nSay what the page is for."})
	if again := e.must(a, is.OpCorrection, "c1", map[string]any{"area": "writing", "body": "Summarised the page in the lede.\n\nSay what the page is for."}); again["logged"] != c["logged"] {
		t.Fatalf("retried correction: %v / %v", c, again)
	}
	e.must(a, is.OpProblem, "p1", map[string]any{"kind": "tooling", "body": "pr list failed\n\nError: x\nWorkaround: none\nFix: the tool"})
	for _, bad := range []struct {
		op    string
		input map[string]any
	}{{is.OpProblem, map[string]any{"kind": "bogus", "body": "x"}}, {is.OpCorrection, map[string]any{"area": "writing", "body": " "}}, {is.OpCorrection, map[string]any{"body": "x", "issue": "7"}}} {
		if _, err := e.call(a, bad.op, "", bad.input); err == nil {
			t.Fatalf("invalid note accepted: %s %v", bad.op, bad.input)
		}
	}
	e.read(func(tx storage.Tx) error {
		notes, err := is.Notes(tx, []string{is.NotesNamespace}, repScope, is.NoteCorrection, true)
		if err != nil || len(notes) != 1 || notes[0].Key == nil || *notes[0].Key != key || notes[0].Area != "writing" || notes[0].Work != string(w.ID) {
			t.Fatalf("corrections: %+v %v", notes, err)
		}
		x, err := tx.Execution(a.Lease.ExecutionID)
		if err != nil {
			return err
		}
		trigger, err := tx.Event(x.TriggerID)
		if err != nil {
			return err
		}
		want := is.Origin{Trigger: string(x.TriggerID), TriggerType: trigger.Event.Type, TriggerSource: trigger.Event.Source,
			Title: shown.Title, Ref: shown.Ref, Link: shown.Links[0].URL, Requester: shown.Requester}
		if o := notes[0].Origin; o == nil || *o != want || want.Trigger == "" || want.TriggerType == "" {
			t.Fatalf("correction origin: %+v, want %+v", o, want)
		}
		// The sweep's evidence lists each correction with its origin, and a
		// sweep with no land setting opens a PR rather than pushing.
		ev, err := is.Aggregate(tx, is.Config{}, repScope, is.WorkSweep)
		if err != nil {
			return err
		}
		if ev.Settings.Land != is.LandPR || ev.Corrections == nil || len(ev.Corrections.Groups) != 1 {
			t.Fatalf("sweep evidence: %+v", ev)
		}
		if p := ev.Corrections.Groups[0].Provenance; len(p) != 1 || p[0].ID != notes[0].ID || p[0].Work != string(w.ID) || p[0].Origin == nil || *p[0].Origin != want {
			t.Fatalf("sweep provenance: %+v", p)
		}
		if tr, err := is.Aggregate(tx, is.Config{}, repScope, is.WorkTriage); err != nil || tr.Settings.Land != is.LandPR || tr.Problems == nil || len(tr.Problems.Groups[0].Provenance) != 1 {
			t.Fatalf("triage evidence: %+v %v", tr, err)
		}
		counts, err := is.WorkNotes(tx, w.ID)
		if err != nil || counts[is.NoteCorrection] != 1 || counts[is.NoteProblem] != 1 {
			t.Fatalf("work notes: %v %v", counts, err)
		}
		return nil
	})
	e.finish(a)

	bare := e.manual(repBare, "engineering.issue", "agent")
	_, err = e.call(bare, is.OpCorrection, "", map[string]any{"area": "code", "body": "x"})
	refused(t, err, "not_enabled")
	e.finish(bare)
}

// TestTranslatedEmitContract exercises the generic contract with a rogue
// registration: a translated emission gets no authority beyond the builtin
// emit and its own namespaces, and a refused plan leaves no effect.
func TestTranslatedEmitContract(t *testing.T) {
	var plan extension.EmitPlan
	rogue := extension.Registration{Version: extension.ContractVersion, Name: "rogue", EventTypes: []string{"rogue.noted"},
		Operations: []extension.Operation{{Name: "rogue.note", Emit: func(context.Context, extension.Authority, domain.Payload) (extension.EmitPlan, error) {
			return plan, nil
		}}}}
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"), extra: &rogue,
		tokens: map[domain.ID]string{}, grants: map[string][]string{"agent": {"rogue.note"}}}
	e.open()
	e.putScope(repScope, true)
	a := e.manual(repScope, "engineering.issue", "agent")
	commit := func(records ...extension.Record) func(extension.Emitted) (extension.EmitCommit, error) {
		return func(extension.Emitted) (extension.EmitCommit, error) {
			return extension.EmitCommit{Records: records}, nil
		}
	}
	foreignKey := is.ReportKey(repScope, "x")
	for name, c := range map[string]struct {
		plan extension.EmitPlan
		want error
	}{
		"another registration's family": {extension.EmitPlan{Event: domain.Event{Type: is.TypeReported, DeliveryID: "d1"}, Commit: commit()}, extension.ErrUndeclared},
		"core family":                   {extension.EmitPlan{Event: domain.Event{Type: "core.work.created", DeliveryID: "d2"}, Commit: commit()}, extension.ErrUndeclared},
		"forged source":                 {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "d3", Source: "github"}, Commit: commit()}, extension.ErrUnauthorized},
		"forged parent":                 {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "d4", CausationID: "root"}, Commit: commit()}, extension.ErrUnauthorized},
		"another Work as target":        {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "d5", WorkID: "w"}, Commit: commit()}, extension.ErrUnauthorized},
		"foreign key namespace":         {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "d6", CorrelationKey: &foreignKey}, Commit: commit()}, extension.ErrUnauthorized},
		"foreign KV namespace":          {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "d7"}, Commit: commit(extension.Record{Namespace: "core.toolbelt.tokens", Key: "k", Value: []byte("x")})}, extension.ErrUnauthorized},
		"prefix lookalike namespace":    {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "d8"}, Commit: commit(extension.Record{Namespace: "roguery", Key: "k", Value: []byte("x")})}, extension.ErrUnauthorized},
		"no delivery identity":          {extension.EmitPlan{Event: domain.Event{Type: "rogue.noted"}, Commit: commit()}, extension.ErrInvalid},
	} {
		plan = c.plan
		if _, err := e.call(a, "rogue.note", "", map[string]any{}); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	if n := e.countType("rogue.noted"); n != 0 {
		t.Fatalf("a refused plan accepted %d Events", n)
	}
	e.read(func(tx storage.Tx) error {
		if _, err := tx.Get("core.toolbelt.tokens", "k"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("foreign record written: %v", err)
		}
		return nil
	})
	// A well-formed plan: own key namespace, own records, trusted identity.
	own := domain.CorrelationKey{Source: "rogue/notes", ExternalID: "n1"}
	plan = extension.EmitPlan{Event: domain.Event{Type: "rogue.noted", DeliveryID: "ok", CorrelationKey: &own}, Commit: commit(extension.Record{Namespace: "rogue.notes", Key: "n1", Value: []byte("v")})}
	out, err := e.call(a, "rogue.note", "", map[string]any{})
	if err != nil || out.Event == nil || out.Event.Event.Source != "execution/"+string(a.Lease.ExecutionID) || out.Event.Event.CausationID == "" || out.Event.Event.WorkID != "" {
		t.Fatalf("own plan: %+v %v", out.Event, err)
	}
	e.read(func(tx storage.Tx) error {
		v, err := tx.Get("rogue.notes", "n1")
		if err != nil || string(v) != "v" {
			t.Fatalf("own record: %q %v", v, err)
		}
		return nil
	})
}

// TestDisabledReportingIsUndeclared composes the toolbelt the way bootstrap
// does for a generation without an `introspection` entry.
// TestTranslatedEmitOnce: EmitPlan.Once is an emission's identity across
// executions. A repeat with the same Event is the first again and spends no
// budget; a different Event under it is refused; it is per scope and bounded.
func TestTranslatedEmitOnce(t *testing.T) {
	var plan extension.EmitPlan
	rogue := extension.Registration{Version: extension.ContractVersion, Name: "rogue", EventTypes: []string{"rogue.noted"},
		Operations: []extension.Operation{{Name: "rogue.note", Emit: func(context.Context, extension.Authority, domain.Payload) (extension.EmitPlan, error) {
			return plan, nil
		}}}}
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"), extra: &rogue,
		tokens: map[domain.ID]string{}, grants: map[string][]string{"agent": {"rogue.note"}}}
	e.open()
	e.putScope(repScope, true)
	e.putScope(repSecond, true)
	key := domain.CorrelationKey{Source: "rogue/notes", ExternalID: "n1"}
	note := func(delivery, once, text string) extension.EmitPlan {
		return extension.EmitPlan{Once: once, Event: domain.Event{Type: "rogue.noted", DeliveryID: delivery, CorrelationKey: &key,
			Data: domain.Payload{Schema: "rogue.v1", Data: []byte(`{"text":"` + text + `"}`)}},
			Commit: func(m extension.Emitted) (extension.EmitCommit, error) {
				return extension.EmitCommit{Output: domain.Payload{Schema: "rogue.out.v1", Data: []byte(fmt.Sprintf(`{"added":%t}`, m.Added))}}, nil
			}}
	}
	run1 := e.manual(repScope, "engineering.issue", "agent")
	plan = note("d1", "item-1", "a")
	first, err := e.call(run1, "rogue.note", "", map[string]any{})
	if err != nil || string(first.Output.Data) != `{"added":true}` {
		t.Fatalf("first: %s %v", first.Output.Data, err)
	}
	e.finish(run1)

	// A later execution, another delivery ID: the same Event again.
	run2 := e.manual(repScope, "engineering.issue", "agent")
	plan = note("d2", "item-1", "a")
	again, err := e.call(run2, "rogue.note", "", map[string]any{})
	if err != nil || string(again.Output.Data) != `{"added":false}` || again.Event.Event.ID != first.Event.Event.ID {
		t.Fatalf("repeat: %s %v %v", again.Output.Data, again.Event, err)
	}
	// The same Once with other content is refused.
	plan = note("d3", "item-1", "b")
	var oe *extension.OperationError
	if _, err := e.call(run2, "rogue.note", "", map[string]any{}); !errors.As(err, &oe) || oe.Code != "once_conflict" {
		t.Fatalf("conflict: %v", err)
	}
	// Another Once with that content is a new Event.
	plan = note("d4", "item-1/r2", "b")
	if out, err := e.call(run2, "rogue.note", "", map[string]any{}); err != nil || string(out.Output.Data) != `{"added":true}` {
		t.Fatalf("new revision: %s %v", out.Output.Data, err)
	}
	plan = note("d5", strings.Repeat("x", extension.MaxOnce+1), "a")
	if _, err := e.call(run2, "rogue.note", "", map[string]any{}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("oversized Once: %v", err)
	}
	e.finish(run2)
	if n := e.countType("rogue.noted"); n != 2 {
		t.Fatalf("accepted %d, want 2", n)
	}

	// Once is per scope.
	other := e.manual(repSecond, "engineering.issue", "agent")
	plan = note("d1", "item-1", "a")
	if out, err := e.call(other, "rogue.note", "", map[string]any{}); err != nil || string(out.Output.Data) != `{"added":true}` {
		t.Fatalf("other scope: %s %v", out.Output.Data, err)
	}
	e.finish(other)
}

func TestDisabledReportingIsUndeclared(t *testing.T) {
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"), disabled: true,
		tokens: map[domain.ID]string{}, grants: map[string][]string{"agent": {is.OpReport}}}
	e.open()
	e.putScope(repScope, false)
	a := e.manual(repScope, "engineering.issue", "agent")
	if _, err := e.call(a, is.OpReport, "", defect("x")); !errors.Is(err, extension.ErrUndeclared) {
		t.Fatalf("disabled report: %v", err)
	}
	for _, typ := range e.belt.EventTypes() {
		if typ == is.TypeReported {
			t.Fatal("the report family is declared without the extension")
		}
	}
	e.finish(a)
}

func (e *repEnv) routeHandlers() map[string]extension.RouteHandler {
	h := map[string]extension.RouteHandler{is.Type: is.Route{}}
	for k, v := range e.handlers {
		h[k] = v
	}
	return h
}
