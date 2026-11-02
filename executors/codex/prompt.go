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
	kindStart  turnKind = "start"
	kindEvents turnKind = "events"
)

// body composes the arguments and body of a turn, as Claude Code's do, so
// a playbook reads the same message whichever agent runs it.
func body(kind turnKind, args, events string) (string, string) {
	if kind == kindStart {
		if events == "" {
			return args, ""
		}
		return args, "## Already waiting\n\n" + events
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
// line, state, url, title, body or text).
func RenderEvents(events []domain.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		parts = append(parts, renderEvent(e))
	}
	return strings.Join(parts, "\n\n")
}

func renderEvent(e domain.Event) string {
	var m map[string]any
	_ = json.Unmarshal(e.Data.Data, &m)
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			if v != 0 {
				return fmt.Sprintf("%v", v)
			}
		}
		return ""
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
	if text == "" && m == nil && len(e.Data.Data) > 0 {
		text = truncate(string(e.Data.Data), 2000)
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
