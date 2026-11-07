---
title: "Performance"
description: "What is known about Open Conduit's throughput, latency, contention, recovery and resource use, what has not been measured, and how each gap would be measured."
---

This page gathers the performance evidence for Open Conduit as it stands. Most of it is design bounds: defaults and limits set in code, and properties that follow from how the daemon is built. Little is measured: the extension transport, the calls the host makes to extensions inside its transaction, and how the daemon's hot paths cost as history grows. Each section keeps the two apart, says plainly where nothing has been measured, and sets out the benchmark or load test that would produce the number.

Open Conduit is a single local daemon ([overview](../../start/overview/)). Its management API is served only on an owner-only Unix socket ([local transport](../config-management/#local-transport-discovery-and-authority)), and the [extension protocol](../extension-protocol/) has no consumers outside this project. The load it is built for is one operator's repositories, Slack threads and schedules, running at most four agent turns and four jobs at once by default ([capacity pools](../execution-scheduling/#capacity-pools)). It is not a multi-tenant service, and several pages state outright that they make no throughput claim: [storage contracts](../domain-storage/), [event ingress](../event-ingress/#bounded-scheduling-and-inspection) and [routing limits](../routing-lifecycle/#limits).

## What is measured

- **Cost against retained history.** [`history_bench_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/history_bench_test.go) times the daemon's hot paths on real SQLite with 1,000, 10,000, 50,000 and 1,000,000 retained Events. One install took in about 20,000 Events in its first week, so 50,000 is a few weeks and a million a year or two. Routing, `Poll`, the live Work read and the capacity check are flat up to a million Events; the retention sweep and the full Work list grow with the Work ever made ([results](#history-growth)). Rerun it with (`OC_HISTORY_SIZES=1000,50000` picks sizes):

  ```sh
  make bench-history
  ```

- **The in-transaction extension calls, against a budget.** [`extension_budget_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/extension_budget_test.go) times `work_kind.trigger`, `recurrence.next`, `dependency.filter` and `emit.commit`, the four calls the host makes to an extension process while it holds its coordination transaction, round trip through `extproc.Remote` to the notes example ([results](#in-transaction-extension-calls)). It is a gate, not just a number: `make check` runs it on its own after the integration shards, prints each round's numbers, and fails when a call's median or 95th percentile is over budget in three rounds running. Print the numbers without `-race` with:

  ```sh
  make bench-extension
  ```

- **Extension transport.** The transport's own figures come from the [transport prototype](https://github.com/open-conduit/open-conduit/tree/main/prototypes/transport), which ran one workload over JSON-RPC on stdio and over go-plugin/gRPC ([transport comparison](../extension-protocol/#transport-comparison)). For the chosen JSON-RPC transport: cold start 105 ms, warm restart 10 ms, version-mismatch refusal 6 ms, echo p50 32 µs and p99 61 µs, host callback 29 µs, a 16 MiB artifact in 64 KiB chunks at 143 MiB/s, cancellation seen by the extension in 173 µs, crash detected in 1.0 ms, and a 3.8 MiB binary. That is one representative run on darwin/arm64 with Go 1.27.3, without `-race`. Cold start varied between 100 and 350 ms across runs, and `-race` slowed everything by about 3×. It measures the transport alone, not the daemon. Rerun it with:

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

The history benchmarks are the only Go benchmark functions in the repository. There is no load test. CI runs `make check` (split over parallel jobs), `make certify` and `make cross-build` ([`ci.yml`](https://github.com/open-conduit/open-conduit/blob/main/.github/workflows/ci.yml)). Apart from the in-transaction call budget, which `make check` enforces, all of them check correctness, and no CI job records a performance number.

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
- The routing pass and `Poll`'s Event phase read only unrouted Events. `Poll`'s Wait and lease phases read only active Waits and unfinished attempts, `startReady` and control dispatch read only unfinished Work, and the capacity check reads only unfinished attempts plus those whose resource exclusion is unconfirmed (a KV index the scheduler keeps, `core.scheduler.unexcluded`). So an idle pass follows the live set, not the Work and attempts ever made ([history growth](#history-growth)).
- Executor calls run outside transactions ([execution scheduling](../execution-scheduling/)), so a slow `claude -p` launch delays the pass but holds no storage lock.

### Measurements

The parts of an idle pass that read history are measured against retained history ([history growth](#history-growth)): at a million Events the routing pass takes 18 µs, `Poll` 80 µs and the live Work read `startReady` and control dispatch each make 57 µs, about the same as at a thousand. No test records the time from acceptance to `execution.started`, or the duration of a whole pass.

### To measure

- Lifecycle records already carry their time (`At`) and IDs, so the latency can be read off `logs/lifecycle.jsonl`: `event.accepted` → `route.decided` → `execution.queued` → `execution.started` → `execution.launched`. A load test against a composed daemon with a shell binding that exits at once would emit Events through the management API at a fixed rate, at several history sizes, and report the p50, p95 and p99 of each step from the log.
- Add a per-pass duration record (or a histogram through the OpenTelemetry exporter) to the host loop, so an idle daemon's pass time can be watched as history grows. The same load test then records pass duration against Event count.

## SQLite contention

### Design bounds

- WAL mode and `synchronous=FULL` are required, not tuning options ([coordination backend](../sqlite-artifacts/#coordination-backend)).
- Each `Store` has one pooled connection for `Transact` (`SetMaxOpenConns(1)`) with a 5 s busy timeout (`busyTimeout` in [`store.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)). The daemon opens one `Store` on `conduit.db` ([`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)).
- Every `Transact` starts with `BEGIN IMMEDIATE`. No callback is retried ([`Transact`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)). Management reads, and so the TUI's and the web view's, go through `View` instead: a deferred read transaction on one of up to four `query_only` reader connections (`readerConns`), which in WAL mode reads a committed snapshot without taking the writer's connection.
- Records are stored as JSON blobs. Indexes cover primary keys, correlation keys, the delivery key, Event sequence, unrouted Events, unfinished Work, unfinished Executions, active Waits, Executions by Work and the one-active-attempt rule. The unrouted, unfinished-Work and active-Wait indexes are partial indexes over a state in the JSON record, so they need no schema version change: they are created when the store opens, and a binary without them keeps them current. Waits by Work and Work by Scope are not indexed (the schema in [`store.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store.go)). KV prefix listing compares a substring over the namespace rather than using a range ([`timers_kv.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/timers_kv.go)).
- Four extension callbacks run inside a transaction and are bounded at 250 ms (`InTransactionDeadlineMillis` in [`protocol.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/protocol.go); [in-transaction calls](../extension-protocol/#transactions-and-in-transaction-calls)). Every other host call to an extension or executor runs outside transactions.
- The TUI refreshes every 2 s (`interval` in [`tui.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/tui/tui.go)). Management lists take 1–200 items per page, default 50 ([API version 1](../config-management/#api-version-1)).

### What the design implies

- All coordination writes, and the runtime's own reads, are serialised through one connection in the daemon. There is no lock contention between SQLite connections in normal running; contention appears as goroutines queuing for the one connection. Operator reads don't queue there: up to four run at once on the reader connections, alongside whatever transaction holds the writer. The 5 s busy timeout matters only when another process opens the same file, and the daemon's exclusive state lock prevents that ([names and paths](../config-management/#names-paths-and-precedence)).
- So "SQLite contention" here means how long one transaction holds the connection. That time grows with the Work, Execution and Wait lists described above, and with the cost of a `synchronous=FULL` commit on the host's disk.
- A hung extension can hold the connection for at most 250 ms per in-transaction call.

### Measurements

Not measured. [`TestConcurrentFirstOpen`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/store_test.go) (4 concurrent opens) and [`TestProductionSchedulerCapacityAcrossInstances`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/scheduler_test.go) prove correctness under competing handles, not timing.

### To measure

- Go benchmarks in `internal/storage/sqlite`: `BenchmarkTransactEmpty` (the floor: `BEGIN IMMEDIATE` and `COMMIT` at `synchronous=FULL`), `BenchmarkAcceptEvent`, and `BenchmarkListEvents` at 10,000 and 100,000 rows. Run with `b.RunParallel` to show queueing on the single connection. Record ns/op and the p99 wait for the connection.
- In the daemon, time from requesting the connection to `BEGIN IMMEDIATE` succeeding, and from there to `COMMIT`, exported as histograms. A load test would drive the TUI's 2 s refresh, a management list loop and webhook pushes together, and report how long reads wait behind writes.

## Large-queue recovery

### Design bounds

- After a crash, attempts are recovered once their 1-minute lease expires. After a graceful stop, leases are released and attempts are adopted at once ([host loop](../daemon-core-gate/#host-loop)).
- `Poll` recovers at most 100 expired attempts, fires at most 100 timers and handles at most 100 retry/timeout Events and Waits per pass, with a fair cursor. Pending Events are routed in full, in pages of 200, at the start of every pass ([`loop.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/loop.go)).
- Ready Work starts only as capacity allows: 4 agent turns and 4 jobs by default.
- Reconcile resumes from durable checkpoints, one page per source per poll, within the page limits above ([transactional snapshot generations](../event-ingress/#transactional-snapshot-generations)).
- A retention sweep runs every 10 minutes (`--retention-interval`) and expires at most 100 attempts per sweep. It keeps 20 per Work (`settings.kept_executions`), and none for Work finished more than 14 days ago (`settings.kept_finished_days`; [`retention.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/retention.go)). Without the age rule almost nothing expired: on one install after under a week, 203 Work had 787 runs, none had more than 20, and run directories and artifacts held about 400 MB.

### What the design implies

- A backlog of N due timers needs at least N/100 passes. Passes run back to back while nudges arrive, and otherwise one per poll interval, so at the default 1 s interval that is roughly N/100 seconds at best.
- A backlog of N unrouted Events is drained in the first pass, but each route reads the whole unrouted backlog, so the drain costs about N² record reads. Retained routed history adds nothing.
- No drill restarts the daemon with a large backlog. The [kill drills](../daemon-core-gate/#kill-drills) each use a handful of Work.

### Measurements

Not measured.

### To measure

- A drill in `integration` that seeds a state directory with a large backlog (for example 10,000 unrouted Events across 1,000 Work, 1,000 due timers, and 100 running attempts with expired leases), starts the daemon on it and records: time to the first `execution.started`, time until no Events are unrouted, time until every due timer has fired, and peak RSS. The `faultinject` build the kill drills use can produce the crashed state honestly instead of seeding it. It would be a new test beside the drills that `make drill` runs, or have its own `make` target if it is too slow for every CI run.

## Resource consumption under concurrent load

### Design bounds

- **Processes.** One daemon. One process per running attempt, up to `concurrency` + `job_concurrency` (8 by default); a Claude Code segment is one `claude -p` process ([Claude Code executor](../claude-code-executor/#a-segment)). One process per running extension package, bundled or installed, restarted with backoff from 100 ms to 30 s ([extension protocol](../extension-protocol/)). Optionally one tunnel process ([operator setup](../operator-setup/)).
- **Goroutines.** One observer per running attempt, with a renewal ticker; one per telemetry exporter. The daemon composes the JSON log and OpenTelemetry exporters, the packages' exporters and, when usage reporting is on, the usage reporter ([`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)).
- **Memory bounds.** Each exporter queue holds 4,096 records and drops the oldest when full; batches are 128 records with a 10 s export timeout and 3 attempts ([export pipeline](../observability-history/#export-pipeline); [`pipeline.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/telemetry/pipeline.go)). The daemon flushes every 200 ms. An extension serves at most 64 requests at once and refuses beyond that with `overloaded`; messages are capped at 16 MiB and artifact chunks at 256 KiB ([flow control](../extension-protocol/#flow-control)). Staged reconcile observations are capped at 32 MiB per scan. The daemon's per-subject diagnostic memory forgets an Event's, Work's or execution's code an hour after it was last reported.
- **Output and logs.** Extension stderr logs are truncated at 1 MiB ([`process.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/process.go)). Live run tails default to 64 KiB, at most 4 MiB ([`history.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/management/history.go)). A file moved through the toolbelt is capped at 50 MiB ([`files.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/files.go)). `cmd:` secrets get 10 s and 64 KiB ([secrets](../config-management/#secrets-and-process-environment)). Config files and request bodies are capped at 1 MiB.
- **Disk.** Workspaces are refused when a checkout would leave the disk more than 90% full, keeping at most 10 GiB free; a spare-workspace sweep runs every 6 hours ([disk safety](../workspaces/#disk-safety-and-the-spare-workspace-sweep)). Artifacts are kept for the newest 20 attempts of each Work.
- **Background intervals.** Resource reconcile every 5 s, executor maintenance every 30 s, retention every 10 minutes ([`daemon.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/daemon.go)).

### What the design implies

- The large consumers are the executor processes, not the daemon: each agent turn is a full `claude` process with its own workspace checkout. Daemon memory has two parts: bounded buffers, and an unbounded part from decoding the Work, Execution and Wait lists in each pass, which grows with the Work ever made.
- `conduit.db` grows without bound. Event rows, Work, Executions and their metadata, and each Event's acceptance and route records in KV, are never deleted, and the store runs no `VACUUM`. Revoked and expired toolbelt tokens and finished executions' replay records are deleted a day later ([toolbelt](../toolbelt-management/)). On one install after under a week, Events held 36 MB of 115 MB (29,600 rows), KV 21 MB (93,000 rows, of which acceptance and route records were 57,000), and toolbelt tokens and replays about 2 MB. Each open runs `quick_check` rather than the full `integrity_check`, which also matches every index against its table, so startup grows more slowly with the database ([SQLite](../sqlite-artifacts/)). The WAL is checkpointed by SQLite's defaults. Pending artifact streams left by a killed daemon and unreferenced artifacts stay on disk until removed by hand ([recovery and retention](../sqlite-artifacts/#recovery-and-retention)).

### Measurements

Not measured.

### To measure

- A soak test against a composed daemon at full default capacity (4 agent turns using a fake `claude` that streams output for a set time, and 4 shell jobs), with webhook traffic, for several hours. Sample the daemon's RSS, goroutine count, open file descriptors and CPU every 10 s, and the sizes of `conduit.db`, its WAL and `artifacts/`. Report peak and slope. Drive it from a new `make` target outside `make check`. Its results would go in a table on this page, with the host, Go version and commit, the way the transport table is reported.
- A history-growth run of the whole daemon: idle at 1,000, 50,000 and 1,000,000 retained Events, recording RSS and the duration of a whole pass. The [history benchmarks](#history-growth) cover the storage reads, not the rest of the pass.

## History growth

Event rows are never deleted, so a daemon that runs for months carries every Event it has accepted. [`history_bench_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/history_bench_test.go) seeds a real SQLite store with N Events of about 1 KiB, in threads of 25 Events. Every Event is routed to its thread's Work, and each thread's Work is finished by one attempt, so N/25 Work and Executions. Then it times:

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

### Live reads at a million Events (#353)

Every seeded Work is finished, so the live set is the few Work the accept-and-route benchmark opens. Three more passes are timed:

- **Live Work**: the `LiveWorks` read `startReady` and control dispatch each make per pass, which replaced the full Work list.
- **Capacity**: `Scheduler.Capacity`, the read of the attempts holding a slot that `Start` makes for every claim.
- **Retention**: one `SweepRetention` with nothing to expire, every Work finished within `kept_finished_days`.

Before is the code as of #388, which read every Work, Execution and Wait in these paths. After adds the live reads, the unexcluded-attempts index and the retention markers. One run, darwin/arm64 (Apple M1 Pro), Go 1.27.3, without `-race`, 20 iterations:

| Pass | Events | Before | After |
| --- | ---: | ---: | ---: |
| Accept and route | 1,000 | 0.84 ms | 1.0 ms |
| | 50,000 | 0.77 ms | 0.82 ms |
| | 1,000,000 | 0.76 ms | 0.82 ms |
| Routing pass | 1,000,000 | | 18 µs |
| `Poll` | 1,000 | 0.20 ms | 88 µs |
| | 50,000 | 7.3 ms | 82 µs |
| | 1,000,000 | 171 ms | 80 µs |
| Work list (`startReady`, twice a pass) | 1,000 | 0.15 ms | 67 µs |
| | 50,000 | 4.7 ms | 59 µs |
| | 1,000,000 | 109 ms | 57 µs |
| Capacity (every claim) | 1,000 | 0.34 ms | 27 µs |
| | 50,000 | 17 ms | 27 µs |
| | 1,000,000 | 354 ms | 27 µs |
| Retention (every 10 minutes) | 1,000 | 0.45 ms | 0.19 ms |
| | 50,000 | 22 ms | 7.9 ms |
| | 1,000,000 | 502 ms | 190 ms |

Before, an idle pass at a million Events spent about 0.4 s of every second in `Poll` and the two Work lists, all holding the only connection, and each claim added a third of a second. After, the idle pass reads only what is live and costs the same at every size.

What still grows is the retention sweep, which reads every Work record (109 ms at a million Events, 40,000 Work) and its markers, once every 10 minutes. The full Work list is still what management and the projections read.

## Operator views (#523)

The web view's pages are management reads. Measured against a copy of one install's `conduit.db` (66,000 Events, most of them GitHub deliveries for no Work; 410 Work; 1,628 runs; 687 Waits), served by a daemon with that config's scopes but no extensions, so provider calls (actions and panels) cost nothing. Each page loaded 30 times in a row through `internal/web`'s handler, darwin/arm64 (Apple M1 Pro), on a machine busy with other agents, before and after the changes:

| Page | Before p50 | Before p90 | After p50 | After p90 |
| --- | ---: | ---: | ---: | ---: |
| Runs (`/`) | 1.47 s | 1.69 s | 77 ms | 112 ms |
| Needs you | 1.54 s | 1.76 s | 41 ms | 82 ms |
| Runs, closed group | 1.53 s | 1.62 s | 67 ms | 81 ms |
| A Work | 5.6 s | 6.0–7.9 s | 62–66 ms | 82–99 ms |
| A run's output | 45–55 ms | 72–78 ms | 43–47 ms | 68–72 ms |
| Config | 307 ms | 381 ms | 310 ms | 363 ms |

On a quiet machine the same pages took about 20 ms each after. What it took:

- **A Work's events** (`GET /v1/events?work_id=`) scanned the whole log: 3 s. `WorkEvents` reads them from two expression indexes, `events_work` and `events_routed_work`: 0.2 ms at 1,000 and at 50,000 Events in the history benchmark.
- **A scope's events** (an extension's `state.events`) read the whole log and worked out each Event's scope through Work, correlation and acceptance lookups: 3.7 s on a 73,392-Event store, 3.2 s of it the lookups. Each Event now records its scope at acceptance and `ScopeEvents` reads it from the `events_scope` expression index; `TestStateEventsFollowTheExtensionsScopes` bounds the Events the host reads, with a log-scanning store as the negative control.
- **Run lists** built every run's full record for each page of 200: 112 ms of storage work a page, nine pages to read them all. A page now builds only the runs it returns.
- **The world read** behind the Runs and Needs you pages read every run on every load (6.6 MB of JSON, and only the oldest 1,000). It then read runs by Work, keeping a finished Work's in memory while its revision stood, so the first load after a restart read every Work's runs again, one request per Work, and could pass the view's 15 s call timeout ([#544](https://github.com/open-conduit/open-conduit/issues/544)). Now the world is the open Work alone, listed with its runs (`GET /v1/work?with=actions,panels,runs`, 50 a request), and a page asks for the closed Work it shows by its own query. The Runs page's facets come from `GET /v1/work/facets`, cached for 30 seconds, and without a facet set they also give the Closed count. With a facet set, the count is the list's `total`. The Closed rows are one page of 50 with their runs, and "Show more" reads 50 more. So the default view reads no closed Work, warm or cold. The daemon answers each request through storage's Work queries (`QueryWorks`, `CountWorks`, `WorkFacets`, [#548](https://github.com/open-conduit/open-conduit/issues/548)): SQLite selects, orders and pages Work from expression indexes over the records (a page of Work in several states reads a page of each state and merges them), counts from the indexes without reading a record, and steps through the distinct scopes, sources and executors one index entry each. Each attempt records its executor, so a Work's latest executor is read from an index rather than the attempt's state. `TestRunsPageReadsFollowTheirAnswer` bounds the Work records these requests read, with a store that reads every Work as the negative control.

`BenchmarkRunsPageReads` in `integration` times each request through the management API on SQLite. Each closed Work has four finished attempts, and 20 Work are open. One run, darwin/arm64 (Apple M1 Pro), 20 iterations, on a machine with a load average of about 90, so these are upper bounds. Before is the scan in process (#547), after is the index-backed queries:

| Request | 420 closed, before | 420 closed, after | 5,000 closed, before | 5,000 closed, after |
| --- | ---: | ---: | ---: | ---: |
| Open Work with runs (the world) | 7.3 ms | 5.3 ms | 49 ms | 2.7 ms |
| Facets (at most every 30 s) | 58 ms | 1.2 ms | 193 ms | 1.3 ms |
| Closed count with a facet | 4.9 ms | 5.6 ms | 54 ms | 7.5 ms |
| Closed page of 50 with runs | 45 ms | 37 ms | 110 ms | 27 ms |
| Closed search (`q`) | 79 ms | 117 ms | 668 ms | 564 ms |

The page's cost is now the 50 Work's runs, at either size. The search still grows with history: words are matched in process on every Work the other filters select, with each one's latest attempt for its outcome summary, because storage has no text index.
- **Reads queued on the writer.** Every read, the token check included, was a `BEGIN IMMEDIATE` transaction on the one connection, so a page's concurrent reads queued behind each other and the runtime. They now run as Views on reader connections.

- **Doctor** (#530), which the Config page runs on every load, read every attempt's state twice (for `resource_holds` and `pinned_executors`) and every retention record, all on the writer. It now reads the live attempts, the scheduler's index of attempts whose exclusion is unconfirmed and the retention ledger's pending list, through a View; only the storage probe still writes. Timed directly (`Diagnose` then `PinnedExecutorCheck`, 30 times in a row) on a fresh copy of the same install (1,684 runs, 67,680 Events), on a machine with a load average of 20–50: p50 435–504 ms and p90 0.77–5.5 s before, p50 5.2–5.4 ms and p90 26–409 ms after. The test `TestDoctorReadsOnlyOutstandingAttempts` seeds 300 finished, excluded attempts and fails if doctor reads their state.

Still slow or unmeasured:

- **Artifact content** finds an artifact's execution by reading every execution: about 18 ms per call.
- **Provider calls.** The list pages ask every open Work's action providers for actions and panels, within a 2 s provider timeout and a 4 s deadline. On the install measured, a third-party package's conversation panel read the whole event log on every call and hit the timeout, so the live pages took 4–5 s whatever the core costs. Management now remembers a call that timed out across requests and skips it until a probe off the request finds it answering ([#537](https://github.com/open-conduit/open-conduit/issues/537)), so only the first page after it starts timing out pays the timeout, and doctor names the provider.
- **Work annotations.** `GET /work/{id}` asked each annotating package for its annotations one after another, each bounded at 5 s, and remembered nothing, so on a loaded machine every read took 4–6 s, and the web view's world build made one such read for every ready or waiting Work. The annotators are now asked together under the view deadline, with provider health as above ([#545](https://github.com/open-conduit/open-conduit/issues/545)), and the world build reads with `annotations=runtime`, which asks no extension.

## In-transaction extension calls

Once every extension runs as a package ([#428](https://github.com/open-conduit/open-conduit/issues/428)), four of the host's calls into an extension happen while it holds its coordination transaction: `work_kind.trigger` on each routing pass with pending input for a kind with a trigger, `recurrence.next` on each timer firing, `dependency.filter` on each Wait match against a filtered dependency, and `emit.commit` on each committed emission (an operation's Event and the records written with it). The in-transaction deadline is 250 ms ([extension protocol](../extension-protocol/)), and the connection is the daemon's only one, so these calls are the hot path the transport adds.

The budget, per call, round trip from the host's adapter to the extension process and back, after 20 warm-up calls:

| | Median | 95th percentile |
| --- | ---: | ---: |
| Budget | 2 ms | 10 ms |
| Budget under `-race` (how `make check` runs) | 10 ms | 50 ms |

A round is 300 calls. A call over budget in three rounds running fails the test; one that answers wrongly (the trigger's fallback after an error, say) fails at once. The 99th percentile and the slowest call are logged, not gated: on a shared runner they belong to the scheduler and the garbage collector. `TestInTransactionRPCBudgetCatchesASlowCall` is the negative control. Each call's payload is realistic: the trigger is given four inputs of about 1 KiB of text, and each commit an annotation of about 240 bytes. A commit ID is used once, so each `emit.commit` call plans a fresh emission first (`operation.emit`, outside the transaction and the timing). `scripts/test-integration.sh` runs these tests alone once the four integration shards finish, so the daemon tests running beside a shard don't take the measurement.

One run, darwin/arm64 (Apple M1 Pro), Go 1.27.3, 300 calls, on a quiet machine. A reviewer's run on a busy machine under `-race` put the trigger's 95th percentile at 29 ms, more than half its 50 ms budget, which is why the tests run alone in CI:

| Call | Median | 95th percentile | Median, `-race` | 95th percentile, `-race` |
| --- | ---: | ---: | ---: | ---: |
| `work_kind.trigger` (4 inputs of 1 KiB) | 0.11 ms | 0.71 ms | 1.1 ms | 1.5 ms |
| `recurrence.next` | 0.10 ms | 1.4 ms | 0.43 ms | 0.55 ms |
| `dependency.filter` | 0.04 ms | 3.4 ms | 0.26 ms | 0.37 ms |
| `emit.commit` (one record) | 0.07 ms | 0.10 ms | 0.50 ms | 0.83 ms |

The trigger costs most because it carries the whole pending batch each way. Its first draft carried 1 KiB of NUL bytes per input, which JSON escapes to 6 KiB: that put its median at 0.8 ms and its 99th percentile at 28 ms. So a trigger's cost follows the size of its inputs' payloads, and a kind whose inputs are large pays for it on every pass.

## Gaps

- No load or soak test exists. The timed figures are the extension transport prototype's, the history benchmarks and the in-transaction call budget.
- Event-ingress throughput, end-to-end scheduler latency, SQLite transaction hold and wait times, recovery time with a large backlog, and daemon RSS, CPU and disk growth under load are all unmeasured.
- The retention sweep reads every Work record, about 190 ms every 10 minutes at a million Events ([history growth](#history-growth)). Draining an unrouted backlog grows with the backlog's square.
- The host loop is serial, so one slow reconcile page delays starting Work. No test measures by how much.
- `admission_limit` is configurable but not enforced by the daemon.
- The SQLite file has no compaction or Event retention: Events and their acceptance and route records are kept for ever, and they are most of the file.
