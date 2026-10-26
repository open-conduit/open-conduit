package github

import (
	"regexp"
	"strings"
)

// Agent marks a comment written by an execution or the projector. Work and
// Intent come from Open Conduit markers; From is the issue reference carried by
// Open Supervisor's (and its predecessor's) markers, kept for fallback parity.
type Agent struct {
	Work   string `json:"work,omitempty"`
	Intent string `json:"intent,omitempty"`
	From   string `json:"from,omitempty"`
}

var (
	conduitMark = regexp.MustCompile(`<!--\s*open-conduit:agent((?:\s+[a-z]+=[^\s>]+)*)\s*-->`)
	markAttr    = regexp.MustCompile(`([a-z]+)=([^\s>]+)`)
	legacyMark  = regexp.MustCompile(`<!--\s*(?:open-supervisor:agent(?:\s+from=([^\s>]+))?|claude-agent(?:\s+from\s+#(\d+))?)\s*-->`)
	markValue   = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,200}$`)
)

// ParseAgent finds the marker an agent comment carries, if any.
func ParseAgent(body string) *Agent {
	if m := conduitMark.FindStringSubmatch(body); m != nil {
		a := &Agent{}
		for _, kv := range markAttr.FindAllStringSubmatch(m[1], -1) {
			switch kv[1] {
			case "work":
				a.Work = kv[2]
			case "intent":
				a.Intent = kv[2]
			}
		}
		return a
	}
	if m := legacyMark.FindStringSubmatch(body); m != nil {
		from := m[1]
		if from == "" {
			from = m[2]
		}
		return &Agent{From: from}
	}
	return nil
}

// LegacyMarker is Open Supervisor's own-note marker, appended after the Open
// Conduit marker for the fallback window (06-02): an Open Supervisor that
// takes a scope back reads Open Conduit's comments as agent notes rather
// than as human input. ParseAgent reads the Open Conduit marker first.
const LegacyMarker = "<!-- open-supervisor:agent -->"

// WithMarker appends the discoverable agent marker. work and intent must be
// secret-free identifiers; invalid values are omitted rather than escaped.
func WithMarker(body, work, intent string) string {
	marker := "<!-- open-conduit:agent"
	if markValue.MatchString(work) {
		marker += " work=" + work
	}
	if markValue.MatchString(intent) {
		marker += " intent=" + intent
	}
	return strings.TrimRight(body, "\n") + "\n\n" + marker + " -->\n" + LegacyMarker + "\n"
}

// hasIntent reports whether body carries the marker for intent.
func hasIntent(body, intent string) bool {
	a := ParseAgent(body)
	return a != nil && a.Intent == intent
}
