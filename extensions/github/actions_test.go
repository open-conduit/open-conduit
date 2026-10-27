package github_test

import (
	"context"
	"errors"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/storage"
)

// ownersStore answers WorkByCorrelation from a fixed set of owned keys.
type ownersStore struct {
	owned map[domain.CorrelationKey]domain.Work
}
type ownersTx struct {
	storage.Tx
	owned map[domain.CorrelationKey]domain.Work
}

func (s ownersStore) Transact(_ context.Context, _ storage.Guard, fn func(storage.Tx) error) error {
	return fn(ownersTx{owned: s.owned})
}
func (t ownersTx) WorkByCorrelation(k domain.CorrelationKey) (domain.Work, error) {
	if w, ok := t.owned[k]; ok {
		return w, nil
	}
	return domain.Work{}, storage.ErrNotFound
}

func claimActions(t *testing.T, owned map[domain.CorrelationKey]domain.Work) gh.Actions {
	t.Helper()
	s, err := gh.ParseExtension([]byte(`{"namespace":"`+ns+`","scope":"eng","repo":"`+repo+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return gh.Actions{Store: ownersStore{owned}, Settings: s}
}

// The claim action meets the shared action contract: offered on its
// entry's scope (not on Work), only to the local operator, for a number.
func TestClaimActionContract(t *testing.T) {
	a := claimActions(t, nil)
	bad := "seven"
	conformance.RunActions(t, func() conformance.ActionFixture {
		return conformance.ActionFixture{Provider: a, Available: extension.ActionTarget{Scope: domain.Scope{ID: "eng"}}, Unavailable: extension.ActionTarget{Scope: domain.Scope{ID: "eng"}, Work: &domain.Work{ID: "w"}},
			Action: gh.ActionClaim, Input: "#7", BadInput: &bad, Caller: extension.LocalOperator, Stranger: extension.Caller{Kind: "github_user", ID: "octocat"}, EventType: gh.TypeClaimRequested}
	})
	plan, err := a.Invoke(context.Background(), extension.ActionCall{Target: extension.ActionTarget{Scope: domain.Scope{ID: "eng"}}, Action: gh.ActionClaim, Input: "7", Caller: extension.LocalOperator})
	if err != nil || plan.Event.CorrelationKey == nil || *plan.Event.CorrelationKey != (domain.CorrelationKey{Source: ns, ExternalID: "issue/7"}) {
		t.Fatalf("claim plan: %+v %v", plan, err)
	}
	if acts, _ := a.Actions(context.Background(), extension.ActionTarget{Scope: domain.Scope{ID: "other"}}); len(acts) != 0 {
		t.Fatalf("claim offered on another scope: %+v", acts)
	}
}

// An issue that already has Work is no longer claimable: the action is
// refused and produces nothing.
func TestClaimActionRefusesOwnedIssue(t *testing.T) {
	a := claimActions(t, map[domain.CorrelationKey]domain.Work{{Source: ns, ExternalID: "issue/7"}: {ID: "w7", State: domain.WorkRunning}})
	_, err := a.Invoke(context.Background(), extension.ActionCall{Target: extension.ActionTarget{Scope: domain.Scope{ID: "eng"}}, Action: gh.ActionClaim, Input: "7", Caller: extension.LocalOperator})
	var oe *extension.OperationError
	if !errors.As(err, &oe) || oe.Code != "already_claimed" {
		t.Fatalf("owned issue: %v", err)
	}
}
