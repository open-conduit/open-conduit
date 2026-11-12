// Package playbooks holds the instructions agents follow under Open Conduit:
// how to work a GitHub issue in turns. Everything that touches GitHub goes
// through the toolbelt operations the github extension registers. The
// extension offers them (Extension.Playbooks), and the host writes them
// into the plugin of skills Claude Code loads; a turn starts with
// /open-conduit:<playbook>.
package playbooks

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

// All returns every agent playbook by name ("work-issue").
func All() map[string]string {
	out := map[string]string{}
	entries, _ := files.ReadDir(".")
	for _, e := range entries {
		data, err := files.ReadFile(e.Name())
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".md")] = string(data)
	}
	return out
}

// Body is a playbook without its frontmatter.
func Body(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return text
	}
	if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
		return strings.TrimLeft(text[4+i+5:], "\n")
	}
	return text
}

// Description is a playbook's frontmatter description.
func Description(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return ""
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return ""
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if v, ok := strings.CutPrefix(line, "description:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
