package bitbucketext

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
	"github.com/open-conduit/open-conduit/extensions/bitbucket"
	"github.com/open-conduit/open-conduit/extensions/bitbucket/bitbuckettest"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/protocol"
)

const (
	fixtureScope = "eng"
	fixtureEntry = "app"
	fixtureRepo  = "acme/app"
	fixtureToken = "bb-certify"
	fixtureHook  = "certify-hook"
)

// fixtureConfig is the fixture's entry. It names no tracker, so its pull
// requests key their own namespace and the runner needs no tracker Work.
var fixtureConfig = json.RawMessage(`{"namespace":"bitbucket/` + fixtureRepo + `","scope":"` + fixtureScope + `","repo":"` + fixtureRepo + `","webhook_secret":"hook"}`)

// Certification is the certification file: the runner asks the package
// for its fixture through `open-conduit extension-run bitbucket
// certify-fixture`.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// NewFixture is the conformance runner's fixture against fake: the entry
// and its connection, and the ingress case of a signed pull request
// delivery, whose canonical observation is the in-process ingress's push.
func NewFixture(fake *bitbuckettest.Fake) (protocol.Fixture, error) {
	fake.AddPR(1, "Fix the export", "agent/fix-export", "aaa1111abc")
	body := []byte(`{"pullrequest":{"id":1},"repository":{"full_name":"` + fixtureRepo + `"}}`)
	h := http.Header{}
	h.Set("X-Event-Key", "pullrequest:created")
	h.Set("X-Request-UUID", "certify-1")
	h.Set("X-Hub-Signature", tracker.SignHMAC(fixtureHook, body))
	conn := json.RawMessage(`{"api_url":"` + fake.URL() + `","username":"me@example.com"}`)
	cfg, err := bitbucket.ParseConnection(conn)
	if err != nil {
		return protocol.Fixture{}, err
	}
	st, err := bitbucket.ParseExtension(fixtureConfig, []string{"hook"})
	if err != nil {
		return protocol.Fixture{}, err
	}
	d, err := bitbucket.VerifyDelivery(st, fixtureHook, h, body, time.Now())
	if err != nil {
		return protocol.Fixture{}, err
	}
	client := bitbucket.NewClient(cfg, func(context.Context, string) (string, error) { return fixtureToken, nil }, bitbucket.ClientOptions{})
	res, err := bitbucket.NewIngress(st, client, time.Now, bitbucket.IngressOptions{}).Push(context.Background(), extension.Source{Namespace: st.Namespace, ScopeID: domain.ID(fixtureScope)}, d)
	if err != nil || len(res.Observations) == 0 {
		return protocol.Fixture{}, fmt.Errorf("reference push: %v (%d observations)", err, len(res.Observations))
	}
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: fixtureScope, Type: Name, Name: fixtureEntry, Config: fixtureConfig, Secrets: []string{"hook"},
			Connection: &protocol.ConnectionEntry{ID: Name, Type: Name, Config: conn, Secrets: []string{bitbucket.SecretToken}}}},
		Secrets: map[string]string{bitbucket.SecretToken: fixtureToken, "hook": fixtureHook},
		Ingress: &protocol.IngressCase{Source: Name + "/" + fixtureScope + "/" + fixtureEntry, Delivery: protocol.FromDelivery(d),
			Canonical:   protocol.FromPushResult(extension.PushResult{Observations: res.Observations[:1]}).Observations[0],
			OneShotType: bitbucket.TypeDelivery, ReceiveTimeVaries: true},
	}, nil
}

// ServeFixture writes the fixture to stdout and keeps its fake Bitbucket
// up until the runner closes stdin.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	fake := bitbuckettest.New(fixtureRepo, "me@example.com", fixtureToken)
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

// certifyFixture runs the certify-fixture subcommand.
func certifyFixture() int {
	if err := ServeFixture(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
