# Shell executor and CLI toolbelt

Ticket 02-07 delivers the first real executor, [`executors/shell`](../../executors/shell), and the [`os` CLI](../../internal/cli/toolbelt.go). It proves the architecture with no agent session, no tracker and no workspace. The executor imports only public contracts. It implements `extension.Executor`, `InterruptExecutor` and `ResumeExecutor`, and advertises `toolbelt: cli` and `interrupt`. `resume` is advertised only when `Options.Resume` is set. Workspace was added in 03-02 and inject in 04-02 (see [Inject inbox](#inject-inbox-04-02)). Daemon composition (a `serve` command, routing and poll loops) is delivered by 02-09 ([daemon composition](daemon-core-gate.md)); the host role in this document's evidence is played by integration tests that compose the production scheduler, toolbelt, management server, SQLite and artifacts.

## Binding config

Binding `config` is opaque to core and strictly checked by the executor. `Executor.ValidateConfig` is called through the optional `config.ConfigValidator` hook at load/apply time, and a rejection becomes an `invalid_config` diagnostic that never echoes values. The fields are:

| Field | Meaning |
| --- | --- |
| `command` | Run with `/bin/sh -c`. Exactly one of `command` or `argv` is required. |
| `argv` | Run directly. Exactly one of `command` or `argv` is required. |
| `dir` | Clean absolute working directory. Without it the command runs in a private per-segment scratch directory. |
| `env` | Explicit non-secret variables. |
| `secret_env` | Maps a selected secret name to a variable. Unmapped secrets use their upper-cased name with other characters replaced by `_`. |
| `timeout_seconds` | Process-tree deadline. It is persisted and enforced across restart. |

Unknown fields, trailing data, NUL bytes, invalid names and any `OS_*` target are rejected. A configuration that bypassed validation, or a program that cannot start, produces a structured failed result with no process, so no uncertain resource hold remains.

## Environment and toolbelt protocol

The environment follows the B05 order:

1. A sanitized baseline: only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ`, `TMPDIR` and `TERM` survive. Unrelated credentials, `OPEN_CONDUIT_*`, `OPEN_SUPERVISOR_*` and inherited `OS_*` variables are dropped.
2. No resource layer exists, because there is no workspace.
3. Binding `env`.
4. Binding-selected secrets, resolved by the scheduler immediately before launch.
5. The trusted toolbelt and execution variables, which no earlier layer can override.

`OS_ENDPOINT`, `OS_TOKEN` and the `os` binary are the PRD's toolbelt protocol names and stay unchanged despite the product rename. The executor prepends `Options.ToolbeltDir` (by default the directory of the running executable when it contains `os`) to `PATH`. `OS_TOKEN_FILE` points at a private 0600 file holding the same credential. Adoption replaces that file, so the `os` CLI reads it first and falls back to `OS_TOKEN`. Without a toolbelt token none of the three transport variables is set. Identity variables are always present:

- `OS_SCOPE_ID`, `OS_WORK_ID`, `OS_EXECUTION_ID`, `OS_ATTEMPT`
- `OS_TRIGGER_ID`, `OS_TRIGGER_TYPE`
- `OS_INPUT_FILE`: the trigger plus the acceptance-ordered input batch
- `OS_OUTPUT_DIR`

Any future legacy `OPEN_SUPERVISOR_*` compatibility belongs in an explicit conversion/adapter (06-02), never in this executor or core.

`os` calls `POST /v1/toolbelt/call` through the public client. It supports these commands:

| Command | Operation |
| --- | --- |
| `os context` | `read_context` |
| `os emit` | `emit` |
| `os work create` | `create_work` |
| `os wait` | `wait` |
| `os outcome completed\|failed\|handoff` | `declare_outcome` |
| `os call OP` | registered extension operations |

Every command accepts `--request-id`. Results print as JSON. Exit status is 0 on success, 1 for a rejected or failed call (safe error code only) and 2 for usage or missing environment. Arguments and credentials are never echoed. A declared wait or outcome takes effect only when the host observes the process finish: `Scheduler.Observe` applies the declaration through `Complete`, as [02-06](toolbelt-management.md) requires.

## Process model, identity and adoption

Every segment is detached by construction. The executor persists its recovery metadata (`open-conduit.shell.recovery.v1`, naming the segment directory) through `PersistRecovery` **before** launch, so a crash can never leave an unacknowledged launch. It then starts a `/bin/sh` wrapper as a new session and process-group leader, with stdout and stderr appended to a private log file. The wrapper:

1. Records its PID.
2. Traps HUP/INT/TERM, so they reach the command with default disposition.
3. Runs the command.
4. Signals any remaining group members, which is process-tree cleanup on normal exit.
5. Atomically records the exit status.

The wrapper's `$0` carries an unguessable per-segment nonce. Process identity is checked beyond PID: a process is the segment only if it is a group leader (`pgid == pid`) whose argument vector contains that nonce. On Linux this reads `/proc`; elsewhere it uses `ps` with a fixed locale. Identity is re-verified immediately before every signal. Once the leader is gone, remaining group members are signaled only while the group is live and no unrelated process holds the leader PID. Groups emptied by PID reuse are therefore never touched; a residual race exists only across a full PID-space wrap. A process that calls `setsid` itself escapes the group and is not tracked.

`Recover` never launches:

- **Live verified process**: it writes the new epoch's lease marker, replaces the token file with the newly issued credential, re-acknowledges the identical metadata under the new lease and returns a polling handle.
- **Already finished segment**: it returns the identical stored result.
- **Dead process with no recorded exit or stop**, or unknown, tampered or other-execution metadata: it returns `ErrNotAdoptable`.
- **Empty metadata**: it first cleans up any segment of that execution.

A lower-epoch recoverer is refused. A superseded instance can neither interrupt nor stop a process that a newer epoch adopted (`ErrSuperseded`); this is how lease ownership is respected at the process boundary.

Contract clarification recorded here: the production scheduler requires `PersistRecovery` to succeed before it accepts an adopted handle, but the 01-03 shared executor case treated any recovery-time call as a replacement launch. The shared case now permits re-acknowledging the identical metadata and still rejects new metadata, with a negative control (`TestExecutorRecoveryReplacement`).

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

The scheduler now preserves executor outcome data when it converts a declared wait into the waiting outcome.

`Interrupt(execution)` acts only on a segment this instance holds. It verifies identity and epoch, records the stop reason and sends SIGTERM to the group; escalation happens during observation. Success means delivered, not stopped. `Stop(execution, epoch)` is trusted host cleanup for a fenced attempt, for example after operator cancel, a scheduler deadline, `ErrNotAdoptable` or an observation error. It terminates any verified live segment and returns nil only when every segment's tree is verified gone. The host then calls `Scheduler.ConfirmStopped(execution, epoch)`. A toolbelt outcome declaration is still never stop evidence. An observed structured result is, because the tree has been drained before `Wait` returns.

## Evidence

- [Unit cases](../../executors/shell/shell_test.go) cover:
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
- [Integration cases](../../integration/shell_test.go) run the real shipped `os` binary against the Unix-socket management server, production scheduler/toolbelt and SQLite:
  - The 02-07 acceptance workflow runs with N+1 wake and with resume. A no-workspace script reads context, emits, creates child Work and waits, then after an operator wake declares an outcome. The test asserts the causal chain, artifacts and a secret delivered only through the selected binding.
  - Exit failure applies retry policy; timeout establishes exclusion; operator cancel plus `Stop` and `ConfirmStopped` hold.
  - A host torn down mid-execution and restarted on the same state adopts the still-running process at a higher epoch. The old credential is rejected and the adopted process emits with its replaced credential, with exactly one launch.
  - Apply-time config rejection is covered, and so are CLI exit codes and non-echo behavior.

The restart here is an in-process teardown and rebuild of every host component; the shell process is a real OS process in its own session. Killing an actual daemon process at each boundary is the 02-09 gate, delivered in [daemon composition](daemon-core-gate.md). 02-08 adds `Forget` for the shared retention policy: it removes an expired execution's run directories and refuses while a verified process still runs. It also adds history/TUI presentation with log tailing from published artifacts ([observability and history](observability-history.md)). Live log streaming during a run is not provided.

## Inject inbox (04-02)

The shell executor implements `InjectExecutor`, the second implementation of the [generic inject acknowledgement](runtime-semantics.md#generic-inject-acknowledgement-04-02).

- **The inbox.** Each segment has `inbox/` in its run directory, named to the process as `OS_INBOX`. `Inject` writes the Event as `<order>-<id digest>.json`, in the `OS_INPUT_FILE` event shape, under the segment's finalize lock. A repeated Event identity is not written again. After the result is recorded, or for an execution this instance does not hold, `Inject` returns `ErrNotAccepting`.
- **Taking input in.** The process reads the inbox and acknowledges each event by creating `inbox/consumed/<id digest>`. `os inbox` does both: it prints the unacknowledged events in order as JSON lines and acknowledges each one after printing it, without any call to the endpoint.
- **The proof.** The result reports acknowledged, delivered events as `Result.ConsumedInputs`. An acknowledgement for a file core never delivered is ignored.
- **Adoption.** The inbox is on disk, so an adopted process keeps receiving input.
- **Scripts that never read the inbox.** Nothing is consumed. Injected input stays pending and reaches attempt N+1, exactly as `queue` would.

Binding an inject route to shell therefore no longer needs an unsupported-capability fallback. `TestSharedInjectConformance`, `TestInboxIgnoresForgedAcknowledgements` and `TestToolbeltInbox` cover it.
