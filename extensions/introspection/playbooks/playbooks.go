// Package playbooks holds the instructions agents follow for introspection
// under Open Conduit: the daily sweep and triage (each scope's standing Work,
// or run by hand) and a requested fix (its own Work). Everything that touches
// reports, notes and fixes goes through the introspection.* toolbelt
// operations.
// The host adds them to the Claude Code plugin it prepares; a turn starts with
// /open-conduit:<playbook>.
package playbooks

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

// All returns every playbook by name ("sweep", "triage-reports",
// "fix-report").
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
