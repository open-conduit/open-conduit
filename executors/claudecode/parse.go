package claudecode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status is the structured result every work-issue turn ends with (Open
// Supervisor's status schema, unchanged so the playbook keeps working).
type Status struct {
	State   string `json:"state"`
	Summary string `json:"summary"`
	PRs     []int  `json:"prs,omitempty"`
	Preview string `json:"preview,omitempty"`
	Touches string `json:"touches,omitempty"`
	Handoff string `json:"handoff,omitempty"`
	Report  string `json:"report,omitempty"`
}

// Turn states a status may report.
const (
	StateBuilding = "building"
	StateReview   = "review"
	StateHelp     = "help"
	StateDone     = "done"
	StateFinished = "finished"
)

// StatusSchema is the JSON schema a structured turn is asked to end with.
var StatusSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"state": map[string]any{
			"type": "string",
			"enum": []string{StateBuilding, StateReview, StateHelp, StateDone, StateFinished},
			"description": "building: you pushed and are waiting for checks or a preview. review: something is up for the owner. " +
				"help: you asked the owner something you cannot go on without. done: everything asked is handled and only a merge or close is left. " +
				"finished: the issue's work is over (the issue closed, or the PR merged or closed where the playbook says that ends it) and you have cleaned up.",
		},
		"summary": map[string]any{"type": "string", "description": "What this turn did, in one line."},
		"prs":     map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Every PR number that belongs to this issue."},
		"preview": map[string]any{"type": "string", "description": "The deploy preview URL, once known."},
		"touches": map[string]any{"type": "string", "description": "What the work changes (pages, data, pipeline steps), in a few words, for other agents."},
		"handoff": map[string]any{"type": "string", "description": "A note to the session that picks this issue up next if this one has gone cold: " +
			"what is pending, the judgement calls you made and why, and what to check next. Intent only; the issue, the PR and git hold the facts."},
		"report": map[string]any{"type": "string", "description": "Optional: a write-up for the operator, in Markdown, shown with this turn in the run's history. " +
			"Use it for findings too long for the one-line summary that no issue or comment holds; leave it out otherwise."},
	},
	"required":             []string{"state", "summary"},
	"additionalProperties": false,
}

// Tokens are a turn's token counts.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// Add returns the sum; nil counts as zero.
func (t *Tokens) Add(u *Tokens) *Tokens {
	if t == nil && u == nil {
		return nil
	}
	out := &Tokens{}
	for _, x := range []*Tokens{t, u} {
		if x != nil {
			out.Input += x.Input
			out.Output += x.Output
			out.CacheRead += x.CacheRead
			out.CacheCreation += x.CacheCreation
		}
	}
	return out
}

// Totals are a session's cumulative cost and per-model usage as claude
// reports them on a result: a resumed session reports since it began.
type Totals struct {
	Session string            `json:"session"`
	Cost    *float64          `json:"cost,omitempty"`
	Models  map[string]Tokens `json:"models,omitempty"`
}

// parsed is how a turn ended, as read from claude's output.
type parsed struct {
	OK         bool
	Status     *Status
	Text       string
	Cost       *float64
	Tokens     *Tokens
	Models     []string
	ModelCalls int
	ToolCalls  int
	Error      string
	Subtype    string
	Limited    bool
	ResetsAt   *time.Time
	Totals     *Totals
}

type result struct {
	Type             string                    `json:"type"`
	Subtype          string                    `json:"subtype"`
	IsError          bool                      `json:"is_error"`
	APIError         string                    `json:"api_error"`
	Result           any                       `json:"result"`
	SessionID        string                    `json:"session_id"`
	TotalCostUSD     *float64                  `json:"total_cost_usd"`
	NumTurns         int                       `json:"num_turns"`
	StructuredOutput json.RawMessage           `json:"structured_output"`
	ModelUsage       map[string]map[string]any `json:"modelUsage"`
}

