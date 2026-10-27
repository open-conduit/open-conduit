---
title: "Engineering tools, waits and playbooks"
---

Ticket 03-04 adds the agent-facing engineering workflow on public extension points. The pieces are:

- the GitHub tool operations ([`extensions/github/tools.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/tools.go), [`tools_wait.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/tools_wait.go)), registered through `extension.Registrar`;
- translated waits: a public `Operation.Wait` contract and the runtime that declares and installs them ([`internal/runtime/toolbelt_wait.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/toolbelt_wait.go), [`waits.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/waits.go));
- scheduled holds: the `github-hold` route and in-process executor ([`extensions/github/hold.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/hold.go));
- the playbooks and setup guide ([`extensions/github/playbooks`](https://github.com/open-conduit/open-conduit/tree/main/extensions/github/playbooks));
- composition in [`internal/bootstrap/tools.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/tools.go).

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

| Operation | Notes |
| --- | --- |
| `github.context` | repo, tracker, issue and URL, branch (`agent/issue-N` when the branch pattern allows it), base branch (`base_branch` or the repository default). |
| `github.issue.view` / `.list` | Each comment carries its author, agent marker and the reply call. |
| `github.issue.comment` | Posted through `Projector.Deliver` as a durable intent `call/<execution>/<request id>` (or the digest of the input when no request ID is given), carrying a marker, so the Work never hears its own note. A lost response returns `pending`. Converge finishes it by marker search, and the comment is never posted twice. `number` reaches another agent's issue. |
| `github.issue.create` | Follow-up child work as a marked, intent-backed issue: an ambiguous create is found again by its marker. With `wait`/`until`, the conditions are validated and the hold marker and `scheduled` label are added. |
| `github.pr.list` / `.view` / `.create` | Create adds `Closes #N` (or `owner/repo#N`) and uses GitHub's one-open-PR-per-head rule for idempotency: a retried or lost create returns the open PR. View includes inline comments and reviews with reply calls. |
| `github.pr.comment` / `.reply` | Durable intents. A reply records `ReplyTo` and recovers by searching the PR's review comments. |
| `github.checks` | The PR head's evaluated runs, failures and preview, using the ingress evaluation. Since 08-01 it also returns the base branch's `required` checks with their sources, any `missing` ones, the states `waiting on required checks` and `required checks missing`, and a note when a source was refused. |
| `github.attachment.upload` | Committed to the orphan `open-conduit-attachments` branch at `<issue>/<intent digest>-<name>`, so a retry keeps one file. |
| `github.attachment.get` | Signed-in fetch, only for this host, its API host or `*.githubusercontent.com`. A redirect that leaves the host drops `Authorization`, and foreign links are refused without a request. The result uses the generic `open-conduit.toolbelt.file.v1` schema; the `os` client writes it to `save_to` in the agent's own directory. A `file_path` input is read and sent as `content_base64` the same way. |
| `github.wait` | See below. |
| `github.correction` / `.problem` | Durable notes in KV namespace `github.notes` (one per request) for the stage 05 sweep and triage. |
| `github.hold.status` / `github.hold` | Used by the hold executor. |

Dry-run is handled per entry by `projection.dry_run`: writes report `dry_run` and make no change.

## Waits

`github.wait {conditions, until, reason}` takes `pr:N[:merged|closed]`, `issue:N[:closed|<turn state>]`, `job:<name>`, `<project>/` prefixes and `until`. It translates as follows:

| Condition | Source key | Types | Filter | History |
| --- | --- | --- | --- | --- |
| `pr:N:*` | the PR's correlation (its issue's, by branch or closing keyword, else `pr/N`) | `merged`, `closed` (either ends the wait) | `{kind: pr, repo, number}` | all |
| `issue:N:closed` | `issue/N` at its current generation | `github.issue.closed` | `{kind: issue, …}` | all |
| `issue:N:<state>` | `issue/N` | `github.agent.turn` | `{kind: turn, …, state}` | from now |
| `until` | (none) | (none) | (none) | core timeout timer |

Behaviour:

