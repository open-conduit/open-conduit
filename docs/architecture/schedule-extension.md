# Scheduled jobs: definitions, catch-up, the one-pending-run rule and job run parity

Ticket 05-01 ports v1's job triggers onto the five primitives. A timed job is a configured independent root on a durable core timer; every trigger is an accepted Event; the `schedule` route makes it input of the job's one standing Work; the scheduler runs it as an ordinary Execution through the job's binding. Nothing schedules work outside event → route → Work → Execution. The extension is [`extensions/schedule`](../../extensions/schedule); [`internal/bootstrap/schedule.go`](../../internal/bootstrap/schedule.go) composes it. Run parity (fresh checkout, detached runs, env and secret precedence, payload artifacts, timeouts, kept runs, the job history view) is delivered by 05-02; see [Job run parity](#job-run-parity-05-02).

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
    {"name": "poke", "on": {"webhook": true}},
    {"name": "by-hand"}
  ]}}
```

| Field | Meaning (v1 `config.Job`) |
| --- | --- |
| `namespace` | Stable source namespace of the entry's triggers and job keys. A job's key is `job/<name>` in it. |
| `scope` | The containing scope (ingress target rule). |
| `webhook_secret` | A selected secret name; it signs the job webhook URLs. Required when a job takes webhooks. |
| `jobs[].name` | `[a-z][a-z0-9_-]{0,62}`. v1's free-text names are converted by 06-02. |
| `every_minutes` | An interval job. Not with `at`/`cron`. |
| `at` | UTC times of day, `HH:MM`. Not with `cron`. |
| `cron`, `time_zone` | A standard five-field cron (the v1 parser, `robfig/cron/v3`) read in an IANA zone, UTC when empty. The zone goes in `time_zone`, never a `TZ=` prefix; `time_zone` is only for `cron`. |
| `on.events` | Code-host event names, optionally with an action (`push`, `release.published`). |
| `on.branches` | Branch globs for ref-carrying events. |
| `on.paths` | GitHub Actions path rules for pushes (`*` within a folder, `**` across, `!` takes back). |
| `on.webhook` | Custom POSTs to the job's signed URL. |

A job with no timing and no `on` is manual only. Validation is v1's (`Job.Validate`) minus the run settings, and it never echoes values.

Routes use one registered handler, `schedule`. Its route config `{"jobs": [...], "triggers": [...]}` selects jobs and, since 05-02, trigger kinds (`timer`, `webhook`, `event`, `manual`); empty selects every job of every schedule entry in the scope, and every kind. One route per binding gives each job its own command:

```json
{"name": "jobs-report", "handler": "schedule", "binding": "report-cmd", "active_policy": "queue", "config": {"jobs": ["report"]}}
```

Two generic contract additions make this possible. `extension.RouteInput.Route` carries the evaluated route, including its opaque handler config. `config.Registry.HandlerConfig` lets a registered handler validate that config at load/apply time. Put a `schedule` route before a `manual` route in the same scope, or the manual route would create `manual.work` for a job key.

**Repository-scoped declarations.** v1 projects were repositories, and v1 read extra jobs from the checkout's `.open-supervisor.yaml` every minute. In Open Conduit the repository's scope declares its jobs. B05 forbids reading `.open-supervisor.yaml` implicitly and requires an explicit apply before disk edits change behavior; 06-02 converts those files into scope entries, routes and bindings (`open-conduit migrate config`, [migration and recovery](migration-recovery.md)). Job reload is therefore config reload.

## Triggers

| Trigger | Event | Producer and identity |
| --- | --- | --- |
| Timed slot (every, at, cron) | `schedule.slot` | A core timer, as a configured independent root. Delivery `slot/<job>/<UTC instant>`. |
| Webhook POST | `schedule.webhook` | The entry's one-shot ingress. Delivery `webhook/<job>/<id>`: the sender's `Idempotency-Key` (or `X-Request-Id`), else a fresh ID per POST. |
| Code-host delivery | `schedule.event` | Derived from the accepted raw delivery (`github.delivery`), which is its causal parent. Delivery `event/<job>/<parent Event>`. |
| Manual | any non-`core.*` type, source `core.manual` | The operator: `open-conduit events emit --scope-id S --type schedule.run --delivery-id D --key-source <namespace> --key-external-id job/<name>`. |

Every trigger carries the `schedule.trigger.v1` payload: job, kind, v1's run label (`cron 2026-10-25 01:30 Europe/London`, `at 2026-10-21 05:17 UTC`, `every 60m`, `webhook`, `push abc1234`), the slot, and the delivery fields or the webhook body.

**Webhooks.** The webhook listener serves `POST /schedule/<scope>/<entry>/<job>/<token>`. The token is an HMAC of the entry's webhook secret over namespace and job, so it is stable across restarts and is revoked by rotating the secret. A wrong token, an unknown job or a job without webhooks gets 404, as in v1. The body is limited to 1 MiB, and the response is 202 only after durable acceptance. `doctor --check-secrets` prints each signed URL (`schedule_webhooks`); plain doctor lists the paths with `<token>`.

**Code-host events.** The host reads accepted `github.delivery` Events after a durable cursor. For each job in the delivery's scope whose `on` matches (v1 `matches`, branches and paths), it accepts a derived `schedule.event` in one transaction with the cursor. A push whose changed files are unknown runs anyway; a deleted branch changes nothing. Path matching uses the delivery's commit file lists. 05-02 decided against v1's `git diff` in the checkout: the extension reads no repository, and only the Git provider touches a checkout. Instead a list that may be truncated (the GitHub ingress cap of 1000 files, or GitHub's own cap of 2048 commits, recorded as `DeliveryPayload.Commits`) counts as unknown, so the job runs rather than skip on a partial list.

## Timers, catch-up and clock changes

Timed jobs use a core timer with a calendar recurrence. `runtime.TimerOptions.Recurrences` registers pure `RecurrenceFunc` policies by kind. `Timers.ArmRecurrence` stores the opaque spec with the timer. Inside the firing transaction core asks the policy which occurrence this firing publishes (or none) and when the timer fires next. Acceptance, the firing ledger and the rearm commit together, so a crash never separates them; replaying a firing returns its original result. A substituted or skipped firing never accepts the template. Core keeps no calendar knowledge.

05-03: the introspection extension registers a second recurrence kind (`introspection.v1`) for its daily sweep and triage, with the same v1 calendar rules. It is not a `schedule` entry, because the PRD makes them Work kinds the introspection extension defines ([introspection](issue-reporting.md#the-sweep-and-triage-schedules)). Bootstrap merges both kinds into `TimerOptions.Recurrences` and both standing Work policies into `SchedulerOptions.WorkKinds`.

The schedule policy (`schedule.Recurrence`) ports v1's `dueSlot`/`cronSlot`:

- **At and cron.** A firing publishes the latest slot at or before now and strictly within six hours of it (`now − slot < 6h`), unless that slot's wall-clock name is the one that ran last. Exactly six hours late, or more, is skipped: v1's algorithm is exclusive, whatever its README prose says. Older missed slots are always skipped. The timer rearms for the next slot after now whose name differs from the one that ran last.
- **Clock changes.** A wall-clock time the clocks skip (01:30 London on the last Sunday in March) has no slot. A repeated time (01:30 on the last Sunday in October) has two instants and one name, so a daily job runs once. A schedule within the hour, such as `*/30`, still runs at repeated times whose name differs from the one just run, exactly as v1's single `LastSlot` did. Slot identity is the instant, which is stable across restarts; the name is the v1 slot.
- **Every N minutes.** A firing publishes its own occurrence and rearms N minutes after it, or N minutes after now when that is already past, so an outage yields one run rather than a burst. Intentional difference: v1 also ran every interval job at each daemon start; the durable timer runs it only when due.

`schedule.Ensure` reconciles timers with each committed generation: at startup, on every apply, and every minute as a repair pass. This is v1's `refresh`:

- A new timed job waits for its next slot rather than run one someone may already have run (v1 first start). A new interval job runs at once.
- An edited timing rearms. An interval job runs at once. A timed job runs its latest slot within six hours if that slot is not the one that ran last (v1 kept `LastSlot` across a timer restart), else it waits. Editing a job's other triggers does not rearm it.
- A removed job or entry loses its timer, while a run already going carries on. Its late triggers match no route (`unmatched`), so they never run.
- A disabled scope's timers stay durable and untouched (B05), and their due Events resume on re-enable.
- An unchanged timer is left alone across restarts, so a missed slot is caught up by the timer's own firing.

## The one-pending-run rule (coalescing decision)

v1 permits one pending rerun while a job runs: at most one waits, a delivery takes the place of a waiting timer request, the newest delivery takes the place of older ones, and a later timer never displaces a waiting request.

**Decision: no core coalesce option and no lossy queue.** B04's queue stays lossless and ordered. The rule falls out of three generic pieces:

1. **One standing Work per job.** The route creates `schedule.job` Work owning `job/<name>` for the first trigger. One nonterminal attempt per Work means one run at a time per job. Later triggers queue on that Work, so the job's run history is that Work's Executions.
2. **The next attempt takes the whole batch.** `Scheduler.Start` already gives one attempt every pending input (`RunRequest.Inputs`), so however many triggers arrive while a run goes, exactly one run is pending. Nothing is dropped from history: each trigger is consumed by that one attempt.
3. **The extension picks the batch's trigger.** `runtime.SchedulerOptions.WorkKinds` registers a lifecycle policy per Work kind. The job kind's `Trigger` function, `schedule.SelectTrigger`, chooses the newest delivery (webhook or code-host event), else the earliest request. That choice becomes `Execution.TriggerID` and `RunRequest.Trigger`. A wake winner still takes precedence.

The same policy makes job Work **standing**. A completed attempt leaves it ready and idle instead of completing it. A failed attempt arms no automatic retry: v1 never retried a failed job, and the next trigger is its retry. Only cancellation ends job Work. A cancelled job Work is terminal, and its later triggers are audited as `terminal`. 05-02 kept this (jobs do not adopt the B04 failure budget) and made an executor's retry hint apply to standing Work too; see [Retry](#retry).

Contract tests cover all of these orders:

- the batch orders of v1's `TestJobDeliveryWhileRunningRunsAgain` and `TestJobTimerRunWaitsForTheOneGoing` (`TestSelectTrigger`, `TestScheduleTriggersCoalesce`);
- the default lifecycle without the policy, and an out-of-range selection (`TestWorkKindPolicyContract`).

Differences:

- Two triggers accepted before an idle job starts collapse into one run; v1 would have started one and queued the other.
- v1 detached jobs started nothing on a delivery while running and retried a `busy` status in 10 minutes. 05-02 maps the delivery rule to a `drop` route and the status file to the `legacy-job` wrapper ([Detached jobs](#detached-long-running-jobs)).
- In a dry run no timers are armed and job Work does not start. Triggers wait and run, coalesced, after a normal start.

## Recoverability of trigger families (X04)

| Family | Readable backing history | Classification |
| --- | --- | --- |
| Timed slot | The durable timer and firing ledger | Not ingress. It survives restarts and catches up within six hours, and needs no webhook. |
| Manual run | Operator acceptance | Not ingress. |
| Custom webhook POST (`schedule.webhook`) | None | One-shot, declared loss (`schedule.WebhookLoss`): a POST sent while the listener or tunnel is down is lost, and the sender sees the failure. Reconcile returns an empty complete snapshot. |
| GitHub push, release, workflow_run (through `github.delivery`) | GitHub keeps branch heads, releases and workflow runs readable, but no per-delivery history | Jobs rely on `github.delivery`, which the GitHub ingress declares non-re-derivable (`DeliveryLoss`). Reconstruction is possible for a moved branch head (collapsing intermediate pushes, which coalescing tolerates), for published releases and for workflow runs within Actions retention. 05-02 keeps the declared loss: no v1 job reconstructed missed deliveries either, a branch-head poll would run a job for a push it never saw described (no files, no before), and a scheduled job is the documented fallback for anything that must not be missed. |

Turning off webhooks (`--no-tunnel`, no listener) affects only these explicitly declared one-shot families: timed and manual runs are unaffected (`TestScheduleWebhookOff`).

## Operation

- Doctor `schedule` lists each job's next slot, event and webhook triggers, and warns on timer or delivery failures.
- Doctor `webhooks` lists the webhook paths.
- Deep doctor `schedule_webhooks` prints the signed URLs.
- Webhook ingress registrations are immutable per runtime instance; moving a namespace reports `restart_required`.
- Jobs and timings reload on apply without a restart.

## Verification

- **Unit tests** (`extensions/schedule`):
  - ports of v1 `TestCronSlot`, `TestCronSlotClockChanges`, `TestJobValidate`, `TestJobMatches`, `TestPathsMatch` and `TestJobPathsFilterPushes`;
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

## Job run parity (05-02)

05-02 ports v1's job runs onto the same Executions, with no job record, job scheduler or job history of its own. What a job runs is still the binding of the `schedule` route that selects it; everything below is binding configuration or a generic runtime rule.

### Bindings

| v1 job | Open Conduit binding |
| --- | --- |
| `command` in the main checkout | `shell` with `command`/`argv`, plus `resource: {"provider": "git", "config": {"checkout": …, "mode": "shared"}}`. The checkout is fast-forwarded when no other execution holds it; otherwise the run is told how far behind it is and why ([workspaces](workspaces.md)). |
| `checkout: fresh` | The same with `"mode": "fresh"`: a detached worktree at the remote head, removed after success and retained `retain_failed_seconds` (default one day) after failure. |
| No checkout | `shell` with no resource (a private scratch directory, or `dir`). |
| An agent job | Any executor: a `claude-code` binding selected by a `schedule` route runs a turn per trigger. |
| `timeoutMinutes` (default 5) | Binding `timeout_seconds` (below). |
| `status` (detached) | `legacy-job` with `status` (below). |
| `keepRuns` (default 30) | Not per job: `settings.kept_executions` (below). |

### Timeouts: one deadline model

Every attempt has one runtime deadline, `settings.timeout_seconds` (default 3600), now overridable per binding with `bindings[].timeout_seconds` for any executor. The attempt pins it with its binding, the host times the attempt out when it passes (stop, then confirmed exclusion), and history shows the deadline and remaining budget the same way for jobs and turns. The shell's own `config.timeout_seconds` remains the process-tree deadline the executor enforces across restarts (SIGTERM, then SIGKILL), and its result reads `shell command timed out`. Intentional difference: a job no longer gets v1's implicit five minutes; 06-02 writes v1's effective value (`timeoutMinutes`, else 5) as the shell timeout, and a long detached job gets a binding `timeout_seconds` above its longest run.

### Kept runs: one retention policy

A job's history is its standing Work's Executions, kept by the one per-Work `kept_executions` (default 20) like every Work. Decision: v1's per-job default of 30 is not reproduced. Coordination history (trigger, timing, outcome, usage, artifact metadata) is never deleted, so only the content (log, outputs, payload) of runs older than the newest 20 is released; an operator who wants more raises `settings.kept_executions`. 06-02 reports a v1 `keepRuns` above the configured value rather than silently dropping it.

### Retry

Jobs keep v1's rule: no automatic failure-budget retry, because a job is often not idempotent (posting, deploying) and its next trigger is its retry. One generic change (05-02): an executor's `Result.RetryHint` is honoured for every Work kind, standing Work included; it is the executor saying when to try again, not a failure budget, so it never exhausts standing Work. While that backoff runs, new triggers queue and join the retry's batch. This is how v1's `busy` status keeps its ten-minute retry, and a Claude Code usage limit on an agent job waits for its reset exactly as an issue turn does.

### Detached long-running jobs

Every shell segment is already detached by construction: its own process group, file-backed output, identity-checked adoption after a daemon restart without relaunch ([shell executor](shell-executor.md)). A v1 detached job is therefore an ordinary job binding with a long binding `timeout_seconds`. Two v1 rules remain, both expressed without core changes:

- **Deliveries and manual runs start nothing while it runs.** Two `schedule` routes to the same binding: one selecting `"triggers": ["timer"]` with `active_policy: "queue"` (a slot due during the run waits for it), then one for the job's remaining kinds with `active_policy: "drop"`. Drop applies only while the Work is running, as for every route.
- **The status folder.** The `legacy-job` wrapper (below) reads `last.json` when the run ends. Its outcome joins the run's summary (`status done abc1234`) and the file becomes a run artifact. With no fresh `last.json` the summary says `no status result`. `busy` fails the run with a retry hint ten minutes out (the hint names a reset eight minutes out; core adds its two-minute margin), unless that would be six hours or more after the triggering slot, in which case the job waits for its next slot, as v1 did. `running.json` is not read: adoption follows the process itself.

Intentional difference: a slot that queued behind a run lasting more than six hours still runs when the run ends (v1 would have let it lapse).

### Open Supervisor compatibility (`legacy-job`)

Open Conduit's job contract is the shell executor's: `OS_TRIGGER_TYPE`, `OS_INPUT_FILE` (the trigger with its `schedule.trigger.v1` payload: kind, v1 label, slot, delivery fields or webhook body) and `OS_OUTPUT_DIR`. Neither core nor the shell executor knows a legacy name. v1 job scripts keep working through the `legacy-job` executor registration, which lives in this extension ([`legacy.go`](../../extensions/schedule/legacy.go)) and wraps the shell executor. Its binding config is `{"status": "<folder>", "shell": {<shell binding config>}}`; the shell part is validated by the shell executor, and capabilities are the shell's. It adds:

| Variable | Value |
| --- | --- |
| `OPEN_SUPERVISOR_JOB`, `_JOB_RUN`, `_PROJECT` | Job name, execution ID, scope ID. |
| `OPEN_SUPERVISOR_JOB_TRIGGER` | v1's label: `cron 2026-10-25 01:30 Europe/London`, `at … UTC`, `every 60m`, `webhook`, `push abc1234`, `by hand`, or `retry`. |
| `OPEN_SUPERVISOR_EVENT`, `_EVENT_ACTION`, `_EVENT_REF`, `_EVENT_SHA` | The delivery trigger's fields (`webhook` for a custom POST); unset for timed and manual runs. |
| `OPEN_SUPERVISOR_EVENT_FILE` | A file holding the webhook body as posted. For a code-host event it holds the trigger payload, because Open Conduit keeps the delivery's fields, not GitHub's raw body (intentional difference). |
| `OPEN_SUPERVISOR_BASE_BRANCH`, `_WORKTREE`, `_REMOTE` | From the acquired workspace (`Resource.Data` base and remote, `Resource.Dir`). |

**Precedence, lowest to highest:** the shell's sanitized baseline; the resource provider's layer with the three workspace names added (a checkout `.env` defining one wins); the legacy job and delivery names, only where the binding's `env` does not set the same name; the binding's `env`; its selected secrets; the trusted `OS_*` variables. A binding can therefore always pin or override a legacy value, and nothing overrides `OS_*`.

**Payload artifacts.** The delivery file is published as an `application/json` artifact of the run, so the body that triggered a run is inspectable from history and is released with the run's content under `kept_executions` (v1 kept job-events for one day). The trigger Event itself keeps the body durably (inline, 1 MiB cap) for every job, wrapped or not.

The translated result is cached beside the run, so observing it again after adoption returns the identical result and publishes nothing twice; `Forget` releases the wrapper's files with the shell's.

### `job:` waits

v1's `job:<name>` condition (03-04 refused it) now translates. The host announces each finished job run as one derived Event on the job's own key, typed by how it ended: `schedule.run.completed`, `.failed`, `.cancelled` or `.handoff` (`schedule.run.v1`: job, work, execution, attempt, outcome, summary). A per-job checkpoint in the extension KV commits in the same transaction as the acceptance, the delivery is `ended/<execution>`, and the parent is the run's trigger, so a crash only repeats a deduplicated delivery. An attempt that ended waiting is passed over. No route delivers these types. `github.wait`/holds translate `job:<name>` (optionally `<project>/job:<name>`) into a FromNow dependency on that key and all four types (an unknown job is refused `no_job`; a job in another scope needs `read_scopes`), so the next run, not an earlier one, wakes the wait, and the agent reads `a run of the backup job ended: failed` (v1: `ended: ok`). Bootstrap injects the resolution (`ToolsOptions.Jobs`), so GitHub does not import this extension.

### Dry run

Unchanged from 05-01 and the same for every trigger kind: a dry run arms no schedule timers and starts no job Work, while webhook, code-host and manual triggers are still accepted and routed. They wait as input and run, coalesced into one run, after a normal start. v1 dropped them instead; Open Conduit keeps accepted input lossless.

### History and the run view

A job run is a `runtime.Run` like a turn: trigger, binding, executor, deadline, outcome, usage, artifacts and retention, filtered by `work_kind=schedule.job` or not at all. 05-02 adds generic workspace metadata for every executor: `extension.Result.Workspace` (`Kind`, `Ref`, `Dir`, `Summary`, `Data`), reported by the shell and Claude Code executors from the acquired resource, with a provider one-line `Resource.Summary` (`main checkout, 1 commit(s) behind origin/main: another execution is using the checkout`, `fresh worktree at origin/main 1a2b3c4`, `worktree on branch agent/work-x, reused`). `GET /v1/runs` returns it as `workspace`, and `runview.Detail` (CLI and TUI) prints it. Nothing in history branches on executor or Work kind.

### Verification (05-02)

- **Unit tests:** the `legacy-job` environment, precedence, delivery file and payload artifact, status translation (done, busy within and past six hours, no result), cached results and `Forget` (`TestLegacyJobEnvironment`, `TestLegacyStatusFolder`, `TestLegacyConfig`); route trigger kinds and truncated file lists (`TestRouteMatch`, `TestJobPathsFilterPushes`); binding `timeout_seconds` (`TestBindingTimeout`); one TUI for a job and a turn with a workspace (`TestJobAndTurnShareTheView`, v1 `TestRunDetail`).
- **SQLite integration:** the binding deadline, a hinted retry on standing Work with triggers joining its batch, no hintless retry, and run-ended Events (`TestJobDeadlineRetryHintAndRunEvents`); v1 `TestJobRunEndsAWait` for holds and agent waits (`TestJobRunEndsAWait`).
- **Composed daemon, part of `make drill`:**
  - `TestDaemonWorkflowC`: crash-safe recurrence (SIGKILL right after the interval timer's fire commits; one run after restart), six-hour catch-up by config edit (one hour past runs once, seven hours past is skipped), a fresh checkout at the remote head with v1's workspace names and removal after success, a detached status job adopted across a daemon SIGKILL with a manual run dropped while it ran, a shell job's `os context` toolbelt call, and an agent turn and the jobs in one `/v1/runs` history rendered by one run view with the workspace shown.
  - `TestDaemonJobParity`: v1 `TestJobDeliveryWhileRunningRunsAgain` (deliveries 1 then 3, with the payload artifact), `TestJobRunsAreKept` under `kept_executions`, `TestJobCatchesUpTheMainCheckout` (stale while another execution holds the checkout, then fast-forwarded), `TestJobEnvResolvesSecrets`, `TestJobTimesOut` (shell deadline) and a binding `timeout_seconds`.
- **Reload** is config apply, covered by 05-01 (`TestScheduleConfigEditsAndRemoval`, `TestDaemonScheduledJobs`) and the 02-09 reload drill for bindings.
