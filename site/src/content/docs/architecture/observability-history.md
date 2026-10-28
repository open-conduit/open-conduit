---
title: "Lifecycle telemetry, unified run history and doctor"
description: "How Open Conduit reports lifecycle telemetry, keeps one run history for every executor, applies retention and runs doctor checks."
---

Open Conduit reports every committed lifecycle change as telemetry, keeps one run history for every executor, applies one retention policy and answers health and doctor checks. This implements the [B03 observability contract](../extensions/#required-lifecycle-telemetry) for the runtime. There are three parts:

- **Instrumentation** in [`internal/runtime/telemetry.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/telemetry.go).
- **Export** in [`internal/telemetry`](https://github.com/open-conduit/open-conduit/tree/main/internal/telemetry): a buffered pipeline, the OpenTelemetry mapping and a local JSON log exporter.
- **Inspection**: the [history](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/history.go), [retention](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/retention.go) and [doctor](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/doctor.go) runtime reads, served by [management](https://github.com/open-conduit/open-conduit/blob/main/internal/management/history.go) and presented by the [client](https://github.com/open-conduit/open-conduit/blob/main/client/client.go), the [`open-conduit` commands](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/history.go), [`runview`](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/runview/runview.go) and the [TUI](https://github.com/open-conduit/open-conduit/blob/main/internal/tui/tui.go).

Exporter configuration (OTLP endpoints, PostHog), host watches and connection reachability probes belong to extensions. The daemon composes the pipeline, its exporters and SDK providers, the retention sweep and the doctor inputs into `open-conduit serve` ([daemon composition](../daemon-core-gate/)).

## Instrumentation

`runtime.Instrument(store, TelemetryOptions{Observer, Diagnostic})` wraps coordination storage, and every runtime component is composed with the wrapped store. Inside each transaction the wrapper captures the pre-state of every record it touches:

- Work, Executions, Timers and Waits
- attempt, retry and ingress-scan ledgers
- accepted and routed Events, lease renewals and releases
- causal rejections and retention expirations

After the callback succeeds, still inside the transaction, it compares pre-state with post-state and resolves correlation. It reports records only after the commit returns. Records therefore come from the durable writes themselves:

- No runtime path (routing, scheduling, waits, timers, toolbelt, operator control, reconcile, retention) can change state without telemetry.
- A rolled-back transaction emits nothing.
- A state that existed only inside a transaction is never reported. Intermediate states collapse to the committed transition.

Instrumentation failures never fail the transaction or the caller: a panic while resolving drops the records, and refusals go to `Diagnostic`. A record that cannot be anchored to a trusted causal Event, such as a root timer armed with no Event, is dropped with the `uncorrelated_record` diagnostic rather than given a fabricated identifier.

The toolbelt adds `toolbelt.called` and `toolbelt.rejected` records through `ToolbeltOptions.Observer`, for every call whose credential resolves to an execution authority. A call that presents no valid credential has no trusted correlation, so it is not recorded.

### Records

| Record | Source | Key attributes |
| --- | --- | --- |
| `event.accepted`, `event.duplicate` | Acceptance, including timer fires and executor emissions | type, source, sequence, causation, `emitted_by_execution` |
| `route.decided` | Routing checkpoint (`To` = disposition) | route name, binding, generation, reason |
| `causal.rejected` | Committed causal refusal | reason, depth, count, root, limits |
| `work.created`, `work.transitioned` | Work (`From`/`To`) | kind, revision, parent |
| `execution.queued` | New attempt | trigger ID, type and trace |
| `execution.started`, `.resumed`, `.recovered` | Claim from pending, parked or running at a higher epoch | trigger, executor registration, generation, deadline, lease owner/epoch; previous epoch and owner on recovery |
| `execution.launched` | Launch acknowledgement (`launch.mode` run, recover/adopt or resume) | epoch, executor |
| `execution.parked`, `execution.{completed,failed,waiting,handoff,cancelled}` | Execution state | outcome kind and summary, duration, artifact count, stop state, resource exclusion, executor usage as `Usage` |
| `execution.stop_confirmed` | Trusted `ConfirmStopped` | epoch |
| `execution.retention_expired` | Retention ledger | artifact count, kept count |
| `lease.renewed`, `lease.released` | Lease | epoch, owner, expiry |
| `retry.scheduled`, `retry.unblocked`, `retry.blocked` | Retry eligibility | failure count, eligible-at, blocked reason, exhaustion, retry event |
| `timer.armed`, `timer.fired`, `timer.cancelled` | Timers, with fire and rearm as one commit | timer ID, event type, fire time, recurring |
| `wait.registered`, `wait.woken`, `wait.cancelled` | Waits | types, key count, timeout timer, wake winner ID and type |
| `toolbelt.declared` | Declared wait or outcome | declaration kind |
| `toolbelt.called`, `toolbelt.rejected` | Toolbelt | operation, safe result code |
| `ingress.scan_started`, `_page`, `_completed`, `_partial`, `_failed` | Reconcile scan ledger | source, scan ID, safe error code, failures, next due |

Every record carries a `record_id` attribute. For state transitions it is derived from the entity and revision. For toolbelt calls it uses a per-call nonce. Exporter retries and duplicate deliveries are therefore identifiable.

### Correlation and traces

Correlation always comes from trusted runtime state, never from an executor payload:

- **Execution records** carry Work, Execution, scope, executor binding and the attempt's persisted `TraceID`. The causal Event is the current segment's trigger, which after a resume is the wake winner.
- **Event records** use the acceptance scope. If an execution emitted the Event, they use that execution's correlation and trace. Otherwise the trace is a deterministic identity of the Event's causal root.
- **Work records** are anchored to the Event routed or accepted for that Work in the same commit, then to the trigger of an attempt touched in that commit.
- **Timer records** are anchored to the Event that armed them, or for a fire, the Event the fire emitted.

Recovery keeps the attempt trace and records the new epoch. `execution.queued` carries `trigger.trace_id`, so an operator can follow an Event chain into the attempt it started.

A reconcile scan is host-polled, so no trigger Event exists to anchor it. Ingress scan records (`ingress.` prefix) are instead anchored by their `ingress.source` attribute and a trace derived from source and scan. `observability.Validate` accepts that anchor only for `ingress.` records, and no Event ID is fabricated. A pre-acceptance push rejection is returned synchronously to the ingress caller and is not a lifecycle record. The shared observer case rejects an unanchored ingress record.

A fired timer's Event is reported once as `event.accepted`, not once by the acceptance and again by `FireTimer`.

## Export pipeline

`telemetry.Pipeline` implements `observability.Observer`. `Observe` validates, applies `observability.Redact` (known secret values from `Options.Secrets`, sensitive keys) and enqueues in constant time into one bounded queue per exporter (default 4096). When a queue is full, its oldest record is dropped and the drop is counted. Each exporter runs on its own goroutine with:

- batching (128 records or 1 s)
- a bounded call (10 s)
- panic isolation
- retry with backoff (3 attempts), after which the batch is dropped

A hung exporter that ignores its context occupies only its own goroutine. A failing exporter never delays a commit, a scheduling decision or an execution outcome, and never starves sibling exporters. Diagnostics carry only safe codes, never exporter error text. `Stats` and `Health` expose observed, exported, dropped and failed counts.

Delivery is at most once, from memory: records still queued at a crash are lost and not replayed. The authoritative record of what happened is durable coordination history, read through the management API, and `record_id` makes duplicates harmless. `Shutdown` drains within its context and counts any remainder as dropped.

`telemetry.OTel` is an exporter that maps records onto the OpenTelemetry API, using a host-supplied `TracerProvider`, `MeterProvider` and `LoggerProvider` (no-op when nil). The host owns SDK setup and the exporter extensions.

- **Spans**: one span per record (`open_conduit.*` attributes; failures and rejections get error status). The span's W3C trace ID comes deterministically from the correlation trace: the first 128 bits of a runtime SHA-256 trace, otherwise a hash. Its parent is a deterministic virtual span for the attempt, or for the causal Event before an attempt exists. Spans emitted across restarts therefore join the same trace. Terminal execution spans cover the attempt duration.
- **Logs**: one log record per lifecycle record, carrying the span context, event name, severity (WARN for failures, rejections, recoveries, retries and duplicates) and the same attributes.
- **Metrics**:
  - `open_conduit.lifecycle.records`, by record name and generic from/to state
  - `open_conduit.execution.duration` in seconds, by binding and outcome
  - `open_conduit.execution.tokens`, by binding and token type
  - `open_conduit.execution.cost`, by binding and currency

  Identifiers never become metric attributes.

`telemetry.LogExporter` writes the same records as local JSON lines, for operation without a collector. Executors report cost and tokens through `extension.Result.Usage` on their structured outcome. The usage appears as `Usage` on the terminal record and in run history.

Host watch and telemetry events are extension adapters on the public observability contract, with no runtime branch ([introspection](../issue-reporting/)):

- **Telemetry exporter.** `introspection.Exporter` is an `observability.Exporter` in the lifecycle pipeline. It turns the committed `event.accepted` records of the extension's families into named telemetry events, each once, written to `logs/introspection-telemetry.jsonl` only while an entry sets `telemetry`.
- **Host watch.** The optional host watch runs as a pass of the daemon loop and reports threshold crossings as extension-originated reports.

## Unified run history and TUI

`runtime.Run` is one attempt's history record, with the same shape for every executor. It contains:

- the execution record (state, attempt, lease epoch, outcome, artifacts, trace)
- scope and Work kind/state
- trigger envelope, plus the resumed segment's trigger when it differs
- executor registration and pinned config generation
- launch state and resource exclusion
- deadline and remaining timeout budget, and duration
- active segments (`segments` and `active_ms` in `GET /v1/runs`): the first start and each resume after a wake (with the waking event's ID), each ended by a park or by the run's finish. A recovery after a lost lease ends the open segment at the lease's expiry and starts one marked `recovered`, so a daemon outage is not counted. The segment running now has no end; the client's `Run.Active(now)` adds it. `active_ms` sums the ended segments, so a turn parked for days counts only the time it ran, while `duration_ms` spans start to finish. A run keeps its last 50 segments; `segments_dropped` counts older ones, whose time stays in `active_ms`. A run that was already parked when segments were introduced reports none, rather than a partial record
- usage and input count
- wait and wake links
- the Work's retry eligibility when this is the latest failed attempt
- retention status
- the resource the attempt ran in (`workspace` in `GET /v1/runs`), from the executor's generic `extension.Result.Workspace`: kind, lease reference, directory and the provider's one-line summary

Nothing in it interprets executor payloads or Work kinds. Management serves it at `GET /v1/runs`, filtered by `scope_id`, `work_id`, `state`, `executor_binding`, `executor`, `work_kind` or `outcome`, with the standard keyset pagination, and at `GET /v1/runs/{id}` with an ETag. `GET /v1/runs/{id}/live?tail=N` returns a running run's outputs so far (each at most its last `N` bytes, 64 KiB by default) through the executor's optional `extension.LiveExecutor`, in the order its artifacts will have; a run that is not running answers `409 not_live`, and an executor without live output `422 live_unsupported`.

Operator commands:

- `open-conduit runs list|get` returns JSON.
- `runs table` prints the shared text rows.
- `runs log ID` streams the run's log artifact, which is the first text artifact. On a running run it prints the output so far, and `--follow` keeps printing new output until the run ends. `--transcript` picks the transcript (the JSON-lines output) instead.
- `open-conduit tui` opens the interactive view.

The TUI is a bubbletea model over the client only. It provides a refreshing list with state filters, a detail view with trigger, timeout, usage, retry, retention, artifacts and every attempt of the Work, and a log view that tails the last 64 KiB with scrolling. On a running run the log view follows the live output, refetching on every tick and staying at the end unless scrolled up; once the run ends it settles on the artifacts. The list shows one row per Work by default: its latest turn, its turn count and its name, which is the first annotation an extension gives it (`repo#12` for a GitHub issue) or else its first correlation key. `g` switches to one row per turn, each marked as turn `k/n` of its Work. Rows and details are rendered by `runview`, so shell, job and agent attempts go through the same code. The `os` binary carries no terminal UI: the entrypoint injects the TUI through `cli.Env`. The TUI keeps one run model and adds operator controls and Work context as data; see [operator setup](../operator-setup/). `open-conduit web` serves the same model to a browser from [`internal/web`](https://github.com/open-conduit/open-conduit/blob/main/internal/web/web.go). It is also a client only, uses the same `runview` helpers, and is injected through `cli.Env` so the `os` binary carries no web UI. It serves loopback only, behind a printed sign-in URL; see [SECURITY.md](https://github.com/open-conduit/open-conduit/blob/main/SECURITY.md).

`runview.Detail` prints an attempt's workspace, so the CLI and TUI show a shell job's fresh worktree or stale shared checkout exactly as they would an agent's issue worktree. Shell jobs and agent turns share this record, the retention policy (`kept_executions`, default 20) and the deadline model (`settings.timeout_seconds`, overridable per binding). See [job run parity](../schedule-extension/#job-runs).

Issue reports are ordinary derived Events, so `events chain <report event>` traces a report from its external root through every report to the execution that made it. Each processing run is an ordinary `introspection.report` Work in `runs` and the TUI. Report records live in the extension's coordination KV namespace `introspection.reports`, not in run history ([issue reporting](../issue-reporting/)). A fix's trace is `events chain <fix event>`: fix → report → the report's origin. The fix Event's source names the requesting triage execution.

## Retention

One kept-executions policy (B05 `kept_executions`, default 20 per Work, `SchedulerOptions.KeptExecutions`) applies to every executor. `Scheduler.SweepRetention(ctx, limit)` keeps the newest N attempts of each Work. An older attempt is expired only if it is terminal and its resource exclusion is confirmed; an uncertain hold is never released. The sweep runs in phases:

1. Commit an expiration ledger entry with the attempt's artifact references, which emits `execution.retention_expired`.
2. Outside the transaction, delete the content through `storage.Artifacts`, then call the optional `extension.ForgetExecutor.Forget` to release executor-private state.
3. Record the release.

A crash between phases is completed by a later sweep.

Coordination history (records, outcomes, usage and artifact metadata) is never deleted. The history shows `retention.state=expired`. Artifact content then returns 404 `artifact_expired`, and the TUI reports expiry instead of fetching the content.

The shell executor's `Forget` removes the execution's run directories. It refuses (`ErrLiveSegment`) and never signals while a verified process still runs. The host decides when to sweep: the daemon loop runs `SweepRetention` every `--retention-interval` (default 10 minutes), then collects artifact content that no execution references and that is more than 24 hours old ([artifact recovery and retention](../sqlite-artifacts/#recovery-and-retention)).

## Doctor

`GET /v1/doctor[?check_secrets=true]` and `open-conduit doctor [--check-secrets] [--json]` return `ok`/`info`/`warn`/`fail` checks with safe codes and identifiers. The CLI exits 1 on `fail`. The checks are:

| Check | Meaning |
| --- | --- |
| `storage` | A read/write coordination transaction commits. |
| `lease_recovery` | Running attempts holding expired leases that await recovery. |
| `resource_holds` | Finished attempts still holding capacity without a confirmed stop. |
| `retention` | Expired attempts with release pending. |
| `reconcile_lag` | Sources that are failing (safe code only), have no complete scan, or are older than two intervals. |
| `non_rederivable` | Declared delivery families with their loss statements. |
| `config` | The active generation against the config file: `pending_edit`, invalid or unreadable. |
| `bindings` | Each binding resolves to a registered executor that satisfies its required capabilities. |
| `connections` | Declared connections have registered types. Reachability is extension-specific. |
| `secrets` | Resolved only with `check_secrets`, reporting names and codes, never values or references. |
| `pinned_executors` | Live or held attempts whose pinned generation or registration is not installed. |

Extension checks (`management.Options.Checks`), such as `telemetry.Pipeline.Health` for exporters or host watches, are appended. Provider error text never appears. The daemon contributes, among others, the `github` (registration, rate limit, reconcile and projection per entry), `github_capability`, `webhooks` and `claude_code` checks, plus a `github_auth` deep check that runs only with `check_secrets=true`; management's `DeepChecks` option carries it. An `introspection` check lists each entry's schedules (next slot), host readings over threshold, telemetry and note namespaces.

When no daemon is reachable (it has never run, so there is no operator token, or nothing listens on its socket), `open-conduit health` says so and to run `open-conduit serve`. `open-conduit doctor` then runs its offline checks instead: `daemon` (`warn`, `not_running`), `config` validated offline, `github_capability` and `claude_code` (`claude --version` for each claude-code binding). A `skipped` check names the checks that need the daemon. A missing token file while a daemon is listening is still reported as a token error.

## Evidence

- **[Telemetry integration](https://github.com/open-conduit/open-conduit/blob/main/integration/telemetry_test.go)**:
  - The real shell and `os` workflow answers from records alone: why each attempt started (route, trigger and wake winner), who ran it (binding, executor, generation, owner), what it emitted (emission, child Work, toolbelt calls and the refused post-wait call) and why it stopped (declared wait, wake, completion). It also checks work transition order, trace continuity, record identity and redaction of secrets and tokens.
  - Retry failure, backoff timer, fire, unblock and the retried attempt's trigger, plus lease recovery with adoption on the original trace.
  - A hung and a failing exporter beside a healthy one do not block durable progress, are isolated, are accounted for and surface through doctor without error text.
  - All shared runtime cases pass through instrumented production components with every record validating.
  - Rollback emits nothing.
- **[History integration](https://github.com/open-conduit/open-conduit/blob/main/integration/history_test.go)**:
  - A real shell attempt and fake agent and job attempts appear in one record schema, filters, rows, detail, TUI navigation and log view, with usage, retry status and attempts.
  - Shared retention expires only the older attempt, keeps its metadata, releases content and executor state (including real shell run directories) and is idempotent.
  - Doctor covers baseline, check_secrets, scan failure, expired lease and unapplied edit, with ingress scan telemetry, and the CLI.
- **[Pipeline and OTel unit tests](https://github.com/open-conduit/open-conduit/tree/main/internal/telemetry)**: the shared observer contract; non-blocking drop accounting under a hung exporter; retries, panics and safe diagnostics; JSON log redaction; and the real OpenTelemetry SDK for span trace and parent identity, attempt duration, error status, correlated logs and low-cardinality metrics with usage.
- **TUI and shell unit tests**: TUI filters, detail, log tail and scroll, errors and expired content; shell `Forget` live-segment refusal and idempotency.

- `TestLifecycleTelemetryRetriesAndRecovery` asserts that a fired timer's Event is reported once.

These tests compose the host in-process. The [daemon composition](../daemon-core-gate/) drills assert lifecycle traces through real process kills of `open-conduit serve`.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 02-08 implemented lifecycle instrumentation, the export pipeline, the single run history and TUI, retention and the health/doctor checks, with the host composed in-process by tests. It also clarified B03 so that host-polled ingress scan records are anchored by their source rather than a trigger Event, and extended the shared observer case to reject an unanchored ingress record. Daemon composition, including the poll loop that calls the retention sweep, was left to 02-09, which wired the pipeline, SDK providers, retention schedule and doctor inputs into `open-conduit serve` and added the kill drills.
- Ticket 03-05 added the daemon's `github`, `github_capability`, `webhooks` and `claude_code` checks, the `github_auth` deep check and the TUI's operator controls and Work context.
- Ticket 03-08 made issue reports ordinary derived Events traceable with `events chain`.
- Ticket 05-02 added the workspace to `runtime.Run` and `runview.Detail` for job run parity.
- Ticket 05-03 added the introspection telemetry exporter, host watch and `introspection` doctor check. It also fixed a regression in which a fired timer's Event was reported as two `event.accepted` records, once by the acceptance and once by `FireTimer`, and added the assertion to `TestLifecycleTelemetryRetriesAndRecovery`.
