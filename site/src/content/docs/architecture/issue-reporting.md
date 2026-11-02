---
title: "Introspection: reporting, sweep, triage and fixes"
---

Introspection is the first-party extension [`extensions/introspection`](https://github.com/open-conduit/open-conduit/tree/main/extensions/introspection), which the daemon composes through the [in-process extension contract](../extensions/#subprocess-extensions) like any other first-party extension. It lets agents report what breaks in daily use and turns those reports, and the corrections and problems agents note, into improvements:

- `report-issue`, with report processing and triage;
- the agents' notes: the corrections they are given and the problems they work around;
- the daily sweep and triage as the extension's own scheduled Work;
- evidence aggregation over reports and the agents' notes;
- fix Work that traces to the report through causation;
- extension-originated reports from an optional host watch;
- telemetry events;
- an explicit removal policy.

It uses only the public `extension/inproc` surfaces and host services (registration, toolbelt, route, Work kind and timer-recurrence policies, timers, root acceptance of its own families, Work cancellation, coordination KV, telemetry capture and observability). Nothing about it is core: self-improvement needs no core code.

## Configuration

A scope's `introspection` entry enables the extension for that scope and the scopes that inherit it. It takes no secrets. An empty config enables reporting alone; each optional block adds one feature:

```json
{"extensions": [{"name": "introspection", "type": "introspection", "secret_names": [], "config": {
   "notes": ["team.notes"],
   "sweep":  {"cron": "41 8 * * *", "time_zone": "UTC", "rules": ["CLAUDE.md"], "guide": "docs/rules.md", "land": "pr", "remote": "origin"},
   "triage": {"cron": "51 8 * * *", "land": "pr", "file_issues": true},
   "host_watch": {"interval_seconds": 60, "load": 1.5, "memory": 90, "disk": 90, "disks": ["~/work"]},
   "telemetry": true}}],
 "routes": [
   {"name": "reports", "handler": "introspection", "binding": "report-agent", "active_policy": "queue", "config": {}},
   {"name": "sweep",   "handler": "introspection", "binding": "sweep",  "active_policy": "queue", "config": {"work": "sweep"}},
   {"name": "triage",  "handler": "introspection", "binding": "triage", "active_policy": "queue", "config": {"work": "triage"}},
   {"name": "fixes",   "handler": "introspection", "binding": "fix",    "active_policy": "queue", "config": {"work": "fix"}}],
 "bindings": [
   {"name": "agent",  "executor": "claude-code", "operations": ["…", "introspection.report-issue", "introspection.correction", "introspection.problem"], "…": "…"},
   {"name": "sweep",  "executor": "claude-code", "operations": ["introspection.evidence", "introspection.notes.resolve"],
    "config": {"playbook": "sweep"}, "resource": {"provider": "git", "config": {"mode": "shared", "…": "…"}}, "…": "…"},
   {"name": "triage", "executor": "claude-code", "operations": ["introspection.evidence", "introspection.reports", "introspection.fix",
    "introspection.report.resolve", "introspection.notes.resolve"], "config": {"playbook": "triage-reports"}, "…": "…"},
   {"name": "fix",    "executor": "claude-code", "operations": ["introspection.reports", "introspection.report.resolve"],
    "config": {"playbook": "fix-report"}, "resource": {"provider": "git", "config": {"mode": "work", "…": "…"}}, "…": "…"}]}
```

| Field | Meaning |
| --- | --- |
| `notes` | Further coordination KV namespaces whose records (the same JSON shape) the sweep and triage read besides the extension's own `introspection.notes`. None by default. This is a data contract, not an import. |
| `sweep`, `triage` | Standard five-field `cron` and IANA `time_zone`. If a block is absent, that run is not scheduled; it can still run by hand. `rules`/`guide` are sweep settings. `land` is `pr` (the default) or `push`, and `remote` defaults to `origin`. A push lands the agents' corrections on the main branch with nobody reviewing them, so it is an explicit opt-in. `file_issues` is a triage setting. |
| `host_watch` | Reading interval, load per CPU / memory % / disk % thresholds (defaults 1.5/90/90) and extra disk paths besides the state directory's. If absent, the watch is off. |
| `telemetry` | Writes the extension's telemetry events to `logs/introspection-telemetry.jsonl`. |

The `introspection` route's `work` is `report` (the default), `sweep`, `triage` or `fix`. Each route gives that kind of Work to the binding the user chose. No executor is built in: without a route, the extension's Events are kept but not processed. The scope's `authorization.operations` ceiling must list each operation, and each binding names the operations it may call. Registering an operation grants nothing.

## Work kinds, Events and keys

| Work kind | Created by | Correlation key (source `introspection/<scope>`) | Work ID |
| --- | --- | --- | --- |
| `introspection.report` | an `introspection.issue.reported` Event | `report/<id>` | `introspection/<scope>/report/<id>` |
| `introspection.sweep` (standing) | an `introspection.sweep.due` slot | `sweep/<epoch>` | `introspection/<scope>/sweep/<epoch>` |
| `introspection.triage` (standing) | an `introspection.triage.due` slot | `triage/<epoch>` | `introspection/<scope>/triage/<epoch>` |
| `introspection.fix` | an `introspection.fix.requested` Event | `fix/<id>` | `introspection/<scope>/fix/<id>` |

The sweep and triage are standing Work: the extension declares `Standing` for their kinds through the contract's `WorkKinds` surface, the same policy scheduled jobs use. A completed run leaves the Work ready and idle for its next slot. A slot that arrives while a run is going waits as one pending run. A failed run arms no automatic retry, because the next slot is its next attempt. The sweep, triage and fix kinds are also `RunOnce`: each run is the whole of its work, so a run whose agent hands back (a `done` status, say) has finished, and fix Work completes rather than waiting for input that never comes. A by-hand run is ordinary manual Work of any other kind on the same binding, for example `open-conduit work create --scope-id eng --kind manual.triage --binding triage`.

## Operations

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.report-issue` | `kind` (`env`, `docs`, `tooling`, `service`, `runtime`, `other`), `title`, `body` | Accepts one report Event and its index record in one transaction. |
| `introspection.reports` | `state`? (`open` default, `fixing`, `resolved`, `all`) | The calling scope's reports, oldest first. `fixing` is derived: an open report that a requested fix names. |
| `introspection.report.resolve` | `id`, `summary`, `link`? | Records how a report was handled. Repeating it changes nothing. |
| `introspection.evidence` | `for` (`sweep` or `triage`) | The run's evidence and settings. |
| `introspection.notes.resolve` | `notes` (ids), `summary` | Records the notes a run handled. |
| `introspection.fix` | `reports`? and/or `notes`?, `title`, `body` | Requests fix Work for one cause. |
| `introspection.correction` | `area` (`writing`, `design`, `data`, `code`, `workflow`; default `workflow`), `body` | Logs a correction the agent was given, one per request. |
| `introspection.problem` | `kind` (`env`, `docs`, `tooling`, `service`, `other`), `body` | Logs a problem the agent worked around, one per request. |

Reports are translated emissions. See [the report Event](#the-report-event) below.

`introspection.report-issue`, `introspection.correction` and `introspection.problem` carry their own agent guidance (`extension.Operation.Guidance`, in [`extensions/introspection/guidance`](https://github.com/open-conduit/open-conduit/tree/main/extensions/introspection/guidance)): when to log, the `body` shape and what a refusal means. The toolbelt serves it in the MCP instructions of every execution whose binding grants the operation, so the GitHub, tracker and Slack playbooks no longer carry their own copies (issue #208).

## The sweep and triage schedules

The runs are timed by the extension's own calendar, not by a `schedule` entry, because they are scheduled Work of their own kinds, defined by the introspection extension. Each configured block gets a durable core timer `introspection/<scope>/<run>` whose recurrence is the extension's `RecurrenceFunc` (`introspection.v1`), declared through the contract's `Recurrences` surface and armed through the host's timers. The calendar rules are those of [scheduled jobs](../schedule-extension/):

- A new schedule waits for its next slot.
- A firing publishes the latest slot strictly within six hours, unless that slot ran last, and rearms. A slot missed by six hours or more is skipped.
- An edited timing catches up its latest slot within six hours, unless it ran.
- On DST nights, a skipped wall-clock time has no run, and a repeated one runs once by name.

`introspection.Ensure` reconciles the timers in the extension's task: after every configure (startup and each committed apply) and every minute. A dry run arms nothing.

A slot Event's identity is its instant, so a replayed firing is deduplicated. Its key carries the schedule's **epoch**, the time its timer was first armed, which edits keep. A schedule that is removed and added again therefore starts a new standing Work instead of addressing a retired one. Core keeps terminal ownership final.

## Evidence aggregation

`introspection.evidence` gives a run:

- **Triage**:
  - the scope's open reports, grouped by kind and normalized leading line (the title);
  - the scope's unhandled `problem` notes, grouped by kind and leading line of the body;
  - `fixing`: open reports a requested fix already covers.
- **Sweep**: the scope's unhandled `correction` notes, grouped by area and leading line.
- **Settings**: `notes`, `rules`, `guide`, `land`, `remote`, `file_issues` and the schedule, read from the current generation.

Each group has its count, the report or note ids, the distinct Work they came from, the distinct correlation keys of the Work notes came from, first and last times, and up to two sample bodies. A group of notes also has `provenance`: each note's id, the Work that logged it and its origin (below). The groups are a starting point. The playbooks still group by cause.

`introspection.correction` and `introspection.problem` write the extension's own namespace `introspection.notes` (`<kind>/<time>/<digest>`). A note records the logging Work's first correlation key rather than any source's issue number, so a GitHub, tracker or Slack Work logs notes the same way. It also records its `origin`, read from the store when it is logged: the Event that triggered the logging execution (id, type and source) and the Work's display as its source set it (title, ref, first link and requester). Neither needs the extension to know the source. The evidence reads that namespace, then the configured ones, through public storage, filtered by the note's `scope`. A note's id is `<namespace>:<key>`. The extension never writes another extension's namespace. `introspection.notes.resolve` records a handled note in `introspection.notes` too (`handled/<scope>/<id>`), and later evidence leaves it out. Records in other namespaces stay intact.

Provenance is there because a correction becomes a standing instruction. Text in an issue or comment can talk an agent into logging a "correction" that is really a stranger's instruction, and once a sweep folds it into `CLAUDE.md` every later session carries it. So the sweep lands through a PR by default, and its playbook lists every folded correction with its provenance in the PR body, so a reviewer can spot one that came from an untrusted author. The requester is who opened the Work, not who wrote the text that triggered the logging turn: on an owner's issue a stranger's comment still shows the owner. `domain.Event` has no source-neutral author, so the triggering event's author is not recorded, and filtering corrections by author membership is not built either; both wait on a source-neutral notion of the actor. Until then provenance can't vouch for a correction, so the sweep playbook drops every correction that asks agents to fetch or install something from outside the project, run a downloaded script, send data out or loosen a check, whoever the provenance names.

## Fix work and cited causation

`introspection.fix` is a translated emission whose Event is addressed to the fix's own key. The `introspection` route (`work: fix`) gives it an `introspection.fix` Work on the user's fix binding. Its record lives in `introspection.fixes` (`fix/<scope>/<sequence>/<id>`) and holds:

- the reports, their Events and the notes it addresses;
- the requesting execution, its Work and its trigger;
- the cause, the fix Event, its root and depth, and the fix Work.

The fix Event **cites the earliest report's Event as its cause**. This uses the generic `extension.EmitPlan.Cause` (see [extensions](../extensions/) and [runtime semantics](../runtime-semantics/#cited-causation)). The fix's chain is therefore:

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

Each crossing over the threshold becomes an **extension-originated report** in every enabled scope that watches the host: a root `introspection.issue.reported` Event in the scope's report namespace, of kind `env` and origin `host-watch`, with the same index record. It is accepted with its record in one transaction (`is.HostReport`, the host's `AcceptInTx` for the extension's own families, `is.PutOriginInTx`), and the crossing's identity deduplicates a repeat.

From there it is an ordinary report:

- it is processed on the report route's binding;
- it is listed in the triage evidence;
- a fix for it cites it, so the fix traces to the reading.

The watch state is in memory, so after a restart a reading that is still over counts as a new crossing. The doctor check lists readings over threshold (`host_over_threshold`).

## Telemetry events

Named product-telemetry events (`introspection_report`, `introspection_fix`, `introspection_run_due`, `host_metrics`, `host_alert`) are an optional adapter on the public observability contract:

- `introspection.Exporter` is an `observability.Exporter` the extension offers through the contract's `Exporter` surface, so it joins the lifecycle pipeline. It turns the committed `event.accepted` records of the extension's families into `introspection_report`, `introspection_fix` and `introspection_run_due`. Each appears exactly once.
- The host watch captures `host_metrics` (with the fullest disk first) and `host_alert` the same way: both go through the host's `Capture` service to its JSON-lines telemetry events log.

Nothing is written unless an enabled entry sets `telemetry`. When it is on, enabled PostHog entries that set `telemetry_events` receive the same events through a fan-out sink ([PostHog](../installed-integrations/#posthog)).

Core reports a fired timer's Event as exactly one `event.accepted` record, which `TestLifecycleTelemetryRetriesAndRecovery` asserts.

## Removal policy

Removing the `introspection` entry, or one of its schedules, takes effect on the next generation:

- **Operations and routes.** The operations are undeclared, the route matches nothing (even for Events in flight), and bindings that still list the operations keep running, because the extension is entry-gated (`inproc.EntryGated`: its registration joins only a generation with an entry) and the host narrows grants to registered operations.
- **Timers.** `Ensure` cancels the schedule's timers. A run already going carries on.
- **Existing Work.** Work of the extension's kinds is **retired** once idle. The rules are in `is.Retired`:
  - every non-terminal, not-running Work of the extension's kinds in a configured scope that has no entry is retired;
  - standing sweep or triage Work whose run key is no longer a live timer's is retired (its schedule was removed or replaced);
  - the extension cancels retired Work through the host's `CancelWork`, the ordinary operator cancellation, and the reason `introspection removed from configuration`;
  - a running attempt finishes first, and its calls to the operations are undeclared. Standing Work that is idle afterwards is cancelled by the next pass.
- **Records.** Reports, fixes, handled notes, Events and run history stay readable. Retiring cancels Work and never deletes records.
- **Disabled scopes.** Disabling a scope is not removal. Its timers and Work wait for re-enable (B05), as scheduled jobs do. Configure carries only enabled scopes' entries, so the extension reads a disabled scope's entry from its stored configuration.

No core code changes. `scripts/check-boundaries` keeps core packages from importing the extension. Its `TestNoPrivilegedReportingPath` finds no introspection, reporting, triage, sweep-slot, fix-request or host-watch vocabulary in core, contract or operator packages.

## The report Event

A report is a **translated emission** (`extension.Operation.Emit`). The handler validates the input and returns an `EmitPlan`. The toolbelt accepts the Event exactly as it accepts the builtin `emit`:

- a family the same registration declared, which the token allows;
- trusted source `execution/<id>` and the execution's trigger as causation;
- the chain-depth and per-chain rate limits.

The plan's KV records go in the same transaction. Nothing in the plan can select a root, another Work or a namespace the registration does not own. The only cause a plan can name is one of the registration's own Events, as above.

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

- **`sweep`** corrections are folded into the rule files and landed as a PR (or a push when `land` is `push`), then resolved.
- **`triage-reports`** starts from the evidence and groups by cause. Causes in the project's own files get a requested fix instead of an inline commit, so each fix is its own traceable Work. Remaining causes are filed when `file_issues` is set, or else written up. Handled reports and problems are resolved.
- **`fix-report`** makes the smallest edit, lands it and resolves the fix's reports.

Open Conduit's own reports are the upstream intake.

## Evidence

- [`integration/introspection_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/introspection_test.go) runs production router, scheduler, toolbelt and timers on SQLite:
  - `TestWorkflowDReportTriageFix`: timers armed and kept; slots fired on a fake clock into standing sweep and triage; triage evidence covering reports and the GitHub tools' problem notes across scopes; a fix citing the earliest report, replayed once, with refusals; a fix Work traced fix → report → engineering root and naming the triage execution; reports resolved by the fix; the sweep folding corrections; a restart, and the next day seeing only what is left.
  - `TestCitedCausationKeepsCoreLimits`: a shallow cause cannot escape the depth limit.
  - `TestCitedCauseContract`: only the registration's own same-scope Events can be cited, and both chains' budgets are charged.
  - `TestIntrospectionRemovalPolicy`: timers cancelled, idle Work retired, running work finishing first, records kept, re-add as a new epoch, a disabled scope kept, schedule-only removal.
  - `TestExtensionOriginatedReportChain`: a host report is its own root, processed, triaged and fixed.
