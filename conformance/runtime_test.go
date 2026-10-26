package conformance_test

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
)

func TestReferenceRuntime(t *testing.T) { conformance.RunRuntime(t, reference.NewRuntime) }

type brokenWake struct{ conformance.RuntimeDriver }

func (b brokenWake) Wake(domain.ID) error { return nil }

type unboundedChain struct{ conformance.RuntimeDriver }

func (b unboundedChain) Derive(e domain.Event) error { return b.Accept(e) }
func TestWeakenedRuntimeContractsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name, diagnostic string
		wrap             func(conformance.RuntimeDriver) conformance.RuntimeDriver
	}{
		{"wait_event_registration_orders", "invalid", func(d conformance.RuntimeDriver) conformance.RuntimeDriver { return brokenWake{d} }},
		{"causal_depth_rate_replay_and_clock", "chain depth bound bypassed", func(d conformance.RuntimeDriver) conformance.RuntimeDriver { return unboundedChain{d} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			found := false
			for _, c := range conformance.RuntimeCases() {
				if c.Name != test.name {
					continue
				}
				found = true
				err := c.Check(func(o conformance.RuntimeOptions) conformance.RuntimeFixture {
					f := reference.NewRuntime(o)
					f.Driver = test.wrap(f.Driver)
					restart := f.Restart
					f.Restart = func() conformance.RuntimeDriver { return test.wrap(restart()) }
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