// lastResult is the last result in claude's output: one JSON object, or one
// per line (stream-json). A message that slipped in just after a result starts
// another in the same process; the totals are cumulative, so the last covers
// both.
func lastResult(stdout string) *result {
	var r result
	trimmed := strings.TrimSpace(stdout)
	if json.Unmarshal([]byte(trimmed), &r) == nil && r.Type == "result" {
		return &r
	}
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m result
		if json.Unmarshal([]byte(lines[i]), &m) == nil && m.Type == "result" {
			return &m
		}
	}
	return nil
}

// toolCalls counts the tool calls in the turn's assistant messages, a
// subagent's included.
func toolCalls(stdout string) int {
	n := 0
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, `"tool_use"`) {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Type != "assistant" {
			continue
		}
		for _, c := range m.Message.Content {
			if c.Type == "tool_use" {
				n++
			}
		}
	}
	return n
}

func num(m map[string]any, k string) int64 {
	if f, ok := m[k].(float64); ok {
		return int64(f)
	}
	return 0
}

func modelTokens(raw map[string]map[string]any) map[string]Tokens {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]Tokens, len(raw))
	for model, m := range raw {
		out[model] = Tokens{Input: num(m, "inputTokens"), Output: num(m, "outputTokens"), CacheRead: num(m, "cacheReadInputTokens"), CacheCreation: num(m, "cacheCreationInputTokens")}
	}
	return out
}

// usage sums the tokens per model, less what before had for the same model,
// and lists the models that used any.
func usage(models, before map[string]Tokens) (*Tokens, []string) {
	if len(models) == 0 {
		return nil, nil
	}
	t := &Tokens{}
	var used []string
	for model, m := range models {
		b := before[model]
		u := Tokens{Input: m.Input - b.Input, Output: m.Output - b.Output, CacheRead: m.CacheRead - b.CacheRead, CacheCreation: m.CacheCreation - b.CacheCreation}
		if u == (Tokens{}) {
			continue
		}
		used = append(used, model)
		t = t.Add(&u)
	}
	sort.Strings(used)
	return t, used
}

// perTurn makes a parsed turn its own. On a resumed session claude reports
// the cost and tokens since the session began, so the totals of the session's
// previous result (kept in the executor's continuity) come off. A session that
// has gone backwards, as after a fork, is left as it is.
func perTurn(p parsed, prev *Totals) parsed {
	cur := p.Totals
	if cur == nil || cur.Session == "" || prev == nil || prev.Session != cur.Session {
		return p
	}
	if prev.Cost == nil || cur.Cost == nil || *prev.Cost > *cur.Cost {
		return p
	}
	for model, b := range prev.Models {
		m := cur.Models[model]
		if b.Input > m.Input || b.Output > m.Output || b.CacheRead > m.CacheRead || b.CacheCreation > m.CacheCreation {
			return p
		}
	}
	cost := *cur.Cost - *prev.Cost
	p.Cost = &cost
	p.Tokens, p.Models = usage(cur.Models, prev.Models)
	return p
}

