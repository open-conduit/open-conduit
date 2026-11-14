---
title: "Daemon composition and the core gate"
description: "How open-conduit serve composes the runtime into one daemon, drives it with a host loop, and the kill drills that prove it survives crashes."
---

`open-conduit serve` is the long-running daemon. It composes storage, telemetry, executors, configuration, routing, scheduling, timers, the toolbelt and the management API into one process and drives them with a single host loop. Real process kill/restart drills prove that it survives a crash at each durable boundary. The composition root is [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/tree/main/internal/bootstrap). The [`serve` command](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/serve.go) resolves paths and hands them to it through `cli.Env.Serve`, so neither the CLI package nor the `os` binary links the runtime. The drills are in [`integration/daemon_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_test.go).

## Composition

`open-conduit [--data-dir DIR] [--socket PATH] serve [--config PATH] [--toolbelt-dir DIR] [--lifecycle-log PATH] [--poll-interval 1s] [--lease-duration 1m] [--retention-interval 10m] [--sweep-interval 6h] [--call-timeout 5s] [--backup-interval 24h] [--backups-kept 7] [--backup-dir DIR]` (plus the webhook, tunnel, dry-run and usage flags listed by `open-conduit --help`) follows B05 precedence: `--config` → `OPEN_CONDUIT_CONFIG` → `${XDG_CONFIG_HOME:-$HOME/.config}/open-conduit/config.json`; `--data-dir` → `OPEN_CONDUIT_DATA_DIR` → `${XDG_STATE_HOME:-$HOME/.local/state}/open-conduit`; `--socket` → `OPEN_CONDUIT_SOCKET` → `<state>/management.sock`. SIGINT/SIGTERM shut down gracefully.

`bootstrap.Serve` assembles the pieces in this order:

1. Takes the exclusive state lock before it touches the database or socket. It then points `<state>/toolbelt/os` at the install's `os` (taken from beside the binary, links resolved, unless `--toolbelt-dir` is given), and executors get `<state>/toolbelt` as their toolbelt directory: on PATH, as the MCP server's command and behind git's `!os git-credential`. A package manager keeps each version in its own directory and removes the old one on upgrade (Homebrew's `Caskroom/open-conduit/<version>`); the daemon stops when the install it runs from goes, and the restarted one re-points the link, so a turn still running across the upgrade finds the new `os`. Sandboxed executors ask to read that directory, which is otherwise hidden with the rest of the state directory. If the link can't be made, executors get the install's own directory and the daemon logs a warning.
2. Opens SQLite (`conduit.db`).
3. Builds the telemetry pipeline. Its exporters are the JSON lifecycle log (`<state>/logs/lifecycle.jsonl`, 0600, appended across restarts and rotated by size: at 10 MB it moves to `lifecycle.jsonl.1`, and the newest five rotated files are kept, as for `introspection-telemetry.jsonl` and an extension's own log (`inproc.Host.OpenLog`, which a bundled package such as claude-code gets through `sdk.RotatingLog` in its state directory); [`protocol/sdk/logfile.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/logfile.go)), `telemetry.OTel` over OpenTelemetry SDK tracer, meter and logger providers, the packages' exporters, bundled (the introspection telemetry exporter, PostHog) and installed, and the usage reporter. The OpenTelemetry providers have no span processors or readers: OTLP and other exporter configuration is extension work ([observability](../observability-history/)).
4. Wraps SQLite in `runtime.Instrument`, so every component shares one instrumented coordination store.
5. Opens local artifacts, removing unfinished writes a killed process left (`Recover`), and writes the playbook plugin. It then opens tools and the extension packages, bundled and installed, so their validators check the configuration; each package's executors (shell, with `os` through the `<state>/toolbelt` link from step 1, Claude Code, Codex and the GitHub hold executor), resource, sandbox, container and tunnel providers register as it starts.
6. Loads the config service against the registry of those executors, the host's own route handler (`manual`) and the packages' declared types, handlers and operations, then registers each package's ingress sources.
7. Bootstraps the operator credential and serves the management API on the owner-only socket, and starts the webhook listener when configured.

