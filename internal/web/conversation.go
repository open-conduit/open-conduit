package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TranscriptTail bounds how much of one turn's transcript the conversation
// view reads: enough for most turns whole, the end of the longest.
const TranscriptTail = 2 << 20

// TranscriptHead is how much of the start of an ended turn's transcript is
// read besides its tail when the whole is longer.
const TranscriptHead = 256 << 10

// Clips keep one long message or tool result from swamping the page; the
// raw transcript keeps everything.
const (
	clipResult  = 6000
	clipMessage = 1500
	clipLine    = 160
)

// A conversation is a turn's transcript as a chat: what the agent was told,
// what it said and the tools it called with what they returned. It reads
// any transcript of JSON lines in the shape of the Messages API (a line per
// message, with a type of user or assistant and the message's content
// blocks), so it is keyed on the format, not on the executor that wrote it.
type conversation struct {
	Entries []entry
	// Trimmed is how many bytes of the transcript's start are not shown.
	Trimmed int64
}

// entry is one item in the chat. Kind is user, assistant, thinking, tool or
// note (a line from the session itself, such as its start or its end).
type entry struct {
	Kind string
	Text string
	// Full is the whole of a clipped message, shown on request.
	Full string
	At   string
	Tool *toolCall
}

// toolCall is a tool the agent called and what it returned. Steps are what
// a tool that runs an agent of its own did inside it.
type toolCall struct {
	ID, Name, Server string
	Headline, Note   string
	Input            []field
	Result           string
	Clipped          int64
	Error, Done      bool
	// Lost marks a call in a turn that has ended with no result in what
	// was read of its transcript.
	Lost  bool
	Steps []entry
}

type field struct {
	Key, Value string
	Block      bool
}

// The headline is the input field that says most about the call, so the
// call reads at a glance without opening it.
var headlineKeys = []string{"command", "file_path", "path", "notebook_path", "pattern", "url", "query", "operation", "skill", "subject", "description", "prompt"}

type line struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Parent  string `json:"parent_tool_use_id"`
	At      string `json:"timestamp"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsError    bool     `json:"is_error"`
	NumTurns   int      `json:"num_turns"`
	DurationMS int64    `json:"duration_ms"`
	Cost       *float64 `json:"total_cost_usd"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseConversation reads a transcript: its head, then, when skipped bytes
// of its middle were not read, its tail. A part cut from a transcript starts
// or ends part-way through a line, which is skipped like any line it cannot
// read.
func parseConversation(head string, skipped int64, tail string) conversation {
	var c conversation
	calls := map[string]*toolCall{}
	// add appends to the conversation, or inside the tool call a step
	// belongs to.
	add := func(parent string, e entry) {
		list := &c.Entries
		if t := calls[parent]; parent != "" && t != nil {
			list = &t.Steps
		}
		if n := len(*list); n > 0 && e.Kind == "assistant" && (*list)[n-1].Kind == "assistant" {
			// One reply split over several messages reads as one.
			prev := &(*list)[n-1]
			prev.Text = strings.TrimSpace(prev.Text + "\n\n" + e.Text)
			return
		}
		*list = append(*list, e)
	}
	text := head
	if skipped > 0 {
		text += "\n" + gapLine + "\n" + tail
	}
	for _, raw := range strings.Split(text, "\n") {
		if raw == gapLine {
			c.Entries = append(c.Entries, entry{Kind: "note", Text: size(skipped) + " of the transcript are not shown here"})
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "" || raw[0] != '{' {
			continue
		}
		var l line
		if json.Unmarshal([]byte(raw), &l) != nil {
			continue
		}
		switch l.Type {
		case "system":
			if l.Subtype == "init" {
				note := "Session started"
				if l.Model != "" {
					note += " on " + l.Model
				}
				add(l.Parent, entry{Kind: "note", Text: note})
			}
		case "result":
			add(l.Parent, entry{Kind: "note", Text: ending(l)})
		case "user", "assistant":
			var blocks []block
			if json.Unmarshal(l.Message.Content, &blocks) != nil {
				var s string
				if json.Unmarshal(l.Message.Content, &s) == nil && strings.TrimSpace(s) != "" {
					add(l.Parent, message(l.Type, s, l.At))
				}
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
						add(l.Parent, message(l.Type, b.Text, l.At))
					}
				case "thinking":
					if strings.TrimSpace(b.Thinking) != "" {
						add(l.Parent, entry{Kind: "thinking", Text: strings.TrimSpace(b.Thinking), At: l.At})
					}
				case "tool_use":
					t := toolUse(b)
					calls[b.ID] = t
					add(l.Parent, entry{Kind: "tool", Tool: t, At: l.At})
				case "tool_result":
					t := calls[b.ToolUseID]
					if t == nil {
						// Its call is in the part of the transcript not read.
						t = &toolCall{ID: b.ToolUseID, Name: "an earlier call"}
						add(l.Parent, entry{Kind: "tool", Tool: t, At: l.At})
					}
					t.Done, t.Error = true, b.IsError
					t.Result, t.Clipped = clip(resultText(b.Content), clipResult)
				}
			}
		}
	}
	return c
}

