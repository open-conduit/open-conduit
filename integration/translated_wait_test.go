package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Translated waits (03-04): an extension operation turns domain wait syntax
// into a generic plan. The toolbelt declares it like the builtin wait and
// installs its dependency subscriptions once the Wait registers. These cases
// exercise the core path the GitHub tools use, with synthetic Events.

var depKey = domain.CorrelationKey{Source: "fixture/account", ExternalID: "other"}

func acceptSource(t *testing.T, h *toolbeltHarness, id, typ, data string) {
	t.Helper()
	ev := domain.Event{ID: domain.ID(id), Type: typ, Source: "fixture/account", DeliveryID: id, OccurredAt: h.f.Clock.Now(), ReceivedAt: h.f.Clock.Now(), CorrelationKey: &depKey}
	if data != "" {
		ev.Data = domain.Payload{Schema: "fixture.dep.v1", Data: json.RawMessage(data)}
	}
	if err := h.f.Driver.Accept(ev); err != nil {
		t.Fatal(err)
	}
}

func depPlan(fromNow bool, filter string) func(extension.Authority, domain.Payload) (extension.WaitPlan, error) {
	return func(extension.Authority, domain.Payload) (extension.WaitPlan, error) {
		d := extension.Dependency{ID: "dep:other:merged", SourceKey: depKey, Types: []string{"fixture.merged"}, AliasType: "fixture.dependency", Label: "dep:other:merged", FromNow: fromNow}
		if filter != "" {
			d.Filter = domain.Payload{Schema: "fixture.filter.v1", Data: json.RawMessage(filter)}
		}
		return extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.dependency", "fixture.changed"}}}, Dependencies: []extension.Dependency{d}, Output: domain.Payload{Schema: "fixture.out.v1", Data: json.RawMessage(`{"waiting":true}`)}}, nil
	}
}

func waitState(t *testing.T, h *toolbeltHarness, id domain.ID) (domain.Wait, domain.Work) {
	t.Helper()
	var w domain.Wait
	var work domain.Work
	routingRead(t, h.f, func(tx storage.Tx) error {
		var err error
		if w, err = tx.Wait(id); err != nil {
			return err
		}
		work, err = tx.Work("work/item")
		return err
	})
	return w, work
}

func declareAndFinish(t *testing.T, h *toolbeltHarness) extension.ToolResult {
	t.Helper()
	ctx := context.Background()
	out, err := h.belt.CallRequest(ctx, h.token, "req-1", extension.ToolCall{Operation: "fixture.wait", Input: domain.Payload{Schema: "fixture.in.v1", Data: json.RawMessage(`{}`)}})
	if err != nil || out.Wait == nil || string(out.Output.Data) != `{"waiting":true}` {
		t.Fatalf("translated wait: %+v %v", out, err)
	}
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.ReadContext}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("translated wait kept authority: %v", err)
	}
	if err = h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "parked"}}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTranslatedWaitAlreadyMetDependencyWakesAtOnce(t *testing.T) {
	h := newToolbeltHarness(t, "reopen")
	h.plan = depPlan(false, "")
	acceptSource(t, h, "merged-before", "fixture.merged", "")
	out := declareAndFinish(t, h)
	w, work := waitState(t, h, out.Wait.ID)
	if w.State != domain.WaitWoken || work.State != domain.WorkReady {
		t.Fatalf("already-met dependency did not wake through the core path: %v %v", w.State, work.State)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		alias, err := tx.Event(w.WakeEventID)
		if err != nil {
			return err
		}
		if alias.Event.Type != "fixture.dependency" || alias.Event.CausationID != "merged-before" || alias.Event.WorkID != "work/item" || alias.Event.CorrelationKey == nil || *alias.Event.CorrelationKey != toolbeltKey {
			t.Fatalf("alias identity: %+v", alias.Event)
		}
		var p struct {
			SourceType string `json:"source_type"`
			Label      string `json:"label"`
		}
		if json.Unmarshal(alias.Event.Data.Data, &p) != nil || p.SourceType != "fixture.merged" || p.Label != "dep:other:merged" {
			t.Fatalf("alias payload: %s", alias.Event.Data.Data)
		}
		// The foreign key is never attached to the waiting Work.
		if _, err := tx.WorkByCorrelation(depKey); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("dependency attached the foreign key: %v", err)
		}
		return nil
	})
}

