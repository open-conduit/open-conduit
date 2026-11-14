package human

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig(nil)
	if err != nil || len(c.Responders) != 1 || c.Responders[0] != "operator:local" || len(c.Surfaces) != 1 || c.Surfaces[0] != SurfaceTUI {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, bad := range []string{`{"responders":["a:b:c"]}`, `{"responders":[""]}`, `{"surfaces":["email"]}`, `{"unknown":1}`} {
		if _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, bad := range []string{
		`{"gates":[{"operations":["x.y"],"escalate":{}}]}`,
		`{"gates":[{"operations":["x.y"],"escalate":{"responders":["a:b:c"]}}]}`,
		`{"gates":[{"operations":["x.y"],"escalate":{"responders":["github:o"],"timeout":"5s"}}]}`,
		`{"gates":[{"operations":["x.y"],"timeout":"8000h","escalate":{"responders":["github:o"],"timeout":"1000h"}}]}`,
	} {
		if _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if c, err := ParseConfig(json.RawMessage(`{"gates":[{"operations":["x.y"],"escalate":{"responders":["github:o"]}}]}`)); err != nil || c.Gates[0].Escalate.timeout != DefaultGateTimeout {
		t.Errorf("escalate defaults: %+v %v", c, err)
	}
	for _, bad := range []string{`{"gates":[{}]}`, `{"gates":[{"kinds":["Bad Kind"]}]}`, `{"gates":[{"kinds":["github."]}]}`, `{"gates":[{"bindings":["a:b"]}]}`} {
		if _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	c, err = ParseConfig(json.RawMessage(`{"gates":[{"operations":["deploy.*"]},{"kinds":["github.*"]},{"bindings":["release"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind, binding string
		want          bool
	}{{"github.issue", "agent", true}, {"linear.issue", "release", true}, {"linear.issue", "agent", false}, {"deploy.run", "agent", false}} {
		if _, ok := c.startGate(tc.kind, tc.binding); ok != tc.want {
			t.Errorf("start gate for %s on %s: %v", tc.kind, tc.binding, ok)
		}
	}
	if len(c.gates("github.issue", "", false)) != 0 {
		t.Error("a kind gate gated an operation")
	}
	if _, err := ParseConfig(json.RawMessage(`{"gates":[{"kinds":["github.*"],"for_bindings":["newbie"]}]}`)); err == nil {
		t.Error("accepted for_bindings without operations")
	}
	c, err = ParseConfig(json.RawMessage(`{"gates":[{"operations":["slack.post"],"for_bindings":["newbie"],"responders":["operator:alice"],"timeout":"1h"},` +
		`{"operations":["slack.*"],"responders":["operator:bob"]},{"operations":["slack.post"],"for_bindings":["other"],"responders":["operator:carol"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rule := gate(c.gates("slack.post", "newbie", true))
	if !slices.Equal(rule.Responders, []string{"operator:alice", "operator:bob"}) || rule.timeout != time.Hour {
		t.Errorf("two gates on one call: %+v", rule)
	}
	if got := c.gates("slack.post", "trusted", true); len(got) != 1 || got[0].Responders[0] != "operator:bob" {
		t.Errorf("for_bindings held another binding's call: %+v", got)
	}
	if got := c.gates("slack.post", "", false); len(got) != 3 {
		t.Errorf("an unknown binding must meet every gate: %+v", got)
	}
	if got := gateBindings(json.RawMessage(`{"gates":[{"bindings":["release"]},{"operations":["x.y"],"for_bindings":["newbie","release"]}]}`)); !slices.Equal(got, []string{"release", "newbie"}) {
		t.Errorf("gate bindings: %v", got)
	}
	if ValidateExtension(nil, []string{"token"}) == nil {
		t.Error("accepted secrets")
	}
	c, _ = ParseConfig(json.RawMessage(`{"responders":["operator","slack_user:U1"]}`))
	if !c.Allows(extension.LocalOperator) || !c.Allows(extension.Caller{Kind: "slack_user", ID: "U1"}) || c.Allows(extension.Caller{Kind: "slack_user", ID: "U2"}) {
		t.Errorf("allows: %+v", c)
	}
}

func TestChoose(t *testing.T) {
	a := Ask{Options: []string{"Yes", "No"}}
	for in, want := range map[string]string{"2": "No", "yes": "Yes", " NO ": "No"} {
		if got, _, ok := a.choose(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"3", "0", "maybe", ""} {
		if _, _, ok := a.choose(in); ok {
			t.Errorf("%q chosen", in)
		}
	}
	if got, n, ok := (Ask{}).choose(" eu-west "); !ok || got != "eu-west" || n != 0 {
		t.Errorf("free answer: %q %d %v", got, n, ok)
	}
}

func TestDependencyFilterCountsOnlyTheAction(t *testing.T) {
	a := Ask{ID: "a1", Provider: "decisions"}
	data, _ := json.Marshal(AnswerPayload{Ask: "a1", Answer: "yes"})
	answer := domain.Event{Type: TypeAnswer, Source: extension.ActionSource, DeliveryID: "decisions/answer/k1", Data: domain.Payload{Schema: SchemaAnswer, Data: data}}
	if !DependencyFilter(filterPayload(a), answer) {
		t.Fatal("the action's answer did not count")
	}
	for name, mutate := range map[string]func(*domain.Event){
		"posted":         func(e *domain.Event) { e.Source = "core.manual" },
		"other provider": func(e *domain.Event) { e.DeliveryID = "widgets/answer/k1" },
		"other action":   func(e *domain.Event) { e.DeliveryID = "decisions/claim/k1" },
		"other ask":      func(e *domain.Event) { d, _ := json.Marshal(AnswerPayload{Ask: "a2"}); e.Data.Data = d },
		"other type":     func(e *domain.Event) { e.Type = TypeAnswered },
	} {
		e := answer
		mutate(&e)
		if DependencyFilter(filterPayload(a), e) {
			t.Errorf("%s counted as an answer", name)
		}
	}
}

func TestParseInput(t *testing.T) {
	now := mustTime(t, "2026-10-27T12:00:00Z")
	in, deadline, err := parseInput([]byte(`{"question":" Go? ","options":["a","b"],"deadline":"2026-10-28T09:00Z"}`), now)
	if err != nil || in.Question != "Go?" || deadline != mustTime(t, "2026-10-28T09:00:00Z") {
		t.Fatalf("%+v %v %v", in, deadline, err)
	}
	for _, bad := range []string{`{}`, `{"question":"?","options":["only"]}`, `{"question":"?","options":["a","A"]}`, `{"question":"?","deadline":"2026-10-26T00:00Z"}`, `{"question":"?","deadline":"tomorrow"}`, `{"question":"?","to":"alice"}`} {
		if _, _, err := parseInput([]byte(bad), now); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func mustTime(t *testing.T, s string) (out time.Time) {
	t.Helper()
	out, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAskGuidance(t *testing.T) {
	ops := (&Tools{}).Registration().Operations
	if len(ops) != 1 || ops[0].Guidance == "" || len(ops[0].Guidance) > extension.MaxGuidance {
		t.Fatalf("human.ask guidance %q", ops[0].Guidance)
	}
}

// one is the only gate of gs.
func one(gs []GateConfig) (GateConfig, bool) {
	if len(gs) != 1 {
		return GateConfig{}, false
	}
	return gs[0], true
}

func TestParseGates(t *testing.T) {
	c, err := ParseConfig(json.RawMessage(`{"responders":["github:alice"],"gates":[{"operations":["github.pr.create","deploy.*"]},{"operations":["shell.run"],"responders":["slack_user:U1"],"timeout":"30m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := one(c.gates("deploy.prod", "", false))
	if !ok || g.timeout != DefaultGateTimeout || len(g.Responders) != 1 || g.Responders[0] != "github:alice" {
		t.Fatalf("prefix gate: %+v %v", g, ok)
	}
	if g, ok = one(c.gates("shell.run", "", false)); !ok || g.timeout != 30*time.Minute || g.Responders[0] != "slack_user:U1" {
		t.Fatalf("second gate: %+v %v", g, ok)
	}
	for _, op := range []string{"deploy", "deployer.x", "github.pr.merge", "shell.runs"} {
		if len(c.gates(op, "", false)) > 0 {
			t.Errorf("%s gated", op)
		}
	}
	for _, bad := range []string{
		`{"gates":[{}]}`,
		`{"gates":[{"operations":["Deploy"]}]}`,
		`{"gates":[{"operations":["deploy.*.*"]}]}`,
		`{"gates":[{"operations":["*"]}]}`,
		`{"gates":[{"operations":["human.ask"]}]}`,
		`{"gates":[{"operations":["emit"]}]}`,
		`{"gates":[{"operations":["create_work"]}]}`,
		`{"gates":[{"operations":["read_context"]}]}`,
		`{"gates":[{"operations":["human.*"]}]}`,
		`{"gates":[{"operations":["a.b"],"timeout":"30s"}]}`,
		`{"gates":[{"operations":["a.b"],"timeout":"soon"}]}`,
		`{"gates":[{"operations":["a.b"],"responders":["a:b:c"]}]}`,
		`{"gates":[{"operations":["a.b"],"on_timeout":"escalate"}]}`,
	} {
		if _, err := ParseConfig(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestRequestHashIsCanonical(t *testing.T) {
	a := requestHash("deploy.run", domain.Payload{Schema: "s", Data: json.RawMessage(`{"b":2,"a":[1, 2.50]}`)})
	b := requestHash("deploy.run", domain.Payload{Schema: "s", Data: json.RawMessage(` { "a" : [1,2.50], "b":2 }`)})
	if a != b {
		t.Fatal("the same call hashed differently")
	}
	for _, other := range []string{
		requestHash("deploy.stop", domain.Payload{Schema: "s", Data: json.RawMessage(`{"b":2,"a":[1,2.50]}`)}),
		requestHash("deploy.run", domain.Payload{Schema: "s", Data: json.RawMessage(`{"b":3,"a":[1,2.50]}`)}),
		requestHash("deploy.run", domain.Payload{Schema: "t", Data: json.RawMessage(`{"b":2,"a":[1,2.50]}`)}),
	} {
		if other == a {
			t.Fatal("different calls hashed the same")
		}
	}
}
