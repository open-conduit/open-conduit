package linear

import (
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// Linear keeps Markdown but drops HTML comments, so an agent's comment is
// marked by a line people can read, naming the Work and the intent, by which
// an ambiguous post is found again.
var (
	conduitMark = regexp.MustCompile(`(?m)^[\W_]*open-conduit((?: [a-z]+=[A-Za-z0-9_.:/@-]+)+)[\W_]*$`)
	markAttr    = regexp.MustCompile(`([a-z]+)=([A-Za-z0-9_.:/@-]+)`)
	markValue   = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,200}$`)
)

// WithMark appends the line that marks a comment as an agent's. A mark
// with neither a valid Work nor intent leaves the body unmarked.
func WithMark(body string, m tracker.Mark) string {
	line := "open-conduit"
	if markValue.MatchString(m.Work) {
		line += " work=" + m.Work
	}
	if markValue.MatchString(m.Intent) {
		line += " intent=" + m.Intent
	}
	out := strings.TrimRight(body, "\n")
	if line != "open-conduit" {
		out += "\n\n*" + line + "*"
	}
	return out + "\n"
}

// ParseMark finds the agent mark a comment carries, if any.
func ParseMark(body string) *tracker.Agent {
	var a *tracker.Agent
	if m := conduitMark.FindStringSubmatch(body); m != nil {
		a = &tracker.Agent{}
		attrs := m[1]
		if strings.HasPrefix(strings.TrimSpace(m[0]), "_") {
			// Linear may rewrite *emphasis* as _emphasis_, whose closing
			// underscore the last value would otherwise take.
			attrs = strings.TrimSuffix(attrs, "_")
		}
		for _, kv := range markAttr.FindAllStringSubmatch(attrs, -1) {
			switch kv[1] {
			case "work":
				a.Work = kv[2]
			case "intent":
				a.Intent = kv[2]
			}
		}
	}
	return a
}