func TestTranslatedWaitFromNowIgnoresHistoryAndWakesOnTheNextOccurrence(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = depPlan(true, "")
	acceptSource(t, h, "old-run", "fixture.merged", "")
	out := declareAndFinish(t, h)
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitActive {
		t.Fatalf("an occurrence before the wait counted: %v", w.State)
	}
	acceptSource(t, h, "new-run", "fixture.merged", "")
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	w, work := waitState(t, h, out.Wait.ID)
	if w.State != domain.WaitWoken || work.State != domain.WorkReady {
		t.Fatalf("next occurrence did not wake: %v %v", w.State, work.State)
	}
	// Replaying reconciliation creates no second alias or wake.
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		n := 0
		for _, a := range all {
			if a.Event.Type == "fixture.dependency" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d aliases", n)
		}
		return err
	})
}

func TestTranslatedWaitFilterSelectsTheNamedItem(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = depPlan(false, `{"number":8}`)
	acceptSource(t, h, "other-pr", "fixture.merged", `{"number":7}`)
	out := declareAndFinish(t, h)
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitActive {
		t.Fatalf("another item on the same key woke the wait: %v", w.State)
	}
	acceptSource(t, h, "named-pr", "fixture.merged", `{"number":8}`)
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitWoken {
		t.Fatalf("named item did not wake: %v", w.State)
	}
}

// TestTranslatedWaitAsksAnUnansweredFilterAgain is the missed merge in
// #613: the named PR merges once, while the extension's filter can't
// answer (a remote extension timing out under load). The pass must not
// read that as "not this PR" and checkpoint past the only merge there will
// be: it reports the filter unavailable, keeps the wait, and the next pass
// asks about the same Event again and wakes it.
func TestTranslatedWaitAsksAnUnansweredFilterAgain(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = depPlan(false, `{"number":8}`)
	acceptSource(t, h, "other-pr", "fixture.merged", `{"number":7}`)
	out := declareAndFinish(t, h)
	h.mu.Lock()
	h.filterDown = true
	h.mu.Unlock()
	acceptSource(t, h, "named-pr", "fixture.merged", `{"number":8}`)
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); !errors.Is(err, runtime.ErrDependencyFilterUnavailable) {
		t.Fatalf("a pass whose filter couldn't answer reported %v", err)
	}
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitActive {
		t.Fatalf("an unanswered filter woke the wait: %v", w.State)
	}
	// Later Events on the key don't carry the scan past the unanswered one.
	acceptSource(t, h, "later-pr", "fixture.merged", `{"number":9}`)
	h.mu.Lock()
	h.filterDown = false
	h.mu.Unlock()
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	w, work := waitState(t, h, out.Wait.ID)
	if w.State != domain.WaitWoken || work.State != domain.WorkReady {
		t.Fatalf("the merge the filter missed never woke the wait: %v %v", w.State, work.State)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		alias, err := tx.Event(w.WakeEventID)
		if err == nil && alias.Event.CausationID != "named-pr" {
			t.Fatalf("woken by %s, want the merge the filter missed", alias.Event.CausationID)
		}
		return err
	})
}

// filterErrors is the DependencyFilterErrors a subscription pass reported.
func filterErrors(t *testing.T, err error) []*runtime.DependencyFilterError {
	t.Helper()
	if err == nil {
		return nil
	}
	parts := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		if _, typed := err.(*runtime.DependencyFilterError); !typed {
			parts = j.Unwrap()
		}
	}
	var out []*runtime.DependencyFilterError
	for _, p := range parts {
		var f *runtime.DependencyFilterError
		if !errors.As(p, &f) {
			t.Fatalf("pass failed: %v", p)
		}
		out = append(out, f)
	}
	return out
}

