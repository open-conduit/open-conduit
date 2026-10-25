# Daemon composition and the core gate

Ticket 02-09 composes the first long-running daemon, `open-conduit serve`, and closes the stage 02 gate with real process kill/restart drills. The composition root is [`internal/bootstrap`](../../internal/bootstrap). The [`serve` command](../../internal/cli/serve.go) resolves paths and hands them to it through `cli.Env.Serve`, so neither the CLI package nor the `os` binary links the runtime. The drills are in [`integration/daemon_test.go`](../../integration/daemon_test.go).

## Composition

`open-conduit [--data-dir DIR] [--socket PATH] serve [--config PATH] [--toolbelt-dir DIR] [--lifecycle-log PATH] [--poll-interval 1s] [--lease-duration 1m] [--retention-interval 10m]` follows B05 precedence: `--config` → `OPEN_CONDUIT_CONFIG` → `${XDG_CONFIG_HOME:-$HOME/.config}/open-conduit/config.json`; `--data-dir` → `OPEN_CONDUIT_DATA_DIR` → `${XDG_STATE_HOME:-$HOME/.local/state}/open-conduit`; `--socket` → `OPEN_CONDUIT_SOCKET` → `<state>/management.sock`. SIGINT/SIGTERM shut down gracefully.

`bootstrap.Serve` assembles the pieces in this order:

1. Takes the exclusive state lock before it touches the database or socket.
2. Opens SQLite (`conduit.db`).
3. Builds the telemetry pipeline. Its exporters are the JSON lifecycle log (`<state>/logs/lifecycle.jsonl`, 0600, appended across restarts) and `telemetry.OTel` over OpenTelemetry SDK tracer, meter and logger providers. The providers have no span processors or readers yet: OTLP and other exporter configuration remains extension work, as 02-08 decided.
4. Wraps SQLite in `runtime.Instrument`, so every component shares one instrumented coordination store.
5. Opens local artifacts and the shell executor, with run state under `<state>/executors/shell` and `os` taken from beside the binary unless `--toolbelt-dir` is given.
6. Loads the config service against the in-process registry: executor `shell` and route handler `manual`.
7. Bootstraps the operator credential and serves the management API on the owner-only socket.

The router, toolbelt, scheduler, timers, control and management server are rebuilt for each committed configuration generation, through `ConfigService.OnApply`. The HTTP handler always delegates to the current server. Settings map as follows:

| Setting | Maps to |
| --- | --- |
| `concurrency` | `GlobalLimit` |
| `timeout_seconds` | `Timeout` |
| `kept_executions` | `KeptExecutions`, with `Artifacts` for content deletion |
| `retry_delays_seconds` | `RetryPolicy.Delays` |
| causal depth and rate | router, scheduler, timers and control `CausalLimits` |

The loop loads the current generation immediately before each claim. An attempt claimed after `Apply` returns therefore pins the new generation. A claim racing the commit itself may still pin the previous one, exactly as if it had started just before the apply. Toolbelt grants still intersect with the current ceiling at once. Generation N installs the same executor instances as `HistoricalExecutors` for generations 1…N−1, so a pinned attempt is reinstalled exactly. One binary registers one implementation per name. Doctor gets the scheduler, resolver, ingress and a `telemetry` check from `Pipeline.Health`.

Each daemon start has a unique lease owner and instance ID, `open-conduit/<host>/<pid>/<random>`.

### The manual route handler

`manual` is the first-party route handler. It matches only operator-submitted Events: source `core.manual`, never `core.*` types.

- An Event addressed to existing Work, by Work ID or an owned key, becomes that Work's queued input.
- An Event with an unowned correlation key creates Work of kind `manual.work` that owns the key.
- Execution emissions, timer Events and core Events never match, so they cannot create Work through this handler.
- A waiting Work's wake is decided by its Wait, not by a route.

`open-conduit events emit` now accepts `--key-source/--key-external-id`, so an operator can submit keyed Events from the CLI.

### Host loop

One loop runs every `--poll-interval`. It also runs immediately after an observed attempt ends or a reload commits. Every pass is idempotent, because all progress is durable:

1. Route every accepted, unrouted Event in acceptance order (`Ingress.Pending` → `Router.Route`). This includes Events accepted by timer fires and execution emissions.
2. Run `Scheduler.Poll`, which covers due timers, retry and timeout Events, active Waits and expired leases. Recovered attempts are observed. An `executor_recovery_failed` diagnostic leads to a trusted stop (see below). Events from fired timers are routed in the same pass.
3. `Start` every ready Work. Ineligible Work (capacity, backoff, no input) reports a safe code once per subject, not on every pass.
4. Dispatch committed inject and interrupt intents for running Work, run `ReconcileSubscriptions` and `Ingress.ReconcileDue`, and run `SweepRetention` every `--retention-interval`.

Every attempt from `Start` or `Recover` is observed by a goroutine that renews its lease every third of the lease duration. On `ErrConflict` it routes pending input and re-observes, as 02-07 requires. A lost lease covers operator cancel, the scheduler deadline and supersession by a newer epoch. A lost lease or an observation failure leads to `executor.Stop(execution, epoch)` and then `ConfirmStopped`. A superseded epoch is refused by the shell executor and confirms nothing.

Shutdown is handled in two ways:

