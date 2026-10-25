# Scheduled jobs: definitions, catch-up and the one-pending-run rule

Ticket 05-01 ports v1's job triggers onto the five primitives. A timed job is a configured independent root on a durable core timer; every trigger is an accepted Event; the `schedule` route makes it input of the job's one standing Work; the scheduler runs it as an ordinary Execution through the job's binding. Nothing schedules work outside event → route → Work → Execution. The extension is [`extensions/schedule`](../../extensions/schedule); [`internal/bootstrap/schedule.go`](../../internal/bootstrap/schedule.go) composes it. Run parity (fresh checkout, detached runs, env and secret precedence, payload artifacts, timeouts, kept runs, the job history view) is 05-02.

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

Routes use one registered handler, `schedule`. Its route config `{"jobs": [...]}` selects jobs; empty selects every job of every schedule entry in the scope. One route per binding gives each job its own command:

```json
{"name": "jobs-report", "handler": "schedule", "binding": "report-cmd", "active_policy": "queue", "config": {"jobs": ["report"]}}
```

Two generic contract additions make this possible. `extension.RouteInput.Route` carries the evaluated route, including its opaque handler config. `config.Registry.HandlerConfig` lets a registered handler validate that config at load/apply time. Put a `schedule` route before a `manual` route in the same scope, or the manual route would create `manual.work` for a job key.

**Repository-scoped declarations.** v1 projects were repositories, and v1 read extra jobs from the checkout's `.open-supervisor.yaml` every minute. In Open Conduit the repository's scope declares its jobs. B05 forbids reading `.open-supervisor.yaml` implicitly and requires an explicit apply before disk edits change behavior; 06-02 converts those files into scope entries, routes and bindings. Job reload is therefore config reload.

## Triggers

| Trigger | Event | Producer and identity |
| --- | --- | --- |
| Timed slot (every, at, cron) | `schedule.slot` | A core timer, as a configured independent root. Delivery `slot/<job>/<UTC instant>`. |
| Webhook POST | `schedule.webhook` | The entry's one-shot ingress. Delivery `webhook/<job>/<id>`: the sender's `Idempotency-Key` (or `X-Request-Id`), else a fresh ID per POST. |
| Code-host delivery | `schedule.event` | Derived from the accepted raw delivery (`github.delivery`), which is its causal parent. Delivery `event/<job>/<parent Event>`. |
| Manual | any non-`core.*` type, source `core.manual` | The operator: `open-conduit events emit --scope-id S --type schedule.run --delivery-id D --key-source <namespace> --key-external-id job/<name>`. |

Every trigger carries the `schedule.trigger.v1` payload: job, kind, v1's run label (`cron 2026-10-25 01:30 Europe/London`, `at 2026-10-21 05:17 UTC`, `every 60m`, `webhook`, `push abc1234`), the slot, and the delivery fields or the webhook body.

**Webhooks.** The webhook listener serves `POST /schedule/<scope>/<entry>/<job>/<token>`. The token is an HMAC of the entry's webhook secret over namespace and job, so it is stable across restarts and is revoked by rotating the secret. A wrong token, an unknown job or a job without webhooks gets 404, as in v1. The body is limited to 1 MiB, and the response is 202 only after durable acceptance. `doctor --check-secrets` prints each signed URL (`schedule_webhooks`); plain doctor lists the paths with `<token>`.

**Code-host events.** The host reads accepted `github.delivery` Events after a durable cursor. For each job in the delivery's scope whose `on` matches (v1 `matches`, branches and paths), it accepts a derived `schedule.event` in one transaction with the cursor. A push whose changed files are unknown runs anyway; a deleted branch changes nothing. Path matching uses the delivery's commit file lists, which can be truncated. 05-02 decides whether to add v1's `git diff` against the checkout.

## Timers, catch-up and clock changes

Timed jobs use a core timer with a calendar recurrence. `runtime.TimerOptions.Recurrences` registers pure `RecurrenceFunc` policies by kind. `Timers.ArmRecurrence` stores the opaque spec with the timer. Inside the firing transaction core asks the policy which occurrence this firing publishes (or none) and when the timer fires next. Acceptance, the firing ledger and the rearm commit together, so a crash never separates them; replaying a firing returns its original result. A substituted or skipped firing never accepts the template. Core keeps no calendar knowledge.

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

The same policy makes job Work **standing**. A completed attempt leaves it ready and idle instead of completing it. A failed attempt arms no automatic retry: v1 never retried a failed job, and the next trigger is its retry. Only cancellation ends job Work. A cancelled job Work is terminal, and its later triggers are audited as `terminal`. 05-02 decides whether jobs should adopt the B04 retry budget instead.

Contract tests cover all of these orders:

- the batch orders of v1's `TestJobDeliveryWhileRunningRunsAgain` and `TestJobTimerRunWaitsForTheOneGoing` (`TestSelectTrigger`, `TestScheduleTriggersCoalesce`);
- the default lifecycle without the policy, and an out-of-range selection (`TestWorkKindPolicyContract`).

Differences and items left to 05-02:

- Two triggers accepted before an idle job starts collapse into one run; v1 would have started one and queued the other.
- v1 detached jobs started nothing on a delivery while running and retried a `busy` status in 10 minutes. With detached adoption, 05-02 maps the delivery rule to the route's `drop` active policy and decides the status file translation.
- In a dry run no timers are armed and job Work does not start. Triggers wait and run, coalesced, after a normal start.

## Recoverability of trigger families (X04)

| Family | Readable backing history | Classification |
| --- | --- | --- |
| Timed slot | The durable timer and firing ledger | Not ingress. It survives restarts and catches up within six hours, and needs no webhook. |
| Manual run | Operator acceptance | Not ingress. |
| Custom webhook POST (`schedule.webhook`) | None | One-shot, declared loss (`schedule.WebhookLoss`): a POST sent while the listener or tunnel is down is lost, and the sender sees the failure. Reconcile returns an empty complete snapshot. |
| GitHub push, release, workflow_run (through `github.delivery`) | GitHub keeps branch heads, releases and workflow runs readable, but no per-delivery history | Jobs rely on `github.delivery`, which the GitHub ingress declares non-re-derivable (`DeliveryLoss`). Reconstruction is possible for a moved branch head (collapsing intermediate pushes, which coalescing tolerates), for published releases and for workflow runs within Actions retention. It is not implemented here and is recorded for 05-02. |

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
