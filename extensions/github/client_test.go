package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func secrets(values map[string]string) SecretFunc {
	var mu sync.Mutex
	return func(_ context.Context, name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := values[name]
		if !ok {
			return "", errors.New("missing secret " + name + " value=hunter2")
		}
		return v, nil
	}
}

func tokenClient(t *testing.T, fake *githubtest.Server, c *clock, tok string) *Client {
	t.Helper()
	cfg, err := ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, secrets(map[string]string{SecretToken: tok}), ClientOptions{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestUnchangedPollsAreConditional(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	c := &clock{t: time.Now()}
	client := tokenClient(t, fake, c, "tok")
	for i := 0; i < 3; i++ {
		if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.NotModified(); got != 2 {
		t.Fatalf("conditional answers = %d, want 2", got)
	}
	fake.SetLabels(1, "bug")
	i, err := client.Issue(context.Background(), "octo/widgets", 1)
	if err != nil || len(i.Labels) != 1 || i.Labels[0].Name != "bug" {
		t.Fatalf("changed issue not refetched: %+v %v", i, err)
	}
}

func TestRateLimitPausesUntilReset(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	c := &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	client := tokenClient(t, fake, c, "tok")
	reset := c.now().Add(10 * time.Minute)
	fake.RateLimitPrimary(reset)
	_, err := client.Issue(context.Background(), "octo/widgets", 1)
	var limit *RateLimitError
	if !errors.As(err, &limit) || !limit.Until.Equal(reset) || limit.Secondary {
		t.Fatalf("primary limit: %v", err)
	}
	before := len(fake.Requests())
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); !errors.As(err, &limit) {
		t.Fatalf("paused call reached through: %v", err)
	}
	if len(fake.Requests()) != before {
		t.Fatal("a paused credential still made a request")
	}
	if until, paused := client.PausedUntil(); !paused || !until.Equal(reset) {
		t.Fatalf("paused until %v %v", until, paused)
	}
	c.add(10 * time.Minute)
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}

func TestSecondaryRateLimitWaitsForRetryAfter(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	c := &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	client := tokenClient(t, fake, c, "tok")
	fake.RateLimitSecondary(90)
	_, err := client.Issue(context.Background(), "octo/widgets", 1)
	var limit *RateLimitError
	if !errors.As(err, &limit) || !limit.Secondary || !limit.Until.Equal(c.now().Add(90*time.Second)) {
		t.Fatalf("secondary limit: %v", err)
	}
	c.add(89 * time.Second)
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); !errors.As(err, &limit) {
		t.Fatalf("retry-after not honoured: %v", err)
	}
	c.add(time.Second)
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
		t.Fatalf("after retry-after: %v", err)
	}
}

func TestForbiddenWithoutRateLimitIsAnOrdinaryError(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	c := &clock{t: time.Now()}
	client := tokenClient(t, fake, c, "tok")
	fake.Forbid()
	_, err := client.Issue(context.Background(), "octo/widgets", 1)
	var limit *RateLimitError
	if errors.As(err, &limit) || !IsStatus(err, http.StatusForbidden) || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("ordinary 403: %v", err)
	}
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
		t.Fatalf("an ordinary 403 paused the credential: %v", err)
	}
}

func TestTokenIsResolvedAgainAfterUnauthorized(t *testing.T) {
	fake := githubtest.New("octo/widgets", "old")
	defer fake.Close()
	fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	values := map[string]string{SecretToken: "old"}
	var mu sync.Mutex
	resolved := 0
	cfg, _ := ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	client, err := NewClient(cfg, func(_ context.Context, name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		resolved++
		return values[name], nil
	}, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
		t.Fatal(err)
	}
	// Rotation: the old token is revoked and the reference now yields a new one.
	fake.RevokeToken("old")
	fake.AddToken("new")
	mu.Lock()
	values[SecretToken] = "new"
	mu.Unlock()
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
		t.Fatalf("not re-resolved after 401: %v", err)
	}
	if resolved != 2 {
		t.Fatalf("resolved %d times, want 2 (lazy, cached, refreshed on 401)", resolved)
	}
}

func TestCredentialFailuresAreSafe(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	cfg, _ := ParseConnection([]byte(`{"api_url":"` + fake.URL + `"}`))
	client, _ := NewClient(cfg, secrets(map[string]string{}), ClientOptions{})
	_, err := client.Issue(context.Background(), "octo/widgets", 1)
	if !errors.Is(err, ErrCredential) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("credential error leaks or is untyped: %v", err)
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("request sent without a credential")
	}
}

func rsaKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestAppTokens(t *testing.T) {
	fake := githubtest.New("octo/widgets")
	defer fake.Close()
	key, pemText := rsaKey(t)
	fake.AddApp("42", &key.PublicKey)
	fake.SetTokenTTL(20 * time.Minute)
	fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	c := &clock{t: fake.Now()}
	cfg, err := ParseConnection([]byte(`{"api_url":"` + fake.URL + `","auth":"app","app_id":"42"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, secrets(map[string]string{SecretAppPrivateKey: pemText}), ClientOptions{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.InstallationTokensIssued(); n != 1 {
		t.Fatalf("installation tokens minted = %d, want 1 (cached)", n)
	}
	// Within five minutes of expiry the token is refreshed before use.
	c.add(16 * time.Minute)
	if _, err := client.Issue(context.Background(), "octo/widgets", 1); err != nil {
		t.Fatal(err)
	}
	if n := fake.InstallationTokensIssued(); n != 2 {
		t.Fatalf("installation tokens minted = %d, want 2 (refreshed near expiry)", n)
	}
	// A bad key is a credential failure without key material in the error.
	bad, _ := NewClient(cfg, secrets(map[string]string{SecretAppPrivateKey: "not pem"}), ClientOptions{Now: c.now})
	if _, err := bad.Issue(context.Background(), "octo/widgets", 1); !errors.Is(err, ErrCredential) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestBotAppWritesAndReaderReads(t *testing.T) {
	fake := githubtest.New("octo/widgets", "reader")
	defer fake.Close()
	key, pemText := rsaKey(t)
	fake.AddApp("7", &key.PublicKey)
	n := fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	cfg, _ := ParseConnection([]byte(`{"api_url":"` + fake.URL + `","bot_app_id":"7"}`))
	client, _ := NewClient(cfg, secrets(map[string]string{SecretToken: "reader", SecretBotPrivateKey: pemText}), ClientOptions{})
	if _, err := client.Issue(context.Background(), "octo/widgets", n); err != nil {
		t.Fatal(err)
	}
	if err := client.EditLabels(context.Background(), "octo/widgets", n, []string{"agent-in-progress"}, []string{"missing"}); err != nil {
		t.Fatalf("writes: %v", err)
	}
	for _, w := range fake.Writes() {
		if !strings.HasPrefix(w.Token, "ghs_7_") && !strings.HasPrefix(w.Token, "app:") {
			t.Fatalf("write %s %s used %q, not the bot app", w.Method, w.Path, w.Token)
		}
	}
}

func TestReactionsAndReact(t *testing.T) {
	fake := githubtest.New("octo/widgets", "tok")
	defer fake.Close()
	n := fake.OpenIssue("One", "", githubtest.User{Login: "alice", Type: "User"})
	id := fake.AddComment(n, githubtest.User{Login: "bob", Type: "User"}, "please")
	client := tokenClient(t, fake, &clock{t: time.Now()}, "tok")
	for i := 0; i < 2; i++ { // idempotent at GitHub
		if err := client.React(context.Background(), "octo/widgets", KindIssue, id, "+1"); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := client.Reactions(context.Background(), "octo/widgets", id)
	if err != nil || counts["+1"] != 1 {
		t.Fatalf("reactions %v %v", counts, err)
	}
	if err := client.React(context.Background(), "octo/widgets", KindReview, 1, "+1"); err != nil {
		t.Fatal("reviews have no reactions endpoint and must be skipped")
	}
}

func TestWebhookSignature(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	h := githubtest.Sign("s3cret", "ping", "abc-1", body)
	if !VerifySignature("s3cret", body, h.Get("X-Hub-Signature-256")) {
		t.Fatal("valid signature rejected")
	}
	for name, tc := range map[string]struct {
		secret, header string
		body           []byte
	}{
		"wrong secret": {"other", h.Get("X-Hub-Signature-256"), body},
		"tampered":     {"s3cret", h.Get("X-Hub-Signature-256"), []byte(`{"zen":"x"}`)},
		"missing":      {"s3cret", "", body},
		"sha1 only":    {"s3cret", "sha1=deadbeef", body},
		"empty secret": {"", h.Get("X-Hub-Signature-256"), body},
	} {
		if VerifySignature(tc.secret, tc.body, tc.header) {
			t.Fatalf("%s: accepted", name)
		}
	}
	d, err := VerifyDelivery("s3cret", h, body, time.Unix(100, 0))
	if err != nil || d.ID != "abc-1" || d.ContentType != DeliveryContentType {
		t.Fatalf("delivery %+v %v", d, err)
	}
	if _, err := VerifyDelivery("other", h, body, time.Now()); !errors.Is(err, ErrSignature) {
		t.Fatalf("bad signature: %v", err)
	}
}

func TestWebhookHandler(t *testing.T) {
	var got []extension.Delivery
	fail := false
	h := WebhookHandler(func(context.Context) (string, error) { return "s3cret", nil }, func(_ context.Context, d extension.Delivery) error {
		if fail {
			return errors.New("storage down")
		}
		got = append(got, d)
		return nil
	}, nil)
	body := []byte(`{"action":"opened"}`)
	send := func(secret string) int {
		req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(string(body)))
		req.Header = githubtest.Sign(secret, "issues", "d-1", body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := send("s3cret"); code != http.StatusAccepted || len(got) != 1 {
		t.Fatalf("accepted delivery: %d %d", code, len(got))
	}
	if code := send("forged"); code != http.StatusUnauthorized || len(got) != 1 {
		t.Fatalf("forged delivery: %d", code)
	}
	fail = true
	if code := send("s3cret"); code != http.StatusServiceUnavailable {
		t.Fatalf("unaccepted delivery acknowledged: %d", code)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks/github", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
}

func TestMarkers(t *testing.T) {
	body := WithMarker("Done.\n", "work/abc", "i123")
	a := ParseAgent(body)
	if a == nil || a.Work != "work/abc" || a.Intent != "i123" || !hasIntent(body, "i123") || hasIntent(body, "i124") {
		t.Fatalf("conduit marker: %+v in %q", a, body)
	}
	// 06-02 fallback window: Open Supervisor's own pattern finds its own-note
	// marker (empty from) in every comment Open Conduit writes.
	if m := legacyMark.FindStringSubmatch(body); m == nil || m[1] != "" || !strings.HasSuffix(body, LegacyMarker+"\n") {
		t.Fatalf("no Open Supervisor own-note marker in %q", body)
	}
	for body, from := range map[string]string{
		"x\n\n<!-- open-supervisor:agent from=12 -->\n": "12",
		"x <!-- open-supervisor:agent -->":              "",
		"x <!-- claude-agent from #9 -->":               "9",
	} {
		a := ParseAgent(body)
		if a == nil || a.From != from || a.Work != "" {
			t.Fatalf("legacy marker %q: %+v", body, a)
		}
	}
	if ParseAgent("plain comment <!-- other -->") != nil {
		t.Fatal("unmarked comment parsed as agent")
	}
	if strings.Contains(WithMarker("x", "bad value -->", "i1"), "bad value") {
		t.Fatal("unsafe marker value written")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{`{"auth":"gh"}`, `{"auth":"app"}`, `{"api_url":"ftp://x"}`, `{"api_url":"https://u:p@x"}`, `{"token":"literal"}`} {
		if ValidateConnection([]byte(bad)) == nil {
			t.Fatalf("connection %s accepted", bad)
		}
	}
	if err := ValidateConnection([]byte(`{"auth":"app","app_id":"12","bot_app_id":"13"}`)); err != nil {
		t.Fatal(err)
	}
	good := `{"namespace":"github/github.com/octo/widgets","scope":"eng","repo":"octo/widgets","webhook_secret":"hook"}`
	s, err := ParseExtension([]byte(good), []string{"hook"})
	if err != nil || s.Labels.Claimed != "agent-in-progress" || s.ClosedHorizon != 168*time.Hour || s.Branch == nil || !s.TrackIssues {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	for _, bad := range []string{
		`{"namespace":"core.x","scope":"eng","repo":"octo/widgets"}`,
		`{"namespace":"n","scope":"eng","repo":"widgets"}`,
		`{"namespace":"n","scope":"eng","repo":"o/r","branch_pattern":"agent/x"}`,
		`{"namespace":"n","scope":"eng","repo":"o/r","labels":{"claimed":" "}}`,
		`{"namespace":"n","scope":"eng","repo":"o/r","unknown":1}`,
		`{"namespace":"n","scope":"eng","repo":"o/r","webhook_secret":"missing"}`,
	} {
		if _, err := ParseExtension([]byte(bad), []string{"hook"}); err == nil {
			t.Fatalf("extension %s accepted", bad)
		}
	}
}
