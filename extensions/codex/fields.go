package codex

import "github.com/open-conduit/open-conduit/protocol"

// BindingFields describe a codex binding's config (Config) for the
// operator's catalogue (manifest fields).
func BindingFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "binary", Default: "codex"},
		{Name: "model", Help: "passed as --model; empty keeps Codex's own default"},
		{Name: "sandbox", Help: "Codex's own sandbox_mode for the commands the agent runs", Default: "danger-full-access", Options: []string{"read-only", "workspace-write", "danger-full-access"}},
		{Name: "playbook", Help: "the playbook a turn starts on; none (or -) starts it on the rendered Work alone"},
		{Name: "plugin_dir", Help: "holds the playbooks as <dir>/skills/<playbook>/SKILL.md; the host's own by default"},
		{Name: "args", Help: "the playbook's argument template", Default: "{number}"},
		{Name: "structured", Help: "ask for the turn status with --output-schema", Default: "true"},
		{Name: "branch"},
		{Name: "workspace_mode"},
		{Name: "dir", Help: "an absolute folder a workspace-free binding runs in"},
		{Name: "env"},
		{Name: "pass_env"},
		{Name: "secret_env"},
		{Name: "extra_args", Help: "added to every codex exec; may not set the session, output files, sandbox, approvals, model or toolbelt"},
		{Name: "timeout_seconds"},
		{Name: "handoff_minutes", Default: "55"},
		{Name: "cold_turns", Help: "what a turn does once the thread is handoff_minutes old", Default: "handoff", Options: []string{"handoff", "resume"}},
		{Name: "handoff_tokens", Help: "a turn after one that ended with more context starts afresh from the handoff note; 0 turns it off", Default: "200000"},
		{Name: "auto_compact_tokens", Help: "where Codex compacts mid-turn (model_auto_compact_token_limit); 0 leaves its own", Default: "250000"},
	}
}
