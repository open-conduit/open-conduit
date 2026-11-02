package schedule

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
)

// renderFiles caps the changed files a rendered trigger lists, and
// renderBody the bytes of a webhook body it shows.
const (
	renderFiles = 20
	renderBody  = 2000
)

// RenderEvent renders a job trigger for an agent job's turn: which job, what
// started the run and, for a code-host delivery, the branch, commit and
// conclusion it names, so the agent need not read them from its context.
func RenderEvent(e domain.Event) (string, bool) {
	p, ok := Payload(e)
	if !ok || p.Job == "" {
		return "", false
	}
	head := fmt.Sprintf("### A run of the %s job", p.Job)
	if p.Trigger != "" {
		head += ": " + p.Trigger
	}
	lines := []string{head}
	add := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			lines = append(lines, label+": "+v)
		}
	}
	event := p.Event
	if event != "" && p.Action != "" {
		event += "." + p.Action
	}
	add("Event", event)
	if branch, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok {
		add("Branch", branch)
	} else {
		add("Ref", p.Ref)
	}
	add("Commit", p.SHA)
	add("Conclusion", p.Conclusion)
	if len(p.Files) > 0 {
		files := p.Files
		more := ""
		if len(files) > renderFiles {
			files, more = files[:renderFiles], fmt.Sprintf(" and %d more", len(p.Files)-renderFiles)
		}
		add("Files", strings.Join(files, ", ")+more)
	}
	out := strings.Join(lines, "\n")
	body := p.Text
	if body == "" && len(p.Body) > 0 {
		body = string(p.Body)
	}
	if body = strings.TrimSpace(body); body != "" {
		if len(body) > renderBody {
			n := renderBody
			for n > 0 && !utf8.RuneStart(body[n]) {
				n--
			}
			body = body[:n] + "…"
		}
		out += "\n\n" + body
	}
	return out, true
}
