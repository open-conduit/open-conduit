package runtime

import (
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

// A Work is never woken by what its own executions published, whatever the
// Event's type; the same Event from another Work, or one that names the Work
// without coming from its execution (an operator's, an ingress observation's),
// still wakes it.
func TestWaitIgnoresTheWorksOwnPublishedEvents(t *testing.T) {
	key := domain.CorrelationKey{Source: "acme", ExternalID: "7"}
	wait := domain.Wait{WorkID: "w1", Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}}}
	for name, c := range map[string]struct {
		work, execution domain.ID
		typ             string
		want            bool
	}{
		"own turn ended":          {"w1", "x1", domain.TypeTurnEnded, false},
		"own emitted event":       {"w1", "x1", "acme.signed_off", false},
		"another work's turn":     {"w2", "x2", domain.TypeTurnEnded, true},
		"names the work, no exec": {"w1", "", "core.ingress.absent", true},
		"external event":          {"", "", "acme.comment", true},
	} {
		a := domain.AcceptedEvent{Sequence: 1, Event: domain.Event{Type: c.typ, WorkID: c.work, ExecutionID: c.execution, CorrelationKey: &key}}
		if got := waitMatches(wait, a); got != c.want {
			t.Errorf("%s: matched=%v, want %v", name, got, c.want)
		}
	}
}
