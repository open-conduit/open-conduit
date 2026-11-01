package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/protocol"
)

func doctorChecks(t *testing.T, x *gh.Extension) (ordinary, deep func(context.Context) inproc.CheckResult) {
	t.Helper()
	for _, c := range x.Checks() {
		run := c.Run
		f := func(ctx context.Context) inproc.CheckResult { return run(ctx, protocol.CheckConfig{}) }
		switch {
		case c.Deep:
			deep = f
		case !c.Offline:
			ordinary = f
		}
	}
	if ordinary == nil || deep == nil {
		t.Fatalf("checks: %+v", x.Checks())
	}
	return ordinary, deep
}

// TestExtensionDoctor: with no entries both checks say so; the ordinary
// check reports the host's registrations without a service call, and the
// connection test signs in as the entry's connection and as each other
// `github` connection the scope's bindings name, with secrets resolved
// through the host's scoped resolver.
func TestExtensionDoctor(t *testing.T) {
	ordinary, deep := doctorChecks(t, gh.NewExtension(nil))
	if c := ordinary(context.Background()); c.Status != inproc.CheckInfo || c.Code != "not_configured" {
		t.Fatalf("no entries: %+v", c)
	}
	if c := deep(context.Background()); c.Status != inproc.CheckInfo || c.Code != "not_configured" {
		t.Fatalf("no entries (deep): %+v", c)
	}

	fake := githubtest.New("octo/widgets", "tok", "rev")
	t.Cleanup(fake.Close)
	fake.SetPermissions(map[string]bool{"pull": true, "push": true})
	settings, err := gh.ParseExtension([]byte(`{"namespace":"github/w","scope":"eng","repo":"octo/widgets"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	view := gh.View{
		Entries: []gh.ViewEntry{{Name: "github/eng/main", Scope: "eng", Connection: "owner", Settings: settings,
			Scan: &gh.Scan{LastError: "rate_limited", Failures: 2}, Projection: "label_failed"}},
		Failed: map[string]string{"github/x": "github_connection_type"},
	}
	x := gh.NewExtension(func(context.Context) gh.View { return view })
	tokens := map[string]string{"owner": "tok", "reviewer": "rev", "stranger": "nope"}
	var asked []string
	if err := x.Start(context.Background(), inproc.Host{Now: fake.Now, Secrets: func(_ context.Context, scope domain.ID, conn string, names []string) (map[string]string, error) {
		asked = append(asked, string(scope)+"/"+conn)
		tok, ok := tokens[conn]
		if scope != "eng" || !ok || len(names) != 1 || names[0] != gh.SecretToken {
			return nil, errors.New("unauthorized")
		}
		return map[string]string{names[0]: tok}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	api := json.RawMessage(`{"api_url":"` + fake.URL + `"}`)
	conn := func(id, typ string) protocol.ConnectionEntry {
		return protocol.ConnectionEntry{ID: id, Type: typ, Config: api, Secrets: []string{gh.SecretToken}}
	}
	owner := conn("owner", gh.Type)
	x.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "eng", Type: gh.Type, Name: "main", Connection: &owner,
		BindingConnections: []protocol.ConnectionEntry{owner, conn("reviewer", gh.Type), conn("stranger", gh.Type), conn("chat", "slack")}}}})
	ordinary, deep = doctorChecks(t, x)

	c := ordinary(context.Background())
	items := strings.Join(c.Items, "\n")
	if c.Status != inproc.CheckFail || c.Code != "github_connection_type" || !strings.Contains(items, "github/x: not registered (github_connection_type)") ||
		!strings.Contains(items, "github/w: reconcile failing (rate_limited, 2 failures); projection label_failed") || len(asked) != 0 {
		t.Fatalf("ordinary: %+v (secrets asked %v)", c, asked)
	}

	c = deep(context.Background())
	items = strings.Join(c.Items, "\n")
	for _, want := range []string{"github/w: pull=true push=true admin=false", "github/w: binding connection reviewer: authenticated", "github/w: binding connection stranger: credential_rejected"} {
		if !strings.Contains(items, want) {
			t.Fatalf("deep: missing %q in %+v", want, c)
		}
	}
	if c.Status != inproc.CheckFail || c.Code != "credential_rejected" || strings.Contains(items, "chat") || strings.Contains(items, "connection owner") {
		t.Fatalf("deep: %+v", c)
	}

	// The entry's own connection unavailable is reported, not skipped.
	x.Configure(context.Background(), protocol.ConfigureParams{})
	if c := deep(context.Background()); c.Code != "credential_unavailable" || len(c.Items) != 1 {
		t.Fatalf("no configured connection: %+v", c)
	}
}

// TestExtensionDoctorLiveWriter: the connection test verifies through the
// client the registered entry runs with, and warns restart_required when the
// connection config has since moved writes to a bot App and git to the bot,
// rather than reporting the configured bot writer as authenticated (#276).
func TestExtensionDoctorLiveWriter(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok", "rev")
	t.Cleanup(fake.Close)
	fake.SetPermissions(map[string]bool{"pull": true, "push": true})
	settings, err := gh.ParseExtension([]byte(`{"namespace":"github/w","scope":"eng","repo":"octo/widgets"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	secrets := func(_ context.Context, name string) (string, error) {
		if name != gh.SecretToken {
			return "", gh.ErrCredential
		}
		return "tok", nil
	}
	live, err := gh.NewClient(gh.ConnectionConfig{APIURL: fake.URL, Auth: "token"}, secrets, gh.ClientOptions{Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	view := gh.View{Entries: []gh.ViewEntry{{Name: "github/eng/main", Scope: "eng", Connection: "owner", Settings: settings, Client: live}}}
	x := gh.NewExtension(func(context.Context) gh.View { return view })
	if err := x.Start(context.Background(), inproc.Host{Now: fake.Now, Secrets: func(_ context.Context, _ domain.ID, _ string, names []string) (map[string]string, error) {
		return nil, errors.New("the configured client must not be called")
	}}); err != nil {
		t.Fatal(err)
	}
	configure := func(cfg string) {
		owner := protocol.ConnectionEntry{ID: "owner", Type: gh.Type, Config: json.RawMessage(cfg), Secrets: []string{gh.SecretToken, gh.SecretBotPrivateKey}}
		x.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: "eng", Type: gh.Type, Name: "main", Connection: &owner}}})
	}
	_, deep := doctorChecks(t, x)

	configure(`{"api_url":"` + fake.URL + `","bot_app_id":"42","git":"bot"}`)
	c := deep(context.Background())
	items := strings.Join(c.Items, "\n")
	want := "github/w: restart_required: writes still go out as the connection's token, the config names bot App 42; git still works as the owner, the config says the bot"
	if c.Status != inproc.CheckWarn || c.Code != "restart_required" || !strings.Contains(items, want) ||
		!strings.Contains(items, "github/w: pull=true push=true admin=false") || strings.Contains(items, "bot writer authenticated") {
		t.Fatalf("drifted: %+v", c)
	}

	// Once the config matches the running client again, nothing is raised.
	configure(`{"api_url":"` + fake.URL + `"}`)
	if c := deep(context.Background()); c.Status != inproc.CheckOK || c.Code != "verified" || strings.Contains(strings.Join(c.Items, "\n"), "restart_required") {
		t.Fatalf("in step: %+v", c)
	}
}
