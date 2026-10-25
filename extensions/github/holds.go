package github

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Wait conditions and hold markers, ported from Open Supervisor's
// core/wait.go. They are the agent-facing syntax only: the tools translate a
// condition into a generic dependency subscription (correlation key, event
// types and an opaque filter) at the extension boundary, so no condition
// kind ever enters core.

// CondKind names what a wait can be on.
type CondKind string

const (
	CondPR    CondKind = "pr"    // a PR merging or closing
	CondIssue CondKind = "issue" // an issue closing, or the agent on it ending a turn in a state
	CondJob   CondKind = "job"   // the next run of one of a scope's jobs ending (05-02)
)

// TurnStates are the states an agent ends a turn in (the claude status
// schema), which an issue condition can wait for.
var TurnStates = []string{"building", "review", "help", "done", "finished"}

// Cond is one thing a Work can wait on: `pr:1257:merged`, `issue:12:review`,
// `job:backup`, each optionally in another scope (`pr:club/88:merged`).
type Cond struct {
	Kind CondKind `json:"kind"`
	// Project names the scope the PR, issue or job belongs to (its scope ID or
	// name); empty is the waiting Work's own.
	Project string `json:"project,omitempty"`
	Ref     string `json:"ref"`
	// On is merged or closed for a PR; closed or a turn state for an issue;
	// empty for a job.
	On string `json:"on,omitempty"`
}

var issueOns = append([]string{"closed"}, TurnStates...)

// ParseCond reads a condition as an agent writes it. A PR's defaults to
// merged, an issue's to closed.
func ParseCond(s string) (Cond, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 || parts[1] == "" {
		return Cond{}, fmt.Errorf("%q isn't a condition: write pr:<n>[:merged|closed], issue:<n>[:closed|<state>] or job:<name>", s)
	}
	c := Cond{Kind: CondKind(strings.ToLower(parts[0])), Ref: parts[1]}
	if len(parts) == 3 {
		c.On = strings.ToLower(parts[2])
	}
	if i := strings.LastIndex(c.Ref, "/"); i >= 0 {
		c.Project, c.Ref = c.Ref[:i], c.Ref[i+1:]
		if c.Project == "" || c.Ref == "" {
			return Cond{}, fmt.Errorf("%q: write <project>/<ref> for another project", s)
		}
	}
	switch c.Kind {
	case CondPR:
		c.Ref = strings.TrimPrefix(c.Ref, "#")
		if n, err := strconv.Atoi(c.Ref); err != nil || n <= 0 {
			return Cond{}, fmt.Errorf("%q: %q isn't a PR number", s, c.Ref)
		}
		if c.On == "" {
			c.On = "merged"
		}
		if c.On != "merged" && c.On != "closed" {
			return Cond{}, fmt.Errorf("%q: a PR can be waited on to be merged or closed", s)
		}
	case CondIssue:
		c.Ref = strings.TrimPrefix(c.Ref, "#")
		if n, err := strconv.Atoi(c.Ref); err != nil || n <= 0 {
			return Cond{}, fmt.Errorf("%q: %q isn't an issue number", s, c.Ref)
		}
		if c.On == "" {
			c.On = "closed"
		}
		ok := false
		for _, o := range issueOns {
			ok = ok || c.On == o
		}
		if !ok {
			return Cond{}, fmt.Errorf("%q: an issue can be waited on to be %s", s, strings.Join(issueOns, ", "))
		}
	case CondJob:
		if c.On != "" {
			return Cond{}, fmt.Errorf("%q: a job is waited on for its next run to end; write job:<name>", s)
		}
	default:
		return Cond{}, fmt.Errorf("%q: waits are on a pr, an issue or a job", s)
	}
	return c, nil
}

// String is the condition as ParseCond reads it.
func (c Cond) String() string {
	ref := c.Ref
	if c.Project != "" {
		ref = c.Project + "/" + ref
	}
	if c.On == "" {
		return string(c.Kind) + ":" + ref
	}
	return string(c.Kind) + ":" + ref + ":" + c.On
}

// Number is the PR or issue number.
func (c Cond) Number() int { n, _ := strconv.Atoi(c.Ref); return n }

// Describe is what the condition waits for, in words.
func (c Cond) Describe() string {
	where := ""
	if c.Project != "" {
		where = " in " + c.Project
	}
	switch c.Kind {
	case CondPR:
		verb := "merge"
		if c.On == "closed" {
			verb = "close"
		}
		return fmt.Sprintf("PR #%s%s to %s", c.Ref, where, verb)
	case CondIssue:
		if c.On == "closed" {
			return fmt.Sprintf("#%s%s to close", c.Ref, where)
		}
		return fmt.Sprintf("the agent on #%s%s to end a turn in %s", c.Ref, where, c.On)
	case CondJob:
		return fmt.Sprintf("the next run of the %s job%s to end", c.Ref, where)
	}
	return c.String()
}

