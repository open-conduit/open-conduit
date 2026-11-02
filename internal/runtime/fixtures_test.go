package runtime_test

import (
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/internal/runtime/runtimetest"
)

// The runtime drivers live in internal/runtime/runtimetest, shared with the
// integration tests; these names keep the tests that build on them readable.
type (
	routingReopening       = runtimetest.Reopening
	routingFixtureExecutor = runtimetest.RouterExecutor
	routingDriver          = runtimetest.RoutingDriver
	routingHandlerFunc     = runtimetest.HandlerFunc
	schedulerHandle        = runtimetest.Handle
	schedulerExecutor      = runtimetest.Executor
	schedulerDriver        = runtimetest.SchedulerDriver
	waitDriver             = runtimetest.WaitDriver
	pinningBroker          = runtimetest.PinningBroker
)

var (
	routingDecision  = runtimetest.Decision
	routingOptions   = runtimetest.Options
	waitOptions      = runtimetest.WaitOptions
	routingEvent     = runtimetest.Event
	requireAccept    = runtimetest.RequireAccept
	requireRoute     = runtimetest.RequireRoute
	routingRead      = runtimetest.Read
	newTestScheduler = runtimetest.NewScheduler
)

func routingFixture(t *testing.T, mode string, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	return runtimetest.RoutingFixture(t, runtimetest.Store(t, mode), o)
}

func schedulerFixture(t *testing.T, mode string, o conformance.RuntimeOptions) (conformance.RuntimeFixture, *schedulerExecutor) {
	t.Helper()
	return runtimetest.SchedulerFixture(t, runtimetest.Store(t, mode), o)
}

func waitFixture(t *testing.T, mode string, o conformance.RuntimeOptions) conformance.RuntimeFixture {
	t.Helper()
	return runtimetest.WaitFixture(t, runtimetest.Store(t, mode), o)
}
