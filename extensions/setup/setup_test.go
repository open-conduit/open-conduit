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

func TestValidateBinding(t *testing.T) {
	ok := extension.RouteBinding{Executor: Executor, Operations: []string{"read_context", "wait", "declare_outcome", OpSay, OpAsk}, Config: json.RawMessage(`{"builtin_tools":[],"models":["opus"]}`)}
	if err := ValidateBinding(ok); err != nil {
		t.Fatalf("refused: %v", err)
	}
	for name, b := range map[string]extension.RouteBinding{
		"a shell":              {Executor: "shell", Config: json.RawMessage(`{"command":"true"}`)},
		"built-in tools unset": {Executor: Executor, Config: json.RawMessage(`{}`)},
		"built-in tools on":    {Executor: Executor, Config: json.RawMessage(`{"builtin_tools":["Bash"]}`)},
		"an unreadable config": {Executor: Executor, Config: json.RawMessage(`[]`)},
		"another extension's":  {Executor: Executor, Config: ok.Config, Operations: []string{"github.issue.comment"}},
		"core emit":            {Executor: Executor, Config: ok.Config, Operations: []string{"emit"}},
		"core create_work":     {Executor: Executor, Config: ok.Config, Operations: []string{"create_work"}},
	} {
		if err := ValidateBinding(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInitSetup(t *testing.T) {
	r, err := Setup{}.Setup(context.Background(), nil, nil)
	if err != nil || len(r.Connections) != 0 || len(r.Next) == 0 {
		t.Fatalf("setup: %+v %v", r, err)
	}
	var scope struct {
		ID       string
		Bindings []struct {
			Executor   string
			Operations []string
			Config     json.RawMessage
		}
		Routes []struct{ Handler, Binding string }
	}
	if err = json.Unmarshal(r.Scope, &scope); err != nil || scope.ID != DefaultScopeID || len(scope.Bindings) != 1 || len(scope.Routes) != 1 || scope.Routes[0].Handler != Type {
		t.Fatalf("scope: %s %v", r.Scope, err)
	}
	b := scope.Bindings[0]
	if err = ValidateBinding(extension.RouteBinding{Executor: b.Executor, Operations: b.Operations, Config: b.Config}); err != nil {
		t.Fatalf("the scope's own binding is refused: %v", err)
	}
	if r, err = (Setup{}).Setup(context.Background(), nil, []string{"--scope", "web-setup"}); err != nil || !strings.Contains(string(r.Scope), `"id":"web-setup"`) {
		t.Fatalf("--scope: %s %v", r.Scope, err)
	}
	for _, args := range [][]string{{"--scope", "Bad ID"}, {"--scope"}, {"--force-ish"}} {
		if _, err = (Setup{}).Setup(context.Background(), nil, args); !errors.Is(err, extension.ErrSetupUsage) {
			t.Errorf("%q: %v", args, err)
		}
	}
}
