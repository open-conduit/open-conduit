package bootstrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
)

const engineerRoute = `{"kinds":[{"kind":"agent.task.engineer","description":"The engineering agent: code, builds and deploys."}]}`

func workRouteInput(eventType, source string, w *domain.Work) extension.RouteInput {
	e := domain.Event{Type: eventType, Source: source}
	if w != nil {
		e.WorkID = w.ID
	}
	return extension.RouteInput{Event: domain.AcceptedEvent{Event: e}, Work: w, Route: domain.Route{Name: "engineer", Handler: "work", Config: domain.Payload{Data: []byte(engineerRoute)}}}
}

// The work route takes only the creation of child Work of a kind it lists.
func TestWorkRouteTakesChildWorkOfItsKinds(t *testing.T) {
	ctx := context.Background()
	child := &domain.Work{ID: "child/1", Kind: "agent.task.engineer", ParentID: "front"}
	if d, err := (workRoute{}).Match(ctx, workRouteInput("core.work.created", "core.work", child)); err != nil || !d.Match || d.Kind != "" || d.CreateID != "" {
		t.Fatalf("child Work of a listed kind: %+v %v", d, err)
	}
	other := &domain.Work{ID: "child/2", Kind: "agent.task.sales", ParentID: "front"}
	root := &domain.Work{ID: "root", Kind: "agent.task.engineer"}
	for name, in := range map[string]extension.RouteInput{
		"another kind":  workRouteInput("core.work.created", "core.work", other),
		"no parent":     workRouteInput("core.work.created", "core.work", root),
		"an operator's": workRouteInput("core.work.created", "core.manual", child),
		"another event": workRouteInput("core.work.child.ended", "core.work", child),
		"no Work":       workRouteInput("core.work.created", "core.work", nil),
	} {
		if d, err := (workRoute{}).Match(ctx, in); err != nil || d.Match || d.Reason == "" {
			t.Fatalf("%s was taken: %+v %v", name, d, err)
		}
	}
}

// A work route's config must list kinds, each a Work kind, once.
func TestWorkRouteConfig(t *testing.T) {
	if _, err := parseWorkRoute([]byte(engineerRoute)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{``, `{}`, `{"kinds":[]}`, `{"kinds":["agent.task.engineer"]}`, `{"kinds":[{"kind":"Engineer"}]}`,
		`{"kinds":[{"kind":"agent.task.x"},{"kind":"agent.task.x"}]}`, `{"kinds":[{"kind":"agent.task.x"}],"extra":1}`} {
		if _, err := parseWorkRoute([]byte(bad)); err == nil {
			t.Fatalf("config %s accepted", bad)
		}
	}
}

// What a kind is for is the description of the route that binds it,
// whichever handler's: the bundled work route's or an installed package's.
func TestDescribeWorkKind(t *testing.T) {
	reg := registry(nil)
	reg.HandlerWorkKinds["acme"] = func(json.RawMessage) []extension.WorkKind {
		return []extension.WorkKind{{Kind: "acme.task.triage", Description: "Acme's triage agent."}}
	}
	scope := config.EffectiveScope{Routes: []config.Route{
		{Name: "manual", Handler: "manual", Config: []byte(`{}`)},
		{Name: "engineer", Handler: "work", Config: []byte(engineerRoute)},
		{Name: "triage", Handler: "acme", Config: []byte(`{}`)},
	}}
	describe := describeWorkKind(reg)
	if got := describe(scope, "agent.task.engineer"); !strings.HasPrefix(got, "The engineering agent") {
		t.Fatalf("description = %q", got)
	}
	if got := describe(scope, "acme.task.triage"); got != "Acme's triage agent." {
		t.Fatalf("an installed handler's kind = %q", got)
	}
	if got := describe(scope, "agent.task.sales"); got != "" {
		t.Fatalf("an unrouted kind was described: %q", got)
	}
}
