package codex

import (
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// InteractiveLaunch is the command that starts the Codex CLI on a playbook
// in the operator's terminal. Codex loads no plugin skills and has no list
// of commands to allow ahead of time: the playbook, without its skill
// frontmatter, is the session's first message, and Codex's own
// workspace-write sandbox asks the operator before any command that writes
// outside the working directory. The request's allowed commands therefore
// still ask, which is the safe side of what the host allowed.
func InteractiveLaunch(r extension.InteractiveLaunchRequest) extension.InteractiveLaunchSpec {
	program := r.Program
	if program == "" {
		program = "codex"
	}
	return extension.InteractiveLaunchSpec{Argv: []string{program, "--sandbox", "workspace-write", "--ask-for-approval", "on-request", PlaybookPrompt(r.Playbook)}}
}

// PlaybookPrompt is a playbook as a first message: its text without the
// skill frontmatter.
func PlaybookPrompt(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
			text = text[4+i+5:]
		}
	}
	return strings.TrimSpace(text)
}
