package codexext_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/codex"
	codexext "github.com/open-conduit/open-conduit/extensions/codex/pkg"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// fakeAPI accepts one bearer token on /models and records each request.
// A refusal carries text that must never reach doctor.
func fakeAPI(t *testing.T, key string) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "provider says: sk-wrong is invalid", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(s.Close)
	return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func binding(name string, cfg map[string]any, isolation string, secrets ...string) protocol.CheckBinding {
	raw, _ := json.Marshal(cfg)
	return protocol.CheckBinding{Name: name, Executor: codex.Name, Config: raw, SecretNames: secrets, Isolation: isolation}
}

// TestAuthCheck: codex-auth, which only doctor --check-secrets runs,
// resolves each binding's secrets over the protocol and lists the models of
// the endpoint the binding reaches with its key: the provider's, never the
// default's, for a binding with a provider. A wrong key is unauthorised,
// with no value and no provider text.
func TestAuthCheck(t *testing.T) {
	openai, openaiSeen := fakeAPI(t, "sk-good")
	gateway, gatewaySeen := fakeAPI(t, "gw-key")
	base := map[string]string{"OPENAI_BASE_URL": openai.URL + "/v1"}
	h := sdktest.New(t, codexext.Build(func() string { return t.TempDir() }),
		sdktest.WithBindingSecrets("eng", "good", map[string]string{"openai": "sk-good"}),
		sdktest.WithBindingSecrets("eng", "wrong", map[string]string{"openai": "sk-wrong"}),
		sdktest.WithBindingSecrets("eng", "gateway", map[string]string{"gw": "gw-key"}),
		sdktest.WithBindingSecrets("eng", "boxed", map[string]string{"openai": "sk-good"}))
	m := codexext.Manifest()
	if !slices.ContainsFunc(m.Provides.Checks, func(c protocol.CheckDecl) bool { return c.Name == codex.AuthCheckName && c.Deep && !c.Offline }) {
		t.Fatalf("checks %+v, want a deep codex-auth", m.Provides.Checks)
	}
	key := map[string]any{"env": base, "secret_env": map[string]string{"openai": "CODEX_API_KEY"}}
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		binding("good", key, "", "openai"),
		binding("wrong", key, "", "openai"),
		binding("gateway", map[string]any{"env": base, "provider": map[string]any{"id": "gw", "base_url": gateway.URL + "/api/v1", "api_key_secret": "gw"}}, "", "gw"),
		binding("boxed", key, protocol.IsolationContainer, "openai"),
		binding("login", map[string]any{}, ""),
		binding("bare-box", map[string]any{}, protocol.IsolationContainer),
		{Name: "other", Executor: "shell"},
	}}}}
	got, err := h.CheckRun(context.Background(), protocol.CheckRunParams{Check: codex.AuthCheckName, Deep: true, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Status != "fail" || got.Code != "unauthorised" {
		t.Fatalf("check %+v, want an unauthorised failure", got)
	}
	host := strings.TrimPrefix(openai.URL, "http://")
	want := []string{
		"eng/good: ok (" + host + ", CODEX_API_KEY)",
		"eng/wrong: unauthorised (" + host + ", CODEX_API_KEY)",
		"eng/gateway: ok (" + strings.TrimPrefix(gateway.URL, "http://") + ", provider key)",
		"eng/boxed: ok (" + host + ", CODEX_API_KEY)",
		"eng/login: not_checked (signs in with `codex login` on the daemon's machine, which has no free check (run `codex login status`))",
		"eng/bare-box: not_configured (no credential: a container binding signs in with a selected secret)",
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items\n%q\nwant\n%q", got.Items, want)
	}
	for _, item := range got.Items {
		if strings.Contains(item, "sk-") || strings.Contains(item, "gw-key") || strings.Contains(item, "provider says") {
			t.Errorf("item %q carries a value or the provider's text", item)
		}
	}
	if g := gatewaySeen(); !slices.Equal(g, []string{"/api/v1/models"}) {
		t.Errorf("gateway saw %q, want one models request", g)
	}
	if o := openaiSeen(); !slices.Equal(o, []string{"/v1/models", "/v1/models", "/v1/models"}) {
		t.Errorf("default endpoint saw %q, want good, wrong and boxed only", o)
	}
}

// TestAuthCheckRefusedSecret: a binding whose secrets the host won't
// resolve fails as secret_unresolved, without a request.
func TestAuthCheckRefusedSecret(t *testing.T) {
	openai, seen := fakeAPI(t, "sk-good")
	h := sdktest.New(t, codexext.Build(func() string { return t.TempDir() }), sdktest.WithSecrets("eng", map[string]string{"openai": "sk-good"}))
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		binding("agent", map[string]any{"env": map[string]string{"OPENAI_BASE_URL": openai.URL}, "secret_env": map[string]string{"openai": "CODEX_API_KEY"}}, "", "openai"),
	}}}}
	got, err := h.CheckRun(context.Background(), protocol.CheckRunParams{Check: codex.AuthCheckName, Deep: true, Config: cfg})
	if err != nil || got.Code != "secret_unresolved" || len(seen()) != 0 {
		t.Fatalf("check %+v (%v), requests %q; want secret_unresolved and none", got, err, seen())
	}
}
