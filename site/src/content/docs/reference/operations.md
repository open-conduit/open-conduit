---
title: Toolbelt operations
description: "Every operation an execution can call through the toolbelt, the extension that serves it, its input and the authority it needs."
---

This page is generated from the operation registry (the core's builtin operations, the bundled extensions' registrations and the installable packages' manifests) by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.

An execution reaches Open Conduit through its toolbelt: the `os` CLI, or the MCP server it serves a coding agent (`os mcp`). The builtin operations are tools of their own; every other operation is called through `call` with its `operation` name and `input` object.

## Authority

The host checks every call where it serves it, from the execution's token, and refuses what the token does not grant:

- The scope's `authorization.operations` lists the operations its executions may call. A child scope's list can only narrow its parent's.
- A binding's `operations` is the exact list its executions get, within the scope's. A Work whose route opts into one of the binding's `grants` also gets that grant's operations, still within the scope's.
- A call acts on the execution's own Work, and only while its lease is current. Waiting on another scope's Events needs that scope in `read_scopes`; filing an issue in, or commenting on, another scope's tracker needs it in `file_scopes`.

See [configuration](../config/#authorization) for the fields and [toolbelt authority](../../architecture/toolbelt-management/) for the design.

## Builtin operations

Served by the core for every execution whose grant lists them.

### `read_context`

Read this Work's accumulated context.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `request_id` | string |  | Optional idempotency key: retrying with the same ID never repeats the effect. |

### `emit`

Emit a declared event for this Work.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `correlation_key` | object |  |  |
| `data` | object |  | A payload: {schema, data}. |
| `delivery_id` | string | yes | Your stable identity for this event; emitting it again is deduplicated. |
| `occurred_at` | string |  | RFC 3339. |
| `request_id` | string |  | Optional idempotency key: retrying with the same ID never repeats the effect. |
| `type` | string | yes |  |

### `create_work`

Create follow-up child Work in this scope.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `context` | object |  | A payload: {schema, data}. |
| `correlation_keys` | array of object |  |  |
| `id` | string |  |  |
| `kind` | string | yes |  |
| `request_id` | string |  | Optional idempotency key: retrying with the same ID never repeats the effect. |

### `wait`

Suspend this Work until an event matching the types and/or correlation keys arrives, or the timeout passes. Finish your turn after calling it.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `after_sequence` | string |  |  |
| `description` | string |  | What you are waiting for, in a few words ("a reply in the thread"). Shown to operators; its first line is kept, cut to 200 bytes. |
| `match` | object | yes |  |
| `request_id` | string |  | Optional idempotency key: retrying with the same ID never repeats the effect. |
| `timeout_seconds` | integer |  |  |

### `declare_outcome`

Declare how this attempt ends (completed, failed or handoff); it applies when your turn finishes.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `data` | object |  | A payload: {schema, data}. |
| `kind` | one of `completed`, `failed`, `handoff` | yes |  |
| `request_id` | string |  | Optional idempotency key: retrying with the same ID never repeats the effect. |
| `summary` | string |  |  |

## Bundled extensions

The extensions built into the daemon register these operations; `open-conduit adapters TYPE` lists them too. Their registrations name each operation but do not declare its input yet, so this section lists names only.

### Bitbucket Cloud (`bitbucket`)

Pull requests, their comments and commit statuses of a repository, correlated with a tracker's issues.

- `bitbucket.checks`
- `bitbucket.context`
- `bitbucket.pr.comment`
- `bitbucket.pr.create`
- `bitbucket.pr.list`
- `bitbucket.pr.reply`
- `bitbucket.pr.view`

### GitHub (`github`)

Issues, pull requests, comments, reviews and checks of a repository; the engineering tools.

- `github.attachment.get`
- `github.attachment.upload`
- `github.checks`
- `github.context`
- `github.git.credential`
- `github.hold`
- `github.hold.status`
- `github.issue.comment`
- `github.issue.create`
- `github.issue.list`
- `github.issue.view`
- `github.pr.comment`
- `github.pr.create`
- `github.pr.list`
- `github.pr.reply`
- `github.pr.resolve`
- `github.pr.review`
- `github.pr.view`
- `github.wait`

### Human decisions (`human`)

Lets an agent ask a person for a private decision with human.ask, answered from the TUI or web view; nothing reaches the issue or thread. Add human.ask to the agent binding's operations.

- `human.ask`

### Introspection (`introspection`)

Issue reports, scheduled sweep and triage, fix Work and the optional host watch.

- `introspection.correction`
- `introspection.evidence`
- `introspection.fix`
- `introspection.notes.resolve`
- `introspection.problem`
- `introspection.report-issue`
- `introspection.report.resolve`
- `introspection.reports`

### Jira (`jira`)

Issues in one or more Jira projects, Cloud or Data Center, through REST API v2 and webhooks.

- `jira.context`
- `jira.issue.comment`
- `jira.issue.create`
- `jira.issue.list`
- `jira.issue.view`

### Linear (`linear`)

Issues in one or more Linear teams through the GraphQL API and webhooks.

- `linear.context`
- `linear.issue.comment`
- `linear.issue.create`
- `linear.issue.list`
- `linear.issue.view`

### Observed sources (`observe`)

Lets a binding's executor report what it observed in an outside source with observe.record; the observe route gives each thread of a source Work. Add observe.record to the reporting binding's operations.

- `observe.record`

### Slack (`slack`)

Support conversations in Slack channels over Socket Mode or the Events API.

- `slack.context`
- `slack.reply`
- `slack.thread`

## Installable packages

Packages installed with `open-conduit extension install` declare their operations in their manifests, with the input each takes.

### pagerduty

PagerDuty incidents as Work, with operations to read the incident, add a note and (when allowed) acknowledge it.

#### `pagerduty.incident.acknowledge`

Takes no input.

#### `pagerduty.incident.note`

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes |  |

#### `pagerduty.incident.view`

Takes no input.

### sentry

Sentry issues and issue alerts as Work, with operations to read, comment on and resolve the issue.

#### `sentry.issue.comment`

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes |  |

#### `sentry.issue.resolve`

Takes no input.

#### `sentry.issue.view`

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `issue` | string |  |  |

### slack

The first-party Slack extension (workspace-free support threads) as a subprocess package.

#### `slack.context`

Its manifest declares no input schema.

#### `slack.reply`

Its manifest declares no input schema.

#### `slack.thread`

Its manifest declares no input schema.
