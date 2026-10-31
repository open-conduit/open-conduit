package web

import (
	"bytes"
	"encoding/json"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// A chat is a turn's conversation as the Work page shows it: people and
// the agent as messages, what the agent sent elsewhere through the
// toolbelt as a sent message with its text, and everything else as quiet
// activity rows. It is built from any Messages-API transcript
// (parseConversation) and knows no tool or operation by name, except the
// toolbelt's own server and its core operations.
type chat struct {
	Blocks []chatBlock
	// Counts are how many items each filter shows.
	Counts map[string]int
	// Errors counts the failed calls and failure notes.
	Errors int
	// Model is the model the session reported at its start; Calls the model
	// calls the session reported at its end; Tools the tool calls made.
	Model string
	Calls int
	Tools int
}

// Transcript filters: messages, toolbelt calls, other tool calls and the
// session's own lines.
const (
	fMessages = "messages"
	fToolbelt = "toolbelt"
	fTools    = "tools"
	fSystem   = "system"
)

var chatFilters = []string{fMessages, fToolbelt, fTools, fSystem}

var filterNames = map[string]string{fMessages: "Messages", fToolbelt: "Toolbelt", fTools: "Tool calls", fSystem: "System"}

// chatBlock is one block of the chat. Kind is prompt, agent, person,
// thinking, sent, call, tool, tools (a group of tool calls), system (a run
// of the session's own lines), orch (an orchestration rule: park, wake,
// recovery, cancel) or note.
type chatBlock struct {
	Kind   string
	Filter string
	At     time.Time
	// Key names a block that opens and closes (a group, a thinking block)
	// the same way on every redraw, so a live transcript keeps it open.
	Key  string
	Text string
	// Full is the whole of a clipped prompt.
	Full string
	// Title is a prompt's first line.
	Title string
	// Author, Via and Initial are a person's message's; Injected marks one
	// delivered into the running turn.
	Author, Via, Initial string
	Injected             bool
	// Tool is a single call; Group a run of calls.
	Tool  *chatTool
	Group []chatTool
	// Summary is a group's "Bash ×7 · Read ×4".
	Summary string
	Failed  int
	MS      int64
	// Lines are a system block's lines.
	Lines []chatLine
	// Tone is an orchestration rule's: park, wake, inject, cancel, start.
	Tone string
	// Error marks the first failure, for "Jump to error".
	First bool
}

type chatLine struct {
	At   time.Time
	Text string
}

// chatTool is a tool call as a row, or a toolbelt call as a sent message.
type chatTool struct {
	Key string
	// Name is the tool, or the toolbelt operation.
	Name string
	// Label is what the call was about (its headline), hidden when it
	// repeats the name.
	Label string
	// Owner is the part of an extension operation before its first dot;
	// empty for core operations and other tools.
	Owner string
	At    time.Time
	// Prose is the message a toolbelt call carried; Facts short scalars of
	// its result.
	Prose string
	Facts []string
	Link  *client.DisplayLink
	Input []field
	// Raw is the toolbelt call's request as JSON.
	Raw     string
	Result  string
	Clipped int64
	Failed  bool
	Running bool
	Lost    bool
	Steps   []entry
}

// coreOps are the toolbelt's builtin operations; any other is an
// extension's.
var coreOps = map[string]bool{"read_context": true, "emit": true, "wait": true, "declare_outcome": true, "create_work": true, "call": true}

// toolbeltServer is the toolbelt's MCP server name.
const toolbeltServer = "open-conduit"

// buildChat turns a conversation into chat blocks, with the run's parks,
// wakes and recoveries placed by time.
func buildChat(c conversation, run client.Run) chat {
	ch := chat{Counts: map[string]int{}}
	prompted := false
	thoughts := map[string]int{}
	for _, e := range c.Entries {
		at := parseTime(e.At)
		switch e.Kind {
		case "user":
			if !prompted {
				prompted = true
				text := e.Text
				if e.Full != "" {
					text = e.Full
				}
				ch.add(chatBlock{Kind: "prompt", Filter: fMessages, At: at, Title: firstLine(text), Text: text})
				continue
			}
			ch.add(chatBlock{Kind: "person", Filter: fMessages, At: at, Author: "Input", Via: "delivered into the running turn", Initial: "I", Injected: true, Text: e.Text})
		case "assistant":
			ch.add(chatBlock{Kind: "agent", Filter: fMessages, At: at, Text: e.Text})
		case "thinking":
			// Keyed by time, not position, so trimming the transcript's start
			// doesn't move a key onto another block.
			k := "think-" + strconv.FormatInt(at.UnixNano(), 36)
			thoughts[k]++
			if n := thoughts[k]; n > 1 {
				k += "-" + strconv.Itoa(n)
			}
			ch.add(chatBlock{Kind: "thinking", Filter: fMessages, At: at, Key: k, Text: e.Text})
		case "note":
			if m, ok := strings.CutPrefix(e.Text, "Session started on "); ok && ch.Model == "" {
				ch.Model = m
			}
			if n := endCalls.FindStringSubmatch(e.Text); n != nil {
				ch.Calls, _ = strconv.Atoi(n[1])
			}
			ch.add(chatBlock{Kind: "system", Filter: fSystem, At: at, Lines: []chatLine{{at, e.Text}}})
		case "tool":
			if e.Tool == nil {
				continue
			}
			ch.Tools++
			t := chatToolOf(*e.Tool, at)
			if e.Tool.Server == toolbeltServer {
				kind := "call"
				if t.Prose != "" {
					kind = "sent"
				}
				ch.add(chatBlock{Kind: kind, Filter: fToolbelt, At: at, Tool: &t})
				continue
			}
			ch.add(chatBlock{Kind: "tool", Filter: fTools, At: at, Tool: &t})
		}
	}
	ch.orchestrate(run)
	ch.group()
	return ch
}

var endCalls = regexp.MustCompile(`(\d+) model calls?`)

func (ch *chat) add(b chatBlock) {
	ch.Counts[b.Filter]++
	ch.Blocks = append(ch.Blocks, b)
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			t, _ := clip(l, 100)
			return t
		}
	}
	return "Prompt"
}

