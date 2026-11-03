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
- Either list can name a [toolset](#toolsets) as `@<name>`: every operation the toolset has in the running release, so an install granting it gets what a later release adds to the role. `!<operation>` (or `!@<toolset>`) takes one back out. A toolset a binding names is narrowed to what the scope grants, and a name that isn't registered fails the config.
- An operation a `human` extension entry lists in `gates` waits at the host for a person's approval before it runs, whoever calls it (see [configuration](../config/#human-decisions-human)).
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

## Toolsets

The operations an extension's agents need for a role, granted together as `@<name>`. Each operation joined its toolset in the version shown; a toolset only gains operations. `open-conduit doctor` reports a list that names an earlier version's operations one by one, with the `config set` that grants the toolset instead.

### `@github.author`

Work a GitHub issue: read it, comment, open and edit its PRs, answer their reviews and wait on them.

| Operation | Since version |
| --- | --- |
| `read_context` | 1 |
| `wait` | 1 |
| `declare_outcome` | 1 |
| `github.context` | 1 |
| `github.issue.view` | 1 |
| `github.issue.comment` | 1 |
| `github.issue.list` | 1 |
| `github.issue.create` | 1 |
| `github.pr.list` | 1 |
| `github.pr.view` | 1 |
| `github.pr.create` | 1 |
| `github.pr.comment` | 1 |
| `github.pr.reply` | 1 |
| `github.checks` | 1 |
| `github.attachment.upload` | 1 |
| `github.attachment.get` | 1 |
| `github.wait` | 1 |
| `github.pr.review` | 2 |
| `github.pr.resolve` | 3 |
| `github.pr.edit` | 4 |

### `@github.holds`

Run held issues: read and settle holds.

| Operation | Since version |
| --- | --- |
| `emit` | 1 |
| `github.hold.status` | 1 |
| `github.hold` | 1 |

### `@github.reviewer`

Review GitHub PRs: read them and their checks, comment, reply in and resolve threads, and submit reviews.

| Operation | Since version |
| --- | --- |
| `read_context` | 1 |
| `wait` | 1 |
| `declare_outcome` | 1 |
| `github.context` | 1 |
| `github.issue.view` | 1 |
| `github.pr.list` | 1 |
| `github.pr.view` | 1 |
| `github.pr.comment` | 1 |
| `github.pr.reply` | 1 |
| `github.pr.resolve` | 1 |
| `github.pr.review` | 1 |
| `github.checks` | 1 |
| `github.attachment.upload` | 1 |
| `github.attachment.get` | 1 |
| `github.wait` | 1 |

### `@github.workspace`

Push from a git workspace: git's credential helper asks for a short-lived token.

| Operation | Since version |
| --- | --- |
| `github.git.credential` | 1 |

## Bundled extensions

The extensions built into the daemon register these operations, each with the input it takes; `open-conduit adapters TYPE` lists them too.

### Bitbucket Cloud (`bitbucket`)

Pull requests, their comments and commit statuses of a repository, correlated with a tracker's issues.

#### `bitbucket.checks`

Show every check on a pull request's head commit, with its state and link.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `number` | integer |  | the number of one of this Work's pull requests; defaults to its open pull request |

#### `bitbucket.context`

Show the Bitbucket repository, this Work's issue and branch, how a PR refers to the issue and the base branch.

Takes no input.

#### `bitbucket.pr.comment`

Comment on one of this Work's pull requests; a retried call never posts twice.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the comment, in Markdown; must not be empty |
| `number` | integer | yes | the number of one of this Work's pull requests |

#### `bitbucket.pr.create`

Open a pull request from this Work's branch that refers to its issue, or return the one already open.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `base` | string |  | the branch to merge into; defaults to the configured base branch, else the repository's default branch |
| `body` | string |  | the pull request's description, in Markdown; a line referring to the issue is added when it does not name the issue |
| `draft` | boolean |  | open the pull request as a draft; defaults to false |
| `title` | string | yes | the pull request's title; must not be empty |

#### `bitbucket.pr.list`

List this Work's pull requests in any state: those from its branch or that mention its issue.

Takes no input.

#### `bitbucket.pr.reply`

Reply to a comment on one of this Work's pull requests; a retried call never posts twice.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the reply, in Markdown; must not be empty |
| `comment_id` | integer | yes | the id of the comment to reply to, as pr.view shows it |
| `number` | integer | yes | the number of one of this Work's pull requests |

#### `bitbucket.pr.view`

Read a pull request with its description and every comment, inline comments included.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `number` | integer | yes | the pull request's number |

### GitHub (`github`)

Issues, pull requests, comments, reviews and checks of a repository; the engineering tools.

#### `github.attachment.get`

Download a file a comment or link points to on GitHub, signed in, to a path you choose.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `project` | string |  | the project (its ID or name) whose issue or PR the attachment was posted on, when it isn't this Work's |
| `save_to` | string |  | the path the os client saves the file to; without it the file isn't saved |
| `url` | string | yes | the link to download: a comment attachment, or a file in a repository this project may read |

#### `github.attachment.upload`

Upload a file to this Work's issue tracker and get a link to put in a comment.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `content_base64` | string | yes | the file's content, base64, up to 50 MB; pass file_path instead and the os client reads the file into it |
| `name` | string |  | the file's name, shown in the link; file_path's base name when the os client fills content_base64 |

#### `github.checks`

Show the state of every check on a PR's head commit, with links to failing ones.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `number` | integer |  | the PR whose checks to read; defaults to the open PR of this Work's issue, and is required for any other |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |

#### `github.context`

Show this Work's repository, issue or PR, branch, base branch and where the project's rules are.

Takes no input.

#### `github.git.credential`

Answer git's credential request with a short-lived token for this Work's code repository.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `host` | string | yes | the host git asks for; only the code repository's GitHub host gets a credential |
| `path` | string |  | the repository path git asks for, when it sends one; only the code repository's gets a credential |
| `protocol` | string | yes | the protocol git asks for; only https gets a credential |

#### `github.hold`

Keep this held issue waiting until what it was opened to wait for happens.

Takes no input.

#### `github.hold.status`

Report whether this held issue still has something to wait for before it is worked.

Takes no input.

#### `github.issue.comment`

Comment on this Work's issue, or another issue by number.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the comment, in Markdown; must not be empty |
| `number` | integer |  | the issue to comment on; defaults to this Work's issue, and is required with project |
| `project` | string |  | another project (its ID or name) whose issue to comment on; this project must be allowed to file there |

#### `github.issue.create`

Open a new issue, optionally held until conditions are met or a time comes, or put on the backlog.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `backlog` | boolean |  | put the issue on the backlog, so it isn't worked until a person takes it off; not with wait or until |
| `body` | string | yes | the issue's description, in Markdown; must not be empty |
| `project` | string |  | another project (its ID or name) to open the issue in; this project must be allowed to file there, and the issue can't then be held |
| `title` | string | yes | the issue's title; must not be empty |
| `until` | string |  | a future ISO time (e.g. 2026-10-25T09:00Z) to hold the issue until; with wait, whichever comes first |
| `wait` | array of string |  | conditions to hold the issue for before it is worked, any one of which starts it: pr:<n>[:merged\|closed\|commented\|pushed\|checks], issue:<n>[:closed\|<state>] or job:<name>, with <project>/<n> or <project>/<name> for another project's |

#### `github.issue.list`

List the open issues of this project's repository, or of another project's.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `project` | string |  | another project (its ID or name) whose open issues to list; this Work's project if omitted |

#### `github.issue.view`

Read this Work's issue, or another by number, with every comment on it.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `number` | integer |  | the issue to read; defaults to this Work's issue, and is required with project |
| `project` | string |  | another project (its ID or name) whose issue to read; this Work's project if omitted |

#### `github.pr.comment`

Comment on a PR's conversation.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the comment, in Markdown; must not be empty |
| `number` | integer | yes | the PR to comment on |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |

#### `github.pr.create`

Open a PR from this issue's branch, linked to the issue so its merge closes it unless told otherwise.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `base` | string |  | the branch to merge into; defaults to the repository's base branch |
| `body` | string | yes | what the PR changes, in Markdown; must not be empty, and a link to the issue is added |
| `closes_issue` | boolean |  | whether merging the PR closes the issue (default true); false on every PR but the last of an issue that takes several |
| `draft` | boolean |  | open the PR as a draft |
| `head` | string |  | the branch to open the PR from; defaults to the issue's branch, and a PR from another branch isn't linked to the issue |
| `title` | string | yes | the PR's title; must not be empty |

#### `github.pr.edit`

Change the title, description or draft state of an open PR this Work opened.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string |  | the new description, in Markdown; not empty, and the link to this Work's issue is kept |
| `draft` | boolean |  | true to mark the PR as a draft, false to mark it ready for review |
| `number` | integer | yes | the PR to edit: an open one this Work opened |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |
| `title` | string |  | the new title; not empty |

#### `github.pr.list`

List the PRs of this Work's issue, open or not.

Takes no input.

#### `github.pr.reply`

Reply in the inline review thread a PR comment is in.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the reply, in Markdown; must not be empty |
| `comment_id` | any | yes | the ID (a number, or a string of digits) of an inline review comment in the thread, as github.pr.view shows it |
| `number` | integer | yes | the PR the review thread is on |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |

#### `github.pr.resolve`

Resolve the inline review thread a PR comment is in, once its point is addressed.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `comment_id` | any | yes | the ID (a number, or a string of digits) of an inline review comment in the thread to resolve, not a review's or a conversation comment's |
| `number` | integer | yes | the PR the review thread is on |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |

#### `github.pr.review`

Submit one review of a PR: approve, request changes or comment, with optional inline comments.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string |  | the review's summary, in Markdown; required unless event is approve |
| `comments` | array of object |  | inline comments on lines of the PR's diff, submitted with the review |
| `event` | string | yes | the verdict: approve, request_changes or comment |
| `number` | integer | yes | the PR to review |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |

#### `github.pr.view`

Read a PR with every comment, inline review comment and review on it.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `number` | integer | yes | the PR to read |
| `repo` | string |  | the repository (owner/name) the PR is in; defaults to this Work's own, else this project's only one |

#### `github.wait`

Park this Work until a PR, issue or job condition is met or a time comes; end the turn after calling it.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `conditions` | array of string |  | what to wait for, any one of which wakes you: pr:<n>[:merged\|closed\|commented\|pushed\|checks], issue:<n>[:closed\|<state>] or job:<name>, with <project>/<n> or <project>/<name> for another project's; give conditions, until or both (a PR's own Work may give neither) |
| `ignore` | array of string |  | news of this Work's own issue or PR that shouldn't wake you, for news your role doesn't act on: conflicts (the PR no longer merges with its base) or checks (check results); it is handed to you with whatever does wake you |
| `reason` | string | yes | what you're waiting for and why; posted on this Work's issue when it has one |
| `repo` | string |  | the repository (owner/name) conditions in this project refer to; defaults to this Work's own, else this project's only one |
| `until` | string |  | a future ISO time (e.g. 2026-10-25T09:00Z) to wake at if nothing else has |

