package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
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
