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
// their skills: a turn starts with /open-conduit:work-issue 12.
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

// body composes the args and body of a turn (Open Supervisor's turnBody).
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

// dryRunPrompt wraps a turn's prompt for a dry run (v1 parity): the agent
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

// AdditionsDir is where a project keeps additions to a playbook, in its
// checkout: <dir>/<playbook>.md. Open Supervisor's .open-supervisor/playbooks
// is read when the project has no Open Conduit file (documented legacy
// fallback, so a project need not move its additions to switch).
const AdditionsDir = ".open-conduit/playbooks"

const legacyAdditionsDir = ".open-supervisor/playbooks"

// projectAdditions reads a project's additions to a playbook from the turn's
// working directory, without any frontmatter; empty when it has none. They
// only ever add to the built-in playbook, never replace it.
func projectAdditions(root, playbook string) string {
	if playbook == "" || playbook == "-" || root == "" {
		return ""
	}
	for _, dir := range []string{AdditionsDir, legacyAdditionsDir} {
		data, err := os.ReadFile(filepath.Join(root, dir, playbook+".md"))
		if err != nil {
			continue
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		if strings.HasPrefix(text, "---\n") {
			if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
				text = text[4+i+5:]
			}
		}
		return strings.TrimSpace(text)
	}
	return ""
}

// withAdditions adds a project's additions to a turn's message (v1 withExtra).
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
