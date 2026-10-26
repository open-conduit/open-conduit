package slackext

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	sl "github.com/alsiola/open-conduit/extensions/slack"
	"github.com/alsiola/open-conduit/extensions/slack/slacktest"
	"github.com/alsiola/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that serves the Slack fake for the run.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: []string{"slack-extension", "certify-fixture"}}
}

// Fixture identities: a support scope watching one channel of the fake
// workspace, reconcile only (receiver none), so the runner needs no socket.
const (
	FixtureScope   = "support"
	FixtureTeam    = "T0TEAM"
	FixtureChannel = "C0SUPPORT"
	FixtureToken   = "xoxb-certify"
	fixtureSecret  = "signing-secret"
)

// FixtureEntry is the slack entry config the fixture configures.
func FixtureEntry() json.RawMessage {
	return json.RawMessage(`{"namespace":"slack/` + FixtureTeam + `","scope":"` + FixtureScope + `","team":"` + FixtureTeam + `","channels":["` + FixtureChannel + `"],"bot_user_id":"UBOT","receiver":"none"}`)
}

// NewFixture builds the certification fixture against a running fake: a
// root message and filler (several reconcile pages), a deleted message for
// the push-only removal family, and the canonical observation computed by
// the in-process reference ingress over the same fake. It also returns one
// route case.
func NewFixture(fake *slacktest.Server) (protocol.Fixture, error) {
	// The fake's clock runs a minute behind, so every message is in the
	// past of the extension's own clock when it reconciles.
	fake.SetClock(time.Now().UTC().Truncate(time.Second).Add(-time.Minute))
	fake.AddToken(FixtureToken)
	fake.Join(FixtureChannel, "UBOB")
	root := fake.Post(FixtureChannel, "UALICE", "the export is broken")
	for _, text := range []string{"filler one", "filler two", "filler three"} {
		fake.Post(FixtureChannel, "UBOB", text)
	}
	gone := fake.Post(FixtureChannel, "UBOB", "oops")
	prev := fake.Delete(FixtureChannel, gone)
	at := fake.Now()
	body := fake.MessageEvent(FixtureChannel, root)
	v, err := sl.VerifyDelivery(fixtureSecret, slacktest.Headers(fixtureSecret, body, at), body, at)
	if err != nil {
		return protocol.Fixture{}, err
	}
	del := fake.DeletedEvent(FixtureChannel, prev)
	dv, err := sl.VerifyDelivery(fixtureSecret, slacktest.Headers(fixtureSecret, del, at), del, at)
	if err != nil {
		return protocol.Fixture{}, err
	}
	settings, err := sl.ParseExtension(FixtureEntry(), nil)
	if err != nil {
		return protocol.Fixture{}, err
	}
	conn := json.RawMessage(`{"api_url":"` + fake.URL + `"}`)
	cfg, err := sl.ParseConnection(conn)
	if err != nil {
		return protocol.Fixture{}, err
	}
	client, err := sl.NewClient(cfg, func(context.Context, string) (string, error) { return FixtureToken, nil }, sl.ClientOptions{})
	if err != nil {
		return protocol.Fixture{}, err
	}
	reference := sl.NewIngress(settings, client, time.Now, sl.IngressOptions{})
	src := extension.Source{Namespace: settings.Namespace, ScopeID: FixtureScope}
	res, err := reference.Push(context.Background(), src, v.Delivery)
	if err != nil || len(res.Observations) != 1 {
		return protocol.Fixture{}, err
	}
	canonical := res.Observations[0]
	name := Name + "/" + FixtureScope + "/helpdesk"
	oneShot := protocol.FromDelivery(dv.Delivery)

	// A route case: the canonical message starts a conversation.
	meta, _ := json.Marshal(map[string]any{"extensions": []map[string]any{{"type": sl.Type, "config": FixtureEntry()}}})
	scope := domain.Scope{ID: FixtureScope, Name: FixtureScope, Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
	ev := canonical.Event
	ev.ID = "certify-route-event"
	in := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 1}, Scope: scope, Route: domain.Route{Name: "threads", Handler: sl.Type, Binding: "agent"}}
	want, err := sl.Route{}.Match(context.Background(), in)
	if err != nil {
		return protocol.Fixture{}, err
	}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: sl.Type, Name: "helpdesk", Config: FixtureEntry(),
			Connection: &protocol.ConnectionEntry{ID: "slack", Type: sl.Type, Config: conn, Secrets: []string{sl.SecretBotToken}}}},
		Secrets: map[string]string{sl.SecretBotToken: FixtureToken},
		Ingress: &protocol.IngressCase{Source: name, Delivery: protocol.FromDelivery(v.Delivery),
			Canonical:   protocol.FromPushResult(extension.PushResult{Observations: []extension.Observation{canonical}}).Observations[0],
			OneShotType: sl.TypeMessageRemoved, OneShot: &oneShot, ReceiveTimeVaries: true},
		Routes: []protocol.RouteCase{{Handler: sl.Type, Input: protocol.FromRouteInput(in), Want: protocol.FromRouteDecision(want)}},
		// The tools bind the calling Work from durable state: an authority
		// for Work the scope does not hold is refused.
		Operations: []protocol.OperationCase{{Operation: sl.OpContext, Input: protocol.Payload{Schema: "slack.tool.v1", Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized}},
	}, nil
}

// ServeFixture runs the certification fixture command: it starts the Slack
// fake, prints the fixture as one JSON line and serves until stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	fake := slacktest.New(FixtureTeam)
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
