package sentry

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/packages/sentry/sentrytest"
	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that serves the Sentry fake for the run.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: []string{"sentry-extension", "certify-fixture"}}
}

// Fixture identities.
const (
	FixtureScope  = "incidents"
	FixtureOrg    = "acme"
	FixtureToken  = "sntrys-certify"
	FixtureSecret = "sentry-client-secret"
	fixtureClient = "client-secret"
)

// FixtureEntry is the sentry entry config the fixture configures.
func FixtureEntry() json.RawMessage {
	return json.RawMessage(`{"scope":"` + FixtureScope + `","org":"` + FixtureOrg + `","projects":["web"],"webhook_secret":"` + FixtureSecret + `"}`)
}

// SeedFixture fills a fake with the fixture's issues: enough unresolved
// ones for several reconcile pages, one in a project the entry doesn't
// watch, and a resolved one.
func SeedFixture(fake *sentrytest.Server) {
	fake.AddToken(FixtureToken)
	fake.SetPageSize(2)
	web := sentrytest.Project{ID: "11", Slug: "web"}
	fake.Put(sentrytest.Issue{ID: "1001", ShortID: "WEB-1", Title: "TypeError: cannot read properties of undefined (reading 'id')", Culprit: "app/checkout.ts in submit",
		Level: "error", Status: "unresolved", Substatus: "new", Permalink: "https://acme.sentry.io/issues/1001/", FirstSeen: "2026-11-01T09:00:00Z",
		LastSeen: "2026-11-01T09:05:00Z", Count: "12", UserCount: 4, Project: web})
	fake.Put(sentrytest.Issue{ID: "1002", ShortID: "WEB-2", Title: "Error: timeout", Level: "warning", Status: "unresolved", Substatus: "ongoing", FirstSeen: "2026-10-20T09:00:00Z", Project: web})
	fake.Put(sentrytest.Issue{ID: "1003", ShortID: "WEB-3", Title: "Error: regressed", Level: "error", Status: "unresolved", Substatus: "regressed", FirstSeen: "2026-10-01T09:00:00Z", Project: web})
	fake.Put(sentrytest.Issue{ID: "1004", ShortID: "API-1", Title: "Error: elsewhere", Level: "error", Status: "unresolved", Substatus: "new", Project: sentrytest.Project{ID: "12", Slug: "api"}})
	fake.Put(sentrytest.Issue{ID: "1005", ShortID: "WEB-4", Title: "Error: fixed", Level: "error", Status: "resolved", Project: web})
}

// NewFixture builds the certification fixture against a running fake: the
// canonical observation comes from the in-process reference ingress over
// the same verified delivery, and the alert delivery is the one-shot family.
func NewFixture(fake *sentrytest.Server) (protocol.Fixture, error) {
	SeedFixture(fake)
	at := time.Date(2026, 11, 1, 9, 5, 0, 0, time.UTC)
	body := fake.IssueWebhook("created", "1001")
	d, err := Verify(fixtureClient, sentrytest.Headers(fixtureClient, "issue", "req-1", body), body, at)
	if err != nil {
		return protocol.Fixture{}, err
	}
	alert := fake.AlertWebhook("Page on checkout errors", "1001", "ev-1")
	ad, err := Verify(fixtureClient, sentrytest.Headers(fixtureClient, "event_alert", "req-2", alert), alert, at)
	if err != nil {
		return protocol.Fixture{}, err
	}
	settings, err := ParseSettings(FixtureEntry(), nil)
	if err != nil {
		return protocol.Fixture{}, err
	}
	reference := &Ingress{Settings: settings, Client: NewClient(fake.URL, FixtureOrg, func(context.Context) (string, error) { return FixtureToken, nil }, nil), Now: time.Now}
	src := extension.Source{Namespace: settings.Namespace, ScopeID: FixtureScope}
	res, err := reference.Push(context.Background(), src, d)
	if err != nil || len(res.Observations) != 1 {
		return protocol.Fixture{}, err
	}
	canonical := res.Observations[0]
	oneShot := protocol.FromDelivery(ad)
	conn := json.RawMessage(`{"api_url":"` + fake.URL + `"}`)

	ev := canonical.Event
	ev.ID = "certify-route-event"
	in := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 1}, Scope: domain.Scope{ID: FixtureScope, Name: FixtureScope},
		Route: domain.Route{Name: "errors", Handler: Name, Binding: "triage"}}
	resolved := ev
	resolved.Type, resolved.ID = TypeResolved, "certify-resolved"
	skip := extension.RouteInput{Event: domain.AcceptedEvent{Event: resolved, Sequence: 2}, Scope: in.Scope, Route: in.Route}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: Name, Name: "errors", Config: FixtureEntry(), Secrets: []string{FixtureSecret},
			Connection: &protocol.ConnectionEntry{ID: "sentry", Type: Name, Config: conn, Secrets: []string{SecretToken}}}},
		Secrets: map[string]string{SecretToken: FixtureToken, FixtureSecret: fixtureClient},
		Ingress: &protocol.IngressCase{Source: Name + "/" + FixtureScope + "/errors", Delivery: protocol.FromDelivery(d),
			Canonical:   protocol.FromPushResult(res).Observations[0],
			OneShotType: TypeAlert, OneShot: &oneShot, ReceiveTimeVaries: true},
		Routes: []protocol.RouteCase{
			// A new issue opens Work for the issue, named for it.
			{Handler: Name, Input: protocol.FromRouteInput(in), Want: protocol.RouteDecision{Match: true, CreateID: "sentry/acme/1001", Kind: WorkKind,
				Reason:  "a new opened the route admits",
				Display: protocol.FromDisplay(Display("1001", "WEB-1", "TypeError: cannot read properties of undefined (reading 'id')", "https://acme.sentry.io/issues/1001/"))}},
			// A resolution opens nothing for an issue without Work.
			{Handler: Name, Input: protocol.FromRouteInput(skip), Want: protocol.RouteDecision{Reason: "not a trigger this route admits"}},
		},
		// The operations bind the calling Work from durable state: Work the
		// scope doesn't hold is refused, and a comment needs a body.
		Operations: []protocol.OperationCase{
			{Operation: OpView, Input: protocol.Payload{Schema: "sentry.issue.view.request.v1", Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized},
			{Operation: OpResolve, Input: protocol.Payload{Schema: "sentry.issue.resolve.request.v1", Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized},
			{Operation: OpComment, Input: protocol.Payload{Schema: "sentry.issue.comment.request.v1", Data: json.RawMessage(`{}`)}, Error: "bad_request"},
		},
	}, nil
}

// ServeFixture runs the certification fixture command: it starts the
// Sentry fake, prints the fixture as one JSON line and serves until stdin
// closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	fake := sentrytest.New(FixtureOrg)
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
