package chatcompletionsext_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/chatcompletions"
	chatcompletionsext "github.com/open-conduit/open-conduit/extensions/chatcompletions/pkg"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// TestAuthCheck: chat-completions-auth, which only doctor --check-secrets
// runs, lists <base_url>/models with each binding's api_key_secret and
// headers. A wrong key is unauthorised with no value or provider text; a
// binding with no key is sent none.
func TestAuthCheck(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Title"))
		mu.Unlock()
		if a := r.Header.Get("Authorization"); a != "" && a != "Bearer sk-good" {
			http.Error(w, "provider says: sk-wrong is invalid", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(api.Close)
	h := sdktest.New(t, chatcompletionsext.Build(func() string { return t.TempDir() }),
		sdktest.WithBindingSecrets("eng", "good", map[string]string{"key": "sk-good"}),
		sdktest.WithBindingSecrets("eng", "wrong", map[string]string{"key": "sk-wrong"}))
	if !slices.ContainsFunc(chatcompletionsext.Manifest().Provides.Checks, func(c protocol.CheckDecl) bool { return c.Name == chatcompletions.AuthCheckName && c.Deep }) {
		t.Fatal("no deep chat-completions-auth check declared")
	}
	b := func(name string, cfg map[string]any, secrets ...string) protocol.CheckBinding {
		raw, _ := json.Marshal(cfg)
		return protocol.CheckBinding{Name: name, Executor: chatcompletions.Name, Config: raw, SecretNames: secrets}
	}
	keyed := map[string]any{"base_url": api.URL + "/v1", "model": "m", "api_key_secret": "key", "headers": map[string]string{"X-Title": "oc"}}
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		b("good", keyed, "key"), b("wrong", keyed, "key"), b("local", map[string]any{"base_url": api.URL + "/v1", "model": "m"}),
	}}}}
	got, err := h.CheckRun(context.Background(), protocol.CheckRunParams{Check: chatcompletions.AuthCheckName, Deep: true, Config: cfg})
	if err != nil || got.OK || got.Status != "fail" || got.Code != "unauthorised" {
		t.Fatalf("check %+v (%v), want an unauthorised failure", got, err)
	}
	host := strings.TrimPrefix(api.URL, "http://")
	want := []string{"eng/good: ok (" + host + ", api_key_secret)", "eng/wrong: unauthorised (" + host + ", api_key_secret)", "eng/local: ok (" + host + ", no key)"}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items\n%q\nwant\n%q", got.Items, want)
	}
	for _, item := range got.Items {
		if strings.Contains(item, "sk-") || strings.Contains(item, "provider says") {
			t.Errorf("item %q carries a value or the provider's text", item)
		}
	}
	if want := []string{"/v1/models Bearer sk-good oc", "/v1/models Bearer sk-wrong oc", "/v1/models  "}; !slices.Equal(seen, want) {
		t.Errorf("requests %q, want %q", seen, want)
	}
}
