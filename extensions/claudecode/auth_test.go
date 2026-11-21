package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
	"github.com/open-conduit/open-conduit/protocol"
)

// fakeAPI is an endpoint that accepts one credential and records what it
// was asked. A refusal carries text that must never reach doctor.
type fakeAPI struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

const providerText = "provider says: key sk-wrong-123 is invalid"

func newFakeAPI(t *testing.T, accept func(r *http.Request) bool) *fakeAPI {
	f := &fakeAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.Path)
		f.mu.Unlock()
		if !accept(r) {
			http.Error(w, providerText, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func authBinding(name string, cfg map[string]any, secrets ...string) protocol.CheckBinding {
	raw, _ := json.Marshal(cfg)
	return protocol.CheckBinding{Name: name, Executor: claudecode.Name, Config: raw, SecretNames: secrets}
}

// TestAuthCheck: with secrets checked, each claude-code binding's
// credential is tried once against the endpoint its runs reach, and
// reported by a safe code with no value and no provider text.
func TestAuthCheck(t *testing.T) {
	anthropic := newFakeAPI(t, func(r *http.Request) bool {
		switch r.URL.Path {
		case "/v1/models":
			return r.Header.Get("x-api-key") == "sk-good"
		case "/api/oauth/usage":
			return r.Header.Get("Authorization") == "Bearer oauth-good" || r.Header.Get("Authorization") == "Bearer login-token"
		}
		return false
	})
	gateway := newFakeAPI(t, func(r *http.Request) bool {
		return r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer gw-key"
	})
	values := map[string]map[string]string{
		"good":     {"ANTHROPIC_API_KEY": "sk-good"},
		"wrong":    {"ANTHROPIC_API_KEY": "sk-wrong-123"},
		"oauth":    {"tok": "oauth-good"},
		"gateway":  {"gw": "gw-key"},
		"boxed":    {"ANTHROPIC_API_KEY": "sk-good"},
		"no-login": {},
	}
	var mu sync.Mutex
	var asked []string
	x := claudecode.NewExtension()
	x.StubAuth(anthropic.URL, anthropic.Client(), func(_ context.Context, dir string) (string, error) {
		if dir == "/nobody" {
			return "", errors.New("no login")
		}
		return "login-token", nil
	}, func(_ context.Context, scope domain.ID, binding string, names []string) (map[string]string, error) {
		mu.Lock()
		asked = append(asked, string(scope)+"/"+binding+":"+strings.Join(names, ","))
		mu.Unlock()
		if binding == "gone" {
			return nil, errors.New("unresolved")
		}
		return values[binding], nil
	})
	boxed := authBinding("boxed", map[string]any{}, "ANTHROPIC_API_KEY")
	boxed.Isolation = protocol.IsolationContainer
	bare := authBinding("bare-box", map[string]any{})
	bare.Isolation = protocol.IsolationContainer
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		authBinding("good", map[string]any{}, "ANTHROPIC_API_KEY"),
		authBinding("wrong", map[string]any{}, "ANTHROPIC_API_KEY"),
		authBinding("oauth", map[string]any{"secret_env": map[string]string{"tok": "CLAUDE_CODE_OAUTH_TOKEN"}}, "tok"),
		authBinding("gateway", map[string]any{"provider": map[string]any{"base_url": gateway.URL, "auth_token_secret": "gw"}}, "gw"),
		authBinding("login", map[string]any{}),
		authBinding("no-login", map[string]any{"env": map[string]string{"CLAUDE_CONFIG_DIR": "/nobody"}}),
		authBinding("gone", map[string]any{}, "ANTHROPIC_API_KEY"),
		authBinding("bedrock", map[string]any{"env": map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}}),
		boxed, bare,
		{Name: "shell", Executor: "shell"},
	}}}}
	got := x.Checks()[1].Run(context.Background(), cfg)
	if got.Name != claudecode.AuthCheckName || got.Status != inproc.CheckFail || got.Code != "unauthorised" {
		t.Fatalf("check %+v, want a failure", got)
	}
	host := strings.TrimPrefix(anthropic.URL, "http://")
	gwHost := strings.TrimPrefix(gateway.URL, "http://")
	want := []string{
		"eng/good: ok (" + host + ", API key)",
		"eng/wrong: unauthorised (" + host + ", API key)",
		"eng/oauth: ok (" + host + ", OAuth token)",
		"eng/gateway: ok (" + gwHost + ", bearer token)",
		"eng/login: ok (" + host + ", Claude login)",
		"eng/no-login: not_configured (no selected credential and no Claude login on the daemon's machine)",
		"eng/gone: secret_unresolved (a selected secret didn't resolve)",
		"eng/bedrock: not_checked (Amazon Bedrock credentials are the cloud's to verify)",
		"eng/boxed: ok (" + host + ", API key)",
		"eng/bare-box: not_configured (no credential: a container binding signs in with a selected secret)",
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items\n%q\nwant\n%q", got.Items, want)
	}
	for _, item := range got.Items {
		if strings.Contains(item, "sk-") || strings.Contains(item, "provider says") || strings.Contains(item, "gw-key") || strings.Contains(item, "oauth-good") {
			t.Errorf("item %q carries a value or the provider's text", item)
		}
	}
	// The gateway binding went to its provider, and the default endpoint's
	// fake never saw it: five requests there, from good, wrong, oauth,
	// login and boxed.
	if g := gateway.requests(); !slices.Equal(g, []string{"/v1/models"}) {
		t.Errorf("gateway saw %q, want one models request", g)
	}
	if a := anthropic.requests(); len(a) != 5 {
		t.Errorf("anthropic saw %q, want five requests", a)
	}
	if slices.ContainsFunc(asked, func(s string) bool { return strings.HasPrefix(s, "eng/shell") || strings.HasPrefix(s, "eng/login") }) {
		t.Errorf("secrets asked for %q, want only the bindings that select some", asked)
	}
}

