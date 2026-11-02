package runtimetest

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Event is a fixture.changed Event from fixture/account, received now.
func Event(f conformance.RuntimeFixture, id string, key *domain.CorrelationKey) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: "fixture.changed", Source: "fixture/account", DeliveryID: id, OccurredAt: f.Clock.Now(), ReceivedAt: f.Clock.Now(), CorrelationKey: key}
}

// RequireRoute routes Event id or fails the test.
func RequireRoute(t testing.TB, d conformance.RuntimeDriver, id domain.ID) {
	t.Helper()
	if err := d.Route(id); err != nil {
		t.Fatal(err)
	}
}

// RequireAccept accepts Event(f, id, key) or fails the test.
func RequireAccept(t testing.TB, f conformance.RuntimeFixture, id string, key *domain.CorrelationKey) {
	t.Helper()
	if err := f.Driver.Accept(Event(f, id, key)); err != nil {
		t.Fatal(err)
	}
}

// Read runs fn in a transaction through the public storage contract or fails
// the test.
func Read(t testing.TB, f conformance.RuntimeFixture, fn func(storage.Tx) error) {
	t.Helper()
	if err := conformance.RuntimeRead(f, fn); err != nil {
		t.Fatal(err)
	}
}

// NewScheduler is another production scheduler over a SchedulerFixture's
// store, clock and router, with its own options.
func NewScheduler(t testing.TB, f conformance.RuntimeFixture, o runtime.SchedulerOptions) *runtime.Scheduler {
	t.Helper()
	s, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*SchedulerDriver).Router, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Endpoint stands in for the daemon's per-execution endpoints in harnesses
// that call the toolbelt in process: every execution is told the same fixed
// endpoint, which nothing dials.
func Endpoint(endpoint string) func(context.Context, domain.ID, string) (string, string, error) {
	return func(context.Context, domain.ID, string) (string, string, error) { return endpoint, "", nil }
}

// PinningBroker stands in for the host's sandbox binding: its plan is pinned
// through the scheduler's pin before it is returned.
type PinningBroker struct {
	Pin func(extension.SandboxPlan) error
}

func (b PinningBroker) Plan(_ context.Context, _ extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	p := extension.SandboxPlan{Argv: l.Argv, Digest: "sha256:pinned", Signals: extension.SandboxTerm, Summary: "fake sandbox"}
	return p, b.Pin(p)
}
func (PinningBroker) Adopt(context.Context, int) error { return nil }
