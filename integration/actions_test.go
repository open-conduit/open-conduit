package integration_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// approveProvider offers "approve" on the ops scope; it grants only the
// local operator answering "yes".
type approveProvider struct{}

func (approveProvider) Actions(_ context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	if t.Scope.ID != "ops" || t.Work != nil {
		return nil, nil
	}
	return []extension.Action{{ID: "approve", Label: "Approve the request", Prompt: "type yes"}}, nil
}

func (approveProvider) Invoke(_ context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	switch {
	case c.Action != "approve":
		return extension.ActionPlan{}, &extension.OperationError{Code: "unknown_action", Message: "no such action"}
	case c.Caller != extension.LocalOperator:
		return extension.ActionPlan{}, &extension.OperationError{Code: "not_allowed", Message: "only the operator approves"}
	case c.Input != "yes":
		return extension.ActionPlan{}, &extension.OperationError{Code: "not_outstanding", Message: "nothing is waiting for that answer"}
	}
	return extension.ActionPlan{Event: domain.Event{Type: "fixture.approved", CorrelationKey: &domain.CorrelationKey{Source: "fixture", ExternalID: "request/1"}}, Message: "approved"}, nil
}

// Actions are listed and invoked through the management API: the provider
// authorises each call, a refusal comes back as its code and message and
// leaves no Event, and a granted call's Event is accepted under
// core.action, which no posted Event can carry.
func TestManagementActions(t *testing.T) {
	h := newManagementHarness(t)
	h.actions = map[string]extension.ActionProvider{"fixture": approveProvider{}}
	ctx := context.Background()
	c := h.operatorClient()
	count := func() int {
		page, err := c.Events(ctx, url.Values{"limit": {"200"}})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Items)
	}

	var listed []client.Action
	h.mustCLI(&listed, "actions", "list", "--scope", "ops")
	if len(listed) != 1 || listed[0] != (client.Action{Provider: "fixture", ID: "approve", Label: "Approve the request", Prompt: "type yes"}) {
		t.Fatalf("listed: %+v", listed)
	}

	before := count()
	_, errOut, code := h.cli("actions", "run", "fixture", "approve", "--scope", "ops", "--input", "no")
	if code != 1 || !strings.Contains(errOut, "not_outstanding") || !strings.Contains(errOut, "nothing is waiting for that answer") {
		t.Fatalf("refusal: %d %s", code, errOut)
	}
	if _, errOut, code = h.cli("actions", "run", "nobody", "approve", "--scope", "ops", "--input", "yes"); code != 1 || !strings.Contains(errOut, "not_found") {
		t.Fatalf("unknown provider: %d %s", code, errOut)
	}
	if count() != before {
		t.Fatal("a refused action left an Event")
	}

	var res client.ActionResult
	h.mustCLI(&res, "actions", "run", "fixture", "approve", "--scope", "ops", "--input", "yes", "--key", "a1")
	if res.Message != "approved" || res.Event.Type != "fixture.approved" || res.Event.Source != extension.ActionSource || res.Event.DeliveryID != "fixture/approve/a1" || res.Event.CausationID != "" || res.Event.ScopeID != "ops" {
		t.Fatalf("granted: %+v", res)
	}
	var again client.ActionResult
	h.mustCLI(&again, "actions", "run", "fixture", "approve", "--scope", "ops", "--input", "yes", "--key", "a1")
	if again.Event.ID != res.Event.ID || count() != before+1 {
		t.Fatalf("replay accepted a second Event: %+v", again)
	}

	// The same family and key posted as an Event carries the operator's
	// manual source, never the action's.
	var posted client.Event
	h.mustCLI(&posted, "events", "emit", "--scope-id", "ops", "--type", "fixture.approved", "--delivery-id", "forged", "--key-source", "fixture", "--key-external-id", "request/1", "--key", "f1")
	if posted.Source != "core.manual" {
		t.Fatalf("posted event source: %+v", posted)
	}
}