// twoDeps waits on the named PR through two filtered dependencies, and a
// third without a filter on a type that never comes, so a pass has filtered
// and unfiltered subscriptions in one namespace.
func twoDeps(a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
	p, err := depPlan(false, `{"number":8}`)(a, in)
	d := p.Dependencies[0]
	d.ID, d.Label = "dep:other:merged:2", "dep:other:merged:2"
	never := p.Dependencies[0]
	never.ID, never.Label, never.Types, never.Filter = "dep:other:never", "dep:other:never", []string{"fixture.never"}, domain.Payload{}
	p.Dependencies = append(p.Dependencies, d, never)
	return p, err
}

func setFilter(h *toolbeltHarness, down bool, failsOn string) {
	h.mu.Lock()
	h.filterDown, h.filterFailsOn, h.filterCalls = down, failsOn, 0
	h.mu.Unlock()
}

func filterCalls(h *toolbeltHarness) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.filterCalls
}

// TestTranslatedWaitOutageNeverPassesTheMergeBy: while a filter can't be
// reached, each pass asks it once (not once per held subscription), each
// held subscription gets its turn, the diagnostic names the Wait, Work,
// dependency and Event, and the route trace shows the Work held. However
// long the outage lasts, the one merge is never passed by: once the filter
// answers again it wakes the wait (#613).
func TestTranslatedWaitOutageNeverPassesTheMergeBy(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = twoDeps
	out := declareAndFinish(t, h)
	setFilter(h, true, "")
	acceptSource(t, h, "merged", "fixture.merged", `{"number":8}`)
	failed := filterErrors(t, h.scheduler.ReconcileSubscriptions(context.Background(), 10))
	// Negative control for the breaker: without it both filtered
	// subscriptions would ask the filter that isn't answering.
	if calls := filterCalls(h); calls != 1 || len(failed) != 1 {
		t.Fatalf("a pass asked the unreachable filter %d times and reported %d; want once", calls, len(failed))
	}
	f := failed[0]
	if !f.Held || !f.Unreachable || f.WaitID != out.Wait.ID || f.WorkID != "work/item" || f.Namespace != "fixture" || f.EventID != "merged" || f.Sequence == 0 || f.Passes != 1 || f.Failures != 0 {
		t.Fatalf("diagnostic: %+v", f)
	}
	// The next pass asks the subscription the breaker held back, not the
	// same one again.
	second := filterErrors(t, h.scheduler.ReconcileSubscriptions(context.Background(), 10))
	if len(second) != 1 || second[0].Label == f.Label {
		t.Fatalf("the held-back subscription didn't get the next pass: first %s, then %+v", f.Label, second)
	}
	tr, err := h.f.Driver.(*schedulerDriver).Router.Trace(context.Background(), "routing-scope", domain.Event{ID: "probe", Type: "fixture.merged", Source: "fixture/account", DeliveryID: "probe", CorrelationKey: &depKey, Data: domain.Payload{Schema: "fixture.dep.v1", Data: json.RawMessage(`{"number":8}`)}}, h.belt.DependencyFilter)
	if err != nil {
		t.Fatal(err)
	}
	held := false
	for _, e := range tr.Also {
		held = held || e.Kind == runtime.EffectHeld && e.WorkID == "work/item"
	}
	if !held || tr.Effect.Kind == runtime.EffectWake {
		t.Fatalf("the trace hid the unreachable filter: %+v", tr)
	}
	// An outage far past the failure bounds still holds.
	for range 3 {
		for range 30 {
			for _, f := range filterErrors(t, h.scheduler.ReconcileSubscriptions(context.Background(), 10)) {
				if !f.Held {
					t.Fatalf("an outage passed the merge by: %+v", f)
				}
			}
		}
		h.f.Clock.Advance(time.Hour)
	}
	setFilter(h, false, "")
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	w, work := waitState(t, h, out.Wait.ID)
	if w.State != domain.WaitWoken || work.State != domain.WorkReady {
		t.Fatalf("the merge held through the outage didn't wake: %v %v", w.State, work.State)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		alias, err := tx.Event(w.WakeEventID)
		if err == nil && alias.Event.CausationID != "merged" {
			t.Fatalf("woken by %s, want the original merge", alias.Event.CausationID)
		}
		return err
	})
}