// parse turns claude's output into a parsed turn. code is nil for an adopted
// turn, whose exit code is unknown.
func parse(stdout, stderr string, code *int, structured bool, now time.Time) parsed {
	r := lastResult(stdout)
	if r == nil {
		tail := strings.TrimSpace(stderr)
		if tail == "" {
			tail = strings.TrimSpace(stdout)
		}
		lines := strings.Split(tail, "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		tail = truncate(strings.Join(lines, " "), 300)
		exit := "unknown"
		if code != nil {
			exit = strconv.Itoa(*code)
		}
		msg := "claude exited " + exit + " without a result"
		if tail != "" {
			msg += ": " + tail
		}
		return parsed{Error: msg}
	}
	p := parsed{Subtype: r.Subtype, Cost: r.TotalCostUSD}
	if s, ok := r.Result.(string); ok {
		p.Text = s
	}
	models := modelTokens(r.ModelUsage)
	if r.SessionID != "" {
		p.Totals = &Totals{Session: r.SessionID, Cost: r.TotalCostUSD, Models: models}
	}
	p.ModelCalls, p.ToolCalls = r.NumTurns, toolCalls(stdout)
	p.Tokens, p.Models = usage(models, nil)
	if r.IsError || (code != nil && *code != 0) {
		p.Limited, p.ResetsAt = limitHit(r, stdout, now)
		text := truncate(p.Text, 300)
		// claude marks some failed turns "success", a limit hit among them: the
		// text alone says what went wrong.
		sub := r.Subtype
		switch {
		case text != "" && (sub == "success" || p.Limited):
			p.Error = text
		case text != "":
			p.Error = sub + ": " + text
		case p.Limited:
			p.Error = "hit the usage limit"
		case sub == "" || sub == "success":
			p.Error = "error"
		default:
			p.Error = sub
		}
		return p
	}
	if !structured {
		p.OK = true
		return p
	}
	var s Status
	if len(r.StructuredOutput) == 0 || json.Unmarshal(r.StructuredOutput, &s) != nil || s.State == "" {
		p.Error = fmt.Sprintf("the turn ended without a status (%s)", r.Subtype)
		return p
	}
	switch s.State {
	case StateBuilding, StateReview, StateHelp, StateDone, StateFinished:
	default:
		p.Error = "the turn ended with an unknown status"
		return p
	}
	p.OK = true
	p.Status = &s
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// limitHit reports whether a turn stopped on the subscription's usage limit,
// and when the limit lifts if claude said. The API error on the result or the
// assistant's stand-in message says so first-hand; claude's wording ("You've
// hit your session limit · resets 9:40am (Europe/London)") covers versions
// that don't carry it.
func limitHit(r *result, stdout string, now time.Time) (bool, *time.Time) {
	text, _ := r.Result.(string)
	hit := r.APIError == "usage_limit_reached" || limitText.MatchString(text)
	var resets *time.Time
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, `"resetsAt"`) || !strings.Contains(line, `"rejected"`) {
			continue
		}
		var m struct {
			APIError  string    `json:"api_error"`
			RateLimit rateLimit `json:"rate_limit_info"`
			Params    struct {
				RateLimit rateLimit `json:"rate_limit_info"`
			} `json:"api_error_params"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.APIError == "usage_limit_reached" {
			hit = true
		}
		// The assistant's error and the rate_limit_event before it both carry
		// the window that turned it away.
		for _, rl := range []rateLimit{m.Params.RateLimit, m.RateLimit} {
			if rl.Status == "rejected" && rl.ResetsAt > 0 {
				t := time.Unix(rl.ResetsAt, 0).UTC()
				resets = &t
			}
		}
	}
	if !hit {
		return false, nil
	}
	if resets == nil {
		resets = resetFromText(text, now)
	}
	return true, resets
}

type rateLimit struct {
	Status   string `json:"status"`
	ResetsAt int64  `json:"resetsAt"`
}

var (
	limitText = regexp.MustCompile(`(?i)you've hit your .*limit`)
	// "resets 9:40am (Europe/London)", "resets 3am (Europe/London)",
	// "resets Oct 27, 3pm (Europe/London)".
	resetText = regexp.MustCompile(`(?i)resets (?:([A-Z][a-z]{2}) (\d{1,2}),? )?(\d{1,2})(?::(\d{2}))?\s*(am|pm)(?: \(([^)]+)\))?`)
)

// resetFromText reads the reset time out of claude's limit message: the next
// such time after now, in the zone it names (the local one if it names none
// or one this machine doesn't know).
func resetFromText(text string, now time.Time) *time.Time {
	m := resetText.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	loc := time.Local
	if m[6] != "" {
		if l, err := time.LoadLocation(m[6]); err == nil {
			loc = l
		}
	}
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	if hour == 12 {
		hour = 0
	}
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}
	local := now.In(loc)
	if m[1] != "" {
		month, err := time.Parse("Jan", m[1])
		if err != nil {
			return nil
		}
		day, _ := strconv.Atoi(m[2])
		t := time.Date(local.Year(), month.Month(), day, hour, minute, 0, 0, loc)
		if t.Before(now.Add(-24 * time.Hour)) {
			t = t.AddDate(1, 0, 0)
		}
		t = t.UTC()
		return &t
	}
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	t = t.UTC()
	return &t
}