- **Graceful shutdown** stops observing without signalling the detached processes, and releases their live leases, so the next daemon recovers them at once.
- **After a crash**, the dead daemon's lease stays live until it expires (one minute by default). Only then does `Poll` recover the attempt under a higher epoch. The exclusive lock proves the old daemon is dead, but storage deliberately has no lease override, so a restart after a crash adopts work only once the lease expires.

## Contract clarifications

The drills found two runtime defects. Both are fixed, with regressions in [`integration/restart_test.go`](../../integration/restart_test.go):

- **Type-only waits (B03/B04).** A Wait is a subscription on its Work's keys. A toolbelt `wait` with types but no keys now subscribes on every key the Work owns. Work without keys is refused at declaration with `invalid`. Previously the declaration was accepted, but the handshake registration failed against storage, which requires keys.
- **Declarations survive recovery (02-06 handshake).** An unapplied toolbelt declaration belongs to the segment being adopted: `Recover` never relaunches, and wait registration clears the declaration before any resume. `Recover` now moves it to the new epoch. Previously, a crash between the declaration and the host's `Complete` lost it. A declared wait then completed the Work instead, and a declared outcome was replaced by the raw exit status.

## Kill drills

The test builds `open-conduit` with the `faultinject` build tag; production builds compile [`fault_none.go`](../../internal/bootstrap/fault_none.go) instead. In that build, `OPEN_CONDUIT_FAULT=<boundary>:<phase>[:<event type>]` wraps raw SQLite, beneath instrumentation, and SIGKILLs the daemon at the first matching write:

| Phase | When the daemon is killed | What it proves |
| --- | --- | --- |
| `before` | Inside the open transaction | SQLite rolls the transaction back |
| `after` | Right after the commit is acknowledged | The commit survives although the caller never acted on it |

The fixture is a scope with one shell binding and the `manual` route. Its script parks on `os wait` in the first segment. A later trigger declares `os outcome completed --summary "woken by <type>"`. Each drill has its own state directory and checks the following:

1. The daemon died by SIGKILL at the armed fault.
2. A restarted daemon on the same state reaches the final outcome. A client whose request died retries with the same idempotency key.
3. The root Event was accepted exactly once and routed to the single Work.
4. The first attempt was triggered by the root Event, and every attempt's trigger is linked to the Work.
5. No attempt is left live, and no Work was ever observed with two running or pending attempts.
6. No (execution, trigger) segment launched twice.
7. `/v1/runs` lists every attempt.
8. Exported lifecycle records keep one trace per execution across the restart.

| Drill | Boundary | Result (before / after) |
| --- | --- | --- |
| `accept` | Operator `drill.event` acceptance, mid-request | Pass / pass. Retry accepts once or returns the original. |
| `route` | Its `MarkRouted` during the request's best-effort routing | Pass / pass. The loop routes the pending Event after restart. |
| `claim` | First execution claim | Pass / pass. After: the unlaunched attempt fails on recovery (not adoptable, stop confirmed), and the retry attempt parks and completes. |
| `wait` | Wait registration at the host handshake | Pass / pass. Before: recovery adopts the finished segment and the carried declaration registers the wait. |
| `outcome` | Final attempt entering completed | Pass / pass. Before: recovery reapplies the stored result and declared outcome. |
| `timer` | `FireTimer` of the wait timeout | Pass / pass. The pending timer survives or its accepted Event routes. The wake's causal chain reaches the root. |
| Detached process, live lease | External SIGKILL while the shell runs | Pass. Adopted under epoch 2 no earlier than the dead lease's expiry, without relaunch. The old token is refused, and `launched mode=recover` continues the original trace. |
| Detached process, expired lease | Restart after expiry, process alive | Pass, with the same checks. |
| Graceful SIGTERM | Lease released | Pass. Adopted immediately, before the lease would have expired. |
| Config reload | Apply generation 2 while parked | Pass. Attempt 1 stays pinned to generation 1, the wake runs under 2, and a restart restores 2. |
| Operator CLI | `events emit` with key, `runs table`, `doctor` | Pass, using only the shipped CLI. |

When the test binary is race-enabled, as under `make check`, the daemon is also built with `-race`. Any data race report in a daemon log fails the drill. Telemetry is at most once from memory, so records queued when a daemon is killed can be lost. The durable history is asserted in full. Lifecycle completeness is required only for records produced after the last kill.

## Scope and limitations

- **Recurring timer rearm.** `FireTimer` is a single storage operation (fire plus delete or replace). The drills kill around the one-shot wait timeout. Atomic recurring rearm under SIGKILL is proven at storage and runtime level by 02-05. The core has no recurring producer until schedules (05-01).
- **Unenforced settings.** `settings.admission_limit` is validated but not enforced, and per-scope `ScopeLimits` are not configurable. `reconcile_seconds` applies only to ingress registrations, and none are loaded in stage 02.
- **Telemetry redaction.** The pipeline redacts sensitive attribute keys. Resolved secret values are not fed to `Options.Secrets`, because lifecycle records never carry executor environment or payloads.
- **Platforms.** Drills ran on macOS (darwin/arm64, host Go and Go 1.24.2, race-enabled) and in a Linux arm64 `golang:1.24.2` container (not race-enabled). Other targets are cross-built only. No hosted CI or Windows run is claimed.