// TestTranslatedWaitPoisonEventIsPassedByWithinBounds: when the live
// extension fails on one Event every time it is asked, that says nothing
// about the namespace's other subscriptions, so it doesn't trip the
// breaker; the subscription holds within the bounds and then passes the
// Event by, recorded, so the wait isn't held for good.
func TestTranslatedWaitPoisonEventIsPassedByWithinBounds(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = twoDeps
	out := declareAndFinish(t, h)
	acceptSource(t, h, "poison", "fixture.merged", `{"number":8}`)
	setFilter(h, false, "poison")
	start := h.f.Clock.Now()
	failed := filterErrors(t, h.scheduler.ReconcileSubscriptions(context.Background(), 10))
	if calls := filterCalls(h); calls != 2 || len(failed) != 2 {
		t.Fatalf("a failure on one Event held back other subscriptions: %d calls, %d reported", calls, len(failed))
	}
	for _, f := range failed {
		if !f.Held || f.Unreachable || f.Failures != 1 {
			t.Fatalf("diagnostic: %+v", f)
		}
	}
	// Many passes within the time bound keep holding.
	for range 30 {
		h.scheduler.ReconcileSubscriptions(context.Background(), 10)
	}
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitActive {
		t.Fatalf("held wait: %v", w.State)
	}
	h.f.Clock.Advance(time.Hour + time.Minute)
	gave := 0
	for _, f := range filterErrors(t, h.scheduler.ReconcileSubscriptions(context.Background(), 10)) {
		if !f.Held {
			gave++
			if f.EventID != "poison" || !f.Since.Equal(start) || f.Failures < 20 {
				t.Fatalf("gave up with %+v", f)
			}
		}
	}
	if gave != 2 {
		t.Fatalf("%d subscriptions passed the poison Event by, want 2", gave)
	}
	acceptSource(t, h, "named-pr", "fixture.merged", `{"number":8}`)
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitWoken {
		t.Fatalf("an Event after the passed-by one didn't wake: %v", w.State)
	}
}

func TestTranslatedWaitOwnKeyInputStillWakes(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = depPlan(false, "")
	out := declareAndFinish(t, h)
	key := toolbeltKey
	requireAccept(t, h.f, "comment", &key)
	requireRoute(t, h.f.Driver, "comment")
	if w, _ := waitState(t, h, out.Wait.ID); w.State != domain.WaitWoken || w.WakeEventID != "comment" {
		t.Fatalf("own-key input did not wake the translated wait: %+v", w)
	}
	// The woken wait's subscription emits nothing more.
	acceptSource(t, h, "late-merge", "fixture.merged", "")
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if a.Event.Type == "fixture.dependency" {
				t.Fatalf("alias after the wait was woken: %s", a.Event.ID)
			}
		}
		return err
	})
}

func TestTranslatedWaitCrossScopeNeedsAuthorization(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	h.plan = func(a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
		p, err := depPlan(false, "")(a, in)
		p.Dependencies[0].SourceScopeID = "other-scope"
		return p, err
	}
	ctx := context.Background()
	if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: "fixture.wait"}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("unauthorized cross-scope dependency declared: %v", err)
	}
	// The refusal kept authority: the attempt can still act.
	if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.ReadContext}); err != nil {
		t.Fatalf("refused translation released authority: %v", err)
	}
	h.mu.Lock()
	h.aliasOK = true
	h.mu.Unlock()
	if out, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: "fixture.wait"}); err != nil || out.Wait == nil {
		t.Fatalf("authorized cross-scope dependency refused: %v", err)
	}
}

func TestTranslatedWaitTimeoutUsesTheSameTimer(t *testing.T) {
	h := newToolbeltHarness(t, "reopen")
	deadline := h.f.Clock.Now().Add(time.Hour)
	h.plan = func(a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
		p, err := depPlan(false, "")(a, in)
		p.Wait.Timeout = &deadline
		return p, err
	}
	out := declareAndFinish(t, h)
	w, _ := waitState(t, h, out.Wait.ID)
	if w.State != domain.WaitActive || w.TimeoutTimerID == "" {
		t.Fatalf("timeout not armed: %+v", w)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		_, err := tx.Timer(w.TimeoutTimerID)
		return err
	})
}
