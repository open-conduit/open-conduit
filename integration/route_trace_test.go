package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
	_ "modernc.org/sqlite"
)

// sqliteDump is every row of every table in the store's database file, so a
// trace that wrote anything at all (any KV namespace included) shows up.
func sqliteDump(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err = tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	out := map[string][]string{}
	for _, n := range names {
		rows, err := db.Query(`SELECT * FROM "` + n + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var cells []string
			for _, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				cells = append(cells, fmt.Sprint(v))
			}
			out[n] = append(out[n], strings.Join(cells, "\x1f"))
		}
		rows.Close()
		sort.Strings(out[n])
	}
	if len(out["kv"]) == 0 || len(out["events"]) == 0 {
		t.Fatalf("dump read nothing from %s", path)
	}
	return out
}

func verdicts(tr runtime.RouteTrace) map[string]string {
	out := map[string]string{}
	for _, r := range tr.Routes {
		out[r.Route] = r.Verdict
	}
	return out
}

func reasons(tr runtime.RouteTrace) map[string]string {
	out := map[string]string{}
	for _, r := range tr.Routes {
		out[r.Route] = r.Reason
	}
	return out
}

func (e *toolsEnv) trace(ev domain.Event) runtime.RouteTrace {
	e.t.Helper()
	tr, err := e.router.Trace(context.Background(), ghScope, ev, e.belt.DependencyFilter)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr
}

func mergeEvent(ns, repo string, pr int) domain.Event {
	data, _ := json.Marshal(gh.PullPayload{Repo: repo, Number: pr, Title: "Schema", State: "closed", Merged: true})
	return domain.Event{Type: gh.TypePullMerged, Source: ns, CorrelationKey: &domain.CorrelationKey{Source: ns, ExternalID: gh.PullKey(pr)}, Data: domain.Payload{Schema: gh.SchemaPull, Data: data}}
}

