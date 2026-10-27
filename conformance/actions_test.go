package conformance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// approver is a reference provider: an "approve" action on open Work for
// the operator, with input "yes". Each weakening drops one guarantee.
type approver struct {
	listAlways, grantStrangers, grantAnyInput, rawError, setsSource bool
}

func (a approver) Actions(_ context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	if !a.listAlways && (t.Work == nil || t.Work.State.Terminal()) {
		return nil, nil
	}
	return []extension.Action{{ID: "approve", Label: "Approve", Prompt: "type yes"}}, nil
}

func (a approver) Invoke(_ context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	refuse := func(code string) (extension.ActionPlan, error) {
		if a.rawError {
			return extension.ActionPlan{}, errors.New(code)
		}
		return extension.ActionPlan{}, &extension.OperationError{Code: code, Message: "refused: " + code}
	}
	switch {
	case c.Action != "approve":
		return refuse("unknown_action")
	case c.Target.Work == nil || c.Target.Work.State.Terminal():
		return refuse("not_outstanding")
	case c.Caller != extension.LocalOperator && !a.grantStrangers:
		return refuse("not_allowed")
	case c.Input != "yes" && !a.grantAnyInput:
		return refuse("invalid_input")
	}
	e := domain.Event{Type: "ref.approved"}
	if a.setsSource {
		e.Source = extension.ActionSource
	}
	return extension.ActionPlan{Event: e}, nil
}

func approverFixture(p extension.ActionProvider) conformance.ActionFixture {
	open := domain.Work{ID: "w", State: domain.WorkWaiting}
	done := domain.Work{ID: "w", State: domain.WorkCancelled}
	bad := "no"
	return conformance.ActionFixture{Provider: p, Available: extension.ActionTarget{Work: &open}, Unavailable: extension.ActionTarget{Work: &done},
		Action: "approve", Input: "yes", BadInput: &bad, Caller: extension.LocalOperator, Stranger: extension.Caller{Kind: "web", ID: "someone"}, EventType: "ref.approved"}
}

func TestReferenceActions(t *testing.T) {
	conformance.RunActions(t, func() conformance.ActionFixture { return approverFixture(approver{}) })
}

func TestWeakenedActionProvidersAreRejected(t *testing.T) {
	for name, p := range map[string]approver{
		"lists unavailable action": {listAlways: true},
		"grants strangers":         {grantStrangers: true},
		"grants invalid input":     {grantAnyInput: true},
		"unauthored refusal":       {rawError: true},
		"sets host fields":         {setsSource: true},
	} {
		if err := conformance.CheckActions(approverFixture(p)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