// gapLine stands where the unread middle of a transcript was. It cannot be
// a transcript's line, which is JSON.
const gapLine = "\x00gap"

// settle marks the calls of an ended turn that never got a result.
func settle(entries []entry) {
	for _, e := range entries {
		if t := e.Tool; t != nil {
			if !t.Done {
				t.Done, t.Lost = true, true
			}
			settle(t.Steps)
		}
	}
}

func message(kind, text, at string) entry {
	text = strings.TrimSpace(text)
	if kind == "user" {
		if note, ok := taskNote(text); ok {
			return entry{Kind: "note", Text: note, At: at}
		}
		text = command(text)
	}
	e := entry{Kind: kind, Text: text, At: at}
	if kind == "user" && len(text) > clipMessage {
		// What the agent was told is often a whole playbook: its start
		// says what it is.
		e.Text, _ = clip(text, clipMessage)
		e.Full = text
	}
	return e
}

func ending(l line) string {
	s := "Turn ended"
	if l.Subtype != "" && l.Subtype != "success" {
		s += " (" + strings.ReplaceAll(l.Subtype, "_", " ") + ")"
	}
	if l.IsError {
		s += " with an error"
	}
	var parts []string
	if l.NumTurns > 0 {
		parts = append(parts, plural(l.NumTurns, "model call"))
	}
	if l.DurationMS > 0 {
		parts = append(parts, (time.Duration(l.DurationMS) * time.Millisecond).Round(time.Second).String())
	}
	if l.Cost != nil {
		parts = append(parts, fmt.Sprintf("$%.2f session cost", *l.Cost))
	}
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, ", ")
	}
	return s
}

// toolUse reads a tool_use block. A tool named mcp__<server>__<tool> is an MCP
// server's tool and reads as the tool, from the server.
func toolUse(b block) *toolCall {
	t := &toolCall{ID: b.ID, Name: b.Name}
	if rest, ok := strings.CutPrefix(b.Name, "mcp__"); ok {
		if server, tool, ok := strings.Cut(rest, "__"); ok {
			t.Server, t.Name = server, tool
		}
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(b.Input, &in) != nil {
		if s := strings.TrimSpace(string(b.Input)); s != "" && s != "null" {
			t.Input = []field{{Key: "input", Value: s, Block: true}}
		}
		return t
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := map[string]string{}
	for _, k := range keys {
		v := value(in[k])
		values[k] = v
		t.Input = append(t.Input, field{Key: k, Value: v, Block: strings.Contains(v, "\n") || len(v) > 80})
	}
	for _, k := range headlineKeys {
		if v := values[k]; v != "" {
			t.Headline, _ = clip(oneLine(v), clipLine)
			if k != "description" && values["description"] != "" {
				t.Note, _ = clip(oneLine(values["description"]), clipLine)
			}
			break
		}
	}
	return t
}

// value is a field's value as text: a string as itself, anything else as
// indented JSON.
func value(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") == nil {
		return b.String()
	}
	return string(raw)
}

// resultText is a tool result's text: a string, or its text blocks with
// any other block named by its type.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return strings.TrimSpace(string(raw))
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else if b.Type != "" {
			parts = append(parts, "["+strings.ReplaceAll(b.Type, "_", " ")+"]")
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// clip cuts s to about n bytes on a rune boundary and reports how many
// bytes it left out.
func clip(s string, n int) (string, int64) {
	if len(s) <= n {
		return s, 0
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \n") + " …", int64(len(s) - cut)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

var (
	fence  = regexp.MustCompile("^\\s*```")
	bullet = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	inline = regexp.MustCompile("`[^`\n]+`|\\*\\*[^*\n]+\\*\\*|\\*[^*\\s][^*\n]*\\*|\\[[^\\]\n]+\\]\\(https?://[^)\\s]+\\)")
	// heading is a Markdown heading: hashes then a space, so "#142 fixes
	// this" stays a line of text.
	heading = regexp.MustCompile(`^\s*#{1,6}\s`)
)

// markdown renders the Markdown agents write as chat text: paragraphs,
// headings, lists, fenced code, inline code, bold and links. Everything is
// escaped first, so it never passes markup through.
func markdown(s string) template.HTML {
	var b strings.Builder
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var para, items []string
	list := ""
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = nil
		}
		if list != "" {
			b.WriteString("<" + list + "><li>" + strings.Join(items, "</li><li>") + "</li></" + list + ">")
			list, items = "", nil
		}
	}
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		switch {
		case fence.MatchString(ln):
			flush()
			var code []string
			for i++; i < len(lines) && !fence.MatchString(lines[i]); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
		case strings.TrimSpace(ln) == "":
			flush()
		case heading.MatchString(ln):
			flush()
			b.WriteString("<p class=\"h\">" + spans(strings.TrimLeft(strings.TrimSpace(ln), "# ")) + "</p>")
		case strings.HasPrefix(strings.TrimSpace(ln), "|"):
			flush()
			var rows []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				rows = append(rows, lines[i])
			}
			i--
			b.WriteString("<pre class=\"table\">" + html.EscapeString(strings.Join(rows, "\n")) + "</pre>")
		case bullet.MatchString(ln):
			kind := "ul"
			if m := strings.TrimSpace(bullet.FindString(ln)); m[0] >= '0' && m[0] <= '9' {
				kind = "ol"
			}
			if len(para) > 0 || list != kind {
				flush()
				list = kind
			}
			items = append(items, spans(bullet.ReplaceAllString(ln, "")))
		case list != "" && strings.HasPrefix(ln, "  "):
			// A list item's continuation line.
			items[len(items)-1] += "<br>" + spans(strings.TrimSpace(ln))
		default:
			if list != "" {
				flush()
			}
			para = append(para, spans(ln))
		}
	}
	flush()
	return template.HTML(b.String())
}

