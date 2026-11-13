package conformance_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

func TestReferenceRuntime(t *testing.T) { conformance.RunRuntime(t, reference.NewRuntime) }

type brokenWake struct{ conformance.RuntimeDriver }

func (b brokenWake) Wake(domain.ID) error { return nil }

type unboundedChain struct{ conformance.RuntimeDriver }

func (b unboundedChain) Derive(e domain.Event) error { return b.Accept(e) }

// noSuccessor checkpoints input for finished Work as terminal without
// asking a route, as the runtime did before routes could open successors.
type noSuccessor struct {
	conformance.RuntimeDriver
	store storage.Store
	now   func() time.Time
}

func (b noSuccessor) Route(id domain.ID) error {
	finished := false
	err := b.store.Transact(context.Background(), storage.Guard{Now: b.now()}, func(tx storage.Tx) error {
		a, err := tx.Event(id)
		if err != nil || a.Routed || a.Event.CorrelationKey == nil {
			return err
		}
		w, err := tx.WorkByCorrelation(*a.Event.CorrelationKey)
		if err != nil || !w.State.Terminal() {
			return nil
		}
		finished = true
		return tx.MarkRouted(id, w.ID, "terminal")
	})
	if err != nil || finished {
		return err
	}
	return b.RuntimeDriver.Route(id)
}

// deliverAlways is a router that ignores RouteDecision.Deliver and queues
// every match as input.
type deliverAlways struct{ extension.RouteHandler }

func (h deliverAlways) Match(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	d, err := h.RouteHandler.Match(ctx, in)
	d.Deliver = extension.DeliverRoute
	return d, err
}

func TestWeakenedRuntimeContractsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name, diagnostic string
		wrap             func(conformance.RuntimeDriver) conformance.RuntimeDriver
		fixture          func(conformance.RuntimeFixture, conformance.RuntimeDriver) conformance.RuntimeDriver
		options          func(conformance.RuntimeOptions) conformance.RuntimeOptions
	}{
		{"route_opened_successor", "successor not opened", nil, func(f conformance.RuntimeFixture, d conformance.RuntimeDriver) conformance.RuntimeDriver {
			return noSuccessor{d, f.Store, f.Clock.Now}
		}, nil},
		{"wait_event_registration_orders", "invalid", func(d conformance.RuntimeDriver) conformance.RuntimeDriver { return brokenWake{d} }, nil, nil},
		{"causal_depth_rate_replay_and_clock", "chain depth bound bypassed", func(d conformance.RuntimeDriver) conformance.RuntimeDriver { return unboundedChain{d} }, nil, nil},
		{"attach_without_input", "attach audit missing", nil, nil, func(o conformance.RuntimeOptions) conformance.RuntimeOptions {
			routes := slices.Clone(o.Routes)
			for i := range routes {
				routes[i].Handler = deliverAlways{routes[i].Handler}
			}
			o.Routes = routes
			return o
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			found := false
			for _, c := range conformance.RuntimeCases() {
				if c.Name != test.name {
					continue
				}
				found = true
				err := c.Check(func(o conformance.RuntimeOptions) conformance.RuntimeFixture {
					if test.options != nil {
						o = test.options(o)
					}
					f := reference.NewRuntime(o)
					wrap := test.wrap
					if test.fixture != nil {
						wrap = func(d conformance.RuntimeDriver) conformance.RuntimeDriver { return test.fixture(f, d) }
					}
					if wrap != nil {
						f.Driver = wrap(f.Driver)
						restart := f.Restart
						f.Restart = func() conformance.RuntimeDriver { return wrap(restart()) }
					}
					return f
				})
				if err == nil || !strings.Contains(err.Error(), test.diagnostic) {
					t.Fatalf("weakened contract not detected: %v", err)
				}
			}
			if !found {
				t.Fatal("negative control did not select shared case")
			}
		})
	}
}