// chatToolOf reads a call: a toolbelt call's operation (the call tool's
// operation field), the prose it carried and the facts its result gave.
func chatToolOf(t toolCall, at time.Time) chatTool {
	c := chatTool{Key: t.ID, Name: t.Name, Label: t.Headline, At: at, Input: t.Input, Result: t.Result, Clipped: t.Clipped, Failed: t.Error, Running: !t.Done, Lost: t.Lost, Steps: t.Steps}
	if c.Label == c.Name {
		c.Label = ""
	}
	if t.Server != toolbeltServer {
		return c
	}
	in := map[string]string{}
	for _, f := range t.Input {
		in[f.Key] = f.Value
	}
	raw := ""
	if t.Name == "call" && in["operation"] != "" {
		c.Name = in["operation"]
		raw = in["input"]
	} else {
		var b bytes.Buffer
		obj := map[string]json.RawMessage{}
		for _, f := range t.Input {
			v := json.RawMessage(strconv.Quote(f.Value))
			if json.Valid([]byte(f.Value)) && !isString(f.Value) {
				v = json.RawMessage(f.Value)
			}
			obj[f.Key] = v
		}
		if j, err := json.Marshal(obj); err == nil && json.Indent(&b, j, "", "  ") == nil {
			raw = b.String()
		}
	}
	if c.Label == c.Name || c.Label == in["operation"] {
		c.Label = ""
	}
	if !coreOps[c.Name] {
		c.Owner = sourceFamily(c.Name)
	}
	c.Raw = raw
	if v, ok := decodeOrdered(raw); ok {
		c.Prose = proseIn(v)
	} else if t.Name == "call" {
		c.Prose = proseIn(raw)
	}
	if v, ok := decodeOrdered(t.Result); ok {
		c.Facts = factsIn(v)
		c.Link = linkIn(v)
	} else if r := strings.TrimSpace(t.Result); r != "" && len(r) <= 60 && !strings.Contains(r, "\n") {
		c.Facts = []string{r}
	}
	return c
}

func isString(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), `"`) }

