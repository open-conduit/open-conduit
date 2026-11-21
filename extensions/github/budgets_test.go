package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// etagServer answers as GitHub does: an ETag over the Accept and Authorization
// headers and the body, so each token gets its own tag for the same answer.
type etagServer struct {
	*httptest.Server
	mu       sync.Mutex
	body     string
	opaque   bool
	statuses []int
	limited  bool
	reset    time.Time
}

func newETagServer(t *testing.T, body string) *etagServer {
	s := &etagServer{body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.limited && r.Method != http.MethodDelete {
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(s.reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			s.statuses = append(s.statuses, http.StatusForbidden)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			s.statuses = append(s.statuses, http.StatusNoContent)
			return
		}
		sum := sha256.Sum256([]byte(r.Header.Get("Accept") + ":" + r.Header.Get("Authorization") + ":" + s.body))
		tag := `W/"` + hex.EncodeToString(sum[:]) + `"`
		if s.opaque {
			tag = `"opaque-` + hex.EncodeToString(sum[:4]) + `"`
		}
		w.Header().Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			s.statuses = append(s.statuses, http.StatusNotModified)
			return
		}
		s.statuses = append(s.statuses, http.StatusOK)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *etagServer) last() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[len(s.statuses)-1]
}

func budgetClient(t *testing.T, s *etagServer, c *clock, b *Budgets) *Client {
	t.Helper()
	cfg, err := ParseConnection([]byte(`{"api_url":"` + s.URL + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, secrets(map[string]string{SecretToken: "unused"}), ClientOptions{Now: c.now, Budgets: b})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRotatedInstallationTokenStillGets304(t *testing.T) {
	s := newETagServer(t, `[{"number":1}]`)
	c := &clock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	b := NewBudgets()
	client := budgetClient(t, s, c, b)
	expires := c.now().Add(time.Hour)
	b.installation("token-one", client.api, 42, expires, c.now())
	b.installation("token-two", client.api, 42, expires.Add(time.Hour), c.now())
	var out []map[string]int
	if _, err := client.raw(context.Background(), "Bearer token-one", http.MethodGet, "/repos/o/r/pulls", nil, &out); err != nil || s.last() != http.StatusOK {
		t.Fatalf("first read: %v %d", err, s.last())
	}
	out = nil
	if _, err := client.raw(context.Background(), "Bearer token-two", http.MethodGet, "/repos/o/r/pulls", nil, &out); err != nil {
		t.Fatal(err)
	}
	if s.last() != http.StatusNotModified || len(out) != 1 || out[0]["number"] != 1 {
		t.Fatalf("after rotation: status %d, body %v", s.last(), out)
	}
}

// If GitHub's tag isn't the digest we expect, a rotation costs a plain 200
// and the answer is still right.
func TestUnrecognisedETagFallsBackToAFullRead(t *testing.T) {
	s := newETagServer(t, `[{"number":1}]`)
	s.opaque = true
	c := &clock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	b := NewBudgets()
	client := budgetClient(t, s, c, b)
	b.installation("token-one", client.api, 42, c.now().Add(time.Hour), c.now())
	b.installation("token-two", client.api, 42, c.now().Add(time.Hour), c.now())
	var out []map[string]int
	for _, tok := range []string{"token-one", "token-two"} {
		out = nil
		if _, err := client.raw(context.Background(), "Bearer "+tok, http.MethodGet, "/repos/o/r/pulls", nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	if s.last() != http.StatusOK || len(out) != 1 {
		t.Fatalf("status %d body %v", s.last(), out)
	}
}

// Two installations never share an answer, even for the same URL.
func TestInstallationsDoNotShareTheCache(t *testing.T) {
	s := newETagServer(t, `[]`)
	c := &clock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	b := NewBudgets()
	client := budgetClient(t, s, c, b)
	b.installation("token-a", client.api, 1, c.now().Add(time.Hour), c.now())
	b.installation("token-b", client.api, 2, c.now().Add(time.Hour), c.now())
	for _, tok := range []string{"token-a", "token-b"} {
		if _, err := client.raw(context.Background(), "Bearer "+tok, http.MethodGet, "/repos/o/r/pulls", nil, nil); err != nil {
			t.Fatal(err)
		}
		if s.last() != http.StatusOK {
			t.Fatalf("%s: %d", tok, s.last())
		}
	}
}

func TestPauseCoversEveryTokenOfTheInstallation(t *testing.T) {
	s := newETagServer(t, `[]`)
	c := &clock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	b := NewBudgets()
	one, two := budgetClient(t, s, c, b), budgetClient(t, s, c, b)
	expires := c.now().Add(time.Hour)
	b.installation("spent", one.api, 42, expires, c.now())
	b.installation("minted", one.api, 42, expires, c.now())
	b.installation("elsewhere", one.api, 43, expires, c.now())
	reset := c.now().Add(10 * time.Minute).Truncate(time.Second)
	s.mu.Lock()
	s.limited, s.reset = true, reset
	s.mu.Unlock()
	var limit *RateLimitError
	if _, err := one.raw(context.Background(), "Bearer spent", http.MethodGet, "/repos/o/r/pulls", nil, nil); !errors.As(err, &limit) {
		t.Fatalf("limit not seen: %v", err)
	}
	s.mu.Lock()
	s.limited = false
	before := len(s.statuses)
	s.mu.Unlock()
	if _, err := two.raw(context.Background(), "Bearer minted", http.MethodGet, "/repos/o/r/pulls", nil, nil); !errors.As(err, &limit) {
		t.Fatalf("a new token of the installation wasn't paused: %v", err)
	}
	s.mu.Lock()
	reached := len(s.statuses) != before
	s.mu.Unlock()
	if reached {
		t.Fatal("a paused installation's token reached GitHub")
	}
	if _, err := two.raw(context.Background(), "Bearer elsewhere", http.MethodGet, "/repos/o/r/pulls", nil, nil); err != nil {
		t.Fatalf("another installation was paused: %v", err)
	}
	if err := two.RevokeToken(context.Background(), "minted"); err != nil {
		t.Fatalf("revoking during a pause: %v", err)
	}
	if s.last() != http.StatusNoContent {
		t.Fatalf("revocation didn't go out: %d", s.last())
	}
	c.add(10 * time.Minute)
	if _, err := two.raw(context.Background(), "Bearer minted", http.MethodGet, "/repos/o/r/pulls", nil, nil); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}

func TestLookbackCoversASlowEntrysInterval(t *testing.T) {
	start := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	last := start.Add(-20 * 24 * time.Hour)
	checkpoint := scanID(last, last.Add(-time.Hour))
	if got := (&Ingress{s: Settings{ClosedHorizon: time.Hour}}).horizon(start, checkpoint); !got.Equal(start.Add(-maxLookback)) {
		t.Fatalf("default lookback: %s", got)
	}
	slow := &Ingress{s: Settings{ClosedHorizon: time.Hour, Reconcile: 15 * 24 * time.Hour}}
	if got := slow.horizon(start, checkpoint); !got.Equal(last) {
		t.Fatalf("slow entry's lookback: %s", got)
	}
}
