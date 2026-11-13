package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// turnKind is how a turn starts: a Work's first, or one more on the
// events since the last.
type turnKind string

const (
	kindStart   turnKind = "start"
	kindEvents  turnKind = "events"
	kindHandoff turnKind = "handoff"
)

// Why a handoff turn left its thread: the last turn ended with more than
// handoff_tokens of context, or more than handoff_minutes ago.
const (
	handoffSize = "size"
	handoffCold = "cold"
)

// body composes the arguments and body of a turn, as Claude Code's do, so
// a playbook reads the same message whichever agent runs it. A handoff
// turn starts a new thread on the last turn's note.
func body(kind turnKind, args, note, events string) (string, string) {
	switch kind {
	case kindStart:
		if events == "" {
			return args, ""
		}
		return args, "## Already waiting\n\n" + events
	case kindHandoff:
		return strings.TrimSpace(args + " handoff"), "## Handoff from your last session\n\n" + strings.TrimSpace(note) + "\n\n## Since your last turn\n\n" + events
	}
	return strings.TrimSpace(args + " events"), "## Since your last turn\n\n" + events
}

// prompt is the turn's message. Codex has no plugin skills to start a
// turn on, so the playbook's own text comes first, then its arguments,
// exactly where a skill invocation would put them, then the body.
func prompt(playbookText, args, body string) string {
	if playbookText == "" {
		return strings.TrimSpace(args + "\n\n" + body)
	}
	text := playbookText + "\n\nARGUMENTS: " + args
	if body != "" {
		text += "\n\n" + body
	}
	return text
}

// readPlaybook reads <pluginDir>/skills/<playbook>/SKILL.md without its
// frontmatter. Empty for no playbook.
func readPlaybook(pluginDir, playbook string) (string, error) {
	if playbook == "" || playbook == "-" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(pluginDir, "skills", playbook, "SKILL.md"))
	if err != nil {
		return "", err
	}
	return stripFrontmatter(string(data)), nil
}

func stripFrontmatter(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
			text = text[4+i+5:]
		}
	}
	return strings.TrimSpace(text)
}

// AdditionsDir is where a project keeps additions to a playbook in its
// checkout, <dir>/<playbook>.md, as for Claude Code.
const AdditionsDir = ".open-conduit/playbooks"

// projectAdditions reads a project's additions to a playbook from the
// turn's working directory; empty when it has none.
func projectAdditions(root, playbook string) string {
	if playbook == "" || playbook == "-" || root == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, AdditionsDir, playbook+".md"))
	if err != nil {
		return ""
	}
	return stripFrontmatter(string(data))
}

// withAdditions adds a project's additions to a turn's body.
func withAdditions(body, extra string) string {
	if extra == "" {
		return body
	}
	add := "## This project's additions to the playbook\n\nFollow these as well as the playbook. They add to its steps and never replace them.\n\n" + extra
	if body == "" {
		return add
	}
	return body + "\n\n" + add
}

// interjection is the follow-up turn's message for Events injected while
// a turn ran.
func interjection(events []domain.Event) string {
	return "## Arrived during this turn\n\nThis came in while you were working. Take it into account now, as you would the events at the start of a turn, then end the turn again with your status.\n\n" + RenderEvents(events)
}

// RenderEvents is the generic renderer: it knows no service schema and
// reads the common fields many payloads carry (author, kind, number, path,
// line, state, url, title, body or text) and otherwise shows the rest of
// the payload as compact JSON.
func RenderEvents(events []domain.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		parts = append(parts, renderEvent(e))
	}
	return strings.Join(parts, "\n\n")
}

func renderEvent(e domain.Event) string {
	if e.Type == domain.TypeExecutionTimeout {
		return renderTurnLimit(e)
	}
	var m map[string]any
	_ = json.Unmarshal(e.Data.Data, &m)
	shown := map[string]bool{}
	str := func(k string) string {
		v := ""
		switch x := m[k].(type) {
		case string:
			v = strings.TrimSpace(x)
		case float64:
			if x != 0 {
				v = fmt.Sprintf("%v", x)
			}
		}
		if v != "" {
			shown[k] = true
		}
		return v
	}
	head := "### " + e.Type
	if kind := str("kind"); kind != "" {
		head += " (" + kind + ")"
	}
	if n := str("number"); n != "" {
		head += " #" + n
	}
	if path := str("path"); path != "" {
		head += " at " + path
		if line := str("line"); line != "" {
			head += ":" + line
		}
	}
	if author := str("author"); author != "" {
		head += " by " + author
	}
	at := e.OccurredAt
	if at.IsZero() {
		at = e.ReceivedAt
	}
	if !at.IsZero() {
		head += " (" + at.UTC().Format(time.RFC3339) + ")"
	}
	lines := []string{head}
	if state := str("state"); state != "" {
		lines = append(lines, "State: "+state)
	}
	if title := str("title"); title != "" {
		lines = append(lines, "Title: "+title)
	}
	if url := str("url"); url != "" {
		lines = append(lines, url)
	}
	text := str("body")
	if text == "" {
		text = str("text")
	}
	if text == "" {
		text = unshown(e.Data.Data, shown)
	}
	out := strings.Join(lines, "\n")
	if text != "" {
		out += "\n\n" + text
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// renderTurnLimit tells the agent its last turn was cut off at its time
// limit, so it can pick up where it left off.
func renderTurnLimit(e domain.Event) string {
	var p domain.ExecutionTimeout
	_ = json.Unmarshal(e.Data.Data, &p)
	limit := "its time limit"
	if p.TimeoutSeconds > 0 {
		limit += " of " + spell(time.Duration(p.TimeoutSeconds)*time.Second)
	}
	return "### Your last turn ended at its time limit\nYour last turn ran for " + limit + " and was stopped partway through, perhaps in the middle of a command. Nothing failed. See where it got to (the workspace, anything it had started) and carry on from there."
}

// spell is a time limit in words: "1 hour", "90 minutes", "45s".
func spell(d time.Duration) string {
	unit := func(n int64, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	switch {
	case d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return d.String()
}

// payloadClip is the most of a payload's remaining JSON a turn shows.
const payloadClip = 2000

// unshown is the rest of a payload without a body or text, beyond the fields
// renderEvent has already worded (shown), as compact JSON with each value's
// exact bytes, so an Event whose schema no renderer knows still reaches the
// turn with its data (#574). A common field the heading couldn't word (an
// object author, a line without a path) stays in. A payload that isn't an
// object is shown as it is. Either is clipped, and says so. It is empty when
// nothing is left.
func unshown(raw []byte, shown map[string]bool) string {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return ""
	}
	text := string(raw)
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		for k := range shown {
			delete(fields, k)
		}
		if len(fields) == 0 {
			return ""
		}
		var rest strings.Builder
		enc := json.NewEncoder(&rest)
		enc.SetEscapeHTML(false)
		if enc.Encode(fields) != nil {
			return ""
		}
		text = strings.TrimSpace(rest.String())
	}
	if len(text) <= payloadClip {
		return text
	}
	return truncate(text, payloadClip) + fmt.Sprintf("\n\n(clipped: the payload has %d bytes)", len(text))
}
