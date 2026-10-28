package setup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func TestValidate(t *testing.T) {
	if ValidateExtension(nil, nil) != nil || ValidateExtension(json.RawMessage(`{}`), nil) != nil || ValidateRoute(nil, nil) != nil {
		t.Fatal("refused an empty config")
	}
	if ValidateExtension(nil, []string{"token"}) == nil || ValidateExtension(json.RawMessage(`{"x":1}`), nil) == nil || ValidateRoute(json.RawMessage(`{"x":1}`), nil) == nil {
		t.Fatal("accepted secrets or unknown fields")
	}
}

func routeIn(e domain.Event, w *domain.Work) extension.RouteInput {
	return extension.RouteInput{Event: domain.AcceptedEvent{Event: e}, Scope: domain.Scope{ID: "setup"}, Work: w}
}

func TestRoute(t *testing.T) {
	key := SessionKey("setup", "abc")
	start := domain.Event{Type: TypeStart, Source: extension.ActionSource, CorrelationKey: &key, Data: payload(SchemaStart, Start{Model: "sonnet"})}
	d, err := Route{}.Match(context.Background(), routeIn(start, nil))
	if err != nil || !d.Match || d.Kind != WorkKind || d.CreateID != "setup/setup/abc" || d.Display == nil || d.Context.Schema != SchemaContext || string(d.Context.Data) != `{"model":"sonnet"}` {
		t.Fatalf("start: %+v %v", d, err)
	}
	session := &domain.Work{ID: d.CreateID, Kind: WorkKind, ScopeID: "setup", CorrelationKeys: []domain.CorrelationKey{key}}
	other := &domain.Work{ID: "x", Kind: "github.issue", ScopeID: "setup"}
	elsewhere := SessionKey("eng", "abc")
	agent := domain.Event{Type: TypeMessage, Source: "execution/x", CorrelationKey: &key}
	for name, c := range map[string]struct {
		e     domain.Event
		w     *domain.Work
		match bool
	}{
		"message":              {domain.Event{Type: TypeMessage, Source: extension.ActionSource, CorrelationKey: &key}, session, true},
		"answer":               {domain.Event{Type: TypeAnswer, Source: extension.ActionSource, CorrelationKey: &key}, session, true},
		"the agent's own":      {agent, session, false},
		"a second start":       {start, session, false},
		"a message unowned":    {domain.Event{Type: TypeMessage, Source: extension.ActionSource, CorrelationKey: &key}, nil, false},
		"other Work's key":     {domain.Event{Type: TypeMessage, Source: extension.ActionSource, CorrelationKey: &key}, other, false},
		"another scope's key":  {domain.Event{Type: TypeStart, Source: extension.ActionSource, CorrelationKey: &elsewhere, Data: start.Data}, nil, false},
		"a start with no key":  {domain.Event{Type: TypeStart, Source: extension.ActionSource, Data: start.Data}, nil, false},
		"a posted start event": {domain.Event{Type: TypeStart, Source: "core.manual", CorrelationKey: &key, Data: start.Data}, nil, false},
	} {
		d, err := Route{}.Match(context.Background(), routeIn(c.e, c.w))
		if err != nil || d.Match != c.match || d.Reason == "" {
			t.Errorf("%s: %+v %v", name, d, err)
		}
	}
}

func TestModelsAndChoose(t *testing.T) {
	scope := domain.Scope{ID: "setup",
		Routes:   []domain.Route{{Name: "other", Handler: "github", Binding: "run"}, {Name: "setup", Handler: Type, Binding: "agent"}},
		Bindings: []domain.Binding{{Name: "run"}, {Name: "agent", Config: domain.Payload{Data: json.RawMessage(`{"models":["opus","sonnet"]}`)}}}}
	if got := strings.Join(Models(scope), ","); got != "opus,sonnet" {
		t.Fatalf("models: %q", got)
	}
	if Models(domain.Scope{}) != nil {
		t.Fatal("models without a setup route")
	}
	for in, want := range map[string]string{"2": "sonnet", "Opus": "opus", " sonnet ": "sonnet"} {
		if got, ok := choose(Models(scope), in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "3", "haiku"} {
		if got, ok := choose(Models(scope), in); ok {
			t.Errorf("%q chose %q", in, got)
		}
	}
}

func TestInvokeRefusesWithoutTheStore(t *testing.T) {
	w := domain.Work{ID: "x", Kind: "github.issue", ScopeID: "setup"}
	for name, c := range map[string]extension.ActionCall{
		"not the operator": {Action: ActionStart, Caller: extension.Caller{Kind: "slack_user", ID: "U1"}},
		"unknown action":   {Action: "apply", Caller: extension.LocalOperator},
		"start on Work":    {Action: ActionStart, Target: extension.ActionTarget{Work: &w}, Caller: extension.LocalOperator},
		"message, no Work": {Action: ActionMessage, Caller: extension.LocalOperator},
		"other Work":       {Action: ActionMessage, Target: extension.ActionTarget{Work: &w}, Input: "hi", Caller: extension.LocalOperator},
	} {
		var oe *extension.OperationError
		if _, err := (Actions{}).Invoke(context.Background(), c); !errors.As(err, &oe) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRenderEvent(t *testing.T) {
	for _, c := range []struct {
		e    domain.Event
		want string
	}{
		{domain.Event{Type: TypeStart, Data: payload(SchemaStart, Start{Model: "opus"})}, "started a setup session (on opus)"},
		{domain.Event{Type: TypeMessage, Data: payload(SchemaMessage, Message{Role: RoleOperator, Text: "use the work account"})}, "The operator says\nuse the work account"},
		{domain.Event{Type: TypeAnswer, Data: payload(SchemaAnswer, Answer{Question: "q", Answer: "octo/widgets"})}, "answered your question\nocto/widgets"},
	} {
		if got, ok := RenderEvent(c.e); !ok || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", c.e.Type, got)
		}
	}
	if _, ok := RenderEvent(domain.Event{Type: TypeMessage, Data: payload(SchemaMessage, Message{Role: RoleAgent, Text: "hi"})}); ok {
		t.Error("rendered the agent's own message")
	}
}
