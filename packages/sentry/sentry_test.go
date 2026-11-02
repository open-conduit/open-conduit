package sentry_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/packages/sentry"
	"github.com/open-conduit/open-conduit/packages/sentry/sentrytest"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

const scope = sentry.FixtureScope

var source = sentry.Name + "/" + scope + "/errors"

// harness configures the package against a seeded fake, with Work opened
// for issue 1001 and Work for something else in the same scope.
func harness(t *testing.T, secret string) (*sdktest.Host, *sentrytest.Server) {
	t.Helper()
	fake := sentrytest.New(sentry.FixtureOrg)
	t.Cleanup(fake.Close)
	sentry.SeedFixture(fake)
	fake.SetLatestEvent("1001", json.RawMessage(`{"eventID":"ev-1","title":"TypeError","platform":"javascript","dateCreated":"2026-11-01T09:05:00Z",
		"tags":[{"key":"environment","value":"production"},{"key":"release","value":"web@1.4.2"}],
		"entries":[{"type":"exception","data":{"values":[{"type":"TypeError","value":"cannot read properties of undefined (reading 'id')",
		"stacktrace":{"frames":[{"filename":"node_modules/react/index.js","function":"render","lineNo":10,"inApp":false,"context":[[10,"x"]]},
		{"filename":"app/checkout.ts","function":"submit","lineNo":42,"colNo":7,"inApp":true,"context":[[41,"const user = session.user"],[42,"return user.id"]]}]}}]}}]}`))
	fake.SetCommitters("ev-1", json.RawMessage(`{"committers":[{"author":{"name":"Sam"},"commits":[{"id":"abc123","message":"refactor checkout session","repository":{"name":"acme/web","url":"https://github.com/acme/web"}}]}]}`))
	secrets := map[string]string{}
	if secret != "" {
		secrets[sentry.FixtureSecret] = secret
	}
	ns := sentry.Name + "/" + sentry.FixtureOrg
	h := sdktest.New(t, sentry.Build(sentry.Options{}),
		sdktest.WithWorks(
			domain.Work{ID: "sentry/acme/1001", ScopeID: scope, State: domain.WorkRunning, CorrelationKeys: []domain.CorrelationKey{{Source: ns, ExternalID: "1001"}}},
			domain.Work{ID: "incident", ScopeID: scope, State: domain.WorkRunning},
		),
		sdktest.WithSecrets(scope, secrets),
		sdktest.WithConnectionSecrets(scope, "sentry", map[string]string{sentry.SecretToken: sentry.FixtureToken}))
	conn := json.RawMessage(`{"api_url":"` + fake.URL + `"}`)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: sentry.Name, Name: "errors",
		Config: sentry.FixtureEntry(), Secrets: []string{sentry.FixtureSecret},
		Connection: &protocol.ConnectionEntry{ID: "sentry", Type: sentry.Name, Config: conn, Secrets: []string{sentry.SecretToken}}}}}); err != nil {
		t.Fatal(err)
	}
	return h, fake
}

func webhook(t *testing.T, h *sdktest.Host, header http.Header, body []byte) int {
	t.Helper()
	var out protocol.WebhookHandleResult
	if err := h.Call(context.Background(), "webhook.handle", protocol.WebhookHandleParams{Path: "/" + source, Method: http.MethodPost, Header: header, Body: body, ReceivedAt: sdktest.Start}, &out); err != nil {
		t.Fatal(err)
	}
	return out.Status
}

func TestWebhookVerifiesAndDeduplicatesByIssue(t *testing.T) {
	h, fake := harness(t, "client-secret")
	body := fake.IssueWebhook("created", "1001")
	if got := webhook(t, h, sentrytest.Headers("client-secret", "issue", "r1", body), body); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	// Sentry redelivering, or the next alert on the same issue, adds no
	// second issue Event, and the alert carries the same correlation key.
	if got := webhook(t, h, sentrytest.Headers("client-secret", "issue", "r2", body), body); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	alert := fake.AlertWebhook("Page on checkout errors", "1001", "ev-1")
	if got := webhook(t, h, sentrytest.Headers("client-secret", "event_alert", "r3", alert), alert); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	events := h.Events()
	if len(events) != 2 || events[0].Event.Type != sentry.TypeOpened || events[1].Event.Type != sentry.TypeAlert {
		t.Fatalf("events %+v", events)
	}
	for _, e := range events {
		if e.Event.CorrelationKey == nil || e.Event.CorrelationKey.ExternalID != "1001" {
			t.Fatalf("correlation %+v", e.Event.CorrelationKey)
		}
	}
	// A project the entry doesn't watch is accepted and dropped.
	other := fake.IssueWebhook("created", "1004")
	if got := webhook(t, h, sentrytest.Headers("client-secret", "issue", "r4", other), other); got != http.StatusOK || len(h.Events()) != 2 {
		t.Fatalf("unwatched project: status %d, %d events", got, len(h.Events()))
	}
}

