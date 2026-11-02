package extproc

import (
	"context"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// An installed package's gate needs the operation.gate permission, and is
// asked only about calls in scopes the package is configured for.
func TestInstalledGateNeedsPermissionAndScope(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{1}, Name: "approvals", Version: "1", Command: []string{"./approvals"}, Provides: protocol.Provides{OperationGate: true}}
	s, err := NewService(m, Deps{Now: time.Now, Scopes: func() []domain.ID { return []domain.ID{"mine"} }})
	if err != nil {
		t.Fatal(err)
	}
	if g := (&Remote{m: m, s: s}).Registration().Gate; g != nil {
		t.Fatal("a package without the operation.gate permission got a gate")
	}
	if s.enabled["operation.gate"] {
		t.Fatal("operation.gate is enabled without its permission")
	}
	m.Permissions = []string{protocol.PermissionOperationGate}
	if s, err = NewService(m, Deps{Now: time.Now, Scopes: func() []domain.ID { return []domain.ID{"mine"} }}); err != nil {
		t.Fatal(err)
	}
	g := (&Remote{m: m, s: s}).Registration().Gate
	if g == nil || !s.enabled["operation.gate"] {
		t.Fatal("a package with the permission got no gate")
	}
	// A call in another scope never reaches the process (there is none
	// here: reaching it would panic).
	plan, err := g(context.Background(), extension.Authority{ScopeID: "theirs", WorkID: "w"}, extension.GateCall{Operation: "github.pr.create", Owner: "github"})
	if plan != nil || err != nil {
		t.Fatalf("a call outside the package's scopes was gated: %+v %v", plan, err)
	}
}
