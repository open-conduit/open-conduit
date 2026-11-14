package codex

import (
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

// The lineage key the host adds when Work waits or asks for child Work is
// not its subject, so the arguments a template fills don't change.
func TestExpandSkipsTheLineageKey(t *testing.T) {
	issue := domain.CorrelationKey{Source: "github/x", ExternalID: "issue/12"}
	for _, c := range []struct {
		keys []domain.CorrelationKey
		want string
	}{
		{[]domain.CorrelationKey{issue}, "12 issue/12"},
		{[]domain.CorrelationKey{issue, domain.LineageKey("work/d5bc")}, "12 issue/12"},
		{[]domain.CorrelationKey{domain.LineageKey("work/d5bc"), issue}, "12 issue/12"},
		{[]domain.CorrelationKey{domain.LineageKey("work/d5bc")}, " "},
		{nil, " "},
	} {
		if got := expand("{number} {key}", domain.Work{ID: "work/d5bc", CorrelationKeys: c.keys}); got != c.want {
			t.Fatalf("%+v: %q, want %q", c.keys, got, c.want)
		}
	}
}
