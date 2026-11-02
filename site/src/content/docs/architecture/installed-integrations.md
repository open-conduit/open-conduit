---
title: "Installed integrations: Linear, Jira, Bitbucket, PostHog and operator tools"
---

Besides GitHub, Open Conduit ships adapters for Linear and Jira (trackers), Bitbucket Cloud (a code host for a tracker scope) and PostHog (telemetry), plus operator tools that work across every integration. The trackers share the kit in [`extensions/tracker`](https://github.com/open-conduit/open-conduit/tree/main/extensions/tracker); each adapter is its own extension under [`extensions/`](https://github.com/open-conduit/open-conduit/tree/main/extensions), and [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/tree/main/internal/bootstrap) composes them. The tests run them against fakes (`lineartest`, `jiratest`, `bitbuckettest` and a PostHog capture fake); they have not been checked against real accounts (see [Limitations](#limitations)).

## The shared tracker kit

[`extensions/tracker`](https://github.com/open-conduit/open-conduit/tree/main/extensions/tracker) is the code every non-GitHub tracker shares. It imports only public contracts. A tracker supplies a `Service` (its API client and webhook format), and the kit supplies:

- **Ingress.** Push and reconcile share one normalizer, so equal state has an equal delivery ID from either transport.
  - Families are `<type>.issue.open|closed`, `<type>.comment.human|bot|agent` and the one-shot `<type>.delivery`.
  - An issue is keyed `issue/KEY`, or `issue/KEY/generation/reopen-<unix>` after a reopening. The generation is derived from the service's immutable reopen history. A comment joins the generation current when it was written.
  - The issue payload omits update times, status labels and any workflow status the entry's `transitions` manage, so projection never feeds back as input.
  - Reconcile lists open issues and issues updated within `closed_horizon_hours` in creation order, plus each listed issue's comments. Pages are bounded by detail reads and wall time. Failures are errors, so no absence is inferred.
  - Push re-reads exactly the issue or comment a verified delivery reports.
- **Route** (handler = type). The opt-in/skip claim policy:
  - The issue must be open and not *waiting*: outside Linear's claim states.
  - It must have no skip label, and a required label when any are configured.
  - It must not carry the claimed label, unless it is a reopened generation.
  - An optional route `match` on attributes (labels, status, title, team, project, priority, assignee, components, custom fields) selects issues per route.
  - Input: closes, human comments and other Works' agent comments; never edits, bot comments or the Work's own notes.
  - An operator's `<type>.claim.requested` overrides the policy. For GitHub this is the entry's `claim` action, accepted under `core.action`.
- **Projector.** It is level-triggered from durable Work state:
  - Labels: claimed while owned, plus at most one of the help, review or done labels, none while an attempt runs. The claimed label comes off once completed Work's issue is closed: admission never claims a closed issue, and a reopening starts a new generation, which is claimed again. Failed and cancelled Work keep it.
  - It re-reads the issue when desired state changes, and at `verify_seconds` for nonterminal Work and for completed Work whose issue is still open, until the issue closes. After it moves a completed Work's issue (a `finished` transition that may close it), the next pass re-reads it at once.
  - Workflow status: claimed, then the declared status (`core.turn.v1` `{status, comment}`, or for one release the `<type>.status.v1` stored outcomes keep), done or help for completed or failed Work. It is moved only when the entry names a status for that state.
  - Each outcome comment goes through the `Outbox`: pending → attempted (recorded before the write) → posted, with mark search after a lost answer, so nothing is posted twice across a crash.
- **Tools.** `<type>.context`, `.issue.view`, `.issue.comment` (a durable intent, idempotent per request), `.issue.list` and `.issue.create` (never repeated after an ambiguous answer).

The runtime holds no tracker vocabulary. Labels and transitions exist only in the kit's projector, and the same SQLite runtime cases run for both trackers (`TestTrackerReconcileClaimProjectAndRecover`).

**Boundary decision (provisional).** `extensions/linear`, `extensions/jira` and `extensions/bitbucket` may import the public-only kit. No other extension may, and the kit reaches no extension. The boundary checker and its cases enforce this.

## Linear

[`extensions/linear`](https://github.com/open-conduit/open-conduit/tree/main/extensions/linear) works one or more teams over the GraphQL API.

- **Connection.** `api_url` and `agent_account`. The secret is `token`: a personal key, or `lin_oauth_…` sent as a bearer token.
- **Entry.** `teams` and `states` (claim-from state types or names; default unstarted, started), plus the kit's fields.
- **Rate limits and credentials.** A `RATELIMITED` answer pauses the client until `X-RateLimit-Requests-Reset`. A 401 re-resolves the token once.
- **Marks.** Linear drops HTML comments, so a comment carries a visible line `*open-conduit work=… intent=…*`. The line is visible in Linear: a provisional decision, not yet checked against how Linear renders it.
- **Webhooks.** `Linear-Signature` (hex HMAC-SHA256), a ten-minute replay window on the signed `webhookTimestamp` (a delivery without one, or dated more than ten minutes ahead, is refused) and `Linear-Delivery` as the identity. `manage_webhook` creates a team's hook, or an all-public-teams hook for several teams. That needs a workspace admin.
- **Labels.** A missing managed label is created on first use.

## Jira

[`extensions/jira`](https://github.com/open-conduit/open-conduit/tree/main/extensions/jira) works Cloud or Data Center projects over REST API v2.

- **Connection.** `base_url`, `auth` (basic with `email`, or bearer) and `agent_account`. The secret is `token`.
- **Entry.** `projects`, `jql` and `fields` (custom fields routes can match), plus the kit's fields. Labels cannot contain spaces.
- **Bodies.** Wiki markup is converted to and from Markdown.
- **Listing.** The enhanced search (`/search/jql`, token pages) falls back to the classic search on a 404.
- **Closed.** An issue is closed once its status is in the done category or, when the issue carries no category, once it has a resolution. A `finished` or `done` transition to a done-category status therefore closes it, and completed Work then loses the claimed label.
- **Reopens.** They come from the changelog against status categories, or from a cleared resolution when the categories are unreadable.
- **Marks.** Comments carry an entity property (`work` and `intent`).
- **Transitions.** Matched by target status first, then by transition name.
- **Webhooks.** `X-Hub-Signature` HMAC through the admin API (needs an administrator, refusal code `hook_permission_denied`). **Intentional difference:** unsigned deliveries are refused rather than accepted with a log line; reconcile recovers what they would have said.

## Bitbucket Cloud

[`extensions/bitbucket`](https://github.com/open-conduit/open-conduit/tree/main/extensions/bitbucket) is the code host for a tracker scope.

- **Families.** `bitbucket.pull_request.open|closed|merged`, `bitbucket.comment.*`, `bitbucket.checks.pending|passed|failed` (commit statuses, with the preview URL from `preview_pattern`) and the one-shot `bitbucket.delivery`.
- **Correlation.** A PR is keyed to its tracker issue in the tracker entry's namespace (`tracker {namespace, keys}`). The issue comes from the branch (`agent/issue/KEY`, the claude-code `agent/{key}` branch) first, then from a key in the title or description, and the key uses the issue generation current at the PR's creation. Bootstrap resolves the generation through the registered tracker's reopen history. Otherwise the PR keeps `pr/N`.
- **Route** (`bitbucket`). It never creates Work. Input: merges, closes, finished checks, human comments and other agents' comments.
- **Tools.** `bitbucket.context`, `.pr.list`, `.pr.view`, `.pr.create` (idempotent by state: an open PR from the branch is the answer; the body gets the issue reference `issue_ref`), `.pr.comment` and `.pr.reply`. The comment tools use the outbox with an invisible Markdown mark. `.checks` completes the set.
- **Webhooks.** `X-Hub-Signature` HMAC, `X-Event-Key`, and `X-Request-UUID` as the identity. Pagination never follows a link off the configured API.

## PostHog

[`extensions/posthog`](https://github.com/open-conduit/open-conduit/tree/main/extensions/posthog) is configured by a `posthog` connection (`host`, `distinct_id`, secret `token` = `phc_…`) and a non-ingress `posthog` entry in a scope (`turns`, `telemetry_events`, both default true, and `records`, default none). `telemetry_events` was called `introspection` before. The old name is still accepted for one release: doctor warns `renamed_setting` and `config check` warns too. `open-conduit migrate upgrade [--write]` renames it in place.

- **Exporter.** It is on the observability pipeline. It sends `agent_turn` for each finished execution of the entry's scope: outcome, attempt, executor and `executor_binding` (the configured binding, so two setups of one executor stay apart), work kind, duration, tokens and cost, plus `status` (the state the turn ended in: `building`, `review`, `help`, `done` or `finished`), `started_at`, and `trigger_type` and `trigger_at` for the event that started the turn. Time in a state is the gap from a turn's event to the next turn's `trigger_at` for the same `work_id`: the time after a `review` or `help` turn is how long a person took to answer. A comment's `trigger_type` says who wrote it: `<source>.comment.human`, `.bot` or `.agent`. A failing PostHog loses telemetry (counted, reported as doctor `capture_failing`) and never delays the runtime.
- **Lifecycle records.** `records` selects lifecycle records of the entry's scope ([records](../observability-history/#records)) to send as events named after them: exact names (`work.transitioned`), families (`execution.*`) or `*` for all. Each event carries `record`, `trace_id`, `event_id`, `work_id`, `execution_id`, `executor_binding`, `from` and `to` when present, every attribute under its record name (integers as numbers) and the usage fields of `agent_turn`. The pipeline has already redacted them. Each exported batch goes to PostHog's `/batch/` endpoint as one request per entry. Its `uuid` derives from the scope, record name and `record_id`, so PostHog keeps one copy of a redelivered record. `lease.renewed` and `ingress.scan_page` are frequent, so name families rather than `*` unless you want them.
- **Host telemetry events.** The host writes its telemetry events (reports, fixes, host readings) to its JSON log, then hands each one to every extension implementing `inproc.Sink`: `Capture(ctx, scope, name, at, props)`. `scope` is the event's scope, or `""` for one about the machine, and `props` never carry it. PostHog sends a scoped event to that scope's entries that set `telemetry_events`, and an unscoped one to every such entry, once per project and `distinct_id` when several scopes' entries share a connection. It adds the scope as the `scope` property.
- **Dry run.** A daemon dry run sends nothing.

## Composition, doctor, setup and dry run

[`internal/bootstrap/trackers.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/trackers.go) and [`posthog.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/posthog.go) register the types, the route-config validators, the tools on every generation, and the webhook routes `POST /<type>/<scope>/<entry>` on the listener. They also run `manage_webhook` re-pointing through the same retry and doctor paths as GitHub, and a background projection pass.

- **Doctor.** `trackers` (registration, rate limit, reconcile, projection, a Bitbucket entry whose tracker is not registered, dry run), `trackers_auth` (check_secrets: sign-in), `posthog` and `posthog_auth` (key form; PostHog cannot be asked about a key without sending an event). The `webhooks` check lists tracker routes and hooks.
- **Setup.** `init linear --teams ENG[,OPS]` and `init jira --site … --projects …`. Both take `--bitbucket WORKSPACE/SLUG --bitbucket-username … --bitbucket-token REF` and an optional `--checkout`. They write a claude-code binding on the `work-tracker-issue` playbook (`args {key}`, branch `agent/{key}`) and validate offline. The playbook is the kit's and, like the kit, names no other extension: it refers to the tracker's operations as `<tracker>.*` and the code host's as `<host>.*`, whichever the toolbelt lists. It leaves house workflow (how to sort issues, what a report says) to the project's rules. Scopes stay workspace-free unless a checkout is given.
- **Dry run.** `serve --dry-run` makes every tracker client write nothing. Projection, tools and webhook management report `dry_run`.

## Operator tools

- **`open-conduit adapters [TYPE] [--json]`** lists the registered integrations and the connection, secret, entry, route and binding fields each takes, from `bootstrap.Adapters()`.
- **`open-conduit route KEY [--scope S]`** explains how a key's Events were routed: the owning Work, and for each correlated Event its disposition and the router's durable decision (route, binding, generation, policy, reason). It reads them through `GET /v1/events/{id}/route`, so the explanation is what execution used. The decision is recorded, not predicted.
- **`open-conduit config edit [--file F]`** is the full-screen editor. It shows the config tree and the catalog's documented fields for the selected connection, entry, route, binding or resource, marking those present. It makes the same pointer edits as `config set/unset`, shows secret references redacted (and they must stay references), and saves only a document that validates offline.
- **Rendered activity.** The Claude Code executor writes each turn's `activity.txt` (agent text, tool calls, tool results, the turn's end) as the first run artifact. The TUI's log view and `runs log` show it; `t` cycles to the raw transcript and stderr. While a turn runs, the executor's `Live` renders the same activity from the segment's `out.jsonl` so far and returns it with the transcript and stderr, so `l`, `t` and `runs log --follow` work before the artifacts are published. The executor owns the transcript format, so the TUI stays free of executor branches.

## Evidence

- **Unit and contract tests.** Each extension's tests include the shared ingress conformance, cases (wiki conversion, marks, pagination and fallback, transitions, reopens, webhooks and admin refusals, rate limits and credentials) and the kit's route, generation and config cases.
- **SQLite.** [`integration/tracker_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/tracker_test.go) runs for both trackers: claim policy, input, labels and status convergence without feedback, a lost comment answer recovered after a restart, close and reopen generations, the tools, and Bitbucket PR, checks, comment and merge input to the tracker Work.
- **Daemon drill.** `TestDaemonTrackersAndPostHog` (in `make drill`) runs Linear and Jira scopes, Bitbucket and PostHog on fakes. It covers `os call` tools, projection, PostHog `agent_turn`, doctor, `route` and `adapters`, and a restart that posts nothing again.
- **Setup.** `TestTrackerSetupScopesValidateOffline`.
- **Editor.** `TestConfigEditor`.

## Limitations

- **No live evidence.** Real accounts, webhooks, rate limits and admin hooks for Linear, Jira, Bitbucket and PostHog are untested; all evidence uses fixtures and fakes.
- **Deleted issues.** A deleted issue ages out as absence; it is not delivered as a close.
- **Multi-scope routing.** One entry per scope with a route `match`, kept single-owner by the claimed label. Overlapping matches can double-claim a reopened generation, so keep matches disjoint.
- **Tracker waits and holds.** `issue:`/`pr:` waits and scheduled holds are GitHub-only. Tracker agents use the builtin `wait`.
- **Bitbucket comment tools.** An ambiguous answer is confirmed by calling again; there is no background redrive.
- **Comment detection.** Linear comments on issues that are not listed are read only while the issue is listed: open, or updated within the horizon.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 06-01 delivered the shared tracker kit, the Linear, Jira, Bitbucket and PostHog adapters, `init linear` and `init jira`, and the operator tools (`adapters`, `route`, `config edit`, rendered activity). `GET /v1/events/{id}/route` was added for `route`.
- All 06-01 evidence used fixtures and fakes. No real service was called; checks against configured accounts were left as live-operation evidence for the deferred tickets 03-07, 03-09 and 06-04, and the Linear mark's rendering check was deferred to live use.
