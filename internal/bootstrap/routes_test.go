package bootstrap

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func TestManualRouteSamples(t *testing.T) {
	samples := manualRoute{}.Samples(domain.Scope{}, domain.Route{})
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range samples {
		if s.ID == "" || s.Title == "" || s.Event.Type == "" {
			t.Fatalf("incomplete sample: %+v", s)
		}
	}
	d, err := manualRoute{}.Match(context.Background(), extension.RouteInput{Event: domain.AcceptedEvent{Event: samples[0].Event}})
	if err != nil || !d.Match || d.Kind != "manual.work" {
		t.Fatalf("the keyed sample must create manual work: %+v, %v", d, err)
	}
}
