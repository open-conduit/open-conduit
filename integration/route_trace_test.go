package integration_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// storeDump is every record a route could write: accepted Events and their
// dispositions, Work, attempts, waits, timers, scopes and the routing,
// wait and subscription records.
type storeDump struct {
	Latest     uint64
	Events     []domain.AcceptedEvent
	Works      []domain.Work
	Executions []domain.Execution
	Waits      []domain.Wait
	Timers     []domain.Timer
	Scopes     []domain.Scope
	KV         map[string][]storage.Entry
}

func (e *toolsEnv) dump() storeDump {
	e.t.Helper()
	d := storeDump{KV: map[string][]storage.Entry{}}
	e.read(func(tx storage.Tx) error {
		var err error
		if d.Latest, err = tx.LatestSequence(); err != nil {
			return err
		}
		if d.Events, err = tx.Events(0, 0); err != nil {
			return err
		}
		if d.Works, err = tx.ListWorks(); err != nil {
			return err
		}
		for _, w := range d.Works {
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			ws, err := tx.ListWaits(w.ID)
			if err != nil {
				return err
			}
			d.Executions, d.Waits = append(d.Executions, xs...), append(d.Waits, ws...)
		}
		if d.Timers, err = tx.ListTimers(); err != nil {
			return err
		}
		if d.Scopes, err = tx.ListScopes(); err != nil {
			return err
		}
		for _, ns := range []string{"core.routing", "core.routing.queue", "core.routing.intents", "core.routing.consumed", "core.waits", "core.waits.subscriptions", "core.acceptance"} {
			if d.KV[ns], err = tx.List(ns, ""); err != nil {
				return err
			}
		}
		return nil
	})
	return d
}

func verdicts(tr runtime.RouteTrace) map[string]string {
	out := map[string]string{}
	for _, r := range tr.Routes {
		out[r.Route] = r.Verdict
	}
	return out
}

// With the hold route before the issue route, a merge that frees a held
// issue is credited to `held` and `issues` is never reached; a new issue
// skips `held` (not scheduled) and matches `issues`. Tracing writes nothing.
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
	ctx := context.Background()
	before := e.dump()

	data, _ := json.Marshal(gh.PullPayload{Repo: ghRepo, Number: pr, Title: "Schema", State: "closed", Merged: true})
	merge := domain.Event{Type: gh.TypePullMerged, Source: ghNS, CorrelationKey: &domain.CorrelationKey{Source: ghNS, ExternalID: gh.PullKey(pr)}, Data: domain.Payload{Schema: gh.SchemaPull, Data: data}}
	tr, err := e.router.Trace(ctx, ghScope, merge, e.belt.DependencyFilter)
	if err != nil {
		t.Fatal(err)
	}
	if got := verdicts(tr); got["held"] != runtime.VerdictMatch || got["issues"] != runtime.VerdictNotReached || got["requests"] != runtime.VerdictNotReached {
		t.Fatalf("merge verdicts: %v (%+v)", got, tr.Routes)
	}
	if tr.Effect.Kind != runtime.EffectWake || tr.Effect.WorkID != held.ID || tr.Effect.Binding != "hold" {
		t.Fatalf("merge effect: %+v", tr.Effect)
	}

	// Another PR merging frees nothing.
	other := merge
	data, _ = json.Marshal(gh.PullPayload{Repo: ghRepo, Number: pr + 100, State: "closed", Merged: true})
	other.CorrelationKey = &domain.CorrelationKey{Source: ghNS, ExternalID: gh.PullKey(pr + 100)}
	other.Data = domain.Payload{Schema: gh.SchemaPull, Data: data}
	if tr, err = e.router.Trace(ctx, ghScope, other, e.belt.DependencyFilter); err != nil {
		t.Fatal(err)
	}
	if tr.Effect.Kind != runtime.EffectUnmatched {
		t.Fatalf("unrelated merge: %+v", tr.Effect)
	}

	// The issue route's own sample: a new issue.
	var issue domain.Event
	scope := e.scope(ghScope)
	for _, s := range (gh.Route{}).Samples(scope, domain.Route{}) {
		if s.ID == "issue-opened" {
			issue = s.Event
		}
	}
	if issue.Type != gh.TypeIssueOpen {
		t.Fatalf("no issue-opened sample: %+v", issue)
	}
	if tr, err = e.router.Trace(ctx, ghScope, issue, e.belt.DependencyFilter); err != nil {
		t.Fatal(err)
	}
	if got := verdicts(tr); got["held"] != runtime.VerdictSkip || got["issues"] != runtime.VerdictMatch || got["requests"] != runtime.VerdictNotReached {
		t.Fatalf("new issue verdicts: %v (%+v)", got, tr.Routes)
	}
	if tr.Routes[0].Reason != "the issue is not labelled scheduled" {
		t.Fatalf("held skip reason: %q", tr.Routes[0].Reason)
	}
	if tr.Effect.Kind != runtime.EffectStart || tr.Effect.Binding != "agent" {
		t.Fatalf("new issue effect: %+v", tr.Effect)
	}

	// The held issue's own new comment is kept as context while it waits.
	comment := domain.Event{Type: gh.TypeCommentHuman, Source: ghNS, CorrelationKey: &held.CorrelationKeys[0]}
	if tr, err = e.router.Trace(ctx, ghScope, comment, e.belt.DependencyFilter); err != nil {
		t.Fatal(err)
	}
	if tr.Work == nil || tr.Work.ID != held.ID || tr.Effect.Kind != runtime.EffectQueue {
		t.Fatalf("comment while held: %+v", tr.Effect)
	}

	if after := e.dump(); !reflect.DeepEqual(before, after) {
		t.Fatal("tracing changed stored state")
	}
}

func (e *toolsEnv) scope(id domain.ID) domain.Scope {
	var s domain.Scope
	e.read(func(tx storage.Tx) error { var err error; s, err = tx.Scope(id); return err })
	return s
}

// The management endpoint and `route --trace` answer without an
// Idempotency-Key and accept nothing; an owned key traces to its Work.
func TestManagementRouteTrace(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	var root client.Event
	h.mustCLI(&root, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", "d1", "--key-source", "core.manual", "--key-external-id", "k1", "--key", "k1")
	var before, after client.Page[client.Event]
	h.mustCLI(&before, "events", "list")

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
	if _, errOut, code := h.cli("route", "--trace", "no-such-sample", "--scope", "ops"); code != 2 || !strings.Contains(errOut, "no sample no-such-sample") {
		t.Fatalf("unknown sample: %d %s", code, errOut)
	}
	var samples []client.RouteSample
	h.mustCLI(&samples, "route", "--samples", "--scope", "ops")
	if len(samples) != 0 {
		t.Fatalf("the fixture handler offers no samples: %+v", samples)
	}
	h.mustCLI(&after, "events", "list")
	if !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatal("tracing accepted or routed an event")
	}
}
