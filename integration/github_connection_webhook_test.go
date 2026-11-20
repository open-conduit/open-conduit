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
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// ghShared is the GitHub extension with two entries in two scopes, both
// for ghRepo on one connection that names a webhook_secret, over the real
// runtime ingress and SQLite: the shape of a GitHub App's webhook.
type ghShared struct {
	t       *testing.T
	fake    *githubtest.Server
	store   *sqlite.Store
	ingress *runtime.Ingress
	x       *gh.Extension
	// refuse, when set, fails the next delivery to that entry, once.
	mu     sync.Mutex
	refuse string
}

func newGitHubShared(t *testing.T) *ghShared {
	t.Helper()
	fake := githubtest.New(ghRepo, "tok")
	t.Cleanup(fake.Close)
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
	s := &ghShared{t: t, fake: fake, store: store, ingress: in, x: gh.New()}
	entry := func(scope string) string {
		return `{"namespace":"github/` + scope + `/widgets","scope":"` + scope + `","repo":"` + ghRepo + `","projection":{"disabled":true}}`
	}
	for _, scope := range []string{"eng", "ops"} {
		meta, _ := json.Marshal(map[string]any{"enabled": true, "extensions": []map[string]any{
			{"name": "widgets", "type": "github", "connection": "app", "config": json.RawMessage(entry(scope))},
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
			values := map[string]string{gh.SecretToken: "tok", gh.SecretWebhook: ghSecret}
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
	conn := protocol.ConnectionEntry{ID: "app", Type: gh.Type, Config: json.RawMessage(`{"api_url":"` + fake.URL + `"}`), Secrets: []string{gh.SecretToken, gh.SecretWebhook}}
	var entries []protocol.ConfigEntry
	for _, scope := range []string{"eng", "ops"} {
		entries = append(entries, protocol.ConfigEntry{ScopeID: scope, Type: gh.Type, Name: "widgets", Config: json.RawMessage(entry(scope)), Connection: &conn})
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

// post sends a signed delivery to the connection's webhook route.
func (s *ghShared) post(secret, event, id string, body []byte) *httptest.ResponseRecorder {
	s.t.Helper()
	h := s.x.Webhook("app")
	if h == nil {
		s.t.Fatal("the connection's webhook route is not mounted")
	}
	req := httptest.NewRequest(http.MethodPost, "/github/app", strings.NewReader(string(body)))
	req.Header = githubtest.Sign(secret, event, id, body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// events counts the accepted Events by scope.
func (s *ghShared) events() map[domain.ID]int {
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

func receiptOf(t *testing.T, w *httptest.ResponseRecorder) (entries int, duplicate bool) {
	t.Helper()
	var r struct {
		Entries   int  `json:"entries"`
		Duplicate bool `json:"duplicate"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("answer %d %q", w.Code, w.Body.String())
	}
	return r.Entries, r.Duplicate
}

// One URL for every entry of the connection: a delivery is verified once
// and delivered to each entry whose repository it is about, a redelivery is
// answered duplicate, and one for no entry is answered 421 and recorded.
func TestGitHubConnectionWebhookFansIn(t *testing.T) {
	s := newGitHubShared(t)
	// The bridge serves only the paths the extension declares.
	if paths := s.x.WebhookPaths(); len(paths) != 1 || paths[0] != "app" {
		t.Fatalf("declared webhook paths %v", paths)
	}
	n := s.fake.OpenIssue("Header", "", ghAlice)
	body := s.fake.IssuesHook("opened", n)

	if w := s.post("wrong", "issues", "d-0", body); w.Code != http.StatusUnauthorized || len(s.events()) != 0 {
		t.Fatalf("bad signature: %d, events %v", w.Code, s.events())
	}

	if entries, dup := receiptOf(t, s.post(ghSecret, "issues", "d-1", body)); entries != 2 || dup {
		t.Fatalf("first delivery: entries %d duplicate %v", entries, dup)
	}
	first := s.events()
	if first["eng"] == 0 || first["eng"] != first["ops"] {
		t.Fatalf("each entry should take the delivery once: %v", first)
	}
	if entries, dup := receiptOf(t, s.post(ghSecret, "issues", "d-1", body)); entries != 2 || !dup {
		t.Fatalf("redelivery: entries %d duplicate %v", entries, dup)
	}
	if again := s.events(); again["eng"] != first["eng"] || again["ops"] != first["ops"] {
		t.Fatalf("redelivery added events: %v -> %v", first, again)
	}

	// A repository no entry is for, and an App-level event: 421, no Event.
	other := []byte(`{"action":"opened","repository":{"full_name":"octo/gadgets"},"issue":{"number":1}}`)
	if w := s.post(ghSecret, "issues", "d-2", other); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unknown repository: %d %q", w.Code, w.Body.String())
	}
	if w := s.post(ghSecret, "installation", "d-3", []byte(`{"action":"created","installation":{"id":1}}`)); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("installation: %d", w.Code)
	}
	if got := s.events(); got["eng"] != first["eng"] || got["ops"] != first["ops"] {
		t.Fatalf("a misrouted delivery added events: %v", got)
	}
	// The App's own ping checks the URL: acknowledged.
	if entries, _ := receiptOf(t, s.post(ghSecret, "ping", "d-4", []byte(`{"zen":"hi","hook_id":1}`))); entries != 0 {
		t.Fatalf("ping delivered to %d entries", entries)
	}
	var check inproc.CheckResult
	for _, c := range s.x.Checks() {
		if !c.Offline && !c.Deep {
			check = c.Run(context.Background(), protocol.CheckConfig{})
		}
	}
	items := strings.Join(check.Items, "\n")
	if !strings.Contains(items, "POST /github/app: 2 deliveries for no configured entry (answered 421), the last installation") ||
		!strings.Contains(items, "POST /github/app (the connection's webhook)") {
		t.Fatalf("doctor: %+v", check)
	}
}

// A delivery one entry fails to accept is not acknowledged; the
// redelivery completes it there and doesn't duplicate it in the other.
func TestGitHubConnectionWebhookRetriesAPartialDelivery(t *testing.T) {
	s := newGitHubShared(t)
	n := s.fake.OpenIssue("Header", "", ghAlice)
	body := s.fake.IssuesHook("opened", n)
	s.mu.Lock()
	s.refuse = "github/ops/widgets"
	s.mu.Unlock()
	if w := s.post(ghSecret, "issues", "d-1", body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial delivery answered %d", w.Code)
	}
	partial := s.events()
	if partial["eng"] == 0 || partial["ops"] != 0 {
		t.Fatalf("partial delivery: %v", partial)
	}
	if entries, dup := receiptOf(t, s.post(ghSecret, "issues", "d-1", body)); entries != 2 || dup {
		t.Fatalf("redelivery: entries %d duplicate %v", entries, dup)
	}
	if got := s.events(); got["eng"] != partial["eng"] || got["ops"] != partial["eng"] {
		t.Fatalf("redelivery: %v after %v", got, partial)
	}
}

// The entries a delivery goes to share its read-backs: a pull request's
// linked issue is read once, not once for each entry.
func TestGitHubConnectionWebhookSharesReads(t *testing.T) {
	s := newGitHubShared(t)
	s.fake.OpenIssue("Header", "", ghAlice)
	p := s.fake.OpenPull("Fix", "", "agent/issue-1", "abc", ghAlice)
	before := len(s.fake.Requests())
	if entries, _ := receiptOf(t, s.post(ghSecret, "pull_request", "d-1", s.fake.PullHook("opened", p))); entries != 2 {
		t.Fatalf("delivered to %d entries", entries)
	}
	reads := s.fake.Requests()[before:]
	seen := map[string]bool{}
	for _, r := range reads {
		if seen[r] {
			t.Fatalf("%s read twice for one delivery: %v", r, reads)
		}
		seen[r] = true
	}
	if len(reads) == 0 {
		t.Fatal("no read-backs: the case proves nothing")
	}
}
