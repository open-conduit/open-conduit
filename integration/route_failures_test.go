package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Route failures (issue #342): one Event whose route errs, hangs or panics
// is given up on and routed as failed, so the Work it targets takes its
// later input instead of being held forever.

// poisonHandler routes every Event as routingDecision does, except those
// named "poison", which bad decides.
func poisonHandler(bad func(context.Context) (extension.RouteDecision, error)) extension.RouteHandler {
	return routeHandlerCtx(func(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
		if strings.HasPrefix(string(in.Event.Event.ID), "poison") {
			return bad(ctx)
		}
		return routingDecision(in)
	})
}

type routeHandlerCtx func(context.Context, extension.RouteInput) (extension.RouteDecision, error)

func (f routeHandlerCtx) Match(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(ctx, in)
}

// failureRouter is the production router on f's store with h as the
// fixture's only route handler.
func failureRouter(t *testing.T, f conformance.RuntimeFixture, h extension.RouteHandler, timeout time.Duration, max int) *runtime.Router {
	t.Helper()
	r, err := runtime.NewRouter(f.Store, f.Clock.Now, runtime.RouterOptions{Generation: "fixture-generation", Handlers: map[string]extension.RouteHandler{"handler-0": h},
		Bindings:     map[string]runtime.RouteBinding{"fixture": {Executor: routingFixtureExecutor{}}},
		RouteTimeout: timeout, MaxRouteFailures: max})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// stuckWork accepts and routes "first" (making the Work), then accepts
// "poison" and "later" for the same Work.
func stuckWork(t *testing.T, f conformance.RuntimeFixture, r *runtime.Router) {
	t.Helper()
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "stuck"}
	requireAccept(t, f, "first", &key)
	if err := r.Route(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	requireAccept(t, f, "poison", &key)
	requireAccept(t, f, "later", &key)
}

func eventState(t *testing.T, f conformance.RuntimeFixture, id domain.ID) domain.AcceptedEvent {
	t.Helper()
	var a domain.AcceptedEvent
	routingRead(t, f, func(tx storage.Tx) error { var e error; a, e = tx.Event(id); return e })
	return a
}

func routeErr(t *testing.T, err error) *runtime.RouteError {
	t.Helper()
	var re *runtime.RouteError
	if !errors.As(err, &re) {
		t.Fatalf("want a route error, got %v", err)
	}
	return re
}

// TestRouteAlwaysFailingGivesUp: a route that always errs is retried after
// a backoff, holding the Work's later input, and after MaxRouteFailures
// the Event is routed as failed with the error in its decision; the later
// input then routes. The control: before the limit, the Event is still
// unrouted and the later input is still refused.
func TestRouteAlwaysFailingGivesUp(t *testing.T) {
	ctx := context.Background()
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	r := failureRouter(t, f, poisonHandler(func(context.Context) (extension.RouteDecision, error) {
		return extension.RouteDecision{}, errors.New("boom")
	}), time.Second, 3)
	stuckWork(t, f, r)

	if err := r.Route(ctx, "later"); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("later input overtook the pending one: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		re := routeErr(t, r.Route(ctx, "poison"))
		if re.Code != runtime.RouteHandlerFailed || re.Attempts != attempt || re.DeadLettered != (attempt == 3) {
			t.Fatalf("attempt %d: %+v", attempt, re)
		}
		if attempt == 3 {
			break
		}
		// Control: short of the limit the Event holds the Work.
		if a := eventState(t, f, "poison"); a.Routed {
			t.Fatalf("attempt %d: routed before the limit: %+v", attempt, a)
		}
		if err := r.Route(ctx, "later"); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("attempt %d: later input routed while the failing one is pending: %v", attempt, err)
		}
		// Within the backoff it is not tried, and not counted, again.
		if re := routeErr(t, r.Route(ctx, "poison")); !re.Backoff || re.Attempts != attempt {
			t.Fatalf("attempt %d: retried inside its backoff: %+v", attempt, re)
		}
		f.Clock.Advance(time.Hour)
	}
	a := eventState(t, f, "poison")
	if !a.Routed || a.Disposition != runtime.DispositionFailed || a.RoutedWorkID != "work/stuck" {
		t.Fatalf("not routed as failed on its Work: %+v", a)
	}
	d, err := r.Decision(ctx, "poison")
	if err != nil || d.Policy != runtime.DispositionFailed || d.Reason != runtime.RouteHandlerFailed || d.Error != "boom" || d.Attempts != 3 || d.Route != "default" {
		t.Fatalf("decision: %+v %v", d, err)
	}
	if err := r.Route(ctx, "later"); err != nil {
		t.Fatalf("later input still held: %v", err)
	}
	if a := eventState(t, f, "later"); !a.Routed || a.RoutedWorkID != "work/stuck" {
		t.Fatalf("later input: %+v", a)
	}
}

// TestRouteMissingHandlerGivesUpAtOnce: a route whose handler is gone (its
// extension removed) can't be helped by retrying, so its Event is routed
// as failed on the first try.
func TestRouteMissingHandlerGivesUpAtOnce(t *testing.T) {
	ctx := context.Background()
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	stuckWork(t, f, failureRouter(t, f, routingHandlerFunc(routingDecision), time.Second, 3))
	gone, err := runtime.NewRouter(f.Store, f.Clock.Now, runtime.RouterOptions{Generation: "removed", Handlers: map[string]extension.RouteHandler{},
		Bindings: map[string]runtime.RouteBinding{"fixture": {Executor: routingFixtureExecutor{}}}})
	if err != nil {
		t.Fatal(err)
	}
	re := routeErr(t, gone.Route(ctx, "poison"))
	if re.Code != runtime.RouteHandlerMissing || !re.DeadLettered || re.Attempts != 1 {
		t.Fatalf("%+v", re)
	}
}

// TestRouteMatchThatNeverReturns: a handler that ignores its context and
// never returns is abandoned at the route deadline, counted as a failure,
// and given up on at the limit. The control: the handler really is still
// blocked when routing returns.
func TestRouteMatchThatNeverReturns(t *testing.T) {
	ctx := context.Background()
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	release, entered := make(chan struct{}), make(chan struct{}, 4)
	t.Cleanup(func() { close(release) })
	r := failureRouter(t, f, poisonHandler(func(context.Context) (extension.RouteDecision, error) {
		entered <- struct{}{}
		<-release
		return extension.RouteDecision{}, nil
	}), 20*time.Millisecond, 2)
	stuckWork(t, f, r)
	for attempt := 1; attempt <= 2; attempt++ {
		done := make(chan error, 1)
		go func() { done <- r.Route(ctx, "poison") }()
		var err error
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("routing is held by a handler that never returns")
		}
		select {
		case <-entered:
		default:
			t.Fatal("the handler was never called")
		}
		re := routeErr(t, err)
		if re.Code != runtime.RouteHandlerTimeout || !re.Retryable || re.DeadLettered != (attempt == 2) {
			t.Fatalf("attempt %d: %+v", attempt, re)
		}
		f.Clock.Advance(time.Hour)
	}
	if a := eventState(t, f, "poison"); a.Disposition != runtime.DispositionFailed {
		t.Fatalf("%+v", a)
	}
	if err := r.Route(ctx, "later"); err != nil {
		t.Fatal(err)
	}
}

