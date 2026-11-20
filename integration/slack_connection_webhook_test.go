package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// slShared is the Slack extension with two receiver http entries in two
// scopes, both watching slChannel of slTeam on one connection that names a
// signing_secret, over the real runtime ingress and SQLite: the shape of a
// Slack app's one Events API Request URL.
type slShared struct {
	t       *testing.T
	fake    *slacktest.Server
	store   *sqlite.Store
	ingress *runtime.Ingress
	x       *sl.Extension
	mu      sync.Mutex
	refuse  string // fails the next delivery to that entry, once
}

func newSlackShared(t *testing.T) *slShared {
	t.Helper()
	fake := slacktest.New(slTeam, "xoxb-test")
	t.Cleanup(fake.Close)
	fake.Join(slChannel, slBob)
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	in, err := runtime.NewIngress(store, fake.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := &slShared{t: t, fake: fake, store: store, ingress: in, x: sl.New()}
	t.Cleanup(s.x.Stop)
	entry := func(scope string) string {
		return `{"namespace":"slack/` + scope + `","scope":"` + scope + `","team":"` + slTeam + `","channels":["` + slChannel + `"],"bot_user_id":"UBOT","claim_from":"all","receiver":"http"}`
	}
	for _, scope := range []string{"support", "ops"} {
		meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
			{"name": "helpdesk", "type": "slack", "connection": "app", "config": json.RawMessage(entry(scope))},
		}})
		if err := store.Transact(ctx, storage.Guard{Now: fake.Now()}, func(tx storage.Tx) error {
			return tx.PutScope(domain.Scope{ID: domain.ID(scope), Name: scope, Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	deliver := func(ctx context.Context, name string, d extension.Delivery) ([]domain.AcceptedEvent, []bool, error) {
		s.mu.Lock()
		refused := s.refuse == name
		if refused {
			s.refuse = ""
		}
		s.mu.Unlock()
		if refused {
			return nil, nil, errors.New("storage unavailable")
		}
		return in.Deliver(ctx, name, d)
	}
	if err := s.x.Start(ctx, inproc.Host{Store: store, Now: fake.Now, DeliverAdded: deliver,
		Deliver: func(ctx context.Context, name string, d extension.Delivery) error {
			_, _, err := deliver(ctx, name, d)
			return err
		},
		Secrets: func(_ context.Context, _ domain.ID, conn string, names []string) (map[string]string, error) {
			values := map[string]string{sl.SecretBotToken: "xoxb-test", sl.SecretSigning: slSecret}
			out := map[string]string{}
			for _, n := range names {
				if conn != "app" || values[n] == "" {
					return nil, extension.ErrUnauthorized
				}
				out[n] = values[n]
			}
			return out, nil
		}}); err != nil {
		t.Fatal(err)
	}
	conn := protocol.ConnectionEntry{ID: "app", Type: sl.Type, Config: json.RawMessage(`{"api_url":"` + fake.URL + `"}`), Secrets: []string{sl.SecretBotToken, sl.SecretSigning}}
	var entries []protocol.ConfigEntry
	for _, scope := range []string{"support", "ops"} {
		entries = append(entries, protocol.ConfigEntry{ScopeID: scope, Type: sl.Type, Name: "helpdesk", Config: json.RawMessage(entry(scope)), Connection: &conn})
	}
	s.x.Configure(ctx, protocol.ConfigureParams{Entries: entries})
	sources := s.x.Sources()
	if len(sources) != 2 {
		t.Fatalf("sources %+v", sources)
	}
	for _, src := range sources {
		if err := in.Register(ctx, src.Name, src.Source, src.Ingress, 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *slShared) post(secret string, body []byte) *httptest.ResponseRecorder {
	s.t.Helper()
	h := s.x.Webhook("app")
	if h == nil {
		s.t.Fatal("the connection's Request URL is not mounted")
	}
	req := httptest.NewRequest(http.MethodPost, "/slack/app", strings.NewReader(string(body)))
	req.Header = slacktest.Headers(secret, body, s.fake.Now())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func (s *slShared) events() map[domain.ID]int {
	s.t.Helper()
	out := map[domain.ID]int{}
	if err := s.store.Transact(context.Background(), storage.Guard{Now: s.fake.Now()}, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			out[a.ScopeID]++
		}
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func slReceipt(t *testing.T, w *httptest.ResponseRecorder) (entries int, duplicate bool) {
	t.Helper()
	var r struct {
		Entries   int  `json:"entries"`
		Duplicate bool `json:"duplicate"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("answer %d %q", w.Code, w.Body.String())
	}
	return r.Entries, r.Duplicate
}

// One Request URL for every entry of the app: a callback is verified once
// and delivered to each entry that watches its workspace and channel, a
// retry is answered duplicate, and one for no entry is answered 421 without
// a retry and recorded.
func TestSlackConnectionWebhookFansIn(t *testing.T) {
	s := newSlackShared(t)
	// The bridge serves only the paths the extension declares.
	if paths := s.x.WebhookPaths(); len(paths) != 1 || paths[0] != "app" {
		t.Fatalf("declared webhook paths %v", paths)
	}
	root := s.fake.Post(slChannel, slAlice, "export fails")
	body := s.fake.MessageEvent(slChannel, root)

	if w := s.post("wrong", body); w.Code != http.StatusUnauthorized || len(s.events()) != 0 {
		t.Fatalf("bad signature: %d, events %v", w.Code, s.events())
	}
	challenge := []byte(`{"type":"url_verification","challenge":"abc123"}`)
	if w := s.post(slSecret, challenge); w.Code != http.StatusOK || w.Body.String() != "abc123" {
		t.Fatalf("url verification: %d %q", w.Code, w.Body.String())
	}

	if entries, dup := slReceipt(t, s.post(slSecret, body)); entries != 2 || dup {
		t.Fatalf("first callback: entries %d duplicate %v", entries, dup)
	}
	first := s.events()
	if first["support"] != 1 || first["ops"] != 1 {
		t.Fatalf("each entry should take the callback once: %v", first)
	}
	if entries, dup := slReceipt(t, s.post(slSecret, body)); entries != 2 || !dup {
		t.Fatalf("retry: entries %d duplicate %v", entries, dup)
	}
	if again := s.events(); again["support"] != 1 || again["ops"] != 1 {
		t.Fatalf("retry added events: %v", again)
	}

	// Another workspace, and a channel no entry watches: 421, no Event.
	other := []byte(strings.Replace(string(body), slTeam, "T0OTHER", 1))
	w := s.post(slSecret, other)
	if w.Code != http.StatusMisdirectedRequest || w.Header().Get("X-Slack-No-Retry") != "1" {
		t.Fatalf("another workspace: %d %v", w.Code, w.Header())
	}
	unwatched := []byte(strings.ReplaceAll(string(body), slChannel, "C0ELSEWHERE"))
	if w := s.post(slSecret, unwatched); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unwatched channel: %d", w.Code)
	}
	if got := s.events(); got["support"] != 1 || got["ops"] != 1 {
		t.Fatalf("a misrouted callback added events: %v", got)
	}
	var check inproc.CheckResult
	for _, c := range s.x.Checks() {
		if !c.Offline && !c.Deep {
			check = c.Run(context.Background(), protocol.CheckConfig{})
		}
	}
	items := strings.Join(check.Items, "\n")
	if !strings.Contains(items, "POST /slack/app: 2 callbacks for no configured entry (answered 421), the last from team "+slTeam+" channel C0ELSEWHERE") ||
		!strings.Contains(items, "POST /slack/app (the connection's; set it as the app's Events API Request URL)") {
		t.Fatalf("doctor: %+v", check)
	}
}

// A callback one entry fails to accept is not acknowledged; Slack's retry
// completes it there and doesn't duplicate it in the other.
func TestSlackConnectionWebhookRetriesAPartialDelivery(t *testing.T) {
	s := newSlackShared(t)
	root := s.fake.Post(slChannel, slAlice, "export fails")
	body := s.fake.MessageEvent(slChannel, root)
	s.mu.Lock()
	s.refuse = "slack/ops/helpdesk"
	s.mu.Unlock()
	if w := s.post(slSecret, body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial callback answered %d", w.Code)
	}
	if got := s.events(); got["support"] != 1 || got["ops"] != 0 {
		t.Fatalf("partial callback: %v", got)
	}
	if entries, dup := slReceipt(t, s.post(slSecret, body)); entries != 2 || dup {
		t.Fatalf("retry: entries %d duplicate %v", entries, dup)
	}
	if got := s.events(); got["support"] != 1 || got["ops"] != 1 {
		t.Fatalf("retry: %v", got)
	}
}