func TestWebhookFailsClosed(t *testing.T) {
	h, fake := harness(t, "client-secret")
	body := fake.IssueWebhook("created", "1001")
	bad := sentrytest.Headers("wrong-secret", "issue", "r1", body)
	if got := webhook(t, h, bad, body); got != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", got)
	}
	unsigned := sentrytest.Headers("client-secret", "issue", "r1", body)
	unsigned.Del("Sentry-Hook-Signature")
	if got := webhook(t, h, unsigned, body); got != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", got)
	}
	if len(h.Events()) != 0 {
		t.Fatal("an unverified delivery was accepted")
	}
	// With the secret unresolvable nothing is accepted either.
	h2, fake2 := harness(t, "")
	body = fake2.IssueWebhook("created", "1001")
	if got := webhook(t, h2, sentrytest.Headers("", "issue", "r1", body), body); got != http.StatusServiceUnavailable || len(h2.Events()) != 0 {
		t.Fatalf("no secret: %d", got)
	}
}

func TestVerifyNeverAcceptsAnEmptySecret(t *testing.T) {
	body := []byte(`{}`)
	if _, err := sentry.Verify("", sentrytest.Headers("", "issue", "r", body), body, sdktest.Start); !errors.Is(err, sentry.ErrSignature) {
		t.Fatalf("empty secret verified: %v", err)
	}
}

