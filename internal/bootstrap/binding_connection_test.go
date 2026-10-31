package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

type connTestExecutor struct{}

func (connTestExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: "mcp"}
}
func (connTestExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, errors.New("unused")
}
func (connTestExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, errors.New("unused")
}

// bindingConnectionDaemon is a daemon whose current generation has an
// `acme` extension entry in scope work naming acme-main, a binding there
// naming acme-reviewer, one naming the other-typed other-bot and one naming
// none; scope bare whose binding names acme-reviewer but which has no acme
// entry; and scope unbound with an acme entry whose authorization grants
// acme-reviewer though no binding names it.
func bindingConnectionDaemon(t *testing.T) *daemon {
	t.Helper()
	binding := func(name, conn string) map[string]any {
		b := map[string]any{"name": name, "executor": "agent", "requires": map[string]any{"toolbelt": "mcp"}, "operations": []any{}, "secret_names": []any{}, "config": map[string]any{}}
		if conn != "" {
			b["connection"] = conn
		}
		return b
	}
	entry := map[string]any{"name": "acme", "type": "acme", "connection": "acme-main", "config": map[string]any{}, "secret_names": []any{}}
	scope := func(id string, conns []any, bindings, extensions []any) map[string]any {
		return map[string]any{"id": id, "name": id,
			"authorization": map[string]any{"connections": conns, "operations": []any{}, "read_scopes": []any{}},
			"bindings":      bindings, "routes": []any{}, "extensions": extensions, "secret_refs": map[string]any{}}
	}
	conn := func(id, typ, env string) map[string]any {
		return map[string]any{"id": id, "type": typ, "config": map[string]any{"account": id}, "secret_refs": map[string]any{"token": "env:" + env}}
	}
	doc := map[string]any{"version": 1, "settings": map[string]any{}, "connections": []any{
		conn("acme-main", "acme", "ACME_MAIN"), conn("acme-reviewer", "acme", "ACME_REVIEWER"), conn("acme-spare", "acme", "ACME_SPARE"), conn("other-bot", "other", "OTHER_BOT"),
	}, "scopes": []any{
		scope("work", []any{"acme-main", "acme-reviewer", "other-bot"}, []any{binding("writer", ""), binding("reviewer", "acme-reviewer"), binding("other", "other-bot")}, []any{entry}),
		scope("bare", []any{"acme-reviewer"}, []any{binding("reviewer", "acme-reviewer")}, []any{}),
		scope("unbound", []any{"acme-main", "acme-reviewer"}, []any{binding("writer", "")}, []any{entry}),
	}}
	raw, _ := json.Marshal(doc)
	reg := config.Registry{
		Executors:   map[string]extension.Executor{"agent": connTestExecutor{}},
		Extensions:  map[string]config.ExtensionType{"acme": {}},
		Connections: map[string]func(json.RawMessage) error{"acme": nil, "other": nil},
	}
	s, err := config.Validate(raw, reg)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	env := map[string]string{"ACME_MAIN": "main-token", "ACME_REVIEWER": "reviewer-token", "ACME_SPARE": "spare-token", "OTHER_BOT": "other-token"}
	d := &daemon{resolver: &config.Resolver{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}}
	d.external.current.Store(&generationSnapshot{s: s, gen: 1})
	return d
}

// TestBindingConnectionSecrets: the host resolver, which in-process
// extensions and installed packages share, lets the extension owning a
// connection's type resolve the secrets of a connection a binding in the
// scope names, as well as its entries' own, and refuses a connection that
// isn't granted, that no binding or entry names, of another type, or in a
// scope without one of the extension's entries.
func TestBindingConnectionSecrets(t *testing.T) {
	d := bindingConnectionDaemon(t)
	acme := d.scopedSecrets(map[string]bool{"acme": true}, map[string]bool{"acme": true})
	ctx := context.Background()
	for conn, want := range map[string]string{"acme-main": "main-token", "acme-reviewer": "reviewer-token"} {
		got, err := acme(ctx, "work", conn, []string{"token"})
		if err != nil || got["token"] != want {
			t.Fatalf("%s: %v %v", conn, got, err)
		}
	}
	refused := []struct {
		name, scope, conn string
		names             []string
	}{
		{"not granted or named", "work", "acme-spare", []string{"token"}},
		{"of another type", "work", "other-bot", []string{"token"}},
		{"granted but named by no binding", "unbound", "acme-reviewer", []string{"token"}},
		{"named by a binding where the extension has no entry", "bare", "acme-reviewer", []string{"token"}},
		{"a name the connection does not have", "work", "acme-reviewer", []string{"password"}},
		{"an unknown connection", "work", "nope", []string{"token"}},
	}
	for _, c := range refused {
		if got, err := acme(ctx, domain.ID(c.scope), c.conn, c.names); !errors.Is(err, extension.ErrUnauthorized) {
			t.Errorf("%s: %v %v", c.name, got, err)
		}
	}
	// The other type's owner gets its connection; acme's are refused to it.
	other := d.scopedSecrets(map[string]bool{"acme": true}, map[string]bool{"other": true})
	if got, err := other(ctx, "work", "other-bot", []string{"token"}); err != nil || got["token"] != "other-token" {
		t.Fatalf("other-bot to its owner: %v %v", got, err)
	}
	if _, err := other(ctx, "work", "acme-reviewer", []string{"token"}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("acme-reviewer to the other type's owner: %v", err)
	}
}

// TestBindingConnectionConfigure: configure sends each entry the
// connections the scope's bindings name, of the extension's connection
// types, with their config and secret names; a binding without a
// connection adds nothing.
func TestBindingConnectionConfigure(t *testing.T) {
	d := bindingConnectionDaemon(t)
	params := d.configureParams(map[string]bool{"acme": true}, map[string]bool{"acme": true})
	got := map[string]protocol.ConfigEntry{}
	for _, e := range params.Entries {
		got[e.ScopeID] = e
	}
	work := got["work"]
	if work.Connection == nil || work.Connection.ID != "acme-main" {
		t.Fatalf("the entry's own connection: %+v", work.Connection)
	}
	want := []protocol.ConnectionEntry{{ID: "acme-reviewer", Type: "acme", Config: json.RawMessage(`{"account":"acme-reviewer"}`), Secrets: []string{"token"}}}
	if !reflect.DeepEqual(work.BindingConnections, want) {
		t.Fatalf("binding connections in work: %+v", work.BindingConnections)
	}
	if u, ok := got["unbound"]; !ok || len(u.BindingConnections) != 0 {
		t.Fatalf("a scope whose bindings name no connection: %+v", u)
	}
	if _, ok := got["bare"]; ok {
		t.Fatal("a scope without the extension's entry was configured")
	}
	other := d.configureParams(map[string]bool{"acme": true}, map[string]bool{"other": true})
	for _, e := range other.Entries {
		if e.ScopeID == "work" && (len(e.BindingConnections) != 1 || e.BindingConnections[0].ID != "other-bot") {
			t.Fatalf("the other type's owner: %+v", e.BindingConnections)
		}
	}
}
