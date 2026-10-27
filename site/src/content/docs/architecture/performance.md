---
title: "Performance"
description: "What is known about Open Conduit's throughput, latency, contention, recovery and resource use, what has not been measured, and how each gap would be measured."
---

This page gathers the performance evidence for Open Conduit as it stands. Most of it is design bounds: defaults and limits set in code, and properties that follow from how the daemon is built. Little is measured: the extension transport, and how the daemon's hot paths cost as history grows. Each section keeps the two apart, says plainly where nothing has been measured, and sets out the benchmark or load test that would produce the number.

Open Conduit is a single local daemon ([overview](../../start/overview/)). Its management API is served only on an owner-only Unix socket ([local transport](../config-management/#local-transport-discovery-and-authority)), and the [extension protocol](../extension-protocol/) has no consumers outside this project. The load it is built for is one operator's repositories, Slack threads and schedules, running at most four agent turns and four jobs at once by default ([capacity pools](../execution-scheduling/#capacity-pools)). It is not a multi-tenant service, and several pages state outright that they make no throughput claim: [storage contracts](../domain-storage/), [event ingress](../event-ingress/#bounded-scheduling-and-inspection) and [routing limits](../routing-lifecycle/#limits).

## What is measured

- **Cost against retained history.** [`history_bench_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/history_bench_test.go) times the daemon's hot paths on real SQLite with 1,000, 10,000 and 50,000 retained Events: roughly days, months and a year or more of one operator's repositories and threads. Routing one Event and the idle routing pass are flat; `Poll` and the Work list grow with the number of Work and Executions, by a few milliseconds a year ([results](#history-growth)). Rerun it with:

  ```sh
  make bench-history
  ```

- **Extension transport.** The only timed figures in the repository come from the [transport prototype](https://github.com/open-conduit/open-conduit/tree/main/prototypes/transport), which ran one workload over JSON-RPC on stdio and over go-plugin/gRPC ([transport comparison](../extension-protocol/#transport-comparison)). For the chosen JSON-RPC transport: cold start 105 ms, warm restart 10 ms, version-mismatch refusal 6 ms, echo p50 32 µs and p99 61 µs, host callback 29 µs, a 16 MiB artifact in 64 KiB chunks at 143 MiB/s, cancellation seen by the extension in 173 µs, crash detected in 1.0 ms, and a 3.8 MiB binary. That is one representative run on darwin/arm64 with Go 1.27.3, without `-race`. Cold start varied between 100 and 350 ms across runs, and `-race` slowed everything by about 3×. It measures the transport alone, not the daemon. Rerun it with:

  ```sh
  make protocol-prototype
  ```

- **Upper bounds asserted by tests.** A few tests fail if something takes too long. They record pass or fail, not a duration:
  - [`TestPipelineNeverBlocksOnHungExporter`](https://github.com/open-conduit/open-conduit/blob/main/internal/telemetry/pipeline_test.go): 1,000 `Observe` calls behind a hung exporter, with a 4-record queue, finish in under 2 s.
  - [`TestExternalInTransactionTimeoutAndReentrancy`](https://github.com/open-conduit/open-conduit/blob/main/integration/extension_faults_test.go): a hung in-transaction extension call returns between 200 ms and 2 s (the deadline is 250 ms).
  - [`TestExternalIngressDurableAcceptanceAcrossCrash`](https://github.com/open-conduit/open-conduit/blob/main/integration/extension_faults_test.go): a push across an extension crash fails in under 5 s rather than hanging.
  - [`TestCommand`](https://github.com/open-conduit/open-conduit/blob/main/internal/config/secrets_test.go): a `cmd:` secret with a 200 ms timeout has its process group killed in under 3 s.
- **Volume in correctness tests, untimed.** [`TestSlackPushReconcileOverlapOneWork`](https://github.com/open-conduit/open-conduit/blob/main/integration/slack_test.go) pushes and reconciles 300 replies in one thread (150 under `-race`) and checks they become 301 distinct Events on one Work, each queued once. It shows correctness at that volume, not speed.
- **Informal observations.** Two code comments record timings that nobody measured systematically: the [Makefile](https://github.com/open-conduit/open-conduit/blob/main/Makefile) says the integration package takes about 8 minutes on hosted runners under `-race`, and [`slack_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/slack_test.go) says the race-instrumented SQLite driver is about 30× slower per transaction.

The history benchmarks are the only Go benchmark functions in the repository. There is no load test, and no CI job that records a performance number. CI runs `make check`, `make drill`, `make certify` and `make cross-build` ([`ci.yml`](https://github.com/open-conduit/open-conduit/blob/main/.github/workflows/ci.yml)), and all of them check correctness.

## Event-ingress throughput

### Design bounds

- **Reconcile pages.** Defaults are 1,000 observations or direct Events per page, 10,000 staged items, 1,000 pages, 8 MiB per page and 32 MiB of staged bytes, with a 30 s page timeout and a 1-minute scan lease ([`NewIngress`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/ingress.go); [bounded scheduling](../event-ingress/#bounded-scheduling-and-inspection)).
- **Reconcile pacing.** `ReconcileDue` makes at most one page call per source per poll, and the daemon passes a limit of 100 (`passLimit` in [`loop.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/loop.go)). Failed or partial scans back off from 5 s, doubling up to the configured interval ([`retryDelay`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/reconcile.go)). The default interval is `settings.reconcile_seconds`, 900 s ([configuration](../config-management/#version-1-schema-and-inheritance)).
- **Adapter pages.** A GitHub reconcile call stops after 25 detail reads or 10 s ([GitHub ingress](../github-extension/#ingress-and-canonical-identity)). A Slack call stops after 20 API calls or 10 s, and reads at most 50 pages of 200 replies per thread ([Slack reconcile](../slack-extension/#reconcile)).
- **Push bodies.** Management and schedule webhook bodies are capped at 1 MiB (`maxBody` in [`server.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/management/server.go); [schedule webhooks](../schedule-extension/)). An extension protocol message is capped at 16 MiB (`MaxMessageBytes` in [`protocol.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/protocol.go)).
- **Causal limits.** Derived Events are limited to depth 32 and 128 per root per minute ([causal limits](../routing-lifecycle/#causal-limits); `normalizeCausalLimits` in [`causal.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/causal.go)).

### What the design implies

- Each push (one delivery, or one batch) commits in its own `BEGIN IMMEDIATE` transaction, and a webhook is answered only after that commit, so push throughput is bounded by serial SQLite commits at `synchronous=FULL` (see [SQLite contention](#sqlite-contention)).
- Acceptance, including derived acceptance, deduplicates through the `UNIQUE(source, delivery_id)` index ([`records.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/records.go)). Routing, `MarkRouted` and the routing pass read only unrouted Events, through a partial index on the unrouted backlog (`events_unrouted` in [`store.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)). Steps that need the latest sequence read it from the primary key.
- Event rows are never deleted. Retention removes artifact content and executor-private state, and keeps coordination history ([retention](../observability-history/#retention)). Retained history no longer changes the cost of routing an Event. Routing one Event still reads the whole unrouted backlog, so draining a backlog grows with the backlog's square.

### Measurements

Accepting and routing one Event that opens a new Work is flat against retained history: about 0.7 ms at 1,000, 10,000 and 50,000 Events. Before the unrouted index it took 31 ms, 283 ms and 1.35 s ([history growth](#history-growth)). Push throughput under load is not measured.

### To measure

- A load test against a composed daemon: replay N signed GitHub and Slack webhook deliveries at a fixed rate, and record the acknowledged deliveries per second and the time from 2xx to `route.decided` in the lifecycle log. It would live beside the drills in `integration` behind its own build tag, run by a new `make` target outside `make check`.

## Scheduler latency

### Design bounds

- **Loop cadence.** One host loop runs every `--poll-interval`, default 1 s ([`serve.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/serve.go); [host loop](../daemon-core-gate/#host-loop)). Webhook acceptance, management requests, observed attempt ends and reloads nudge it to run at once (`poke` in [`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)). A nudge during a pass queues one more pass.
- **Per-pass limits.** `Scheduler.Poll` takes a limit of 1–200, and the daemon passes 100: at most 100 due timers, and at most 100 items in each of the retry/timeout Event, active Wait and expired-lease phases, rotated by a durable cursor ([`Poll`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/timers_scheduler.go)). Pending Events are routed in pages of 200 until none are left.
- **Capacity.** `settings.concurrency` (default 4) bounds agent turns and `settings.job_concurrency` (default 4) bounds jobs ([capacity pools](../execution-scheduling/#capacity-pools); defaults in `settings` in [`config.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/config/config.go)). A scope's `limits.concurrency` caps the attempts running in it and its descendants, across both pools. The config also accepts `settings.admission_limit` (default 10), but the daemon does not enforce it ([scope and limitations](../daemon-core-gate/#limitations)).
- **Leases.** Leases last `--lease-duration`, default 1 minute, and are renewed every third of that, about 20 s ([`loop.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/loop.go)). After a crash, running attempts are adopted only once their lease expires, so up to 1 minute later ([host loop](../daemon-core-gate/#host-loop)).
- **Retries.** Failures wait 300 s, then 600 s (`settings.retry_delays_seconds`). Usage-limit retries wait for the reset plus 2 minutes, or 30 minutes if it is unknown ([Claude Code executor](../claude-code-executor/)).

### What the design implies

- The pass is serial. Routing, `Poll`, starting ready Work, control dispatch, subscription reconcile, `ReconcileDue`, extension projections and resource reconcile all run one after another in one goroutine ([`loop`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/loop.go)). The time from a webhook to a started attempt is therefore the rest of the current pass, plus routing, plus `Start`. A slow provider page holds the pass for up to its page budget (10 s for GitHub and Slack, 30 s by default), once per due source.
- The routing pass and `Poll`'s Event phase read only unrouted Events. `Poll` still reads every Wait and every Execution, and `startReady` reads every Work, each inside a transaction. So an idle pass grows with the number of Work and attempts ever made, not with the Events retained ([history growth](#history-growth)).
- Executor calls run outside transactions ([execution scheduling](../execution-scheduling/)), so a slow `claude -p` launch delays the pass but holds no storage lock.

### Measurements

The parts of an idle pass that read history are measured against retained history ([history growth](#history-growth)): at 50,000 Events the routing pass takes 27 µs, `Poll` 7 ms and the `startReady` Work list 5 ms. No test records the time from acceptance to `execution.started`, or the duration of a whole pass.

### To measure

- Lifecycle records already carry their time (`At`) and IDs, so the latency can be read off `logs/lifecycle.jsonl`: `event.accepted` → `route.decided` → `execution.queued` → `execution.started` → `execution.launched`. A load test against a composed daemon with a shell binding that exits at once would emit Events through the management API at a fixed rate, at several history sizes, and report the p50, p95 and p99 of each step from the log.
- Add a per-pass duration record (or a histogram through the OpenTelemetry exporter) to the host loop, so an idle daemon's pass time can be watched as history grows. The same load test then records pass duration against Event count.

## SQLite contention

### Design bounds

- WAL mode and `synchronous=FULL` are required, not tuning options ([coordination backend](../sqlite-artifacts/#coordination-backend)).
- Each `Store` has one pooled connection (`SetMaxOpenConns(1)`) with a 5 s busy timeout (`busyTimeout` in [`store.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)). The daemon opens one `Store` on `conduit.db` ([`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)).
- Every `Transact`, including read-only management and TUI reads, starts with `BEGIN IMMEDIATE`. No callback is retried ([`Transact`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)).
- Records are stored as JSON blobs. Indexes cover primary keys, correlation keys, the delivery key, Event sequence, unrouted Events, Executions by Work and the one-active-attempt rule. The unrouted index is a partial index over the routed flag in the Event record, so it needs no schema version change: it is created when the store opens, and a binary without it keeps it current. Waits by Work and Work by Scope are not indexed (the schema in [`store.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)). KV prefix listing compares a substring over the namespace rather than using a range ([`timers_kv.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/timers_kv.go)).
- Four extension callbacks run inside a transaction and are bounded at 250 ms (`InTransactionDeadlineMillis` in [`protocol.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/protocol.go); [in-transaction calls](../extension-protocol/#transactions-and-in-transaction-calls)). Every other host call to an extension or executor runs outside transactions.
- The TUI refreshes every 2 s (`interval` in [`tui.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/tui/tui.go)). Management lists take 1–200 items per page, default 50 ([API version 1](../config-management/#api-version-1)).

### What the design implies

- All coordination work, reads included, is serialised through one connection in the daemon. There is no lock contention between SQLite connections in normal running; contention appears as goroutines queuing for the one connection. The 5 s busy timeout matters only when another process opens the same file, and the daemon's exclusive state lock prevents that ([names and paths](../config-management/#names-paths-and-precedence)).
- So "SQLite contention" here means how long one transaction holds the connection. That time grows with the Work, Execution and Wait lists described above, and with the cost of a `synchronous=FULL` commit on the host's disk.
- A hung extension can hold the connection for at most 250 ms per in-transaction call.

### Measurements

Not measured. [`TestConcurrentFirstOpen`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store_test.go) (4 concurrent opens) and [`TestProductionSchedulerCapacityAcrossInstances`](https://github.com/open-conduit/open-conduit/blob/main/integration/scheduler_test.go) prove correctness under competing handles, not timing.

### To measure

- Go benchmarks in `internal/storage/sqlite`: `BenchmarkTransactEmpty` (the floor: `BEGIN IMMEDIATE` and `COMMIT` at `synchronous=FULL`), `BenchmarkAcceptEvent`, and `BenchmarkListEvents` at 10,000 and 100,000 rows. Run with `b.RunParallel` to show queueing on the single connection. Record ns/op and the p99 wait for the connection.
- In the daemon, time from requesting the connection to `BEGIN IMMEDIATE` succeeding, and from there to `COMMIT`, exported as histograms. A load test would drive the TUI's 2 s refresh, a management list loop and webhook pushes together, and report how long reads wait behind writes.

## Large-queue recovery

### Design bounds

- After a crash, attempts are recovered once their 1-minute lease expires. After a graceful stop, leases are released and attempts are adopted at once ([host loop](../daemon-core-gate/#host-loop)).
- `Poll` recovers at most 100 expired attempts, fires at most 100 timers and handles at most 100 retry/timeout Events and Waits per pass, with a fair cursor. Pending Events are routed in full, in pages of 200, at the start of every pass ([`loop.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/loop.go)).
- Ready Work starts only as capacity allows: 4 agent turns and 4 jobs by default.
- Reconcile resumes from durable checkpoints, one page per source per poll, within the page limits above ([transactional snapshot generations](../event-ingress/#transactional-snapshot-generations)).
- A retention sweep runs every 10 minutes (`--retention-interval`) and expires at most 100 attempts per sweep, keeping 20 per Work (`settings.kept_executions`; [`retention.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/retention.go)).

### What the design implies

- A backlog of N due timers needs at least N/100 passes. Passes run back to back while nudges arrive, and otherwise one per poll interval, so at the default 1 s interval that is roughly N/100 seconds at best.
- A backlog of N unrouted Events is drained in the first pass, but each route reads the whole unrouted backlog, so the drain costs about N² record reads. Retained routed history adds nothing.
- No drill restarts the daemon with a large backlog. The [kill drills](../daemon-core-gate/#kill-drills) and [`TestDaemonMigrationRehearsal`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_migration_test.go) each use a handful of Work.

### Measurements

Not measured.

### To measure

- A drill in `integration` that seeds a state directory with a large backlog (for example 10,000 unrouted Events across 1,000 Work, 1,000 due timers, and 100 running attempts with expired leases), starts the daemon on it and records: time to the first `execution.started`, time until no Events are unrouted, time until every due timer has fired, and peak RSS. The `faultinject` build the kill drills use can produce the crashed state honestly instead of seeding it. It would be a new test beside the drills that `make drill` runs, or have its own `make` target if it is too slow for every CI run.

## Resource consumption under concurrent load

### Design bounds

- **Processes.** One daemon. One process per running attempt, up to `concurrency` + `job_concurrency` (8 by default); a Claude Code segment is one `claude -p` process ([Claude Code executor](../claude-code-executor/#a-segment)). One process per installed subprocess extension, restarted with backoff from 100 ms to 30 s ([extension protocol](../extension-protocol/)). Optionally one tunnel process ([operator setup](../operator-setup/)).
- **Goroutines.** One observer per running attempt, with a renewal ticker; one per telemetry exporter. The daemon composes five exporters ([`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)).
- **Memory bounds.** Each exporter queue holds 4,096 records and drops the oldest when full; batches are 128 records with a 10 s export timeout and 3 attempts ([export pipeline](../observability-history/#export-pipeline); [`pipeline.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/telemetry/pipeline.go)). The daemon flushes every 200 ms. An extension serves at most 64 requests at once and refuses beyond that with `overloaded`; messages are capped at 16 MiB and artifact chunks at 256 KiB ([flow control](../extension-protocol/#flow-control)). Staged reconcile observations are capped at 32 MiB per scan.
- **Output and logs.** Extension stderr logs are truncated at 1 MiB ([`process.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/process.go)). Live run tails default to 64 KiB, at most 4 MiB ([`history.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/management/history.go)). A file moved through the toolbelt is capped at 50 MiB ([`files.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/files.go)). `cmd:` secrets get 10 s and 64 KiB ([secrets](../config-management/#secrets-and-process-environment)). Config files and request bodies are capped at 1 MiB.
- **Disk.** Workspaces are refused when a checkout would leave the disk more than 90% full, keeping at most 10 GiB free; a spare-workspace sweep runs every 6 hours ([disk safety](../workspaces/#disk-safety-and-the-spare-workspace-sweep)). Artifacts are kept for the newest 20 attempts of each Work.
- **Background intervals.** Resource reconcile every 5 s, executor maintenance every 30 s, retention every 10 minutes ([`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)).

### What the design implies

- The large consumers are the executor processes, not the daemon: each agent turn is a full `claude` process with its own workspace checkout. Daemon memory has two parts: bounded buffers, and an unbounded part from decoding the Work, Execution and Wait lists in each pass, which grows with the Work ever made.
- `conduit.db` grows without bound. Event rows, Work, Executions and their metadata are never deleted, and the store runs no `VACUUM`. The WAL is checkpointed by SQLite's defaults. Pending artifact streams left by a killed daemon and unreferenced artifacts stay on disk until removed by hand ([recovery and retention](../sqlite-artifacts/#recovery-and-retention)).

### Measurements

Not measured.

### To measure

- A soak test against a composed daemon at full default capacity (4 agent turns using a fake `claude` that streams output for a set time, and 4 shell jobs), with webhook traffic, for several hours. Sample the daemon's RSS, goroutine count, open file descriptors and CPU every 10 s, and the sizes of `conduit.db`, its WAL and `artifacts/`. Report peak and slope. Drive it from a new `make` target outside `make check`. Its results would go in a table on this page, with the host, Go version and commit, the way the transport table is reported.
- A history-growth run of the whole daemon: idle at 1,000, 10,000 and 50,000 retained Events, recording RSS and the duration of a whole pass. The [history benchmarks](#history-growth) cover the storage reads, not the rest of the pass.

## History growth

Event rows are never deleted, so a daemon that runs for months carries every Event it has accepted. [`history_bench_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/history_bench_test.go) seeds a real SQLite store with N Events of about 1 KiB, in threads of 25 Events. Every Event is routed, and each thread's Work is finished by one attempt, so N/25 Work and Executions. Then it times:

- **Accept and route**: accept one Event for a new thread and route it, which opens its Work and queues the input.
- **Routing pass**: the host loop's `routePending` with nothing to route.
- **`Poll`**: one `Scheduler.Poll` pass with a limit of 100 and nothing due.
- **Work list**: the `ListWorks` read `startReady` makes each pass.

Before is the code as of #34, which read every Event in routing and in `Poll`. After adds the unrouted index and the lookups by delivery and latest sequence (#36). One run, darwin/arm64 (Apple M1 Pro), Go 1.27.3, without `-race`, 10 iterations before and 20 after:

| Pass | Events | Before | After |
| --- | ---: | ---: | ---: |
| Accept and route | 1,000 | 31 ms | 0.72 ms |
| | 10,000 | 283 ms | 0.72 ms |
| | 50,000 | 1.35 s | 0.70 ms |
| Routing pass | 1,000 | 6.1 ms | 23 µs |
| | 10,000 | 60 ms | 23 µs |
| | 50,000 | 303 ms | 27 µs |
| `Poll` | 1,000 | 6.1 ms | 0.23 ms |
| | 10,000 | 62 ms | 1.6 ms |
| | 50,000 | 299 ms | 7.2 ms |
| Work list | 1,000 | 0.11 ms | 0.10 ms |
| | 10,000 | 0.96 ms | 0.92 ms |
| | 50,000 | 4.8 ms | 4.7 ms |

Before, routing one Event at a year's history held the daemon's only connection for over a second, and an idle pass spent about 0.6 s reading Events, which also delayed management and TUI reads. Routing now allocates the same 81 KB at every size, where before it allocated 978 MB at 50,000 Events.

What still grows is `Poll`, which reads every Execution and Wait, and the Work list. At 2,000 Work and Executions (50,000 Events) they cost about 12 ms a pass together, so they were left as they are. If they ever matter, the fix is the same shape: an index on running Executions and active Waits, and a ready-Work index for `startReady`.

## Gaps

- No load or soak test exists. The timed figures are the extension transport prototype's and the history benchmarks.
- Event-ingress throughput, end-to-end scheduler latency, SQLite transaction hold and wait times, recovery time with a large backlog, and daemon RSS, CPU and disk growth under load are all unmeasured.
- `Poll` reads every Execution and Wait and `startReady` every Work inside `BEGIN IMMEDIATE`, so an idle pass grows with the Work ever made: about 12 ms at a year's history ([history growth](#history-growth)). Draining an unrouted backlog grows with the backlog's square.
- The host loop is serial, so one slow reconcile page delays starting Work. No test measures by how much.
- `admission_limit` is configurable but not enforced by the daemon.
- The SQLite file has no compaction or Event retention.
