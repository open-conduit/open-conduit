package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// ActionClaim is the entry's scope action that claims an issue by hand.
const ActionClaim = "claim"

// Actions is a GitHub entry's extension.ActionProvider. On the entry's
// scope it offers "claim": the local operator names an issue and, unless
// Work already owns it, the action produces TypeClaimRequested on the
// entry's issue key, which Route turns into Work whatever the labels.
type Actions struct {
	Store    storage.Store
	Settings Settings
}

var _ extension.ActionProvider = Actions{}

func (a Actions) Actions(_ context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	if t.Work != nil || string(t.Scope.ID) != a.Settings.Scope {
		return nil, nil
	}
	return []extension.Action{{ID: ActionClaim, Label: "Claim an issue in " + a.Settings.TrackerRepo + " by hand", Prompt: "issue number"}}, nil
}

func refuseAction(code, message string) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, &extension.OperationError{Code: code, Message: message}
}

func (a Actions) Invoke(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	if c.Action != ActionClaim || c.Target.Work != nil || string(c.Target.Scope.ID) != a.Settings.Scope {
		return refuseAction("unknown_action", "GitHub offers no action "+c.Action+" here")
	}
	if c.Caller.Kind != extension.CallerOperator {
		return refuseAction("not_allowed", "only the local operator claims issues by hand")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(c.Input), "#"))
	if err != nil || n <= 0 {
		return refuseAction("invalid_input", "give an issue number, such as 12 or #12")
	}
	key := domain.CorrelationKey{Source: a.Settings.Namespace, ExternalID: IssueKey(n, "")}
	if a.Store != nil {
		var owner domain.Work
		err := a.Store.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
			var e error
			owner, e = tx.WorkByCorrelation(key)
			return e
		})
		switch {
		case err == nil:
			return refuseAction("already_claimed", fmt.Sprintf("%s#%d already has Work %s (%s)", a.Settings.TrackerRepo, n, owner.ID, owner.State))
		case !errors.Is(err, storage.ErrNotFound):
			return extension.ActionPlan{}, err
		}
	}
	data, _ := json.Marshal(map[string]int{"number": n})
	return extension.ActionPlan{
		Event:   domain.Event{Type: TypeClaimRequested, CorrelationKey: &key, Data: domain.Payload{Schema: "github.claim.v1", Data: data}},
		Message: fmt.Sprintf("claimed %s#%d", a.Settings.TrackerRepo, n),
	}, nil
}
