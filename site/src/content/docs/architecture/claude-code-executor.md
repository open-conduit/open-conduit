---
title: "Claude Code executor and private continuity"
description: "How the Claude Code executor runs agent turns as detached segments, keeps session continuity private to itself, and gives the agent its toolbelt over MCP."
---

The Claude Code executor runs each agent turn as a detached `claude -p` segment on the Work's session, resumes or hands off sessions from continuity it keeps private to itself, and gives the agent its toolbelt over MCP. It lives in [`executors/claudecode`](https://github.com/open-conduit/open-conduit/tree/main/executors/claudecode) and implements the [B03 executor contract](../extensions/). `os mcp`, the toolbelt's MCP transport, lives in [`internal/cli`](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/mcp.go). Like the shell executor, the package imports only public contracts. Its process identity helpers are copied from `executors/shell`, because B01 forbids one executor importing another. `internal/bootstrap` registers it as executor `claude-code`.

The executor advertises `interrupt`, `inject`, `resume`, `workspace` and `toolbelt: mcp`.

## A segment

One segment is `claude -p` on the Work's session:

```
claude -p --model M --permission-mode P --input-format stream-json --output-format stream-json
  --verbose --replay-user-messages [--plugin-dir D] [--json-schema STATUS]
  (--resume S | --session-id S) [--mcp-config <segment>/mcp.json] [extra_args…]
```

The segment runs detached, like the shell executor's:

- **Recovery intent first.** It persists `open-conduit.claudecode.recovery.v1` (naming the segment directory) through `PersistRecovery` before it touches anything else.
- **Process group.** A `/bin/sh` wrapper runs as a new session and process-group leader. Its `$0` carries an unguessable nonce, so identity is checked beyond the PID. The wrapper records its PID and exit status, and it signals the remaining group members (including the MCP server) when claude exits.
- **Files.** stdout goes to `out.jsonl` and stderr to `err.log`.
- **Prompt on stdin.** The first user message is the prompt. stdin stays open while this daemon instance holds the process. The follower closes it when the `result` line arrives, so claude exits.

`Recover` follows the shell executor:

- It adopts a verified live leader at a higher epoch and replaces the MCP credential file first.
- It returns a recorded result identically.
- It refuses lower epochs, tampered metadata, and a dead process with no recorded exit or stop (`ErrNotAdoptable`).
- It never launches.

An adopted segment keeps running and finishes its turn. Its stdin went with the daemon that started it, so it takes no injected input. `Stop`, `Interrupt` (SIGTERM, then SIGKILL after the grace period) and `Forget` behave as they do in the shell executor.

## Prompt, inputs and injection

A turn starts on the playbook skill `/open-conduit:<playbook> <args>` (the default playbook is `work-issue`). The body is:

| Turn | Arguments | Body |
| --- | --- | --- |
| A session's first | `<args>` | `## Already waiting` + the rendered inputs |
| Another turn, or a resumed wake | `<args> events` | `## Since your last turn` + inputs |
| A handoff to a fresh session | `<args> handoff` | `## Handoff from your last session` + note + `## Since your last turn` + inputs |

`args` and `branch` are templates. `{key}` is the Work's first correlation key, `{number}` is the first number in it (the key itself when it has none) and `{work}` is the Work ID. The inputs are the acceptance-ordered `RunRequest.Inputs`. The default renderer knows no service schema: it shows each Event's type, the common payload fields (author, kind, number, path, line, state, title, URL) and the body or text. A tracker extension may supply a richer `Renderer`; GitHub's adds reply hints (see [playbooks and rendering](#playbooks-and-rendering)).

