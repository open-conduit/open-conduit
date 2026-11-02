---
title: "Scheduled jobs: definitions, catch-up, the one-pending-run rule and job runs"
---

Scheduled jobs run on the five primitives. A timed job is a configured independent root on a durable core timer; every trigger is an accepted Event; the `schedule` route makes it input of the job's one standing Work; the scheduler runs it as an ordinary Execution through the job's binding. Nothing schedules work outside event → route → Work → Execution. The extension is [`extensions/schedule`](https://github.com/open-conduit/open-conduit/tree/main/extensions/schedule); [`internal/bootstrap/schedule.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/schedule.go) composes it. How a job runs (fresh checkout, detached runs, env and secret precedence, payload artifacts, timeouts, kept runs, the job history view) is covered under [Job runs](#job-runs).

## Configuration

A scope's `schedule` extension entry declares its jobs. Run settings are not here: what a job runs is the binding of the `schedule` route that selects it.

```json
{"name": "jobs", "type": "schedule", "secret_names": ["hook"], "config": {
  "namespace": "schedule/eng", "scope": "eng", "webhook_secret": "hook",
  "jobs": [
    {"name": "report", "cron": "0 9,13,18 * * *", "time_zone": "Europe/London"},
    {"name": "refresh", "at": ["05:17"]},
    {"name": "tick", "every_minutes": 60},
    {"name": "post", "on": {"events": ["push"], "branches": ["main"], "paths": ["data/reels/*.json", "!data/reels/posted.json"]}},
    {"name": "red-main", "on": {"events": ["check_suite.completed"], "branches": ["main"], "conclusions": ["failure", "timed_out"]}},
    {"name": "poke", "on": {"webhook": true}},
    {"name": "by-hand"}
  ]}}
```

| Field | Meaning |
| --- | --- |
| `namespace` | Stable source namespace of the entry's triggers and job keys. A job's key is `job/<name>` in it. |
| `scope` | The containing scope (ingress target rule). |
| `webhook_secret` | A selected secret name; it signs the job webhook URLs. Required when a job takes webhooks. |
| `jobs[].name` | `[a-z][a-z0-9_-]{0,62}`. |
| `every_minutes` | An interval job. Not with `at`/`cron`. |
| `at` | UTC times of day, `HH:MM`. Not with `cron`. |
| `cron`, `time_zone` | A standard five-field cron (`robfig/cron/v3`) read in an IANA zone, UTC when empty. The zone goes in `time_zone`, never a `TZ=` prefix; `time_zone` is only for `cron`. |
| `on.events` | Code-host event names, optionally with an action (`push`, `release.published`). |
| `on.branches` | Branch globs for ref-carrying events. A check suite or run carries its head branch. |
| `on.paths` | GitHub Actions path rules for pushes (`*` within a folder, `**` across, `!` takes back). |
| `on.conclusions` | Conclusions a finished check suite or run must end in (`failure`, `timed_out`, `cancelled`, `action_required`, …), case-insensitive. Other events carry none, so they never match a job that lists any. |
| `on.webhook` | Custom POSTs to the job's signed URL. |

A job with no timing and no `on` is manual only. Validation never echoes values.

Routes use one registered handler, `schedule`. Its route config `{"jobs": [...], "triggers": [...]}` selects jobs and trigger kinds (`timer`, `webhook`, `event`, `manual`); empty selects every job of every schedule entry in the scope, and every kind. One route per binding gives each job its own command:

```json
{"name": "jobs-report", "handler": "schedule", "binding": "report-cmd", "active_policy": "queue", "config": {"jobs": ["report"]}}
```

Two generic contract pieces make this possible. `extension.RouteInput.Route` carries the evaluated route, including its opaque handler config. `config.Registry.HandlerConfig` lets a registered handler validate that config at load/apply time. The extension also describes its config for the operator: each job and when it runs (`every hour; on push to main`), and the jobs and triggers each `schedule` route takes, which the web view's Config page shows. Put a `schedule` route before a `manual` route in the same scope, or the manual route would create `manual.work` for a job key.

**Repository-scoped declarations.** The repository's scope declares its jobs. B05 requires an explicit apply before disk edits change behavior. Job reload is therefore config reload.

## Triggers

| Trigger | Event | Producer and identity |
| --- | --- | --- |
| Timed slot (every, at, cron) | `schedule.slot` | A core timer, as a configured independent root. Delivery `slot/<job>/<UTC instant>`. |
| Webhook POST | `schedule.webhook` | The entry's one-shot ingress. Delivery `webhook/<job>/<id>`: the sender's `Idempotency-Key` (or `X-Request-Id`), else a fresh ID per POST. |
| Code-host delivery | `schedule.event` | Derived from the accepted raw delivery (`github.delivery`), which is its causal parent. Delivery `event/<job>/<parent Event>`. |
| Manual | any non-`core.*` type, source `core.manual` | The operator: `open-conduit events emit --scope-id S --type schedule.run --delivery-id D --key-source <namespace> --key-external-id job/<name>`. |

Every trigger carries the `schedule.trigger.v1` payload: job, kind, run label (`cron 2026-10-25 01:30 Europe/London`, `at 2026-10-21 05:17 UTC`, `every 60m`, `webhook`, `push abc1234`, `check_suite.completed abc1234 failure`), the slot, and the delivery fields (event, action, ref, commit, conclusion, changed files) or the webhook body. An agent job's turn reads the trigger rendered by `schedule.RenderEvent`: the job and label as its heading, then the event, branch, commit, conclusion and up to 20 changed files, and a webhook body clipped to 2000 bytes.

**Webhooks.** The webhook listener serves `POST /schedule/<scope>/<entry>/<job>/<token>`. The token is an HMAC of the entry's webhook secret over namespace and job, so it is stable across restarts and is revoked by rotating the secret. A wrong token, an unknown job or a job without webhooks gets 404. The body is limited to 1 MiB, and the response is 202 only after durable acceptance. `doctor --check-secrets` prints each signed URL (`schedule_webhooks`); plain doctor lists the paths with `<token>`.

**Code-host events.** The GitHub ingress records every verified delivery as a `github.delivery` Event (`github.delivery.v1`: event, action, ref, before, after, conclusion, changed files). A push names its ref and the commit it moved to; a finished `check_suite` or `check_run` names its head branch as the ref, its head commit as `after` and its conclusion, so a job can run when a branch's checks fail ([fix red CI on the default branch](../../workflows/red-ci/)). The managed repository webhook subscribes to check suites and runs, but not to pushes, releases or workflow runs: a job on those needs them added to the webhook. The host reads accepted `github.delivery` Events after a durable cursor. For each job in the delivery's scope whose `on` matches (branches and paths), it accepts a derived `schedule.event` in one transaction with the cursor. A push whose changed files are unknown runs anyway; a deleted branch changes nothing. Path matching uses the delivery's commit file lists. The extension reads no repository, and only the Git provider touches a checkout. A list that may be truncated (the GitHub ingress cap of 1000 files, or GitHub's own cap of 2048 commits, recorded as `DeliveryPayload.Commits`) counts as unknown, so the job runs rather than skip on a partial list.

