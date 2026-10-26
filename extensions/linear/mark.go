package linear

import (
	"regexp"
	"strings"

	"github.com/alsiola/open-conduit/extensions/tracker"
)

// Linear keeps Markdown but drops HTML comments, so an agent's comment is
// marked by lines people can read: Open Supervisor's footer (which it still
// recognizes, so both runtimes see each other's notes during the fallback
// release) and Open Conduit's line naming the Work and the intent, by which
// an ambiguous post is found again.
var (
	legacyMark  = regexp.MustCompile(`(?m)^[\W_]*Posted by an open-supervisor agent(?: working on ([A-Za-z][A-Za-z0-9_]*-\d+))?[\W_]*$`)
	conduitMark = regexp.MustCompile(`(?m)^[\W_]*open-conduit((?: [a-z]+=[A-Za-z0-9_.:/@-]+)+)[\W_]*$`)
	markAttr    = regexp.MustCompile(`([a-z]+)=([A-Za-z0-9_.:/@-]+)`)
	markValue   = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,200}$`)
)

// WithMark appends the footer that marks a comment as an agent's.
func WithMark(body string, m tracker.Mark) string {
	footer := "*Posted by an open-supervisor agent"
	if markValue.MatchString(m.From) && tracker.ValidKey(m.From) {
		footer += " working on " + m.From
	}
	footer += "*"
	line := "open-conduit"
	if markValue.MatchString(m.Work) {
		line += " work=" + m.Work
	}
	if markValue.MatchString(m.Intent) {
		line += " intent=" + m.Intent
	}
	out := strings.TrimRight(body, "\n") + "\n\n" + footer
	if line != "open-conduit" {
		out += "\n*" + line + "*"
	}
	return out + "\n"
}

// ParseMark finds the agent mark a comment carries, if any.
func ParseMark(body string) *tracker.Agent {
	var a *tracker.Agent
	if m := legacyMark.FindStringSubmatch(body); m != nil {
		a = &tracker.Agent{From: strings.ToUpper(m[1])}
	}
	if m := conduitMark.FindStringSubmatch(body); m != nil {
		if a == nil {
			a = &tracker.Agent{}
		}
		for _, kv := range markAttr.FindAllStringSubmatch(m[1], -1) {
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