- **Refusals.** The call checks each condition before any side effect and refuses one that has already happened (`already_met`: carry on), an unknown project, a past time or nothing to wait for. A race between that read and registration is closed by the subscription backscan.
- **Jobs.** `job:<name>` (05-02) is a FromNow dependency on the job's key in its schedule entry, over the `schedule.run.completed|failed|cancelled|handoff` Events the host announces for each finished run, so the job's next run wakes it ([job waits](../schedule-extension/#job-waits)). A job the scope's schedule entries do not declare refuses with `no_job`.
- **The reason.** It is posted on the issue with the hold marker, so people see what is awaited.
- **What wakes the Wait.** The Wait matches the Work's own keys with the alias type plus the own-key inputs that still reach a waiting agent: human comments, closes, merges and check results. A comment therefore wakes the agent.
- **One wake.** One wake ends a core Wait, so the agent waits again if the prerequisite is still pending. The playbook says so. Agent comments are excluded, because the wait's own reason comment is one.
- **Ending the turn.** `github.wait` releases authority, so it is the turn's last call. Not waiting is the cancel.

`issue:N:<state>` needs an Event: the projector derives `github.agent.turn` (`{repo, number, state}`) from each finished attempt whose outcome declares a `github.status.v1` status. Bootstrap accepts it as a trusted derived Event of that attempt's trigger, with delivery `turn/<execution>`, so a crash only repeats a deduplicated delivery. The `github` route never delivers it.

The renderer (`RenderEvent`, wired as the Claude Code `Options.Render`) uses this wording: `You were waiting for PR #50 to merge. That has happened: PR #50 was merged.` A timeout reads that the latest time has come. Comments carry their reply call (for example `call github.pr.reply {"number": 4, "comment_id": 77, …}`).

## Scheduled holds

A scope enables holds with a `github-hold` route ahead of its `github` route, bound to a `github-hold` binding (executor `github-hold`, toolbelt `sdk`, operations `emit`, `github.hold.status`, `github.hold`). The flow is:

1. **Claim.** The route claims an open issue carrying the `scheduled` label that otherwise passes admission (skip, require and claimed rules) as `github.issue.held` Work. The `github` route never claims scheduled issues.
2. **Hold.** The executor runs in process and calls the toolbelt with the attempt's own credential over the management socket. It reads the latest marker (body, then comments; a cancel marker drops it) and declares the translated hold with `github.hold`: dependency aliases plus a core timeout. The attempt then parks.
3. **Already met.** A condition that already happened is aliased from history at registration and wakes the hold at once. This is the same core path as every wait.
4. **Release.** When the hold ends, the executor emits `github.hold.released {why}` and hands off. The reason is a dependency met, the time come, a cancel marker, or "no marker", which is a scheduled task that starts now. The `github` route queues the release for the agent binding, so the first agent turn starts with text such as "This issue was held, waiting for PR #61 to merge. That has happened: …".
5. **Moved time.** A time-out re-reads the marker first, so an owner who moved the time is honoured.

Projections: held Work carries only `scheduled` (not claimed) until its first agent turn starts. Work parked on an agent's wait carries `claimed` and `scheduled` and no status. `scheduled` is a managed label.

## Checks timing

- **Checks without the Checks permission (03-06).** A fine-grained personal access token cannot be granted Checks, and GitHub answers its check-run reads with an ordinary 403. On the real sandbox that failed every reconcile once a PR existed. Now:
  - The client falls back to the head's GitHub Actions workflow runs (Actions: read).
  - When those are refused too, only commit statuses are evaluated, and a head with nothing readable is never reported as having no checks.
  - The `github.checks` tool returns state `unknown` with a note.
  - Doctor's `github_auth` reports `workflow_runs` as a note and `checks_unreadable` as a warning. A GitHub App covers check runs fully.
- **No checks.** `github.checks.none` is the no-checks event. An open PR head with no runs or statuses is observed as having no checks once its commit is older than `no_checks_minutes` (default 10; `-1` disables). The occurrence is the commit time plus the delay, so it is deterministic for push and reconcile. The route delivers it, and the renderer prints the "No checks ran" text. A failed commit read observes nothing rather than failing a scan.
- **Required checks (08-01).** GitHub already names the checks a PR needs, so there is no new setting. The checks the PR's base branch requires come from two sources:
  - the branch read's `protection.required_status_checks`, which needs no Administration permission;
  - the branch rules read (`rules/branches/{branch}`, rulesets).

  Both are conditional GETs under the client's rate-limit pause, and each evaluation memoizes them per base branch. When the evaluation would be a pass, the extension compares the head's runs and statuses with the required names:
  - **Something missing.** A head missing a required check is held: it is observed as `github.checks.pending` with `missing`, which is never delivered. Once `no_checks_minutes` has passed since the commit, it becomes `github.checks.missing`. Its occurrence is the later of the last run and the commit time plus the delay, so push and reconcile agree. The agent hears "Required checks never reported on PR #N at sha: …" with its advice, not "Checks passed".
  - **Nothing ran at all.** A head with no runs or statuses on a branch that requires checks reports `github.checks.missing` naming every required check, never "No checks ran".
  - **A failure too.** A failed head lists its missing checks as well ("These required checks never reported either").

  States stay deduplicated, with no debounce. The playbook gains the "Required checks never reported" line.
- **Degrading explicitly.** Required checks can be unreadable in three ways, and none of them fails a reconcile:
  - **A refused source.** GitHub can refuse a source with 403 or 404: a token without the permission, or rulesets on a plan that lacks them, as on a private GitHub Free repository. That source is skipped and recorded under `refused`. The `github.checks` tool's note says which sources were read. Doctor's `github_auth` names the required checks and their source. It notes refused rulesets and warns `required_checks_unreadable` when the protection read is refused.
  - **No check runs.** Required names are job names, while Actions workflow runs carry workflow names. So when check runs were refused (03-06), required checks are not verified at all, rather than every job looking missing. The note and doctor say so.
  - **Rate limits and failed reads.** These stay errors, so a missing requirement is never inferred from them.
- **No debounce.** There is no debounce. Deliveries are state-versioned aggregates, so repeated evaluations of one head state deduplicate. A check suite that starts after the others completed can still produce an early `passed`.

## Playbooks and setup

- **work-issue.** `extensions/github/playbooks/work-issue.md` carries the operational instructions, with tool calls using the operations above. The turn runs in the issue's workspace, and cleanup is host removal on completion. Waits follow the one-wake rule.
- **Setup guide.** `setup/setup.md` is the setup guide for Open Conduit's JSON config, `config validate|apply`, scopes, bindings, routes, the GitHub entry and the git resource. Its example config validates against the daemon's registry. The setup command (`init --with claude`) is delivered by 03-05, which also extends the guide with `init`, `config set`, `secret set`, webhooks, permissions, dry run and the TUI controls.
- **Plugin.** Bootstrap writes the plugin (`.claude-plugin/plugin.json`, `skills/<name>/SKILL.md`) to `<state>/playbooks/plugin` at every start. A `claude-code` binding without `plugin_dir` uses it.
- **Project additions.** Read from the turn's workspace at `.open-conduit/playbooks/<playbook>.md`. The additions are appended as "This project's additions to the playbook".

## Verification

```sh
go test -race ./extensions/github/... ./internal/cli ./executors/claudecode
go test -race -run 'TestTranslatedWait|TestGitHubTools|TestGitHubWait|TestGitHubAlready|TestGitHubHeld|TestGitHubCrossScope|TestGitHubNoChecks|TestGitHubRequiredChecks|TestGitHubToolWrites|TestSetupGuide|TestDaemonGitHub' ./integration
```

- **Translated waits.** [`translated_wait_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/translated_wait_test.go) runs translated waits through the production toolbelt and scheduler on SQLite, persistent and reopened. It covers an already-met dependency waking at once with alias lineage and no key transfer, `FromNow` ignoring history and then waking once, a filter selecting the named item, own-key input still waking, cross-scope refusal and authorization, and the timeout timer.
- **Tools and holds.** [`github_tools_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/github_tools_test.go) runs against the GitHub fake, through the real toolbelt, router, scheduler, projector and hold executor. It covers:
  - every tool, including retried comments, PRs, replies, uploads, notes and issues being written once, and authenticated fetch with no token leaving the host;
  - lost responses recovered by marker;
  - `TestWaitOnAnotherPRWakesTheAgentWhenItMerges`, `TestAlreadyMetWaitWakesAtOnce`, `TestHeldIssueStartsWhenAnotherAgentReports`, and a held issue starting at its time;
  - a cross-scope wait and no-checks.
- **Daemon.** [`TestDaemonGitHubHeldIssueAndTools`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_github_test.go) drives the composed daemon: config validation of the operations, the hold route, the in-process executor over the real socket, a timer release, and `os call github.issue.comment` posting once.
- **Unit and contract tests.** These cover condition and marker parsing, filters, routes, rendering and the registration, the playbooks naming only registered operations, plugin preparation, project additions, and the `os` client's file conventions.

## Limitations and follow-on work

- **Fixtures only.** No real GitHub write or real Claude Code session ran. Real-issue evidence of the tools, waits and holds is 03-06.
- **Not ported yet.** Job waits are delivered by 05-02. A hold condition nothing here can meet (an unknown job or project) leaves only the hold's time; with no time the issue starts at once and its first turn says why, rather than being held for ever. Sweep and triage consume `github.notes` since 05-03: the introspection extension reads these records through public storage as evidence and records the notes it handled in its own namespace, leaving `github.notes` untouched ([introspection](../issue-reporting/#evidence-aggregation)). The setup command and operator controls are delivered by 03-05 ([operator setup](../operator-setup/)).
- **Waits and injected comments.** One wake ends a wait, so the agent waits again. Since 03-06 an injected comment that claude echoed is consumed with its turn (`Result.ConsumedInputs`). Since 04-02 the proof also counts when the turn ends before the dispatch acknowledgement commits, and on waiting and failed turns, so an injected comment is never listed again once claude echoed it; the playbook's "may be listed again" caveat is removed.
