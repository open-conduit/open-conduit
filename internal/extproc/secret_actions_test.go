package extproc

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestOfferedActionsRefuseAnUndeclaredSecret checks that a package can't ask
// for a secret without declaring it, and that the refusal fails the listing:
// the check keeping a value out of an action's input reads that list.
func TestOfferedActionsRefuseAnUndeclaredSecret(t *testing.T) {
	listed := protocol.WorkActionsResult{Actions: []protocol.Action{
		{ID: "say", Label: "Say"},
		{ID: "give", Label: "Token", Prompt: "Paste", Secret: "gh_token"},
	}}
	var m protocol.Manifest
	m.Name = "acme"
	m.Provides.Actions = true

	got, err := offeredActions(m, listed)
	var oe *extension.OperationError
	if !errors.As(err, &oe) || oe.Code != "undeclared_secret_action" || !strings.Contains(oe.Message, "give") {
		t.Fatalf("undeclared secret action: %v, want undeclared_secret_action naming it", err)
	}
	if got != nil {
		t.Fatalf("undeclared secret action listed %v, want nothing", got)
	}

	m.Provides.SecretActions = true
	got, err = offeredActions(m, listed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Secret != "gh_token" {
		t.Fatalf("declared secret action: %+v", got)
	}
}

// TestOfferedActionsWithoutASecretNeedNoDeclaration is the control: a package
// with ordinary actions is unaffected.
func TestOfferedActionsWithoutASecretNeedNoDeclaration(t *testing.T) {
	var m protocol.Manifest
	m.Provides.Actions = true
	got, err := offeredActions(m, protocol.WorkActionsResult{Actions: []protocol.Action{{ID: "say", Label: "Say"}}})
	if err != nil || len(got) != 1 {
		t.Fatalf("ordinary action: %+v, %v", got, err)
	}
}
