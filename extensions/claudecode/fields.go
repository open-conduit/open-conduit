package claudecode

import "github.com/open-conduit/open-conduit/protocol"

// bindingFields describe a claude-code binding's config for the operator's
// catalogue (manifest fields).
func bindingFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "binary", Default: "claude"},
		{Name: "model"},
		{Name: "permission_mode", Default: "bypassPermissions"},
		{Name: "playbook", Help: "the playbook skill turns start on, as the extension's setup writes it; none (or -) starts them on the rendered Work alone"},
		{Name: "plugin_dir"},
		{Name: "args", Help: "a template of {key}, {number} and {work}", Default: "{number}"},
		{Name: "status_schema", Default: "core.turn.v1"},
		{Name: "branch", Help: "the workspace branch template, of {key}, {number} and {work}"},
		{Name: "workspace_mode"},
		{Name: "dir", Help: "a folder a workspace-free binding runs in, as-is"},
		{Name: "handoff_minutes"},
		{Name: "cold_turns", Help: "what a turn does once the session is handoff_minutes old", Default: "handoff", Options: []string{"handoff", "resume"}},
		{Name: "handoff_tokens", Help: "a turn after one that ended with more context starts afresh from the handoff note; 0 turns it off", Default: "400000"},
		{Name: "auto_compact_tokens", Help: "where Claude Code compacts mid-turn (CLAUDE_CODE_AUTO_COMPACT_WINDOW); 0 leaves its own", Default: "600000"},
		{Name: "env"},
		{Name: "pass_env"},
		{Name: "secret_env"},
		{Name: "extra_args"},
		{Name: "builtin_tools", Help: "the only built-in tools offered; [] turns them all off"},
		{Name: "models", Help: "models a Work may pick with model in its context"},
		{Name: "usage", Help: "doctor reads the subscription usage windows of this binding's Claude login"},
	}
}
