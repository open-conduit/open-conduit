package linear_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/conformance"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/extensions/linear"
	"github.com/alsiola/open-conduit/extensions/linear/lineartest"
	"github.com/alsiola/open-conduit/extensions/tracker"
)

const entry = `{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"webhook_secret":"hook","transitions":{"claimed":"In Progress","review":"in review"}}`

func newClient(t *testing.T, f *lineartest.Fake, cfg string, token string) (*linear.Client, linear.Settings) {
	t.Helper()
	conn, err := linear.ParseConnection([]byte(`{"api_url":"` + f.URL() + `","agent_account":"robot@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := linear.ParseExtension([]byte(cfg), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := linear.NewClient(conn, s, func(context.Context, string) (string, error) { return token, nil }, linear.ClientOptions{Now: f.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{`{"api_url":"ftp://x"}`, `{"api_url":"https://u:p@x"}`, `{"token":"x"}`} {
		if linear.ValidateConnection(json.RawMessage(bad)) == nil {
			t.Errorf("connection %s accepted", bad)
		}
	}
	for _, bad := range []string{
		`{"namespace":"linear/eng","scope":"eng","teams":["not a key"]}`,
		`{"namespace":"linear/eng","scope":"eng","teams":[]}`,
		`{"namespace":"core.x","scope":"eng","teams":["ENG"]}`,
		`{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"transitions":{"wat":"Done"}}`,
		`{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"webhook_secret":"missing"}`,
		`{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"repo":"x"}`,
	} {
		if linear.ValidateExtension(json.RawMessage(bad), []string{"hook"}) == nil {
			t.Errorf("entry %s accepted", bad)
		}
	}
	s, err := linear.ParseExtension([]byte(`{"namespace":"linear/eng","scope":"eng","teams":["eng"," Ops"],"states":["Backlog","started"]}`), nil)
	if err != nil || strings.Join(s.Teams, ",") != "ENG,OPS" || strings.Join(s.States, ",") != "backlog,started" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestListIssuesWaitingAndAttrs(t *testing.T) {
	f := lineartest.New("lin_api_x", "ENG", "OPS")
	defer f.Close()
	f.AddIssue("ENG", 1, "first", "Todo", "web")
	f.AddIssue("ENG", 2, "second", "Backlog")
	f.AddIssue("ENG", 3, "third", "In Progress")
	f.AddIssue("OPS", 1, "other team", "Todo")
	c, _ := newClient(t, f, entry, "lin_api_x")
	var all []tracker.Issue
	cursor := ""
	for {
		page, err := c.ListIssues(context.Background(), f.Now().Add(-time.Hour), cursor)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Issues...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(all) != 3 || all[0].Key != "ENG-1" || all[1].Key != "ENG-2" || all[2].Key != "ENG-3" {
		t.Fatalf("listing: %+v", all)
	}
	if all[0].Waiting || !all[1].Waiting || all[2].Waiting {
		t.Fatalf("only the backlog issue waits: %v %v %v", all[0].Waiting, all[1].Waiting, all[2].Waiting)
	}
	i := all[0]
	if !i.Open || i.Status != "Todo" || i.Labels[0] != "web" || strings.Join(i.Attrs["team"], ",") != "ENG,ENG team" || i.Attrs["priority"][0] != "High" || i.Attrs["statetype"][0] != "unstarted" {
		t.Fatalf("issue: %+v", i)
	}
	// Claiming from Backlog too, by state name.
	c2, _ := newClient(t, f, `{"namespace":"linear/eng","scope":"eng","teams":["ENG"],"states":["Backlog","started"]}`, "lin_api_x")
	page, err := c2.ListIssues(context.Background(), f.Now(), "")
	if err != nil || !page.Issues[0].Waiting || page.Issues[1].Waiting || page.Issues[2].Waiting {
		t.Fatalf("by name, ENG-1 (Todo) waits and the backlog doesn't: %+v %v", page, err)
	}
	if !c.Owns("eng-9") || c.Owns("OPS-1") || c.RefFor("ENG-9") != "Closes ENG-9" {
		t.Fatal("ownership or reference")
	}
	if refs := c.RefsIn("Closes eng-12; not XENG-4 or OPS-5. ENG-12"); strings.Join(refs, ",") != "ENG-12" {
		t.Fatalf("refs %v", refs)
	}
}

func TestMarks(t *testing.T) {
	body := linear.WithMark("Done.\n", tracker.Mark{Work: "w-1", Intent: "iabc", From: "ENG-4"})
	a := linear.ParseMark(body)
	if a == nil || a.From != "ENG-4" || a.Work != "w-1" || a.Intent != "iabc" {
		t.Fatalf("mark lost: %q -> %+v", body, a)
	}
	if !strings.Contains(body, "*Posted by an open-supervisor agent working on ENG-4*\n") {
		t.Fatalf("Open Supervisor's footer must stay readable to it: %q", body)
	}
	// Linear may rewrite the emphasis.
	if a := linear.ParseMark("ok\n\n_Posted by an open-supervisor agent_"); a == nil || a.From != "" {
		t.Fatalf("plain mark: %+v", a)
	}
	if linear.ParseMark("I was posted by an open-supervisor agent, apparently") != nil {
		t.Fatal("a mention mid-sentence isn't a mark")
	}
	f := lineartest.New("k", "ENG")
	defer f.Close()
	f.AddIssue("ENG", 1, "x", "Todo")
	f.AddComment("ENG-1", "Robot@example.com", "hi")
	bot := f.AddComment("ENG-1", "GitHub", "deployed")
	bot.Bot = true
	c, _ := newClient(t, f, entry, "k")
	comments, err := c.Comments(context.Background(), "ENG-1")
	if err != nil || len(comments) != 2 {
		t.Fatal(comments, err)
	}
	if comments[0].Agent == nil {
		t.Fatal("the agent account's comment isn't marked")
	}
	if !comments[1].Bot || comments[1].Author != "GitHub" {
		t.Fatalf("bot: %+v", comments[1])
	}
}

func signed(t *testing.T, payload map[string]any) (http.Header, []byte) {
	t.Helper()
	if _, ok := payload["webhookTimestamp"]; !ok {
		payload["webhookTimestamp"] = time.Now().UnixMilli()
	}
	body, _ := json.Marshal(payload)
	h := http.Header{}
	h.Set("Linear-Signature", tracker.SignHMAC("s3cret", body))
	h.Set("Linear-Delivery", "d-"+tracker.Digest(string(body))[:8])
	return h, body
}

func TestParseHook(t *testing.T) {
	f := lineartest.New("k", "ENG")
	defer f.Close()
	c, _ := newClient(t, f, entry, "k")
	body := []byte(`{"type":"Issue"}`)
	h := http.Header{}
	h.Set("Linear-Signature", tracker.SignHMAC("wrong", body))
	if _, err := c.ParseHook(h, body, "s3cret", time.Now()); !errors.Is(err, tracker.ErrSignature) {
		t.Fatal("a bad signature was taken")
	}
	h, body = signed(t, map[string]any{"type": "Issue", "action": "create", "webhookTimestamp": time.Now().Add(-time.Hour).UnixMilli(), "data": map[string]any{"identifier": "ENG-5"}})
	if _, err := c.ParseHook(h, body, "s3cret", time.Now()); err == nil {
		t.Fatal("an hour-old delivery was taken")
	}
	h, body = signed(t, map[string]any{"type": "Issue", "action": "create", "data": map[string]any{"identifier": "eng-5"}})
	hook, err := c.ParseHook(h, body, "s3cret", time.Now())
	if err != nil || len(hook.Issues) != 1 || hook.Issues[0] != "ENG-5" || hook.ID == "" {
		t.Fatalf("issue: %+v %v", hook, err)
	}
	h, body = signed(t, map[string]any{"type": "Issue", "action": "create", "data": map[string]any{"identifier": "OPS-1"}})
	if hook, _ := c.ParseHook(h, body, "s3cret", time.Now()); len(hook.Issues) != 0 {
		t.Fatalf("another team's issue: %+v", hook)
	}
	h, body = signed(t, map[string]any{"type": "Comment", "action": "create", "data": map[string]any{"id": "c9"}})
	if hook, _ := c.ParseHook(h, body, "s3cret", time.Now()); len(hook.Comments) != 1 || hook.Comments[0].ID != "c9" {
		t.Fatalf("comment: %+v", hook)
	}
	// The boundary answers without reading the API.
	if f.Calls["issue"] != 0 || f.Calls["comment"] != 0 {
		t.Fatalf("parse read the API: %v", f.Calls)
	}
}

func TestEditLabelsAndTransition(t *testing.T) {
	f := lineartest.New("k", "ENG")
	defer f.Close()
	f.AddIssue("ENG", 1, "x", "Todo", "web", "agent-needs-help")
	c, _ := newClient(t, f, entry, "k")
	ctx := context.Background()
	if err := c.EditLabels(ctx, "ENG-1", []string{"ready-for-review"}, []string{"agent-needs-help"}); err != nil {
		t.Fatal(err)
	}
	labels, state, _ := f.Snapshot("ENG-1")
	if strings.Join(labels, ",") != "ready-for-review,web" || state != "Todo" {
		t.Fatalf("labels %v state %s", labels, state)
	}
	if err := c.Transition(ctx, "ENG-1", "in review"); err != nil {
		t.Fatal(err)
	}
	updates := f.Calls["issueUpdate"]
	if err := c.Transition(ctx, "ENG-1", "In Review"); err != nil || f.Calls["issueUpdate"] != updates {
		t.Fatalf("already there is a no-op: %v %d", err, f.Calls["issueUpdate"])
	}
	if _, state, _ = f.Snapshot("ENG-1"); state != "In Review" {
		t.Fatalf("state %s", state)
	}
	if err := c.Transition(ctx, "ENG-1", "Nowhere"); !errors.Is(err, linear.ErrNoState) {
		t.Fatalf("unknown state: %v", err)
	}
}

func TestRateLimitAndCredential(t *testing.T) {
	f := lineartest.New("lin_oauth_t", "ENG")
	defer f.Close()
	f.AddIssue("ENG", 1, "x", "Todo")
	resolved := 0
	conn, _ := linear.ParseConnection([]byte(`{"api_url":"` + f.URL() + `"}`))
	s, _ := linear.ParseExtension([]byte(entry), []string{"hook"})
	c, _ := linear.NewClient(conn, s, func(context.Context, string) (string, error) { resolved++; return "lin_oauth_t", nil }, linear.ClientOptions{Now: f.Now})
	ctx := context.Background()
	f.RateLimited = 1
	var limit *tracker.RateLimitError
	if _, err := c.Issue(ctx, "ENG-1"); !errors.As(err, &limit) {
		t.Fatalf("rate limit: %v", err)
	}
	if _, err := c.Issue(ctx, "ENG-1"); !errors.As(err, &limit) || f.Calls["issue"] != 0 {
		t.Fatalf("a paused client calls nobody: %v %v", err, f.Calls)
	}
	c2, _ := linear.NewClient(conn, s, func(context.Context, string) (string, error) { resolved++; return "lin_oauth_t", nil }, linear.ClientOptions{Now: f.Now})
	f.Unauthorized = 1
	if _, err := c2.Issue(ctx, "ENG-1"); err != nil || resolved < 3 {
		t.Fatalf("a 401 re-resolves the token and retries once: %v (resolved %d)", err, resolved)
	}
	f.Unauthorized = 2
	if _, err := c2.Issue(ctx, "ENG-1"); !errors.Is(err, tracker.ErrCredential) {
		t.Fatalf("credential: %v", err)
	}
	if _, err := c2.Issue(ctx, "ENG-404"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}
}

func TestIngressConformance(t *testing.T) {
	conformance.RunIngress(t, func() conformance.IngressFixture {
		f := lineartest.New("k", "ENG")
		t.Cleanup(f.Close)
		fixed := time.Now().UTC()
		f.Now = func() time.Time { return fixed }
		f.AddIssue("ENG", 1, "first", "Todo", "web")
		f.AddIssue("ENG", 2, "second", "Todo")
		f.AddComment("ENG-2", "Ada", "please")
		c, s := newClient(t, f, entry, "k")
		in := tracker.NewIngress(s.Settings, c, f.Now, tracker.IngressOptions{BatchUnits: 2})
		src := extension.Source{Namespace: "linear/eng", ScopeID: "eng"}
		h, body := signed(t, map[string]any{"type": "Issue", "action": "update", "data": map[string]any{"identifier": "ENG-1"}})
		d, err := tracker.VerifyDelivery(linear.Type, c, "s3cret", h, body, f.Now())
		if err != nil {
			t.Fatal(err)
		}
		snap, err := in.Reconcile(context.Background(), extension.Scan{Source: src})
		if err != nil || len(snap.Observations) == 0 {
			t.Fatal(snap, err)
		}
		return conformance.IngressFixture{Ingress: in, Source: src, Delivery: d, Canonical: snap.Observations[0], OneShotType: "linear.delivery"}
	})
}

func TestReopenGenerations(t *testing.T) {
	f := lineartest.New("k", "ENG")
	defer f.Close()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return now }
	f.AddIssue("ENG", 1, "x", "Todo")
	f.AddComment("ENG-1", "Ada", "before")
	now = now.Add(time.Hour)
	f.Move("ENG-1", "Done")
	now = now.Add(time.Hour)
	f.Move("ENG-1", "Todo")
	now = now.Add(time.Hour)
	f.AddComment("ENG-1", "Ada", "after")
	c, s := newClient(t, f, entry, "k")
	in := tracker.NewIngress(s.Settings, c, f.Now, tracker.IngressOptions{})
	snap, err := in.Reconcile(context.Background(), extension.Scan{Source: extension.Source{Namespace: "linear/eng", ScopeID: "eng"}})
	if err != nil || !snap.Complete || len(snap.Observations) != 3 {
		t.Fatal(snap, err)
	}
	gen := "issue/ENG-1/generation/reopen-" + jsonInt(now.Add(-time.Hour).Unix())
	if k := snap.Observations[0].Event.CorrelationKey.ExternalID; k != gen {
		t.Fatalf("issue key %s, want %s", k, gen)
	}
	var p tracker.IssuePayload
	_ = json.Unmarshal(snap.Observations[0].Event.Data.Data, &p)
	if p.Predecessor != "issue/ENG-1" || p.Status != "Todo" {
		t.Fatalf("payload %+v", p)
	}
	if k := snap.Observations[1].Event.CorrelationKey.ExternalID; k != "issue/ENG-1" {
		t.Fatalf("an older comment stays with the first generation: %s", k)
	}
	if k := snap.Observations[2].Event.CorrelationKey.ExternalID; k != gen {
		t.Fatalf("a newer comment joins the reopened generation: %s", k)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestWebhookHandlerAndEnsure(t *testing.T) {
	f := lineartest.New("k", "ENG")
	defer f.Close()
	f.AddIssue("ENG", 1, "x", "Todo")
	c, s := newClient(t, f, entry, "k")
	in := tracker.NewIngress(s.Settings, c, f.Now, tracker.IngressOptions{})
	var pushed []extension.PushResult
	handler := tracker.WebhookHandler(linear.Type, c, func(context.Context) (string, error) { return "s3cret", nil }, func(ctx context.Context, d extension.Delivery) error {
		res, err := in.Push(ctx, extension.Source{Namespace: "linear/eng", ScopeID: "eng"}, d)
		pushed = append(pushed, res)
		return err
	}, f.Now)
	h, body := signed(t, map[string]any{"type": "Issue", "action": "update", "data": map[string]any{"identifier": "ENG-1"}})
	req := httptest.NewRequest(http.MethodPost, "/linear/eng/eng", strings.NewReader(string(body)))
	req.Header = h
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || len(pushed) != 1 || len(pushed[0].Observations) != 1 || len(pushed[0].Events) != 1 {
		t.Fatalf("delivery: %d %+v", w.Code, pushed)
	}
	req = httptest.NewRequest(http.MethodPost, "/linear/eng/eng", strings.NewReader(string(body)))
	req.Header = http.Header{"Linear-Signature": {"00"}}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || len(pushed) != 1 {
		t.Fatalf("forged: %d", w.Code)
	}
	id, err := c.EnsureWebhook(context.Background(), "https://example.test/linear/eng/eng", "s3cret", "")
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	again, err := c.EnsureWebhook(context.Background(), "https://example.test/new", "s3cret", id)
	if err != nil || again != id || f.Webhooks[id]["url"] != "https://example.test/new" {
		t.Fatalf("re-point: %s %v", again, err)
	}
	gone, err := c.EnsureWebhook(context.Background(), "https://example.test/new", "s3cret", "hook-missing")
	if err != nil || gone == "hook-missing" {
		t.Fatalf("a gone hook is made again: %s %v", gone, err)
	}
}
