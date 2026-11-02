package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// Turn states, as Claude Code's executor reports them, so every projector
// and playbook reads the same.
const (
	StateBuilding = "building"
	StateReview   = "review"
	StateHelp     = "help"
	StateDone     = "done"
	StateFinished = "finished"
)

// StatusSchema is the --output-schema a structured turn ends with. Codex
// checks it strictly, so every property is required and the optional ones
// may be empty.
var StatusSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"state": map[string]any{
			"type": "string",
			"enum": []string{StateBuilding, StateReview, StateHelp, StateDone, StateFinished},
			"description": "The state this turn leaves the work in; your playbook says which fits. " +
				"building: you started something that will report back, and you are resumed when it does. " +
				"review: something is ready for the owner to look at. " +
				"help: you asked the owner something you cannot go on without. " +
				"done: everything asked is handled and only the owner's last step is left. " +
				"Each of these keeps the work open, and you are resumed with whatever arrives next. " +
				"finished: the work is over and you have cleaned up; it completes, and nothing resumes it.",
		},
		"summary": map[string]any{"type": "string", "description": "What this turn did, in one line."},
		"handoff": map[string]any{"type": "string", "description": "A note to whoever picks this work up next: " +
			"what is pending, the judgement calls you made and why, and what to look at next. Empty when there is nothing to add."},
		"report": map[string]any{"type": "string", "description": "A write-up for the operator, in Markdown, shown with this turn in the run's history, " +
			"for findings too long for the one-line summary that aren't written down anywhere else. Empty otherwise."},
	},
	"required":             []string{"state", "summary", "handoff", "report"},
	"additionalProperties": false,
}

// Status is a structured turn's end.
type Status struct {
	State   string `json:"state"`
	Summary string `json:"summary"`
	Handoff string `json:"handoff,omitempty"`
	Report  string `json:"report,omitempty"`
}

// Tokens are a segment's token counts, summed over its turns.
type Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
}

// parsed is what a segment's `codex exec --json` events say.
type parsed struct {
	Thread    string
	Turns     int
	Failed    bool
	Error     string
	Tokens    *Tokens
	ToolCalls int
	Text      string
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
}

// parseEvents reads the events of every turn the segment ran. A turn
// that fails after an earlier one succeeded fails the segment.
func parseEvents(path string) parsed {
	var p parsed
	f, err := os.Open(path)
	if err != nil {
		return p
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for s.Scan() {
		var ev codexEvent
		if json.Unmarshal(s.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			if p.Thread == "" {
				p.Thread = ev.ThreadID
			}
		case "turn.started":
			p.Turns++
			p.Failed, p.Error = false, ""
		case "turn.completed":
			if ev.Usage != nil {
				p.Tokens = p.Tokens.add(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CachedInputTokens)
			}
		case "turn.failed":
			p.Failed = true
			if ev.Error != nil {
				p.Error = ev.Error.Message
			}
		case "error":
			if p.Error == "" {
				p.Error = ev.Message
			}
		case "item.completed":
			if ev.Item == nil {
				continue
			}
			switch ev.Item.Type {
			case "agent_message":
				p.Text = ev.Item.Text
			case "command_execution", "mcp_tool_call", "file_change", "web_search":
				p.ToolCalls++
			}
		}
	}
	return p
}

func (t *Tokens) add(in, out, cached int64) *Tokens {
	n := &Tokens{}
	if t != nil {
		*n = *t
	}
	n.Input += in
	n.Output += out
	n.CacheRead += cached
	return n
}

// readStatus reads a structured turn's last message: the status object,
// or nil when the message is not one.
func readStatus(path string) *Status {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s Status
	if json.Unmarshal([]byte(strings.TrimSpace(string(b))), &s) != nil {
		return nil
	}
	switch s.State {
	case StateBuilding, StateReview, StateHelp, StateDone, StateFinished:
		return &s
	}
	return nil
}

// limited reports a usage or rate limit in codex's error text.
func limited(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "usage limit") || strings.Contains(t, "rate limit") || strings.Contains(t, "quota")
}
