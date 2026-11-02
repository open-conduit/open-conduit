package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Route failures (issue #342). A route that errs, hangs or panics must not
// hold its Event, and with it the Work it targets, pending forever: each
// failure is counted on the Event, retried after a growing backoff, and
// after MaxRouteFailures (or straight away when retrying cannot help) the
// Event is routed with the "failed" disposition. It stays in the Work's
// history and in telemetry, and an operator can route a copy again.

const routeFailureNamespace = "core.routing.failures"

const (
	// DefaultRouteTimeout bounds one route handler's decision.
	DefaultRouteTimeout = 5 * time.Second
	// DefaultMaxRouteFailures is how many retryable failures an Event is
	// given before it is routed as failed.
	DefaultMaxRouteFailures = 5
	// DefaultRouteFailureWindow is the least time between an Event's first
	// retryable failure and routing giving up on it, so an extension
	// restarting or a dependency down for a while doesn't lose good input.
	DefaultRouteFailureWindow = 15 * time.Minute
	// routeBackoffCap bounds the wait between tries.
	routeBackoffCap = 5 * time.Minute
	// routeBackoffBase is the wait after the first failure; it doubles
	// with each one after.
	routeBackoffBase = 2 * time.Second
	routeErrorLimit  = 2048
)

// DispositionFailed marks an Event routing gave up on.
const DispositionFailed = "failed"

// DispositionSkipped marks an Event an operator told routing to skip.
const DispositionSkipped = "skipped"

// Route failure codes.
const (
	RouteHandlerMissing = "handler_missing"
	RouteBindingInvalid = "binding_invalid"
	RoutePolicyInvalid  = "policy_invalid"
	RouteHandlerFailed  = "handler_failed"
	RouteHandlerInvalid = "handler_invalid"
	RouteHandlerTimeout = "handler_timeout"
	RouteHandlerPanic   = "handler_panic"
)

// RouteError is a route's failure to decide an Event. Retryable failures
// count towards MaxRouteFailures; the rest route the Event as failed at once.
type RouteError struct {
	EventID   domain.ID
	Route     string
	Code      string
	Retryable bool
	// Attempts is how many failures the Event has had, this one included.
	Attempts int
	// DeadLettered is set once the Event was routed as failed.
	DeadLettered bool
	// Backoff is set when the Event was not tried because its last
	// failure's backoff has not passed.
	Backoff bool
	Err     error
}

func (e *RouteError) Error() string {
	if e.Err == nil {
		return "route " + e.Code
	}
	return "route " + e.Code + ": " + e.Err.Error()
}
func (e *RouteError) Unwrap() error { return e.Err }

// RouteFailure is the durable count of an unrouted Event's failures.
type RouteFailure struct {
	EventID     domain.ID
	Route, Code string
	Error       string
	// Generation is the configuration the failures were under: a new one
	// is tried at once, since it may have fixed the route.
	Generation  string
	Attempts    int
	First, Last time.Time
	Next        time.Time
}

func routeFailureKey(event domain.ID) string { return identity(string(event)) }

func truncateRouteError(s string) string {
	if len(s) > routeErrorLimit {
		return s[:routeErrorLimit]
	}
	return s
}

func (r *Router) routeTimeout() time.Duration {
	if r.options.RouteTimeout > 0 {
		return r.options.RouteTimeout
	}
	return DefaultRouteTimeout
}

func (r *Router) routeFailureWindow() time.Duration {
	if r.options.RouteFailureWindow > 0 {
		return r.options.RouteFailureWindow
	}
	return DefaultRouteFailureWindow
}

func (r *Router) maxRouteFailures() int {
	if r.options.MaxRouteFailures > 0 {
		return r.options.MaxRouteFailures
	}
	return DefaultMaxRouteFailures
}

// match asks one handler for its decision under the route deadline. The
// handler runs on its own goroutine, so one that ignores its context or
// never returns cannot hold routing, and a panic is turned into an error
// carrying the stack.
func (r *Router) match(ctx context.Context, event domain.ID, route domain.Route, h extension.RouteHandler, in extension.RouteInput) (extension.RouteDecision, error) {
	cx, cancel := context.WithTimeout(ctx, r.routeTimeout())
	defer cancel()
	type result struct {
		d   extension.RouteDecision
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				done <- result{err: &RouteError{EventID: event, Route: route.Name, Code: RouteHandlerPanic, Err: fmt.Errorf("panic: %v\n%s", v, debug.Stack())}}
			}
		}()
		d, err := h.Match(cx, in)
		done <- result{d, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-cx.Done():
		if ctx.Err() != nil {
			return extension.RouteDecision{}, ctx.Err()
		}
		return extension.RouteDecision{}, &RouteError{EventID: event, Route: route.Name, Code: RouteHandlerTimeout, Retryable: true, Err: fmt.Errorf("no decision within %s", r.routeTimeout())}
	}
	if res.err == nil {
		return res.d, nil
	}
	var re *RouteError
	if errors.As(res.err, &re) {
		return res.d, re
	}
	if ctx.Err() != nil {
		return res.d, ctx.Err()
	}
	if errors.Is(res.err, context.DeadlineExceeded) {
		return res.d, &RouteError{EventID: event, Route: route.Name, Code: RouteHandlerTimeout, Retryable: true, Err: res.err}
	}
	if errors.Is(res.err, extension.ErrInvalid) {
		// The handler says the input itself is malformed: it will say so
		// again.
		return res.d, &RouteError{EventID: event, Route: route.Name, Code: RouteHandlerInvalid, Err: res.err}
	}
	return res.d, &RouteError{EventID: event, Route: route.Name, Code: RouteHandlerFailed, Retryable: true, Err: res.err}
}

