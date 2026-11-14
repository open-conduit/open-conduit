package claudecode

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

// A child's report shows the asking agent its result, and that the child is
// still open, so it knows more may come.
func TestRenderChildReported(t *testing.T) {
	e := domain.Event{Type: domain.TypeChildReported, Data: domain.Payload{Schema: domain.SchemaChildReported,
		Data: []byte(`{"work":"child/1","kind":"agent.task.engineer","state":"ready","status":"review","summary":"PR opened","result":"https://example.test/pr/1"}`)}}
	out := RenderEvents([]domain.Event{e})
	for _, want := range []string{"agent.task.engineer Work you asked for (child/1) reported (review)", "still open", "Summary: PR opened", "https://example.test/pr/1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
	}
}