### Human decisions (`human`)

Lets an agent ask a person for a private decision with human.ask, answered from the TUI or web view; nothing reaches the issue or thread. Add human.ask to the agent binding's operations. Gates make named operations wait for a person's approval, whoever calls them.

#### `human.ask`

Ask the person the project has set up to answer for a decision, then end your turn until they answer.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `context` | string |  | what the person needs to know to answer, at most 4000 bytes |
| `deadline` | string |  | a future ISO time with Z or an offset, such as 2026-10-25T09:00Z; with no answer by then you are resumed without one |
| `options` | array of string |  | 2 to 20 distinct answers to choose from, each one line of at most 200 bytes; omit for a free answer |
| `question` | string | yes | the question, at most 2000 bytes |

### Introspection (`introspection`)

Issue reports, scheduled sweep and triage, fix Work and the optional host watch.

#### `introspection.correction`

Log the lesson of a correction you were given, so the daily sweep folds it into the project's rules.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `area` | string |  | what the correction is about: writing, design, data, code or workflow; default workflow |
| `body` | string | yes | the theme in one sentence, a blank line, then the rule a future agent should follow; at most 8192 bytes |

#### `introspection.evidence`

Gather the open reports and unhandled notes a sweep or triage run acts on, grouped, with its settings.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `for` | string | yes | the run the evidence is for: sweep (corrections) or triage (reports and problems) |

