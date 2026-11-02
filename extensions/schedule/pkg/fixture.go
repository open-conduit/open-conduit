package scheduleext

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/protocol"
)

const (
	fixtureScope = "ops"
	fixtureEntry = "jobs"
	fixtureHook  = "certify-hook"
)

// fixtureConfig is the fixture's entry: one job that takes webhooks.
var fixtureConfig = json.RawMessage(`{"namespace":"schedule/` + fixtureScope + `","scope":"` + fixtureScope + `","webhook_secret":"hook","jobs":[{"name":"deploy","on":{"webhook":true}}]}`)

// Certification is the certification file: the runner asks the package
// for its fixture through `open-conduit extension-run schedule
// certify-fixture`.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// NewFixture is the conformance runner's fixture: the entry, its webhook
// secret and the ingress case of one verified job webhook POST. A job
// webhook is push-only (an arbitrary POST with no readable history), so
// the case has no canonical observation, only the one-shot event.
func NewFixture() protocol.Fixture {
	body, _ := json.Marshal(map[string]any{"job": "deploy", "content_type": "application/json", "body": []byte(`{"ref":"main"}`)})
	return protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: fixtureScope, Type: Name, Name: fixtureEntry, Config: fixtureConfig, Secrets: []string{"hook"}}},
		Secrets: map[string]string{"hook": fixtureHook},
		Ingress: &protocol.IngressCase{Source: Name + "/" + fixtureScope + "/" + fixtureEntry,
			Delivery:    protocol.Delivery{ID: "certify-1", ReceivedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), ContentType: schedule.WebhookContentType, Body: body},
			OneShotType: schedule.TypeWebhook},
	}
}

// ServeFixture writes the fixture to stdout and waits for the runner to
// close stdin.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	b, err := json.Marshal(NewFixture())
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