// With the hold route before the issue route, a merge that frees a held
// issue is decided by the handlers (both skip it) and wakes the held Work
// through its dependency; a new issue skips `held` and matches `issues`; an
// event for the waiting Work itself bypasses every route. Tracing writes
// nothing.
func TestRouteTraceHeldBeforeIssues(t *testing.T) {
	e := newToolsEnv(t)
	pr := e.fake.OpenPull("Schema", "", "elsewhere", "s50", ghAlice)
	e.sync()
	_, held := e.heldClaim("Wave 2", "after the schema\n\n<!-- waitFor: pr:"+itoa(pr)+":merged -->")
	if why := e.runHold(held.ID); why != "" {
		t.Fatalf("released early: %q", why)
	}
	if got := e.work(held.ID); got.State != domain.WorkWaiting {
		t.Fatalf("held Work: %v", got.State)
	}
	before := sqliteDump(t, e.path)

	tr := e.trace(mergeEvent(ghNS, ghRepo, pr))
	if got := verdicts(tr); got["held"] != runtime.VerdictSkip || got["issues"] != runtime.VerdictSkip {
		t.Fatalf("merge verdicts: %+v", tr.Routes)
	}
	if got := reasons(tr); got["held"] != "only an opened issue is held" || got["issues"] != "only an opened issue starts Work" {
		t.Fatalf("merge reasons: %+v", tr.Routes)
	}
	if tr.Effect.Kind != runtime.EffectWake || tr.Effect.WorkID != held.ID || !strings.Contains(tr.Effect.Detail, "in scope "+ghScope) || !strings.Contains(tr.Effect.Detail, "(wait ") {
		t.Fatalf("merge effect: %+v", tr.Effect)
	}

	// Another PR merging frees nothing.
	if tr = e.trace(mergeEvent(ghNS, ghRepo, pr+100)); tr.Effect.Kind != runtime.EffectUnmatched || len(tr.Also) != 0 {
		t.Fatalf("unrelated merge: %+v", tr)
	}

	// The issue route's own sample: a new issue.
	var issue domain.Event
	for _, s := range (gh.Route{}).Samples(context.Background(), extension.SampleInput{Scope: e.scope(ghScope)}) {
		if s.ID == "issue-opened" {
			issue = s.Event
		}
	}
	tr = e.trace(issue)
	if got := verdicts(tr); got["held"] != runtime.VerdictSkip || got["issues"] != runtime.VerdictMatch || got["requests"] != runtime.VerdictNotReached {
		t.Fatalf("new issue verdicts: %+v", tr.Routes)
	}
	if reasons(tr)["held"] != "the issue is not labelled scheduled" {
		t.Fatalf("held skip reason: %+v", tr.Routes)
	}
	if tr.Effect.Kind != runtime.EffectStart || tr.Effect.Binding != "agent" {
		t.Fatalf("new issue effect: %+v", tr.Effect)
	}

	// Input for the waiting Work itself: no route is consulted; its wait
	// decides. A comment is kept as context; what it waits for wakes it.
	key := held.CorrelationKeys[0]
	tr = e.trace(domain.Event{Type: gh.TypeCommentHuman, Source: ghNS, CorrelationKey: &key})
	for _, r := range tr.Routes {
		if r.Verdict != runtime.VerdictBypassed || !strings.Contains(r.Reason, "its wait decides") {
			t.Fatalf("waiting Work's routes: %+v", tr.Routes)
		}
	}
	if tr.Work == nil || tr.Work.ID != held.ID || tr.Effect.Kind != runtime.EffectQueue || !strings.Contains(tr.Effect.Detail, "does not match its wait") {
		t.Fatalf("comment while held: %+v", tr.Effect)
	}
	met := domain.Event{Type: gh.TypeDependencyMet, Source: ghNS, CorrelationKey: &key}
	if tr = e.trace(met); tr.Effect.Kind != runtime.EffectWake || tr.Effect.WorkID != held.ID {
		t.Fatalf("own wake: %+v", tr.Effect)
	}

	if after := sqliteDump(t, e.path); !reflect.DeepEqual(before, after) {
		t.Fatal("tracing changed stored state")
	}

	// A wait the causal limits blocked never wakes: the same input is
	// kept as context, and the merge wakes nothing.
	e.blockWaits(held.ID)
	if reflect.DeepEqual(before, sqliteDump(t, e.path)) {
		t.Fatal("the dump does not see a KV write")
	}
	if tr = e.trace(met); tr.Effect.Kind != runtime.EffectQueue || !strings.Contains(tr.Effect.Detail, "blocked by the causal limits") {
		t.Fatalf("blocked wait: %+v", tr.Effect)
	}
	if tr = e.trace(mergeEvent(ghNS, ghRepo, pr)); tr.Effect.Kind != runtime.EffectUnmatched || len(tr.Also) != 0 {
		t.Fatalf("merge with a blocked wait: %+v", tr)
	}
}