#### `introspection.fix`

Request Work that fixes the cause of the named reports and notes.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | what to change and why, for the Work that makes the fix, at most 16384 bytes |
| `notes` | array of string |  | the ids of up to 100 notes the fix addresses; name at least one report or note |
| `reports` | array of string |  | the ids of up to 100 open reports the fix addresses; name at least one report or note |
| `title` | string | yes | the fix in one line of at most 200 bytes |

#### `introspection.notes.resolve`

Mark corrections or problem notes handled so later sweeps and triages leave them out.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `notes` | array of string | yes | the ids of 1 to 500 notes handled, as introspection.evidence gives them |
| `summary` | string | yes | how the notes were handled, at most 2000 bytes |

#### `introspection.problem`

Log a problem you worked around, so the daily triage fixes or files it.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | what you tried in one line, then the exact error, your workaround and where the fix belongs; at most 8192 bytes |
| `kind` | string | yes | what went wrong: env (a missing variable, tool or credential), docs (a wrong or stale skill or rule), tooling (a toolbelt operation failed or is missing), service (the tracker, code host or deploy service misbehaving) or other |

#### `introspection.report-issue`

Report a defect in Open Conduit itself, such as a toolbelt operation or playbook that misbehaved.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | what you tried, the exact error, your workaround and where the fix belongs, at most 8192 bytes |
| `kind` | string | yes | the kind of defect: env, docs, tooling, service, runtime or other |
| `title` | string | yes | the defect in one line of at most 200 bytes |

