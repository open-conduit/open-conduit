package human

import (
	"encoding/json"
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
