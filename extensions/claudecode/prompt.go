package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// PluginName is the plugin the playbooks are loaded as, and so the prefix of
// their skills: a turn starts with /open-conduit:<playbook> <args>.
const PluginName = "open-conduit"

// Renderer turns accepted Events into the markdown a turn reads. The default
// is RenderEvents; an extension that owns a payload schema may supply a
// richer one through Options.
type Renderer func([]domain.Event) string

var firstNumber = regexp.MustCompile(`\d+`)

// templateVars are the playbook argument/branch template values: {key} is the
// Work's first correlation key, {number} the first number in it (the key when
// it has none) and {work} the Work ID.
func expand(template string, w domain.Work) string {
	key := ""
	if len(w.CorrelationKeys) > 0 {
		key = w.CorrelationKeys[0].ExternalID
	}
	number := firstNumber.FindString(key)
	if number == "" {
		number = key
	}
	return strings.NewReplacer("{key}", key, "{number}", number, "{work}", string(w.ID)).Replace(template)
}

// turnKind is how a turn starts: a session's first, a handoff to a fresh
// session, or one more turn on events since the last.
type turnKind string

const (
	kindStart   turnKind = "start"
	kindEvents  turnKind = "events"
	kindHandoff turnKind = "handoff"
)

// body composes the args and body of a turn.
func body(kind turnKind, args, note string, events string) (string, string) {
	switch kind {
	case kindHandoff:
		return strings.TrimSpace(args + " handoff"), "## Handoff from your last session\n\n" + strings.TrimSpace(note) + "\n\n## Since your last turn\n\n" + events
	case kindStart:
		if events == "" {
			return args, ""
		}
		return args, "## Already waiting\n\n" + events
	}
	return strings.TrimSpace(args + " events"), "## Since your last turn\n\n" + events
}

// prompt is the first user message: the playbook skill with its arguments,
// then the body.
func prompt(playbook, args, body string) string {
	if playbook == "-" || playbook == "" {
		return strings.TrimSpace(args + "\n\n" + body)
	}
	text := "/" + PluginName + ":" + playbook
	if args != "" {
		text += " " + args
	}
	if body != "" {
		text += "\n\n" + body
	}
	return text
}

// dryRunPrompt wraps a turn's prompt for a dry run: the agent
// is told to do nothing and call no tools.
func dryRunPrompt(text string, structured bool) string {
	end := "End with a one-line summary."
	if structured {
		end = `End with state=review and summary="dry run".`
	}
	return "This is an Open Conduit dry run: do nothing and call no tools. The real prompt would have been:\n\n" + text + "\n\n" + end
}

// interjection is an Event handed to a turn that is already running.
func interjection(render Renderer, e domain.Event) string {
	return "## Arrived during this turn\n\nThis came in while you were working. Take it into account now, in this turn, as you would the events at the start of one.\n\n" + render([]domain.Event{e})
}

// RenderEvents is the generic renderer. It knows no service schema: it reads
// the common fields many payloads carry (author, kind, number, path, line,
// state, url, title, body or text) and otherwise shows the compact payload.
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

// AdditionsDir is where a project keeps additions to a playbook, in its
// checkout: <dir>/<playbook>.md.
const AdditionsDir = ".open-conduit/playbooks"

// projectAdditions reads a project's additions to a playbook from the turn's
// working directory, without any frontmatter; empty when it has none. They
// only ever add to the built-in playbook, never replace it.
func projectAdditions(root, playbook string) string {
	if playbook == "" || playbook == "-" || root == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, AdditionsDir, playbook+".md"))
	if err != nil {
		return ""
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
			text = text[4+i+5:]
		}
	}
	return strings.TrimSpace(text)
}

// withAdditions adds a project's additions to a turn's message.
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

// renderTurnLimit tells the agent its last turn was cut off at its time
// limit, so it can pick up where it left off.
func renderTurnLimit(e domain.Event) string {
	var p domain.ExecutionTimeout
	_ = json.Unmarshal(e.Data.Data, &p)
	limit := "its time limit"
	if p.TimeoutSeconds > 0 {
		limit += " of " + spell(time.Duration(p.TimeoutSeconds)*time.Second)
	}
	return "### Your last turn ended at its time limit\nYour last turn ran for " + limit + " and was stopped partway through, perhaps in the middle of a command. Nothing failed. See where it got to (the workspace, anything it had started) and carry on from there. A single step that runs long belongs in a task (`" + OpTaskStart + "`), which outlives the turn."
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