#### `introspection.report.resolve`

Mark one of this scope's defect reports resolved, saying how it was handled.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id` | string | yes | the report's id, as introspection.reports or introspection.evidence gives it |
| `link` | string |  | a link to the fix, issue or write-up, at most 500 bytes |
| `summary` | string | yes | how the report was handled, at most 2000 bytes |

#### `introspection.reports`

List this scope's defect reports in a given state, oldest first.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `state` | string |  | which reports to list: open, fixing (a fix has been requested), resolved or all; default open |

### Jira (`jira`)

Issues in one or more Jira projects, Cloud or Data Center, through REST API v2 and webhooks.

#### `jira.context`

Show the tracker, this Work's issue and its URL, the branch to push, how a PR refers to the issue and where the rules are.

Takes no input.

#### `jira.issue.comment`

Comment on this Work's issue, or another issue of the same tracker; a retried call never posts twice.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the comment, in Markdown; must not be empty |
| `key` | string |  | another issue of the same tracker entry to comment on, such as ABC-123; defaults to this Work's issue |

#### `jira.issue.create`

Open a follow-up issue in the tracker; a retried call never opens it twice.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string |  | the new issue's description, in Markdown |
| `labels` | array of string |  | label names to put on the new issue; Linear skips names it does not have and Jira refuses names with spaces |
| `title` | string | yes | the new issue's title; must not be empty |

#### `jira.issue.list`

List the open issues of this tracker entry (up to 200), to check for one before opening another.

Takes no input.

#### `jira.issue.view`

Read an issue with its status, labels and every comment; this Work's issue unless another is named.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `key` | string |  | another issue of the same tracker entry to read, such as ABC-123; defaults to this Work's issue |

