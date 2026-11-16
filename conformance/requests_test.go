package conformance_test

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// asks is a reference request provider: on waiting Work, an earlier
// request refused by the operator and an open one "approve" answers; on
// ended Work, the refused one alone. Each weakening breaks one rule.
type asks struct {
	approver
	noID, twice, badState, openBy, settledAction, unoffered, sharedAction, noAction bool
}

func (a asks) Requests(_ context.Context, t extension.ActionTarget) ([]extension.Request, error) {
	if t.Work == nil {
		return nil, nil
	}
	settled := extension.Request{ID: "r1", Label: "Approve", State: extension.RequestRefused, By: "operator:local"}
	if a.settledAction {
		settled.Action = "approve"
	}
	if t.Work.State.Terminal() {
		return []extension.Request{settled}, nil
	}
	open := extension.Request{ID: "r2", Label: "Approve", Action: "approve", Responders: []string{"operator"}, State: extension.RequestOpen}
	switch {
	case a.noID:
		open.ID = ""
	case a.twice:
		open.ID = settled.ID
	case a.badState:
		open.State = "pending"
	case a.openBy:
		open.By = "operator:local"
	case a.unoffered:
		open.Action = "deploy"
	case a.noAction:
		open.Action = ""
	}
	out := []extension.Request{settled, open}
	if a.sharedAction {
		out = append(out, extension.Request{ID: "r3", Label: "Approve again", Action: "approve", State: extension.RequestOpen})
	}
	return out, nil
}

func asksFixture(p conformance.RequestProvider) conformance.RequestFixture {
	open := domain.Work{ID: "w", State: domain.WorkWaiting}
	done := domain.Work{ID: "w", State: domain.WorkCancelled}
	return conformance.RequestFixture{Provider: p, Answerable: extension.ActionTarget{Work: &open}, Others: []extension.ActionTarget{{Work: &done}, {}}}
}

func TestReferenceRequests(t *testing.T) {
	conformance.RunRequests(t, func() conformance.RequestFixture { return asksFixture(asks{}) })
}

func TestWeakenedRequestProvidersAreRejected(t *testing.T) {
	for name, p := range map[string]asks{
		"request without an ID":          {noID: true},
		"an ID reported twice":           {twice: true},
		"an unknown state":               {badState: true},
		"an open request settled by":     {openBy: true},
		"a settled request names action": {settledAction: true},
		"names an action not offered":    {unoffered: true},
		"two requests name one action":   {sharedAction: true},
		"answerable request no action":   {noAction: true},
	} {
		if err := conformance.CheckRequests(asksFixture(p)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
