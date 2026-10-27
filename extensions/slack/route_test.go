package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/slack/slacktest"
)

func scopeWith(t *testing.T, extra string) domain.Scope {
	t.Helper()
	cfg := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","channels":["` + chanA + `"],"bot_user_id":"` + botUser + `"` + extra + `}`
	meta, _ := json.Marshal(map[string]any{"extensions": []map[string]any{{"type": "slack", "config": json.RawMessage(cfg)}}})
	return domain.Scope{ID: scope, Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
}

func event(t *testing.T, typ string, p slack.MessagePayload, received time.Time) domain.AcceptedEvent {
	t.Helper()
	p.Team, p.Channel = team, chanA
	if p.Thread == "" {
		p.Thread = p.TS
	}
	p.Root = p.Thread == p.TS
	data, _ := json.Marshal(p)
	return domain.AcceptedEvent{Event: domain.Event{ID: "e", Type: typ, Source: ns, DeliveryID: "d", ReceivedAt: received,
		CorrelationKey: &domain.CorrelationKey{Source: ns, ExternalID: slack.ThreadKey(chanA, p.Thread)}, Data: domain.Payload{Schema: slack.SchemaMessage, Data: data}}}
}

func match(t *testing.T, sc domain.Scope, e domain.AcceptedEvent, w *domain.Work) extension.RouteDecision {
	t.Helper()
	d, err := slack.Route{}.Match(context.Background(), extension.RouteInput{Event: e, Scope: sc, Work: w})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRouteStartsConversations(t *testing.T) {
	sc := scopeWith(t, "")
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	root := slack.MessagePayload{TS: slack.TS(written), User: alice, Text: "help"}
	if d := match(t, sc, event(t, slack.TypeMessageHuman, root, written.Add(time.Second)), nil); !d.Match || d.Kind != slack.WorkKind {
		t.Fatalf("a fresh human thread must start a conversation: %+v", d)
	}
	if d := match(t, sc, event(t, slack.TypeMessageHuman, root, written.Add(25*time.Hour)), nil); d.Match {
		t.Fatal("history older than start_window started a conversation")
	}
	reply := slack.MessagePayload{TS: slack.TS(written.Add(time.Minute)), Thread: root.TS, User: alice, Text: "more"}
	if d := match(t, sc, event(t, slack.TypeMessageHuman, reply, written.Add(time.Minute)), nil); d.Match {
		t.Fatal("a reply in an unowned thread started a conversation in start=all")
	}
	for _, typ := range []string{slack.TypeMessageBot, slack.TypeMessageAgent, slack.TypeMessageDeleted} {
		if d := match(t, sc, event(t, typ, root, written), nil); d.Match {
			t.Fatalf("%s started a conversation", typ)
		}
	}
	edited := root
	edited.Edited = slack.TS(written.Add(time.Minute))
	if d := match(t, sc, event(t, slack.TypeMessageHuman, edited, written.Add(2*time.Minute)), nil); !d.Match {
		t.Fatal("a root first seen edited (its push missed) must still start")
	}
	// Mention mode: any human message mentioning the bot in an unowned thread.
	mention := scopeWith(t, `,"start":"mention"`)
	if d := match(t, mention, event(t, slack.TypeMessageHuman, root, written), nil); d.Match {
		t.Fatal("start=mention without a mention")
	}
	reply.Mentions = []string{botUser}
	if d := match(t, mention, event(t, slack.TypeMessageHuman, reply, written.Add(time.Minute)), nil); !d.Match {
		t.Fatal("a mention in an unowned thread must start")
	}
	// Another namespace's event never matches.
	foreign := event(t, slack.TypeMessageHuman, root, written)
	foreign.Event.Source = "slack/T0OTHER"
	foreign.Event.CorrelationKey.Source = "slack/T0OTHER"
	if d := match(t, sc, foreign, nil); d.Match {
		t.Fatal("foreign namespace matched")
	}
}

func TestRouteSelectsConversationInput(t *testing.T) {
	sc := scopeWith(t, "")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	w := &domain.Work{ID: "w1", Kind: slack.WorkKind, ScopeID: scope}
	msg := slack.MessagePayload{TS: slack.TS(at.Add(time.Hour)), Thread: slack.TS(at), User: alice, Text: "any news?"}
	cases := []struct {
		name  string
		typ   string
		edit  func(*slack.MessagePayload)
		input bool
	}{
		{"human reply", slack.TypeMessageHuman, nil, true},
		{"human edit", slack.TypeMessageHuman, func(p *slack.MessagePayload) { p.Edited = slack.TS(at.Add(2 * time.Hour)) }, true},
		{"other bot", slack.TypeMessageBot, func(p *slack.MessagePayload) { p.User, p.BotID = "", "BOTHER" }, false},
		{"own reply", slack.TypeMessageAgent, func(p *slack.MessagePayload) { p.Agent = &slack.Agent{Work: "w1", Intent: "i1"} }, false},
		{"unmarked app message", slack.TypeMessageAgent, func(p *slack.MessagePayload) { p.Agent = &slack.Agent{} }, false},
		{"other Work's reply", slack.TypeMessageAgent, func(p *slack.MessagePayload) { p.Agent = &slack.Agent{Work: "w2", Intent: "i2"} }, true},
		{"tombstone", slack.TypeMessageDeleted, func(p *slack.MessagePayload) { p.Deleted, p.Text = true, "" }, false},
	}
	for _, c := range cases {
		p := msg
		if c.edit != nil {
			c.edit(&p)
		}
		if d := match(t, sc, event(t, c.typ, p, at.Add(time.Hour)), w); d.Match != c.input || d.Kind != "" {
			t.Fatalf("%s: %+v", c.name, d)
		}
	}
	removed := domain.AcceptedEvent{Event: domain.Event{Type: slack.TypeMessageRemoved, Source: ns, CorrelationKey: &domain.CorrelationKey{Source: ns, ExternalID: slack.ThreadKey(chanA, msg.Thread)}}}
	if d := match(t, sc, removed, w); d.Match {
		t.Fatal("removal delivered as input")
	}
}

func TestWebhookVerificationAndStatuses(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"type":"event_callback","team_id":"T0TEAM","event_id":"Ev123","event":{"type":"message"}}`)
	if _, err := slack.VerifyDelivery(secret, slacktest.Headers("wrong", body, now), body, now); err != slack.ErrSignature {
		t.Fatal("wrong secret verified")
	}
	if _, err := slack.VerifyDelivery("", slacktest.Headers("", body, now), body, now); err != slack.ErrSignature {
		t.Fatal("empty secret verified")
	}
	if _, err := slack.VerifyDelivery(secret, slacktest.Headers(secret, body, now.Add(-6*time.Minute)), body, now); err != slack.ErrSignature {
		t.Fatal("stale timestamp accepted (replay)")
	}
	v, err := slack.VerifyDelivery(secret, slacktest.Headers(secret, body, now), body, now)
	if err != nil || v.Delivery.ID != "Ev123" || v.Delivery.ContentType != slack.DeliveryContentType {
		t.Fatalf("verified delivery: %+v %v", v, err)
	}
	var pushed []extension.Delivery
	fail := false
	h := slack.WebhookHandler(func(context.Context) (string, error) { return secret, nil }, func(_ context.Context, d extension.Delivery) error {
		if fail {
			return extension.ErrInvalid
		}
		pushed = append(pushed, d)
		return nil
	}, func() time.Time { return now })
	serve := func(b []byte, hdr http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/slack", strings.NewReader(string(b)))
		req.Header = hdr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := serve(body, slacktest.Headers(secret, body, now)); w.Code != http.StatusOK || len(pushed) != 1 {
		t.Fatalf("accepted delivery: %d", w.Code)
	}
	if w := serve(body, slacktest.Headers("bad", body, now)); w.Code != http.StatusUnauthorized || len(pushed) != 1 {
		t.Fatalf("forged delivery: %d", w.Code)
	}
	challenge := []byte(`{"type":"url_verification","challenge":"abc123","token":"x"}`)
	if w := serve(challenge, slacktest.Headers(secret, challenge, now)); w.Code != http.StatusOK || w.Body.String() != "abc123" {
		t.Fatalf("url verification: %d %q", w.Code, w.Body.String())
	}
	if w := serve(challenge, slacktest.Headers("bad", challenge, now)); w.Code != http.StatusUnauthorized {
		t.Fatal("unsigned url verification answered")
	}
	limited := []byte(`{"type":"app_rate_limited","team_id":"T0TEAM","minute_rate_limited":1}`)
	if w := serve(limited, slacktest.Headers(secret, limited, now)); w.Code != http.StatusOK || len(pushed) != 1 {
		t.Fatalf("app_rate_limited: %d", w.Code)
	}
	fail = true
	if w := serve(body, slacktest.Headers(secret, body, now)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unaccepted delivery must not be acknowledged: %d", w.Code)
	}
	bad := []byte(`{"type":"event_callback","event_id":"not-an-id"}`)
	if w := serve(bad, slacktest.Headers(secret, bad, now)); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed delivery: %d", w.Code)
	}
}

func TestConfigValidation(t *testing.T) {
	ok := `{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C2","C1"]}`
	s, err := slack.ParseExtension([]byte(ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Start != slack.StartAll || s.History != 336*time.Hour || s.StartWindow != 24*time.Hour || s.Refresh != time.Hour || s.Channels[0] != "C1" || !s.Watches("C2") || s.Watches("C3") {
		t.Fatalf("defaults: %+v", s)
	}
	for _, bad := range []string{
		`{"namespace":"core.x","scope":"support","team":"T1","channels":["C1"]}`,
		`{"namespace":"slack/T1","scope":"Support","team":"T1","channels":["C1"]}`,
		`{"namespace":"slack/T1","scope":"support","team":"t1","channels":["C1"]}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":[]}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1","C1"]}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"start":"mention"}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"start":"sometimes"}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"history_hours":5000}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"history_hours":12,"start_window_hours":24}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"signing_secret":"unselected"}`,
		`{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"],"unknown":1}`,
	} {
		if _, err := slack.ParseExtension([]byte(bad), nil); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := slack.ParseConnection([]byte(`{"api_url":"https://user:pw@slack.com/api"}`)); err == nil {
		t.Fatal("credentials in api_url accepted")
	}
	if c, err := slack.ParseConnection(nil); err != nil || c.APIURL != "https://slack.com/api" {
		t.Fatalf("connection default: %+v %v", c, err)
	}
}

func TestRenderEvent(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e := event(t, slack.TypeMessageHuman, slack.MessagePayload{TS: slack.TS(at), User: alice, Text: "export fails", Files: []slack.File{{ID: "F1", Name: "log.txt"}}}, at)
	text, ok := slack.RenderEvent(e.Event)
	if !ok || !strings.Contains(text, "<@UALICE>") || !strings.Contains(text, "export fails") || !strings.Contains(text, slack.OpReply) || !strings.Contains(text, "log.txt") {
		t.Fatalf("render: %q", text)
	}
	if _, ok := slack.RenderEvent(domain.Event{Type: "github.comment.human"}); ok {
		t.Fatal("rendered a foreign event")
	}
}

func TestReceiverConfig(t *testing.T) {
	base := `{"namespace":"slack/T1","scope":"support","team":"T1","channels":["C1"]`
	if s, err := slack.ParseExtension([]byte(base+`}`), nil); err != nil || s.Receiver != slack.ReceiverSocket {
		t.Fatalf("default receiver: %+v %v", s, err)
	}
	if _, err := slack.ParseExtension([]byte(base+`,"receiver":"http"}`), nil); err == nil {
		t.Fatal("http receiver without signing_secret")
	}
	if s, err := slack.ParseExtension([]byte(base+`,"receiver":"http","signing_secret":"sig"}`), []string{"sig"}); err != nil || s.Receiver != slack.ReceiverHTTP {
		t.Fatalf("http receiver: %v", err)
	}
	if _, err := slack.ParseExtension([]byte(base+`,"receiver":"carrier-pigeon"}`), nil); err == nil {
		t.Fatal("unknown receiver")
	}
}

func TestRouteChannelsNarrowTheRoute(t *testing.T) {
	sc := scopeWith(t, "")
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	root := event(t, slack.TypeMessageHuman, slack.MessagePayload{TS: slack.TS(written), User: alice, Text: "help"}, written)
	route := func(cfg string) domain.Route {
		return domain.Route{Name: "r", Handler: "slack", Config: domain.Payload{Data: json.RawMessage(cfg)}}
	}
	for cfg, want := range map[string]bool{
		``:                               true,
		`{}`:                             true,
		`{"channels":["` + chanA + `"]}`: true,
		`{"channels":["` + chanB + `"]}`: false,
		`{"channels":["` + chanB + `","` + chanA + `"]}`: true,
		`{"channels":"` + chanA + `"}`:                   false,
	} {
		for _, w := range []*domain.Work{nil, {ID: "w", Kind: slack.WorkKind}} {
			d, err := slack.Route{}.Match(context.Background(), extension.RouteInput{Event: root, Scope: sc, Work: w, Route: route(cfg)})
			if err != nil || d.Match != want {
				t.Fatalf("config %q (work %v): match %v, want %v (%v)", cfg, w != nil, d.Match, want, err)
			}
		}
	}
	for cfg, ok := range map[string]bool{
		`{}`:                             true,
		`{"channels":["` + chanA + `"]}`: true,
		`{"channels":["#support"]}`:      false,
		`{"channels":["` + chanA + `","` + chanA + `"]}`: false,
		`{"channel":["` + chanA + `"]}`:                  false,
	} {
		if err := slack.ValidateRoute(json.RawMessage(cfg)); (err == nil) != ok {
			t.Fatalf("ValidateRoute(%s) = %v, want ok %v", cfg, err, ok)
		}
	}
}
