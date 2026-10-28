package slack_test

import (
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/slack"
)

func TestSamplesTraceAgainstTheScope(t *testing.T) {
	sc := scopeWith(t, "")
	samples := slack.Route{}.Samples(sc, domain.Route{})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	e := samples[0].Event
	e.ReceivedAt = time.Now()
	if d := match(t, sc, domain.AcceptedEvent{Event: e}, nil); !d.Match || d.Kind != slack.WorkKind {
		t.Fatalf("the mention sample must start a conversation: %+v", d)
	}
}
