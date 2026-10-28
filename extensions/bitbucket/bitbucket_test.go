package bitbucket_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/bitbucket"
	"github.com/open-conduit/open-conduit/extensions/bitbucket/bitbuckettest"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

const entry = `{"namespace":"bitbucket/acme/app","scope":"proj","repo":"acme/app","tracker":{"namespace":"jira/acme/proj","keys":["PROJ"]},"webhook_secret":"hook"}`

func newEnv(t *testing.T) (*bitbuckettest.Fake, *bitbucket.Client, bitbucket.Settings) {
	t.Helper()
	f := bitbuckettest.New("acme/app", "me@example.com", "tok")
	t.Cleanup(f.Close)
	fixed := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return fixed }
	conn, err := bitbucket.ParseConnection([]byte(`{"api_url":"` + f.URL() + `","username":"me@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := bitbucket.ParseExtension([]byte(entry), []string{"hook"})
	if err != nil {
		t.Fatal(err)
	}
	return f, bitbucket.NewClient(conn, func(context.Context, string) (string, error) { return "tok", nil }, bitbucket.ClientOptions{Now: f.Now}), s
}

func TestConfigAndIssueOf(t *testing.T) {
	for _, bad := range []string{`{}`, `{"username":"x","auth":"oauth"}`, `{"api_url":"ftp://x","auth":"bearer"}`} {
		if bitbucket.ValidateConnection(json.RawMessage(bad)) == nil {
			t.Errorf("connection %s accepted", bad)
		}
	}
	for _, bad := range []string{
		`{"namespace":"b","scope":"proj","repo":"noslash"}`,
		`{"namespace":"b","scope":"proj","repo":"a/b","tracker":{"namespace":"j","keys":["bad key"]}}`,
		`{"namespace":"b","scope":"proj","repo":"a/b","issue_ref":"no key"}`,
		`{"namespace":"b","scope":"proj","repo":"a/b","branch_pattern":"no-capture"}`,
	} {
		if bitbucket.ValidateExtension(json.RawMessage(bad), nil) == nil {
			t.Errorf("entry %s accepted", bad)
		}
	}
	_, _, s := newEnv(t)
	for _, c := range []struct{ branch, title, body, want string }{
		{"agent/issue/PROJ-7", "x", "", "PROJ-7"},
		{"agent/issue/PROJ-7/generation/reopen-12", "x", "", "PROJ-7"},
		{"feature", "Fix proj-9 crash", "", "PROJ-9"},
		{"feature", "x", "See PROJ-3", "PROJ-3"},
		{"feature", "XPROJ-4 and WEB-1", "", ""},
		{"agent/issue/WEB-1", "x", "", ""},
	} {
		if got := s.IssueOf(c.branch, c.title, c.body); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
}

func TestMarkRoundTrip(t *testing.T) {
	body := bitbucket.Mark("Done.\n", tracker.Mark{Work: "w-1", Intent: "iabc", From: "PROJ-4"})
	text, a := bitbucket.Unmark(body)
	if text != "Done." || a == nil || a.From != "PROJ-4" || a.Work != "w-1" || a.Intent != "iabc" {
		t.Fatalf("%q -> %q %+v", body, text, a)
	}
	if text, a := bitbucket.Unmark("ok\n\n[//]: # (open-supervisor:agent)"); text != "ok" || a == nil || a.From != "" {
		t.Fatalf("Open Supervisor's plain mark: %q %+v", text, a)
	}
	if _, a := bitbucket.Unmark("plain"); a != nil {
		t.Fatal("unmarked")
	}
}

func TestReconcilePaginationCommentsAndChecks(t *testing.T) {
	f, c, s := newEnv(t)
	f.PageSize = 1
	f.AddPR(1, "Fix it", "agent/issue/PROJ-7", "aaa1111abc")
	f.AddPR(2, "Unrelated", "feature", "bbb2222abc")
	f.AddComment(1, "Ada", "looks good")
	f.AddComment(1, "Agent", bitbucket.Mark("pushed", tracker.Mark{Work: "w-9", From: "PROJ-7"}))
	bot := f.AddComment(1, "CI", "deployed")
	bot.App = true
	inline := f.AddComment(1, "Ada", "rename this")
	inline.Path, inline.Line = "main.go", 12
	f.SetStatus("aaa1111abc", "build", "SUCCESSFUL", "https://ci/1")
	f.SetStatus("aaa1111abc", "deploy-preview", "SUCCESSFUL", "https://preview/1")
	f.SetStatus("bbb2222abc", "build", "INPROGRESS", "")
	in := bitbucket.NewIngress(s, c, f.Now, bitbucket.IngressOptions{BatchUnits: 2,
		Generations: func(_ context.Context, key string, _ time.Time) (string, error) {
			if key == "PROJ-7" {
				return "reopen-5", nil
			}
			return "", nil
		}})
	src := extension.Source{Namespace: "bitbucket/acme/app", ScopeID: "proj"}
	var all []extension.Observation
	scan := extension.Scan{Source: src}
	for n := 0; n < 10; n++ {
		snap, err := in.Reconcile(context.Background(), scan)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, snap.Observations...)
		if snap.NextCursor == "" {
			if !snap.Complete {
				t.Fatal("incomplete")
			}
			break
		}
		scan.ScanID, scan.Cursor = snap.ScanID, snap.NextCursor
	}
	types := map[string]string{}
	for _, o := range all {
		types[o.ID] = o.Event.Type
	}
	want := map[string]string{"pr/1": bitbucket.TypePullOpen, "pr/1/checks": bitbucket.TypeChecksPassed, "pr/2": bitbucket.TypePullOpen, "pr/2/checks": bitbucket.TypeChecksPending}
	for id, typ := range want {
		if types[id] != typ {
			t.Errorf("%s: %s, want %s", id, types[id], typ)
		}
	}
	var human, agent, botType, inlineSeen int
	for _, o := range all {
		if o.ID == "pr/1" && o.Event.CorrelationKey.Source != "jira/acme/proj" || o.ID == "pr/1" && o.Event.CorrelationKey.ExternalID != "issue/PROJ-7/generation/reopen-5" {
			t.Errorf("PR 1 belongs to the tracker issue's generation: %+v", o.Event.CorrelationKey)
		}
		if o.ID == "pr/2" && o.Event.CorrelationKey.ExternalID != "pr/2" {
			t.Errorf("an unrelated PR keeps its own key: %+v", o.Event.CorrelationKey)
		}
		switch o.Event.Type {
		case bitbucket.TypeCommentHuman:
			human++
			var p bitbucket.CommentPayload
			_ = json.Unmarshal(o.Event.Data.Data, &p)
			if p.Path == "main.go" && p.Line == 12 {
				inlineSeen++
			}
		case bitbucket.TypeCommentAgent:
			agent++
		case bitbucket.TypeCommentBot:
			botType++
		case bitbucket.TypeChecksPassed:
			var p bitbucket.ChecksPayload
			_ = json.Unmarshal(o.Event.Data.Data, &p)
			if p.Preview != "https://preview/1" || p.SHA != "aaa1111abc" {
				t.Errorf("checks %+v", p)
			}
		}
	}
	if human != 2 || agent != 1 || botType != 1 || inlineSeen != 1 {
		t.Fatalf("comments human %d agent %d bot %d inline %d", human, agent, botType, inlineSeen)
	}
}

func signed(event string, payload map[string]any) (http.Header, []byte) {
	payload["repository"] = map[string]any{"full_name": "acme/app"}
	body, _ := json.Marshal(payload)
	h := http.Header{}
	h.Set("X-Event-Key", event)
	h.Set("X-Hub-Signature", "sha256="+tracker.SignHMAC("s3cret", body))
	h.Set("X-Request-UUID", "{"+tracker.Digest(string(body))[:12]+"}")
	return h, body
}

func TestPushMatchesReconcileAndRoute(t *testing.T) {
	f, c, s := newEnv(t)
	f.AddPR(1, "Fix it", "agent/issue/PROJ-7", "aaa1111abc")
	cm := f.AddComment(1, "Ada", "please rename")
	f.SetStatus("aaa1111abc", "build", "FAILED", "https://ci/1")
	in := bitbucket.NewIngress(s, c, f.Now, bitbucket.IngressOptions{})
	src := extension.Source{Namespace: "bitbucket/acme/app", ScopeID: "proj"}
	snap, err := in.Reconcile(context.Background(), extension.Scan{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]extension.Observation{}
	for _, o := range snap.Observations {
		byID[o.ID] = o
	}
	for _, d := range []struct {
		event string
		p     map[string]any
		id    string
	}{
		{"pullrequest:updated", map[string]any{"pullrequest": map[string]any{"id": 1}}, "pr/1"},
		{"pullrequest:comment_created", map[string]any{"pullrequest": map[string]any{"id": 1}, "comment": map[string]any{"id": cm.ID}}, "pr/1/comment/" + jsonInt(cm.ID)},
		{"repo:commit_status_updated", map[string]any{"commit_status": map[string]any{"commit": map[string]any{"hash": "aaa1111abc"}}}, "pr/1/checks"},
	} {
		h, body := signed(d.event, d.p)
		del, err := bitbucket.VerifyDelivery(s, "s3cret", h, body, f.Now())
		if err != nil {
			t.Fatal(err)
		}
		res, err := in.Push(context.Background(), src, del)
		if err != nil || len(res.Observations) != 1 || len(res.Events) != 1 {
			t.Fatalf("%s: %+v %v", d.event, res, err)
		}
		if res.Observations[0].Event.DeliveryID != byID[d.id].Event.DeliveryID {
			t.Errorf("%s: push and reconcile identity differ", d.event)
		}
	}
	h, body := signed("pullrequest:updated", map[string]any{"pullrequest": map[string]any{"id": 1}})
	h.Set("X-Hub-Signature", "sha256=00")
	if _, err := bitbucket.VerifyDelivery(s, "s3cret", h, body, f.Now()); !errors.Is(err, tracker.ErrSignature) {
		t.Fatal("forged delivery")
	}
	// Route: input for existing Work only.
	meta, _ := json.Marshal(map[string]any{"extensions": []map[string]any{{"type": "bitbucket", "config": json.RawMessage(entry)}}})
	scope := domain.Scope{ID: "proj", Config: domain.Payload{Data: meta}}
	work := &domain.Work{ID: "w1", CorrelationKeys: []domain.CorrelationKey{{Source: "jira/acme/proj", ExternalID: "issue/PROJ-7"}}}
	route := func(o extension.Observation, w *domain.Work) bool {
		d, err := bitbucket.Route{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: o.Event}, Scope: scope, Work: w})
		if err != nil {
			t.Fatal(err)
		}
		return d.Match
	}
	if route(byID["pr/1"], nil) || route(byID["pr/1"], work) {
		t.Fatal("an open PR is never input and never creates Work")
	}
	if !route(byID["pr/1/checks"], work) || !route(byID["pr/1/comment/"+jsonInt(cm.ID)], work) {
		t.Fatal("failed checks and human comments are input")
	}
	f.SetState(1, "MERGED")
	merged, _ := in.Reconcile(context.Background(), extension.Scan{Source: src})
	for _, o := range merged.Observations {
		if o.ID == "pr/1" && (o.Event.Type != bitbucket.TypePullMerged || !route(o, work)) {
			t.Fatalf("merge: %s", o.Event.Type)
		}
	}
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestWebhookHandlerAndEnsureWebhook(t *testing.T) {
	f, c, s := newEnv(t)
	f.AddPR(1, "Fix it", "agent/issue/PROJ-7", "aaa1111abc")
	in := bitbucket.NewIngress(s, c, f.Now, bitbucket.IngressOptions{})
	accepted := 0
	handler := bitbucket.WebhookHandler(s, func(context.Context) (string, error) { return "s3cret", nil }, func(ctx context.Context, d extension.Delivery) error {
		_, err := in.Push(ctx, extension.Source{Namespace: "bitbucket/acme/app", ScopeID: "proj"}, d)
		if err == nil {
			accepted++
		}
		return err
	}, f.Now)
	h, body := signed("pullrequest:created", map[string]any{"pullrequest": map[string]any{"id": 1}})
	req := httptest.NewRequest(http.MethodPost, "/bitbucket/proj/app", strings.NewReader(string(body)))
	req.Header = h
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || accepted != 1 {
		t.Fatalf("delivery %d", w.Code)
	}
	id, err := c.EnsureWebhook(context.Background(), s, "https://example.test/bb", "s3cret", "", false)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	if again, err := c.EnsureWebhook(context.Background(), s, "https://example.test/new", "s3cret", id, false); err != nil || again != id || f.Hooks[id]["url"] != "https://example.test/new" {
		t.Fatal(again, err)
	}
	if gone, err := c.EnsureWebhook(context.Background(), s, "https://example.test/new", "s3cret", "{missing}", false); err != nil || gone == "{missing}" {
		t.Fatalf("a gone hook is made again: %s %v", gone, err)
	}
	if dry, _ := c.EnsureWebhook(context.Background(), s, "x", "s", "", true); dry != "dry-run" {
		t.Fatal(dry)
	}
	if who, err := c.Verify(context.Background()); err != nil || who != "signed in as Fake Owner" {
		t.Fatal(who, err)
	}
}

func TestIngressConformance(t *testing.T) {
	conformance.RunIngress(t, func() conformance.IngressFixture {
		f, c, s := newEnv(t)
		f.AddPR(1, "Fix it", "agent/issue/PROJ-7", "aaa1111abc")
		f.AddPR(2, "Other", "feature", "bbb2222abc")
		in := bitbucket.NewIngress(s, c, f.Now, bitbucket.IngressOptions{BatchUnits: 2})
		src := extension.Source{Namespace: "bitbucket/acme/app", ScopeID: "proj"}
		h, body := signed("pullrequest:updated", map[string]any{"pullrequest": map[string]any{"id": 1}})
		d, err := bitbucket.VerifyDelivery(s, "s3cret", h, body, f.Now())
		if err != nil {
			t.Fatal(err)
		}
		snap, err := in.Reconcile(context.Background(), extension.Scan{Source: src})
		if err != nil || len(snap.Observations) == 0 {
			t.Fatal(snap, err)
		}
		return conformance.IngressFixture{Ingress: in, Source: src, Delivery: d, Canonical: snap.Observations[0], OneShotType: bitbucket.TypeDelivery}
	})
}

func TestSamplesTraceAgainstTheScope(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"extensions": []map[string]any{{"type": "bitbucket", "config": json.RawMessage(entry)}}})
	scope := domain.Scope{ID: "proj", Config: domain.Payload{Data: meta}}
	samples := bitbucket.Route{}.Samples(scope, domain.Route{})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	work := &domain.Work{ID: "w1", CorrelationKeys: []domain.CorrelationKey{{Source: "jira/acme/proj", ExternalID: "issue/PROJ-7"}}}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
		d, err := bitbucket.Route{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: s.Event}, Scope: scope, Work: work})
		if err != nil || !d.Match {
			t.Fatalf("%s must be input for the issue's Work: %+v, %v", s.ID, d, err)
		}
	}
}
