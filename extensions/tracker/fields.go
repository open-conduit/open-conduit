package tracker

import "github.com/open-conduit/open-conduit/protocol"

// EntryFields are the entry config fields every tracker kind takes, for
// the operator's catalogue (manifest fields); a kind adds its own after
// them (Kind.EntryFields).
func EntryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "namespace", Help: "stable source identity and correlation-key source", Required: true},
		{Name: "scope", Help: "the containing scope's id", Required: true},
		{Name: "labels", Help: "claimed, help, review, done, skip[], require[] (Open Supervisor's names by default)"},
		{Name: "transitions", Help: "workflow status per state: claimed, building, review, help, done, finished (missing leaves it alone)"},
		{Name: "closed_horizon_hours", Help: "how long closed issues stay observed", Default: "168"},
		{Name: "projection", Help: "disabled, verify_seconds (default 900), dry_run"},
		{Name: "webhook_secret", Help: "names a selected scope secret that verifies deliveries"},
		{Name: "manage_webhook", Help: "create or re-point the service webhook at the listener's public URL (needs admin)"},
	}
}

// RouteFields are the route config fields of every tracker kind.
func RouteFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "match", Help: "attribute -> values the route claims (labels, status, title, team, project, priority, assignee, components, custom fields); one entry per scope for multi-scope routing"},
	}
}
