package integration_test

import (
	"path/filepath"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
	"github.com/open-conduit/open-conduit/storage"
)

// The runtime drivers live in internal/runtime/runtimetest, where the
// runtime's own tests run the shared suites; these names keep the
// integration tests that build on them readable.
type (
	routingFixtureExecutor = runtimetest.RouterExecutor
	routingDriver          = runtimetest.RoutingDriver
	routingHandlerFunc     = runtimetest.HandlerFunc
	schedulerHandle        = runtimetest.Handle
	schedulerExecutor      = runtimetest.Executor
	schedulerDriver        = runtimetest.SchedulerDriver
	pinningBroker          = runtimetest.PinningBroker
)

var (
	routingDecision  = runtimetest.Decision
	routingOptions   = runtimetest.Options
	routingEvent     = runtimetest.Event
	requireAccept    = runtimetest.RequireAccept
	requireRoute     = runtimetest.RequireRoute
	routingRead      = runtimetest.Read
	newTestScheduler = runtimetest.NewScheduler
	fixtureEndpoint  = runtimetest.Endpoint
)

// fixtureStore is runtimetest.Store, plus "instrumented": persistent SQLite
// wrapped in the telemetry instrumentation.
func fixtureStore(t *testing.T, mode string) storage.Store {
	t.Helper()
	if mode == "instrumented" {
		return instrumentedBackend(t, filepath.Join(t.TempDir(), "routing.db"))
	}
	return runtimetest.Store(t, mode)
}

func routingFixture(t *testing.T, mode string, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	return runtimetest.RoutingFixture(t, fixtureStore(t, mode), o)
}

func schedulerFixture(t *testing.T, mode string, o conformance.RuntimeOptions) (conformance.RuntimeFixture, *schedulerExecutor) {
	t.Helper()
	return runtimetest.SchedulerFixture(t, fixtureStore(t, mode), o)
}

func waitFixture(t *testing.T, mode string, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	return runtimetest.WaitFixture(t, fixtureStore(t, mode), o)
}
