package bootstrap

import (
	bb "github.com/open-conduit/open-conduit/extensions/bitbucket"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/extensions/jira"
	"github.com/open-conduit/open-conduit/extensions/linear"
	"github.com/open-conduit/open-conduit/extensions/observe"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/internal/cli"
)

// Adapters is the catalog of the integrations this daemon registers (v1
// `adapters [type]`, 06-01): what each registers and the fields it takes.
// It is descriptive only; validation stays with each registration. The
// full-screen config editor builds its forms from it.
func Adapters() []cli.Adapter {
	f := func(name, help string) cli.Field { return cli.Field{Name: name, Help: help} }
	req := func(name, help string) cli.Field { return cli.Field{Name: name, Help: help, Required: true} }
	def := func(name, value, help string) cli.Field { return cli.Field{Name: name, Default: value, Help: help} }
	opt := func(name, value string, options []string, help string) cli.Field {
		return cli.Field{Name: name, Default: value, Options: options, Help: help}
	}
	trackerCommon := []cli.Field{
		req("namespace", "stable source identity and correlation-key source"),
		req("scope", "the containing scope's id"),
		f("labels", "claimed, help, review, done, skip[], require[] (Open Supervisor's names by default)"),
		f("transitions", "workflow status per state: claimed, building, review, help, done, finished (missing leaves it alone)"),
		def("closed_horizon_hours", "168", "how long closed issues stay observed"),
		f("projection", "disabled, verify_seconds (default 900), dry_run"),
		f("webhook_secret", "names a selected scope secret that verifies deliveries"),
		f("manage_webhook", "create or re-point the service webhook at the listener's public URL (needs admin)"),
	}
	trackerRoute := []cli.Field{f("match", "attribute -> values the route claims (labels, status, title, team, project, priority, assignee, components, custom fields); one entry per scope for multi-scope routing")}
	return []cli.Adapter{
		{Type: gh.Type, Title: "GitHub", Description: "Issues, pull requests, comments, reviews and checks of a repository; the engineering tools.", Setup: "github",
			Kinds:      []string{"connection", "extension", "route", "executor:github-hold"},
			Connection: []cli.Field{def("api_url", "https://api.github.com", "or an Enterprise /api/v3"), opt("auth", "token", []string{"token", "app"}, ""), f("app_id", "for auth app"), f("bot_app_id", "an App that performs writes")},
			Secrets:    []cli.Field{f(gh.SecretToken, "token auth (cmd:gh auth token for the gh login)"), f(gh.SecretAppPrivateKey, "app auth"), f(gh.SecretBotPrivateKey, "the bot App")},
			Entry: []cli.Field{req("namespace", ""), req("scope", ""), req("repo", "owner/name"), f("tracker", "shared tracker {repo, namespace}"), def("track_issues", "true", ""),
				f("labels", ""), def("closed_horizon_hours", "168", ""), def("branch_pattern", `^agent/issue-(\d+)$`, ""), def("preview_pattern", "deploy-preview", ""),
				f("projection", ""), f("webhook_secret", ""), f("manage_webhook", ""), f("base_branch", ""), def("no_checks_minutes", "10", "-1 disables")},
			Operations: gh.Operations()},
		{Type: linear.Type, Title: "Linear", Description: "Issues in one or more Linear teams through the GraphQL API and webhooks.", Setup: "linear",
			Kinds:      []string{"connection", "extension", "route"},
			Connection: []cli.Field{def("api_url", linear.DefaultAPIURL, ""), f("agent_account", "a user whose comments always count as an agent's")},
			Secrets:    []cli.Field{req(linear.SecretToken, "a personal API key, or an OAuth token (lin_oauth_…)")},
			Entry: append(append([]cli.Field{}, trackerCommon...), req("teams", "team keys (ENG)"),
				def("states", "unstarted, started", "state types or names issues are claimed from; others wait")),
			Route: trackerRoute, Operations: tracker.Operations(linear.Type)},
		{Type: jira.Type, Title: "Jira", Description: "Issues in one or more Jira projects, Cloud or Data Center, through REST API v2 and webhooks.", Setup: "jira",
			Kinds:      []string{"connection", "extension", "route"},
			Connection: []cli.Field{req("base_url", "https://acme.atlassian.net"), opt("auth", "basic", []string{"basic", "bearer"}, "basic: email + API token (Cloud); bearer: a personal access token"), f("email", "for basic auth"), f("agent_account", "")},
			Secrets:    []cli.Field{req(jira.SecretToken, "API token or personal access token")},
			Entry: append(append([]cli.Field{}, trackerCommon...), req("projects", "project keys (PROJ)"), f("jql", "ANDed with the project and open filter"),
				f("fields", "extra field ids routes match on (customfield_10010)")),
			Route: trackerRoute, Operations: tracker.Operations(jira.Type)},
		{Type: bb.Type, Title: "Bitbucket Cloud", Description: "Pull requests, their comments and commit statuses of a repository, correlated with a tracker's issues.", Setup: "linear|jira --bitbucket",
			Kinds:      []string{"connection", "extension", "route"},
			Connection: []cli.Field{def("api_url", bb.DefaultAPIURL, ""), opt("auth", "basic", []string{"basic", "bearer"}, ""), f("username", "the Atlassian account email (basic)")},
			Secrets:    []cli.Field{req(bb.SecretToken, "API token (basic) or access token (bearer)")},
			Entry: []cli.Field{req("namespace", ""), req("scope", ""), req("repo", "workspace/slug"), f("tracker", "{namespace, keys}: the tracker entry and its key prefixes"),
				def("branch_pattern", `^agent/issue/([A-Za-z][A-Za-z0-9_]*-[0-9]+)(?:/|$)`, ""), def("preview_pattern", "preview|deploy", ""), def("issue_ref", "{key}", "Closes {key} for Linear"),
				def("closed_horizon_hours", "168", ""), f("base_branch", ""), f("webhook_secret", ""), f("manage_webhook", ""), f("dry_run", "")},
			Operations: bb.Operations()},
		{Type: "posthog", Title: "PostHog", Description: "Sends an agent_turn event per finished execution, the lifecycle records an entry selects and the introspection telemetry events to PostHog.",
			Kinds:      []string{"connection", "extension"},
			Connection: []cli.Field{opt("host", "https://us.i.posthog.com", []string{"https://us.i.posthog.com", "https://eu.i.posthog.com"}, "ingestion host"), def("distinct_id", "open-conduit@<hostname>", "")},
			Secrets:    []cli.Field{req("token", "the project API key (phc_…)")},
			Entry: []cli.Field{def("turns", "true", "agent_turn events of the entry's scope"), def("telemetry_events", "true", "forward introspection telemetry events"),
				f("records", `lifecycle records of the entry's scope to send, each as an event named after it: names, families such as "execution.*", or "*"`)}},
		{Type: sl.Type, Title: "Slack", Description: "Support conversations in Slack channels over Socket Mode or the Events API.", Setup: "slack",
			Kinds:      []string{"connection", "extension", "route"},
			Connection: []cli.Field{def("api_url", "https://slack.com/api", "")},
			Secrets:    []cli.Field{req(sl.SecretBotToken, "xoxb-…"), f(sl.SecretAppToken, "xapp-… for Socket Mode")},
			Entry: []cli.Field{req("namespace", ""), req("scope", ""), req("team", ""), req("channels", ""), f("start", ""), f("bot_user_id", ""), f("start_window_hours", ""), f("history_hours", ""),
				f("refresh_minutes", ""), opt("receiver", "socket", []string{"socket", "events", "none"}, ""), f("signing_secret", ""), f("projection", "")},
			Operations: sl.Operations()},
		{Type: "schedule", Title: "Scheduled jobs", Description: "Every/at/cron jobs and webhook, code-host and manual triggers as standing Work.",
			Kinds: []string{"extension", "route", "executor:legacy-job"},
			Entry: []cli.Field{req("namespace", ""), req("scope", ""), f("webhook_secret", ""), req("jobs", "name, every_minutes | at | cron, time_zone, on {events, branches, paths, webhook}")},
			Route: []cli.Field{f("jobs", "job names the route takes"), f("triggers", "trigger kinds")}},
		{Type: "introspection", Title: "Introspection", Description: "Issue reports, scheduled sweep and triage, fix Work and the optional host watch.",
			Kinds: []string{"extension", "route"},
			Entry: []cli.Field{f("notes", ""), f("sweep", "{cron, time_zone, rules, guide, land, remote}"), f("triage", "{cron, time_zone, file_issues}"),
				f("host_watch", "{interval_seconds, load, memory, disk, disks}"), f("telemetry", "write telemetry events (and send them to PostHog entries)")},
			Route: []cli.Field{opt("work", "report", []string{"report", "sweep", "triage", "fix"}, "")}},
		{Type: human.Type, Title: "Human decisions", Description: "Lets an agent ask a person for a private decision with human.ask, answered from the TUI or web view; nothing reaches the issue or thread. Add human.ask to the agent binding's operations.",
			Kinds:      []string{"extension"},
			Entry:      []cli.Field{def("responders", `["operator:local"]`, "who may answer, each kind:id or a bare kind"), def("surfaces", `["tui"]`, "where the question is shown")},
			Operations: []string{human.OpAsk}},
		{Type: observe.Type, Title: "Observed sources", Description: "Lets a binding's executor report what it observed in an outside source with observe.record; the observe route gives each thread of a source Work. Add observe.record to the reporting binding's operations.",
			Kinds:      []string{"extension", "route"},
			Entry:      []cli.Field{req("sources", `[{"name": "slack-help", "reporters": ["watcher"]}]: each source and the bindings that may report for it`)},
			Route:      []cli.Field{req("source", "the source whose threads the route takes")},
			Operations: observe.Operations()},
		{Type: "claude-code", Title: "Claude Code", Description: "Runs Claude Code turns with sessions, handoff, attach and the MCP toolbelt.", Setup: "--with claude",
			Kinds: []string{"executor"},
			Binding: []cli.Field{def("binary", "claude", ""), f("model", ""), def("permission_mode", "bypassPermissions", ""), f("playbook", "the playbook skill turns start on, as the extension's setup writes it; none (or -) starts them on the rendered Work alone"),
				f("plugin_dir", ""), def("args", "{number}", "a template of {key}, {number} and {work}"), def("status_schema", "core.turn.v1", ""), f("branch", "agent/issue-{number}; agent/{key} for trackers"),
				f("workspace_mode", ""), f("dir", "a folder a workspace-free binding runs in, as-is"), f("handoff_minutes", ""), opt("cold_turns", "handoff", []string{"handoff", "resume"}, "what a turn does once the session is handoff_minutes old"), f("env", ""), f("pass_env", ""), f("secret_env", ""), f("extra_args", ""), f("builtin_tools", "the only built-in tools offered; [] turns them all off"), f("models", "models a Work may pick with model in its context")}},
		{Type: "shell", Title: "Shell", Description: "Runs a command as a detached, adoptable process with the os CLI toolbelt.",
			Kinds:   []string{"executor"},
			Binding: []cli.Field{f("command", "a shell command"), f("argv", "or an argument vector"), f("dir", ""), f("env", ""), f("secret_env", ""), f("timeout_seconds", "")}},
		{Type: "git", Title: "Git workspace", Description: "Shared, fresh or per-Work worktree workspaces of a checkout, or per-Work clones of it.",
			Kinds: []string{"resource"},
			Binding: []cli.Field{req("checkout", "the main checkout"), opt("mode", "work", []string{"work", "fresh", "shared", "clone"}, ""), f("remote", ""), f("base_branch", ""), f("branch_template", ""),
				f("sign", ""), f("copy", ""), f("setup", ""), f("setup_when", ""), f("setup_timeout_seconds", ""),
				def("disk_full_percent", "90", "a new workspace may not leave the disk fuller; 100 turns the check off"), f("disk_reserve_bytes", "the most kept free, default 10 GiB")}},
		{Type: "tunnel", Title: "Webhook tunnels", Description: "Exposes the webhook listener: ngrok, cloudflared or a static public URL (settings.webhooks; serve flags of the same names override it for one run).",
			Kinds: []string{"serve"},
			Binding: []cli.Field{req("addr", "the listener, for example 127.0.0.1:8787"), opt("tunnel", "", []string{"ngrok", "cloudflared", "static"}, ""), f("tunnel_domain", "ngrok reserved domain"),
				f("tunnel_name", "cloudflared named tunnel"), f("tunnel_hostname", "its hostname"), f("public_url", "static")}},
	}
}