`Inject` writes the Event as a stream-json user message (`## Arrived during this turn …`). claude takes it in before its next model call and echoes it back (`--replay-user-messages`). The echo is recorded as consumption. The result data reports `injected: {accepted, consumed}`, and the echoed Event IDs are returned as `Result.ConsumedInputs`, so core consumes them with the turn and starts no further attempt for them. `Inject` success alone still means accepted for delivery: an input that was never echoed stays eligible for the next attempt. A segment that is adopted or already has its result, or an execution this instance does not hold, returns `extension.ErrNotAccepting`. Core then records the intent as not delivered and keeps the input for the next attempt or wake. A retried dispatch of an Event already on stdin is not written twice. The echo proof applies on waiting and failed turns too, and also when the turn ends before core commits the dispatch acknowledgement ([generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement)). Input accepted just before the result, and so never echoed, is presented again in the same way. The executor passes `conformance.CheckInjectExecutor`.

## Outcomes

The executor adds no core state. The structured status uses `--json-schema` (state, summary, prs, preview, touches, handoff). It maps as follows:

| Turn ended | Outcome | Work afterwards |
| --- | --- | --- |
| `finished` | completed | completed |
| `building`, `review`, `help`, `done` | handoff | ready: the next accepted input starts attempt N+1 |
| The agent called `wait` through MCP | waiting (core converts the result and keeps its data) | parked; the wake resumes the same attempt as a new `--resume` segment |
| Error, no result, no status, interrupt | failed | retry policy |
| Usage limit | failed with `Result.RetryHint{ResetAt}` | retried at the reset plus two minutes (or after 30 minutes if unknown) |

The outcome data schema is the binding's optional `status_schema`. The default is `core.turn.v1`, which every projector reads and from which the host publishes the turn-ended Event; a source-named `<source>.status.v1` that earlier setups wrote is read as `core.turn.v1`. A config that cannot be read fails under `open-conduit.claudecode.result.v1`. The projector maps `help`, `review` and `done` to labels. `building` and `finished` project no status label. The other data fields are:

- `reason`, `prs`, `preview`, `touches` and `kind`
- `models`, `model_calls`, `tool_calls` and `tokens` (input, output, cache read, cache creation)
- `limited` and `resets_at`
- `exit_code` and `injected`
- the `transcript` and `log` artifact IDs
- `workspace` `{ref, schema, data}`

The data never carries the session or the handoff note. Usage maps to `extension.Usage`, with cost in USD and `CachedTokens` holding the cache-read count. A resumed session reports cumulative totals, so the per-turn delta subtracts the session's previous totals, which are kept in continuity. Totals that went backwards (a fork) are left as reported.

**Usage limits.** The executor detects a limit from `api_error: usage_limit_reached`, the `rate_limit_event`/`api_error_params` reset, or claude's wording ("resets 9:40am (Europe/London)"). A limit records a hold for the whole executor until the reset plus the grace period. While the hold lasts, `Run` returns a failed result with the same hint and launches nothing. `extension.Result.RetryHint` lets a structured failed result carry the hint. Core treats it like a `RetryHintProvider` error, but the observed result also proves the attempt finished, so no uncertain hold remains. A hint on any other outcome is invalid. A hinted failure still uses the bounded failure budget.

## Private continuity

Continuity lives in namespaced coordination KV (`executor.claudecode`, key `work/<work id>`). Core never reads it. It holds:

- the session and past sessions
- the count of turns that ended with a status
- the handoff note and the last state
- the last turn's end time
- the session's cumulative totals
- the in-flight decision
- the attach command

Before launch, `Run` decides between resume and fresh and records that decision in a transaction fenced by the attempt's lease. A crash after the record is written, but before launch, leaves an attempt that cannot be adopted. Its retry takes the recorded decision over (same session and kind) instead of deciding again. Finishing a segment applies the turn exactly once per segment (the same per-turn delta every time), before the result is recorded. Applying it again after a crash changes nothing.

The resume and handoff rules are:

- A session whose transcript exists in the workspace (`<config>/projects/<cwd with non-alphanumerics as ->/<session>.jsonl`) resumes, even when its first turn never ended.
- A session with a handoff note that was never written starts afresh from the note.
- A session is *cold* when its last turn ended more than `handoff_minutes` ago (default 55). A cold session follows `cold_turns`: `handoff` (the default, and what the playbook tells agents to expect) starts a fresh session from the note, `resume` resumes it.
- `Attach` returns `claude --resume <session>` in the workspace and clears the note, so the next turn resumes even when cold. It implements the public `extension.AttachExecutor`; Work without a session returns `ErrNoSession`, which wraps `extension.ErrNothingToAttach`.

**Release.** `Maintain` releases terminal Work's continuity. Records written while the handoff experiment ran may still carry a trial and feedback questions; they are ignored.

Telemetry goes to `Options.Report`. The daemon writes it to `logs/claude-code.jsonl`, outside core, as one `agent_turn` record per turn.

Like core telemetry, it is at most once across a kill.

## Toolbelt over MCP

When the attempt has a toolbelt token and the `os` binary is available, the segment's `mcp.json` defines the stdio server `open-conduit`: `<toolbelt dir>/os mcp`, with `OS_ENDPOINT` and `OS_TOKEN_FILE` (a private 0600 file: the host-kept `ToolbeltTokenFile` in the execution's run directory when the host gives one, see [execution endpoints](../toolbelt-management/#execution-endpoints), otherwise one in the segment directory). The token itself never appears in the file or in claude's environment. Adoption replaces the token file, and `os mcp` re-reads it on every call.

`os mcp` speaks newline-delimited JSON-RPC 2.0 (`initialize` with version negotiation, `ping`, `tools/list` and `tools/call`) and offers these tools:

- `read_context`
- `emit`
- `create_work`
- `wait`
- `declare_outcome`
- `call` (registered extension operations)

Their inputs are the toolbelt wire inputs, and every call goes through `POST /v1/toolbelt/call`, the CLI's authority path. Refusals come back as `isError` tool results carrying only the safe error code. claude's own environment gets the execution identity (`OS_SCOPE_ID`, `OS_WORK_ID`, `OS_EXECUTION_ID`, `OS_ATTEMPT`) but no toolbelt credential.

## Playbooks and rendering

The playbooks, waits and GitHub tools themselves are described in [engineering tools, waits and playbooks](../engineering-tools/). The executor's side is:

- **Playbooks.** Bootstrap writes the playbook plugin to `<state>/playbooks/plugin`, the default `Options.PluginDir` for bindings without `plugin_dir`.
- **Project additions.** Read from the turn's workspace (`.open-conduit/playbooks/<playbook>.md`) and appended to the turn body.
- **Rendering.** A GitHub `Options.Render` with reply hints naming toolbelt calls.
- **Tool calls.** The MCP `call` tool sends its `input` object as payload data, with the `file_path`/`save_to` file conventions.

A `call github.wait` parks the attempt exactly like `wait`, because `Complete` applies the declaration.

## Operator controls

[Operator setup](../operator-setup/) covers attach through `POST /work/{id}/attach` under an operator hold, transcripts reachable as run artifacts, `Usage` (opt-in, doctor), `Version` (doctor), `Options.DryRun` (a stub turn on `haiku`) and the `init --with claude` setup command. The executor also renders each turn's activity as a run artifact, shown by the TUI and `runs log` ([installed integrations](../installed-integrations/#operator-tools)).

Handed-back Work sits `ready` with no pending input, and the host loop skips it quietly.

## Workspace and environment

With a binding resource, a new segment calls `Acquire({Mode: workspace_mode (default "work"; "clone" for a git clone that shares nothing writable with the checkout), Params: {"branch": …} when branch is set})`. The executor reports that mode to configuration validation (`config.ResourceModer`), so a sandboxed binding can be checked against it ([sandboxed bindings](../workspaces/#sandboxed-bindings)). A resumed segment calls `Lookup` on the parked segment's lease reference, which never creates anything. claude then runs in `Resource.Dir`. The reference is kept with the segment and reported in the result. A setup failure or an unavailable workspace is a structured failure with no process. Without a resource, the session lives in the binding's `dir`, used as-is and shared by every Work on the binding, or else in a private directory that stays the same for the Work. A `dir` that is gone at turn start is a structured `workspace_unavailable` failure, and a binding with both a resource and `dir` fails its turns as `invalid_config`. The executor never removes workspaces; the host reconciles them (B07).

A binding with a sandbox ([B09](../sandboxes/)) wraps claude through `extension.WrapLaunch`, needing its workspace, its own config directory (`CLAUDE_CONFIG_DIR` in the binding's `env`, apart from the operator's `~/.claude` and `~/.claude.json`, which its needs deny; [B09](../sandboxes/#executors)), its MCP config, the playbooks, its token directory and toolbelt socket, and `api.anthropic.com`. Sandboxed, `TMPDIR` and `CLAUDE_CODE_TMPDIR` are the segment's own `tmp` directory, while srt itself keeps `/tmp` for its sockets. A refusal is a structured `sandbox_denied` or `sandbox_unavailable` failure with no process, `Recover` adopts a live segment only once `extension.AdoptSandboxed` proves it is in its sandbox, and an interrupted turn's result says whether it got `term` or `kill`. Claude Code's own `sandbox.*` settings are executor config, not a provider; they can be added inside an srt sandbox with `extra_args` as defence in depth.

The environment follows B05:

1. **Sanitized baseline.** The shell baseline plus `CLAUDE_CONFIG_DIR` and the binding's `pass_env` names. `ANTHROPIC_API_KEY`, `CLAUDECODE`, `CLAUDE_CODE_*` and other credentials are dropped.
2. **Provider layer.** Any `OS_*` names are dropped from it.
3. **Binding `env`.**
4. **Selected secrets.** An API key comes only from a secret mapped with `secret_env`.
5. **Execution identity.**

## Binding config

The config is strict and validated at load. Every field is optional:

| Field | Meaning |
| --- | --- |
| `binary` | The Claude Code CLI (default `claude`). |
| `model` | The model (default `opus`). |
| `permission_mode` | Default `bypassPermissions`. |
| `playbook` | Default `work-issue`; `-` means none. |
| `plugin_dir` | The playbook skills directory. |
| `args` | Default `{number}`. |
| `structured` | Default true. |
| `status_schema` | The outcome data schema (default `core.turn.v1`). |
| `branch`, `workspace_mode` | The workspace request. |
| `dir` | An absolute folder a workspace-free binding's sessions run in, used as-is. A folder that has gone fails only that binding's turns, as `workspace_unavailable`, never the config. In an [included folder](../config-management/#included-folders), `dir` defaults to the folder (unless the binding has a resource), and a relative `dir` or `plugin_dir` resolves against it. |
| `handoff_minutes` | How long after a turn the session counts as cold (default 55). |
| `cold_turns` | What a cold turn does: `handoff` (default) or `resume`. `handoff_share`, the removed experiment's setting, is ignored with a `config check` warning for one release, then rejected. |
| `env`, `pass_env`, `secret_env` | The environment. |
| `extra_args` | Extra arguments. These may not override `-p`, the session or the MCP flags, nor `--tools` when `builtin_tools` is set. |
| `builtin_tools` | The only built-in tools Claude Code offers, passed as `--tools`. An empty list turns them all off, so the agent has only its MCP toolbelt: no shell, file access or web. Unset keeps Claude Code's default set. Attaching to the session keeps the same limit. |
| `strict_mcp` | `true` passes `--strict-mcp-config`, so the toolbelt is the agent's only MCP server: none of the operator's own, a project's or claude.ai's servers and connectors load. Attaching keeps it. Default `false`. |
| `models` | Models a Work may pick for its turns through `model` in its context, for example one an operator chose when starting it. A Work that names none, or one not listed, uses `model`. |

## Setup probe

`claude-code.status` is the executor's probe for [setup sessions](../setup-sessions/) (`extension.SetupProbes`). It says whether `claude` is on the daemon's `PATH`, its version, and whether it is signed in and how: `subscription` when a Claude login is found (the macOS Keychain item or `~/.claude/.credentials.json`), `api_key` when `ANTHROPIC_API_KEY` is set in the daemon's environment. It never returns the token or account detail, and it starts no session.

## Verification

- [Unit tests](https://github.com/open-conduit/open-conduit/tree/main/executors/claudecode) run a fake `claude`, which records its invocation, echoes stream-json replays, writes the transcript and reports cumulative totals. They cover:
  - the shared executor conformance case
  - the invocation, the MCP config and the sanitized environment
  - resumed and fresh sessions with per-turn usage deltas
  - the port of `TestColdSessionHandsOffToAFreshOne`: warm resume, a cold handoff, a cold resume under `cold_turns: resume`, attach forcing resume and release of terminal Work
  - the cold decision across executor restart: adopting the running turn and taking over an undone decision
  - continuity left by the handoff experiment loading and releasing without error
  - interjection consumption
  - usage-limit delay and the executor-wide hold
  - failures, interrupt and missing binary
  - detached adoption with credential replacement and lower-epoch and superseded refusals
  - work-workspace acquisition and resume by lookup
  - config validation
  - the ported parser tests (parse, limits, reset text, call counts, `perTurn`)
- [`TestProductionSchedulerResultRetryHint`](https://github.com/open-conduit/open-conduit/blob/main/integration/scheduler_test.go) checks that a result hint delays the retry and leaves no uncertain hold.
- [`TestDaemonClaudeCodeExecution`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_claudecode_test.go) runs against the composed daemon:
  - a fake claude drives the real `os mcp` (initialize, tools/list, read_context, wait) and parks
  - the wake resumes the same session
  - an event mid-turn is injected and consumed
  - a turn running at a daemon SIGKILL is adopted with one launch
  - no runtime API shows the session
- [`TestDaemonClaudeCodeContinuityKill`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_claudecode_test.go) uses the `kv` fault boundary to SIGKILL the daemon before and after the decision commit. The retry launches once and resumes the Work's session.

## Limitations

- **No "in flight" list.** The cross-issue "in flight" list has no Open Conduit equivalent; the playbook instead tells the agent to read overlapping issues and their PRs.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **03-03** delivered the executor, private continuity, the handoff experiment and `os mcp`. No real Claude Code session ran; all evidence used fake binaries and fixture transcripts. Real-agent turns, real limits, real resume and real handoff telemetry were left to 03-06. At that point reply-command hints, the playbooks, plugin preparation and the engineering tools were deferred to 03-04, and operator controls to 03-05. Handed-back `ready` Work was reported on every host pass.
- **03-04** delivered the playbooks and plugin, project additions, the GitHub renderer with reply hints and the MCP `call` payload and file conventions.
- **03-05** delivered attach, transcripts as run artifacts, `Usage`, `Version`, dry run and `init --with claude`, and made the host loop skip idle `ready` Work quietly. A rendered transcript and activity view was not ported then; it arrived later as the activity artifact.
- **03-06** made an echoed injection consumed with its turn through `Result.ConsumedInputs`, after a real turn showed the extra attempt it otherwise caused.
- **04-02** extended that proof to every outcome and to the in-flight acknowledgement race, and stopped a retried dispatch writing an Event to stdin twice.
- At 03-03 the refusal was documented as `ErrNotAccepting`; it is now `extension.ErrNotAccepting`.
- **07-01** made `Attach` implement the public `extension.AttachExecutor`.
- `extension.Result.RetryHint` was a contract clarification made for this executor; the bounded failure budget it still uses dates from 02-04.
- **#73** removed the handoff experiment (the draw, trials, the blinded feedback question and `handoff_share`); a cold turn now follows the binding's `cold_turns`.
- **#164** added the sandbox wrap ([B09](../sandboxes/)).