- [`integration/reporting_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/reporting_test.go) is the reporting suite: a report from an engineering execution, the agents' notes (`TestAgentNotes`), report-about-a-report loops stopped at the chain depth, a burst hitting the chain rate, permission boundaries, the translated-emission contract and disabled reporting.
- [`integration/daemon_introspection_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_introspection_test.go) (in `make drill`) uses the real daemon with shell executors calling `os`:
  - `TestDaemonIntrospection` (workflow D): an engineering report; the standing triage caught up by a config edit; evidence and a fix request; the fix Work resolving the report; `events chain` fix → report → operator root, the fix source naming the triage run and the fix trigger being the fix Event; telemetry events exactly once; SIGKILL and restart keeping the schedule; removal by apply retiring the standing triage, with the operations undeclared and the records kept.
  - `TestDaemonHostWatch`: a disk over threshold becomes a root report processed on the report binding, doctor shows `host over`, and host telemetry is written.
- [`integration/daemon_reporting_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_reporting_test.go) (`TestDaemonIssueReporting`): a report loop stopped at depth 4, retention across SIGKILL, by-hand triage and disable by apply.
- Unit tests cover config, routes, the calendar (catch-up, six hours, DST, epochs), the host watcher, the telemetry mapping, rendering, the registration and each playbook's operations.

## Limitations

- **No recorded real-use evidence.** The evidence above uses fake clocks, fakes and shell stand-ins; real reports, a real triage and a real fix are not recorded here.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 03-08 delivered issue reporting, a sequence-step-3 requirement of the planning document: the agents' own `report-issue` and triage surface what breaks in early daily use. It added the report Event contract, the `introspection.reports` index, the `report` route kind, bootstrap's narrowing of grants to registered operations, and the suites `integration/reporting_test.go` and `TestDaemonIssueReporting`. Its live-use validation was assigned to 03-09, which was deferred.
- Ticket 05-03 completed the workload: the daily sweep and triage as the extension's own scheduled Work, evidence aggregation, fix Work with cited causation (the then-new generic `extension.EmitPlan.Cause`, after which a plan could name one of its own Events as cause), extension-originated reports from the host watch, telemetry events, the removal policy and the `fix-report` playbook. Its telemetry work found a core defect, a fired timer's Event reported as two `event.accepted` records, which was fixed and is now asserted by `TestLifecycleTelemetryRetriesAndRecovery`.
- At 05-03, telemetry went only to the JSON-lines log; a PostHog sink was left to 06-01, which added it ([installed integrations](../installed-integrations/#posthog)).
- Real use (real reports, a real triage and a real fix) was left to the deferred 03-09.
- Issue #70 moved the extension onto the in-process contract (bootstrap's `introspection.go` went away) and, per #67's plan, replaced the GitHub tools' `github.correction`/`github.problem` and their `github.notes` namespace with `introspection.correction`/`introspection.problem`.
