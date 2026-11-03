---
title: Configuration
description: "Every field of the version 1 config file: its type, default and meaning, and the fields each integration takes."
---

This page is generated from [the configuration schema](https://github.com/open-conduit/open-conduit/blob/main/docs/architecture/config.schema.json) and the integrations' catalog (`open-conduit adapters --json`) by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.

The config file is JSON, by default at `~/.config/open-conduit/config.json`. `open-conduit config check` validates it offline; [configuration and secrets](../../guides/configuration/) walks through writing one, and [decision B05](../../architecture/config-management/) records how inheritance, reload and secrets work.

## Top level

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `connections` | array of [connection](#connections) | yes |  | Credentials and endpoints for the services integrations talk to. |
| `includes` | array of object |  |  | Folders whose .open-conduit/config.json adds scopes beneath parent, a scope of this file. path is absolute or relative to this file's directory. |
| `scopes` | array of [scope](#scopes) | yes |  | The tree of scopes: what each watches, how its Work runs and what it may do. |
| `settings` | [settings](#settings) | yes |  | Daemon-wide limits and timing. |
| `version` | `1` | yes |  | Config format version; always 1. |

## settings

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `admission_limit` | integer |  | `10` | Reserved for a cap on claimed Work per scope. Validated but not enforced: config check warns where a config sets it. |
| `budgets` | array of object |  |  | Usage limits the host enforces before it starts a turn: at most max of unit per Work, per scope or across every scope, over the Work's life or each UTC day or week. Work over a limit is held with the reason. |
| `causal_max_depth` | integer |  | `32` | How many Events one chain of causes may reach (an Event caused by an execution that an Event started, and so on). The Event past it is refused with a depth_limit diagnostic, which stops feedback loops without the extension's cooperation. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `causal_per_minute` | integer |  | `128` | How many Events one causal chain may add per minute; a burst past it is refused. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `concurrency` | integer |  | `4` | Most agent turns (claude-code and installed extension executors) running at once across every scope. A scope's limits.concurrency can only narrow it. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `job_concurrency` | integer |  | `4` | Most job attempts (the shell and github-hold executors) running at once, counted apart from concurrency so a busy agent pool never holds a due job back. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `kept_executions` | integer |  | `20` | Executions per Work whose content (logs, transcripts, outputs) is kept; older ones are released by the retention sweep. Their coordination history (trigger, timing, outcome, usage) is never deleted. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `kept_finished_days` | integer |  | `14` | Days a finished Work keeps its attempts' content (logs, transcripts, outputs) after it last changed; the retention sweep then releases all of it. Coordination history is never deleted. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `reconcile_seconds` | integer |  | `900` | How often each source (a repository, a tracker, a Slack workspace, a schedule) is reconciled against its service, so missed webhooks cost only this much latency. Sources added by a later generation (open-conduit config apply) use the new value; bundled sources already running keep theirs until serve restarts on that generation. |
| `retry_delays_seconds` | array of integer |  | `[300,600]` | The wait before each retry of a failed attempt, in order; once they are used up automatic retries stop and the Work stays open for an operator to retry. Usage-limit retries wait for the reported reset instead. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. |
| `timeout_seconds` | integer |  | `3600` | The deadline of one attempt (an Execution) before the host stops it. A binding's own timeout_seconds replaces it for that binding. Takes effect when the next config generation is applied with open-conduit config apply; editing the file or restarting serve alone changes nothing. An attempt already running keeps the deadline it started with. |
| `webhooks` | object |  |  | The webhook listener and tunnel serve starts with; serve flags override it for one run. Without it the daemon is reconcile-only. Takes effect at the next serve start after the generation holding it is applied with open-conduit config apply. |

### settings.webhooks

The webhook listener and tunnel serve starts with; serve flags override it for one run. Without it the daemon is reconcile-only. Takes effect at the next serve start after the generation holding it is applied with open-conduit config apply.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `addr` | string | yes |  | Local address the listener binds, such as 127.0.0.1:8787. |
| `public_url` | string |  |  | The URL services deliver webhooks to, for tunnel static. |
| `tunnel` | one of `ngrok`, `cloudflared`, `static` |  |  | How the listener is reached from outside: ngrok, cloudflared, or static (public_url already reaches it). |
| `tunnel_domain` | string |  |  | The ngrok domain to use. |
| `tunnel_hostname` | string |  |  | The cloudflared tunnel's public hostname. |
| `tunnel_name` | string |  |  | The cloudflared named tunnel to run. |

## connections

Each entry of `connections`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `config` | object | yes |  | The integration's connection fields (open-conduit adapters TYPE). |
| `id` | string | yes |  | Stable ID that scopes, bindings and extension entries name. |
| `secret_refs` | object |  |  | Secret name to reference (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND); values never appear in the config. |
| `type` | string | yes |  | The integration that owns it (open-conduit adapters lists them). |

## scopes

Each entry of `scopes`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `authorization` | [authorization](#authorization) |  |  | What executions in the scope may do. Omitted, a child inherits its parent's; given, each list must be a subset of the parent's. |
| `bindings` | array of [binding](#bindings) |  |  | Executor bindings. |
| `enabled` | boolean |  | `true` | false stops new Work and attempts in the scope and its descendants. |
| `extensions` | array of [extension](#extensions) |  |  | Extension entries. |
| `id` | string | yes |  | Stable scope ID. |
| `limits` | object |  |  | Limits that narrow the parent's. |
| `name` | string | yes |  | Display name. |
| `parent` | string |  |  | Parent scope; a child inherits its bindings, routes, extensions and authorization, and can only narrow them. |
| `routes` | array of [route](#routes) |  |  | Event routes. |
| `secret_refs` | object |  |  | Scope secret name to reference. |

### limits

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `admission_limit` | integer |  |  | Reserved; validated but not enforced. |
| `concurrency` | integer |  |  | Most attempts running in this scope at once, at most the parent's. |

## authorization

A scope's `authorization`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `connections` | array of string | yes |  | Connection IDs this scope (and its bindings) may use. |
| `file_scopes` | array of string |  |  | Other scopes whose trackers this scope's agents may file issues in and comment on. |
| `operations` | array of string | yes |  | Toolbelt operations executions in this scope may call (see the operations reference): an operation, a toolset an extension publishes as `@<name>` (every operation it has in the running release), or either denied with a leading `!`. A binding's operations narrow it. |
| `read_scopes` | array of string | yes |  | Other scopes whose Events this scope's executions may wait on. |

## bindings

How Work is executed: an executor and what it is given. {"name", "disabled": true} turns off an inherited binding.

Each entry of a scope's `bindings`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `config` | object |  |  | The executor's own config (open-conduit adapters TYPE). |
| `connection` | string |  |  | Connection the executor's tools act through. |
| `container` | object |  |  | Container provider and its config, pinned to an image digest. |
| `executor` | string | yes |  | The registered executor that runs attempts. |
| `grants` | array of object |  |  | Named sets of extra operations and config a Work can opt into. |
| `name` | string | yes |  | Name routes and Work refer to. |
| `operations` | array of string |  |  | Narrows the scope's authorization.operations for this binding: operations, toolsets as `@<name>` and denials with a leading `!`, as there. A toolset is narrowed to what the scope grants. |
| `requires` | [requires](#requires) |  |  | Capabilities the executor must declare; binding fails closed otherwise. |
| `resource` | object |  |  | Resource provider and its config (such as a git workspace). |
| `sandbox` | object |  |  | Process sandbox provider and its config. |
| `secret_names` | array of string |  |  | Scope secrets the executor may resolve. |
| `timeout_seconds` | integer |  |  | Replaces settings.timeout_seconds for this binding's attempts. |

## requires

A binding's `requires`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `inject` | boolean |  | `false` | The executor must accept messages into a running attempt. |
| `interrupt` | boolean |  | `false` | The executor must support interrupting a running attempt. |
| `resume` | boolean |  | `false` | The executor must resume a parked attempt's session. |
| `toolbelt` | one of `none`, `cli`, `mcp`, `sdk` |  |  | How the executor reaches the toolbelt. |
| `workspace` | boolean |  | `false` | The executor must run in a workspace. |

## routes

Which Events start or reach Work, and with which binding. {"name", "disabled": true} turns off an inherited route.

Each entry of a scope's `routes`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `active_policy` | one of `queue`, `drop`, `inject`, `interrupt` | yes |  | What a matching Event does while the Work already has an attempt running: queue it for the next attempt, drop it, inject it into the running attempt, or interrupt the attempt. |
| `binding` | string | yes |  | Binding new Work runs with. |
| `config` | object |  |  | The handler's own config (open-conduit adapters TYPE). |
| `error_fallback` | one of `queue`, `drop` |  |  | What to do when injecting or interrupting fails. |
| `handler` | string | yes |  | The registered route handler that matches Events. |
| `name` | string | yes |  | Stable route name. |
| `unsupported_fallback` | one of `queue`, `drop` |  |  | What to do when the executor cannot inject or interrupt as active_policy asks. |

## extensions

An extension entry: an integration watching a source for this scope. {"name", "disabled": true} turns off an inherited entry.

Each entry of a scope's `extensions`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `config` | object |  |  | The entry's fields (open-conduit adapters TYPE). |
| `connection` | string |  |  | Connection it reads and writes through. |
| `name` | string | yes |  | Stable entry name. |
| `secret_names` | array of string |  |  | Scope secrets it may resolve. |
| `type` | string | yes |  | The registered extension type (open-conduit adapters). |

## includes

Each entry of `includes`.

| Field | Type | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `parent` | string | yes |  | A scope defined in this file that the folder's scopes sit beneath. |
| `path` | string | yes |  | The folder holding .open-conduit/config.json, absolute or relative to this file's directory (no ~). |

Folders whose .open-conduit/config.json adds scopes beneath parent, a scope of this file. path is absolute or relative to this file's directory.

## Integration fields

The `config` objects of connections, extension entries, routes and bindings belong to the integration that registers each type, which checks them. `open-conduit adapters TYPE` prints the same list.

### Bitbucket Cloud (`bitbucket`)

Pull requests, their comments and commit statuses of a repository, correlated with a tracker's issues. Registers: connection, extension, route.
Set up with `open-conduit init linear|jira --bitbucket`.

Connection `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `api_url` | `https://api.bitbucket.org/2.0` |  |
| `auth` | `basic` | One of basic, bearer. |
| `username` |  | the Atlassian account email (basic) |

Connection `secret_refs`:

| Field | Default | Meaning |
| --- | --- | --- |
| `token` |  | Required. API token (basic) or access token (bearer) |

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `namespace` |  | Required. |
| `scope` |  | Required. |
| `repo` |  | Required. workspace/slug |
| `tracker` |  | {namespace, keys}: the tracker entry and its key prefixes |
| `branch_pattern` | `^agent/issue/([A-Za-z][A-Za-z0-9_]*-[0-9]+)(?:/\|$)` |  |
| `preview_pattern` | `preview\|deploy` |  |
| `issue_ref` | `{key}` | Closes {key} for Linear |
| `closed_horizon_hours` | `168` |  |
| `base_branch` |  |  |
| `webhook_secret` |  |  |
| `manage_webhook` |  |  |
| `dry_run` |  |  |

### Claude Code (`claude-code`)

Runs Claude Code turns with sessions, handoff, attach and the MCP toolbelt. Registers: executor.
Set up with `open-conduit init --with claude`.

Binding `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `binary` | `claude` |  |
| `model` |  |  |
| `permission_mode` | `bypassPermissions` |  |
| `playbook` |  | the playbook skill turns start on, as the extension's setup writes it; none (or -) starts them on the rendered Work alone |
| `plugin_dir` |  |  |
| `args` | `{number}` | a template of {key}, {number} and {work} |
| `status_schema` | `core.turn.v1` |  |
| `branch` |  | agent/issue-{number}; agent/{key} for trackers |
| `workspace_mode` |  |  |
| `dir` |  | a folder a workspace-free binding runs in, as-is |
| `handoff_minutes` |  |  |
| `cold_turns` | `handoff` | what a turn does once the session is handoff_minutes old One of handoff, resume. |
| `handoff_tokens` | `400000` | a turn after one that ended with more context starts afresh from the handoff note; 0 turns it off |
| `auto_compact_tokens` | `600000` | where Claude Code compacts mid-turn (CLAUDE_CODE_AUTO_COMPACT_WINDOW); 0 leaves its own |
| `env` |  |  |
| `pass_env` |  |  |
| `secret_env` |  |  |
| `extra_args` |  |  |
| `builtin_tools` |  | the only built-in tools offered; [] turns them all off |
| `models` |  | models a Work may pick with model in its context |
| `usage` |  | doctor reads the subscription usage windows of this binding's Claude login |

### Git workspace (`git`)

Shared, fresh or per-Work worktree workspaces of a checkout, or per-Work clones of it. Registers: resource.

Binding `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `checkout` |  | Required. the main checkout |
| `mode` | `work` | One of work, fresh, shared, clone. |
| `remote` |  |  |
| `base_branch` |  |  |
| `branch_template` |  |  |
| `sign` |  |  |
| `copy` |  |  |
| `setup` |  |  |
| `setup_when` |  |  |
| `setup_timeout_seconds` |  |  |
| `disk_full_percent` | `90` | a new workspace may not leave the disk fuller; 100 turns the check off |
| `disk_reserve_bytes` |  | the most kept free, default 10 GiB |

### GitHub (`github`)

Issues, pull requests, comments, reviews and checks of a repository; the engineering tools. Registers: connection, extension, route, executor:github-hold.
Set up with `open-conduit init github`.

Connection `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `api_url` | `https://api.github.com` | or an Enterprise /api/v3 |
| `auth` | `token` | One of token, app. |
| `app_id` |  | for auth app |
| `bot_app_id` |  | an App that performs writes |

Connection `secret_refs`:

| Field | Default | Meaning |
| --- | --- | --- |
| `token` |  | token auth (cmd:gh auth token for the gh login) |
| `app_private_key` |  | app auth |
| `bot_private_key` |  | the bot App |

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `namespace` |  | Required. |
| `scope` |  | Required. |
| `repo` |  | Required. owner/name |
| `tracker` |  | shared tracker {repo, namespace} |
| `track_issues` | `true` |  |
| `labels` |  |  |
| `closed_horizon_hours` | `168` |  |
| `branch_pattern` | `^agent/issue-(\d+)$` |  |
| `preview_pattern` | `deploy-preview` |  |
| `projection` |  |  |
| `webhook_secret` |  |  |
| `manage_webhook` |  |  |
| `base_branch` |  |  |
| `no_checks_minutes` | `10` | -1 disables |

### Human decisions (`human`)

Lets an agent ask a person for a private decision with human.ask, answered from the TUI or web view; nothing reaches the issue or thread. Add human.ask to the agent binding's operations. Gates make named operations wait for a person's approval, whoever calls them. Registers: extension.

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `responders` | `["operator:local"]` | who may answer, each kind:id or a bare kind |
| `surfaces` | `["tui"]` | where the question is shown |
| `gates` |  | [{"operations": ["github.pr.create"], "responders": ["operator:local"], "timeout": "24h"}]: operations that need a person's approval before they run |

### Introspection (`introspection`)

Issue reports, scheduled sweep and triage, fix Work and the optional host watch. Registers: extension, route.

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `notes` |  |  |
| `sweep` |  | {cron, time_zone, rules, guide, land, remote} |
| `triage` |  | {cron, time_zone, file_issues} |
| `host_watch` |  | {interval_seconds, load, memory, disk, disks} |
| `telemetry` |  | write telemetry events (and send them to PostHog entries) |

Route `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `work` | `report` | One of report, sweep, triage, fix. |

### Jira (`jira`)

Issues in one or more Jira projects, Cloud or Data Center, through REST API v2 and webhooks. Registers: connection, extension, route.
Set up with `open-conduit init jira`.

Connection `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `base_url` |  | Required. https://acme.atlassian.net |
| `auth` | `basic` | basic: email + API token (Cloud); bearer: a personal access token One of basic, bearer. |
| `email` |  | for basic auth |
| `agent_account` |  |  |

Connection `secret_refs`:

| Field | Default | Meaning |
| --- | --- | --- |
| `token` |  | Required. API token or personal access token |

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `namespace` |  | Required. stable source identity and correlation-key source |
| `scope` |  | Required. the containing scope's id |
| `labels` |  | claimed, help, review, done, skip[], require[] (Open Supervisor's names by default) |
| `transitions` |  | workflow status per state: claimed, building, review, help, done, finished (missing leaves it alone) |
| `closed_horizon_hours` | `168` | how long closed issues stay observed |
| `projection` |  | disabled, verify_seconds (default 900), dry_run |
| `webhook_secret` |  | names a selected scope secret that verifies deliveries |
| `manage_webhook` |  | create or re-point the service webhook at the listener's public URL (needs admin) |
| `projects` |  | Required. project keys (PROJ) |
| `jql` |  | ANDed with the project and open filter |
| `fields` |  | extra field ids routes match on (customfield_10010) |

Route `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `match` |  | attribute -> values the route claims (labels, status, title, team, project, priority, assignee, components, custom fields); one entry per scope for multi-scope routing |

### Linear (`linear`)

Issues in one or more Linear teams through the GraphQL API and webhooks. Registers: connection, extension, route.
Set up with `open-conduit init linear`.

Connection `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `api_url` | `https://api.linear.app/graphql` |  |
| `agent_account` |  | a user whose comments always count as an agent's |

Connection `secret_refs`:

| Field | Default | Meaning |
| --- | --- | --- |
| `token` |  | Required. a personal API key, or an OAuth token (lin_oauth_…) |

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `namespace` |  | Required. stable source identity and correlation-key source |
| `scope` |  | Required. the containing scope's id |
| `labels` |  | claimed, help, review, done, skip[], require[] (Open Supervisor's names by default) |
| `transitions` |  | workflow status per state: claimed, building, review, help, done, finished (missing leaves it alone) |
| `closed_horizon_hours` | `168` | how long closed issues stay observed |
| `projection` |  | disabled, verify_seconds (default 900), dry_run |
| `webhook_secret` |  | names a selected scope secret that verifies deliveries |
| `manage_webhook` |  | create or re-point the service webhook at the listener's public URL (needs admin) |
| `teams` |  | Required. team keys (ENG) |
| `states` | `unstarted, started` | state types or names issues are claimed from; others wait |

Route `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `match` |  | attribute -> values the route claims (labels, status, title, team, project, priority, assignee, components, custom fields); one entry per scope for multi-scope routing |

### Observed sources (`observe`)

Lets a binding's executor report what it observed in an outside source with observe.record; the observe route gives each thread of a source Work. Add observe.record to the reporting binding's operations. Registers: extension, route.

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `sources` |  | Required. [{"name": "slack-help", "reporters": ["watcher"]}]: each source and the bindings that may report for it |

Route `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `source` |  | Required. the source whose threads the route takes |

### PostHog (`posthog`)

Sends an agent_turn event per finished execution, the lifecycle records an entry selects and the introspection telemetry events to PostHog. Registers: connection, extension.

Connection `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `host` | `https://us.i.posthog.com` | ingestion host One of https://us.i.posthog.com, https://eu.i.posthog.com. |
| `distinct_id` | `open-conduit@<hostname>` |  |

Connection `secret_refs`:

| Field | Default | Meaning |
| --- | --- | --- |
| `token` |  | Required. the project API key (phc_…) |

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `turns` | `true` | agent_turn events of the entry's scope |
| `telemetry_events` | `true` | forward introspection telemetry events |
| `records` |  | lifecycle records of the entry's scope to send, each as an event named after it: names, families such as "execution.*", or "*" |

### Scheduled jobs (`schedule`)

Every/at/cron jobs and webhook, code-host and manual triggers as standing Work. Registers: extension, route.

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `namespace` |  | Required. |
| `scope` |  | Required. |
| `webhook_secret` |  |  |
| `jobs` |  | Required. name, every_minutes \| at \| cron, time_zone, on {events, branches, paths, webhook} |

Route `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `jobs` |  | job names the route takes |
| `triggers` |  | trigger kinds |

### Shell (`shell`)

Runs a command as a detached, adoptable process with the os CLI toolbelt. Registers: executor.

Binding `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `command` |  | a shell command |
| `argv` |  | or an argument vector |
| `dir` |  |  |
| `env` |  |  |
| `secret_env` |  |  |
| `timeout_seconds` |  |  |

### Slack (`slack`)

Support conversations in Slack channels over Socket Mode or the Events API. Registers: connection, extension, route.
Set up with `open-conduit init slack`.

Connection `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `api_url` | `https://slack.com/api` |  |

Connection `secret_refs`:

| Field | Default | Meaning |
| --- | --- | --- |
| `bot_token` |  | Required. xoxb-… |
| `app_token` |  | xapp-… for Socket Mode |

Extension entry `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `namespace` |  | Required. |
| `scope` |  | Required. |
| `team` |  | Required. |
| `channels` |  | Required. |
| `start` |  |  |
| `bot_user_id` |  |  |
| `start_window_hours` |  |  |
| `history_hours` |  |  |
| `refresh_minutes` |  |  |
| `receiver` | `socket` | One of socket, events, none. |
| `signing_secret` |  |  |
| `projection` |  |  |

### Webhook tunnels (`tunnel`)

Exposes the webhook listener: ngrok, cloudflared or a static public URL (settings.webhooks; serve flags of the same names override it for one run). Registers: serve.

Binding `config`:

| Field | Default | Meaning |
| --- | --- | --- |
| `addr` |  | Required. the listener, for example 127.0.0.1:8787 |
| `tunnel` |  | One of ngrok, cloudflared, static. |
| `tunnel_domain` |  | ngrok reserved domain |
| `tunnel_name` |  | cloudflared named tunnel |
| `tunnel_hostname` |  | its hostname |
| `public_url` |  | static |
