// Package playbooks holds the instructions an agent follows under Open
// Conduit to work a Slack thread in turns. Everything that touches Slack
// goes through the slack.* toolbelt operations. The host adds them to the
// Claude Code plugin it prepares; a turn starts with /open-conduit:<playbook>.
package playbooks

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

// Thread is the playbook a Slack binding starts its turns on.
const Thread = "slack-thread"

// LegacyThread is the playbook's earlier name. It is shipped as an alias of
// Thread for one release, so bindings that still name it keep working.
const LegacyThread = "support-conversation"

// All returns every playbook by name ("slack-thread", and its alias
// "support-conversation").
func All() map[string]string {
	out := map[string]string{}
	entries, _ := files.ReadDir(".")
	for _, e := range entries {
		if data, err := files.ReadFile(e.Name()); err == nil {
			out[strings.TrimSuffix(e.Name(), ".md")] = string(data)
		}
	}
	if text, ok := out[Thread]; ok {
		out[LegacyThread] = strings.Replace(text, "name: "+Thread+"\n", "name: "+LegacyThread+"\n", 1)
	}
	return out
}
