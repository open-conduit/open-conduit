# Engineering tools, waits and playbooks

Ticket 03-04 ports Open Supervisor's agent-facing engineering workflow (`internal/toolbelt`, `internal/playbooks`, `internal/core/wait.go` and the hold half of `internal/engine/waits.go`, pinned `c2461e55`) onto public extension points. The pieces are:

- the GitHub tool operations ([`extensions/github/tools.go`](../../extensions/github/tools.go), [`tools_wait.go`](../../extensions/github/tools_wait.go)), registered through `extension.Registrar`;
- translated waits: a public `Operation.Wait` contract and the runtime that declares and installs them ([`internal/runtime/toolbelt_wait.go`](../../internal/runtime/toolbelt_wait.go), [`waits.go`](../../internal/runtime/waits.go));
- scheduled holds: the `github-hold` route and in-process executor ([`extensions/github/hold.go`](../../extensions/github/hold.go));
- the ported playbooks and setup guide ([`extensions/github/playbooks`](../../extensions/github/playbooks));
- composition in [`internal/bootstrap/tools.go`](../../internal/bootstrap/tools.go).

No domain condition, issue, PR or label type entered core. Core gained only generic, extension-neutral additions: an operation kind that translates to a wait, opaque dependency filters, a request identity for handlers, and authored refusals.

## Contract additions (B03/B04)

