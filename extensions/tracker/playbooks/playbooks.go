// Package playbooks holds the instructions a coding agent follows under Open
// Conduit for an issue in a tracker other than GitHub (Linear, Jira), with
// pull requests on whichever code host the toolbelt lists. Everything that touches
// the tracker or the code host goes through the toolbelt. The host adds the
// playbooks to the Claude Code plugin it prepares; a turn starts with
// /open-conduit:<playbook>.
package playbooks

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

// All returns every playbook by name ("work-tracker-issue").
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
