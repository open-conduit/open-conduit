---
title: "Engineering tools, waits and playbooks"
description: "The agent-facing GitHub workflow: tool operations, translated waits, scheduled holds, checks timing and the playbooks."
---

The engineering workflow is what an agent working a GitHub issue uses: GitHub tool operations on its toolbelt, waits on other issues, PRs and jobs, scheduled holds, and the playbooks that tell it how to work. It is built on public extension points. The pieces are:

- the GitHub tool operations ([`extensions/github/tools.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/tools.go), [`tools_wait.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/tools_wait.go)), registered through `extension.Registrar`;
- translated waits: a public `Operation.Wait` contract and the runtime that declares and installs them ([`internal/runtime/toolbelt_wait.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/toolbelt_wait.go), [`waits.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/waits.go));
- scheduled holds: the `github-hold` route and in-process executor ([`extensions/github/hold.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/hold.go));
- the playbooks ([`extensions/github/playbooks`](https://github.com/open-conduit/open-conduit/tree/main/extensions/github/playbooks)); the setup guide is the `setup` package's ([`playbooks`](https://github.com/open-conduit/extension-setup/tree/main/playbooks) in open-conduit/extension-setup, bundled with the release);
- composition in [`internal/bootstrap/tools.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/tools.go).

No domain condition, issue, PR or label type is in core. Core carries only generic, extension-neutral support: an operation kind that translates to a wait, opaque dependency filters, a request identity for handlers, and authored refusals.

## Contract additions (B03/B04)

| Addition | Meaning |
| --- | --- |
| `extension.Operation.Wait` | Set instead of `Handler`. It returns a `WaitPlan`: a `WaitRequest` on the current Work's own keys (types plus an optional timeout), `Dependencies` and an `Output`. The toolbelt runs it outside storage after the fenced authority check, then declares the request exactly like the builtin `wait`: same fence, watermark and immediate authority release, applied by the trusted host handshake when the segment finishes or suspends. The reference toolbelt double supports pure handlers only. |
| `extension.Dependency` | A B04 authorized dependency subscription: `SourceKey` (another Work's correlation, or one nobody owns yet), `Types`, `AliasType`, an opaque `Filter`, a short `Label`, an optional `SourceScopeID` and `FromNow`. `FromNow` scans only Events accepted after the declaration (an occurrence such as "the next turn ends"); otherwise all accepted history counts, so an already-met state wakes the wait at once. |
| `Registration.DependencyFilter` | A pure function that decides whether a type-matching source Event satisfies a dependency's opaque filter (for example, one PR among several keyed to the same issue). Core calls it inside the reconcile transaction through `SchedulerOptions.AliasFilter` (`Toolbelt.DependencyFilter`), keyed by the registration name, which is the subscription namespace. |
| `extension.CallRequestID(ctx)` | The caller's toolbelt request ID, so handlers derive stable intent identities for non-re-derivable writes. |
| `extension.OperationError` | A refusal the handler authors: a stable code and a short, secret-free message the agent can act on ("PR #12 is already merged: no need to wait, carry on now"). The management API returns it as `422` with that code and message instead of a generic failure. |

Runtime (B04): a translated wait's dependencies are stored in the toolbelt declaration and persisted as pending subscriptions *in the wait-registration transaction*. If an own-key match already woke the wait, nothing is pending. They are then installed through `Scheduler.Subscribe`, which authorizes outside storage, revalidates scopes and backscans source history. Installation runs best-effort right after `Complete`, and durably on every `ReconcileSubscriptions` pass, so a crash between registration and installation only delays it. Refused installations are recorded and not retried. `SubscriptionRequest` carries `After` (the source scan start), `Filter` and `Label`. The alias payload (`core.subscription.alias.v1`) carries `subscription`, `event`, `source_type` and `label`: lineage, never the provider body. A cross-scope dependency is checked at declaration (`ToolbeltOptions.AuthorizeAlias`, which refuses the call and keeps authority) and again at installation and reconcile (`SchedulerOptions.AuthorizeAlias`). Bootstrap answers both from B05: the waiting scope must list the source scope in `authorization.read_scopes`.

## Tool operations

An agent reaches an operation as MCP `call {operation, input}` or `os call <operation> --data <json>`. The MCP `call` tool sends its `input` object as the payload's data; an explicit `{schema, data}` payload still passes through. Every handler reads the Work it serves from durable state, finds the issue it owns (`issue/N` in a registered namespace) and acts on that issue and its repository. With a shared tracker, PR operations use the code repository whose entry names the tracker.

`github.pr.view`, `github.pr.comment`, `github.pr.reply`, `github.pr.review` and `github.checks` also act on a PR the Work doesn't own, so Work started elsewhere (a Slack request to review a PR, a job) can read and answer one:

- They take an optional `repo`. Without it they use the Work's own code repository, or the scope's only registered repository when the Work owns no issue. A scope with several refuses `repo_required` and lists them, and a repository the scope doesn't register is refused `unregistered_repo`.
- Posts are the same marked durable intents, carrying the calling Work's marker, so they are exactly-once, recovered by marker and never handed back to the caller. The PR's key is never attached to the calling Work: the PR stays keyed to its tracker issue, and the Work that owns it hears the posts as another agent's comments.
- `github.checks` needs `number` outside the Work's own issue. View's reply calls carry `repo` there.
- `github.wait` also binds Work without an issue: see below.
- `github.issue.list`, `.view`, `.comment` and `.create` bind any Work in the scope, so Work without an issue (a triage, a scheduled job, a Slack thread) can find and file issues. Without `project` they act on the Work's own tracker, or the scope's first registered tracker when the Work owns no issue (`no_repository` when it registers none). `view` and `comment` then need `number` (`number_required`), and view's reply calls carry it.
- A PR's [own Work](../github-extension/#pull-request-work) can also read links in its PR's repository with `github.attachment.get`. A reviewer can then read the issue its PR closes with `github.issue.view`.
- Every other operation still needs the Work's own issue and refuses `no_issue` without one.

`github.issue.list`, `.view`, `.comment` and `.create` take an optional `project` naming another scope by ID or name, as a wait condition's `<project>/` does, so an agent can find and file issues where the work belongs without knowing its repository or connection. The call acts on that scope's first registered issue tracker, reading and writing on its entry's connection (a binding's own `connection` is granted in the calling scope, so it isn't used there). Creating or commenting needs the scope in the caller's `authorization.file_scopes` (`ToolsOptions.MayFile`, answered by `config.Snapshot.AuthorizeFile`); listing or viewing needs it in `file_scopes` or `read_scopes`. A refusal is `not_allowed` and names the grant to add; an unknown scope or one with no tracker is `no_project`. `view` and `comment` need `number` there (`number_required`), and `create` refuses `wait`/`until` (`hold_unsupported`): the new issue is routed by that scope's own routes. Writes are the same marked intents, held by the target entry's projector, so a retry opens or posts once.

| Operation | Notes |
| --- | --- |
| `github.context` | repo, tracker, issue and URL, branch (`agent/issue-N` when the branch pattern allows it), base branch (`base_branch` or the repository default). For a PR's [own Work](../github-extension/#pull-request-work): repo, `pr` and `pr_url`. |
| `github.issue.view` / `.list` | Each comment carries its author, agent marker and the reply call. |
| `github.issue.comment` | Posted through `Projector.Deliver` as a durable intent `call/<execution>/<request id>` (or the digest of the input when no request ID is given), carrying a marker, so the Work never hears its own note. A lost response returns `pending`. Converge finishes it by marker search, and the comment is never posted twice. `number` reaches another agent's issue. |
| `github.issue.create` | Follow-up child work as a marked, intent-backed issue: an ambiguous create is found again by its marker. With `wait`/`until`, the conditions are validated and the hold marker and `scheduled` label are added. |
| `github.pr.list` / `.view` / `.create` | Create adds `Closes #N` (or `owner/repo#N`), or `Refs #N` when the input sets `closes_issue: false` (every PR but the last of a multi-PR issue, so its merge leaves the issue open), unless the body already names the issue with a closing or reference keyword. It uses GitHub's one-open-PR-per-head rule for idempotency: a retried or lost create returns the open PR. View includes inline comments and reviews with reply calls. |
| `github.pr.comment` / `.reply` | Durable intents. A reply records `ReplyTo` and recovers by searching the PR's review comments. |
| `github.pr.review` | `{number, event, body, comments?, repo?}` submits one review: `event` is `approve`, `request_changes` or `comment`, and `comments` are inline `{path, line, side?, body}` on lines of the diff (`side` `RIGHT`, the default, or `LEFT`). It is a durable intent like a comment, recovered by searching the PR's reviews for its marker, so a retry or a lost response submits one review. The review and each inline comment carry the Work's marker, so neither the review nor GitHub's thread around an inline comment is handed back as a person's news. `body` may be empty only for an approval. GitHub's refusals post nothing and are not redone: `own_pull_request` (see below), `github_unprocessable` (a line outside the diff, or a closed PR) and `github_forbidden` (the connection lacks Pull requests write). |
| `github.checks` | The PR head's evaluated runs, failures and preview, using the ingress evaluation. It also returns the base branch's `required` checks with their sources, any `missing` ones, the states `waiting on required checks` and `required checks missing`, and a note when a source was refused. |
| `github.attachment.upload` | Committed to the orphan `open-conduit-attachments` branch at `<issue>/<intent digest>-<name>`, so a retry keeps one file. |
| `github.attachment.get` | Signed-in fetch, only for this host, its API host or `*.githubusercontent.com`. A redirect that leaves the host drops `Authorization`, and foreign links are refused without a request. The result uses the generic `open-conduit.toolbelt.file.v1` schema; the `os` client writes it to `save_to` in the agent's own directory. A `file_path` input is read and sent as `content_base64` the same way. |
| `github.wait` | See below. |
| `github.hold.status` / `github.hold` | Used by the hold executor. |
| `github.git.credential` | `{protocol, host, path?}`, git's credential request. It sits behind `os git-credential`, the git credential helper a container launch sets ([Sandboxes](../sandboxes/#claude-code-in-a-container)), and that the GitHub extension's run environment layer sets for every run whose binding's GitHub connection (its own, else the scope's) has `git` `bot` ([GitHub extension](../github-extension/#git-as-the-bot)); agents never call it. On a GitHub App connection it returns `{username, password, expires_at}`: an installation token minted for the Work's code repository alone (or the scope's only one), with `contents: write`, from the bot App when there is one, of the calling binding's own `github` connection (`Authority.Connection`) when it names one, else of the entry's (one that can't be used refuses `github_writer`). It answers only for `https` on the connection's web host and that repository's path, and it reuses the execution's token while more than five minutes remain. Any other URL gets `{}`. A token connection refuses `github_static_token`, and a call with a request ID refuses `request_id_refused`, so the token is never kept for replay. The host revokes an execution's tokens when its segment ends (`RevokeGitTokens`) and sweeps those of executions that aren't running (`SweepGitTokens`). |

Dry-run is handled per entry by `projection.dry_run`: writes report `dry_run` and make no change.

**Whose review it is.** GitHub records a review as the connection's writer: the bot App (`bot_app_id`) when there is one, otherwise the token's account. It is never the person who asked for the review or approved the decision to submit it. Two consequences follow:

- GitHub doesn't let the account that opened a PR approve it or request changes on it. Agents open their PRs as the same connection, so on an agent's own PR the operation refuses `own_pull_request` and a `comment` review is the only kind it can submit, unless the reviewing binding names a connection of its own (below).
- An approval counts toward branch protection's required reviews only when the writer is an identity whose approval that rule accepts: an account with write access, a code owner where code-owner review is required, and not the account that pushed last where that is excluded. Where protection needs a particular person's review, the agent's approval doesn't satisfy it, and a person still has to approve.

**A reviewer's own writer.** A binding whose `connection` is another `github` connection, such as `"connection": "github-reviewer"`, writes as it. That binding's comments, review-thread replies, wait notes and reviews (everything that goes through `Projector.Deliver`) then go out as that connection's writer, typically a second bot App, so its reviews aren't the PR author's and it can approve and request changes on PRs the builder's binding opened. Everything else stays on the entry's connection: reads, ingest, label projection, `github.issue.create` and `github.pr.create`. A binding that names no connection, the entry's own, or one of another type (a `slack` connection) writes as the entry's connection. This uses only the host's generic surface for acting as a binding's connection ([`Authority.Connection`](../extension-protocol/), `config.Snapshot.ExtensionConnection`), so an outside extension could do the same.

- **Which writer.** The toolbelt reads the connection pinned on the calling execution's authority (`Authority.Connection`), and `gh.Writers` builds and keeps a client for it from the config and secrets the host resolves (`ToolsOptions.Writers`). The connection is recorded on the intent (`Intent.Writer`), so a retry, a recovery by marker search and a `Converge` that finishes the delivery later all post as the same writer. Marker searches read with the entry's connection, since any identity that can read the PR finds the marker.
- **Same GitHub.** The connection must have the same `api_url` as the entry's, since its writes land on the entry's repository. That, an invalid config, or a connection the host no longer supplies (removed since the attempt started) fails the intent with `writer_unavailable`, and the tool refuses `github_writer`. Config check validates the binding's `connection` as for any binding: defined and granted by the scope authorization.
- **Doctor.** Under `check_secrets`, `github_auth` also verifies each other `github` connection a binding in the entry's scope names, as it verifies a bot writer: signed in as that connection, it reads the entry's repository (`Client.Verify`). The item reads `binding connection <id>: bot writer authenticated`, or a failure code.
- **Approvals.** Whether the second App's approval satisfies required reviews depends on the rule, as above. GitHub doesn't document App approvals explicitly; reports are that an App installed with Pull requests write counts toward a plain required-approvals count. It can't be a code owner, so where code-owner review is required a person still has to approve. Open Conduit's own `main` requires no approving reviews, only checks.

## Waits

`github.wait {conditions, until, reason}` takes `pr:N[:merged|closed|commented|pushed|checks]`, `issue:N[:closed|<turn state>]`, `job:<name>`, `<project>/` prefixes and `until`. It translates as follows:

| Condition | Source key | Types | Filter | History |
| --- | --- | --- | --- | --- |
| `pr:N:merged`, `pr:N:closed` | the PR's correlation (its issue's, by branch or closing keyword, else `pr/N`) | `merged`, `closed` (either ends the wait) | `{kind: pr, repo, number}` | all |
| `pr:N:commented` | the PR's correlation | `github.comment.human`, `github.comment.agent` | `{kind: pr_comment, …, work}`: a new (not edited) PR conversation comment, review or review comment, not marked as the waiting Work's own | from now |
| `pr:N:pushed` | the PR's correlation | `github.pull_request.open` | `{kind: pr_push, …, head}`: a head SHA other than the one read at registration; refused on a closed PR | from now |
| `pr:N:checks` | the PR's correlation | `github.checks.passed`, `failed`, `none`, `missing`, and `github.pull_request.open`, `merged`, `closed` | `{kind: pr_checks, …, head}`: a result for the head read at registration, a push of another head, or a merge or close (the wake says which); refused on a closed PR, and refused as already met when that head's checks have finished (a hold keeps it, met from history by that head's result) | from now while the checks are pending, so an earlier run's result never counts |
| `issue:N:closed` | `issue/N` at its current generation | `github.issue.closed` | `{kind: issue, …}` | all |
| `issue:N:<state>` | `issue/N` | `core.execution.turn` | `{kind: turn, …, state}`: the Event's `status` | from now |
| `until` | (none) | (none) | (none) | core timeout timer |

Behaviour:

- **Refusals.** The call checks each condition before any side effect and refuses one that has already happened (`already_met`: carry on), an unknown project, a project this scope may not read (`not_allowed`, saying to add it to `authorization.read_scopes`; `ToolsOptions.MayRead`, answered as `AuthorizeAlias` is), a past time or nothing to wait for. A race between that read and registration is closed by the subscription backscan.
- **Jobs.** `job:<name>` is a FromNow dependency on the job's key in its schedule entry, over the `schedule.run.completed|failed|cancelled|handoff` Events the host announces for each finished run, so the job's next run wakes it ([job waits](../schedule-extension/#job-waits)). A job the scope's schedule entries do not declare refuses with `no_job`.
- **Work without an issue.** `github.wait` binds any Work in the scope and takes an optional `repo`, resolved like the PR operations' (`repo_required`, `unregistered_repo`). Conditions without a project name that repository. Holds stay issue-bound.
- **Wait description.** The plan describes the Wait in the condition's words (`checks on PR #137`, `PR #12 to merge, or 25 Oct 2026 09:00 UTC`, `until …` for a time alone), which operator views show from the Wait record ([wait descriptions](../extension-protocol/#work-display-and-wait-descriptions)).
- **The reason.** It is posted on the issue with the hold marker, so people see what is awaited. Work without an issue has nowhere on GitHub to put it: the call returns it, and the agent tells its own conversation (a Slack thread) if people should know. Nothing goes on the PR.
- **What wakes the Wait.** The Wait matches the Work's own keys with the alias type plus the own-key inputs that still reach a waiting agent: human comments, closes, merges and check results. A comment therefore wakes the agent. Work without an issue matches only the alias type (and its time), since GitHub can't tell its own surface's input from its own posts.
- **Pull request Work.** A PR's [own Work](../github-extension/#pull-request-work) may wait with no condition and no time: its Wait matches its PR key's own-key inputs (as above: human comments, merges and closes, conflicts and check results) and new versions of the open PR (a push), plus the peer dependency for other agents' comments. Conditions and `until` add to that. Nothing is posted; the description names the PR.
- **One wake.** One wake ends a core Wait, so the agent waits again if the prerequisite is still pending. The playbook says so. Agent comments are excluded, because the wait's own reason comment is one.
- **Injected comments.** A comment injected into a running turn that claude echoed is consumed with its turn (`Result.ConsumedInputs`), on every outcome and even when the turn ends before the dispatch acknowledgement commits, so it is never listed again ([Claude Code executor](../claude-code-executor/#prompt-inputs-and-injection)).
- **Unmeetable holds.** A hold condition nothing here can meet (an unknown job or project, or a project this scope may not read) leaves only the hold's time; with no time the issue starts at once and its first turn says why, with the condition's refusal (how to allow it, for an unreadable project), rather than being held for ever or failing and retrying a wait that can't register until the config changes. A hold that parks on its time alone says in its outcome which conditions it can't wait on and why.
- **Ending the turn.** `github.wait` releases authority, so it is the turn's last call. Not waiting is the cancel.

`issue:N:<state>` waits on the host's turn-ended Event (`core.execution.turn`, [execution scheduling](../execution-scheduling/)), which every turn declaring `core.turn.v1` publishes on its Work's keys, whatever source the Work came from. The issue's key selects the issue and the filter the state. The `github` route never delivers it. A wait registered on the `github.agent.turn` Event of earlier releases is not met by the new Event: give it an `until`, or wait again after upgrading.

The renderer (`RenderEvent`, wired as the Claude Code `Options.Render`) uses this wording: `You were waiting for PR #50 to merge. That has happened: PR #50 was merged.` A timeout reads that the latest time has come. Comments carry their reply call (for example `call github.pr.reply {"number": 4, "comment_id": 77, …}`).

## Scheduled holds

A scope enables holds with a `github-hold` route ahead of its `github` route, bound to a `github-hold` binding (executor `github-hold`, toolbelt `sdk`, operations `emit`, `github.hold.status`, `github.hold`). The flow is:

1. **Claim.** The route claims an open issue carrying the `scheduled` label that otherwise passes admission (skip, require and claimed rules) as `github.issue.held` Work. The `github` route never claims scheduled issues.
2. **Hold.** The executor runs in process and calls the toolbelt with the attempt's own credential over the attempt's own toolbelt endpoint. It reads the latest marker (body, then comments; a cancel marker drops it) and declares the translated hold with `github.hold`: dependency aliases plus a core timeout. The attempt then parks.
3. **Already met.** A condition that already happened is aliased from history at registration and wakes the hold at once. This is the same core path as every wait.
4. **Release.** When the hold ends, the executor emits `github.hold.released {why}` and hands off. The reason is a dependency met, the time come, a cancel marker, or "no marker", which is a scheduled task that starts now. The `github` route queues the release for the agent binding, so the first agent turn starts with text such as "This issue was held, waiting for PR #61 to merge. That has happened: …".
5. **Moved time.** A time-out re-reads the marker first, so an owner who moved the time is honoured.
6. **Unscheduled.** The owner taking the `scheduled` label off releases the hold at once, whatever the marker says. The hold carries one more dependency, on the issue's own key: the next `github.issue.open` seen without the label (filter `unscheduled`). The hold status also reports `unscheduled` when the label is already gone, so a hold read after the owner took it off (the first attempt, a time-out) starts too. The first turn reads "The scheduled label was taken off this issue, so it starts now."

Projections: held Work carries only `scheduled` (not claimed) until its first agent turn starts. The projection never adds `scheduled` back to held Work: taking it off is how the owner starts the issue. Work parked on an agent's wait carries `claimed` and `scheduled` and no status. `scheduled` is a managed label.

## Checks timing

- **Checks without the Checks permission.** A fine-grained personal access token cannot be granted Checks, and GitHub answers its check-run reads with an ordinary 403. Rather than fail every reconcile once a PR exists:
  - The client falls back to the head's GitHub Actions workflow runs (Actions: read).
  - When those are refused too, only commit statuses are evaluated, and a head with nothing readable is never reported as having no checks.
  - The `github.checks` tool returns state `unknown` with a note.
  - Doctor's `github_auth` reports `workflow_runs` as a note and `checks_unreadable` as a warning. A GitHub App covers check runs fully.
- **No checks.** `github.checks.none` is the no-checks event. An open PR head with no runs or statuses is observed as having no checks once its commit is older than `no_checks_minutes` (default 10; `-1` disables). The occurrence is the commit time plus the delay, so it is deterministic for push and reconcile. The route delivers it, and the renderer prints the "No checks ran" text. A failed commit read observes nothing rather than failing a scan.
- **Required checks.** GitHub already names the checks a PR needs, so there is no new setting. The checks the PR's base branch requires come from two sources:
  - the branch read's `protection.required_status_checks`, which needs no Administration permission;
  - the branch rules read (`rules/branches/{branch}`, rulesets).

  Both are conditional GETs under the client's rate-limit pause, and each evaluation memoizes them per base branch. When the evaluation would be a pass, the extension compares the head's runs and statuses with the required names:
  - **Something missing.** A head missing a required check is held: it is observed as `github.checks.pending` with `missing`, which is never delivered. Once `no_checks_minutes` has passed since the commit, it becomes `github.checks.missing`. Its occurrence is the later of the last run and the commit time plus the delay, so push and reconcile agree. The agent hears "Required checks never reported on PR #N at sha: …" with its advice, not "Checks passed".
  - **Nothing ran at all.** A head with no runs or statuses on a branch that requires checks reports `github.checks.missing` naming every required check, never "No checks ran".
  - **A failure too.** A failed head lists its missing checks as well ("These required checks never reported either").

  States stay deduplicated, with no debounce. The playbook gains the "Required checks never reported" line.
- **Degrading explicitly.** Required checks can be unreadable in three ways, and none of them fails a reconcile:
  - **A refused source.** GitHub can refuse a source with 403 or 404: a token without the permission, or rulesets on a plan that lacks them, as on a private GitHub Free repository. That source is skipped and recorded under `refused`. The `github.checks` tool's note says which sources were read. Doctor's `github_auth` names the required checks and their source. It notes refused rulesets and warns `required_checks_unreadable` when the protection read is refused.
  - **No check runs.** Required names are job names, while Actions workflow runs carry workflow names. So when check runs are refused, required checks are not verified at all, rather than every job looking missing. The note and doctor say so.
  - **Rate limits and failed reads.** These stay errors, so a missing requirement is never inferred from them.
- **No debounce.** There is no debounce. Deliveries are state-versioned aggregates, so repeated evaluations of one head state deduplicate. A check suite that starts after the others completed can still produce an early `passed`.

## Playbooks and setup

- **work-issue.** `extensions/github/playbooks/work-issue.md` carries the operational instructions, with tool calls using the operations above. The turn runs in the issue's workspace, and cleanup is host removal on completion. Waits follow the one-wake rule. The playbook covers what the GitHub extension owns: its operations, its events, waits, turns and end states, and how GitHub renders `#<number>`. Operations other extensions add (`introspection.correction`, `introspection.problem`, `introspection.report-issue`, `human.ask`) come with their own guidance through the toolbelt's MCP instructions, served only when the binding grants them, so the playbook only points at them (issue #208). It leaves house workflow to the project: how to sort issues, start and report comments, what to do with a preview, and when to comment on another agent's issue. This repository keeps its own in `.open-conduit/playbooks/work-issue.md` (issue #193).
- **Setup guide.** `playbooks/setup.md` in the [setup package](https://github.com/open-conduit/extension-setup) is the setup guide for Open Conduit's JSON config, `config validate|apply`, scopes, bindings, routes, the GitHub entry and the git resource. It also covers `init`, `config set`, `secret set`, webhooks, permissions, dry run and the TUI controls. It opens with a plain-language picture of the tool and defines each term (scope, binding, route, hold, reconcile) where a question first needs it. It covers a daemon that has run before (the stored generation, adopting a valid config file when the stored generation no longer validates, never deleting the state directory), the `human` and `introspection` extensions, pinning the GitHub account `cmd:` resolves to with a login check that can't print a token, and checking for unfinished Work before removing a scope or renaming an entry. Its example config validates against the daemon's registry. `init --with claude` starts Claude Code with the guide ([operator setup](../operator-setup/#setup-from-nothing)). The docs site also publishes it as plain Markdown at `/setup-guide.md` (`site/src/pages/setup-guide.md.ts`, without its browser-session section), which [Point your agent here](/open-conduit/start/agents/) hands to any coding agent the owner already uses.
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

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **03-04** delivered the tool operations, translated waits and their contract additions (B03/B04 clarifications), scheduled holds, `github.checks.none`, the playbooks, the plugin and the setup guide. No real GitHub write or real Claude Code session ran; real-issue evidence of the tools, waits and holds was left to 03-06. Job waits were not yet available, so a `job:` condition could only be met by the hold's time. The setup command and operator controls were left to 03-05, and the `github.notes` sweep and triage to stage 05.
- **03-05** delivered `init --with claude` and extended the setup guide with `init`, `config set`, `secret set`, webhooks, permissions, dry run and the TUI controls.
- **03-06** added the Actions workflow-run and commit-status fallback for checks after a fine-grained token, refused check runs, failed every reconcile on the real sandbox once a PR existed. It also made an injected comment that claude echoed consumed with its turn.
- **04-02** extended that consumption proof to turns that end before the dispatch acknowledgement commits and to waiting and failed turns. The playbook's "may be listed again" caveat was removed.
- **05-02** delivered `job:<name>` waits.
- **05-03** made sweep and triage consume `github.notes` through the introspection extension. Issue #70 removed `github.correction`, `github.problem` and `github.notes`: agents log notes with `introspection.correction` and `introspection.problem`.
- **08-01** added required checks: `github.checks.missing`, the `required` and `missing` fields of `github.checks`, the degraded-source handling and the playbook's "Required checks never reported" line.