// routeConfigError is a route that cannot be evaluated as configured (its
// handler or binding is gone): retrying the same generation cannot help.
func routeConfigError(event domain.ID, route domain.Route, code string, err error) error {
	return &RouteError{EventID: event, Route: route.Name, Code: code, Err: err}
}

// backoff reports a not-yet-due retry of event as a RouteError.
func (r *Router) backoff(ctx context.Context, event domain.ID) error {
	var f RouteFailure
	err := r.transact(ctx, nil, func(tx storage.Tx) error { return getJSON(tx, routeFailureNamespace, routeFailureKey(event), &f) })
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if f.Generation == r.options.Generation && r.now().Before(f.Next) {
		return &RouteError{EventID: event, Route: f.Route, Code: f.Code, Retryable: true, Attempts: f.Attempts, Backoff: true, Err: fmt.Errorf("retried after %s: %s", f.Next.Format(time.RFC3339), f.Error)}
	}
	return nil
}

// fail counts re against its Event and, once it may not be retried, routes
// the Event as failed. A failure inside the backoff of the last one (an
// earlier input evaluated again in another's batch) is not counted again.
func (r *Router) fail(ctx context.Context, re *RouteError) error {
	return r.transact(ctx, nil, func(tx storage.Tx) error {
		a, e := tx.Event(re.EventID)
		if e != nil {
			return e
		}
		if a.Routed {
			return nil
		}
		now := r.now()
		key := routeFailureKey(re.EventID)
		var f RouteFailure
		switch e = getJSON(tx, routeFailureNamespace, key, &f); {
		case errors.Is(e, storage.ErrNotFound):
			f = RouteFailure{EventID: re.EventID, First: now}
		case e != nil:
			return e
		case re.Retryable && f.Generation == r.options.Generation && now.Before(f.Next):
			re.Attempts = f.Attempts
			return nil
		}
		f.Attempts++
		f.Route, f.Code, f.Last, f.Generation = re.Route, re.Code, now, r.options.Generation
		f.Error = re.Code
		if re.Err != nil {
			f.Error = truncateRouteError(re.Err.Error())
		}
		f.Next = now.Add(min(routeBackoffBase<<min(f.Attempts-1, 10), routeBackoffCap))
		re.Attempts = f.Attempts
		if re.Retryable && (f.Attempts < r.maxRouteFailures() || now.Sub(f.First) < r.routeFailureWindow()) {
			return putJSON(tx, routeFailureNamespace, key, f)
		}
		re.DeadLettered = true
		return r.deadLetterInTx(tx, a, f)
	})
}

// deadLetterInTx routes a as failed, on the Work it targets when there is
// one, so it shows in that Work's history and no longer holds its input.
func (r *Router) deadLetterInTx(tx storage.Tx, a domain.AcceptedEvent, f RouteFailure) error {
	var work domain.ID
	if w, e := routeOwner(tx, a.Event); e == nil {
		work = w.ID
	}
	rec := RouteRecord{EventID: a.Event.ID, WorkID: work, Route: f.Route, Generation: r.options.Generation, Policy: DispositionFailed, Reason: f.Code, Error: f.Error, Attempts: f.Attempts}
	if e := putJSON(tx, routeNamespace, identity(string(a.Event.ID)), rec); e != nil {
		return e
	}
	if e := tx.Delete(routeFailureNamespace, routeFailureKey(a.Event.ID)); e != nil {
		return e
	}
	return tx.MarkRouted(a.Event.ID, work, DispositionFailed)
}

// RouteFailureInTx reads the failure count of a still-unrouted Event.
func RouteFailureInTx(tx storage.Tx, event domain.ID) (RouteFailure, bool, error) {
	var f RouteFailure
	err := getJSON(tx, routeFailureNamespace, routeFailureKey(event), &f)
	if errors.Is(err, storage.ErrNotFound) {
		return f, false, nil
	}
	return f, err == nil, err
}
