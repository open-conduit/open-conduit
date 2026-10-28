package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// A route's display lands on the Work it routes to: the first input says
// what the Work is, a later match without one keeps it, a later one with
// one replaces it, and a display over the limits fails the routing pass.
func TestRoutingStoresWorkDisplay(t *testing.T) {
	first := &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Requester: "example-user", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}}}
	later := &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}, {Label: "PR #2", URL: "https://tracker.example/pull/2"}}}
	displays := map[domain.ID]*domain.WorkDisplay{"a": first, "c": later, "bad": {Title: "x", Links: []domain.Link{{Label: "Bad", URL: "javascript:alert(1)"}}}}
	handler := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, err := routingDecision(in)
		d.Display = displays[in.Event.Event.ID]
		return d, err
	})
	f := routingFixture(t, "", routingOptions(handler, "queue"))
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "item"}
	display := func() *domain.WorkDisplay {
		var w domain.Work
		routingRead(t, f, func(tx storage.Tx) error { var e error; w, e = tx.Work("work/item"); return e })
		return w.Display
	}
	for _, step := range []struct {
		event string
		want  *domain.WorkDisplay
	}{{"a", first}, {"b", first}, {"c", later}} {
		requireAccept(t, f, step.event, &key)
		requireRoute(t, f.Driver, domain.ID(step.event))
		if got := display(); !equalDisplay(got, step.want) {
			t.Fatalf("after %s the Work shows %+v, want %+v", step.event, got, step.want)
		}
	}
	requireAccept(t, f, "bad", &key)
	if err := f.Driver.Route("bad"); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("a display linking to javascript: was routed: %v", err)
	}
	if got := display(); !equalDisplay(got, later) {
		t.Fatalf("a refused display changed the Work: %+v", got)
	}
}

func equalDisplay(a, b *domain.WorkDisplay) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Title != b.Title || a.Ref != b.Ref || a.Requester != b.Requester || len(a.Links) != len(b.Links) {
		return false
	}
	for i := range a.Links {
		if a.Links[i] != b.Links[i] {
			return false
		}
	}
	return true
}

// A wait's description is fixed on the Wait it registers, from the
// builtin wait and a translated one alike; one over the limit is refused.
func TestToolbeltWaitDescription(t *testing.T) {
	ctx := context.Background()
	h := newToolbeltHarness(t, "")
	long := &extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.changed"}}, Description: strings.Repeat("w", extension.MaxWaitDescription+1)}
	if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Wait, Wait: long}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("an overlong description was declared: %v", err)
	}
	out, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.changed"}}, Description: "a reply in the example thread"}})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait declaration: %v", err)
	}
	if err = h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		w, err := tx.Wait(out.Wait.ID)
		if err != nil || w.Description != "a reply in the example thread" {
			t.Fatalf("registered wait %+v %v", w, err)
		}
		// Core never lets the description change once declared.
		w.Description, w.State = "something else", domain.WaitCancelled
		if err = tx.PutWait(w, w.Revision); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("description rewritten: %v", err)
		}
		return nil
	})
}

func TestToolbeltTranslatedWaitDescription(t *testing.T) {
	ctx := context.Background()
	h := newToolbeltHarness(t, "")
	h.plan = func(extension.Authority, domain.Payload) (extension.WaitPlan, error) {
		return extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.changed"}}, Description: "checks on PR #2"}}, nil
	}
	out, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: "fixture.wait", Input: domain.Payload{Schema: "fixture.wait.v1", Data: []byte(`{}`)}})
	if err != nil {
		t.Fatalf("translated wait: %v", err)
	}
	if err = h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		waits, err := tx.ListWaits("work/item")
		if err != nil || len(waits) != 1 || waits[0].Description != "checks on PR #2" {
			t.Fatalf("translated wait registered %+v %v (output %+v)", waits, err, out)
		}
		return nil
	})
}
