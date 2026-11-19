package conformance_test

import (
	"context"
	"testing"
	"time"

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
	openSettledAt, bothWordings, badTone, unofferedChoice, choiceTwice              bool
}

// Actions offers approver's action with choices, which the open request
// words.
func (a asks) Actions(ctx context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	actions, err := a.approver.Actions(ctx, t)
	for i := range actions {
		actions[i].Choices = []string{"yes", "no"}
	}
	return actions, err
}

func (a asks) Requests(_ context.Context, t extension.ActionTarget) ([]extension.Request, error) {
	if t.Work == nil {
		return nil, nil
	}
	settled := extension.Request{ID: "r1", Label: "Approve", State: extension.RequestRefused, By: "operator:local", SettledAt: time.Date(2026, 11, 19, 7, 53, 0, 0, time.UTC),
		Wording: &extension.Wording{Wants: "deploy", Choices: []extension.ChoiceWording{{Choice: "yes", Label: "Approve", Tone: extension.ToneAffirm}}}}
	if a.settledAction {
		settled.Action = "approve"
	}
	if t.Work.State.Terminal() {
		return []extension.Request{settled}, nil
	}
	open := extension.Request{ID: "r2", Label: "Approve", Action: "approve", Responders: []string{"operator"}, State: extension.RequestOpen,
		Wording: &extension.Wording{Question: "Go?", Choices: []extension.ChoiceWording{{Choice: "yes", Label: "Yes", Tone: extension.ToneAffirm}}}}
	switch {
	case a.openSettledAt:
		open.SettledAt = settled.SettledAt
	case a.bothWordings:
		open.Wording.Wants = "deploy"
	case a.badTone:
		open.Wording.Choices[0].Tone = "primary"
	case a.unofferedChoice:
		open.Wording.Choices[0].Choice = "maybe"
	case a.choiceTwice:
		open.Wording.Choices = append(open.Wording.Choices, open.Wording.Choices[0])
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
		"request without an ID":           {noID: true},
		"an ID reported twice":            {twice: true},
		"an unknown state":                {badState: true},
		"an open request settled by":      {openBy: true},
		"a settled request names action":  {settledAction: true},
		"names an action not offered":     {unoffered: true},
		"two requests name one action":    {sharedAction: true},
		"answerable request no action":    {noAction: true},
		"an open request settled at":      {openSettledAt: true},
		"worded as approval and question": {bothWordings: true},
		"a tone outside affirm or danger": {badTone: true},
		"words a choice not offered":      {unofferedChoice: true},
		"words a choice twice":            {choiceTwice: true},
	} {
		if err := conformance.CheckRequests(asksFixture(p)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
