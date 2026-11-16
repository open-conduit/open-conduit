package slack_test

import (
	"testing"

	"github.com/open-conduit/open-conduit/extensions/slack"
)

func TestFeedbackConfig(t *testing.T) {
	base := `{"namespace":"` + ns + `","scope":"` + scope + `","team":"` + team + `","channels":["` + chanA + `"]`
	for cfg, ok := range map[string]bool{
		base + `,"feedback":{"enabled":true}}`:                                 true,
		base + `,"feedback":{"enabled":true,"bindings":["support","triage"]}}`: true,
		base + `,"feedback":{"bindings":["support"]}}`:                         false, // buttons for nobody
		base + `,"feedback":{"enabled":true,"bindings":[" "]}}`:                false,
		base + `,"feedback":{"enabled":true,"bindings":["a","a"]}}`:            false,
	} {
		if _, err := slack.ParseExtension([]byte(cfg), nil); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok %v", cfg, err, ok)
		}
	}
	all, err := slack.ParseExtension([]byte(base+`,"feedback":{"enabled":true}}`), nil)
	if err != nil || !all.AsksFeedback("support") || !all.AsksFeedback("") {
		t.Fatalf("every binding: %+v %v", all, err)
	}
	some, err := slack.ParseExtension([]byte(base+`,"feedback":{"enabled":true,"bindings":["triage","support"]}}`), nil)
	if err != nil || !some.AsksFeedback("support") || !some.AsksFeedback("triage") || some.AsksFeedback("other") || some.AsksFeedback("") {
		t.Fatalf("named bindings: %+v %v", some, err)
	}
	if off := settings(t, ""); off.AsksFeedback("support") {
		t.Fatal("feedback on without the setting")
	}
}
