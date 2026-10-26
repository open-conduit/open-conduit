package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// The complete introspection workload (05-03) through the production router,
// scheduler, toolbelt and timers over SQLite, composed as bootstrap composes
// it: the extension's own calendar timers start each scope's standing sweep
// and triage Work on bindings the user chose; the triage aggregates reports
// and the GitHub tools' problem notes as evidence and requests fix Work whose
// Event cites the report it fixes, so the fix traces to the report and its
// origin through causation; the sweep folds corrections; core limits bound
// cited causation as they bound everything else; and removing the entry
// retires the extension's Work under an explicit policy.

func introspectionGrants() map[string][]string {
	return map[string][]string{
		"agent":     {is.OpReport, extension.ReadContext},
		"processor": {is.OpReport, is.OpFix},
		"triager":   {is.OpEvidence, is.OpList, is.OpFix, is.OpResolve, is.OpNotesResolve},
		"sweeper":   {is.OpEvidence, is.OpNotesResolve},
		"fixer":     {is.OpList, is.OpResolve},
		"triage":    {is.OpList, is.OpResolve},
	}
}

// putIntrospectionScope writes a scope whose `introspection` entry has cfg
// (nil: no entry), with one route per extension Work kind.
func (e *repEnv) putIntrospectionScope(id domain.ID, cfg map[string]any) {
	e.t.Helper()
	extensions := []map[string]any{}
	if cfg != nil {
		extensions = append(extensions, map[string]any{"name": "introspection", "type": is.Type, "config": cfg, "secret_names": []string{}})
	}
	meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": extensions})
	var bindings []domain.Binding
	for _, name := range []string{"agent", "processor", "triager", "sweeper", "fixer", "triage"} {
		bindings = append(bindings, domain.Binding{Name: name, Executor: "fixture", Requires: domain.Capabilities{Toolbelt: extension.ToolbeltCLI}})
	}
	route := func(name, binding, work string) domain.Route {
		return domain.Route{Name: name, Handler: is.Type, Binding: binding, ActivePolicy: "queue", Config: domain.Payload{Schema: "core.config.route.v1", Data: []byte(`{"work":"` + work + `"}`)}}
	}
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		old, err := tx.Scope(id)
		var revision domain.Revision
		if err == nil {
			revision = old.Revision
		}
		return tx.PutScope(domain.Scope{ID: id, Name: string(id), Bindings: bindings,
			Routes: []domain.Route{route("reports", "processor", is.WorkReport), route("sweep", "sweeper", is.WorkSweep), route("triage", "triager", is.WorkTriage), route("fixes", "fixer", is.WorkFix)},
			Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, revision)
	}); err != nil {
		e.t.Fatal(err)
	}
}

func newIntrospectionEnv(t *testing.T, limits runtime.CausalLimits, cfg map[string]any) *repEnv {
	t.Helper()
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"), limits: limits,
		tokens: map[domain.ID]string{}, grants: introspectionGrants()}
	e.open()
	e.putIntrospectionScope(repScope, cfg)
	e.putIntrospectionScope(repSecond, map[string]any{})
	return e
}

// ensure reconciles the timers as bootstrap's apply pass does.
func (e *repEnv) ensure(entries ...is.Entry) is.EnsureResult {
	e.t.Helper()
	res, err := is.Ensure(context.Background(), e.store, e.clock.Now(), e.timers, entries)
	if err != nil || len(res.Codes) > 0 {
		e.t.Fatalf("ensure: %+v %v", res, err)
	}
	return res
}

// fire fires every due timer and routes what they published.
func (e *repEnv) fire() []runtime.TimerFire {
	e.t.Helper()
	fired, err := e.timers.Due(context.Background(), 100)
	if err != nil {
		e.t.Fatal(err)
	}
	e.route()
	return fired
}

func (e *repEnv) execution(id domain.ID) domain.Execution {
	var x domain.Execution
	e.read(func(tx storage.Tx) error { var err error; x, err = tx.Execution(id); return err })
	return x
}

// standing finds a scope's standing run Work of kind.
func (e *repEnv) standing(scope domain.ID, kind string) []domain.Work {
	var out []domain.Work
	e.read(func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		for _, w := range works {
			if w.ScopeID == scope && w.Kind == kind {
				out = append(out, w)
			}
		}
		return err
	})
	return out
}

