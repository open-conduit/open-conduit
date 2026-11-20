package codex_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/codex"
	"github.com/open-conduit/open-conduit/protocol"
)

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
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(good.Close)
	b := func(name string, cfg map[string]any) protocol.CheckBinding {
		raw, _ := json.Marshal(cfg)
		return protocol.CheckBinding{Name: name, Executor: codex.Name, Config: raw}
	}
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		b("stuck", map[string]any{"provider": map[string]any{"id": "gw", "base_url": hung.URL + "/v1"}}),
		b("fine", map[string]any{"provider": map[string]any{"id": "gw", "base_url": good.URL + "/v1"}}),
	}}}}
	got := codex.CheckAuth(context.Background(), cfg, codex.AuthOptions{Budget: 200 * time.Millisecond})
	want := []string{"eng/stuck: unreachable (" + strings.TrimPrefix(hung.URL, "http://") + ", no key)", "eng/fine: ok (" + strings.TrimPrefix(good.URL, "http://") + ", no key)"}
	if got.Status != "warn" || got.Code != "unreachable" || !slices.Equal(got.Items, want) {
		t.Fatalf("check %+v, want items %q", got, want)
	}
	if !overlapped.Load() {
		t.Fatal("the fine binding was asked only after the stuck one gave up: bindings must be tried at once")
	}
}
