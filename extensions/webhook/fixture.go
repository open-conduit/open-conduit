package webhook

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: the runner asks the
// package for its fixture through `open-conduit extension-run webhook
// certify-fixture`.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// Fixture identities.
const (
	FixtureScope  = "ops"
	FixtureEntry  = "inbox"
	FixtureSecret = "hook"
	FixtureValue  = "certify-hook"
)

// FixtureSource is the fixture entry's source and webhook mount.
var FixtureSource = Namespace(FixtureScope, FixtureEntry)

// FixtureConfig is the fixture's entry: a deploys hook deduplicated by a
// header and threaded by the body's service, and a forms hook with
// neither key.
func FixtureConfig() json.RawMessage {
	return json.RawMessage(`{"scope":"` + FixtureScope + `","webhook_secret":"` + FixtureSecret + `","hooks":[` +
		`{"name":"deploys","delivery_key":"header:X-Delivery-Id","correlation_key":"body:/service"},{"name":"forms"}]}`)
}

// NewFixture is the conformance runner's fixture: the entry, its secret,
// the ingress case of one accepted POST (a one-shot family, so there is no
// canonical observation) and the route's decisions on it.
func NewFixture() (protocol.Fixture, error) {
	settings, err := ParseSettings(FixtureConfig(), nil)
	if err != nil {
		return protocol.Fixture{}, err
	}
	body, _ := json.Marshal(envelope{Hook: "deploys", Delivery: "d-1", Correlation: "checkout", ContentType: "application/json", Body: []byte(`{"service":"checkout","status":"failed"}`)})
	d := extension.Delivery{ID: "k-certify-1", ReceivedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), ContentType: ContentType, Body: body}
	in := &Ingress{Namespace: FixtureSource, Settings: settings, Now: time.Now}
	res, err := in.Push(context.Background(), extension.Source{Namespace: FixtureSource, ScopeID: FixtureScope}, d)
	if err != nil {
		return protocol.Fixture{}, err
	}
	ev := res.Events[0]
	scope := domain.Scope{ID: FixtureScope, Name: FixtureScope}
	route := domain.Route{Name: "deliveries", Handler: Name, Binding: "triage"}
	p, _ := payload(ev)
	open := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 1}, Scope: scope, Route: route}
	work := domain.Work{ID: domain.ID(ev.Source + "/" + ev.CorrelationKey.ExternalID), ScopeID: FixtureScope, Kind: WorkKind, State: domain.WorkRunning}
	follow := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 2}, Scope: scope, Route: route, Work: &work}
	other := route
	other.Config = domain.Payload{Data: json.RawMessage(`{"hooks":["forms"]}`)}
	skip := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 3}, Scope: scope, Route: other}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: Name, Name: FixtureEntry, Config: FixtureConfig(), Secrets: []string{FixtureSecret}}},
		Secrets: map[string]string{FixtureSecret: FixtureValue},
		Ingress: &protocol.IngressCase{Source: FixtureSource, Delivery: protocol.FromDelivery(d), OneShotType: TypeDelivery},
		Routes: []protocol.RouteCase{
			// A delivery whose thread has no Work opens Work for it.
			{Handler: Name, Input: protocol.FromRouteInput(open), Want: protocol.RouteDecision{Match: true, CreateID: string(work.ID), Kind: WorkKind,
				Reason: "a new delivery to deploys", Display: protocol.FromDisplay(Display(p))}},
			// A later delivery of the thread is its Work's input.
			{Handler: Name, Input: protocol.FromRouteInput(follow), Want: protocol.RouteDecision{Match: true, Reason: "a later delivery of the thread"}},
			// A route limited to other hooks takes nothing.
			{Handler: Name, Input: protocol.FromRouteInput(skip), Want: protocol.RouteDecision{Reason: "a hook this route doesn't take"}},
		},
		// Reading a delivery binds the calling Work from durable state: Work
		// the scope doesn't hold is refused.
		Operations: []protocol.OperationCase{
			{Operation: OpRead, Input: protocol.Payload{Schema: OpRead + ".request.v1", Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized},
		},
	}, nil
}

// ServeFixture writes the fixture to stdout and waits for the runner to
// close stdin.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	f, err := NewFixture()
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
