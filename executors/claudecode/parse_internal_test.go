package claudecode

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
)

var now = time.Date(2026, 10, 24, 8, 11, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	code := 1
	p := parse(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out"}`, "", &code, true, now)
	if p.OK || !strings.Contains(p.Error, "error_max_turns") {
		t.Fatalf("%+v", p)
	}
	p = parse("", "boom\nreally", nil, true, now)
	if p.OK || !strings.Contains(p.Error, "unknown without a result: boom really") {
		t.Fatalf("%+v", p)
	}
	ok := 0
	p = parse(`{"type":"result","subtype":"success","result":"x"}`, "", &ok, true, now)
	if p.OK || !strings.Contains(p.Error, "without a status") {
		t.Fatalf("%+v", p)
	}
	p = parse(`{"type":"result","subtype":"success","result":"x","structured_output":{"state":"sleeping","summary":"x"}}`, "", &ok, true, now)
	if p.OK || !strings.Contains(p.Error, "unknown status") {
		t.Fatalf("%+v", p)
	}
	p = parse(`{"type":"result","subtype":"success","result":"Swept 3"}`, "", &ok, false, now)
	if !p.OK || p.Text != "Swept 3" {
		t.Fatalf("%+v", p)
	}
	p = parse(`{"type":"result","subtype":"success","result":"x","structured_output":{"state":"review","summary":"did it","prs":[7],"handoff":"pending: the colour"}}`, "", &ok, true, now)
	if !p.OK || p.Status.State != StateReview || p.Status.PRs[0] != 7 || p.Status.Handoff != "pending: the colour" {
		t.Fatalf("%+v", p)
	}
}

func TestParseUsageLimit(t *testing.T) {
	out := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792831200,"rateLimitType":"five_hour"}}
{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 9:40am (Europe/London)"}]},"error":"rate_limit","api_error":"usage_limit_reached","api_error_params":{"rate_limit_info":{"status":"rejected","resetsAt":1792831200,"rateLimitType":"five_hour"}}}
{"type":"result","subtype":"success","is_error":true,"api_error":"usage_limit_reached","num_turns":1,"total_cost_usd":0,"result":"You've hit your session limit · resets 9:40am (Europe/London)"}`
	code := 1
	p := parse(out, "", &code, true, now)
	if p.OK || !p.Limited || p.ResetsAt == nil || p.ResetsAt.Unix() != 1792831200 {
		t.Fatalf("%+v", p)
	}
	if p.Error != "You've hit your session limit · resets 9:40am (Europe/London)" {
		t.Fatalf("error %q", p.Error)
	}
	// Without the API error, the wording alone says so, and when.
	p = parse(`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 9:40am (Europe/London)"}`, "", &code, true, now)
	if !p.Limited || p.ResetsAt == nil || strings.HasPrefix(p.Error, "success") {
		t.Fatalf("%+v", p)
	}
	p = parse(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`, "", &code, true, now)
	if p.Limited || p.Error != "error_during_execution: boom" {
		t.Fatalf("%+v", p)
	}
}

func TestResetFromText(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no zone data")
	}
	at := time.Date(2026, 10, 24, 8, 11, 0, 0, london)
	cases := []struct {
		text string
		want time.Time
	}{
		{"You've hit your session limit · resets 9:40am (Europe/London)", time.Date(2026, 10, 24, 9, 40, 0, 0, london)},
		{"You've hit your session limit · resets 3am (Europe/London)", time.Date(2026, 10, 25, 3, 0, 0, 0, london)},
		{"You've hit your session limit · resets 12:30pm (Europe/London)", time.Date(2026, 10, 24, 12, 30, 0, 0, london)},
		{"You've hit your weekly limit · resets Oct 27, 3pm (Europe/London)", time.Date(2026, 10, 27, 15, 0, 0, 0, london)},
	}
	for _, c := range cases {
		got := resetFromText(c.text, at)
		if got == nil || !got.Equal(c.want) {
			t.Errorf("%q: got %v, want %v", c.text, got, c.want)
		}
	}
	if resetFromText("You've hit your session limit", at) != nil {
		t.Error("a time from a message without one")
	}
}

func TestParseCountsCalls(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a tool_use"},{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"\"tool_use\""}]}}`,
		`{"type":"assistant","parent_tool_use_id":"x","message":{"content":[{"type":"tool_use","name":"Grep"}]}}`,
		`{"type":"result","subtype":"success","num_turns":4,"total_cost_usd":0.1}`,
	}, "\n")
	p := parse(out, "", nil, false, now)
	if p.ModelCalls != 4 || p.ToolCalls != 3 {
		t.Fatalf("model calls %d, tool calls %d", p.ModelCalls, p.ToolCalls)
	}
}