// orchestrate places the run's parks, wakes, recoveries and cancellation
// among the blocks by time. Without times in the transcript there is
// nowhere to place them, and they are left out.
func (ch *chat) orchestrate(run client.Run) {
	var rules []chatBlock
	for i, s := range run.Segments {
		start, end := parseTime(s.StartedAt), parseTime(s.EndedAt)
		if i > 0 {
			switch {
			case s.Recovered:
				rules = append(rules, chatBlock{Kind: "orch", Tone: "start", At: start, Text: "Recovered by a new owner; the turn continues"})
			case s.WakeEventID != "":
				rules = append(rules, chatBlock{Kind: "orch", Tone: "wake", At: start, Text: "Woken by event " + shortID(s.WakeEventID) + "; resumed in this session"})
			default:
				rules = append(rules, chatBlock{Kind: "orch", Tone: "wake", At: start, Text: "Resumed in this session"})
			}
		}
		if s.Parked && !end.IsZero() {
			rules = append(rules, chatBlock{Kind: "orch", Tone: "park", At: end, Text: "Parked: waiting for an event to resume"})
		}
	}
	if run.State == "cancelled" {
		at := parseTime(run.FinishedAt)
		rules = append(rules, chatBlock{Kind: "orch", Tone: "cancel", At: at, Text: "Cancelled"})
	}
	timed := false
	for _, b := range ch.Blocks {
		timed = timed || !b.At.IsZero()
	}
	if len(rules) == 0 || !timed {
		return
	}
	for _, r := range rules {
		i := sort.Search(len(ch.Blocks), func(i int) bool { return !ch.Blocks[i].At.IsZero() && ch.Blocks[i].At.After(r.At) })
		ch.Blocks = append(ch.Blocks[:i], append([]chatBlock{r}, ch.Blocks[i:]...)...)
	}
}

func shortID(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}

// group joins consecutive tool calls into one block and consecutive
// system lines into one, and marks the first failure.
func (ch *chat) group() {
	var out []chatBlock
	for _, b := range ch.Blocks {
		n := len(out)
		switch {
		case b.Kind == "tool" && n > 0 && out[n-1].Kind == "tools":
			out[n-1].Group = append(out[n-1].Group, *b.Tool)
		case b.Kind == "tool":
			// A group grows at its end, so its first call names it.
			out = append(out, chatBlock{Kind: "tools", Filter: fTools, At: b.At, Key: "group-" + b.Tool.Key, Group: []chatTool{*b.Tool}})
		case b.Kind == "system" && n > 0 && out[n-1].Kind == "system":
			out[n-1].Lines = append(out[n-1].Lines, b.Lines...)
		default:
			out = append(out, b)
		}
	}
	for i := range out {
		b := &out[i]
		if b.Kind == "tools" {
			counts := map[string]int{}
			var order []string
			for _, t := range b.Group {
				if counts[t.Name] == 0 {
					order = append(order, t.Name)
				}
				counts[t.Name]++
				if t.Failed {
					b.Failed++
				}
			}
			parts := make([]string, len(order))
			for j, name := range order {
				parts[j] = name + " ×" + strconv.Itoa(counts[name])
			}
			b.Summary = strings.Join(parts, " · ")
			if len(b.Group) < 3 {
				// Fewer than three show as rows of their own.
				b.Kind = "rows"
			}
		}
		failed := b.Failed > 0 || (b.Tool != nil && b.Tool.Failed)
		if failed {
			ch.Errors++
			if ch.Errors == 1 {
				b.First = true
			}
		}
	}
	ch.Blocks = out
}

// show filters the chat to the filters on and to blocks matching q, the
// way the toolbar does without script.
func (ch chat) show(on map[string]bool, q string) []chatBlock {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []chatBlock
	for _, b := range ch.Blocks {
		if b.Kind != "orch" && !on[b.Filter] {
			continue
		}
		if q != "" && b.Kind != "orch" && !strings.Contains(strings.ToLower(b.text()), q) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// text is everything a block says, for finding in it.
func (b chatBlock) text() string {
	var s strings.Builder
	s.WriteString(b.Text + " " + b.Title + " " + b.Summary)
	for _, l := range b.Lines {
		s.WriteString(" " + l.Text)
	}
	tools := b.Group
	if b.Tool != nil {
		tools = append(tools, *b.Tool)
	}
	for _, t := range tools {
		s.WriteString(" " + t.Name + " " + t.Label + " " + t.Prose + " " + t.Raw + " " + t.Result)
		for _, f := range t.Input {
			s.WriteString(" " + f.Value)
		}
	}
	return s.String()
}

// ---- prose and facts: what a toolbelt call said, not the JSON it said it in ----

// kv is one member of a JSON object, in order.
type kv struct {
	Key string
	Val any
}

// decodeOrdered decodes JSON keeping objects' member order: an object is
// []kv, an array []any.
func decodeOrdered(s string) (any, bool) {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return nil, false
	}
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	v, err := ordered(d)
	return v, err == nil
}

func ordered(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		if t == '{' {
			var out []kv
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				v, err := ordered(d)
				if err != nil {
					return nil, err
				}
				out = append(out, kv{k.(string), v})
			}
			_, err := d.Token()
			return out, err
		}
		var out []any
		for d.More() {
			v, err := ordered(d)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		_, err := d.Token()
		return out, err
	}
	return t, nil
}

