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

### Audit ledger

Telemetry is best-effort on purpose: an exporter's queue drops its oldest records when full, and a batch is dropped after its retries. The audit ledger is the durable trail of administrative calls, kept in coordination storage beside the state it describes (`storage.Tx.AppendAudit`, `AuditRecords`). Each record (`domain.AuditRecord`) has these fields:

- `Actor` and `Via`: who made the call, as the host authenticated it. Both are empty when the credential was refused.
- `Action`: the management route (`POST /v1/config/apply`), or for a control or action, the type of the Event it made.
- `Target`: what the call acted on (`work/ID`, `credential/TOKEN_ID`, `secrets/NAME`, `auth/issuers/ID`), empty for the daemon as a whole.
- `Outcome`: `ok`, or the code the call was refused or failed with.
- `EventID`: the Event a control or action made, which holds the rest of what it did.
- `Detail`: a few facts that are never secret, such as the generation and hash an apply published, the operator and role a credential was issued for, or the ID of a revoked credential someone tried.

Where each record is written:

- **In the change's own transaction.** A control's or action's record is written where its Event is accepted (`runtime.auditAccepted`), for an Event that the host stamped with an actor. A config or include apply's record is written with the generation it publishes. A credential issued, rotated or revoked, or an issuer trusted or removed, is recorded with that write. The change and its record commit together or not at all.
- **Once the call returns.** A change storage can't join (a secret in the keyring or file, a release's binaries, an attach) is recorded after it is made. If that write fails, the call's answer still stands, and the daemon logs the `audit_unrecorded` diagnostic.
- **On refusal.** An operator's audited call that fails is recorded with its code. So is any operator request refused for its role, scope or subject (`403`, reads included). A request refused for its credential (`401`, on any route, the toolbelt's included) is recorded as that refusal and nothing more, whichever route it asked for. A refused credential the host issued is named by its token ID, never its token. Someone with no credential must not be able to grow a ledger that is never deleted, so these records have a budget: `AuthFailureLimit` (10) per `AuthFailureWindow` (a minute) for each network client, execution socket and the owner's socket, with as many clients tracked as the listener's throttle tracks. A request the listener throttles (`429`) is never recorded.

Successful reads, route traces and config validation are not recorded, and neither are an execution's toolbelt calls once its credential resolves (`toolbelt.called` and `toolbelt.rejected` record those).

Storage numbers the records 1, 2, 3 with no gaps (a rolled-back append takes no number), stamps them with the transaction's time, and chains them. Each record's `Prev` is the previous record's `Hash`, and its `Hash` is `Digest()`, a SHA-256 over every other field in a fixed length-prefixed encoding. A same-user process can still rewrite the local database. A copy held elsewhere can, though, tell a missing, reordered or rewritten record from the chain alone (`domain.VerifyAuditChain`). The storage case `audit_ledger_numbers_chains_and_pages` holds a backend to this. Its negative controls are a store that numbers from a counter a rollback advances, and one that drops a field.

