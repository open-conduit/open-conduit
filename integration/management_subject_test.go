package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// callerProvider offers "ack" on every target and grants it to anyone,
// remembering who asked.
type callerProvider struct {
	mu      sync.Mutex
	callers []extension.Caller
}

func (*callerProvider) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	return []extension.Action{{ID: "ack", Label: "Acknowledge"}}, nil
}

func (p *callerProvider) Invoke(_ context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	p.mu.Lock()
	p.callers = append(p.callers, c.Caller)
	p.mu.Unlock()
	return extension.ActionPlan{Event: domain.Event{Type: "fixture.acked"}, Message: "acked by " + c.Caller.String()}, nil
}

func (p *callerProvider) last() extension.Caller {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.callers) == 0 {
		return extension.Caller{}
	}
	return p.callers[len(p.callers)-1]
}

// TestManagementActForSubject checks a client acting for the person
// signed in to it (issue #687): only a credential given the right acts
// for a subject, and only for its one kind; a subject of that kind may
// invoke actions and nothing else; an operator subject has its own
// authority intersected with the client's; what it does is recorded as
// the subject's, through the client; and its request keys are its own.
func TestManagementActForSubject(t *testing.T) {
	h := newManagementHarnessWith(t, grantsConfig, nil)
	acks := &callerProvider{}
	h.actions = map[string]extension.ActionProvider{"fixture": acks}
	h.packages = map[string]bool{"slack": true}
	ctx := context.Background()
	owner := h.operatorClient()
	work := map[string]client.Work{}
	for _, scope := range []string{"team-a", "team-b"} {
		created, err := owner.CreateWork(ctx, client.WorkRequest{ScopeID: scope, Kind: "manual.task", Binding: "run"}, "make-"+scope)
		if err != nil {
			t.Fatal(err)
		}
		work[scope] = created.Work
	}
	login := func(name string, args ...string) (*client.Client, client.TokenFile) {
		t.Helper()
		path := filepath.Join(h.state, name+".token")
		h.mustCLI(nil, append([]string{"auth", "add", name, "--out", path}, args...)...)
		f, err := client.ReadTokenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(h.endpoint, f.Token)
		if err != nil {
			t.Fatal(err)
		}
		return c, f
	}
	refused := func(what string, err error, status int, code string) {
		t.Helper()
		var api *client.Error
		if !errors.As(err, &api) || api.Status != status || api.Code != code {
			t.Fatalf("%s: want %d %s, got %v", what, status, code, err)
		}
	}
	recorded := func(what, eventID, actor, via string) {
		t.Helper()
		e, err := owner.Event(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Actor != actor || e.Via != via {
			t.Fatalf("%s: recorded as %q via %q, want %q via %q", what, e.Actor, e.Via, actor, via)
		}
	}
	as := func(subject string) context.Context { return client.WithSubject(ctx, subject) }
	scopeA := client.ActionTarget{ScopeID: "team-a"}

	// Only a credential given the right acts for anyone.
	plain, _ := login("plain", "--role", "operate")
	_, err := plain.InvokeAction(as("plain:alice"), scopeA, "fixture", "ack", "", "p1")
	refused("a credential without act_for", err, 403, "forbidden_subject")
	_, err = owner.InvokeAction(as("operator:alice"), scopeA, "fixture", "ack", "", "p1")
	refused("the owner's own credential", err, 403, "forbidden_subject")

	// A package's kind of people are its own to vouch for.
	if _, errOut, code := h.cli("auth", "add", "fake-slack", "--act-for", "slack", "--out", filepath.Join(h.state, "fake-slack.token")); code == 0 || !strings.Contains(errOut, "act_for_taken") {
		t.Fatalf("act_for a package's kind: %d %s", code, errOut)
	}
	if _, err = owner.IssueOperator(ctx, client.OperatorRequest{Name: "bad", Role: "operate", ActFor: "Acme UI"}); !isStatus(err, 400) {
		t.Fatalf("act_for that isn't a kind: %v", err)
	}

	// A frontend acts for its own people: their actions are theirs,
	// through it, and its provider sees who.
	ui, _ := login("acme-ui", "--role", "operate", "--act-for", "acme-ui")
	var listed []client.Operator
	h.mustCLI(&listed, "auth", "operators")
	for _, o := range listed {
		if o.Name == "acme-ui" && o.ActFor != "acme-ui" {
			t.Fatalf("operators list: %+v", o)
		}
	}
	res, err := ui.InvokeAction(as("acme-ui:alice"), scopeA, "fixture", "ack", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if acks.last() != (extension.Caller{Kind: "acme-ui", ID: "alice"}) || res.Message != "acked by acme-ui:alice" {
		t.Fatalf("provider saw %+v", acks.last())
	}
	if res.Event.Actor != "acme-ui:alice" || res.Event.Via != "operator:acme-ui" {
		t.Fatalf("action recorded as %q via %q", res.Event.Actor, res.Event.Via)
	}
	recorded("alice's action", res.Event.ID, "acme-ui:alice", "operator:acme-ui")
	// Her key is hers: bob's same key is his own request, her replay is
	// her first, and the client's own is neither.
	again, err := ui.InvokeAction(as("acme-ui:alice"), scopeA, "fixture", "ack", "", "k1")
	if err != nil || again.Event.ID != res.Event.ID {
		t.Fatalf("alice's replay: %+v %v", again, err)
	}
	bob, err := ui.InvokeAction(as("acme-ui:bob"), scopeA, "fixture", "ack", "", "k1")
	if err != nil || bob.Event.ID == res.Event.ID || bob.Event.Actor != "acme-ui:bob" {
		t.Fatalf("bob's request under alice's key: %+v %v", bob, err)
	}
	own, err := ui.InvokeAction(ctx, scopeA, "fixture", "ack", "", "k1")
	if err != nil || own.Event.ID == res.Event.ID || own.Event.ID == bob.Event.ID || own.Event.Actor != "operator:acme-ui" || own.Event.Via != "" {
		t.Fatalf("the client's own request under the key: %+v %v", own, err)
	}

	// A subject without operator authority may only invoke actions: no
	// control or read falls back to the client's own authority.
	_, err = ui.CancelWork(as("acme-ui:alice"), work["team-a"].ID, "", "by alice", "c1")
	refused("alice cancels", err, 403, "subject_not_permitted")
	_, err = ui.Works(as("acme-ui:alice"), nil)
	refused("alice lists Work", err, 403, "subject_not_permitted")
	_, err = ui.Rotate(as("acme-ui:alice"), "")
	refused("rotating the client's credential as alice", err, 403, "subject_not_permitted")
	// Only its one kind.
	_, err = ui.InvokeAction(as("operator:bea"), scopeA, "fixture", "ack", "", "k2")
	refused("acme-ui asserts an operator", err, 403, "forbidden_subject")
	_, err = ui.InvokeAction(as("slack:U123"), scopeA, "fixture", "ack", "", "k2")
	refused("acme-ui asserts a Slack user", err, 403, "forbidden_subject")
	_, err = ui.InvokeAction(as("acme-ui"), scopeA, "fixture", "ack", "", "k2")
	refused("a subject without an id", err, 400, "malformed")

	// A console trusted to assert operators acts for each with that
	// operator's authority within its own.
	_, beaFile := login("bea", "--grants", "team-a=operate")
	console, _ := login("console", "--role", "operate", "--act-for", "operator")
	viewer, _ := login("viewer", "--role", "view", "--act-for", "operator")
	login("dan", "--role", "admin")
	page, err := console.Works(as("operator:bea"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range page.Items {
		if w.ScopeID != "team-a" {
			t.Fatalf("bea through the console sees %s's Work", w.ScopeID)
		}
	}
	if len(page.Items) == 0 {
		t.Fatal("bea sees no Work through the console")
	}
	_, err = console.CancelWork(as("operator:bea"), work["team-b"].ID, "", "by bea", "c1")
	refused("bea cancels team-b's Work", err, 403, "forbidden_scope")
	cancelled, err := console.CancelWork(as("operator:bea"), work["team-a"].ID, "", "by bea", "c1")
	if err != nil {
		t.Fatal(err)
	}
	recorded("bea's cancel", cancelled.EventID, "operator:bea", "operator:console")
	// An admin through an operate client is operate: no config apply, no
	// secrets.
	_, err = console.Secrets(as("operator:dan"))
	refused("dan's secrets through an operate console", err, 403, "forbidden_role")
	// An operate subject through a view client may only read.
	_, err = viewer.CancelWork(as("operator:bea"), work["team-a"].ID, "", "by bea", "c2")
	refused("bea's cancel through a view console", err, 403, "forbidden_role")
	if _, err = viewer.Works(as("operator:bea"), nil); err != nil {
		t.Fatalf("bea reads through a view console: %v", err)
	}
	// Nobody acts for the owner, an operator with no credential, or an
	// identity provider's operator by name alone.
	_, err = console.Works(as("operator:local"), nil)
	refused("the owner as a subject", err, 403, "forbidden_subject")
	_, err = console.Works(as("operator:nobody"), nil)
	refused("an operator without a credential", err, 403, "forbidden_subject")
	_, err = console.Works(as("operator:corp:alice@example.com"), nil)
	refused("an issuer's operator by name", err, 403, "forbidden_subject")

	// By the subject's own credential: whoever it authenticates as.
	byToken := client.WithSubjectToken(ctx, beaFile.Token)
	if page, err = console.Works(byToken, nil); err != nil || len(page.Items) == 0 || page.Items[0].ScopeID != "team-a" {
		t.Fatalf("bea by her credential: %+v %v", page, err)
	}
	_, err = console.Works(client.WithSubjectToken(ctx, "not-a-credential"), nil)
	refused("a credential nobody issued", err, 401, "subject_unauthenticated")
	_, err = ui.Works(byToken, nil)
	refused("acme-ui sends an operator's credential", err, 403, "forbidden_subject")

	// Revoking bea's credential stops anyone acting for her.
	if _, err = owner.Revoke(ctx, beaFile.TokenID, "left", "r1"); err != nil {
		t.Fatal(err)
	}
	_, err = console.Works(as("operator:bea"), nil)
	refused("bea after her revocation", err, 403, "forbidden_subject")
	_, err = console.Works(byToken, nil)
	refused("bea's revoked credential", err, 401, "subject_unauthenticated")
}