The router, toolbelt, scheduler, timers, control and management server are rebuilt for each committed configuration generation, through `ConfigService.OnApply`. The HTTP handler always delegates to the current server. Settings map as follows:

| Setting | Maps to |
| --- | --- |
| `concurrency` | `GlobalLimit`: attempts of every executor without a pool, which are the agent turns (`claude-code`) and installed extension executors |
| `job_concurrency` | `PoolLimits["jobs"]`, with `ExecutorPools` placing each executor that declares the `jobs` pool there (`shell`, `github-hold`, and installed executors with `executors[].pool: "jobs"`) |
| `timeout_seconds` | `Timeout` |
| `kept_executions` | `KeptExecutions`, with `Artifacts` for content deletion |
| `kept_finished_days` | `KeptFinished`, in days |
| `retry_delays_seconds` | `RetryPolicy.Delays` |
| causal depth and rate | router, scheduler, timers and control `CausalLimits` |

The loop loads the current generation immediately before each claim. An attempt claimed after `Apply` returns therefore pins the new generation. A claim racing the commit itself may still pin the previous one, exactly as if it had started just before the apply. Toolbelt grants still intersect with the current ceiling at once. Generation N installs the same executor instances as `HistoricalExecutors` for generations 1…N−1, so a pinned attempt is reinstalled exactly. One binary registers one implementation per name. Doctor gets the scheduler, resolver, ingress and a `telemetry` check from `Pipeline.Health`.

Each daemon start has a unique lease owner and instance ID, `open-conduit/<host>/<pid>/<random>`.

### The manual route handler

`manual` is the first-party route handler for operator input. It matches only operator-submitted Events: source `core.manual`, never `core.*` types.

- An Event addressed to existing Work, by Work ID or an owned key, becomes that Work's queued input.
- An Event with an unowned correlation key creates Work of kind `manual.work` that owns the key.
- Execution emissions, timer Events and core Events never match, so they cannot create Work through this handler.
- A waiting Work's wake is decided by its Wait, not by a route.

`open-conduit events emit` accepts `--key-source/--key-external-id`, so an operator can submit keyed Events from the CLI.

### Host loop

One loop runs every `--poll-interval`. It also runs immediately after an observed attempt ends or a reload commits. Every pass is idempotent, because all progress is durable:

1. Route every accepted, unrouted Event in acceptance order (`Ingress.Pending` → `Router.Route`). This includes Events accepted by timer fires and execution emissions.
2. Run `Scheduler.PollClaims`, which covers due timers, retry and timeout Events, active Waits and expired leases. Each attempt it recovers is claimed, not launched: the pass hands it to its holder (below), which adopts it or relaunches it. Events from fired timers are routed in the same pass.
3. `Claim` every ready Work and hand each claimed attempt to its holder. Ineligible Work (capacity, backoff, no input) reports a safe code once per subject, not on every pass. The daemon remembers the last code per subject to do that; a code for one Event, Work, execution or intent that has gone unreported for an hour is forgotten on the next retention pass, so the memory follows live problems.
4. Dispatch committed inject and interrupt intents for running Work, run `ReconcileSubscriptions` and `Ingress.ReconcileDue`, run the extension passes (schedules, GitHub, tracker and Slack projection, webhooks, resources and others), and run `SweepRetention` (by count and by age) every `--retention-interval`.