// TestPerTurnTakesOffTheSessionsEarlierTotals ports the cumulative usage
// delta: a resumed session reports totals since it began, so the totals of
// the session's previous result come off; another session starts from
// nothing, and totals that went backwards are left alone.
func TestPerTurnTakesOffTheSessionsEarlierTotals(t *testing.T) {
	res := func(session string, cost float64, read int) string {
		return `{"type":"result","subtype":"success","session_id":"` + session + `","total_cost_usd":` + ftoa(cost) + `,"modelUsage":{"opus":{"inputTokens":10,"outputTokens":` + itoa(read/10) + `,"cacheReadInputTokens":` + itoa(read) + `,"cacheCreationInputTokens":3},"haiku":{"inputTokens":1}}}`
	}
	first := parse(res("s1", 8.5, 1000), "", nil, false, now)
	p := perTurn(first, nil)
	if *p.Cost != 8.5 || p.Tokens.CacheRead != 1000 || len(p.Models) != 2 {
		t.Fatalf("a session's first turn keeps its totals: %+v %+v", p, p.Tokens)
	}
	second := parse(res("s1", 9, 1500), "", nil, false, now)
	p = perTurn(second, first.Totals)
	if math.Abs(*p.Cost-0.5) > 1e-9 || *p.Tokens != (Tokens{Output: 50, CacheRead: 500}) || len(p.Models) != 1 || p.Models[0] != "opus" {
		t.Fatalf("a resumed turn is its own: %+v %+v", p, p.Tokens)
	}
	other := parse(res("s2", 1, 100), "", nil, false, now)
	if p := perTurn(other, second.Totals); *p.Cost != 1 {
		t.Fatalf("another session starts from nothing: %+v", p)
	}
	back := parse(res("s1", 2, 200), "", nil, false, now)
	if p := perTurn(back, second.Totals); *p.Cost != 2 || p.Tokens.CacheRead != 200 {
		t.Fatalf("totals that went backwards are left alone: %+v", p)
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
func itoa(i int) string     { return strconv.Itoa(i) }

func TestBodyAndPrompt(t *testing.T) {
	w := domain.Work{ID: "w1", CorrelationKeys: []domain.CorrelationKey{{Source: "github/x", ExternalID: "issue/12/generation/reopen-99"}}}
	if got := expand("{number}", w); got != "12" {
		t.Fatalf("number %q", got)
	}
	if got := expand("agent/issue-{number} {key} {work}", w); got != "agent/issue-12 issue/12/generation/reopen-99 w1" {
		t.Fatalf("template %q", got)
	}
	args, text := body(kindEvents, "12", "", "### an event")
	if p := prompt("work-issue", args, text); p != "/open-conduit:work-issue 12 events\n\n## Since your last turn\n\n### an event" {
		t.Fatalf("prompt %q", p)
	}
	args, text = body(kindHandoff, "12", "kept the old colour", "### blue please")
	if args != "12 handoff" || !strings.Contains(text, "## Handoff from your last session\n\nkept the old colour") || !strings.Contains(text, "## Since your last turn\n\n### blue please") {
		t.Fatalf("handoff %q %q", args, text)
	}
	args, text = body(kindStart, "12", "", "")
	if p := prompt("work-issue", args, text); p != "/open-conduit:work-issue 12" {
		t.Fatalf("start prompt %q", p)
	}
	if p := prompt("-", "", "just this"); p != "just this" {
		t.Fatalf("no playbook %q", p)
	}
}
