package setup

import (
	_ "embed"
	"strings"
)

//go:embed playbook/setup.md
var playbook string

// PlaybookName is the skill the setup playbook loads as, in the turn plugin
// and in the plugin `init --with claude` prepares.
const PlaybookName = "setup"

// Playbook is the setup playbook; bin is how the agent runs open-conduit.
// It covers every integration, so it lives here rather than with GitHub's.
func Playbook(bin string) string { return strings.ReplaceAll(playbook, "{{bin}}", bin) }

// Playbooks adds the setup playbook to the turn plugin, so a session's
// binding (`playbook: setup`) starts on it.
func (*Extension) Playbooks() map[string]string {
	return map[string]string{PlaybookName: Playbook("open-conduit")}
}
