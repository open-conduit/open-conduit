package pagerduty

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/packages/pagerduty/pagerdutytest"
	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that serves the PagerDuty fake for the run.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: []string{"pagerduty-extension", "certify-fixture"}}
}

// Fixture identities.
const (
	FixtureScope   = "incidents"
	FixtureService = "PWEB"
	FixtureToken   = "pd-certify"
	FixtureSecret  = "PAGERDUTY_WEBHOOK_SECRET"
	FixtureFrom    = "oncall-agent@acme.example"
	fixtureSigning = "signing-secret"
)

// FixtureEntry is the pagerduty entry config the fixture configures.
func FixtureEntry() json.RawMessage {
	return json.RawMessage(`{"services":["` + FixtureService + `"],"webhook_secret":"` + FixtureSecret + `"}`)
}

// SeedFixture fills a fake with the fixture's incidents: open ones across
// several reconcile pages, one on a service the entry doesn't watch, and a
// resolved one.
func SeedFixture(fake *pagerdutytest.Server) {
	fake.AddToken(FixtureToken)
	fake.SetPageSize(2)
	web := pagerdutytest.Service{ID: FixtureService, Summary: "Web checkout"}
	fake.Put(pagerdutytest.Incident{ID: "Q1", Number: 101, Title: "Checkout error rate above 5%", Status: "triggered", Urgency: "high",
		HTMLURL: "https://acme.pagerduty.com/incidents/Q1", CreatedAt: "2026-11-01T09:06:00Z", LastStatusChangeAt: "2026-11-01T09:06:00Z", Service: web})
	fake.Put(pagerdutytest.Incident{ID: "Q2", Number: 102, Title: "Latency", Status: "acknowledged", Urgency: "low", LastStatusChangeAt: "2026-11-01T08:00:00Z", Service: web})
	fake.Put(pagerdutytest.Incident{ID: "Q3", Number: 103, Title: "Disk", Status: "triggered", Urgency: "high", LastStatusChangeAt: "2026-11-01T07:00:00Z", Service: pagerdutytest.Service{ID: "PDB"}})
	fake.Put(pagerdutytest.Incident{ID: "Q4", Number: 104, Title: "Queue depth", Status: "triggered", Urgency: "low", LastStatusChangeAt: "2026-11-01T06:00:00Z", Service: web})
	fake.Put(pagerdutytest.Incident{ID: "Q5", Number: 105, Title: "Fixed", Status: "resolved", LastStatusChangeAt: "2026-10-31T06:00:00Z", Service: web})
	fake.SetAlerts("Q1", json.RawMessage(`[{"id":"A1","summary":"Sentry: WEB-1 TypeError in checkout","status":"triggered","created_at":"2026-11-01T09:06:00Z",
		"body":{"details":{"sentry_issue":"1001","url":"https://acme.sentry.io/issues/1001/"}}}]`))
}

// NewFixture builds the certification fixture against a running fake: the
// canonical observation comes from the in-process reference ingress over
// the same verified delivery.
func NewFixture(fake *pagerdutytest.Server) (protocol.Fixture, error) {
	SeedFixture(fake)
	at := time.Date(2026, 11, 1, 9, 6, 0, 0, time.UTC)
	body := fake.Webhook("01EVT", "incident.triggered", "Q1")
	d, err := Verify(fixtureSigning, pagerdutytest.Headers(fixtureSigning, body), body, at)
	if err != nil {
		return protocol.Fixture{}, err
	}
	settings, err := ParseSettings(FixtureEntry(), nil)
	if err != nil {
		return protocol.Fixture{}, err
	}
	reference := &Ingress{Settings: settings, Client: NewClient(fake.URL, FixtureFrom, func(context.Context) (string, error) { return FixtureToken, nil }, nil), Now: time.Now}
	src := extension.Source{Namespace: settings.Namespace, ScopeID: FixtureScope}
	res, err := reference.Push(context.Background(), src, d)
	if err != nil || len(res.Observations) != 1 {
		return protocol.Fixture{}, err
	}
	canonical := res.Observations[0]
	conn := json.RawMessage(`{"api_url":"` + fake.URL + `","from":"` + FixtureFrom + `"}`)

	ev := canonical.Event
	ev.ID = "certify-route-event"
	in := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 1}, Scope: domain.Scope{ID: FixtureScope, Name: FixtureScope},
		Route: domain.Route{Name: "incidents", Handler: Name, Binding: "triage"}}
	acked := ev
	acked.Type, acked.ID = TypeAcknowledged, "certify-acked"
	skip := extension.RouteInput{Event: domain.AcceptedEvent{Event: acked, Sequence: 2}, Scope: in.Scope, Route: in.Route}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: Name, Name: "oncall", Config: FixtureEntry(), Secrets: []string{FixtureSecret},
			Connection: &protocol.ConnectionEntry{ID: "pagerduty", Type: Name, Config: conn, Secrets: []string{SecretToken}}}},
		Secrets: map[string]string{SecretToken: FixtureToken, FixtureSecret: fixtureSigning},
		Ingress: &protocol.IngressCase{Source: Name + "/" + FixtureScope + "/oncall", Delivery: protocol.FromDelivery(d),
			Canonical: protocol.FromPushResult(res).Observations[0]},
		Routes: []protocol.RouteCase{
			// A triggered incident opens Work for the incident, named for it.
			{Handler: Name, Input: protocol.FromRouteInput(in), Want: protocol.RouteDecision{Match: true, CreateID: "pagerduty/Q1", Kind: WorkKind,
				Reason:  "a triggered incident the route admits",
				Display: protocol.FromDisplay(Display(IncidentPayload{Incident: "Q1", Number: 101, Title: "Checkout error rate above 5%", URL: "https://acme.pagerduty.com/incidents/Q1"}))}},
			// An acknowledgement opens nothing for an incident without Work.
			{Handler: Name, Input: protocol.FromRouteInput(skip), Want: protocol.RouteDecision{Reason: "only a triggered incident opens Work"}},
		},
		// The operations bind the calling Work from durable state: Work the
		// scope doesn't hold is refused, and a note needs a body.
		Operations: []protocol.OperationCase{
			{Operation: OpView, Input: protocol.Payload{Schema: "pagerduty.incident.view.request.v1", Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized},
			{Operation: OpAcknowledge, Input: protocol.Payload{Schema: "pagerduty.incident.acknowledge.request.v1", Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized},
			{Operation: OpNote, Input: protocol.Payload{Schema: "pagerduty.incident.note.request.v1", Data: json.RawMessage(`{}`)}, Error: "bad_request"},
		},
	}, nil
}

// ServeFixture runs the certification fixture command: it starts the
// PagerDuty fake, prints the fixture as one JSON line and serves until
// stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	fake := pagerdutytest.New()
	defer fake.Close()
	f, err := NewFixture(fake)
	if err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}