// proseIn is the longest prose string anywhere in v: over 40 characters,
// with whitespace, not JSON itself. It is the message a call carried,
// whatever the operation.
func proseIn(v any) string {
	best := ""
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			t := strings.TrimSpace(x)
			if len(x) > 40 && strings.ContainsAny(x, " \t\n") && !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") && len(x) > len(best) {
				best = x
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		case []kv:
			for _, m := range x {
				walk(m.Val)
			}
		}
	}
	walk(v)
	return best
}

// factsIn is up to three short scalar members of v, as "key value": not a
// schema, nothing with braces or quotes in it, no URL (the link shows that).
func factsIn(v any) []string {
	var out []string
	var walk func(x any, key string, depth int)
	walk = func(x any, key string, depth int) {
		if len(out) >= 3 || depth > 3 {
			return
		}
		switch x := x.(type) {
		case []kv:
			for _, m := range x {
				walk(m.Val, m.Key, depth+1)
			}
		case string:
			if len(x) <= 40 && key != "schema" && !strings.ContainsAny(x, `{}"`) && !strings.Contains(x, "://") && key != "" {
				out = append(out, key+" "+x)
			}
		case json.Number:
			if key != "" {
				out = append(out, key+" "+x.String())
			}
		case bool:
			if key != "" {
				out = append(out, key+" "+strconv.FormatBool(x))
			}
		}
	}
	walk(v, "", 0)
	return out
}

// linkIn is the first http(s) URL in v, labelled by its last two path
// segments ("pull/131").
func linkIn(v any) *client.DisplayLink {
	var found string
	var walk func(any)
	walk = func(x any) {
		if found != "" {
			return
		}
		switch x := x.(type) {
		case string:
			if (strings.HasPrefix(x, "https://") || strings.HasPrefix(x, "http://")) && !strings.ContainsAny(x, " \n") {
				found = x
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		case []kv:
			for _, m := range x {
				walk(m.Val)
			}
		}
	}
	walk(v)
	if found == "" {
		return nil
	}
	rest := found[strings.Index(found, "://")+3:]
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	label := parts[0]
	if len(parts) >= 3 {
		label = strings.Join(parts[len(parts)-2:], "/")
	}
	return &client.DisplayLink{Label: label, URL: found}
}

// ---- formatting ----

// prose is light formatting for messages sent elsewhere and outcomes:
// paragraphs, line breaks, `code`, **bold** and chat-style *bold*. All of
// it is escaped first.
func prose(s string) template.HTML {
	return `<div class="prose">` + markdown(s) + `</div>`
}

// codeLine is one line of a code block with its highlight: an added,
// removed or hunk line of a diff, or a real failure line in a failed
// call's result.
type codeLine struct {
	Text  string
	Class string
}

// failureLine matches the lines that say a thing failed, not prose that
// mentions an error.
var failureLine = regexp.MustCompile(`(?i)^(--- )?FAIL|^panic:|\berror:|no such host|exit status [1-9]`)

var diffLine = regexp.MustCompile(`^[+-]\s|^@@`)

// code is a code block's lines: the first n shown, the rest behind "Show
// all". Only bad (a failed call's result) highlights failure lines.
type code struct {
	Label string
	Shown []codeLine
	More  []codeLine
	Total int
}

func codeOf(label, text string, n int, bad bool) code {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	diff := false
	for _, l := range lines {
		diff = diff || diffLine.MatchString(l)
	}
	c := code{Label: label, Total: len(lines)}
	for i, l := range lines {
		cl := codeLine{Text: l}
		switch {
		case diff && strings.HasPrefix(l, "+"):
			cl.Class = "ln-add"
		case diff && strings.HasPrefix(l, "-"):
			cl.Class = "ln-del"
		case diff && strings.HasPrefix(l, "@@"):
			cl.Class = "ln-hunk"
		case !diff && bad && failureLine.MatchString(l):
			cl.Class = "ln-bad"
		}
		if i < n {
			c.Shown = append(c.Shown, cl)
		} else {
			c.More = append(c.More, cl)
		}
	}
	return c
}

// inputText is a call's input as one block of text: each field as "key:
// value", a single field as its value.
func inputText(fs []field) string {
	if len(fs) == 1 {
		return fs[0].Value
	}
	var b strings.Builder
	for _, f := range fs {
		if f.Block {
			b.WriteString(f.Key + ":\n" + f.Value + "\n")
		} else {
			b.WriteString(f.Key + ": " + f.Value + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