// TestRouteHandlerPanicIsRecovered: a handler that panics doesn't take
// routing (and the daemon) down. The panic, with its stack, is the failed
// decision's error, and the Event is routed as failed at once, so a
// restart doesn't meet it again. The control: the handler does panic
// when called directly.
func TestRouteHandlerPanicIsRecovered(t *testing.T) {
	ctx := context.Background()
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	h := poisonHandler(func(context.Context) (extension.RouteDecision, error) {
		var m map[string]int
		m["x"] = 1
		return extension.RouteDecision{}, nil
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("control: the handler did not panic")
			}
		}()
		_, _ = h.Match(ctx, extension.RouteInput{Event: domain.AcceptedEvent{Event: domain.Event{ID: "poison-control"}}})
	}()
	r := failureRouter(t, f, h, time.Second, 5)
	stuckWork(t, f, r)
	re := routeErr(t, r.Route(ctx, "poison"))
	if re.Code != runtime.RouteHandlerPanic || !re.DeadLettered {
		t.Fatalf("%+v", re)
	}
	d, err := r.Decision(ctx, "poison")
	if err != nil || d.Policy != runtime.DispositionFailed || !strings.Contains(d.Error, "assignment to entry in nil map") || !strings.Contains(d.Error, "goroutine") {
		t.Fatalf("decision lacks the panic and its stack: %+v %v", d, err)
	}
	if err := r.Route(ctx, "later"); err != nil {
		t.Fatal(err)
	}
}

// TestRouteFailureBatchedEarlierInput: when the failing Event is an earlier
// input a later Event's key brings in, giving up on it lets the later one
// route in the same call.
func TestRouteFailureBatchedEarlierInput(t *testing.T) {
	ctx := context.Background()
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "batched"}
	requireAccept(t, f, "poison-early", &key)
	requireAccept(t, f, "creator", &key)
	r := failureRouter(t, f, poisonHandler(func(context.Context) (extension.RouteDecision, error) {
		return extension.RouteDecision{}, extension.ErrInvalid
	}), time.Second, 5)
	// creator makes the Work; poison-early, already pending for its key,
	// is evaluated in creator's batch, fails as malformed and is given up
	// on, and creator then routes.
	if err := r.Route(ctx, "creator"); err != nil {
		t.Fatal(err)
	}
	if a := eventState(t, f, "poison-early"); a.Disposition != runtime.DispositionFailed {
		t.Fatalf("earlier input: %+v", a)
	}
	if a := eventState(t, f, "creator"); !a.Routed {
		t.Fatalf("creator: %+v", a)
	}
}

