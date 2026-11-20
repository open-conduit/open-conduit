package slack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

// The interactivity Request URL verifies the signature and timestamp over
// the raw form before parsing it, hands the form's payload to take, and
// answers 200 with an empty body; take refusing is 503, a bad signature or a
// stale timestamp 401, and a body without a JSON payload 400. The Events
// API route still refuses the same form: it is not a callback.
func TestInteractivityHandler(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var taken [][]byte
	busy := false
	h := slack.InteractivityHandler(func(context.Context) (string, error) { return "signing", nil }, func(p []byte) bool {
		if busy {
			return false
		}
		taken = append(taken, p)
		return true
	}, func() time.Time { return now })
	payload := []byte(`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},"actions":[{"action_id":"oc_answer_0","value":"v","action_ts":"1.2"}]}`)
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := serve(slacktest.InteractionRequest("/slack/app/interactivity", "signing", payload, now)); w.Code != http.StatusOK || w.Body.Len() != 0 || len(taken) != 1 || string(taken[0]) != string(payload) {
		t.Fatalf("a signed press: %d %q, taken %q", w.Code, w.Body.String(), taken)
	}
	if w := serve(slacktest.InteractionRequest("/slack/app/interactivity", "wrong", payload, now)); w.Code != http.StatusUnauthorized || len(taken) != 1 {
		t.Fatalf("a bad signature: %d, taken %d", w.Code, len(taken))
	}
	if w := serve(slacktest.InteractionRequest("/slack/app/interactivity", "signing", payload, now.Add(-slack.MaxSkew-time.Second))); w.Code != http.StatusUnauthorized || len(taken) != 1 {
		t.Fatalf("a stale timestamp: %d, taken %d", w.Code, len(taken))
	}
	// A tampered body fails the signature made over the original.
	req := slacktest.InteractionRequest("/slack/app/interactivity", "signing", payload, now)
	req.Body = http.NoBody
	if w := serve(req); w.Code != http.StatusUnauthorized {
		t.Fatalf("a body the signature isn't over: %d", w.Code)
	}
	for _, body := range []string{`{"type":"block_actions"}`, "payload=not-json", "other=1"} {
		req := httptest.NewRequest(http.MethodPost, "/slack/app/interactivity", strings.NewReader(body))
		req.Header = slacktest.Headers("signing", []byte(body), now)
		if w := serve(req); w.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d", body, w.Code)
		}
	}
	busy = true
	if w := serve(slacktest.InteractionRequest("/slack/app/interactivity", "signing", payload, now)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a press that couldn't be started: %d", w.Code)
	}
	if w := serve(httptest.NewRequest(http.MethodGet, "/slack/app/interactivity", nil)); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	unavailable := slack.InteractivityHandler(func(context.Context) (string, error) { return "", errors.New("no secret") }, func([]byte) bool { return true }, nil)
	w := httptest.NewRecorder()
	unavailable.ServeHTTP(w, slacktest.InteractionRequest("/slack/app/interactivity", "signing", payload, time.Now()))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without the secret: %d", w.Code)
	}

	// The same press sent to the Events API route passes its signature
	// and is refused as no callback.
	events := slack.WebhookHandler(func(context.Context) (string, error) { return "signing", nil }, nil, func() time.Time { return now })
	w = httptest.NewRecorder()
	events.ServeHTTP(w, slacktest.InteractionRequest("/slack/app", "signing", payload, now))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a press on the Events API route: %d", w.Code)
	}
}

// Only an entry that can receive a press posts buttons: receiver none
// leaves out the feedback buttons (and, in the projector, the Stop button,
// request messages and a prompt's buttons). A press is an entry's only in
// its own workspace and a channel it observes.
func TestPressesFollowTheReceiver(t *testing.T) {
	for receiver, presses := range map[string]bool{"": true, "socket": true, "http": true, "none": false} {
		s, err := slack.ParseExtension([]byte(`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"receiver":"`+receiver+`","feedback":{"enabled":true}}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Presses() != presses || s.AsksFeedback("agent") != presses {
			t.Errorf("receiver %q: presses %v, feedback %v", receiver, s.Presses(), s.AsksFeedback("agent"))
		}
		if !s.TakesPress("T1", "C1") || s.TakesPress("T2", "C1") || s.TakesPress("T1", "C2") || s.TakesPress("T1", "D1") {
			t.Errorf("receiver %q: presses taken from the wrong place", receiver)
		}
	}
}
