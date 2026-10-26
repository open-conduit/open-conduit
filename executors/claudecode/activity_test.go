package claudecode

import (
	"strings"
	"testing"
)

func TestRenderActivity(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-haiku"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the issue."},{"type":"tool_use","name":"mcp__open-conduit__call","input":{"operation":"github.issue.view","input":{}}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"{\"title\":\"Header wobbles\"}"}]}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"permission denied"}]}}`,
		`not json`,
		`{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":0.0123}`,
	}, "\n")
	got := RenderActivity(stdout)
	for _, want := range []string{"session started (claude-haiku)", "assistant: Reading the issue.", `tool mcp__open-conduit__call {"operation":"github.issue.view","input":{}}`,
		`result {"title":"Header wobbles"}`, "error permission denied", "turn ended (success, 3 model turns, $0.0123 session cost)"} {
		if !strings.Contains(got, want) {
			t.Errorf("activity lacks %q:\n%s", want, got)
		}
	}
	long := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 3000) + `"}]}}`
	if out := RenderActivity(long); len(out) > activityText+40 || !strings.Contains(out, "…") {
		t.Fatalf("long text is clipped: %d", len(out))
	}
	if RenderActivity("") != "" {
		t.Fatal("empty output renders nothing")
	}
}