The pass never launches anything itself. Each claimed attempt gets a holder, a goroutine of its own that holds the lease from the claim until the attempt ends (`Scheduler.Hold`). It renews the lease every third of the lease duration, launches the attempt (`Scheduler.Launch`: toolbelt, secrets, environment, workspace, sandbox or container, then the executor) and then observes it. A launch that takes many lease durations, such as a long clone, an image pull or a loaded host, keeps its lease. A slow launch also holds up nothing else: routing, timers, recovery, controls and other Work's starts carry on, and concurrent launches are bounded by the capacity the claims already count. Losing the lease ends the hold's context with `runtime.ErrLeaseLost`, so in-flight launch work stops at once. The holder then logs `lease lost while launching; leaving the attempt for recovery`, and the next `Poll` recovers it under a higher epoch. A launch that had pinned nothing is relaunched, and one that had is adopted ([recovery](../execution-scheduling/#recovery-and-relaunch)). A failed launch is reported against its Work (`start`) or execution (`poll`). An `executor_failed`, `invalid_state` or `executor_recovery_failed` code leads to a trusted stop (see below). Inject and interrupt intents for an attempt still launching wait for the launch to finish.

Once launched, the holder observes the attempt. On `ErrConflict` it routes pending input and re-observes. A lost lease covers operator cancel, the scheduler deadline, supersession by a newer epoch and a lease that lapsed while the host slept or stalled. When the attempt is still running and its Work open (`Scheduler.Adoptable`), the goroutine only stops observing and logs it: a lapsed lease is recovered and the same process adopted by the next `Poll`, and a newer epoch already holds it. Otherwise a lost lease or an observation failure leads to `executor.Stop(execution, epoch)` and then `ConfirmStopped`. A superseded epoch is refused by the shell executor and confirms nothing. A wall clock stepped back never loses the lease: `Renew` keeps the later expiry.

Shutdown is handled in two ways:

- **Graceful shutdown** stops observing without signalling the detached processes, and releases their live leases, so the next daemon recovers them at once.
- **After a crash**, the dead daemon's lease stays live until it expires (one minute by default). Only then does `Poll` recover the attempt under a higher epoch. The exclusive lock proves the old daemon is dead, but storage deliberately has no lease override, so a restart after a crash adopts work only once the lease expires.

## Waits and declarations across recovery

Two rules keep toolbelt waits and declarations correct across a crash. Regressions are in [`integration/restart_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/restart_test.go):

- **Type-only waits (B03/B04).** A Wait is a subscription on its Work's keys. A toolbelt `wait` with types but no keys subscribes on every key the Work owns. Work that owns no key is given its own, `core.work`/`<its ID>`, in the declaring transaction, so it subscribes on that.
- **Declarations survive recovery.** An unapplied toolbelt declaration belongs to the segment being adopted: `Recover` relaunches only a launch that started nothing, which can have declared nothing, and wait registration clears the declaration before any resume. `Recover` moves it to the new epoch, so a crash between the declaration and the host's `Complete` does not lose it.

## Kill drills

The test builds `open-conduit` with the `faultinject` build tag; production builds compile [`fault_none.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/fault_none.go) instead. In that build, `OPEN_CONDUIT_FAULT=<boundary>:<phase>[:<event type>]` wraps raw SQLite, beneath instrumentation, and SIGKILLs the daemon at the first matching write:

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
| `claim` | First execution claim | Pass / pass. After: recovery relaunches the unlaunched attempt under epoch 2, since nothing ran, and it parks and completes without using a retry. |
| `wait` | Wait registration at the host handshake | Pass / pass. Before: recovery adopts the finished segment and the carried declaration registers the wait. |
| `outcome` | Final attempt entering completed | Pass / pass. Before: recovery reapplies the stored result and declared outcome. |
| `timer` | `FireTimer` of the wait timeout | Pass / pass. The pending timer survives or its accepted Event routes. The wake's causal chain reaches the root. |
| Detached process, live lease | External SIGKILL while the shell runs | Pass. Adopted under epoch 2 no earlier than the dead lease's expiry, without relaunch. The old token is refused, and `launched mode=recover` continues the original trace. |
| Detached process, expired lease | Restart after expiry, process alive | Pass, with the same checks. |
| Graceful SIGTERM | Lease released | Pass. Adopted immediately, before the lease would have expired. |
| Config reload | Apply generation 2 while parked | Pass. Attempt 1 stays pinned to generation 1, the wake runs under 2, and a restart restores 2. |
| Operator CLI | `events emit` with key, `runs table`, `doctor` | Pass, using only the shipped CLI. |

A `kv` boundary (an extension KV `Put`, optionally in one namespace) is also available. [`TestDaemonClaudeCodeContinuityKill`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_claudecode_test.go) arms it at the Claude Code executor's pre-launch continuity decision, before and after the commit. [`TestDaemonClaudeCodeExecution`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_claudecode_test.go) kills the daemon externally while a claude turn runs. See [the Claude Code executor](../claude-code-executor/).

### Loop barrier

The `faultinject` build also gives the drills a barrier instead of sleeps. With `OPEN_CONDUIT_PASS_FILE` set, each completed pass of the host loop (routing, starting ready Work, dispatching controls, projecting to trackers and Slack, reconciling resources) atomically rewrites that file with the number of passes the process has completed. Production builds compile the hook as a no-op. A drill that must show something did not happen, such as nothing posted again after a restart, calls `drill.settle`: it waits until three full passes that began after the call have completed, then checks. That is faster than a fixed sleep and stronger, since the check runs only once the loop has really looked at the state. It does not cover an executor's process, which runs outside the loop; a drill waits on that process's own effect instead. A drill that needs a recovered attempt waits for its `execution.launched` record with `launch.mode` `recover` rather than sleeping out the lease.

When the test binary is race-enabled, as under `make check`, the daemon is also built with `-race`. Any data race report in a daemon log fails the drill. Telemetry is at most once from memory, so records queued when a daemon is killed can be lost. The durable history is asserted in full. Lifecycle completeness is required only for records produced after the last kill.

## Limitations

- **Recurring timer rearm.** `FireTimer` is a single storage operation (fire plus delete or replace). The drills above kill around the one-shot wait timeout. Atomic recurring rearm under SIGKILL is proven at storage and runtime level ([timers](../timers-waits/)), and the [all-workflows drill](#all-four-workflows-on-one-daemon) kills the daemon right after an interval job's slot fire and rearm commit.
- **Unenforced setting.** `settings.admission_limit` and a scope's `limits.admission_limit` are validated but not enforced, and `config check` warns where a config sets them. A scope's `limits.concurrency` is enforced: the daemon passes each scope that narrows its parent's limit to `SchedulerOptions.ScopeLimits` ([capacity pools](../execution-scheduling/#capacity-pools)). `reconcile_seconds` applies only to ingress registrations.
- **Telemetry redaction.** The pipeline redacts sensitive attribute keys. Resolved secret values are not fed to `Options.Secrets`, because lifecycle records never carry executor environment or payloads.

## All four workflows on one daemon

This drill runs the composed daemon with every workload at once. [`TestDaemonAllWorkflows`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_all_workflows_test.go) (in `make drill`) starts one `serve` process on three scopes, with fakes only: the GitHub and Slack fakes, a fake Claude Code CLI, and a shell executor calling the real `os` toolbelt.

| Scope | Workflow | Executors |
| --- | --- | --- |
| `eng` | A: the GitHub route claims an issue; the agent reports a defect and parks on the issue. D: a report-about-a-report loop on the report binding; the extension's triage timer; fix Work that parks on a timeout and then resolves the reports | shell |
| `support` | B: a workspace-free Slack conversation that replies and parks on the thread | claude-code (fake) |
| `ops` | C: an interval job that reads its context and parks on a timeout, plus a manual job | shell |

The drill checks the following, in order:

1. **Crash at a boundary during mixed startup.** The daemon is SIGKILLed right after the interval job's slot fire (and rearm) commits, while the GitHub and Slack registrations come up. After a restart, the job runs exactly once.
2. **Concurrent progress.** A parks, B replies and parks, C's timeout wakes its next attempt, and the core depth limit (`causal_max_depth` 5) stops the report loop without the extension's cooperation.
3. **External SIGKILL mid-wait.** A and B keep their Work and stay waiting across the restart.
4. **Webhooks off.** The human comment and the thread reply are never pushed. An operator scope reconcile, standing in for the next poll, finds both. A's next attempt finishes the issue (one marked comment, converged labels), and B resumes the same session.
5. **Workflow D.** Editing triage to an hour ago catches up one slot. Triage requests one fix for the open reports, and `events chain <fix>` traces fix → report → … → the GitHub origin.
6. **One wake path.** Each parked Work has exactly one woken Wait: A and B by reconciled ingress Events, C and D by their timeout timers. Each also has a `wait.woken` lifecycle record naming the waking type. The executor's capability alone decides the effect: the resumable claude-code attempt continues through `core.wait.woken`, while the shell attempts end and the wake Event triggers the next attempt. Every wake goes through `WakeWaitInTx` (routing, subscriptions, timers and the operator wake all call it).
7. **One history, view and retention model.** `/v1/runs` across all scopes holds shell and claude-code runs of `github.issue`, `slack.conversation`, `schedule.job` and the three introspection kinds. Every run renders in `runview` and carries its trigger, executor and binding. `kept_executions` 1 (swept every second) expires the earlier attempts of the A, C and D Work alike.
8. **Explainability.** Every attempt after the last kill has a start record with its trigger type and executor and a stop record with its outcome. The triage's fix request appears as a `toolbelt.called` record.
9. **Namespace isolation.** Every routed Event reaches Work in its own scope, and every Work's keys lie in its scope's namespaces.
10. **Redaction.** No secret value appears in any file under the drill root (SQLite, logs, artifacts, executor state, fixture output) or in the management API's runs, events and Work.
11. **Doctor.** The `github`, `schedule` and `introspection` checks pass on the shared daemon.

An operator scope reconcile that commits while a generation is under way (a page in flight, or between pages) leaves the source due when that generation completes, so the request is answered by a generation that starts after it. [`TestScopeReconcileDuringScanRunsAgain`](https://github.com/open-conduit/open-conduit/blob/main/integration/operator_controls_test.go) covers both cases. See [operator controls](../operator-setup/) and [event ingress](../event-ingress/#bounded-scheduling-and-inspection).

**Storage negative controls on SQLite.** [`TestWeakenedSQLiteIsRejected`](https://github.com/open-conduit/open-conduit/blob/main/internal/storage/sqlite/negative_test.go) wraps the real backend three ways: lost atomicity (a failed callback still commits), lost deduplication (the delivery identity includes the Event ID) and lost fencing (the lease guard is dropped). Each wrapper is rejected by the shared cases it weakens, and the same cases pass on plain SQLite. The reference-backend controls in `conformance` still run.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 02-09 composed the first long-running daemon and closed the stage 02 gate with the kill drills. At that stage the in-process registry held only executor `shell` and route handler `manual`, the telemetry exporters were the JSON lifecycle log and `telemetry.OTel`, and no ingress registrations were loaded, so `reconcile_seconds` had no effect. The core had no recurring timer producer until schedules arrived in 05-01. `events emit` gained `--key-source/--key-external-id` in this ticket.
- The 02-09 drills found two runtime defects, both fixed then:
  - **Type-only waits.** A toolbelt `wait` with types but no keys was accepted at declaration, but the handshake registration then failed against storage, which requires keys.
  - **Declarations lost on recovery.** A crash between a toolbelt declaration and the host's `Complete` lost the declaration (from the 02-06 handshake). A declared wait then completed the Work instead, and a declared outcome was replaced by the raw exit status.
- The 02-09 drills ran on macOS (darwin/arm64, host Go and Go 1.24.2, race-enabled) and in a Linux arm64 `golang:1.24.2` container (not race-enabled). Other targets were cross-built only. No hosted CI or Windows run was claimed.
- Ticket 03-03 added the `kv` fault boundary, which `TestDaemonClaudeCodeContinuityKill` arms.
- Ticket 05-04 added the all-workflows drill as the stage 05 gate. It found a regression: an operator scope reconcile that committed while a generation was under way was absorbed by that generation, which could predate the request, yet its completion scheduled the next scan a full interval away. The ingress state now counts requests, and `TestScopeReconcileDuringScanRunsAgain` was added.