// putNote writes an agent's note exactly as the GitHub tools do (the data
// contract the extension reads through public storage).
func (e *repEnv) putNote(kind, area string, scope domain.ID, work, body string, issue int) string {
	e.t.Helper()
	e.seq++
	at := e.clock.Now().Add(time.Duration(e.seq) * time.Millisecond).UTC()
	key := kind + "/" + at.Format("20060102T150405.000Z") + "/" + strings.Repeat("a", 11) + string(rune('a'+e.seq%26))
	raw, _ := json.Marshal(gh.Note{Kind: kind, Area: area, Scope: string(scope), Work: work, Execution: "x-" + work, Issue: issue, Body: body, At: at})
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		return tx.Put(gh.NotesNamespace, key, raw)
	}); err != nil {
		e.t.Fatal(err)
	}
	return gh.NotesNamespace + ":" + key
}

func decode[T any](t *testing.T, m map[string]any) T {
	t.Helper()
	var out T
	raw, _ := json.Marshal(m)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// TestWorkflowDReportTriageFix: a report → scheduled triage → fix chain,
// traced through causation from the fix's execution back to the
// engineering execution's origin.
func TestWorkflowDReportTriageFix(t *testing.T) {
	ctx := context.Background()
	cfg := map[string]any{"sweep": map[string]any{"cron": "41 8 * * *", "rules": []string{"CLAUDE.md"}}, "triage": map[string]any{"cron": "51 8 * * *", "land": "pr"}}
	e := newIntrospectionEnv(t, runtime.CausalLimits{}, cfg)
	parsed, err := is.ParseConfig(rawJSON(cfg))
	if err != nil {
		t.Fatal(err)
	}
	entries := []is.Entry{{Scope: repScope, Enabled: true, Config: parsed}, {Scope: repSecond, Enabled: true}}

	// New schedules wait for their next slot (09:00 is past today's).
	if res := e.ensure(entries...); res.Armed != 2 {
		t.Fatalf("armed: %+v", res)
	}
	if res := e.ensure(entries...); res.Kept != 2 || res.Armed+res.Rearmed != 0 {
		t.Fatalf("second pass: %+v", res)
	}
	if fired := e.fire(); len(fired) != 0 {
		t.Fatalf("fired early: %+v", fired)
	}

	// An engineering execution reports the same defect twice and another
	// once; each report is processed on its own Work (which does nothing).
	a := e.manual(repScope, "engineering.issue", "agent")
	r1 := e.must(a, is.OpReport, "r1", defect("os call failed with 503"))
	e.clock.Add(time.Second)
	r2 := e.must(a, is.OpReport, "r2", map[string]any{"kind": "runtime", "title": "OS call failed with   503", "body": "again"})
	r3 := e.must(a, is.OpReport, "r3", map[string]any{"kind": "docs", "title": "setup guide names a moved script", "body": "scripts/setup.sh is gone"})
	engineering := a.Request.Work.ID
	e.finish(a)
	for _, r := range []map[string]any{r1, r2, r3} {
		e.finish(e.start(is.WorkID(repScope, r["reported"].(string))))
	}
	// Problems and corrections logged by GitHub issue agents.
	p1 := e.putNote(is.NoteProblem, "env", repScope, "gh/12", "SUPERVISOR_SCRIPTS is unset\nworked around", 12)
	p2 := e.putNote(is.NoteProblem, "env", repScope, "gh/13", "SUPERVISOR_SCRIPTS is unset", 13)
	c1 := e.putNote(is.NoteCorrection, "writing", repScope, "gh/12", "Keep PR titles under 60 characters", 12)
	c2 := e.putNote(is.NoteCorrection, "code", repScope, "gh/14", "Run go vet before pushing", 14)
	e.putNote(is.NoteProblem, "env", repSecond, "gh/1", "another scope's problem", 1)

	// The next morning both slots fire and start the standing runs.
	e.clock.Add(24*time.Hour - 9*time.Minute) // 2026-10-27 08:51
	if fired := e.fire(); len(fired) != 2 {
		t.Fatalf("slots fired: %+v", fired)
	}
	triages := e.standing(repScope, is.WorkKindTriage)
	sweeps := e.standing(repScope, is.WorkKindSweep)
	if len(triages) != 1 || len(sweeps) != 1 || !strings.HasPrefix(string(triages[0].ID), "introspection/eng/triage/") {
		t.Fatalf("standing runs: %+v %+v", triages, sweeps)
	}
	triage := triages[0]
	tr := e.start(triage.ID)
	trigger := e.event(e.execution(tr.Lease.ExecutionID).TriggerID)
	if trigger.Event.Type != is.TypeTriageDue || trigger.Event.CausationID != "" || e.execution(tr.Lease.ExecutionID).ExecutorBinding != "triager" {
		t.Fatalf("triage trigger: %+v", trigger.Event)
	}
	if text, ok := is.RenderEvent(trigger.Event); !ok || !strings.Contains(text, is.OpEvidence) {
		t.Fatalf("slot render: %q", text)
	}

	// Evidence: the open reports grouped by cause, the scope's problems,
	// and the run's settings.
	ev := decode[is.Evidence](t, e.must(tr, is.OpEvidence, "", map[string]any{"for": "triage"}))
	if ev.Reports == nil || ev.Reports.Count != 3 || len(ev.Reports.Groups) != 2 || ev.Reports.Groups[0].Count != 2 || ev.Reports.Groups[0].Works[0] != string(engineering) {
		t.Fatalf("report evidence: %+v", ev.Reports)
	}
	if ev.Problems == nil || ev.Problems.Count != 2 || len(ev.Problems.Groups) != 1 || len(ev.Problems.Groups[0].Issues) != 2 || ev.Settings.Land != is.LandPR || ev.Settings.Notes[0] != gh.NotesNamespace {
		t.Fatalf("problem evidence: %+v %+v", ev.Problems, ev.Settings)
	}
	_, err = e.call(tr, is.OpEvidence, "", map[string]any{"for": "jobs"})
	refused(t, err, "invalid_input")

	// The triage requests one fix for the shared cause; its Event cites the
	// earliest report as its cause.
	fixOut := e.must(tr, is.OpFix, "fix-1", map[string]any{"reports": []any{r2["reported"], r1["reported"]}, "notes": []any{p1, p2},
		"title": "Toolbelt calls fail with 503", "body": "Restart the endpoint wiring in scripts/run.sh."})
	if fixOut["cause"] != r1["event"] || fixOut["depth"] != float64(2) || fixOut["root"] != r1["root"] {
		t.Fatalf("fix: %v (report %v)", fixOut, r1)
	}
	if again := e.must(tr, is.OpFix, "fix-1", map[string]any{"reports": []any{r2["reported"], r1["reported"]}, "notes": []any{p1, p2},
		"title": "Toolbelt calls fail with 503", "body": "Restart the endpoint wiring in scripts/run.sh."}); again["fix"] != fixOut["fix"] {
		t.Fatalf("fix replay: %v", again)
	}
	for name, in := range map[string]map[string]any{
		"unknown report": {"reports": []any{"nope"}, "title": "t", "body": "b"},
		"unknown note":   {"notes": []any{"github.notes:problem/x"}, "title": "t", "body": "b"},
		"nothing named":  {"title": "t", "body": "b"},
	} {
		if _, err := e.call(tr, is.OpFix, "", in); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if res := e.must(tr, is.OpNotesResolve, "", map[string]any{"notes": []any{p1, p2}, "summary": "fix requested"}); res["handled"] != float64(2) {
		t.Fatalf("notes resolve: %v", res)
	}
	_, err = e.call(tr, is.OpNotesResolve, "", map[string]any{"notes": []any{"github.notes:problem/none"}, "summary": "x"})
	refused(t, err, "not_found")
	e.finish(tr)
	if w, _ := e.work(triage.ID); w.State != domain.WorkReady {
		t.Fatalf("standing triage after its run: %s", w.State)
	}

	// The fix is its own Work on the fix binding; its trigger is the fix
	// Event, which traces through the report to the engineering origin.
	fixWork, ok := e.work(domain.ID(fixOut["work"].(string)))
	if !ok || fixWork.Kind != is.WorkKindFix {
		t.Fatalf("fix Work: %+v %v", fixWork, ok)
	}
	fx := e.start(fixWork.ID)
	fixExec := e.execution(fx.Lease.ExecutionID)
	chain := e.chain(fixExec.TriggerID)
	if fixExec.ExecutorBinding != "fixer" || len(chain) != 3 || chain[0].Type != is.TypeFixRequested || chain[1].ID != domain.ID(r1["event"].(string)) || chain[2].Type != "core.work.created" || chain[2].ID != domain.ID(r1["root"].(string)) {
		t.Fatalf("fix trace: %+v", chain)
	}
	if chain[0].Source != "execution/"+string(tr.Lease.ExecutionID) {
		t.Fatalf("the fix Event does not name the triage execution: %s", chain[0].Source)
	}
	if text, ok := is.RenderEvent(chain[0]); !ok || !strings.Contains(text, "Toolbelt calls fail") || !strings.Contains(text, string(chain[1].ID)) {
		t.Fatalf("fix render: %q", text)
	}
	fixing := e.must(fx, is.OpList, "", map[string]any{"state": "fixing"})
	if n := len(fixing["reports"].([]any)); n != 2 {
		t.Fatalf("fixing reports: %v", fixing)
	}
	for _, r := range []map[string]any{r1, r2} {
		e.must(fx, is.OpResolve, "", map[string]any{"id": r["reported"], "summary": "fixed", "link": "https://example.test/pull/7"})
	}
	e.finish(fx)
	var fixes []is.Fix
	e.read(func(tx storage.Tx) error { var err error; fixes, err = is.Fixes(tx, repScope); return err })
	if len(fixes) != 1 || fixes[0].Cause != r1["event"] || fixes[0].Execution != string(tr.Lease.ExecutionID) || len(fixes[0].Notes) != 2 {
		t.Fatalf("fix record: %+v", fixes)
	}
	reports := e.reports(repScope)
	if reports[0].State != is.StateResolved || reports[0].Fix != fixes[0].ID || reports[2].State != is.StateOpen {
		t.Fatalf("report states: %+v", reports)
	}

	// The sweep folds the corrections.
	sw := e.start(sweeps[0].ID)
	sev := decode[is.Evidence](t, e.must(sw, is.OpEvidence, "", map[string]any{"for": "sweep"}))
	if sev.Corrections == nil || sev.Corrections.Count != 2 || sev.Reports != nil || len(sev.Settings.Rules) != 1 {
		t.Fatalf("sweep evidence: %+v", sev)
	}
	e.must(sw, is.OpNotesResolve, "", map[string]any{"notes": []any{c1, c2}, "summary": "folded into CLAUDE.md"})
	if _, err := e.call(sw, is.OpFix, "", map[string]any{"notes": []any{c1}, "title": "t", "body": "b"}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("the sweep binding requested a fix: %v", err)
	}
	e.finish(sw)

	// Restart: the next day's triage sees only what is left.
	e.reopen()
	e.clock.Add(24 * time.Hour)
	if fired := e.fire(); len(fired) != 2 {
		t.Fatalf("second day: %+v", fired)
	}
	if got := e.standing(repScope, is.WorkKindTriage); len(got) != 1 || got[0].ID != triage.ID {
		t.Fatalf("one standing triage: %+v", got)
	}
	tr2 := e.start(triage.ID)
	ev2 := decode[is.Evidence](t, e.must(tr2, is.OpEvidence, "", map[string]any{"for": "triage"}))
	if ev2.Reports.Count != 1 || ev2.Reports.Groups[0].IDs[0] != r3["reported"] || ev2.Problems.Count != 0 || len(ev2.Fixing) != 0 {
		t.Fatalf("second triage evidence: %+v %+v", ev2.Reports, ev2.Problems)
	}
	e.finish(tr2)
	sw2 := e.start(sweeps[0].ID)
	if sev := decode[is.Evidence](t, e.must(sw2, is.OpEvidence, "", map[string]any{"for": "sweep"})); sev.Corrections.Count != 0 {
		t.Fatalf("second sweep: %+v", sev.Corrections)
	}
	e.finish(sw2)
	_ = ctx
}

// TestCitedCausationKeepsCoreLimits: citing a cause never shortens a chain
// or escapes the trigger's budgets, and an extension can cite only its own
// Events in the emitting Work's scope.
func TestCitedCausationKeepsCoreLimits(t *testing.T) {
	const depth = 3
	e := newIntrospectionEnv(t, runtime.CausalLimits{MaxDepth: depth}, map[string]any{})
	a := e.manual(repScope, "engineering.issue", "agent")
	r1 := e.must(a, is.OpReport, "r1", defect("first"))
	e.finish(a)
	// r1's processing reports again (depth 2), and that report's
	// processing reports once more (depth 3).
	p1 := e.start(is.WorkID(repScope, r1["reported"].(string)))
	r2 := e.must(p1, is.OpReport, "loop", defect("about r1"))
	// From a trigger at depth 1, a fix citing r1 (depth 1) sits at 2.
	if f := e.must(p1, is.OpFix, "f1", map[string]any{"reports": []any{r1["reported"]}, "title": "t", "body": "b"}); f["depth"] != float64(2) || f["cause"] != r1["event"] {
		t.Fatalf("fix from depth 1: %v", f)
	}
	e.finish(p1)
	p2 := e.start(is.WorkID(repScope, r2["reported"].(string)))
	r3 := e.must(p2, is.OpReport, "loop", defect("about r2"))
	e.finish(p2)
	p3 := e.start(is.WorkID(repScope, r3["reported"].(string)))
	// The trigger is at the depth limit: citing the shallow r1 does not
	// reset the chain, so core refuses the fix without the extension's help.
	if _, err := e.call(p3, is.OpFix, "f3", map[string]any{"reports": []any{r1["reported"]}, "title": "t", "body": "b"}); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("a cited cause escaped the depth limit: %v", err)
	}
	e.finish(p3)
	causal, err := runtime.NewCausal(e.store, e.clock.Now, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	rejections, err := causal.Rejections(context.Background())
	if err != nil || len(rejections) != 1 || rejections[0].Reason != "depth_limit" || rejections[0].ParentID != domain.ID(r1["event"].(string)) {
		t.Fatalf("rejection: %+v %v", rejections, err)
	}
}

// TestCitedCauseContract exercises the generic contract with a rogue
// registration: only its own Events in the scope can be cited, and a cited
// emission spends both chains' per-minute budgets.
func TestCitedCauseContract(t *testing.T) {
	var plan extension.EmitPlan
	rogue := extension.Registration{Version: extension.ContractVersion, Name: "rogue", EventTypes: []string{"rogue.noted"},
		Operations: []extension.Operation{{Name: "rogue.note", Emit: func(context.Context, extension.Authority, domain.Payload) (extension.EmitPlan, error) {
			return plan, nil
		}}}}
	const perMinute = 3
	e := &repEnv{t: t, clock: &ghClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, path: filepath.Join(t.TempDir(), "conduit.db"), extra: &rogue,
		limits: runtime.CausalLimits{PerMinute: perMinute}, tokens: map[domain.ID]string{}, grants: map[string][]string{"agent": {"rogue.note", is.OpReport}}}
	e.open()
	e.putScope(repScope, true)
	e.putScope(repSecond, true)
	note := func(delivery, external string, cause domain.ID) extension.EmitPlan {
		// A key source belongs to one scope; ops notes use their own.
		source := "rogue/notes"
		if strings.HasPrefix(external, "ops-") {
			source = "rogue/ops"
		}
		k := domain.CorrelationKey{Source: source, ExternalID: external}
		return extension.EmitPlan{Cause: cause, Event: domain.Event{Type: "rogue.noted", DeliveryID: delivery, CorrelationKey: &k},
			Commit: func(extension.Emitted) (extension.EmitCommit, error) { return extension.EmitCommit{}, nil }}
	}
	// A (root R1) emits its own Event; an introspection report and a root
	// exist too, and another scope has a rogue Event of its own.
	a := e.manual(repScope, "engineering.issue", "agent")
	plan = note("own", "n1", "")
	own, err := e.call(a, "rogue.note", "", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	report := e.must(a, is.OpReport, "", defect("a report"))
	other := e.manual(repSecond, "engineering.issue", "agent")
	plan = note("other-scope", "ops-n2", "")
	foreignScope, err := e.call(other, "rogue.note", "", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	b := e.manual(repScope, "engineering.issue", "agent")
	aRoot := e.execution(a.Lease.ExecutionID).TriggerID
	for name, cause := range map[string]domain.ID{
		"another registration's Event": domain.ID(report["event"].(string)),
		"another chain's core root":    aRoot,
		"another scope's own Event":    foreignScope.Event.Event.ID,
		"a missing Event":              "emit/none",
	} {
		plan = note("bad-"+name, "x", cause)
		if _, err := e.call(b, "rogue.note", "", map[string]any{}); !errors.Is(err, extension.ErrUnauthorized) {
			t.Errorf("citing %s: %v", name, err)
		}
	}
	// B cites A's Event: it joins A's chain, one deeper than the deeper of
	// the two.
	plan = note("cite-1", "c1", own.Event.Event.ID)
	cited, err := e.call(b, "rogue.note", "", map[string]any{})
	if err != nil || cited.Event.Event.CausationID != own.Event.Event.ID || cited.Event.Event.Source != "execution/"+string(b.Lease.ExecutionID) {
		t.Fatalf("cited: %+v %v", cited.Event, err)
	}
	// A's chain has spent own, report and cite-1: a third cite is refused.
	plan = note("cite-2", "c2", own.Event.Event.ID)
	if _, err := e.call(b, "rogue.note", "", map[string]any{}); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("A's budget: %v", err)
	}
	// B's own chain was charged for cite-1 too: two more plain emissions
	// exhaust it (cite-1 + 2 = 3).
	for i, want := range []bool{true, true, false} {
		plan = note("plain-"+string(rune('a'+i)), "p", "")
		_, err := e.call(b, "rogue.note", "", map[string]any{})
		if (err == nil) != want {
			t.Fatalf("plain emission %d: %v", i, err)
		}
	}
}

// TestIntrospectionRemovalPolicy: removing the entry cancels its timers and
// retires its idle Work (running attempts finish first), keeping every
// record; adding it back starts a new standing run; a disabled scope keeps
// everything.
func TestIntrospectionRemovalPolicy(t *testing.T) {
	cfg := map[string]any{"triage": map[string]any{"cron": "51 8 * * *"}}
	e := newIntrospectionEnv(t, runtime.CausalLimits{}, cfg)
	parsed, _ := is.ParseConfig(rawJSON(cfg))
	with := is.Entry{Scope: repScope, Enabled: true, Config: parsed}
	e.ensure(with)
	e.clock.Add(24*time.Hour - 9*time.Minute)
	e.fire()
	triage := e.standing(repScope, is.WorkKindTriage)[0]
	e.finish(e.start(triage.ID)) // idle standing Work, ready

	a := e.manual(repScope, "engineering.issue", "agent")
	r1 := e.must(a, is.OpReport, "", defect("one"))
	r2 := e.must(a, is.OpReport, "", defect("two"))
	e.finish(a)
	running := e.start(is.WorkID(repScope, r1["reported"].(string)))
	manualTriage := e.manual(repScope, is.WorkKindTriage, "triage")
	e.finish(manualTriage)
	retired := func(configured map[domain.ID]bool) []domain.ID {
		var ids []domain.ID
		e.read(func(tx storage.Tx) error {
			ws, err := is.Retired(tx, configured)
			for _, w := range ws {
				ids = append(ids, w.ID)
			}
			return err
		})
		return ids
	}

	// With the entry, nothing is retired; a disabled scope keeps its timer.
	if ids := retired(map[domain.ID]bool{repScope: true, repSecond: true}); len(ids) != 0 {
		t.Fatalf("retired with the entry: %v", ids)
	}
	if res := e.ensure(is.Entry{Scope: repScope, Enabled: false, Config: parsed}); res.Cancelled != 0 || res.Kept != 1 {
		t.Fatalf("disabled scope: %+v", res)
	}

	// Removal: the timer goes, the idle Work is retired, the running attempt
	// is left to finish, and records stay.
	e.putIntrospectionScope(repScope, nil)
	if res := e.ensure(); res.Cancelled != 1 {
		t.Fatalf("removal: %+v", res)
	}
	ids := retired(map[domain.ID]bool{repScope: false, repSecond: true})
	want := []domain.ID{is.WorkID(repScope, r2["reported"].(string)), triage.ID, manualTriage.Request.Work.ID}
	if len(ids) != 3 || !hasIDs(ids, want) || hasIDs(ids, []domain.ID{running.Request.Work.ID}) {
		t.Fatalf("retired: %v, want %v", ids, want)
	}
	for _, id := range ids {
		w, _ := e.work(id)
		if _, err := e.control.CancelWork(context.Background(), id, w.Revision, is.RetireReason, "retire/"+string(id)); err != nil {
			t.Fatalf("cancel %s: %v", id, err)
		}
	}
	e.finish(running)
	if w, _ := e.work(running.Request.Work.ID); w.State != domain.WorkCompleted {
		t.Fatalf("the running attempt did not finish: %s", w.State)
	}
	if ids := retired(map[domain.ID]bool{repScope: false}); len(ids) != 0 {
		t.Fatalf("retired again: %v", ids)
	}
	if n := len(e.reports(repScope)); n != 2 {
		t.Fatalf("records after removal: %d", n)
	}

	// Adding the entry back arms a new epoch whose slots start a new
	// standing run instead of addressing the retired one.
	e.putIntrospectionScope(repScope, cfg)
	e.clock.Add(time.Hour)
	if res := e.ensure(with); res.Armed != 1 {
		t.Fatalf("re-add: %+v", res)
	}
	e.clock.Add(24 * time.Hour)
	e.fire()
	var live []domain.Work
	for _, w := range e.standing(repScope, is.WorkKindTriage) {
		if !w.State.Terminal() && w.ID != manualTriage.Request.Work.ID {
			live = append(live, w)
		}
	}
	if len(live) != 1 || live[0].ID == triage.ID || live[0].State != domain.WorkReady {
		t.Fatalf("new standing run: %+v", live)
	}

	// Removing only the schedule retires its standing run once idle.
	e.putIntrospectionScope(repScope, map[string]any{})
	if res := e.ensure(is.Entry{Scope: repScope, Enabled: true}); res.Cancelled != 1 {
		t.Fatalf("schedule removal: %+v", res)
	}
	if ids := retired(map[domain.ID]bool{repScope: true}); len(ids) != 1 || ids[0] != live[0].ID {
		t.Fatalf("retired standing run: %v", ids)
	}
}

func hasIDs(have, want []domain.ID) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

func rawJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// TestExtensionOriginatedReportChain: a host-watch report (no execution
// made it) is its own causal root, gets its own report Work on the user's
// binding, appears in the triage evidence, and a fix for it traces to it.
func TestExtensionOriginatedReportChain(t *testing.T) {
	e := newIntrospectionEnv(t, runtime.CausalLimits{}, map[string]any{"host_watch": map[string]any{}})
	alert := is.HostAlert{Metric: "disk", Path: "/", Value: 95, Threshold: 90, Over: true}
	ev, rep := is.HostReport(repScope, alert, e.clock.Now())
	accept := func() bool {
		added := false
		if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
			accepted, ok, err := runtime.AcceptInTx(tx, repScope, ev)
			if err != nil || !ok {
				return err
			}
			added = true
			return is.PutOriginInTx(tx, accepted, rep)
		}); err != nil {
			t.Fatal(err)
		}
		return added
	}
	if !accept() || accept() {
		t.Fatal("the crossing was not reported exactly once")
	}
	e.route()
	reports := e.reports(repScope)
	if len(reports) != 1 || reports[0].Origin != is.OriginHostWatch || reports[0].Depth != 0 || reports[0].Root != string(ev.ID) || reports[0].Work != "" {
		t.Fatalf("origin record: %+v", reports)
	}
	p := e.start(is.WorkID(repScope, rep.ID))
	if x := e.execution(p.Lease.ExecutionID); x.TriggerID != ev.ID || x.ExecutorBinding != "processor" {
		t.Fatalf("processing: %+v", x)
	}
	e.finish(p)
	tr := e.manual(repScope, "manual.triage", "triager")
	evd := decode[is.Evidence](t, e.must(tr, is.OpEvidence, "", map[string]any{"for": "triage"}))
	if evd.Reports.Count != 1 || evd.Reports.Groups[0].Kind != "env" || len(evd.Reports.Groups[0].Works) != 0 {
		t.Fatalf("evidence: %+v", evd.Reports)
	}
	fix := e.must(tr, is.OpFix, "", map[string]any{"reports": []any{rep.ID}, "title": "Free disk space", "body": "Prune old workspaces."})
	if fix["cause"] != string(ev.ID) || fix["depth"] != float64(1) || fix["root"] != string(ev.ID) {
		t.Fatalf("fix of an origin report: %v", fix)
	}
	e.finish(tr)
}