// Happened describes the fact that met the condition: the source Event type
// that the dependency alias carried (v1 Fact.Describe).
func (c Cond) Happened(sourceType string) string {
	where := ""
	if c.Project != "" {
		where = " in " + c.Project
	}
	switch c.Kind {
	case CondPR:
		if sourceType == TypePullClosed && c.On == "merged" {
			return fmt.Sprintf("PR #%s%s was closed without merging, so it won't merge", c.Ref, where)
		}
		if sourceType == TypePullMerged {
			return fmt.Sprintf("PR #%s%s was merged", c.Ref, where)
		}
		return fmt.Sprintf("PR #%s%s was closed", c.Ref, where)
	case CondIssue:
		if c.On == "closed" {
			return fmt.Sprintf("#%s%s was closed", c.Ref, where)
		}
		return fmt.Sprintf("the agent on #%s%s ended a turn in %s", c.Ref, where, c.On)
	case CondJob:
		return fmt.Sprintf("a run of the %s job%s %s", c.Ref, where, jobEnded(sourceType))
	}
	return c.Describe()
}

// Hold is what a Work is held for: any one of its conditions, or its time,
// whichever is first. Cancel drops the hold before it.
type Hold struct {
	Conds  []Cond     `json:"conds,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
	Cancel bool       `json:"cancel,omitempty"`
}

// Spec is the hold in the words a marker carries: `runAt: 2026-10-25T09:00Z`
// for a time alone, else `waitFor: pr:1257:merged issue:12:review; until …`,
// or `waitFor: cancel`. Open Supervisor writes and reads the same markers.
func (h Hold) Spec() string {
	if h.Cancel {
		return "waitFor: cancel"
	}
	if len(h.Conds) == 0 && h.Until != nil {
		return "runAt: " + h.Until.UTC().Format("2006-01-02T15:04Z")
	}
	var conds []string
	for _, c := range h.Conds {
		conds = append(conds, c.String())
	}
	s := "waitFor: " + strings.Join(conds, " ")
	if h.Until != nil {
		s += "; until " + h.Until.UTC().Format("2006-01-02T15:04Z")
	}
	return s
}

// Marker is the hidden comment carrying the hold.
func (h Hold) Marker() string { return "<!-- " + h.Spec() + " -->" }

// Describe is the hold in words: "PR #1257 to merge, or 25 Oct 09:00 UTC".
func (h Hold) Describe() string {
	if h.Cancel {
		return "nothing (cancelled)"
	}
	var parts []string
	for _, c := range h.Conds {
		parts = append(parts, c.Describe())
	}
	if h.Until != nil {
		parts = append(parts, h.Until.UTC().Format("2 Jan 2006 15:04 UTC"))
	}
	return strings.Join(parts, ", or ")
}

// CondsText lists conditions in words.
func CondsText(cs []Cond) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, c.Describe())
	}
	return strings.Join(parts, ", or ")
}

var holdRe = regexp.MustCompile(`(?:<!--\s*(runAt|waitFor)[:(\s]\s*([^>]*?)\s*\)?\s*-->|\[(runAt|waitFor):\s*([^\]]*?)\s*\])`)

// HoldIn returns the last valid hold marker in a body.
func HoldIn(body string) (Hold, bool) {
	var found Hold
	ok := false
	for _, m := range holdRe.FindAllStringSubmatch(body, -1) {
		kind, v := m[1], m[2]
		if kind == "" {
			kind, v = m[3], m[4]
		}
		if h, err := parseHold(kind, v); err == nil {
			found, ok = h, true
		}
	}
	return found, ok
}

func parseHold(kind, v string) (Hold, error) {
	v = strings.TrimSpace(v)
	if kind == "runAt" {
		t, err := ParseTime(v)
		if err != nil {
			return Hold{}, err
		}
		return Hold{Until: &t}, nil
	}
	if strings.EqualFold(v, "cancel") {
		return Hold{Cancel: true}, nil
	}
	var h Hold
	conds, until, _ := strings.Cut(v, ";")
	for _, f := range strings.Fields(conds) {
		c, err := ParseCond(f)
		if err != nil {
			return Hold{}, err
		}
		h.Conds = append(h.Conds, c)
	}
	if until = strings.TrimSpace(until); until != "" {
		t, err := ParseTime(strings.TrimSpace(strings.TrimPrefix(until, "until")))
		if err != nil {
			return Hold{}, err
		}
		h.Until = &t
	}
	if len(h.Conds) == 0 && h.Until == nil {
		return Hold{}, fmt.Errorf("an empty wait")
	}
	return h, nil
}

// ParseTime reads an ISO instant with Z or an offset, with or without
// seconds; a bare local-less layout is read as UTC (v1 core.ParseTime).
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z", "2006-01-02T15:04:05Z", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q isn't an ISO time", s)
}

// jobEnded is how a job's run ended, from the run-ended Event type the
// schedule extension announces (schedule.run.<outcome>); GitHub does not
// import the schedule extension, so it reads the type's last segment.
func jobEnded(sourceType string) string {
	switch sourceType[strings.LastIndexByte(sourceType, '.')+1:] {
	case "completed":
		return "ended: completed"
	case "failed":
		return "ended: failed"
	case "cancelled":
		return "was cancelled"
	case "handoff":
		return "ended: handed off"
	}
	return "ended"
}
