package bitbucket

import "github.com/open-conduit/open-conduit/protocol"

// connectionFields describe the bitbucket connection's config and
// secret_refs for the operator's catalogue (manifest fields).
func connectionFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "api_url", Default: DefaultAPIURL},
		{Name: "auth", Default: "basic", Options: []string{"basic", "bearer"}},
		{Name: "username", Help: "the Atlassian account email (basic)"},
		{Name: SecretToken, Help: "API token (basic) or access token (bearer)", Required: true, Secret: true},
	}
}

// entryFields describe a bitbucket entry's config.
func entryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "namespace", Help: "stable source identity and correlation-key source", Required: true},
		{Name: "scope", Help: "the containing scope's id", Required: true},
		{Name: "repo", Help: "workspace/slug", Required: true},
		{Name: "tracker", Help: "{namespace, keys}: the tracker entry and its key prefixes"},
		{Name: "branch_pattern", Default: `^agent/issue/([A-Za-z][A-Za-z0-9_]*-[0-9]+)(?:/|$)`},
		{Name: "preview_pattern", Default: "preview|deploy"},
		{Name: "issue_ref", Help: "Closes {key} for Linear", Default: "{key}"},
		{Name: "closed_horizon_hours", Help: "how long closed pull requests stay observed", Default: "168"},
		{Name: "base_branch"},
		{Name: "webhook_secret", Help: "names a selected scope secret that verifies deliveries"},
		{Name: "manage_webhook", Help: "create or re-point the repository webhook at the listener's public URL (needs admin)"},
		{Name: "dry_run"},
	}
}