| Addition | Meaning |
| --- | --- |
| `extension.Operation.Wait` | Set instead of `Handler`. It returns a `WaitPlan`: a `WaitRequest` on the current Work's own keys (types plus an optional timeout), `Dependencies` and an `Output`. The toolbelt runs it outside storage after the fenced authority check, then declares the request exactly like the builtin `wait`: same fence, watermark and immediate authority release, applied by the trusted host handshake when the segment finishes or suspends. The reference toolbelt double still supports pure handlers only. |
| `extension.Dependency` | A B04 authorized dependency subscription: `SourceKey` (another Work's correlation, or one nobody owns yet), `Types`, `AliasType`, an opaque `Filter`, a short `Label`, an optional `SourceScopeID` and `FromNow`. `FromNow` scans only Events accepted after the declaration (an occurrence such as "the next turn ends"); otherwise all accepted history counts, so an already-met state wakes the wait at once. |
| `Registration.DependencyFilter` | A pure function that decides whether a type-matching source Event satisfies a dependency's opaque filter (for example, one PR among several keyed to the same issue). Core calls it inside the reconcile transaction through `SchedulerOptions.AliasFilter` (`Toolbelt.DependencyFilter`), keyed by the registration name, which is the subscription namespace. |
| `extension.CallRequestID(ctx)` | The caller's toolbelt request ID, so handlers derive stable intent identities for non-re-derivable writes. |
| `extension.OperationError` | A refusal the handler authors: a stable code and a short, secret-free message the agent can act on ("PR #12 is already merged: no need to wait, carry on now"). The management API returns it as `422` with that code and message instead of a generic failure. |

Runtime (B04 clarification): a translated wait's dependencies are stored in the toolbelt declaration and persisted as pending subscriptions *in the wait-registration transaction*. If an own-key match already woke the wait, nothing is pending. They are then installed through `Scheduler.Subscribe`, which authorizes outside storage, revalidates scopes and backscans source history. Installation runs best-effort right after `Complete`, and durably on every `ReconcileSubscriptions` pass, so a crash between registration and installation only delays it. Refused installations are recorded and not retried. `SubscriptionRequest` gains `After` (the source scan start), `Filter` and `Label`. The alias payload (`core.subscription.alias.v1`) now carries `subscription`, `event`, `source_type` and `label`: lineage, never the provider body. A cross-scope dependency is checked at declaration (`ToolbeltOptions.AuthorizeAlias`, which refuses the call and keeps authority) and again at installation and reconcile (`SchedulerOptions.AuthorizeAlias`). Bootstrap answers both from B05: the waiting scope must list the source scope in `authorization.read_scopes`.

## Tool operations

An agent reaches an operation as MCP `call {operation, input}` or `os call <operation> --data <json>`. The MCP `call` tool now sends its `input` object as the payload's data; an explicit `{schema, data}` payload still passes through. Every handler reads the Work it serves from durable state, finds the issue it owns (`issue/N` in a registered namespace) and acts on that issue and its repository. With a shared tracker, PR operations use the code repository whose entry names the tracker.

| Operation | v1 command | Notes |
| --- | --- | --- |
| `github.context` | `agent context` | repo, tracker, issue and URL, branch (`agent/issue-N` when the branch pattern allows it), base branch (`base_branch` or the repository default). |
| `github.issue.view` / `.list` | `agent issue view` / `list` | Each comment carries its author, agent marker and the reply call. |
| `github.issue.comment` | `agent issue comment` | Posted through `Projector.Deliver` as a durable intent `call/<execution>/<request id>` (or the digest of the input when no request ID is given), carrying a marker, so the Work never hears its own note. A lost response returns `pending`. Converge finishes it by marker search, and the comment is never posted twice. `number` reaches another agent's issue. |
| `github.issue.create` | `agent issue create [--wait] [--until]` | Follow-up child work as a marked, intent-backed issue: an ambiguous create is found again by its marker. With `wait`/`until`, the conditions are validated and the hold marker and `scheduled` label are added. |
| `github.pr.list` / `.view` / `.create` | `agent pr list/view/create` | Create adds `Closes #N` (or `owner/repo#N`) and uses GitHub's one-open-PR-per-head rule for idempotency: a retried or lost create returns the open PR. View includes inline comments and reviews with reply calls. |
| `github.pr.comment` / `.reply` | `agent pr comment/reply` | Durable intents. A reply records `ReplyTo` and recovers by searching the PR's review comments. |
| `github.checks` | `agent checks` | The PR head's evaluated runs, failures and preview, using the ingress evaluation. |
| `github.attachment.upload` | `agent attachment upload` | Committed to the orphan `open-conduit-attachments` branch at `<issue>/<intent digest>-<name>`, so a retry keeps one file. |
| `github.attachment.get` | `agent attachment get` | Signed-in fetch, only for this host, its API host or `*.githubusercontent.com`. A redirect that leaves the host drops `Authorization`, and foreign links are refused without a request. The result uses the generic `open-conduit.toolbelt.file.v1` schema; the `os` client writes it to `save_to` in the agent's own directory. A `file_path` input is read and sent as `content_base64` the same way. |
| `github.wait` | `agent wait`, `agent schedule` | See below. |
| `github.correction` / `.problem` | `agent correction/problem` | Durable notes in KV namespace `github.notes` (one per request) for the stage 05 sweep and triage. Areas and kinds are v1's. |
| `github.hold.status` / `github.hold` | (engine) | Used by the hold executor. |

Not ported:

- **Worktree commands.** `agent worktree setup/remove` are replaced by B07: the Claude Code binding acquires the issue's `work` workspace before the turn, and the host removes it when the Work completes. No workspace operation is registered, and agents get no removal authority.
- **`inflight`.** Not ported (03-03 note), so the playbook drops it.
- **`--upstream` reporting.** Not reproduced as a GitHub flag. 03-08 delivers reporting as the introspection extension's own operations (`introspection.report-issue`), which never hold ([issue reporting](issue-reporting.md)).
- **Dry-run.** Handled per entry by `projection.dry_run`: writes report `dry_run` and make no change.

## Waits

`github.wait {conditions, until, reason}` keeps v1's syntax: `pr:N[:merged|closed]`, `issue:N[:closed|<turn state>]`, `job:<name>`, `<project>/` prefixes and `until`. It translates as follows:

| Condition | Source key | Types | Filter | History |
| --- | --- | --- | --- | --- |
| `pr:N:*` | the PR's correlation (its issue's, by branch or closing keyword, else `pr/N`) | `merged`, `closed` (either ends the wait, v1) | `{kind: pr, repo, number}` | all |
| `issue:N:closed` | `issue/N` at its current generation | `github.issue.closed` | `{kind: issue, …}` | all |
| `issue:N:<state>` | `issue/N` | `github.agent.turn` | `{kind: turn, …, state}` | from now (v1 `WaitSince`) |
| `until` | (none) | (none) | (none) | core timeout timer |

Behaviour:

- **Refusals.** As in v1, the call checks each condition before any side effect and refuses one that has already happened (`already_met`: carry on), an unknown project, a past time or nothing to wait for. A race between that read and registration is closed by the subscription backscan.
- **Jobs.** `job:<name>` (05-02) is a FromNow dependency on the job's key in its schedule entry, over the `schedule.run.completed|failed|cancelled|handoff` Events the host announces for each finished run, so the job's next run wakes it ([job waits](schedule-extension.md#job-waits)). A job the scope's schedule entries do not declare refuses with `no_job`.
- **The reason.** It is posted on the issue with v1's hold marker, so people, and Open Supervisor, see what is awaited.
- **What wakes the Wait.** The Wait matches the Work's own keys with the alias type plus the own-key inputs that still reach a waiting agent: human comments, closes, merges and check results. A comment therefore wakes the agent, as in v1.
- **Intentional difference.** One wake ends a core Wait, so the agent waits again if the prerequisite is still pending. The playbook says so. Agent comments are excluded, because the wait's own reason comment is one.
- **Ending the turn.** `github.wait` releases authority, so it is the turn's last call. `--cancel` is gone: not waiting is the cancel.

`issue:N:<state>` needs an Event: the projector derives `github.agent.turn` (`{repo, number, state}`) from each finished attempt whose outcome declares a `github.status.v1` status. Bootstrap accepts it as a trusted derived Event of that attempt's trigger, with delivery `turn/<execution>`, so a crash only repeats a deduplicated delivery. The `github` route never delivers it.

The renderer (`RenderEvent`, wired as the Claude Code `Options.Render`) uses v1's wording: `You were waiting for PR #50 to merge. That has happened: PR #50 was merged.` A timeout reads that the latest time has come. Comments carry their reply call (for example `call github.pr.reply {"number": 4, "comment_id": 77, …}`).

## Scheduled holds

A scope enables holds with a `github-hold` route ahead of its `github` route, bound to a `github-hold` binding (executor `github-hold`, toolbelt `sdk`, operations `emit`, `github.hold.status`, `github.hold`). The flow is:

1. **Claim.** The route claims an open issue carrying the `scheduled` label that otherwise passes admission (skip, require and claimed rules) as `github.issue.held` Work. The `github` route never claims scheduled issues.
2. **Hold.** The executor runs in process and calls the toolbelt with the attempt's own credential over the management socket. It reads the latest marker (body, then comments; a cancel marker drops it) and declares the translated hold with `github.hold`: dependency aliases plus a core timeout. The attempt then parks.
3. **Already met.** A condition that already happened is aliased from history at registration and wakes the hold at once. This is the same core path as every wait.
4. **Release.** When the hold ends, the executor emits `github.hold.released {why}` and hands off. The reason is a dependency met, the time come, a cancel marker, or "no marker", which is a scheduled task that starts now. The `github` route queues the release for the agent binding, so the first agent turn starts with v1's text, for example "This issue was held, waiting for PR #61 to merge. That has happened: …".
5. **Moved time.** A time-out re-reads the marker first, so an owner who moved the time is honoured.

Projections (v1): held Work carries only `scheduled` (not claimed) until its first agent turn starts. Work parked on an agent's wait carries `claimed` and `scheduled` and no status. `scheduled` is a managed label.

## Checks timing

- **Checks without the Checks permission (03-06).** A fine-grained personal access token cannot be granted Checks, and GitHub answers its check-run reads with an ordinary 403. On the real sandbox that failed every reconcile once a PR existed. Now:
  - The client falls back to the head's GitHub Actions workflow runs (Actions: read).
  - When those are refused too, only commit statuses are evaluated, and a head with nothing readable is never reported as having no checks.
  - The `github.checks` tool returns state `unknown` with a note.
  - Doctor's `github_auth` reports `workflow_runs` as a note and `checks_unreadable` as a warning. A GitHub App covers check runs fully.
- **No checks.** `github.checks.none` replaces v1's no-checks event. An open PR head with no runs or statuses is observed as having no checks once its commit is older than `no_checks_minutes` (default 10; `-1` disables). The occurrence is the commit time plus the delay, so it is deterministic for push and reconcile. The route delivers it, and the renderer prints v1's "No checks ran" text. A failed commit read observes nothing rather than failing a scan.
- **No debounce.** v1's 20-second debounce is intentionally not ported. Deliveries are state-versioned aggregates, so repeated evaluations of one head state deduplicate. A check suite that starts after the others completed can still produce an early `passed`, as it could in v1 beyond 20 seconds.

## Playbooks and setup

- **work-issue.** `extensions/github/playbooks/work-issue.md` keeps v1's operational instructions, with tool calls rewritten to the operations above. The worktree steps become "you are in the issue's workspace", and cleanup becomes host removal on completion. Waits gain the one-wake rule, and the in-flight list is dropped.
- **Setup guide.** `setup/setup.md` is the setup guide for Open Conduit's JSON config, `config validate|apply`, scopes, bindings, routes, the GitHub entry and the git resource. Its example config validates against the daemon's registry. The setup command (`init --with claude`) is delivered by 03-05, which also extends the guide with `init`, `config set`, `secret set`, webhooks, permissions, dry run and the TUI controls.
- **Plugin.** Bootstrap writes the plugin (`.claude-plugin/plugin.json`, `skills/<name>/SKILL.md`) to `<state>/playbooks/plugin` at every start. A `claude-code` binding without `plugin_dir` uses it.
- **Project additions.** Read from the turn's workspace at `.open-conduit/playbooks/<playbook>.md`. Open Supervisor's `.open-supervisor/playbooks/` is a documented fallback when the project has no Open Conduit file. The additions are appended as "This project's additions to the playbook".

## Verification

```sh
go test -race ./extensions/github/... ./internal/cli ./executors/claudecode
go test -race -run 'TestTranslatedWait|TestGitHubTools|TestGitHubWait|TestGitHubAlready|TestGitHubHeld|TestGitHubCrossScope|TestGitHubNoChecks|TestGitHubToolWrites|TestSetupGuide|TestDaemonGitHub' ./integration
```

- **Translated waits.** [`translated_wait_test.go`](../../integration/translated_wait_test.go) runs translated waits through the production toolbelt and scheduler on SQLite, persistent and reopened. It covers an already-met dependency waking at once with alias lineage and no key transfer, `FromNow` ignoring history and then waking once, a filter selecting the named item, own-key input still waking, cross-scope refusal and authorization, and the timeout timer.
- **Tools and holds.** [`github_tools_test.go`](../../integration/github_tools_test.go) ports v1's fixtures against the GitHub fake, through the real toolbelt, router, scheduler, projector and hold executor. It covers:
  - every tool, including retried comments, PRs, replies, uploads, notes and issues being written once, and authenticated fetch with no token leaving the host;
  - lost responses recovered by marker;
  - `TestWaitOnAnotherPRWakesTheAgentWhenItMerges`, `TestAlreadyMetWaitWakesAtOnce`, `TestHeldIssueStartsWhenAnotherAgentReports`, and a held issue starting at its time;
  - a cross-scope wait and no-checks.
- **Daemon.** [`TestDaemonGitHubHeldIssueAndTools`](../../integration/daemon_github_test.go) drives the composed daemon: config validation of the operations, the hold route, the in-process executor over the real socket, a timer release, and `os call github.issue.comment` posting once.
- **Unit and contract tests.** These cover condition and marker parsing (v1 cases), filters, routes, rendering and the registration, the playbooks naming only registered operations, plugin preparation, project additions, and the `os` client's file conventions.

## Limitations and follow-on work

- **Fixtures only.** No real GitHub write or real Claude Code session ran. Real-issue evidence of the tools, waits and holds is 03-06.
- **Not ported yet.** Job waits are delivered by 05-02. A hold condition nothing here can meet (an unknown job or project) leaves only the hold's time; with no time the issue starts at once and its first turn says why, rather than being held for ever. Sweep and triage consume `github.notes` since 05-03: the introspection extension reads these records through public storage as evidence and records the notes it handled in its own namespace, leaving `github.notes` untouched ([introspection](issue-reporting.md#evidence-aggregation)). The setup command and operator controls are delivered by 03-05 ([operator setup](operator-setup.md)).
- **Intentional differences from v1.** One wake ends a wait, so the agent waits again. There is no `--cancel`. The checks debounce is gone. Since 03-06 an injected comment that claude echoed is consumed with its turn (`Result.ConsumedInputs`). Since 04-02 the proof also counts when the turn ends before the dispatch acknowledgement commits, and on waiting and failed turns, so an injected comment is never listed again once claude echoed it; the playbook's "may be listed again" caveat is removed.
