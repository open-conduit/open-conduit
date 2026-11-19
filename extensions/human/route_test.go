package human

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// The route gives each announced request its own Work: opened starts it,
// and the request's later Events are its input. An Event of the same
// family posted under another source (an operator's) starts nothing.
func TestRouteGivesEachRequestItsWork(t *testing.T) {
	a := Ask{ID: "a1", Scope: "eng", Work: "work/1", Provider: "decisions", Gate: "deploy.run", Question: "Allow deploy.run?", Options: []string{OptionApprove, OptionRefuse},
		Responders: []string{"slack:U0123ABCD"}, AskedAt: time.Unix(100, 0), Deadline: time.Unix(7300, 0), State: StateOpen, Announce: true}
	event := func(phase string) domain.AcceptedEvent {
		e, err := requestEvent(a, "agent", phase, time.Unix(200, 0))
		if err != nil {
			t.Fatal(err)
		}
		return domain.AcceptedEvent{Event: e}
	}
	match := func(e domain.AcceptedEvent, w *domain.Work, config string) extension.RouteDecision {
		t.Helper()
		d, err := Route{}.Match(context.Background(), extension.RouteInput{Event: e, Work: w, Route: domain.Route{Config: domain.Payload{Data: json.RawMessage(config)}}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	opened := event(announcedOpened)
	if d := match(opened, nil, `{}`); !d.Match || d.Kind != WorkKindRequest || d.Display == nil || d.Display.Title != "Approve or refuse deploy.run" {
		t.Fatalf("opened: %+v", d)
	}
	if d := match(opened, nil, `{"x":1}`); d.Match {
		t.Fatal("matched with config the route doesn't take")
	}
	settled := event(announcedSettled)
	if d := match(settled, nil, ``); d.Match {
		t.Fatal("a settled request started Work")
	}
	if d := match(settled, &domain.Work{Kind: WorkKindRequest}, ``); !d.Match {
		t.Fatalf("settled for its Work: %+v", d)
	}
	if d := match(settled, &domain.Work{Kind: "github.issue"}, ``); d.Match {
		t.Fatal("fed other Work")
	}
	posted := opened
	posted.Event.Source = "core.manual"
	if d := match(posted, nil, ``); d.Match {
		t.Fatal("an Event posted under another source started Work")
	}
	other := opened
	other.Event.Type = TypeAnswer
	if d := match(other, nil, ``); d.Match {
		t.Fatal("matched an answer")
	}
}

// A record announces each phase it calls for once, in order.
func TestAskDuePhases(t *testing.T) {
	a := Ask{State: StateOpen, Announce: true}
	for _, tc := range []struct {
		announced, state string
		escalated        bool
		want             []string
	}{
		{"", StateOpen, false, []string{announcedOpened}},
		{announcedOpened, StateOpen, false, nil},
		{announcedOpened, StateOpen, true, []string{announcedEscalated}},
		{"", StateExpired, true, []string{announcedOpened, announcedEscalated, announcedSettled}},
		{announcedEscalated, StateAnswered, true, []string{announcedSettled}},
		{announcedOpened, StateWithdrawn, false, []string{announcedSettled}},
		{announcedSettled, StateWithdrawn, false, nil},
	} {
		a.Announced, a.State, a.EscalatedAt = tc.announced, tc.state, time.Time{}
		if tc.escalated {
			a.EscalatedAt = time.Unix(1, 0)
		}
		if got := a.due(); len(got) != len(tc.want) || len(got) > 0 && got[len(got)-1] != tc.want[len(tc.want)-1] || len(got) > 0 && got[0] != tc.want[0] {
			t.Errorf("%+v: %v", tc, got)
		}
	}
	if (Ask{State: StateOpen}).due() != nil {
		t.Error("an ask from before announcing is announced")
	}
}
