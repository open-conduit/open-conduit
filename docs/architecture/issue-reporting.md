# Issue reporting through public extension hooks

Ticket 03-08 delivers the PRD's sequence-step-3 requirement: the agents' own `report-issue` and triage surface what breaks in early daily use, without hard-coding introspection into the runtime. Reporting is the first-party extension [`extensions/introspection`](../../extensions/introspection), composed by [`internal/bootstrap/introspection.go`](../../internal/bootstrap/introspection.go). It uses only the public registration, toolbelt, route and coordination KV interfaces. Stage 05 (05-03) builds the complete introspection workload on it: sweep, schedules, evidence aggregation and the report lifecycle. Live-use validation is 03-09.

## Configuration

A scope's `introspection` entry enables reporting for that scope and the scopes that inherit it. Its config is an empty object and it takes no secrets. Processing is bound explicitly: an `introspection` route names a binding the user chose. No executor is built in, and without the route reports are kept but not processed.

```json
{"extensions": [{"name": "reports", "type": "introspection", "config": {}, "secret_names": []}],
 "routes": [{"name": "reports", "handler": "introspection", "binding": "report-agent", "active_policy": "queue", "config": {}}],
 "bindings": [
   {"name": "agent", "executor": "claude-code", "operations": ["…", "introspection.report-issue"], "…": "…"},
   {"name": "report-agent", "executor": "claude-code", "operations": ["introspection.report-issue"], "…": "…"},
   {"name": "triage", "executor": "claude-code", "operations": ["introspection.reports", "introspection.report.resolve"],
    "config": {"playbook": "triage-reports"}, "…": "…"}]}
```

The scope's `authorization.operations` ceiling must also list each operation. Registering an operation grants nothing: each binding names the operations it may call.