func TestViewReadsTheEventAndSuspects(t *testing.T) {
	h, _ := harness(t, "client-secret")
	ctx := context.Background()
	got, err := sdktest.Call[sentry.ViewOutput](ctx, h, sentry.OpView, map[string]any{})
	if err == nil {
		t.Fatalf("the default sdktest Work has no issue, yet view answered %+v", got)
	}
	res, err := h.OperationAs(ctx, extension.Authority{ScopeID: scope, WorkID: "sentry/acme/1001"}, sentry.OpView, domain.Payload{Data: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var v sentry.ViewOutput
	if err := json.Unmarshal(res.Output.Data, &v); err != nil {
		t.Fatal(err)
	}
	if !v.Own || v.ShortID != "WEB-1" || v.Event == nil || v.Event.Release != "web@1.4.2" || v.Event.Environment != "production" || len(v.Event.Exceptions) != 1 {
		t.Fatalf("view %+v", v)
	}
	frames := v.Event.Exceptions[0].Frames
	if len(frames) != 2 || frames[0].Context != nil || frames[1].File != "app/checkout.ts" || len(frames[1].Context) != 2 {
		t.Fatalf("frames %+v", frames)
	}
	if len(v.Suspects) != 1 || v.Suspects[0].URL != "https://github.com/acme/web/commit/abc123" {
		t.Fatalf("suspects %+v", v.Suspects)
	}
	// Work opened for something else may read an issue by ID.
	res, err = h.OperationAs(ctx, extension.Authority{ScopeID: scope, WorkID: "incident"}, sentry.OpView, domain.Payload{Data: json.RawMessage(`{"issue":"1002"}`)})
	if err != nil || !json.Valid(res.Output.Data) {
		t.Fatalf("read by ID: %v", err)
	}
	if err := json.Unmarshal(res.Output.Data, &v); err != nil || v.Own || v.ShortID != "WEB-2" || len(v.Incomplete) != 1 {
		t.Fatalf("read by ID %+v", v)
	}
}

func TestChangesOnlyTheWorksOwnIssue(t *testing.T) {
	h, fake := harness(t, "client-secret")
	ctx := context.Background()
	own := extension.Authority{ScopeID: scope, WorkID: "sentry/acme/1001"}
	comment := domain.Payload{Data: json.RawMessage(`{"body":"Caused by abc123; fix in acme/web#7"}`)}
	if _, err := h.OperationAs(ctx, own, sentry.OpComment, comment); err != nil {
		t.Fatal(err)
	}
	if c := fake.Comments(); len(c) != 1 || c[0].Issue != "1001" {
		t.Fatalf("comments %+v", c)
	}
	if _, err := h.OperationAs(ctx, own, sentry.OpResolve, domain.Payload{Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if i, _ := fake.Issue("1001"); i.Status != "resolved" {
		t.Fatalf("status %s", i.Status)
	}
	// Other Work can't change an issue, and there is no input to name one.
	other := extension.Authority{ScopeID: scope, WorkID: "incident"}
	if _, err := h.OperationAs(ctx, other, sentry.OpResolve, domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("other Work resolved an issue")
	}
	if _, err := h.OperationAs(ctx, other, sentry.OpResolve, domain.Payload{Data: json.RawMessage(`{"issue":"1002"}`)}); err == nil {
		t.Fatal("resolve took an issue")
	}
	// Work in another scope is refused outright.
	if _, err := h.OperationAs(ctx, extension.Authority{ScopeID: "elsewhere", WorkID: "sentry/acme/1001"}, sentry.OpView, domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("another scope's authority was served")
	}
}

func TestReconcileMatchesPush(t *testing.T) {
	_, fake := harness(t, "client-secret")
	settings, _ := sentry.ParseSettings(sentry.FixtureEntry(), nil)
	in := &sentry.Ingress{Settings: settings, Client: sentry.NewClient(fake.URL, sentry.FixtureOrg, func(context.Context) (string, error) { return sentry.FixtureToken, nil }, nil), Now: sdktest.Start.Local}
	src := extension.Source{Namespace: settings.Namespace, ScopeID: scope}
	scan := extension.Scan{Source: src}
	seen := map[string]extension.Observation{}
	for range 10 {
		page, err := in.Reconcile(context.Background(), scan)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Observations {
			seen[o.ID] = o
		}
		if page.Complete {
			break
		}
		scan.ScanID, scan.Cursor = page.ScanID, page.NextCursor
	}
	if len(seen) != 3 || seen["1003"].Event.Type != sentry.TypeRegressed || seen["1004"].ID != "" {
		t.Fatalf("reconciled %+v", seen)
	}
	body := fake.IssueWebhook("created", "1001")
	d, err := sentry.Verify("k", sentrytest.Headers("k", "issue", "r", body), body, sdktest.Start)
	if err != nil {
		t.Fatal(err)
	}
	res, err := in.Push(context.Background(), src, d)
	if err != nil || len(res.Observations) != 1 {
		t.Fatal(err)
	}
	if got, want := res.Observations[0], seen["1001"]; got.Version != want.Version || got.Event.DeliveryID != want.Event.DeliveryID || string(got.Event.Data.Data) != string(want.Event.Data.Data) {
		t.Fatalf("push %+v\nreconcile %+v", got, want)
	}
}

func TestRouteOpensOneWorkPerIssue(t *testing.T) {
	h, _ := harness(t, "client-secret")
	ctx := context.Background()
	ns := sentry.Name + "/" + sentry.FixtureOrg
	ev := domain.Event{ID: "e1", Type: sentry.TypeRegressed, Source: ns, CorrelationKey: &domain.CorrelationKey{Source: ns, ExternalID: "1003"},
		Data: domain.Payload{Schema: sentry.SchemaIssue, Data: json.RawMessage(`{"short_id":"WEB-3","title":"Error: regressed","level":"error"}`)}}
	route := domain.Route{Name: "errors", Handler: sentry.Name, Binding: "triage"}
	d, err := h.Route(ctx, sentry.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: ev}, Route: route})
	if err != nil || !d.Match || d.CreateID != "sentry/acme/1003" || d.Kind != sentry.WorkKind {
		t.Fatalf("regression: %+v %v", d, err)
	}
	route.Config = domain.Payload{Data: json.RawMessage(`{"triggers":["alert"],"levels":["fatal"]}`)}
	if d, _ := h.Route(ctx, sentry.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: ev}, Route: route}); d.Match {
		t.Fatalf("a trigger the route doesn't admit matched: %+v", d)
	}
	// A later change reaches the issue's Work whatever the triggers.
	ev.Type = sentry.TypeResolved
	if d, _ := h.Route(ctx, sentry.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: ev}, Route: route, Work: &domain.Work{ID: "sentry/acme/1003"}}); !d.Match {
		t.Fatalf("a change to an issue with Work didn't match: %+v", d)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"org":"a/b"}`, `{"org":"acme","namespace":"other"}`, `{"org":"acme","interval_seconds":5}`, `{"org":"acme","bogus":1}`} {
		if _, err := sentry.ParseSettings(json.RawMessage(raw), nil); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if _, err := sentry.ParseSettings(json.RawMessage(`{"org":"acme","webhook_secret":"S"}`), []string{"T"}); err == nil {
		t.Error("a webhook secret outside secret_names was accepted")
	}
	if _, err := sentry.ParseRoute(json.RawMessage(`{"triggers":["resolved"]}`)); err == nil {
		t.Error("an unknown trigger was accepted")
	}
}