// spans escapes a line and marks up its inline code, bold and links.
func spans(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range inline.FindAllStringIndex(s, -1) {
		b.WriteString(html.EscapeString(s[last:m[0]]))
		tok := s[m[0]:m[1]]
		switch {
		case tok[0] == '`':
			b.WriteString("<code>" + html.EscapeString(tok[1:len(tok)-1]) + "</code>")
		case strings.HasPrefix(tok, "**"):
			b.WriteString("<strong>" + html.EscapeString(tok[2:len(tok)-2]) + "</strong>")
		case tok[0] == '*':
			// Chat apps write *bold* with one asterisk.
			b.WriteString("<strong>" + html.EscapeString(tok[1:len(tok)-1]) + "</strong>")
		default:
			text, href, _ := strings.Cut(tok[1:len(tok)-1], "](")
			b.WriteString("<a href=\"" + html.EscapeString(href) + "\" rel=\"noreferrer\">" + html.EscapeString(text) + "</a>")
		}
		last = m[1]
	}
	b.WriteString(html.EscapeString(s[last:]))
	return b.String()
}

// clock is a transcript timestamp as a local time of day.
func clock(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04:05")
}

// size is a byte count for people.
func size(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return plural(int(n), "byte")
}

// Claude Code writes some of its own notes into a session as user
// messages, in tags: a background command finishing, and the slash command
// a turn was started with.
// tags reads a message made only of tags (and space between them) into
// their contents by name, or reports that it is not one.
func tags(text string) (map[string]string, bool) {
	out := map[string]string{}
	rest := strings.TrimSpace(text)
	for rest != "" {
		if rest[0] != '<' {
			return out, false
		}
		end := strings.IndexByte(rest, '>')
		if end < 2 {
			return out, false
		}
		name := rest[1:end]
		body, after, ok := strings.Cut(rest[end+1:], "</"+name+">")
		if !ok || strings.ContainsAny(name, " /") {
			return out, false
		}
		out[name] = strings.TrimSpace(body)
		rest = strings.TrimSpace(after)
	}
	return out, len(out) > 0
}

// taskNote is a background command's notification as a note: what it was
// and how it ended.
func taskNote(text string) (string, bool) {
	t, ok := tags(text)
	body, isTask := t["task-notification"]
	if !ok || !isTask {
		return "", false
	}
	inner, _ := tags(body)
	note := "Background task"
	if s := inner["status"]; s != "" {
		note += " " + s
	}
	if s := inner["summary"]; s != "" {
		note += ": " + s
	}
	return note, true
}

// command is a slash command's message as the command and its arguments.
func command(text string) string {
	t, ok := tags(text)
	name := t["command-name"]
	if !ok || name == "" {
		return text
	}
	if args := t["command-args"]; args != "" {
		name += "\n\n" + args
	}
	return name
}
