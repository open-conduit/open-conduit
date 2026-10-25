// Package playbooks holds the instructions a support agent follows under
// Open Conduit: how to answer a Slack thread in turns. Everything that
// touches Slack goes through the slack.* toolbelt operations. The host adds
// them to the Claude Code plugin it prepares; a turn starts with
// /open-conduit:<playbook>.
package playbooks

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

// All returns every playbook by name ("support-conversation").
func All() map[string]string {
	out := map[string]string{}
	entries, _ := files.ReadDir(".")
	for _, e := range entries {
		if data, err := files.ReadFile(e.Name()); err == nil {
			out[strings.TrimSuffix(e.Name(), ".md")] = string(data)
		}
	}
	return out
}