// TestAuthCheckGrantCredential: a grant that selects a credential can supply
// the one a binding otherwise lacks. Not stored yet, it says nothing, because
// a grant is optional. Refused, it is reported, because its Work would fail.
func TestAuthCheckGrantCredential(t *testing.T) {
	api := newFakeAPI(t, func(r *http.Request) bool { return r.Header.Get("x-api-key") == "sk-good" })
	x := claudecode.NewExtension()
	x.StubAuth(api.URL, api.Client(), func(_ context.Context, dir string) (string, error) {
		if dir == "/nobody" {
			return "", errors.New("no login")
		}
		return "login-token", nil
	}, func(_ context.Context, _ domain.ID, binding string, _ []string) (map[string]string, error) {
		switch binding {
		case "granted":
			return map[string]string{"ANTHROPIC_API_KEY": "sk-good"}, nil
		case "refused":
			return map[string]string{"ANTHROPIC_API_KEY": "sk-wrong-123"}, nil
		}
		return nil, errors.New("unresolved")
	})
	noLogin := map[string]any{"env": map[string]string{"CLAUDE_CONFIG_DIR": "/nobody"}}
	with := func(name string) protocol.CheckBinding {
		b := authBinding(name, noLogin)
		b.Grants = []protocol.CheckBindingGrant{{Name: "model", Config: b.Config, SecretNames: []string{"ANTHROPIC_API_KEY"}}}
		return b
	}
	got := x.Checks()[1].Run(context.Background(), protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		with("granted"), with("not-stored"), with("refused"),
	}}}})
	host := strings.TrimPrefix(api.URL, "http://")
	want := []string{
		"eng/granted: ok (with grant model: " + host + ", API key)",
		"eng/not-stored: not_configured (no selected credential and no Claude login on the daemon's machine)",
		"eng/refused: unauthorised (with grant model: " + host + ", API key)",
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items\n%q\nwant\n%q", got.Items, want)
	}
	for _, item := range got.Items {
		if strings.Contains(item, "sk-") {
			t.Errorf("item %q carries a value", item)
		}
	}
}

// TestAuthCheckNeedsSecrets: the credential check is the deep one, which
// doctor runs only with --check-secrets; the plain check makes no request.
func TestAuthCheckNeedsSecrets(t *testing.T) {
	api := newFakeAPI(t, func(*http.Request) bool { return true })
	x := claudecode.NewExtension()
	x.StubAuth(api.URL, api.Client(), func(context.Context, string) (string, error) { return "login-token", nil },
		func(context.Context, domain.ID, string, []string) (map[string]string, error) {
			t.Error("the plain check resolved a secret")
			return nil, nil
		})
	x.StubChecks(nil, func(context.Context, string) (string, error) { return "2.1.0", nil })
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{authBinding("agent", map[string]any{}, "ANTHROPIC_API_KEY")}}}}
	for _, check := range x.Checks() {
		if !check.Deep {
			check.Run(context.Background(), cfg)
		}
	}
	if seen := api.requests(); len(seen) != 0 {
		t.Errorf("plain doctor made requests %q", seen)
	}
}

// TestAuthCheckHungEndpoint: bindings are tried at once, each within its
// own budget, so an endpoint that never answers is reported unreachable
// without costing the bindings after it their results, and the items keep
// the config's order.
func TestAuthCheckHungEndpoint(t *testing.T) {
	// The stuck endpoint answers only once the fine one has been asked, so
	// bindings tried one after another would leave it hanging to its budget
	// and the fine one unasked: overlapped records that they weren't.
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	var overlapped atomic.Bool
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-reached:
			overlapped.Store(true)
			<-r.Context().Done()
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); hung.Close() })
	good := newFakeAPI(t, func(*http.Request) bool {
		once.Do(func() { close(reached) })
		return true
	})
	x := claudecode.NewExtension()
	x.SetProbeBudget(200 * time.Millisecond)
	x.StubAuth(good.URL, good.Client(), nil, func(_ context.Context, _ domain.ID, _ string, _ []string) (map[string]string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-good"}, nil
	})
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		authBinding("stuck", map[string]any{"env": map[string]string{"ANTHROPIC_BASE_URL": hung.URL}}, "ANTHROPIC_API_KEY"),
		authBinding("fine", map[string]any{}, "ANTHROPIC_API_KEY"),
	}}}}
	got := x.Checks()[1].Run(context.Background(), cfg)
	host := strings.TrimPrefix(good.URL, "http://")
	want := []string{"eng/stuck: unreachable (" + strings.TrimPrefix(hung.URL, "http://") + ", API key)", "eng/fine: ok (" + host + ", API key)"}
	if got.Status != inproc.CheckWarn || got.Code != "unreachable" || !slices.Equal(got.Items, want) {
		t.Fatalf("check %+v, want items %q", got, want)
	}
	if !overlapped.Load() {
		t.Fatal("the fine binding was asked only after the stuck one gave up: bindings must be tried at once")
	}
}