## Operations and the report Event

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.report-issue` | `kind` (`env`, `docs`, `tooling`, `service`, `runtime`, `other`), `title` (one line, up to 200 bytes), `body` (up to 8 KiB) | Accepts one `introspection.issue.reported` Event and writes its index record in one transaction. Returns `reported`, `event`, `root`, `depth` and `duplicate`. |
| `introspection.reports` | `state`? (`open` by default, `resolved` or `all`) | The calling scope's reports, oldest first. |
| `introspection.report.resolve` | `id`, `summary`, `link`? | Records how triage handled a report. Repeating it changes nothing. |

A report is a **translated emission**, the new generic `extension.Operation.Emit` contract (see [extensions](extensions.md)). The handler validates the input and returns an `EmitPlan`. The toolbelt accepts the Event exactly as it does the builtin `emit`:

- a family the same registration declared, which the token allows;
- trusted source `execution/<id>` and the execution's trigger as causation;
- the chain-depth and per-chain rate limits.

The plan's KV records go in the same transaction. Nothing in the plan can select a root, a parent or another Work, or write outside the registration's own namespaces.

Each report has the correlation key `introspection/<scope>` / `report/<id>`. No Work owns that key, and it lies in the registration's own source namespace. The Event is therefore addressed to the key rather than to the reporting Work: its envelope has no Work or Execution, but its source and causation still name the execution. The `introspection` route then gives each report an `introspection.report` Work of its own (`introspection/<scope>/report/<id>`) on the route's binding. The processing execution's trigger is the report.

The report's identity is its request ID, else its input, under the reporting execution. A retried call never reports twice, and a reused request ID with other input is `idempotency_conflict`. Reports never hold or wait: they are not GitHub issues. This keeps v1's `TestUpstreamIssueIsNeverHeld` guarantee.

## The report index (coordination KV)

The namespace `introspection.reports` has one record per report at `report/<scope>/<acceptance sequence>/<id>`. A record holds:

- the id, scope, kind, title and body;
- the reporting Work and execution, and the trigger the report derives from;
- the report Event, its causal root and depth;
- the key, the acceptance time and the state (`open` or `resolved`), plus the resolution: summary, link, triage Work and execution, and time.

The reporter fields come from validated toolbelt authority, never from the caller. Records and Events are ordinary SQLite state and survive restarts.

The report Event's causal chain is visible with `open-conduit events chain <event>`, from the external root through every report to the execution that made it. Its routed Work and the processing run show in `runs` and the TUI like any other.

## Triage as ordinary manual Work

The `triage-reports` playbook ports v1's `internal/playbooks/triage.md` onto these operations: list, group by cause, fix plainly wrong project files in one commit, file the rest, and resolve what was handled. It joins the Claude Code plugin with the other playbooks. During the early switch it runs by hand as ordinary Work:

```sh
open-conduit work create --scope-id eng --kind introspection.triage --binding triage
```

Stage 05 (05-03) schedules sweep and triage with the [schedule extension](schedule-extension.md) and aggregates evidence. It also completes the GitHub notes (`github.problem`/`github.correction` in `github.notes`) that this bootstrap leaves to it. Filing issues uses whatever tracker access the triage's binding has. `github.*` tools only work on a GitHub issue's Work, so a hand-started triage writes the remaining causes in its summary instead. v1's `--upstream` flag is not reproduced: Open Conduit's own reports are the upstream intake.

## Chain limits and permission boundaries

- **Report-about-a-report loops.** Each processing execution's report derives from its trigger, which is the previous report. Depth grows by one per round and core rejects the round past `causal_max_depth`, with a durable `depth_limit` diagnostic. The extension does nothing to cooperate. A burst from one chain hits `causal_per_minute` (`rate_limit`). A refused report leaves no Event, record or Work. The refusal returns `429 causal_limit` to `os`/MCP.
- **Bindings.** A processing binding that does not grant `introspection.report-issue` is refused (`forbidden`). Only a binding that grants the triage operations can list or resolve reports.
- **Scopes.** A scope without an entry is refused with `not_enabled`. Listing and resolving reach only the calling scope's reports.
- **The contract itself.** A plan naming another registration's or a core family is `undeclared`. One that presets source, causation or target, addresses a key in a foreign namespace, or writes a foreign KV namespace (a lookalike prefix included) is `forbidden`, and nothing it planned is committed.

## Disabling and removal

The operations and the report family are registered on a generation's toolbelt only when an enabled scope of that generation has an `introspection` entry. To turn reporting off, remove or tombstone the entry and apply the config. On the next generation:

- calls to the operations are `undeclared`;
- the route matches nothing, even for reports already in flight;
- bindings that still list the operations keep running, because bootstrap narrows a grant to the operations registered for the generation;
- existing reports, Events and processing Work stay readable, and the operator decides what to do with that Work.

No core code changes. `scripts/check-boundaries` keeps core packages from importing the extension, and its `TestNoPrivilegedReportingPath` finds no reporting, introspection or triage vocabulary in core, contract or operator packages.

## Regression found

The daemon applied the configured `causal_max_depth`/`causal_per_minute` to routing, timers and control, but not to the toolbelt. An execution's first emission off a new root therefore pinned the default limits (32/128) on it. Bootstrap now passes the configured limits to the toolbelt, and `TestDaemonIssueReporting` would catch it: its loop must stop at the configured depth of 4.

## Evidence

- [`integration/reporting_test.go`](../../integration/reporting_test.go) runs the production router, scheduler and toolbelt on SQLite. It covers:
  - a report from an engineering execution: replay, input dedup, request conflict, invalid input, envelope, causal trace and index record;
  - the user-bound processing Work and its trigger;
  - manual triage list and resolve;
  - retention across reopen;
  - the depth-limited loop with its diagnostic;
  - the per-minute burst, with no record left by a refusal;
  - binding, scope and cross-scope boundaries;
  - route stop on entry removal;
  - the translated-emit contract against a rogue registration;
  - disabled composition being undeclared.
- [`integration/daemon_reporting_test.go`](../../integration/daemon_reporting_test.go) (`TestDaemonIssueReporting`, in `make drill`) runs the real daemon with shell bindings calling `os`. An engineering report starts a processing loop that core stops at depth 4. Every report traces to the operator's root. After a SIGKILL and restart every report is retained, and triage runs by hand to list and resolve them. Config apply removes the entry: the agent's call is `undeclared` while its binding keeps running.
- Unit tests cover config, enablement, the route, rendering, the registration and the playbook's operation names.
