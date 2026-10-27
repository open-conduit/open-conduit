# Introspection: reporting, sweep, triage and fixes

Introspection is the first-party extension [`extensions/introspection`](../../extensions/introspection), composed by [`internal/bootstrap/introspection.go`](../../internal/bootstrap/introspection.go). It uses only the public registration, toolbelt, route, timer-recurrence, coordination KV and observability interfaces. Nothing about it is core (PRD workflow D: "no core code for self-improvement").

- Ticket 03-08 delivered issue reporting, the PRD's sequence-step-3 requirement: the agents' own `report-issue` and triage surface what breaks in early daily use. Its live-use validation is 03-09 (deferred).
- Ticket 05-03 completed the workload:
  - the daily sweep and triage as the extension's own scheduled Work;
  - evidence aggregation over reports and the agents' notes;
  - fix Work that traces to the report through causation;
  - extension-originated reports from an optional host watch;
  - telemetry events;
  - an explicit removal policy.

## Configuration

A scope's `introspection` entry enables the extension for that scope and the scopes that inherit it. It takes no secrets. An empty config enables reporting alone; each optional block adds one feature:

```json
{"extensions": [{"name": "introspection", "type": "introspection", "secret_names": [], "config": {
   "notes": ["github.notes"],
   "sweep":  {"cron": "41 8 * * *", "time_zone": "UTC", "rules": ["CLAUDE.md"], "guide": "docs/rules.md", "land": "pr", "remote": "origin"},
   "triage": {"cron": "51 8 * * *", "land": "push", "file_issues": true},
   "host_watch": {"interval_seconds": 60, "load": 1.5, "memory": 90, "disk": 90, "disks": ["~/work"]},
   "telemetry": true}}],
 "routes": [
   {"name": "reports", "handler": "introspection", "binding": "report-agent", "active_policy": "queue", "config": {}},
   {"name": "sweep",   "handler": "introspection", "binding": "sweep",  "active_policy": "queue", "config": {"work": "sweep"}},
   {"name": "triage",  "handler": "introspection", "binding": "triage", "active_policy": "queue", "config": {"work": "triage"}},
   {"name": "fixes",   "handler": "introspection", "binding": "fix",    "active_policy": "queue", "config": {"work": "fix"}}],
 "bindings": [
   {"name": "agent",  "executor": "claude-code", "operations": ["…", "introspection.report-issue"], "…": "…"},
   {"name": "sweep",  "executor": "claude-code", "operations": ["introspection.evidence", "introspection.notes.resolve"],
    "config": {"playbook": "sweep"}, "resource": {"provider": "git", "config": {"mode": "shared", "…": "…"}}, "…": "…"},
   {"name": "triage", "executor": "claude-code", "operations": ["introspection.evidence", "introspection.reports", "introspection.fix",
    "introspection.report.resolve", "introspection.notes.resolve"], "config": {"playbook": "triage-reports"}, "…": "…"},
   {"name": "fix",    "executor": "claude-code", "operations": ["introspection.reports", "introspection.report.resolve"],
    "config": {"playbook": "fix-report"}, "resource": {"provider": "git", "config": {"mode": "work", "…": "…"}}, "…": "…"}]}
```

