package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

type secretActions struct {
	list []extension.Action
	err  error
}

func (p secretActions) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	return p.list, p.err
}

func (secretActions) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, nil
}

// An action asking for a secret takes only the secret's name, so a value
// can't reach the provider or an Event from any surface. The plain action
// is the negative control: its input is untouched.
func TestSecretActionRefusesAValue(t *testing.T) {
	const canary = "canary-secret-value"
	p := secretActions{list: []extension.Action{{ID: "give", Label: "Token", Prompt: "Paste", Secret: "gh_token"}, {ID: "say", Label: "Say", Prompt: "Text"}}}
	ctx := context.Background()
	err := refuseSecretInput(ctx, p, extension.ActionCall{Action: "give", Input: canary})
	var oe *extension.OperationError
	if !errors.As(err, &oe) || oe.Code != "secret_in_input" || strings.Contains(err.Error(), canary) {
		t.Fatalf("a value as input: %v", err)
	}
	if err := refuseSecretInput(ctx, p, extension.ActionCall{Action: "give", Input: "gh_token"}); err != nil {
		t.Fatalf("the name as input: %v", err)
	}
	if err := refuseSecretInput(ctx, p, extension.ActionCall{Action: "say", Input: canary}); err != nil {
		t.Fatalf("a plain action: %v", err)
	}
	if err := refuseSecretInput(ctx, secretActions{err: errors.New("down")}, extension.ActionCall{Action: "give", Input: canary}); err == nil {
		t.Fatal("a provider that can't list must fail closed")
	}
}