`GET /v1/audit?after=N&limit=M` (admin; [HTTP API](../../reference/http-api/#audit-ledger)) and `client.Audit` read the ledger by sequence. An exporter package (an ordinary extension holding an operator credential, with no privileged hook) streams it to a SIEM: it asks again after the last sequence it holds, checks each page continues the chain, and resumes from that sequence after a restart. Each record also goes out as `audit.recorded` telemetry once it commits. The ledger is coordination history, and like the rest of it, it is never deleted.

Not yet recorded: reads of run output and artifact content, and the CLI's offline operations (`auth recover`, extension install, update and remove), which the daemon never sees.

### Records

| Record | Source | Key attributes |
| --- | --- | --- |
| `event.accepted`, `event.duplicate` | Acceptance, including timer fires and executor emissions | type, source, sequence, causation, `emitted_by_execution`, and who caused it when the host knows (`event.actor`, `event.via`: a control's operator, an action's caller) |
| `audit.recorded` | An audit ledger record committed ([audit ledger](#audit-ledger)) (`To` = outcome) | `audit.sequence`, `audit.action`, `audit.outcome`, `audit.actor`, `audit.via`, `audit.target`, `audit.event_id`, `audit.prev`, `audit.hash` and each fact of its detail as `audit.detail.<key>`. It may describe a request that authenticated no one, so it is anchored by `audit.sequence` and needs no scope; one that points at an Event carries that Event's correlation |
| `route.decided` | Routing checkpoint (`To` = disposition) | route name, binding, generation, reason; for `failed`, the error (a panic's with its stack) and attempts; for `unmatched`, `route.refusals`: each route that declined the Event and its handler's reason, as `route: reason`, one a line |
| `causal.rejected` | Committed causal refusal | reason, depth, count, root, limits |
| `work.created`, `work.transitioned` | Work (`From`/`To`) | kind, revision, parent |
| `execution.queued` | New attempt | trigger ID, type and trace |
| `execution.started`, `.resumed`, `.recovered` | Claim from pending, parked or running at a higher epoch | trigger, executor registration, generation, deadline, lease owner/epoch; previous epoch and owner on recovery |
| `execution.launched` | Launch acknowledgement (`launch.mode` run, recover/adopt or resume) | epoch, executor |
| `execution.launch_phase` | A launch phase ended (`To` its outcome) | `launch.phase`, `launch.outcome` (`ok`, `failed`, `cancelled`), `launch.duration_ms`, `launch.started_at`, epoch, segment |
| `execution.parked`, `execution.{completed,failed,waiting,handoff,cancelled}` | Execution state | outcome kind and summary, the turn's declared state (`turn.status`), start time, duration, the trigger's type and time (`trigger.type`, `trigger.occurred_at`), Work kind, artifact count, stop state, resource exclusion, executor usage as `Usage` |
| `execution.stop_confirmed` | Trusted `ConfirmStopped` | epoch |
| `execution.retention_expired` | Retention ledger | artifact count, kept count |
| `lease.renewed`, `lease.released` | Lease | epoch, owner, expiry |
| `retry.scheduled`, `retry.unblocked`, `retry.blocked` | Retry eligibility | failure count, eligible-at, blocked reason, exhaustion, retry event |
| `timer.armed`, `timer.fired`, `timer.cancelled` | Timers, with fire and rearm as one commit | timer ID, event type, fire time, recurring |
| `wait.registered`, `wait.woken`, `wait.cancelled` | Waits | types, key count, timeout timer, wake winner ID and type |
| `toolbelt.declared` | Declared wait or outcome | declaration kind |
| `toolbelt.called`, `toolbelt.rejected` | Toolbelt | operation, safe result code, and for a refusal which check refused it (`toolbelt.refusal`, [toolbelt](../toolbelt-management/)) |
| `ingress.scan_started`, `_page`, `_completed`, `_partial`, `_failed` | Reconcile scan ledger | source, scan ID, safe error code, failures, next due; for `provider_failed`, the provider's failure class, code and bounded message (`ingress.error_class`, `ingress.error_code`, `ingress.error_message`) |
| `provider.timing_out`, `provider.answering` | An action provider's actions or panels call, or an annotating extension's annotations call, starts timing out across requests, or answers again (management's provider health) | `provider.name`, `provider.call` (`actions`, `panels` or `annotations`), `provider.since`, `provider.timeouts`, and for `timing_out` the time it is next asked (`provider.retry_at`); correlated with the scope and Work whose view saw it |
| `operator.secret_stored`, `operator.secret_removed`, `operator.secrets_listed` | An admin's call to `PUT`, `DELETE` or `GET /v1/secrets` ([secrets](../config-management/#secrets-and-process-environment)), refused or not | `operator.actor` (`operator:<name>`), `operator.outcome` (`ok` or the refusal's code), the secret's name (`operator.target`) and for a store the store asked for or used (`operator.store`); never a value. No scope or Event precedes the call, so it carries neither |
| `auth.credential_verified`, `auth.credential_refused` | A trusted issuer's verifier asked about a bearer credential the host didn't issue, once per credential (a cached one is not asked again) ([authentication](../toolbelt-management/#local-management-api)) | `auth.issuer`, `auth.type`, the first 16 hex characters of the credential's digest (`auth.credential`), and the operator it named (`operator.actor`) or the refusal's `auth.code` and the verifier's `auth.reason` (at most 300 bytes, the token redacted); never the token. It precedes any operator, so it is anchored by `auth.issuer` and carries no scope |
| `secret.resolved`, `secret.refused` | The host resolving a provider-backed secret reference through the package that serves its scheme (`secret.serve`): at an attempt's start, for a package's `secret.resolve`, or for doctor and `GET /v1/secrets` ([secrets](../config-management/#secrets-and-process-environment)) | `secret.scheme`, the secret's name in its slot when the caller has one (`secret.name`), the first 16 hex characters of the reference's digest (`secret.reference`; reference text is sensitive, and the value is never recorded), the package that served it (`secret.provider`), the package it was resolved for, if any (`secret.consumer`), `duration_ms`, and for a refusal its safe `secret.code`. It may precede any Event, so it is anchored by `secret.scheme` and carries no scope |
| `operator.issuer_set`, `operator.issuer_removed` | An admin's call to `PUT` or `DELETE /v1/auth/issuers/{id}`: trusting an identity provider issuer, changing its config or group table, or no longer trusting it ([authentication](../toolbelt-management/#local-management-api)), refused or not | `operator.actor`, `operator.outcome` and the issuer's ID (`operator.target`). No scope or Event precedes the call, so it carries neither |
| `operator.include_applied` | A call to `POST /v1/config/includes/{name}/apply`: a team, or an admin, applying a submitted include's fragment ([submitted includes](../config-management/#submitted-includes)), refused or not | `operator.actor`, `operator.outcome` and the include's name (`operator.target`). No Event precedes the call, so it carries none |
| `operator.release_installed` | A call to `POST /v1/releases/install`: an admin, usually through the web view's update page, installing the latest release over the daemon's binaries, refused or not (a role refusal comes before the call, so it isn't one) | `operator.actor`, `operator.outcome` (`ok`, `stale_release`, `updates_off`, `install_failed`, …) and the version asked for (`operator.target`). No Event precedes the call, so it carries none |

Every record carries a `record_id` attribute. For state transitions it is derived from the entity and revision. For toolbelt calls it uses a per-call nonce. Exporter retries and duplicate deliveries are therefore identifiable.

### Correlation and traces

Correlation always comes from trusted runtime state, never from an executor payload:

- **Execution records** carry Work, Execution, scope, executor binding and the attempt's persisted `TraceID`. The causal Event is the current segment's trigger, which after a resume is the wake winner.
- **Event records** use the acceptance scope. If an execution emitted the Event, they use that execution's correlation and trace. Otherwise the trace is a deterministic identity of the Event's causal root.
- **Work records** are anchored to the Event routed or accepted for that Work in the same commit, then to the trigger of an attempt touched in that commit.
- **Timer records** are anchored to the Event that armed them, or for a fire, the Event the fire emitted.

Recovery keeps the attempt trace and records the new epoch. `execution.queued` carries `trigger.trace_id`, so an operator can follow an Event chain into the attempt it started.

A reconcile scan is host-polled, so no trigger Event exists to anchor it. Ingress scan records (`ingress.` prefix) are instead anchored by their `ingress.source` attribute and a trace derived from source and scan. `observability.Validate` accepts that anchor only for `ingress.` records, and no Event ID is fabricated. A pre-acceptance push rejection is returned synchronously to the ingress caller and is not a lifecycle record. The shared observer case rejects an unanchored ingress record. Provider health records (`provider.` prefix) are host-observed in the same way, when a view finds an action provider's or annotator's call timing out or answering again: they are anchored by their `provider.name` attribute and a trace derived from the provider, call and when it began timing out, and the shared observer case rejects one without it. An operator record (`operator.` prefix) is a daemon-wide administrative call (a secret stored, an issuer trusted, an include applied, a release installed), which neither an Event nor a scope precedes: it is anchored by the operator who made it (`operator.actor`) and a trace derived from the call, and it is the one kind of record that needs no scope. The shared observer case rejects one without its operator. An audit record (`audit.` prefix) is anchored by its ledger sequence (`audit.sequence`) and needs no scope either. The local daemon log keeps every record, but it rotates and an exporter's queue drops when full, so the complete trail is the [audit ledger](#audit-ledger), not telemetry.

A fired timer's Event is reported once as `event.accepted`, not once by the acceptance and again by `FireTimer`.

## Export pipeline

`telemetry.Pipeline` implements `observability.Observer`. `Observe` validates, applies `observability.Redact` (known secret values from `Options.Secrets`, which the daemon feeds the values operations with a secret output served ([secret outputs](../extensions/#secret-outputs)), and sensitive keys) and enqueues in constant time into one bounded queue per exporter (default 4096). When a queue is full, its oldest record is dropped and the drop is counted. Each exporter runs on its own goroutine with:

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
  - `open_conduit.host.pass.duration` in seconds, one per host loop pass, and `open_conduit.host.phase.duration` in seconds by phase (`open_conduit.phase`), recorded by the daemon itself rather than from records ([host loop](../daemon-core-gate/#host-loop))

  Identifiers never become metric attributes.

`telemetry.LogExporter` writes the same records as local JSON lines, for operation without a collector. Executors report cost and tokens through `extension.Result.Usage` on their structured outcome. The usage appears as `Usage` on the terminal record and in run history. Each result reports its own segment's usage; an attempt that parks and resumes reports the sum of its segments, kept in attempt state as `EarlierUsage` beside the latest result.

The host's usage reporter (`internal/bootstrap/usage.go`) is one more exporter in the pipeline. It only counts `work.created` and terminal `execution.*` records, and a release build sends those counts with the active configuration's shape (scope counts, first-party types) to the project's PostHog at start, daily and at stop. `serve --no-telemetry`, `OPEN_CONDUIT_NO_TELEMETRY` or `DO_NOT_TRACK` leave it out of the pipeline; [usage telemetry](/open-conduit/reference/telemetry/) lists what it sends.

Host watch and telemetry events are extension adapters on the public observability contract, with no runtime branch ([introspection](../issue-reporting/)):

- **Telemetry exporter.** `introspection.Exporter` is an `observability.Exporter` in the lifecycle pipeline. It turns the committed `event.accepted` records of the extension's families into named telemetry events, each once, written to `logs/introspection-telemetry.jsonl` only while an entry sets `telemetry`.
- **Host watch.** The optional host watch runs as a pass of the daemon loop and reports threshold crossings as extension-originated reports.

## Unified run history and TUI

`runtime.Run` is one attempt's history record, with the same shape for every executor. It contains:

- the execution record (state, attempt, lease epoch, outcome, artifacts, trace). Its `artifacts` are its outcome's, the last turn's; the artifacts each earlier turn published stay the attempt's (`domain.Execution.EarlierArtifacts`, kept when a later result replaces them), so they are listed, backed up, kept from collection and released by retention with the rest
- scope and Work kind/state
- trigger envelope, plus the resumed segment's trigger when it differs
- executor registration and pinned config generation
- launch state and resource exclusion
- the running turn's deadline, and duration
- `interrupt`, the envelope of the Event whose interrupt ended the run (beside `interrupt_event_id`): a `core.execution.timeout` one means the turn ended at its time limit and the Work carried on, which the CLI, TUI and web view show as "turn ended at its time limit" rather than a failure or a cancel (the client's `Run.TurnTimeLimit()`)
- active segments (`segments` and `active_ms` in `GET /v1/runs`), the time the attempt held its lease, each segment a turn with its own deadline ([attempt segments](../execution-scheduling/#attempt-segments)). A segment begins at the first start, at each resume after a wake and at each recovery (`recovered`). Any segment begun by a wake, resumed or a new attempt, names the waking event. It ends at a park (`parked`), at a recovery or at the run's finish, and the segment running now has no end; the client's `Run.Active(now)` adds it. `active_ms` sums the ended segments: a turn parked for days counts only the time it held a lease, while `duration_ms` spans start to finish. A run keeps its last 50 segments, and `segments_dropped` counts the older ones, whose time stays in `active_ms`. A segment of a sandboxed binding carries its `sandbox` pin (the plan's `digest` and `summary`, [B09](../sandboxes/#4-pinned-per-attempt)), so history shows the policy each segment ran under; the TUI's run detail lists each change of it. Each segment carries its `launch`: every phase (`name`, `started_at`, `ended_at`, `duration_ms`, `outcome`; one still running has no `ended_at`, and the client's `LaunchPhase.Took(now)` gives its time so far). The run page shows them in a Launch table under the turn's stats, and the TUI's run detail on one `launch` line. A segment in which recovery relaunched a launch that had started nothing is `relaunched` ([recovery and relaunch](../execution-scheduling/#recovery-and-relaunch)). Each segment names the `artifacts` its turn published (`id`, `size`, `media_type`): a parked turn's transcript and activity as well as the last turn's, each readable at `GET /v1/artifacts/{id}/content` for as long as the run's own, so a client that never read the run while it was parked can still show every turn. A turn the run ended while it ran (an operator's interrupt, an input's, its deadline, the Work's cancel) keeps what it wrote up to the stop too: the run is fenced before its process stops, and once the host has stopped the process it records the stopped turn's artifacts on the run and its segment without changing the run's state or outcome (`Scheduler.RecordStopped`, #647). Until then that ended segment carries `artifacts_pending: true`. It clears once the transcript is recorded or the stop is confirmed (`resource_excluded`). An ended segment without it has every artifact it will ever have, so a client waits for a transcript only while the segment says one may still come. `GET /v1/artifacts?execution_id=` lists all of an attempt's artifacts, earlier turns' first, each with the `segment` (counting from 1, dropped ones included) that published it; one from a segment the run no longer keeps has none. A run started before segments were recorded reports none, rather than a partial record
- attach containers (`attaches` in `GET /v1/runs`): each container an operator attached to the attempt's session in after it ended (`open-conduit container attach`), with when it was pinned and its plan, image and daemon, as a segment's container is shown ([attach in a container](../sandboxes/#attach-in-a-container)).
- usage and input count
- wait and wake links
- the Work's retry eligibility when this is the latest failed attempt
- retention status
- the resource the attempt ran in (`workspace` in `GET /v1/runs`), from the executor's generic `extension.Result.Workspace`: kind, lease reference, directory and the provider's one-line summary

Nothing in it interprets executor payloads or Work kinds. Management serves it at `GET /v1/runs`, filtered by `scope_id`, `work_id`, `state`, `executor_binding`, `executor`, `work_kind` or `outcome`, with the standard keyset pagination, and at `GET /v1/runs/{id}` with an ETag. A page filters on what each execution and its Work record say and builds the full record (attempt state, trigger, retry and retention) only for the runs it returns, checking the `executor` filter, which only the attempt state carries, as it builds them; `GET /v1/waits?state=active` likewise reads the active Waits through their index and each Wait's timer only for the page. `GET /v1/runs/{id}/live?tail=N` returns a running run's outputs so far (each at most its last `N` bytes, 64 KiB by default) through the executor's optional `extension.LiveExecutor`, in the order its artifacts will have; a run that is not running answers `409 not_live`, and an executor without live output `422 live_unsupported`.

Operator commands:

- `open-conduit runs list` prints the shared text rows (`runs table` is the same); `--json` prints the records.
- `runs get ID` prints the record as JSON.
- `runs log ID` streams the run's log artifact, which is the first text artifact. On a running run it prints the output so far, and `--follow` keeps printing new output until the run ends. `--transcript` picks the transcript (the JSON-lines output) instead.
- `open-conduit tui` opens the interactive view.

The TUI is a bubbletea model over the client only. It provides a refreshing list with state filters, a detail view with trigger, timeout, usage, retry, retention, artifacts and every attempt of the Work, and a log view that tails the last 64 KiB with scrolling. On a running run the log view follows the live output, refetching on every tick and staying at the end unless scrolled up; once the run ends it settles on the artifacts. The list shows one row per Work by default: its latest turn, its turn count and its name, which is the first annotation an extension gives it (`repo#12` for a GitHub issue) or else its first correlation key. `g` switches to one row per turn, each marked as turn `k/n` of its Work. Rows and details are rendered by `runview`, so shell, job and agent attempts go through the same code. The `os` binary carries no terminal UI: the entrypoint injects the TUI through `cli.Env`. The TUI keeps one run model and adds operator controls and Work context as data; see [operator setup](../operator-setup/). `open-conduit web` serves the same model to a browser from [`internal/web`](https://github.com/open-conduit/open-conduit/blob/main/internal/web/web.go). It is also a client only, uses the same `runview` helpers, and is injected through `cli.Env` so the `os` binary carries no web UI. `serve --web` runs it inside the daemon's process, still as a client of the socket. Its Runs and Needs you pages read each Work's runs by the Work's ID (`GET /v1/runs?work_id=`), keeping a finished Work's from the last read while its revision stands, since they change only when the Work does (a retry reopens it). So a page load reads the open Work and whatever changed, not every run ever made, and no Work's turns are cut off however many runs the daemon holds. Its status line offers a newer release when one is out, from a check the entrypoint's `upgrade.Updater` makes in the background at most every six hours, and its update page asks the daemon to install it (`POST /v1/releases/install`, as the person signed in). The daemon checks the admin role there, as for its other administrative routes, refuses the call over the network, installs only the latest release, and records each call in the operator trail as `operator.release_installed`. It installs through the same updater as `open-conduit upgrade` (`internal/cli/upgrade`), which verifies the release's signed `checksums.txt` with `internal/provenance` before trusting any checksum and never installs an unsigned release this way. The view then restarts the background service through `cli.ServiceControl` once it has answered, since the restart may stop its own process. With `--no-updates` (`serve --no-updates`, which also turns the daemon's route off with `updates_off`, or `web --no-updates`), for a binary a package manager, an image or a fleet manages, the view has no update page or notice and `/update` answers 404. It serves loopback only, behind a sign-in URL kept in the state directory across restarts, and answers only to its configured name, which it also takes without a port so that a reverse proxy on 443 can pass `Host` through (see [share it with a team](../../guides/team/)); see [SECURITY.md](https://github.com/open-conduit/open-conduit/blob/main/SECURITY.md).

An extension can contribute a page of its own to the web view ([pages](../extension-protocol/#pages)), listed in the navigation from `GET /v1/pages` and served at `/pages/<id>`. `internal/web` names no extension: it draws every page from the declaration, the scope's actions and the session Work's panels. With no session open it offers the page's start action on a scope with its entry, with the grants of its route's binding as checkboxes. With one open, the first panel section of messages is the conversation, drawn as a chat (an agent's text as Markdown, the operator's and notes as plain text, an answered question with the choice taken), and the session's other panels stand beside it. One message box sends the first of the page's compose actions the session offers, a waiting question's choices as buttons above it. When the session proposes configuration, core reviews it from `GET /v1/work/{id}/config-candidate`: each open or approved change with its pointer, reason and problems, its before and after side by side, and Approve and Reject buttons (rejecting takes an optional reason), Approve all and Validate when offered, and a table of every change with its state. Every button is the extension's own action, named by the candidate and shown only while the Work lists it, so each decision is still a `core.action` Event. Apply is core's: it reads the candidate and sends its text to `POST /v1/config/apply` as `config_text`, with the candidate's hash and its base generation as the expected generation, so a configuration that moved meanwhile is refused (`generation_conflict`) rather than overwritten. Once a change is applied, the page shows doctor's checks (without secrets) beside it. The page keeps no state of its own: panels, actions and the candidate are read afresh on every load, so a reload loses only an unsent draft. The setup extension's page is one of these ([setup sessions](../setup-sessions/)).

`runview.Detail` prints an attempt's workspace, so the CLI and TUI show a shell job's fresh worktree or stale shared checkout exactly as they would an agent's issue worktree. Shell jobs and agent turns share this record, the retention policy (`kept_executions`, default 20) and the deadline model (`settings.timeout_seconds`, overridable per binding). See [job run parity](../schedule-extension/#job-runs).

Issue reports are ordinary derived Events, so `events chain <report event>` traces a report from its external root through every report to the execution that made it. Each processing run is an ordinary `introspection.report` Work in `runs` and the TUI. Report records live in the extension's coordination KV namespace `introspection.reports`, not in run history ([issue reporting](../issue-reporting/)). A fix's trace is `events chain <fix event>`: fix → report → the report's origin. The fix Event's source names the requesting triage execution.

## Retention

One retention policy applies to every executor, with two rules:

- **By count.** `kept_executions` (B05, default 20 per Work, `SchedulerOptions.KeptExecutions`): the newest N attempts of each Work keep their content and older ones expire.
- **By age.** `kept_finished_days` (default 14, `SchedulerOptions.KeptFinished`): once a Work is finished (completed, failed or cancelled) and has not changed for that long, all its attempts expire, including the newest N. A Work still in progress never expires this way, however old. Once every attempt of a finished Work has expired, the sweep marks the Work (`core.retention.done`) and later sweeps skip it without reading its attempts or ledger entries. A finished Work younger than `kept_finished_days` gets no new attempts, so once its attempts beyond `kept_executions` have expired it is marked too (`core.retention.counted`, with the `kept_executions` it was settled under; a lower setting reads it again) and skipped until it is old enough to expire whole. The sweep reads only the Work that can have something to expire (#707): the live Work, the finished Work changed since its change cursor (`storage.LiveOrChanged`), which it expires by count, and the finished Work that has aged past `kept_finished_days` since its age cursor, read in order of last change (a page that starts at one Work and doubles, so a sweep with nothing aged reads one, and at most the sweep's limit). Each looks up its own markers. Neither cursor, kept in `core.retention.cursor`, passes finished Work the sweep's limit left unsettled. Finished Work an uncertain resource hold leaves goes on the held list (`core.retention.held`) instead, which every sweep reads by ID until the Work settles, so one stuck attempt costs a read a sweep rather than holding the cursors back; the change cursor stays `storage.ChangeLag` behind the sweep's own clock, which is the one that stamps the Work, and a lower `kept_executions` resets it, so finished Work is read again once. It reads only those Work's attempts, and the release phase reads only the entries still being released (`core.retention.pending`), so neither reads every attempt or expiration ever made. A ledger written before the pending list existed is indexed by the first sweep.

`Scheduler.SweepRetention(ctx, limit)` applies both. An attempt is expired only if it is terminal and its resource exclusion is confirmed; an uncertain hold is never released. The sweep runs in phases:

1. Commit an expiration ledger entry with the attempt's artifact references (every turn's) and the rule that expired it (`kept_executions` or `kept_finished_days`). This emits `execution.retention_expired` with `retention.reason`.
2. Outside the transaction, delete the content through `storage.Artifacts`, then call the optional `extension.ForgetExecutor.Forget` to release executor-private state.
3. Record the release.

A crash between phases is completed by a later sweep.

A retention hold defers phase 2's content deletes, and only those. It is a coordination record with an expiry (`runtime.HoldRetention`, `ReleaseRetention`; `core.retention.hold`), which a backup commits before its snapshot, renews while it copies and releases when done. The sweep reads the holds in the transaction that reads what to release, and while one is live it deletes no content (`RetentionResult.Held`): it still records expirations and calls `Forget`, and the content waits, still pending, for the first sweep with no live hold. Under serializable transactions that is enough. A release whose list was read before the hold committed acts only on expirations committed before it, and so before the snapshot, which records those attempts as expired and doesn't need their content. One read after it sees the hold. No artifact backend needs a lock for this. A hold its holder never released (the backup crashed) delays retention only until it lapses, and the sweep then removes it.

Coordination history (records, outcomes with their summaries, usage and artifact metadata, and the audit ledger) is never deleted, so a finished Work's runs still show what happened after their content is gone. The history shows `retention.state=expired`. Artifact content then returns 404 `artifact_expired`, and the TUI reports expiry instead of fetching the content.

The shell executor's `Forget` removes the execution's run directories. It refuses (`ErrLiveSegment`) and never signals while a verified process still runs. The host decides when to sweep: the daemon loop runs `SweepRetention` every `--retention-interval` (default 10 minutes), then collects artifact content that no attempt references and that is more than 24 hours old (`Scheduler.CollectArtifacts`). Collection pages the artifact store with `List` and asks coordination which IDs of each page are referenced (`Tx.ReferencedArtifacts`), one View per page, so each step's cost follows the page rather than the attempts retained, and it works over any `storage.ArtifactStore`. Content an attempt references, expired or not, is never collected; retention releases it. Executors attach what they publish in the same result, so the grace (`Options.ArtifactGrace`) far exceeds the publish-to-attach window. A collection error (for example an object with corrupt metadata) stops that pass and is logged as an `artifacts` runtime diagnostic. Both run while a backup copies, because the backup's hold defers the sweep's deletes and collection spares what the snapshot references.

## Doctor

`GET /v1/doctor[?check_secrets=true]` and `open-conduit doctor [--check-secrets] [--json]` return `ok`/`info`/`warn`/`fail` checks with safe codes and identifiers. The CLI exits 1 on `fail`. Doctor needs the `view` role; `check_secrets=true` resolves every secret reference the config makes (running each `cmd:` one and reading the keyring) and runs the checks that authenticate with them, so the host refuses it with `403 forbidden_role` below `admin`, before anything is resolved. The checks are:

| Check | Meaning |
| --- | --- |
| `storage` | A read/write coordination transaction commits. |
| `lease_recovery` | Running attempts holding expired leases that await recovery. |
| `resource_holds` | Finished attempts still holding capacity without a confirmed stop, each with the Work it blocks (`<execution> blocks work <work>`). |
| `retention` | Expired attempts with release pending. |
| `routing` | Events unrouted for over ten minutes, each holding the Work it targets, with its route failures (`unrouted_events`); `events skip` lets the Work move on ([route failures](../routing-lifecycle/#route-failures)). |
| `reconcile_lag` | Sources that are failing (safe code, and for `provider_failed` the provider's failure class, code and bounded message), have no complete scan, or are older than two intervals; when none is, each source's last complete scan (or that its first is pending). |
| `non_rederivable` | Declared delivery families with their loss statements. |
| `config` | The active generation against the config file: `pending_edit`, invalid, unreadable, `disk_missing`, or `default_config` (info) while the daemon runs on the default configuration with no file. |
| `bindings` | Each binding resolves to a registered executor that satisfies its required capabilities. |
| `connections` | Declared connections have registered types. Reachability is extension-specific. |
| `secrets` | Resolved only with `check_secrets`, reporting names and codes, never values or references. |
| `pinned_executors` | Live or held attempts whose pinned generation or registration is not installed. |
| `loop` | How long the host loop's latest 600 passes took (median, 95th percentile and longest), and the same for each phase (`route`, `poll`, `start`, `controls`, `subscriptions`, `ingress`, `packages`, `endpoints`, `retention`, `backup`), in the order a pass runs them ([host loop](../daemon-core-gate/#host-loop)). It reports, and never warns: how long a pass may take is the operator's call. |
| `providers` | Action providers and annotating extensions whose actions, panels or annotations call is timing out, so views skip it (`provider_timing_out`, `warn`): each with the call, since when, how many timeouts, the scope and Work it last timed out on, and when it is next asked. |

Only the `storage` probe writes, and so waits its turn with the daemon's writes. Every other runtime check reads through a View ([SQLite](../sqlite-artifacts/)), and reads only what is outstanding. `lease_recovery`, `resource_holds` and `pinned_executors` read the live attempts and the scheduler's index of attempts whose resource exclusion is unconfirmed; `retention` reads the ledger's pending list. An attempt that has finished and confirmed its exclusion is never read again, so doctor costs the same however long the history grows. On a store written before those indexes existed, the checks read every attempt until the daemon's first poll or retention sweep builds them.

Extension checks (`management.Options.Checks`), such as `telemetry.Pipeline.Health` for exporters or host watches, are appended. Provider error text never appears. The daemon contributes, among others, the `github` (registration, rate limit, reconcile and projection per entry), `github-capability`, `github-git-bot`, `webhooks` and `claude-code` checks, plus a `github-auth` deep check that runs only with `check_secrets=true`; management's `DeepChecks` option carries it. An `introspection` check lists each entry's schedules (next slot), host readings over threshold, telemetry and note namespaces.

Each bundled executor contributes a deep check of its bindings' model credentials, so a wrong, expired or mis-mapped key shows in doctor rather than as the first turn that fails. It runs only with `check_secrets=true`: plain doctor makes no outbound call for it and resolves nothing. For each binding of its executor, the check resolves the secrets the binding selects (`secret.resolve` with the binding, which the host allows only to the package whose executor the binding names, and only for the names it selects: the values its runs get), builds the credential and endpoint its runs would get from them, and makes one free request with them. Every binding is tried at once, each within 8 seconds, so an endpoint that never answers shows as `unreachable` without costing the others their results inside the host's 20-second limit for the check. A binding with a `provider` is tried against the provider's host, never the default. A container binding is tried with the secrets its container would be given, so the agent's CLI doesn't need to be on the daemon's machine.

| Check | Request |
| --- | --- |
| `claude-code-auth` | `GET /v1/models` with the bearer token (a `provider`'s, or a selected `ANTHROPIC_AUTH_TOKEN`) or the API key (`ANTHROPIC_API_KEY`) at the binding's base URL, else Anthropic's. An OAuth token (a selected `CLAUDE_CODE_OAUTH_TOKEN`, or for a binding on the host the Claude login on the daemon's machine) reads its subscription usage (`GET /api/oauth/usage`, as `/usage` does); a `403` there is a token the endpoint authenticated whose scope can't read usage (a `claude setup-token` token), which counts as working. Bedrock, Vertex and Foundry bindings (`CLAUDE_CODE_USE_*` in `env`) are `not_checked`: those credentials are the cloud's. |
| `codex-auth` | `GET <base_url>/models` with the `provider`'s `api_key_secret`, or with a selected `CODEX_API_KEY` at OpenAI's API (or the binding's `OPENAI_BASE_URL`). A host binding that signs in with `codex login` is `not_checked`: a ChatGPT sign-in has no free request. |
| `chat-completions-auth` | `GET <base_url>/models` with `api_key_secret` as the bearer token (none without one) and the binding's `headers`. |

Each binding is one item, `<scope>/<binding>: <code> (<host>, <credential kind>)`, and the check takes its worst binding's status and code. The codes are `ok`; `unauthorised` (the endpoint answered `401` or `403`), `not_configured` (no credential the binding's runs would sign in with) and `secret_unresolved` (`fail`); `unreachable`, `rate_limited` and `unexpected_status` with the HTTP status (`warn`); `not_checked` with the reason (`info`); and `not_configured` (`info`) when no binding uses the executor. Neither a value nor the provider's answer text is ever reported: the answer is classified by its status alone. An endpoint that lists its models without asking for a key (OpenRouter's does) reports `ok` whatever the key. A third-party executor contributes the same kind of check through its manifest's `checks` (`deep`) and `sdk.Host.BindingSecrets`.

When no daemon is reachable (it has never run, so there is no operator token, or nothing listens on its socket), `open-conduit health` says so and to run `open-conduit serve`. `open-conduit doctor` then runs its offline checks instead: `daemon` (`warn`, `not_running`), `config` validated offline (installed packages validating their own entries), `grants`, and the extensions' `Offline` checks (`github-capability`, `github-git-bot` for SSH remotes in scopes whose GitHub connection has `git` `bot`, and `claude-code`: `claude --version` for each claude-code binding). A `skipped` check names the checks that need the daemon. A missing token file while a daemon is listening is still reported as a token error.

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
- Issue 227 added the turn's declared state, start time, trigger type and time, and Work kind to terminal execution records, so an exporter can measure how long a Work spent in each state between turns.
