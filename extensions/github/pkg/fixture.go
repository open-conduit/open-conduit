package githubext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/github/githubtest"
	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that serves the GitHub fake for the run.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// Fixture identities: an engineering scope watching one repository of the
// fake, signed in with a token, with a webhook secret.
const (
	FixtureScope  = "eng"
	FixtureRepo   = "acme/widgets"
	FixtureToken  = "ghp-certify"
	fixtureSecret = "certify-secret"
	fixtureEntry  = "widgets"
)

// FixtureEntry is the github entry config the fixture configures.
func FixtureEntry() json.RawMessage {
	return json.RawMessage(`{"namespace":"github/` + FixtureRepo + `","scope":"` + FixtureScope + `","repo":"` + FixtureRepo + `","webhook_secret":"webhook_secret"}`)
}

var alice = githubtest.User{Login: "alice", Type: "User"}

// NewFixture builds the certification fixture against a running fake: an
// issue and filler (several reconcile pages), its issues webhook delivery,
// and the canonical observation the in-process reference ingress computes
// over the same fake, with one route case and one operation case.
func NewFixture(fake *githubtest.Server) (protocol.Fixture, error) {
	fake.AddToken(FixtureToken)
	n := fake.OpenIssue("The export is broken", "it fails on every run", alice)
	for i := range 3 {
		fake.OpenIssue(fmt.Sprintf("filler %d", i), "", alice)
	}
	body := fake.IssuesHook("opened", n)
	now := time.Now().UTC()
	d, err := gh.VerifyDelivery(fixtureSecret, githubtest.Sign(fixtureSecret, "issues", "certify-1", body), body, now)
	if err != nil {
		return protocol.Fixture{}, err
	}
	conn := json.RawMessage(`{"api_url":"` + fake.URL + `"}`)
	cfg, err := gh.ParseConnection(conn)
	if err != nil {
		return protocol.Fixture{}, err
	}
	client, err := gh.NewClient(cfg, func(context.Context, string) (string, error) { return FixtureToken, nil }, gh.ClientOptions{})
	if err != nil {
		return protocol.Fixture{}, err
	}
	settings, err := gh.ParseExtension(FixtureEntry(), []string{"webhook_secret"})
	if err != nil {
		return protocol.Fixture{}, err
	}
	reference := gh.NewIngress(settings, client, time.Now, gh.IngressOptions{})
	src := extension.Source{Namespace: settings.Namespace, ScopeID: FixtureScope}
	res, err := reference.Push(context.Background(), src, d)
	if err != nil || len(res.Observations) != 1 {
		return protocol.Fixture{}, fmt.Errorf("reference push: %v (%d observations)", err, len(res.Observations))
	}
	canonical := res.Observations[0]

	// A route case: the canonical issue starts Work.
	meta, _ := json.Marshal(map[string]any{"extensions": []map[string]any{{"type": gh.Type, "name": fixtureEntry, "config": FixtureEntry()}}})
	scope := domain.Scope{ID: FixtureScope, Name: FixtureScope, Config: domain.Payload{Schema: "core.config.scope.v1", Data: meta}}
	ev := canonical.Event
	ev.ID = "certify-route-event"
	in := extension.RouteInput{Event: domain.AcceptedEvent{Event: ev, Sequence: 1}, Scope: scope, Route: domain.Route{Name: "issues", Handler: gh.Type, Binding: "agent"}}
	want, err := gh.New().Routes()[gh.Type].Match(context.Background(), in)
	if err != nil {
		return protocol.Fixture{}, err
	}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: gh.Type, Name: fixtureEntry, Config: FixtureEntry(), Secrets: []string{"webhook_secret"},
			Connection: &protocol.ConnectionEntry{ID: "github", Type: gh.Type, Config: conn, Secrets: []string{gh.SecretToken}}}},
		Secrets: map[string]string{gh.SecretToken: FixtureToken, "webhook_secret": fixtureSecret},
		Ingress: &protocol.IngressCase{Source: gh.Type + "/" + FixtureScope + "/" + fixtureEntry, Delivery: protocol.FromDelivery(d),
			Canonical:   protocol.FromPushResult(extension.PushResult{Observations: []extension.Observation{canonical}}).Observations[0],
			OneShotType: gh.TypeDelivery, ReceiveTimeVaries: true},
		Routes: []protocol.RouteCase{{Handler: gh.Type, Input: protocol.FromRouteInput(in), Want: protocol.FromRouteDecision(want)}},
		// The tools bind the calling Work from durable state: an authority
		// for Work the scope does not hold is refused.
		Operations: []protocol.OperationCase{{Operation: gh.OpContext, Input: protocol.Payload{Schema: gh.SchemaTool, Data: json.RawMessage(`{}`)}, Error: protocol.CodeUnauthorized}},
		// The hold executor finishes its attempt at once; with no toolbelt
		// endpoint it ends it as failed, which is still a whole result.
		Executor: &protocol.ExecutorCase{Executor: gh.HoldExecutor, Config: protocol.Payload{Schema: gh.SchemaHold, Data: json.RawMessage(`{}`)}},
	}, nil
}

// ServeFixture runs the certification fixture command: it starts the
// GitHub fake, prints the fixture as one JSON line and serves until stdin
// closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	fake := githubtest.New(FixtureRepo)
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