// TestManagementEventSkipAndReroute drives the operator's controls through
// the CLI and the management server: doctor's routing check warns about an
// Event left unrouted (and not before it is old, the control), skip routes
// it as skipped and clears the warning, a second skip is refused, and
// reroute accepts it again as a copy that routes like new input.
func TestManagementEventSkipAndReroute(t *testing.T) {
	h := newManagementHarness(t)
	ctx := context.Background()
	// An Event accepted but not routed, as one whose route keeps failing
	// is.
	key := domain.CorrelationKey{Source: "fixture", ExternalID: "request/9"}
	now := h.clock.Now()
	if err := h.store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		_, _, e := runtime.AcceptInTx(tx, "ops", domain.Event{ID: "stuck-1", Type: "fixture.approved", Source: "fixture/source", DeliveryID: "stuck-1", OccurredAt: now, ReceivedAt: now, CorrelationKey: &key})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var posted client.Event
	h.mustCLI(&posted, "events", "get", "stuck-1")
	if posted.Routed {
		t.Fatalf("control: the event is already routed: %+v", posted)
	}
	doctor := func() string {
		out, errOut, _ := h.cli("doctor")
		return out + errOut
	}
	if d := doctor(); strings.Contains(d, "unrouted_events") {
		t.Fatalf("control: a new event is reported as stuck:\n%s", d)
	}
	h.clock.Advance(11 * time.Minute)
	if d := doctor(); !strings.Contains(d, "unrouted_events") || !strings.Contains(d, posted.ID) {
		t.Fatalf("doctor misses the stuck event:\n%s", d)
	}

	var skipped client.ControlResult
	h.mustCLI(&skipped, "events", "skip", posted.ID, "--reason", "bad payload", "--key", "s1")
	if skipped.EventID == "" || skipped.RoutedEventID != posted.ID {
		t.Fatalf("skip: %+v", skipped)
	}
	var ev client.Event
	h.mustCLI(&ev, "events", "get", posted.ID)
	if !ev.Routed || ev.Disposition != runtime.DispositionSkipped {
		t.Fatalf("skipped event: %+v", ev)
	}
	var control client.Event
	h.mustCLI(&control, "events", "get", skipped.EventID)
	if control.Type != "core.manual.skip_event" || control.Disposition != "control" || !strings.Contains(string(control.Data.Data), posted.ID) {
		t.Fatalf("control event: %+v", control)
	}
	if _, errOut, code := h.cli("events", "skip", posted.ID, "--key", "s2"); code == 0 {
		t.Fatalf("a routed event was skipped again: %s", errOut)
	}
	if d := doctor(); strings.Contains(d, "unrouted_events") {
		t.Fatalf("doctor still warns after the skip:\n%s", d)
	}

	var rerouted client.ControlResult
	h.mustCLI(&rerouted, "events", "reroute", posted.ID, "--key", "r1")
	if rerouted.RoutedEventID == "" || rerouted.RoutedEventID == posted.ID {
		t.Fatalf("reroute made no copy: %+v", rerouted)
	}
	var again client.ControlResult
	h.mustCLI(&again, "events", "reroute", posted.ID, "--key", "r1")
	if again != rerouted {
		t.Fatalf("retried reroute differs: %+v %+v", again, rerouted)
	}
	if err := h.router.Route(ctx, domain.ID(rerouted.RoutedEventID)); err != nil {
		t.Fatal(err)
	}
	var copied client.Event
	h.mustCLI(&copied, "events", "get", rerouted.RoutedEventID)
	if !copied.Routed || copied.Type != posted.Type || copied.CorrelationKey == nil || copied.CorrelationKey.ExternalID != "request/9" {
		t.Fatalf("copy: %+v", copied)
	}
	if _, _, code := h.cli("events", "reroute", rerouted.RoutedEventID, "--key", "r2"); code == 0 {
		t.Fatal("an event routed as usual was rerouted")
	}
}

// TestRouteFailureWindow: a route failing past MaxRouteFailures within the
// failure window (an extension restarting) is not given up on; once the
// window has passed since its first failure, the next failure gives up.
// The control: past the window the same failure routes the Event as failed.
func TestRouteFailureWindow(t *testing.T) {
	ctx := context.Background()
	f := routingFixture(t, "persistent", routingOptions(routingHandlerFunc(routingDecision), "queue"))
	r := failureRouter(t, f, poisonHandler(func(context.Context) (extension.RouteDecision, error) {
		return extension.RouteDecision{}, errors.New("extension restarting")
	}), time.Second, 2)
	stuckWork(t, f, r)
	for attempt := 1; attempt <= 4; attempt++ {
		if re := routeErr(t, r.Route(ctx, "poison")); re.DeadLettered || re.Attempts != attempt {
			t.Fatalf("attempt %d inside the window: %+v", attempt, re)
		}
		f.Clock.Advance(3 * time.Minute)
	}
	if a := eventState(t, f, "poison"); a.Routed {
		t.Fatalf("given up on inside the window: %+v", a)
	}
	f.Clock.Advance(runtime.DefaultRouteFailureWindow)
	if re := routeErr(t, r.Route(ctx, "poison")); !re.DeadLettered {
		t.Fatalf("control: not given up on past the window: %+v", re)
	}
}
