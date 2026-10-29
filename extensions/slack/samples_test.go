package slack_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/slack"
)

// The samples are written by the host's clock, not the wall clock, so a
// mention traced under a test clock (or a skewed one) is still inside the
// start window.
func TestSamplesTraceAgainstTheScope(t *testing.T) {
	sc := scopeWith(t, "")
	now := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	samples := slack.Route{}.Samples(context.Background(), extension.SampleInput{Scope: sc, Now: now})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	e := samples[0].Event
	e.ReceivedAt = now
	if d := match(t, sc, domain.AcceptedEvent{Event: e}, nil); !d.Match || d.Kind != slack.WorkKind {
		t.Fatalf("the mention sample must start a conversation: %+v", d)
	}
}