// blockWaits marks the Work's active wait blocked, as a causal-limit
// refusal at wake time does.
func (e *toolsEnv) blockWaits(work domain.ID) {
	e.t.Helper()
	if err := e.store.Transact(context.Background(), storage.Guard{Now: e.clock.Now()}, func(tx storage.Tx) error {
		rows, err := tx.List("core.waits", "")
		if err != nil {
			return err
		}
		blocked := 0
		for _, row := range rows {
			var meta map[string]any
			if json.Unmarshal(row.Value, &meta) != nil || meta["Binding"] != "hold" {
				continue
			}
			meta["Blocked"] = true
			b, _ := json.Marshal(meta)
			if err = tx.Put("core.waits", row.Key, b); err != nil {
				return err
			}
			blocked++
		}
		if blocked != 1 {
			return fmt.Errorf("blocked %d waits", blocked)
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

// A merge accepted in another scope wakes Work waiting on it through its
// cross-scope dependency; traced from the source scope, the wake names the
// waiting Work's own scope.
func TestRouteTraceCrossScopeDependencyWake(t *testing.T) {
	e := newToolsEnv(t)
	ops := githubtest.New("octo/ops", "tok")
	defer ops.Close()
	opsPR := ops.OpenPull("Deploy", "", "deploy", "d1", ghAlice)
	opsNS := "github/github.com/octo/ops"
	opsExt := `{"namespace":"` + opsNS + `","scope":"ops","repo":"octo/ops"}`
	e.putScope("ops", opsExt)
	cfg, _ := gh.ParseConnection([]byte(`{"api_url":"` + ops.URL + `"}`))
	c, _ := gh.NewClient(cfg, func(context.Context, string) (string, error) { return "tok", nil }, gh.ClientOptions{Now: e.clock.Now})
	s, _ := gh.ParseExtension([]byte(opsExt), nil)
	p, _ := gh.NewProjector(gh.ProjectorOptions{Store: e.store, Client: c, Settings: s, Now: e.clock.Now})
	e.extra = []gh.Source{{Settings: s, Client: c, Projector: p}}
	e.mu.Lock()
	e.aliasOK = true
	e.mu.Unlock()
	_, w := e.claim("Needs the deploy", "", "bug")
	a := e.start(w.ID)
	if _, err := e.call(a, gh.OpWait, "", map[string]any{"conditions": []string{"pr:ops/" + itoa(opsPR) + ":merged"}, "reason": "after the deploy"}); err != nil {
		t.Fatal(err)
	}
	e.finish(a, nil)
	if err := e.scheduler.ReconcileSubscriptions(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if got := e.work(w.ID); got.State != domain.WorkWaiting {
		t.Fatalf("not waiting: %v", got.State)
	}
	tr, err := e.router.Trace(context.Background(), "ops", mergeEvent(opsNS, "octo/ops", opsPR), e.belt.DependencyFilter)
	if err != nil {
		t.Fatal(err)
	}
	if tr.ScopeID != "ops" || tr.Effect.Kind != runtime.EffectWake || tr.Effect.WorkID != w.ID || !strings.Contains(tr.Effect.Detail, "in scope "+ghScope) {
		t.Fatalf("cross-scope wake: %+v", tr)
	}
	// The same merge traced in this scope is not the subscription's source.
	if tr = e.trace(mergeEvent(opsNS, "octo/ops", opsPR)); tr.Effect.Kind == runtime.EffectWake {
		t.Fatalf("wake from the wrong source scope: %+v", tr.Effect)
	}
}

func (e *toolsEnv) scope(id domain.ID) domain.Scope {
	var s domain.Scope
	e.read(func(tx storage.Tx) error { var err error; s, err = tx.Scope(id); return err })
	return s
}

// traceThenRoute traces an Event, then accepts and routes it for real, and
// requires the trace's effect to be the policy the router recorded.
func traceThenRoute(t *testing.T, f conformance.RuntimeFixture, id string, key *domain.CorrelationKey) runtime.RouteTrace {
	t.Helper()
	router := f.Driver.(*routingDriver).router
	tr, err := router.Trace(context.Background(), "routing-scope", routingEvent(f, id, key), nil)
	if err != nil {
		t.Fatalf("trace %s: %v", id, err)
	}
	requireAccept(t, f, id, key)
	requireRoute(t, f.Driver, domain.ID(id))
	rec, err := router.Decision(context.Background(), domain.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	want := rec.Policy
	if want == "terminal" {
		want = runtime.EffectDrop
	}
	if tr.Effect.Kind != want && !(tr.Effect.Kind == runtime.EffectStart && want == "queue") {
		t.Fatalf("%s: traced %+v, routed %+v", id, tr.Effect, rec)
	}
	return tr
}

// The trace's policy is the router's own decision: an unsupported control
// falls back, a drop route still queues for Work that is not running, an
// inject into an attempt on another binding falls back, and terminal Work
// bypasses the routes.
func TestRouteTracePoliciesMatchRouting(t *testing.T) {
	key := &domain.CorrelationKey{Source: "fixture/account", ExternalID: "traced"}
	t.Run("inject_fallback", func(t *testing.T) {
		f := routingFixture(t, "", conformance.RuntimeOptions{Fallback: "queue", Routes: []conformance.RuntimeRoute{{Route: domain.Route{Name: "active", ActivePolicy: "inject"}, Handler: routingHandlerFunc(routingDecision)}}})
		traceThenRoute(t, f, "first", key)
		if _, err := f.Driver.Start("work/traced"); err != nil {
			t.Fatal(err)
		}
		tr := traceThenRoute(t, f, "second", key)
		if tr.Effect.Kind != runtime.EffectQueue || !strings.Contains(tr.Effect.Detail, "cannot do the route's active policy") {
			t.Fatalf("inject fallback: %+v", tr.Effect)
		}
	})
	t.Run("binding_mismatch", func(t *testing.T) {
		initial := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
			d, e := routingDecision(in)
			d.Match = in.Event.Event.ID == "initial"
			return d, e
		})
		f := routingFixture(t, "", conformance.RuntimeOptions{Capabilities: domain.Capabilities{Inject: true}, Fallback: "queue", Routes: []conformance.RuntimeRoute{
			{Route: domain.Route{Name: "initial", Binding: "fixture", ActivePolicy: "queue"}, Handler: initial},
			{Route: domain.Route{Name: "active", Binding: "alternate", ActivePolicy: "inject"}, Handler: routingHandlerFunc(routingDecision)},
		}})
		routingRead(t, f, func(tx storage.Tx) error {
			s, err := tx.Scope("routing-scope")
			if err != nil {
				return err
			}
			s.Bindings = append(s.Bindings, domain.Binding{Name: "alternate", Executor: "fixture"})
			return tx.PutScope(s, s.Revision)
		})
		traceThenRoute(t, f, "initial", key)
		if _, err := f.Driver.Start("work/traced"); err != nil {
			t.Fatal(err)
		}
		tr := traceThenRoute(t, f, "active", key)
		if tr.Effect.Kind != runtime.EffectQueue || !strings.Contains(tr.Effect.Detail, "another binding") {
			t.Fatalf("binding mismatch: %+v", tr.Effect)
		}
	})
	t.Run("drop", func(t *testing.T) {
		f := routingFixture(t, "", routingOptions(routingHandlerFunc(routingDecision), "drop"))
		traceThenRoute(t, f, "first", key)
		// Not running: even a drop route queues.
		if tr := traceThenRoute(t, f, "second", key); tr.Effect.Kind != runtime.EffectQueue {
			t.Fatalf("drop on ready Work: %+v", tr.Effect)
		}
		if _, err := f.Driver.Start("work/traced"); err != nil {
			t.Fatal(err)
		}
		if tr := traceThenRoute(t, f, "third", key); tr.Effect.Kind != runtime.EffectDrop {
			t.Fatalf("drop on running Work: %+v", tr.Effect)
		}
	})
	t.Run("terminal", func(t *testing.T) {
		f := routingFixture(t, "", routingOptions(routingHandlerFunc(routingDecision), "queue"))
		traceThenRoute(t, f, "first", key)
		if err := f.Driver.Cancel("work/traced"); err != nil {
			t.Fatal(err)
		}
		tr := traceThenRoute(t, f, "late", key)
		if tr.Effect.Kind != runtime.EffectDrop || verdicts(tr)["default"] != runtime.VerdictBypassed {
			t.Fatalf("terminal: %+v", tr)
		}
	})
}

// The management endpoint and `route --trace` answer without an
// Idempotency-Key and write nothing; an owned key traces to its Work.
func TestManagementRouteTrace(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	var root client.Event
	h.mustCLI(&root, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "d1", "--key-source", "core.manual", "--key-external-id", "k1", "--key", "k1")
	// Let the clock move so a write would differ from what it replaced.
	h.clock.Advance(time.Minute)
	before := sqliteDump(t, filepath.Join(h.state, "conduit.db"))

	var tr client.RouteTrace
	h.mustCLI(&tr, "route", "--trace", "operator.requested", "--scope", "ops", "--key-source", "core.manual", "--key-external-id", "k2")
	if len(tr.Routes) != 1 || tr.Routes[0].Route != "incoming" || tr.Routes[0].Verdict != "match" || tr.Effect.Kind != "start" || tr.Generation != "1" {
		t.Fatalf("new key: %+v", tr)
	}
	h.mustCLI(&tr, "route", "--trace", "operator.requested", "--scope", "ops", "--key-source", "core.manual", "--key-external-id", "k1")
	if tr.Work == nil || tr.Work.ID != root.RoutedWorkID || tr.Effect.Kind != "queue" || tr.Effect.WorkID != root.RoutedWorkID {
		t.Fatalf("owned key: %+v", tr)
	}
	// The endpoint itself needs no Idempotency-Key.
	if _, err := h.operatorClient().TraceRoute(ctx, "ops", client.TraceEvent{Type: "operator.requested"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.operatorClient().TraceRoute(ctx, "missing", client.TraceEvent{Type: "operator.requested"}); !isStatus(err, 404) {
		t.Fatalf("unknown scope: %v", err)
	}
	// What acceptance would refuse is refused, not explained: core events
	// and sources, and a payload schema that is not namespaced.
	for _, ev := range []client.TraceEvent{{Type: "core.wait.woken"}, {Type: "fixture.thing", Source: "core.subscription"}, {Type: "operator.requested", Data: &client.Payload{Schema: "fixture", Data: json.RawMessage(`{}`)}}} {
		if _, err := h.operatorClient().TraceRoute(ctx, "ops", ev); !isStatus(err, 422) {
			t.Fatalf("%+v: %v", ev, err)
		}
	}
	if _, errOut, code := h.cli("route", "--trace", "no-such-sample", "--scope", "ops"); code != 2 || !strings.Contains(errOut, "no sample no-such-sample") {
		t.Fatalf("unknown sample: %d %s", code, errOut)
	}
	var samples []client.RouteSample
	h.mustCLI(&samples, "route", "--samples", "--scope", "ops")
	if len(samples) != 0 {
		t.Fatalf("the fixture handler offers no samples: %+v", samples)
	}
	if after := sqliteDump(t, filepath.Join(h.state, "conduit.db")); !reflect.DeepEqual(before, after) {
		t.Fatal("tracing changed stored state")
	}
}

// A trace refuses what acceptance would refuse: a malformed envelope, a core
// type or source, and a registered source's input in another scope. A
// source no ingress registers is not known.
func TestRouteTraceRefusesWhatAcceptanceRefuses(t *testing.T) {
	e := newToolsEnv(t)
	issue := mergeEvent(ghNS, ghRepo, 1)
	if err := runtime.CheckTraceEvent(issue); err != nil {
		t.Fatalf("a merge: %v", err)
	}
	for name, ev := range map[string]domain.Event{
		"unnamespaced type": {Type: "merged", Source: ghNS},
		"core type":         {Type: "core.wait.woken", Source: ghNS},
		"core source":       {Type: gh.TypePullMerged, Source: "core.subscription"},
		"bad schema":        {Type: gh.TypePullMerged, Source: ghNS, Data: domain.Payload{Schema: "pull", Data: []byte(`{}`)}},
		"bad data":          {Type: gh.TypePullMerged, Source: ghNS, Data: domain.Payload{Schema: gh.SchemaPull, Data: []byte(`{`)}},
		"half a key":        {Type: gh.TypePullMerged, Source: ghNS, CorrelationKey: &domain.CorrelationKey{Source: ghNS}},
	} {
		if runtime.CheckTraceEvent(ev) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if known, err := e.ingress.AdmitsTrace(ghScope, issue); !known || err != nil {
		t.Fatalf("the merge from its own source: %v %v", known, err)
	}
	if known, err := e.ingress.AdmitsTrace("ops", issue); !known || err == nil || !strings.Contains(err.Error(), "belongs to scope "+ghScope) {
		t.Fatalf("another scope's source: %v %v", known, err)
	}
	if known, err := e.ingress.AdmitsTrace(ghScope, domain.Event{Type: "nobody.thing", Source: "nobody/x"}); known || err != nil {
		t.Fatalf("an unregistered source: %v %v", known, err)
	}
}
