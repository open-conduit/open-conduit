---
title: "Shell executor and CLI toolbelt"
description: "How the shell executor runs, adopts, interrupts and stops detached processes, and how the os CLI reaches the toolbelt."
---

The shell executor, [`executors/shell`](https://github.com/open-conduit/open-conduit/tree/main/executors/shell), runs a binding's command as a detached process tree, and the [`os` CLI](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/toolbelt.go) gives that process the toolbelt. The daemon uses it for scheduled jobs and any other shell binding ([daemon composition](../daemon-core-gate/)). The executor imports only public contracts. It implements `extension.Executor`, `InterruptExecutor`, `InjectExecutor`, `ResumeExecutor` and the optional `StopExecutor` and `ForgetExecutor`, and advertises `toolbelt: cli`, `interrupt`, `inject` and `workspace`. `resume` is advertised only when `Options.Resume` is set.

## Binding config

Binding `config` is opaque to core and strictly checked by the executor. `Executor.ValidateConfig` is called through the optional `config.ConfigValidator` hook at load/apply time, and a rejection becomes an `invalid_config` diagnostic that never echoes values. The fields are:

| Field | Meaning |
| --- | --- |
| `command` | Run with `/bin/sh -c`. Exactly one of `command` or `argv` is required. |
| `argv` | Run directly. Exactly one of `command` or `argv` is required. |
| `dir` | Clean absolute working directory. Without it, and without a bound workspace, the command runs in a private per-segment scratch directory. `dir` and a bound workspace together are rejected. |
| `env` | Explicit non-secret variables. |
| `secret_env` | Maps a selected secret name to a variable. Unmapped secrets use their upper-cased name with other characters replaced by `_`. |
| `timeout_seconds` | Process-tree deadline. It is persisted and enforced across restart. |

Unknown fields, trailing data, NUL bytes, invalid names and any `OS_*` target are rejected. A configuration that bypassed validation, or a program that cannot start, produces a structured failed result with no process, so no uncertain resource hold remains.

## Workspaces

A shell binding whose Scope binds a resource provider runs in the acquired workspace, as described in [workspaces](../workspaces/). Acquisition happens after the recovery intent is persisted and before the process deadline starts. A setup failure, unavailable workspace or full-disk refusal becomes a structured failed result with no process (`workspace_setup_failed`, `workspace_unavailable`, `workspace_no_space`). A result reports the acquired workspace generically as `extension.Result.Workspace` (kind, lease reference, directory, the provider's one-line summary and description), beside the `workspace` field of the result data, so run history shows it for every executor.

## Environment and toolbelt protocol

The environment follows the B05 order:

1. A sanitized baseline: only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ`, `TMPDIR` and `TERM` survive. Unrelated credentials, `OPEN_CONDUIT_*` and inherited `OS_*` variables are dropped.
2. The workspace provider's layer, when a workspace is bound. Reserved and invalid names are dropped from it. With no workspace there is no provider layer.
3. Binding `env`.
4. Binding-selected secrets, resolved by the scheduler immediately before launch.
5. The trusted toolbelt and execution variables, which no earlier layer can override.

`OS_ENDPOINT`, `OS_TOKEN` and the `os` binary are the toolbelt protocol names and keep their `OS_` form despite the product rename. The executor prepends `Options.ToolbeltDir` (by default the directory of the running executable when it contains `os`) to `PATH`. `OS_TOKEN_FILE` points at a private 0600 file holding the same credential: the host-kept `ToolbeltTokenFile` in the execution's run directory when the host gives one ([execution endpoints](../toolbelt-management/#execution-endpoints)), otherwise a file the segment writes itself. Adoption replaces that file, so the `os` CLI reads it first and falls back to `OS_TOKEN`. Without a toolbelt token none of the three transport variables is set. Identity variables are always present:

- `OS_SCOPE_ID`, `OS_WORK_ID`, `OS_EXECUTION_ID`, `OS_ATTEMPT`
- `OS_TRIGGER_ID`, `OS_TRIGGER_TYPE`
- `OS_INPUT_FILE`: the trigger plus the acceptance-ordered input batch
- `OS_OUTPUT_DIR`
- `OS_INBOX` ([inject inbox](#inject-inbox))

`os` calls `POST /v1/toolbelt/call` through the public client. It supports these commands:

| Command | Operation |
| --- | --- |
| `os context` | `read_context` |
| `os emit` | `emit` |
| `os work create` | `create_work` |
| `os wait` | `wait` |
| `os outcome completed\|failed\|handoff` | `declare_outcome` |
| `os call OP` | registered extension operations |

Every command accepts `--request-id`. Results print as JSON. Exit status is 0 on success, 1 for a rejected or failed call (safe error code only) and 2 for usage or missing environment. Arguments and credentials are never echoed. A declared wait or outcome takes effect only when the host observes the process finish: `Scheduler.Observe` applies the declaration through `Complete`, as the [toolbelt](../toolbelt-management/#toolbelt-authority) requires.

## Process model, identity and adoption

Every segment is detached by construction. The executor persists its recovery metadata (`open-conduit.shell.recovery.v1`, naming the segment directory) through `PersistRecovery` **before** launch, so a crash can never leave an unacknowledged launch. It then starts a `/bin/sh` wrapper as a new session and process-group leader, with stdout and stderr appended to a private log file. The wrapper:

1. Records its PID.
2. Traps HUP/INT/TERM, so they reach the command with default disposition.
3. Runs the command.
4. Signals any remaining group members, which is process-tree cleanup on normal exit.
5. Atomically records the exit status.

The wrapper's `$0` carries an unguessable per-segment nonce. Process identity is checked beyond PID: a process is the segment only if it is a group leader (`pgid == pid`) whose argument vector contains that nonce. On Linux this reads `/proc`; elsewhere it uses `ps` with a fixed locale. Identity is re-verified immediately before every signal. Once the leader is gone, remaining group members are signaled only while the group is live and no unrelated process holds the leader PID. Groups emptied by PID reuse are therefore never touched; a residual race exists only across a full PID-space wrap. A process that calls `setsid` itself escapes the group and is not tracked.

`Recover` never launches:

- **Live verified process**: it writes the new epoch's lease marker, replaces its own token file with the newly issued credential (the host has already rotated a host-kept one), re-acknowledges the identical metadata under the new lease and returns a polling handle.
- **Already finished segment**: it returns the identical stored result.
- **Dead process with no recorded exit or stop**, or unknown, tampered or other-execution metadata: it returns `ErrNotAdoptable`.
- **Empty metadata**: it first cleans up any segment of that execution.

A lower-epoch recoverer is refused. A superseded instance can neither interrupt nor stop a process that a newer epoch adopted (`ErrSuperseded`); this is how lease ownership is respected at the process boundary.

The production scheduler requires `PersistRecovery` to succeed before it accepts an adopted handle, so re-acknowledging the identical metadata during recovery is allowed. The shared executor case permits that re-acknowledgement and still rejects new metadata as a replacement launch, with a negative control (`TestExecutorRecoveryReplacement`).

## Results, interruption and stop evidence

`Wait` cancellation stops observation only. Each finished segment is finalized once across instances, under a file lock:

1. Publish the combined log (`text/plain; charset=utf-8`) and up to 64 regular files from `OS_OUTPUT_DIR` as artifacts.
2. Durably record the result for later recovery.
3. Delete the token file.

Outcome data (`open-conduit.shell.result.v1`) carries `reason`, plus `exit_code`, `signal`, `timeout_seconds`, `log`, `outputs` (name → artifact ID) and `artifact_error` when applicable. Artifact store failure does not lose the outcome. The reasons map as follows:

| Reason | Outcome | Summary |
| --- | --- | --- |
| `exit`, status 0 | completed | `exit status 0` |
| `exit`, status N | failed | `exit status N` |
| `timeout` | failed | the persisted deadline elapsed; SIGTERM, then SIGKILL after the grace period |
| `interrupted` | failed | from `Interrupt` or `Stop` |
| `signal` | failed | the process was killed by a signal |
| `invalid_config`, `start_failed` | failed | no process ran |

The scheduler preserves executor outcome data when it converts a declared wait into the waiting outcome.

`Interrupt(execution)` acts only on a segment this instance holds. It verifies identity and epoch, records the stop reason and sends SIGTERM to the group; escalation happens during observation. Success means delivered, not stopped. `Stop(execution, epoch)` (the public optional `extension.StopExecutor`) is trusted host cleanup for a fenced attempt, for example after operator cancel, a scheduler deadline, `ErrNotAdoptable` or an observation error. It terminates any verified live segment and returns nil only when every segment's tree is verified gone. The host then calls `Scheduler.ConfirmStopped(execution, epoch)`. A toolbelt outcome declaration is never stop evidence. An observed structured result is, because the tree has been drained before `Wait` returns.

## Retention and logs

`Forget` implements the shared retention policy for the shell executor: it removes an expired execution's run directories and refuses while a verified process still runs. Run history and the TUI show logs from published artifacts ([observability and history](../observability-history/)). The shell executor does not implement `extension.LiveExecutor`, so a running shell segment's log is not streamed; it is readable once the segment finishes and the log is published.

## Inject inbox

The shell executor implements `InjectExecutor`, the second implementation of the [generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement).

- **The inbox.** Each segment has `inbox/` in its run directory, named to the process as `OS_INBOX`. `Inject` writes the Event as `<order>-<id digest>.json`, in the `OS_INPUT_FILE` event shape, under the segment's finalize lock. A repeated Event identity is not written again. After the result is recorded, or for an execution this instance does not hold, `Inject` returns `ErrNotAccepting`.
- **Taking input in.** The process reads the inbox and acknowledges each event by creating `inbox/consumed/<id digest>`. `os inbox` does both: it prints the unacknowledged events in order as JSON lines and acknowledges each one after printing it, without any call to the endpoint.
- **The proof.** The result reports acknowledged, delivered events as `Result.ConsumedInputs`. An acknowledgement for a file core never delivered is ignored.
- **Adoption.** The inbox is on disk, so an adopted process keeps receiving input.
- **Scripts that never read the inbox.** Nothing is consumed. Injected input stays pending and reaches attempt N+1, exactly as `queue` would.

Binding an inject route to shell therefore needs no unsupported-capability fallback.

## Evidence

- [Unit cases](https://github.com/open-conduit/open-conduit/blob/main/executors/shell/shell_test.go) cover:
  - the shared executor conformance case
  - log and output artifacts, the input file and the scratch directory
  - every environment layer and the stripping of credentials and reserved variables
  - config rejection and structured no-process failures
  - exit failure with background-child cleanup
  - timeout with a TERM-ignoring child forcing SIGKILL
  - interrupt and unowned interrupt
  - adoption by a second instance with a replaced token and no duplicate launch, lower-epoch refusal, and superseded interrupt/stop refusal
  - identity beyond PID: a reused PID is neither adopted nor signaled, and tampered metadata is refused
  - explicit resume capability
- [Integration cases](https://github.com/open-conduit/open-conduit/blob/main/integration/shell_test.go) run the real shipped `os` binary against the Unix-socket management server, production scheduler/toolbelt and SQLite, with the tests playing the host:
  - The acceptance workflow runs with N+1 wake and with resume. A no-workspace script reads context, emits, creates child Work and waits, then after an operator wake declares an outcome. The test asserts the causal chain, artifacts and a secret delivered only through the selected binding.
  - Exit failure applies retry policy; timeout establishes exclusion; operator cancel plus `Stop` and `ConfirmStopped` hold.
  - A host torn down mid-execution and restarted on the same state adopts the still-running process at a higher epoch. The old credential is rejected and the adopted process emits with its replaced credential, with exactly one launch. This restart is an in-process teardown and rebuild of every host component; the shell process is a real OS process in its own session. Killing an actual daemon process at each boundary is covered in [daemon composition](../daemon-core-gate/).
  - Apply-time config rejection is covered, and so are CLI exit codes and non-echo behavior.
- `TestSharedInjectConformance`, `TestInboxIgnoresForgedAcknowledgements` and `TestToolbeltInbox` cover the inject inbox.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 02-07 delivered the shell executor and the `os` CLI as the first real executor, proving the architecture with no agent session, no tracker and no workspace. At that stage the environment had no resource layer because there was no workspace, and the host role in its evidence was played by integration tests composing the production scheduler, toolbelt, management server, SQLite and artifacts. Daemon composition (a `serve` command, routing and poll loops) followed in 02-09.
- While building 02-07, the 01-03 shared executor case was found to treat any recovery-time `PersistRecovery` call as a replacement launch. It was changed to permit re-acknowledging identical metadata, and `TestExecutorRecoveryReplacement` was added as a negative control.
- Ticket 02-08 added `Forget` for the shared retention policy and history/TUI presentation with log tailing from published artifacts.
- Ticket 03-02 added workspace support. Ticket 04-02 added the inject inbox; before it, binding an inject route to shell needed an unsupported-capability fallback. Ticket 05-02 added `extension.Result.Workspace`.
- `extension.StopExecutor` became a public optional interface in 07-01.
