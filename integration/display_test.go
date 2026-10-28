package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// A route's display lands on the Work it routes to: the first input says
// what the Work is, a later match without one keeps it and a later one with
// one replaces it. A display over the limits is dropped and recorded, and
// never stops the Work receiving input.
func TestRoutingStoresWorkDisplay(t *testing.T) {
	first := &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Requester: "example-user", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}}}
	later := &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}, {Label: "PR #2", URL: "https://tracker.example/pull/2"}}}
	last := &domain.WorkDisplay{Title: "Fix the example widget, renamed", Ref: "#1"}
	displays := map[domain.ID]*domain.WorkDisplay{"a": first, "c": later, "bad": {Title: "x", Links: []domain.Link{{Label: "Bad", URL: "javascript:alert(1)"}}}, "d": last}
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
	}{{"a", first}, {"b", first}, {"c", later}, {"bad", later}, {"d", last}} {
		requireAccept(t, f, step.event, &key)
		requireRoute(t, f.Driver, domain.ID(step.event))
		if got := display(); !equalDisplay(got, step.want) {
			t.Fatalf("after %s the Work shows %+v, want %+v", step.event, got, step.want)
		}
	}
	routingRead(t, f, func(tx storage.Tx) error {
		bad, ok, err := runtime.RouteDecisionInTx(tx, "bad")
		if err != nil || !ok || bad.Reason != "matched" || !strings.Contains(bad.DisplayRefused, "http or https") {
			t.Fatalf("the refused display's route record: %+v %v %v", bad, ok, err)
		}
		inputs, err := runtime.PendingInputsInTx(tx, "work/item")
		if err != nil || len(inputs) != 5 {
			t.Fatalf("the Work's input after a refused display: %d %v", len(inputs), err)
		}
		return nil
	})
}

// Earlier input that a key attachment brings into the same routing pass
// builds on the display before it: a link it adds is not lost.
func TestRoutingDisplayAcrossABatch(t *testing.T) {
	own := domain.CorrelationKey{Source: "fixture/account", ExternalID: "item"}
	pr := domain.CorrelationKey{Source: "fixture/account", ExternalID: "pull-2"}
	handler := routingHandlerFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d := extension.RouteDecision{Match: true, CreateID: "work/item", Kind: "fixture.work"}
		var cur domain.WorkDisplay
		if in.Work != nil && in.Work.Display != nil {
			cur = *in.Work.Display
		}
		switch in.Event.Event.ID {
		case "a":
			cur = domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1"}
		case "pull":
			cur.Links = append(cur.Links, domain.Link{Label: "PR #2", URL: "https://tracker.example/pull/2"})
		case "rename":
			cur.Title, d.AttachKeys = "Fix the example widget, renamed", []domain.CorrelationKey{pr}
		}
		d.Display = &cur
		return d, nil
	})
	f := routingFixture(t, "", routingOptions(handler, "queue"))
	requireAccept(t, f, "a", &own)
	requireRoute(t, f.Driver, "a")
	// The PR's input waits unrouted until the rename attaches its key.
	requireAccept(t, f, "pull", &pr)
	requireAccept(t, f, "rename", &own)
	requireRoute(t, f.Driver, "rename")
	var w domain.Work
	routingRead(t, f, func(tx storage.Tx) error { var e error; w, e = tx.Work("work/item"); return e })
	want := &domain.WorkDisplay{Title: "Fix the example widget, renamed", Ref: "#1", Links: []domain.Link{{Label: "PR #2", URL: "https://tracker.example/pull/2"}}}
	if !equalDisplay(w.Display, want) {
		t.Fatalf("after the batch the Work shows %+v, want %+v", w.Display, want)
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
	// An agent's description is cut to one line within the limit, never
	// refused.
	out, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.changed"}}, Description: "a reply in the example thread\nand more " + strings.Repeat("w", extension.MaxWaitDescription)}})
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

// A timed Wait nobody described is described by its due time when it is
// declared, so the description outlives the timer.
func TestToolbeltTimedWaitDescription(t *testing.T) {
	ctx := context.Background()
	h := newToolbeltHarness(t, "")
	due := h.f.Clock.Now().Add(time.Hour)
	out, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Types: []string{"fixture.changed"}}, Timeout: &due}})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait declaration: %v", err)
	}
	if want := "until " + due.UTC().Format(extension.DueLayout); out.Wait.Description != want {
		t.Fatalf("timed wait described %q, want %q", out.Wait.Description, want)
	}
}
