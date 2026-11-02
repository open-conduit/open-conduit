package trackerext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/protocol"
)

// FixtureScope and FixtureEntry name the certification fixture's entry.
const (
	FixtureScope = "eng"
	FixtureEntry = "work"
	// fixtureHookSecret is the entry's webhook secret's name, and
	// FixtureHookValue its value, which a Fixture signs its delivery with.
	fixtureHookSecret, FixtureHookValue = "hook", "certify-hook"
)

// Fixture is a tracker's certification case: a fake service, the
// connection and entry configured against it, and one delivery the
// service signs (with FixtureHookValue) about an issue the fake holds.
type Fixture struct {
	// Conn and Config are the connection and entry configuration; Config
	// names fixtureHookSecret as its webhook_secret.
	Conn, Config json.RawMessage
	// Token is the connection's token secret.
	Token string
	// Header and Body are the signed webhook delivery.
	Header http.Header
	Body   []byte
	// Close stops the fake service.
	Close func()
}

// Certification is the certification file: the runner asks the package
// for its fixture through `open-conduit extension-run <type>
// certify-fixture`.
func (p Package) Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(p.Kind.Type), "certify-fixture")}
}

// protocolFixture is the conformance runner's fixture for f: the entry and
// its connection, their secrets, and the ingress case, whose canonical
// observation is the in-process ingress's push of the same delivery.
func (p Package) protocolFixture(f Fixture) (protocol.Fixture, error) {
	typ := p.Kind.Type
	secrets := map[string]string{"token": f.Token, fixtureHookSecret: FixtureHookValue}
	secret := func(_ context.Context, name string) (string, error) { return secrets[name], nil }
	settings, svc, err := p.Kind.Open(f.Conn, f.Config, []string{fixtureHookSecret}, secret, time.Now, false)
	if err != nil {
		return protocol.Fixture{}, err
	}
	d, err := tracker.VerifyDelivery(typ, svc, FixtureHookValue, f.Header, f.Body, time.Now())
	if err != nil {
		return protocol.Fixture{}, err
	}
	reference := tracker.NewIngress(settings, svc, time.Now, tracker.IngressOptions{})
	res, err := reference.Push(context.Background(), extension.Source{Namespace: settings.Namespace, ScopeID: domain.ID(FixtureScope)}, d)
	if err != nil || len(res.Observations) == 0 {
		return protocol.Fixture{}, fmt.Errorf("reference push: %v (%d observations)", err, len(res.Observations))
	}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: FixtureScope, Type: typ, Name: FixtureEntry, Config: f.Config, Secrets: []string{fixtureHookSecret},
			Connection: &protocol.ConnectionEntry{ID: typ, Type: typ, Config: f.Conn, Secrets: []string{"token"}}}},
		Secrets: secrets,
		Ingress: &protocol.IngressCase{Source: typ + "/" + FixtureScope + "/" + FixtureEntry, Delivery: protocol.FromDelivery(d),
			Canonical:   protocol.FromPushResult(extension.PushResult{Observations: res.Observations[:1]}).Observations[0],
			OneShotType: tracker.TypesOf(typ).Delivery, ReceiveTimeVaries: true},
	}, nil
}

// serveFixture writes the fixture to stdout and keeps its fake service up
// until the runner closes stdin.
func (p Package) serveFixture(stdin io.Reader, stdout io.Writer) error {
	if p.Fixture == nil {
		return fmt.Errorf("%s has no certification fixture", p.Kind.Type)
	}
	f, err := p.Fixture()
	if err != nil {
		return err
	}
	defer f.Close()
	pf, err := p.protocolFixture(f)
	if err != nil {
		return err
	}
	b, err := json.Marshal(pf)
	if err != nil {
		return err
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}

// certifyFixture runs the certify-fixture subcommand.
func (p Package) certifyFixture() int {
	if err := p.serveFixture(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
