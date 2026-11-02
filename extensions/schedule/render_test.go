package schedule_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	sc "github.com/open-conduit/open-conduit/extensions/schedule"
)

// An agent job's turn reads what started it: a failed check suite names the
// branch, commit and conclusion.
func TestRenderEventNamesTheDelivery(t *testing.T) {
	job := sc.Job{Name: "red-main", On: &sc.Trigger{Events: []string{"check_suite.completed"}, Branches: []string{"main"}, Conclusions: []string{"failure"}}}
	d := sc.Delivery{EventID: "e1", ID: "d1", Event: "check_suite", Action: "completed", Ref: "refs/heads/main", SHA: "abc1234def", Conclusion: "failure"}
	text, ok := sc.RenderEvent(sc.EventTrigger("schedule/eng", job, d, domain.Event{ID: "e1"}))
	if !ok {
		t.Fatal("a job trigger was not rendered")
	}
	for _, want := range []string{"### A run of the red-main job: check_suite.completed abc1234 failure", "Event: check_suite.completed", "Branch: main", "Commit: abc1234def", "Conclusion: failure"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered trigger lacks %q:\n%s", want, text)
		}
	}
	// A webhook body is shown, and other schemas are left to other renderers.
	data, _ := json.Marshal(sc.TriggerPayload{Job: "poke", Kind: sc.KindWebhook, Trigger: "webhook", Text: strings.Repeat("é", 1500)})
	text, ok = sc.RenderEvent(domain.Event{Type: sc.TypeWebhook, Data: domain.Payload{Schema: sc.SchemaTrigger, Data: data}})
	if !ok || !strings.HasSuffix(text, "…") || !strings.Contains(text, "### A run of the poke job: webhook") || !utf8.ValidString(text) {
		t.Fatalf("webhook trigger: %q", text[:80])
	}
	if _, ok := sc.RenderEvent(domain.Event{Type: "github.issue.open", Data: domain.Payload{Schema: "github.issue.v1", Data: []byte(`{}`)}}); ok {
		t.Fatal("rendered another extension's Event")
	}
}
