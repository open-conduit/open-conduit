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

// Work's first turn reads what it was asked: an operator's context for Work
// started through the management API, the asking agent's for a child. The
// task is the context's task, else its body or text, else the context.
func TestRenderAsked(t *testing.T) {
	created := func(source, data string) domain.Event {
		return domain.Event{Type: "core.work.created", Source: source, Data: domain.Payload{Schema: "core.work.created.v1", Data: []byte(data)}}
	}
	for _, c := range []struct {
		name string
		e    domain.Event
		want []string
	}{
		{"operator task", created("core.manual", `{"WorkID":"manual/1","ParentID":"","Kind":"agent.brief","Context":{"Schema":"brief.v1","Data":{"task":"Summarise the week","due":"friday"}}}`),
			[]string{"### An operator started this agent.brief Work. The task:\n\nSummarise the week"}},
		{"operator body", created("core.manual", `{"WorkID":"manual/1","ParentID":"","Kind":"agent.brief","Context":{"Schema":"brief.v1","Data":{"body":"Read the report"}}}`),
			[]string{"The task:\n\nRead the report"}},
		{"operator data", created("core.manual", `{"WorkID":"manual/1","ParentID":"","Kind":"agent.brief","Context":{"Schema":"brief.v1","Data":{"city":"Leeds"}}}`),
			[]string{`The task:` + "\n\n" + `{"city":"Leeds"}`}},
		{"operator no context", created("core.manual", `{"WorkID":"manual/1","ParentID":"","Kind":"agent.brief"}`),
			[]string{"### An operator started this agent.brief Work\n\nIt gave no task."}},
		{"operator child", created("core.manual", `{"WorkID":"manual/2","ParentID":"work/1","Kind":"agent.brief","Context":{"Schema":"brief.v1","Data":{"task":"Check it"}}}`),
			[]string{"### An operator started this agent.brief Work. The task:\n\nCheck it"}},
		{"agent child", created("execution/1", `{"WorkID":"child/1","ParentID":"work/1","Kind":"agent.task.engineer","Context":{"Schema":"task.v1","Data":{"task":"Fix the bug"}}}`),
			[]string{"### Another agent asked for this agent.task.engineer Work. The task:\n\nFix the bug"}},
	} {
		out := RenderEvents([]domain.Event{c.e})
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: the turn lacks %q:\n%s", c.name, want, out)
			}
		}
	}
}

// Negative control: Work nobody asked for, a successor or manual Work
// created before its creation carried a kind, is not dressed up as a task.
func TestRenderAskedLeavesUnaskedWork(t *testing.T) {
	for _, data := range []string{`{"WorkID":"w/g2","ParentID":"","PredecessorID":"w"}`, `{"WorkID":"manual/1","ParentID":""}`} {
		for _, source := range []string{"core.work", "core.manual"} {
			out := RenderEvents([]domain.Event{{Type: "core.work.created", Source: source, Data: domain.Payload{Schema: "core.work.created.v1", Data: []byte(data)}}})
			if strings.Contains(out, "started this") || strings.Contains(out, "asked for this") {
				t.Fatalf("Work nobody asked for read as asked (%s):\n%s", source, out)
			}
		}
	}
}
