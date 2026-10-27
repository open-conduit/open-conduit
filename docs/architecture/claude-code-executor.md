# Claude Code executor and private continuity

Ticket 03-03 delivers [`executors/claudecode`](../../executors/claudecode), the agent executor behind the [B03 executor contract](extensions.md), and `os mcp`, the toolbelt's MCP transport in [`internal/cli`](../../internal/cli/mcp.go). Like the shell executor, the package imports only public contracts. Its process identity helpers are copied from `executors/shell`, because B01 forbids one executor importing another. `internal/bootstrap` registers it as executor `claude-code`.

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

`args` and `branch` are templates. `{key}` is the Work's first correlation key, `{number}` is the first number in it (the key itself when it has none) and `{work}` is the Work ID. The inputs are the acceptance-ordered `RunRequest.Inputs`. The default renderer knows no service schema: it shows each Event's type, the common payload fields (author, kind, number, path, line, state, title, URL) and the body or text. A tracker extension may supply a richer `Renderer`. Reply-command hints depend on the engineering tools, so they belong to 03-04, along with the playbooks themselves and the cross-issue "in flight" list.

`Inject` writes the Event as a stream-json user message (`## Arrived during this turn …`). claude takes it in before its next model call and echoes it back (`--replay-user-messages`). The echo is recorded as consumption. The result data reports `injected: {accepted, consumed}`, and the echoed Event IDs are returned as `Result.ConsumedInputs`, so core consumes them with the turn and starts no further attempt for them (added in 03-06 after a real turn showed the extra attempt). `Inject` success alone still means accepted for delivery: an input that was never echoed stays eligible for the next attempt. A segment that is adopted or already has its result, or an execution this instance does not hold, returns `ErrNotAccepting` (now `extension.ErrNotAccepting`). Core then records the intent as not delivered and keeps the input for the next attempt or wake. Since 04-02 a retried dispatch of an Event already on stdin is not written twice. The echo proof applies on waiting and failed turns too, and also when the turn ends before core commits the dispatch acknowledgement ([generic inject acknowledgement](runtime-semantics.md#generic-inject-acknowledgement-04-02)).

## Outcomes

No core state was added. The structured status uses `--json-schema` (state, summary, prs, preview, touches, handoff). It maps as follows:

| Turn ended | Outcome | Work afterwards |
| --- | --- | --- |
| `finished` | completed | completed |
| `building`, `review`, `help`, `done` | handoff | ready: the next accepted input starts attempt N+1 |
| The agent called `wait` through MCP | waiting (core converts the result and keeps its data) | parked; the wake resumes the same attempt as a new `--resume` segment |
| Error, no result, no status, interrupt | failed | retry policy |
| Usage limit | failed with `Result.RetryHint{ResetAt}` | retried at the reset plus two minutes (or after 30 minutes if unknown) |

The outcome data schema is the binding's `status_schema`. The default is `open-conduit.claudecode.result.v1`. A GitHub binding sets `github.status.v1`, so the projector reads `status` directly. The projector maps `help`, `review` and `done` to labels. `building` and `finished` project no status label. The other data fields are:

- `reason`, `prs`, `preview`, `touches` and `kind`
- `models`, `model_calls`, `tool_calls` and `tokens` (input, output, cache read, cache creation)
- `limited` and `resets_at`
- `exit_code` and `injected`
- the `transcript` and `log` artifact IDs
- `workspace` `{ref, schema, data}`

The data never carries the session, the handoff note or the experiment arm. Usage maps to `extension.Usage`, with cost in USD and `CachedTokens` holding the cache-read count. A resumed session reports cumulative totals, so the per-turn delta subtracts the session's previous totals, which are kept in continuity. Totals that went backwards (a fork) are left as reported.

**Usage limits.** The executor detects a limit from `api_error: usage_limit_reached`, the `rate_limit_event`/`api_error_params` reset, or claude's wording ("resets 9:40am (Europe/London)"). A limit records a hold for the whole executor until the reset plus the grace period. While the hold lasts, `Run` returns a failed result with the same hint and launches nothing. Contract clarification: `extension.Result.RetryHint` lets a structured failed result carry the hint. Core treats it like a `RetryHintProvider` error, but the observed result also proves the attempt finished, so no uncertain hold remains. A hint on any other outcome is invalid. A hinted failure still uses the bounded failure budget (02-04).

## Private continuity

Continuity lives in namespaced coordination KV (`executor.claudecode`, key `work/<work id>`). Core never reads it. It holds:

- the session and past sessions
- the count of turns that ended with a status
- the handoff note and the last state
- the last turn's end time
- the session's cumulative totals
- the in-flight decision
- the open trial and the feedback questions
- the attach command

Before launch, `Run` decides between resume and fresh and records that decision in a transaction fenced by the attempt's lease. A crash after the record is written, but before launch, leaves an attempt that cannot be adopted. Its retry takes the recorded decision over (same session and arm) instead of drawing again. Finishing a segment applies the turn exactly once per segment (the same per-turn delta every time), before the result is recorded. Applying it again after a crash changes nothing.

The resume and handoff rules are:

- A session whose transcript exists in the workspace (`<config>/projects/<cwd with non-alphanumerics as ->/<session>.jsonl`) resumes, even when its first turn never ended.
- A session with a handoff note that was never written starts afresh from the note.
- A session is *cold* when its last turn ended more than `handoff_minutes` ago (default 55). A cold session is drawn into the experiment: `handoff_share` of the draws (default 0.5) go to a fresh session started from the note (arm `handoff`), and the rest resume (arm `resume`).
- `Attach` returns `claude --resume <session>` in the workspace and clears the note, so the next turn resumes even when cold. Since 07-01 it implements the public `extension.AttachExecutor`; Work without a session returns `ErrNoSession`, which wraps `extension.ErrNothingToAttach`.

**Trials and feedback.**

- **Trials.** Each draw starts a trial. The trial adds up turns, failures (usage limits excluded), cost, tokens, model and tool calls, agent seconds and follow-ups, until the next draw (`next_draw`), a `finished` status (`finished`) or the Work becoming terminal (`dropped`, with its last state).
- **The question.** A successful drawn turn queues the blinded `FeedbackText`, which is identical for both arms. `Maintain` asks it through the host's `FeedbackHook`. For GitHub, bootstrap uses `extensions/github.WorkComments`: a `Projector.Deliver` intent with a stable ID and marker, so the comment posts exactly once even after an ambiguous post.
- **The answer.** `Maintain` reads reactions for up to seven days. 👍 means `maintained`, 👎 means `worse`, and both mean `mixed`.
- **Release.** Terminal Work's continuity is released once nothing is left to watch.

Telemetry goes to `Options.Report`. The daemon writes it to `logs/claude-code.jsonl`, outside core, in these records:

- `agent_turn`
- `handoff_assigned`
- `handoff_trial`
- `handoff_feedback_asked`
- `handoff_feedback`

Like core telemetry, it is at most once across a kill. The durable trial counters are not affected.

## Toolbelt over MCP

When the attempt has a toolbelt token and the `os` binary is available, the segment's `mcp.json` defines the stdio server `open-conduit`: `<toolbelt dir>/os mcp`, with `OS_ENDPOINT` and `OS_TOKEN_FILE` (a private 0600 file). The token itself never appears in the file or in claude's environment. Adoption replaces the token file, and `os mcp` re-reads it on every call.

`os mcp` speaks newline-delimited JSON-RPC 2.0 (`initialize` with version negotiation, `ping`, `tools/list` and `tools/call`) and offers these tools:

- `read_context`
- `emit`
- `create_work`
- `wait`
- `declare_outcome`
- `call` (registered extension operations)

Their inputs are the toolbelt wire inputs, and every call goes through `POST /v1/toolbelt/call`, the CLI's authority path. Refusals come back as `isError` tool results carrying only the safe error code. claude's own environment gets the execution identity (`OS_SCOPE_ID`, `OS_WORK_ID`, `OS_EXECUTION_ID`, `OS_ATTEMPT`) but no toolbelt credential.

## Workspace and environment

With a binding resource, a new segment calls `Acquire({Mode: workspace_mode (default "work"), Params: {"branch": …} when branch is set})`. A resumed segment calls `Lookup` on the parked segment's lease reference, which never creates anything. claude then runs in `Resource.Dir`. The reference is kept with the segment and reported in the result. A setup failure or an unavailable workspace is a structured failure with no process. Without a resource, the session lives in a private directory that stays the same for the Work. The executor never removes workspaces; the host reconciles them (B07).

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
| `status_schema` | The outcome data schema. |
| `branch`, `workspace_mode` | The workspace request. |
| `handoff_minutes`, `handoff_share` | The experiment (55 and 0.5; a share of 0 turns it off). |
| `env`, `pass_env`, `secret_env` | The environment. |
| `extra_args` | Extra arguments. These may not override `-p`, the session or the MCP flags. |

## Verification

- [Unit tests](../../executors/claudecode) run a fake `claude`, which records its invocation, echoes stream-json replays, writes the transcript and reports cumulative totals. They cover:
  - the shared executor conformance case
  - the invocation, the MCP config and the sanitized environment
  - resumed and fresh sessions with per-turn usage deltas
  - the complete port of `TestColdSessionHandsOffToAFreshOne`: warm resume, handoff arm, resume arm, blinded identical questions, reactions, attach forcing resume and trial accounting to the next draw and the finish
  - arm and trial continuity across executor restart: adopting the drawn turn, taking over an undone decision and answering feedback from a new instance
  - `TestDroppingAnIssueEndsItsTrial`
  - interjection consumption
  - usage-limit delay and the executor-wide hold
  - failures, interrupt and missing binary
  - detached adoption with credential replacement and lower-epoch and superseded refusals
  - work-workspace acquisition and resume by lookup
  - config validation
  - the ported parser tests (parse, limits, reset text, call counts, `perTurn`)
- [`TestProductionSchedulerResultRetryHint`](../../integration/scheduler_test.go) checks that a result hint delays the retry and leaves no uncertain hold.
- [`TestGitHubWorkCommentsForExecutorFeedback`](../../integration/github_test.go) covers the marked comment, which posts exactly once and recovers by marker, and the reaction read.
- [`TestDaemonClaudeCodeExecution`](../../integration/daemon_claudecode_test.go) runs against the composed daemon:
  - a fake claude drives the real `os mcp` (initialize, tools/list, read_context, wait) and parks
  - the wake resumes the same session
  - an event mid-turn is injected and consumed
  - a turn running at a daemon SIGKILL is adopted with one launch
  - no runtime API shows the session
- [`TestDaemonClaudeCodeContinuityKill`](../../integration/daemon_claudecode_test.go) uses the new `kv` fault boundary to SIGKILL the daemon before and after the decision commit. The retry launches once and resumes the Work's session.

## Limits and follow-on work

- **No real claude.** No real Claude Code session ran; all evidence uses fake binaries and fixture transcripts. Real-agent turns, real limits, real resume and real handoff telemetry are 03-06 evidence.
- **Playbooks and tools (03-04).** The playbooks, plugin preparation, the reply commands and the engineering tools.
- **Operator controls (03-05).** Delivered in [operator setup](operator-setup.md): attach through `POST /work/{id}/attach` under an operator hold, transcripts reachable as run artifacts, `Usage` (opt-in, doctor), `Version` (doctor), `Options.DryRun` (a stub turn on `haiku`) and the `init --with claude` setup command. A rendered transcript and activity view is not ported.
- **Not ported yet.** The cross-issue "in flight" list has no Open Conduit equivalent.
- **Inject consumption.** Since 03-06 an echoed injection is consumed with its turn through `Result.ConsumedInputs`. Since 04-02 this holds on every outcome and in the in-flight acknowledgement race. Injected input that was never echoed (for example, accepted just before the result) is presented again to the next attempt or wake. The executor passes `conformance.CheckInjectExecutor`.
- **Idle `ready` Work.** Handed-back Work sits `ready` with no pending input. Since 03-05 the host loop skips it quietly.

03-04 update: [engineering tools, waits and playbooks](engineering-tools.md) delivers the following:

- **Playbooks.** The playbooks and the plugin that bootstrap writes to `<state>/playbooks/plugin`, the default `Options.PluginDir` for bindings without `plugin_dir`.
- **Project additions.** Read from the turn's workspace (`.open-conduit/playbooks/<playbook>.md`) and appended to the turn body.
- **Rendering.** A GitHub `Options.Render` with reply hints naming toolbelt calls.
- **Tool calls.** The MCP `call` tool sends its `input` object as payload data, with the `file_path`/`save_to` file conventions.

A `call github.wait` parks the attempt exactly like `wait`, because `Complete` applies the declaration.