| Field | Meaning |
| --- | --- |
| `notes` | Coordination KV namespaces whose records the sweep and triage read (default `github.notes`, the GitHub tools' corrections and problems). This is a data contract (namespace and JSON shape), not an import. |
| `sweep`, `triage` | Standard five-field `cron` and IANA `time_zone`. If a block is absent, that run is not scheduled; it can still run by hand. `rules`/`guide` are sweep settings. `land` is `push` or `pr`, and `remote` defaults to `origin`. `file_issues` is a triage setting. |
| `host_watch` | Reading interval, load per CPU / memory % / disk % thresholds (defaults 1.5/90/90) and extra disk paths besides the state directory's. If absent, the watch is off. |
| `telemetry` | Writes the extension's telemetry events to `logs/introspection-telemetry.jsonl`. |

The `introspection` route's `work` is `report` (the default, as in 03-08), `sweep`, `triage` or `fix`. Each route gives that kind of Work to the binding the user chose. No executor is built in: without a route, the extension's Events are kept but not processed. The scope's `authorization.operations` ceiling must list each operation, and each binding names the operations it may call. Registering an operation grants nothing.

## Work kinds, Events and keys

| Work kind | Created by | Correlation key (source `introspection/<scope>`) | Work ID |
| --- | --- | --- | --- |
| `introspection.report` | an `introspection.issue.reported` Event | `report/<id>` | `introspection/<scope>/report/<id>` |
| `introspection.sweep` (standing) | an `introspection.sweep.due` slot | `sweep/<epoch>` | `introspection/<scope>/sweep/<epoch>` |
| `introspection.triage` (standing) | an `introspection.triage.due` slot | `triage/<epoch>` | `introspection/<scope>/triage/<epoch>` |
| `introspection.fix` | an `introspection.fix.requested` Event | `fix/<id>` | `introspection/<scope>/fix/<id>` |

The sweep and triage are standing Work: bootstrap declares the generic `runtime.WorkKindPolicy{Standing: true}` for their kinds, the same policy scheduled jobs use. A completed run leaves the Work ready and idle for its next slot. A slot that arrives while a run is going waits as one pending run. A failed run arms no automatic retry, because the next slot is its next attempt. A by-hand run is ordinary manual Work of any other kind on the same binding, for example `open-conduit work create --scope-id eng --kind manual.triage --binding triage`.

## Operations

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.report-issue` | `kind` (`env`, `docs`, `tooling`, `service`, `runtime`, `other`), `title`, `body` | Accepts one report Event and its index record in one transaction (03-08). |
| `introspection.reports` | `state`? (`open` default, `fixing`, `resolved`, `all`) | The calling scope's reports, oldest first. `fixing` is derived: an open report that a requested fix names. |
| `introspection.report.resolve` | `id`, `summary`, `link`? | Records how a report was handled. Repeating it changes nothing. |
| `introspection.evidence` | `for` (`sweep` or `triage`) | The run's evidence and settings. |
| `introspection.notes.resolve` | `notes` (ids), `summary` | Records the notes a run handled. |
| `introspection.fix` | `reports`? and/or `notes`?, `title`, `body` | Requests fix Work for one cause. |

Reports are 03-08's translated emissions. See [the 03-08 contract](#the-report-event-03-08) below.

## The sweep and triage schedules

The runs are timed by the extension's own calendar, not by a `schedule` entry, because the PRD makes them "scheduled Work of their own kinds, defined by the introspection extension". Each configured block gets a durable core timer `introspection/<scope>/<run>` whose recurrence is the extension's `RecurrenceFunc` (`introspection.v1`), registered with the generic `runtime.TimerOptions.Recurrences`. The calendar rules are those of [scheduled jobs](schedule-extension.md):

- A new schedule waits for its next slot.
- A firing publishes the latest slot strictly within six hours, unless that slot ran last, and rearms. A slot missed by six hours or more is skipped.
- An edited timing catches up its latest slot within six hours, unless it ran.
- On DST nights, a skipped wall-clock time has no run, and a repeated one runs once by name.

`introspection.Ensure` reconciles the timers at startup, at every committed apply and every minute. A dry run arms nothing.

A slot Event's identity is its instant, so a replayed firing is deduplicated. Its key carries the schedule's **epoch**, the time its timer was first armed, which edits keep. A schedule that is removed and added again therefore starts a new standing Work instead of addressing a retired one. Core keeps terminal ownership final.

## Evidence aggregation

`introspection.evidence` gives a run:

- **Triage**:
  - the scope's open reports, grouped by kind and normalized leading line (the title);
  - the scope's unhandled `problem` notes, grouped by kind and leading line of the body;
  - `fixing`: open reports a requested fix already covers.
- **Sweep**: the scope's unhandled `correction` notes, grouped by area and leading line.
- **Settings**: `notes`, `rules`, `guide`, `land`, `remote`, `file_issues` and the schedule, read from the current generation.

Each group has its count, the report or note ids, the distinct Work they came from, the issue numbers notes name, first and last times, and up to two sample bodies. The groups are a starting point. The playbooks still group by cause.

Notes are read from the configured namespaces through public storage, filtered by the note's `scope`. A note's id is `<namespace>:<key>`. The extension never writes another extension's namespace. `introspection.notes.resolve` records a handled note in its own namespace `introspection.notes` (`handled/<scope>/<id>`), and later evidence leaves it out. The GitHub tools' records stay intact.

## Fix work and cited causation

`introspection.fix` is a translated emission whose Event is addressed to the fix's own key. The `introspection` route (`work: fix`) gives it an `introspection.fix` Work on the user's fix binding. Its record lives in `introspection.fixes` (`fix/<scope>/<sequence>/<id>`) and holds:

- the reports, their Events and the notes it addresses;
- the requesting execution, its Work and its trigger;
- the cause, the fix Event, its root and depth, and the fix Work.

The fix Event **cites the earliest report's Event as its cause**. This uses the new generic `extension.EmitPlan.Cause` (see [extensions](extensions.md) and [runtime semantics](runtime-semantics.md#cited-causation-05-03)). The fix's chain is therefore:

```
origin (e.g. core.work.created of the engineering Work)
  └─ introspection.issue.reported   (source execution/<engineering attempt>)
       └─ introspection.fix.requested (source execution/<triage attempt>)
            └─ trigger of the fix Work's execution
```

`open-conduit events chain <fix event>` shows it. The fix Event's source names the triage execution that asked for it. The fix execution resolves its reports with a link to the commit or PR. A report-processing execution can also request a fix, because its trigger is the report.

Citing is bounded by core, not by the extension:

- Only an accepted Event of the emitting Work's scope whose key lies in the registration's own source namespace can be cited. Anything else is `forbidden`.
- The cited Event's depth is one more than the deeper of the cause and the trigger, checked against the stricter limit. A shallow cause never resets a chain.
- The cited Event is charged to both chains' per-minute budgets.

## Extension-originated reports and the host watch

With `host_watch`, a host pass reads the machine's load, memory and disks (`extensions/introspection/host`). An alert fires when a reading goes over its threshold, and recovery under 95% of it.

Each crossing over the threshold becomes an **extension-originated report** in every enabled scope that watches the host: a root `introspection.issue.reported` Event in the scope's report namespace, of kind `env` and origin `host-watch`, with the same index record. It is accepted with its record in one transaction (`is.HostReport`, `runtime.AcceptInTx`, `is.PutOriginInTx`), and the crossing's identity deduplicates a repeat.

From there it is an ordinary report:

- it is processed on the report route's binding;
- it is listed in the triage evidence;
- a fix for it cites it, so the fix traces to the reading.

The watch state is in memory, so after a restart a reading that is still over counts as a new crossing. The doctor check lists readings over threshold (`host_over_threshold`).

## Telemetry events

Named product-telemetry events (`introspection_report`, `introspection_fix`, `introspection_run_due`, `host_metrics`, `host_alert`) are an optional adapter on the public observability contract:

- `introspection.Exporter` is an `observability.Exporter` that bootstrap adds to the lifecycle pipeline. It turns the committed `event.accepted` records of the extension's families into `introspection_report`, `introspection_fix` and `introspection_run_due`. Each appears exactly once.
- The host watch captures `host_metrics` (with the fullest disk first) and `host_alert` on the same JSON-lines sink.

Nothing is written unless an enabled entry sets `telemetry`. A PostHog sink belongs to 06-01.

A defect this found was fixed: core reported a fired timer's Event as two `event.accepted` records. `TestLifecycleTelemetryRetriesAndRecovery` now asserts exactly one.

## Removal policy

Removing the `introspection` entry, or one of its schedules, takes effect on the next generation:

- **Operations and routes.** The operations are undeclared, the route matches nothing (even for Events in flight), and bindings that still list the operations keep running, because bootstrap narrows grants to registered operations (03-08).
- **Timers.** `Ensure` cancels the schedule's timers. A run already going carries on.
- **Existing Work.** Work of the extension's kinds is **retired** once idle. The rules are in `is.Retired`:
  - every non-terminal, not-running Work of the extension's kinds in a configured scope that has no entry is retired;
  - standing sweep or triage Work whose run key is no longer a live timer's is retired (its schedule was removed or replaced);
  - bootstrap cancels retired Work with the ordinary operator cancellation and the reason `introspection removed from configuration`;
  - a running attempt finishes first, and its calls to the operations are undeclared. Standing Work that is idle afterwards is cancelled by the next pass.
- **Records.** Reports, fixes, handled notes, Events and run history stay readable. Retiring cancels Work and never deletes records.
- **Disabled scopes.** Disabling a scope is not removal. Its timers and Work wait for re-enable (B05), as scheduled jobs do.

No core code changes. `scripts/check-boundaries` keeps core packages from importing the extension. Its `TestNoPrivilegedReportingPath` finds no introspection, reporting, triage, sweep-slot, fix-request or host-watch vocabulary in core, contract or operator packages.

## The report Event (03-08)

A report is a **translated emission** (`extension.Operation.Emit`). The handler validates the input and returns an `EmitPlan`. The toolbelt accepts the Event exactly as it accepts the builtin `emit`:

- a family the same registration declared, which the token allows;
- trusted source `execution/<id>` and the execution's trigger as causation;
- the chain-depth and per-chain rate limits.

The plan's KV records go in the same transaction. Nothing in the plan can select a root, another Work or a namespace the registration does not own. Since 05-03, the only cause a plan can name is one of its own Events, as above.

Each report has the correlation key `introspection/<scope>` / `report/<id>`. No Work owns that key, and it lies in the registration's own source namespace. The Event is therefore addressed to the key: its envelope has no Work or Execution, but its source and causation still name the execution. The report's identity is its request ID, else its input, under the reporting execution. A retried call never reports twice, and a reused request ID with other input is `idempotency_conflict`. Reports never hold or wait.

The namespace `introspection.reports` has one record per report at `report/<scope>/<acceptance sequence>/<id>`. A record holds:

- the id, scope, kind, title and body;
- the reporting Work, execution and trigger, or the origin for an extension-originated report;
- the report Event, its causal root and depth, and the key;
- the acceptance time and the state, plus the resolution.

## Chain limits and permission boundaries

- **Report-about-a-report loops.** Each processing execution's report derives from its trigger, the previous report. Depth grows by one per round, and core rejects the round past `causal_max_depth` with a durable `depth_limit` diagnostic. The extension does nothing to cooperate. A burst from one chain hits `causal_per_minute`. A refused report leaves no Event, record or Work, and the refusal returns `429 causal_limit` to `os`/MCP. Cited causes cannot escape either limit.
- **Bindings.** An operation a binding does not grant is `forbidden`. For example, a sweep binding cannot request fixes.
- **Scopes.** A scope without an entry is refused with `not_enabled`. Listing, evidence, resolving and fixes reach only the calling scope's reports and notes.
- **The contract itself.** A plan naming another registration's or a core family is `undeclared`. One that presets source, causation or target, addresses a key in a foreign namespace, cites a foreign or cross-scope Event, or writes a foreign KV namespace is `forbidden`, and nothing it planned is committed.

## Playbooks

The playbooks join the Claude Code plugin with the other playbooks, and every introspection action goes through the `introspection.*` operations:

- **`sweep`** corrections are folded into the rule files and landed as a push or PR, then resolved.
- **`triage-reports`** starts from the evidence and groups by cause. Causes in the project's own files get a requested fix instead of an inline commit, so each fix is its own traceable Work. Remaining causes are filed when `file_issues` is set, or else written up. Handled reports and problems are resolved.
- **`fix-report`** (new) makes the smallest edit, lands it and resolves the fix's reports.

Open Conduit's own reports are the upstream intake.

## Evidence

- [`integration/introspection_test.go`](../../integration/introspection_test.go) runs production router, scheduler, toolbelt and timers on SQLite:
  - `TestWorkflowDReportTriageFix`: timers armed and kept; slots fired on a fake clock into standing sweep and triage; triage evidence covering reports and the GitHub tools' problem notes across scopes; a fix citing the earliest report, replayed once, with refusals; a fix Work traced fix → report → engineering root and naming the triage execution; reports resolved by the fix; the sweep folding corrections; a restart, and the next day seeing only what is left.
  - `TestCitedCausationKeepsCoreLimits`: a shallow cause cannot escape the depth limit.
  - `TestCitedCauseContract`: only the registration's own same-scope Events can be cited, and both chains' budgets are charged.
  - `TestIntrospectionRemovalPolicy`: timers cancelled, idle Work retired, running work finishing first, records kept, re-add as a new epoch, a disabled scope kept, schedule-only removal.
  - `TestExtensionOriginatedReportChain`: a host report is its own root, processed, triaged and fixed.
- [`integration/reporting_test.go`](../../integration/reporting_test.go) is 03-08's reporting suite.
- [`integration/daemon_introspection_test.go`](../../integration/daemon_introspection_test.go) (in `make drill`) uses the real daemon with shell executors calling `os`:
  - `TestDaemonIntrospection` (workflow D): an engineering report; the standing triage caught up by a config edit; evidence and a fix request; the fix Work resolving the report; `events chain` fix → report → operator root, the fix source naming the triage run and the fix trigger being the fix Event; telemetry events exactly once; SIGKILL and restart keeping the schedule; removal by apply retiring the standing triage, with the operations undeclared and the records kept.
  - `TestDaemonHostWatch`: a disk over threshold becomes a root report processed on the report binding, doctor shows `host over`, and host telemetry is written.
- [`integration/daemon_reporting_test.go`](../../integration/daemon_reporting_test.go) (`TestDaemonIssueReporting`) is 03-08's loop stopped at depth 4, retention across SIGKILL, by-hand triage and disable by apply.
- Unit tests cover config, routes, the calendar (catch-up, six hours, DST, epochs), the host watcher, the telemetry mapping, rendering, the registration and each playbook's operations.

Real use (real reports, a real triage and a real fix) is recorded under the deferred 03-09.