## Timers, catch-up and clock changes

Timed jobs use a core timer with a calendar recurrence. `runtime.TimerOptions.Recurrences` registers pure `RecurrenceFunc` policies by kind. `Timers.ArmRecurrence` stores the opaque spec with the timer. Inside the firing transaction core asks the policy which occurrence this firing publishes (or none) and when the timer fires next. Acceptance, the firing ledger and the rearm commit together, so a crash never separates them; replaying a firing returns its original result. A substituted or skipped firing never accepts the template. Core keeps no calendar knowledge.

The introspection extension registers a second recurrence kind (`introspection.v1`) for its daily sweep and triage, with the same calendar rules. It is not a `schedule` entry, because the sweep and triage are Work kinds the introspection extension defines ([introspection](../issue-reporting/#the-sweep-and-triage-schedules)). Bootstrap merges both kinds into `TimerOptions.Recurrences` and both standing Work policies into `SchedulerOptions.WorkKinds`.

The schedule policy (`schedule.Recurrence`) decides catch-up:

- **At and cron.** A firing publishes the latest slot at or before now and strictly within six hours of it (`now − slot < 6h`), unless that slot's wall-clock name is the one that ran last. Exactly six hours late, or more, is skipped. Older missed slots are always skipped. The timer rearms for the next slot after now whose name differs from the one that ran last.
- **Clock changes.** A wall-clock time the clocks skip (01:30 London on the last Sunday in March) has no slot. A repeated time (01:30 on the last Sunday in October) has two instants and one name, so a daily job runs once. A schedule within the hour, such as `*/30`, still runs at repeated times whose name differs from the one just run. Slot identity is the instant, which is stable across restarts; the name is the wall-clock slot.
- **Every N minutes.** A firing publishes its own occurrence and rearms N minutes after it, or N minutes after now when that is already past, so an outage yields one run rather than a burst. The durable timer runs an interval job only when due, not at each daemon start.

`schedule.Ensure` reconciles timers with each committed generation: at startup, on every apply, and every minute as a repair pass:

- A new timed job waits for its next slot rather than run one someone may already have run. A new interval job runs at once.
- An edited timing rearms. An interval job runs at once. A timed job runs its latest slot within six hours if that slot is not the one that ran last, else it waits. Editing a job's other triggers does not rearm it.
- A removed job or entry loses its timer, while a run already going carries on. Its late triggers match no route (`unmatched`), so they never run.
- A disabled scope's timers stay durable and untouched (B05), and their due Events resume on re-enable.
- An unchanged timer is left alone across restarts, so a missed slot is caught up by the timer's own firing.

## The one-pending-run rule (coalescing decision)

One pending rerun is permitted while a job runs: at most one waits, a delivery takes the place of a waiting timer request, the newest delivery takes the place of older ones, and a later timer never displaces a waiting request.

**Decision: no core coalesce option and no lossy queue.** B04's queue stays lossless and ordered. The rule falls out of three generic pieces:

1. **One standing Work per job.** The route creates `schedule.job` Work owning `job/<name>` for the first trigger. One nonterminal attempt per Work means one run at a time per job. Later triggers queue on that Work, so the job's run history is that Work's Executions.
2. **The next attempt takes the whole batch.** `Scheduler.Start` already gives one attempt every pending input (`RunRequest.Inputs`), so however many triggers arrive while a run goes, exactly one run is pending. Nothing is dropped from history: each trigger is consumed by that one attempt.
3. **The extension picks the batch's trigger.** `runtime.SchedulerOptions.WorkKinds` registers a lifecycle policy per Work kind. The job kind's `Trigger` function, `schedule.SelectTrigger`, chooses the newest delivery (webhook or code-host event), else the earliest request. That choice becomes `Execution.TriggerID` and `RunRequest.Trigger`. A wake winner still takes precedence.

The same policy makes job Work **standing**. A completed attempt leaves it ready and idle instead of completing it. A failed attempt arms no automatic retry: the next trigger is its retry. Only cancellation ends job Work. A cancelled job Work is terminal, and its later triggers are audited as `terminal`. Jobs do not adopt the B04 failure budget, but an executor's retry hint applies to standing Work too; see [Retry](#retry).

Contract tests cover all of these orders:

- the batch orders of a delivery while a job runs and a timer run while one is going (`TestSelectTrigger`, `TestScheduleTriggersCoalesce`);
- the default lifecycle without the policy, and an out-of-range selection (`TestWorkKindPolicyContract`).

In a dry run no timers are armed and job Work does not start. Triggers wait and run, coalesced, after a normal start.

## Recoverability of trigger families (X04)

| Family | Readable backing history | Classification |
| --- | --- | --- |
| Timed slot | The durable timer and firing ledger | Not ingress. It survives restarts and catches up within six hours, and needs no webhook. |
| Manual run | Operator acceptance | Not ingress. |
| Custom webhook POST (`schedule.webhook`) | None | One-shot, declared loss (`schedule.WebhookLoss`): a POST sent while the listener or tunnel is down is lost, and the sender sees the failure. Reconcile returns an empty complete snapshot. |
| GitHub push, release, workflow_run (through `github.delivery`) | GitHub keeps branch heads, releases and workflow runs readable, but no per-delivery history | Jobs rely on `github.delivery`, which the GitHub ingress declares non-re-derivable (`DeliveryLoss`). Reconstruction is possible for a moved branch head (collapsing intermediate pushes, which coalescing tolerates), for published releases and for workflow runs within Actions retention. The loss stays declared: a branch-head poll would run a job for a push it never saw described (no files, no before), and a scheduled job is the documented fallback for anything that must not be missed. |

Turning off webhooks (`--no-tunnel`, no listener) affects only these explicitly declared one-shot families: timed and manual runs are unaffected (`TestScheduleWebhookOff`).

## Operation

- Doctor `schedule` lists each job's next slot, event and webhook triggers, and warns on timer or delivery failures.
- Doctor `webhooks` lists the webhook paths.
- Deep doctor `schedule_webhooks` prints the signed URLs.
- Webhook ingress registrations are immutable per runtime instance; moving a namespace reports `restart_required`.
- Jobs and timings reload on apply without a restart.

## Verification

- **Unit tests** (`extensions/schedule`):
  - `TestCronSlot`, `TestCronSlotClockChanges`, `TestJobValidate`, `TestJobMatches`, `TestPathsMatch` and `TestJobPathsFilterPushes`;
  - the exact six-hour boundary;
  - recurrence catch-up, DST nights, interval jobs and edit rules;
  - the route, trigger selection and the webhook ingress.
- **SQLite integration** with the production router, scheduler and timers on a fake clock (`integration/schedule_test.go`):
  - exactly six hours, one second inside and just outside, across restarts;
  - several missed slots in one outage, with a crash between the atomic firing and routing;
  - both DST nights;
  - config edits, removal while a run goes, a disabled scope and entry removal;
  - duplicate, manual, webhook and timer triggers coalescing into one pending run, and no retry after a failure;
  - webhook-off operation, and a route per binding.

  Every run is checked to have come through an accepted Event routed by a `schedule` route to the job's Work.
- **Runtime recurrence contract** (`integration/recurrence_test.go`): substitution without accepting the template, skip ledger replay across restart, an unregistered kind and invalid answers.
- **Composed daemon** (`TestDaemonScheduledJobs`, part of `make drill`): an interval job at start and not again after a restart; a signed GitHub push selecting a job by event and branch, with a non-matching branch running nothing; a signed job webhook, a wrong token refused, and the deep doctor URL.

## Job runs

Jobs run as the same Executions as every other Work, with no job record, job scheduler or job history of its own. What a job runs is still the binding of the `schedule` route that selects it; everything below is binding configuration or a generic runtime rule.

### Bindings

| Job | Open Conduit binding |
| --- | --- |
| A command in the main checkout | `shell` with `command`/`argv`, plus `resource: {"provider": "git", "config": {"checkout": …, "mode": "shared"}}`. The checkout is fast-forwarded when no other execution holds it; otherwise the run is told how far behind it is and why ([workspaces](../workspaces/)). |
| A fresh checkout | The same with `"mode": "fresh"`: a detached worktree at the remote head, removed after success and retained `retain_failed_seconds` (default one day) after failure. |
| No checkout | `shell` with no resource (a private scratch directory, or `dir`). |
| An agent job | Any executor: a `claude-code` binding selected by a `schedule` route runs a turn per trigger. |

### Timeouts: one deadline model

Every attempt has one runtime deadline, `settings.timeout_seconds` (default 3600), overridable per binding with `bindings[].timeout_seconds` for any executor. The attempt pins it with its binding, the host times the attempt out when it passes (stop, then confirmed exclusion), and history shows the deadline and remaining budget the same way for jobs and turns. The shell's own `config.timeout_seconds` remains the process-tree deadline the executor enforces across restarts (SIGTERM, then SIGKILL), and its result reads `shell command timed out`. A long detached job gets a binding `timeout_seconds` above its longest run.

### Kept runs: one retention policy

A job's history is its standing Work's Executions, kept by the one per-Work `kept_executions` (default 20) like every Work. Coordination history (trigger, timing, outcome, usage, artifact metadata) is never deleted, so only the content (log, outputs, payload) of runs older than the newest 20 is released; an operator who wants more raises `settings.kept_executions`.

### Retry

Jobs get no automatic failure-budget retry, because a job is often not idempotent (posting, deploying) and its next trigger is its retry. An executor's `Result.RetryHint` is honoured for every Work kind, standing Work included; it is the executor saying when to try again, not a failure budget, so it never exhausts standing Work. While that backoff runs, new triggers queue and join the retry's batch. A Claude Code usage limit on an agent job waits for its reset exactly as an issue turn does.

### Detached long-running jobs

Every shell segment is already detached by construction: its own process group, file-backed output, identity-checked adoption after a daemon restart without relaunch ([shell executor](../shell-executor/)). A detached job is therefore an ordinary job binding with a long binding `timeout_seconds`. One rule needs no core change:

- **Deliveries and manual runs start nothing while it runs.** Two `schedule` routes to the same binding: one selecting `"triggers": ["timer"]` with `active_policy: "queue"` (a slot due during the run waits for it), then one for the job's remaining kinds with `active_policy: "drop"`. Drop applies only while the Work is running, as for every route.

### Job environment

A job's contract is the shell executor's: `OS_TRIGGER_TYPE`, `OS_INPUT_FILE` (the trigger with its `schedule.trigger.v1` payload: kind, label, slot, delivery fields or webhook body) and `OS_OUTPUT_DIR`.

### `job:` waits

The `job:<name>` wait condition translates. The host announces each finished job run as one derived Event on the job's own key, typed by how it ended: `schedule.run.completed`, `.failed`, `.cancelled` or `.handoff` (`schedule.run.v1`: job, work, execution, attempt, outcome, summary). A per-job checkpoint in the extension KV commits in the same transaction as the acceptance, the delivery is `ended/<execution>`, and the parent is the run's trigger, so a crash only repeats a deduplicated delivery. An attempt that ended waiting is passed over. No route delivers these types. `github.wait`/holds translate `job:<name>` (optionally `<project>/job:<name>`) into a FromNow dependency on that key and all four types (an unknown job is refused `no_job`; a job in another scope needs `read_scopes`), so the next run, not an earlier one, wakes the wait, and the agent reads `a run of the backup job ended: failed`. Bootstrap injects the resolution (`ToolsOptions.Jobs`), so GitHub does not import this extension.

### Dry run

The same for every trigger kind: a dry run arms no schedule timers and starts no job Work, while webhook, code-host and manual triggers are still accepted and routed. They wait as input and run, coalesced into one run, after a normal start. Open Conduit keeps accepted input lossless.

### History and the run view

A job run is a `runtime.Run` like a turn: trigger, binding, executor, deadline, outcome, usage, artifacts and retention, filtered by `work_kind=schedule.job` or not at all. Workspace metadata is generic across executors: `extension.Result.Workspace` (`Kind`, `Ref`, `Dir`, `Summary`, `Data`), reported by the shell and Claude Code executors from the acquired resource, with a provider one-line `Resource.Summary` (`main checkout, 1 commit(s) behind origin/main: another execution is using the checkout`, `fresh worktree at origin/main 1a2b3c4`, `worktree on branch agent/work-x, reused`). `GET /v1/runs` returns it as `workspace`, and `runview.Detail` (CLI and TUI) prints it. Nothing in history branches on executor or Work kind.

### Job run verification

- **Unit tests:** route trigger kinds and truncated file lists (`TestRouteMatch`, `TestJobPathsFilterPushes`); binding `timeout_seconds` (`TestBindingTimeout`); one TUI for a job and a turn with a workspace (`TestJobAndTurnShareTheView`).
- **SQLite integration:** the binding deadline, a hinted retry on standing Work with triggers joining its batch, no hintless retry, and run-ended Events (`TestJobDeadlineRetryHintAndRunEvents`); holds and agent waits (`TestJobRunEndsAWait`).
- **Composed daemon, part of `make drill`:**
  - `TestDaemonWorkflowC`: crash-safe recurrence (SIGKILL right after the interval timer's fire commits; one run after restart), six-hour catch-up by config edit (one hour past runs once, seven hours past is skipped), a fresh checkout at the remote head with removal after success, a detached job adopted across a daemon SIGKILL with a manual run dropped while it ran, a shell job's `os context` toolbelt call, and an agent turn and the jobs in one `/v1/runs` history rendered by one run view with the workspace shown.
  - `TestDaemonJobParity`: `TestJobDeliveryWhileRunningRunsAgain` (deliveries 1 then 3), `TestJobRunsAreKept` under `kept_executions`, `TestJobCatchesUpTheMainCheckout` (stale while another execution holds the checkout, then fast-forwarded), `TestJobEnvResolvesSecrets`, `TestJobTimesOut` (shell deadline) and a binding `timeout_seconds`.
- **Reload** is config apply, covered by `TestScheduleConfigEditsAndRemoval`, `TestDaemonScheduledJobs` and the daemon reload drill for bindings (`TestDaemonConfigReload`).

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 05-01 put job triggers on the five primitives: job definitions, the timed, webhook, code-host and manual triggers, the `schedule.Recurrence` catch-up policy, the one-pending-run rule and the dry-run behaviour. It added the generic contract pieces `extension.RouteInput.Route` and `config.Registry.HandlerConfig`, and the runtime recurrence contract. Job runs were left to 05-02.
- Ticket 05-02 delivered job runs: route selection by trigger kind, per-binding `timeout_seconds`, `Result.RetryHint` honoured for standing Work, run-ended Events and `job:` waits, and the generic `extension.Result.Workspace` metadata shown by `GET /v1/runs` and the run view. It kept 05-01's standing-Work rule (no B04 failure budget for jobs), kept 05-01's dry-run behaviour, and kept the declared loss for code-host triggers rather than adding a branch-head poll. Its tests are listed under [Job run verification](#job-run-verification).
- Ticket 05-03 added the introspection extension's `introspection.v1` recurrence kind beside the schedule kind.
- The binding reload drill (`TestDaemonConfigReload`) dates from ticket 02-09.
