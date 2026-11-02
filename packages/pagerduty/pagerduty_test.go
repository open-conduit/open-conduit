package pagerduty_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/packages/pagerduty"
	"github.com/open-conduit/open-conduit/packages/pagerduty/pagerdutytest"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

const scope = pagerduty.FixtureScope

var source = pagerduty.Name + "/" + scope + "/oncall"

// harness configures the package against a seeded fake, with Work opened
// for incident Q1 and Work for something else in the same scope.
func harness(t *testing.T, secret string, entry json.RawMessage) (*sdktest.Host, *pagerdutytest.Server) {
	t.Helper()
	fake := pagerdutytest.New()
	t.Cleanup(fake.Close)
	pagerduty.SeedFixture(fake)
	secrets := map[string]string{}
	if secret != "" {
		secrets[pagerduty.FixtureSecret] = secret
	}
	h := sdktest.New(t, pagerduty.Build(pagerduty.Options{}),
		sdktest.WithWorks(
			domain.Work{ID: "pagerduty/Q1", ScopeID: scope, State: domain.WorkRunning, CorrelationKeys: []domain.CorrelationKey{{Source: pagerduty.Name, ExternalID: "Q1"}}},
			domain.Work{ID: "other", ScopeID: scope, State: domain.WorkRunning},
		),
		sdktest.WithSecrets(scope, secrets),
		sdktest.WithConnectionSecrets(scope, "pagerduty", map[string]string{pagerduty.SecretToken: pagerduty.FixtureToken}))
	conn := json.RawMessage(`{"api_url":"` + fake.URL + `","from":"` + pagerduty.FixtureFrom + `"}`)
	if _, err := h.Configure(context.Background(), protocol.ConfigureParams{Entries: []protocol.ConfigEntry{{ScopeID: scope, Type: pagerduty.Name, Name: "oncall",
		Config: entry, Secrets: []string{pagerduty.FixtureSecret},
		Connection: &protocol.ConnectionEntry{ID: "pagerduty", Type: pagerduty.Name, Config: conn, Secrets: []string{pagerduty.SecretToken}}}}}); err != nil {
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

var own = extension.Authority{ScopeID: scope, WorkID: "pagerduty/Q1"}

func call(t *testing.T, h *sdktest.Host, a extension.Authority, op, input string) (json.RawMessage, error) {
	t.Helper()
	res, err := h.OperationAs(context.Background(), a, op, domain.Payload{Data: json.RawMessage(input)})
	return res.Output.Data, err
}

func TestWebhookObservesStatusChanges(t *testing.T) {
	h, fake := harness(t, "signing-secret", pagerduty.FixtureEntry())
	body := fake.Webhook("E1", "incident.triggered", "Q1")
	if got := webhook(t, h, pagerdutytest.Headers("signing-secret", body), body); got != http.StatusOK {
		t.Fatalf("status %d", got)
	}
	// Redelivery adds nothing.
	if got := webhook(t, h, pagerdutytest.Headers("signing-secret", body), body); got != http.StatusOK || len(h.Events()) != 1 {
		t.Fatalf("redelivery: %d, %d events", got, len(h.Events()))
	}
	// Acknowledged, then triggered again (an acknowledgement timing out):
	// each is a new Event, because the version carries when the status
	// changed.
	q1, _ := fake.Incident("Q1")
	q1.Status, q1.LastStatusChangeAt = "acknowledged", "2026-11-01T09:10:00Z"
	fake.Put(q1)
	body = fake.Webhook("E2", "incident.acknowledged", "Q1")
	webhook(t, h, pagerdutytest.Headers("signing-secret", body), body)
	q1.Status, q1.LastStatusChangeAt = "triggered", "2026-11-01T09:40:00Z"
	fake.Put(q1)
	body = fake.Webhook("E3", "incident.triggered", "Q1")
	webhook(t, h, pagerdutytest.Headers("signing-secret", body), body)
	events := h.Events()
	if len(events) != 3 || events[1].Event.Type != pagerduty.TypeAcknowledged || events[2].Event.Type != pagerduty.TypeTriggered || events[0].Event.DeliveryID == events[2].Event.DeliveryID {
		t.Fatalf("events %+v", events)
	}
	// A service the entry doesn't watch is accepted and dropped.
	body = fake.Webhook("E4", "incident.triggered", "Q3")
	if got := webhook(t, h, pagerdutytest.Headers("signing-secret", body), body); got != http.StatusOK || len(h.Events()) != 3 {
		t.Fatalf("unwatched service: %d, %d events", got, len(h.Events()))
	}
}

func TestWebhookFailsClosed(t *testing.T) {
	h, fake := harness(t, "signing-secret", pagerduty.FixtureEntry())
	body := fake.Webhook("E1", "incident.triggered", "Q1")
	if got := webhook(t, h, pagerdutytest.Headers("wrong", body), body); got != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", got)
	}
	unsigned := pagerdutytest.Headers("signing-secret", body)
	unsigned.Del("X-PagerDuty-Signature")
	if got := webhook(t, h, unsigned, body); got != http.StatusUnauthorized || len(h.Events()) != 0 {
		t.Fatalf("unsigned: %d", got)
	}
	// While a secret rotates PagerDuty sends both signatures; either verifies.
	rotating := pagerdutytest.Headers("signing-secret", body)
	rotating.Set("X-PagerDuty-Signature", pagerdutytest.Signature("old", body)+","+pagerdutytest.Signature("signing-secret", body))
	if got := webhook(t, h, rotating, body); got != http.StatusOK {
		t.Fatalf("rotating: %d", got)
	}
	h2, fake2 := harness(t, "", pagerduty.FixtureEntry())
	body = fake2.Webhook("E1", "incident.triggered", "Q1")
	if got := webhook(t, h2, pagerdutytest.Headers("", body), body); got != http.StatusServiceUnavailable || len(h2.Events()) != 0 {
		t.Fatalf("no secret: %d", got)
	}
	if _, err := pagerduty.Verify("", pagerdutytest.Headers("", body), body, sdktest.Start); !errors.Is(err, pagerduty.ErrSignature) {
		t.Fatalf("empty secret verified: %v", err)
	}
}

func TestViewAndNote(t *testing.T) {
	h, fake := harness(t, "signing-secret", pagerduty.FixtureEntry())
	raw, err := call(t, h, own, pagerduty.OpView, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var v pagerduty.ViewOutput
	if err := json.Unmarshal(raw, &v); err != nil || v.Number != 101 || v.AcknowledgeAllow || len(v.Alerts) != 1 || len(v.Alerts[0].Details) == 0 {
		t.Fatalf("view %s %v", raw, err)
	}
	if _, err := call(t, h, own, pagerduty.OpNote, `{"body":"Suspect commit abc123; fix in acme/web#7"}`); err != nil {
		t.Fatal(err)
	}
	if n := fake.Notes(); len(n) != 1 || n[0].Incident != "Q1" || n[0].From != pagerduty.FixtureFrom {
		t.Fatalf("notes %+v", n)
	}
	// Other Work has no incident to act on.
	if _, err := call(t, h, extension.Authority{ScopeID: scope, WorkID: "other"}, pagerduty.OpNote, `{"body":"x"}`); err == nil {
		t.Fatal("other Work added a note")
	}
	if _, err := call(t, h, extension.Authority{ScopeID: "elsewhere", WorkID: "pagerduty/Q1"}, pagerduty.OpView, `{}`); err == nil {
		t.Fatal("another scope's authority was served")
	}
}

func TestAcknowledgeIsOffByDefault(t *testing.T) {
	h, fake := harness(t, "signing-secret", pagerduty.FixtureEntry())
	_, err := call(t, h, own, pagerduty.OpAcknowledge, `{}`)
	var pe *extension.OperationError
	if !errors.As(err, &pe) || pe.Code != "not_permitted" {
		t.Fatalf("acknowledge without allow_acknowledge: %v", err)
	}
	if i, _ := fake.Incident("Q1"); i.Status != "triggered" {
		t.Fatalf("status %s", i.Status)
	}
	h2, fake2 := harness(t, "signing-secret", json.RawMessage(`{"scope":"`+scope+`","services":["PWEB"],"webhook_secret":"`+pagerduty.FixtureSecret+`","allow_acknowledge":true}`))
	raw, err := call(t, h2, own, pagerduty.OpAcknowledge, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if i, _ := fake2.Incident("Q1"); i.Status != "acknowledged" || string(raw) == "" {
		t.Fatalf("status %s, output %s", i.Status, raw)
	}
}

func TestReconcileMatchesPush(t *testing.T) {
	_, fake := harness(t, "signing-secret", pagerduty.FixtureEntry())
	settings, _ := pagerduty.ParseSettings(pagerduty.FixtureEntry(), nil)
	in := &pagerduty.Ingress{Settings: settings, Client: pagerduty.NewClient(fake.URL, "", func(context.Context) (string, error) { return pagerduty.FixtureToken, nil }, nil), Now: sdktest.Start.Local}
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
	if len(seen) != 3 || seen["Q2"].Event.Type != pagerduty.TypeAcknowledged || seen["Q3"].ID != "" || seen["Q5"].ID != "" {
		t.Fatalf("reconciled %+v", seen)
	}
	body := fake.Webhook("E1", "incident.triggered", "Q1")
	d, err := pagerduty.Verify("k", pagerdutytest.Headers("k", body), body, sdktest.Start)
	if err != nil {
		t.Fatal(err)
	}
	res, err := in.Push(context.Background(), src, d)
	if err != nil || len(res.Observations) != 1 {
		t.Fatal(err)
	}
	if got, want := res.Observations[0], seen["Q1"]; got.Version != want.Version || string(got.Event.Data.Data) != string(want.Event.Data.Data) {
		t.Fatalf("push %+v\nreconcile %+v", got, want)
	}
}

func TestRoute(t *testing.T) {
	h, _ := harness(t, "signing-secret", pagerduty.FixtureEntry())
	ctx := context.Background()
	ev := domain.Event{ID: "e1", Type: pagerduty.TypeTriggered, Source: pagerduty.Name, CorrelationKey: &domain.CorrelationKey{Source: pagerduty.Name, ExternalID: "Q4"},
		Data: domain.Payload{Schema: pagerduty.SchemaIncident, Data: json.RawMessage(`{"incident":"Q4","number":104,"title":"Queue depth","status":"triggered","urgency":"low"}`)}}
	route := domain.Route{Name: "incidents", Handler: pagerduty.Name, Binding: "triage"}
	if d, err := h.Route(ctx, pagerduty.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: ev}, Route: route}); err != nil || !d.Match || d.CreateID != "pagerduty/Q4" || d.Display.Ref != "#104" {
		t.Fatalf("triggered: %+v %v", d, err)
	}
	route.Config = domain.Payload{Data: json.RawMessage(`{"urgencies":["high"]}`)}
	if d, _ := h.Route(ctx, pagerduty.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: ev}, Route: route}); d.Match {
		t.Fatalf("a low-urgency incident matched a high-only route: %+v", d)
	}
	// Resolution reaches the incident's Work, so it can complete.
	ev.Type = pagerduty.TypeResolved
	if d, _ := h.Route(ctx, pagerduty.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: ev}, Route: route, Work: &domain.Work{ID: "pagerduty/Q4"}}); !d.Match {
		t.Fatalf("resolution didn't reach the Work: %+v", d)
	}
	// So does reconcile finding the incident gone after a missed webhook,
	// and that alone opens nothing.
	absent := domain.Event{ID: "a1", Type: pagerduty.TypeAbsent, Source: "core.ingress", CorrelationKey: ev.CorrelationKey}
	if d, _ := h.Route(ctx, pagerduty.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: absent}, Route: route, Work: &domain.Work{ID: "pagerduty/Q4"}}); !d.Match {
		t.Fatalf("absence didn't reach the Work: %+v", d)
	}
	if d, _ := h.Route(ctx, pagerduty.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: absent}, Route: route}); d.Match {
		t.Fatalf("absence opened Work: %+v", d)
	}
	absent.CorrelationKey = &domain.CorrelationKey{Source: "slack/T1", ExternalID: "Q4"}
	if d, _ := h.Route(ctx, pagerduty.Name, extension.RouteInput{Event: domain.AcceptedEvent{Event: absent}, Route: route, Work: &domain.Work{ID: "x"}}); d.Match {
		t.Fatalf("another source's absence matched: %+v", d)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"scope":"prod","namespace":"other"}`, `{"scope":"prod","interval_seconds":5}`, `{"scope":"prod","bogus":1}`} {
		if _, err := pagerduty.ParseSettings(json.RawMessage(raw), nil); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if _, err := pagerduty.ParseConnection(json.RawMessage(`{"from":"not an email"}`)); err == nil {
		t.Error("a from that isn't an email was accepted")
	}
	if s, err := pagerduty.ParseSettings(json.RawMessage(`{"scope":"prod"}`), nil); err != nil || s.AllowAcknowledge {
		t.Errorf("default settings %+v %v", s, err)
	}
}
