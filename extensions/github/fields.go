package github

import "github.com/open-conduit/open-conduit/protocol"

// connectionFields describe the github connection's config and
// secret_refs for the operator's catalogue (manifest fields).
func connectionFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "api_url", Help: "or an Enterprise /api/v3", Default: "https://api.github.com"},
		{Name: "auth", Default: "token", Options: []string{"token", "app"}},
		{Name: "app_id", Help: "for auth app"},
		{Name: "bot_app_id", Help: "an App that performs writes"},
		{Name: SecretToken, Help: "token auth (cmd:gh auth token for the gh login)", Secret: true},
		{Name: SecretAppPrivateKey, Help: "app auth", Secret: true},
		{Name: SecretBotPrivateKey, Help: "the bot App", Secret: true},
	}
}

// entryFields describe a github entry's config.
func entryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "namespace", Help: "stable source identity and correlation-key source", Required: true},
		{Name: "scope", Help: "the containing scope's id", Required: true},
		{Name: "repo", Help: "owner/name", Required: true},
		{Name: "tracker", Help: "shared tracker {repo, namespace}"},
		{Name: "track_issues", Default: "true"},
		{Name: "labels", Help: "claimed, help, review, done, skip[], require[] (Open Supervisor's names by default)"},
		{Name: "closed_horizon_hours", Help: "how long closed issues stay observed", Default: "168"},
		{Name: "branch_pattern", Default: `^agent/issue-(\d+)$`},
		{Name: "preview_pattern", Default: "deploy-preview"},
		{Name: "projection", Help: "disabled, verify_seconds (default 900), dry_run"},
		{Name: "webhook_secret", Help: "names a selected scope secret that verifies deliveries"},
		{Name: "manage_webhook", Help: "create or re-point the repository webhook at the listener's public URL (needs admin)"},
		{Name: "base_branch"},
		{Name: "no_checks_minutes", Help: "-1 disables", Default: "10"},
	}
}