### Linear (`linear`)

Issues in one or more Linear teams through the GraphQL API and webhooks.

#### `linear.context`

Show the tracker, this Work's issue and its URL, the branch to push, how a PR refers to the issue and where the rules are.

Takes no input.

#### `linear.issue.comment`

Comment on this Work's issue, or another issue of the same tracker; a retried call never posts twice.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string | yes | the comment, in Markdown; must not be empty |
| `key` | string |  | another issue of the same tracker entry to comment on, such as ABC-123; defaults to this Work's issue |

#### `linear.issue.create`

Open a follow-up issue in the tracker; a retried call never opens it twice.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `body` | string |  | the new issue's description, in Markdown |
| `labels` | array of string |  | label names to put on the new issue; Linear skips names it does not have and Jira refuses names with spaces |
| `title` | string | yes | the new issue's title; must not be empty |

#### `linear.issue.list`

List the open issues of this tracker entry (up to 200), to check for one before opening another.

Takes no input.

#### `linear.issue.view`

Read an issue with its status, labels and every comment; this Work's issue unless another is named.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `key` | string |  | another issue of the same tracker entry to read, such as ABC-123; defaults to this Work's issue |

### Observed sources (`observe`)

Lets a binding's executor report what it observed in an outside source with observe.record; the observe route gives each thread of a source Work. Add observe.record to the reporting binding's operations.

#### `observe.record`

Record an item you saw in an outside source so it becomes Work or joins its thread's Work.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `item` | string | yes | the source's own stable ID for the item, such as a message timestamp or row ID; at most 256 bytes |
| `revision` | string |  | only for an edited item, the source's own mark of the edit, such as an edit timestamp; at most 128 bytes |
| `source` | string |  | the outside source the item is from; needed only when you report for more than one |
| `text` | string |  | the item's content as you saw it, at most 16384 bytes |
| `thread` | string | yes | the ID of the thread the item belongs to, its own ID if it starts one; at most 256 bytes |

### Slack (`slack`)

Support conversations in Slack channels over Socket Mode or the Events API.

#### `slack.context`

Show the Slack thread this Work serves and how to read it, reply in it and wait for the next message.

Takes no input.

#### `slack.reply`

Post a reply in this Work's Slack thread; a retried call posts it once.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `broadcast` | boolean |  | also post the reply to the channel, not only the thread; default false |
| `text` | string | yes | the reply as Slack mrkdwn text, not blank and at most 40000 bytes |

#### `slack.thread`

Read the messages of this Work's Slack thread, oldest first.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `limit` | integer |  | how many of the latest messages to return, 1 to 1000; default (and any value outside that range) 200 |

### setup (`setup`)

The web setup assistant: a Claude Code session that looks at this machine and proposes config for the operator to approve and apply.

#### `setup.ask`

It declares no input schema.

#### `setup.probe`

It declares no input schema.

#### `setup.propose`

It declares no input schema.

#### `setup.read`

It declares no input schema.

#### `setup.say`

It declares no input schema.

#### `setup.trace`

It declares no input schema.

#### `setup.validate`

It declares no input schema.

#### `setup.withdraw`

It declares no input schema.

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

Works support conversations in Slack channels over Socket Mode or the Events API, each thread its own Work.

#### `slack.context`

Show the Slack thread this Work serves and how to read it, reply in it and wait for the next message.

Takes no input.

#### `slack.reply`

Post a reply in this Work's Slack thread; a retried call posts it once.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `broadcast` | boolean |  | also post the reply to the channel, not only the thread; default false |
| `text` | string | yes | the reply as Slack mrkdwn text, not blank and at most 40000 bytes |

#### `slack.thread`

Read the messages of this Work's Slack thread, oldest first.

| Input | Type | Required | Meaning |
| --- | --- | --- | --- |
| `limit` | integer |  | how many of the latest messages to return, 1 to 1000; default (and any value outside that range) 200 |
