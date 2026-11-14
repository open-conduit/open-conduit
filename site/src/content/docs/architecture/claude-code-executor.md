---
title: "Claude Code executor and private continuity"
description: "How the Claude Code executor runs agent turns as detached segments, keeps session continuity private to itself, and gives the agent its toolbelt over MCP."
---

The Claude Code executor runs each agent turn as a detached `claude -p` segment on the Work's session, resumes or hands off sessions from continuity it keeps private to itself, and gives the agent its toolbelt over MCP. It lives in [`extensions/claudecode`](https://github.com/open-conduit/open-conduit/tree/main/extensions/claudecode) and implements the [B03 executor contract](../extensions/). `os mcp`, the toolbelt's MCP transport, lives in [`internal/cli`](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/mcp.go). Like the shell executor, the package imports only public contracts. Its process identity helpers are copied from `extensions/shell`, because B01 forbids one executor importing another. Its package, [`extensions/claudecode/pkg`](https://github.com/open-conduit/open-conduit/tree/main/extensions/claudecode/pkg), serves it as executor `claude-code`.

The executor advertises `interrupt`, `inject`, `resume`, `workspace`, `sandbox`, `container` and `toolbelt: mcp`. Its extension also registers the [task operations](#background-tasks) (`claude-code.task.*`, toolset `@claude-code.tasks`) and their Event, `claude-code.task.ended`. It runs as a package bundled with the release, served by `open-conduit extension-run claude-code`.

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

A turn starts on the playbook skill `/open-conduit:<playbook> <args>`, or on the arguments and body alone when the binding names no playbook (`-`). The body is:

| Turn | Arguments | Body |
| --- | --- | --- |
| A session's first | `<args>` | `## Already waiting` + the rendered inputs |
| Another turn, or a resumed wake | `<args> events` | `## Since your last turn` + inputs |
| A handoff to a fresh session | `<args> handoff` | `## Handoff from your last session` + note + `## Since your last turn` + inputs |

`args` and `branch` are templates. `{key}` is the Work's first correlation key other than its lineage key (`core.work`/`<its ID>`, which the host adds when the Work first waits with no key or asks for child Work, so the arguments never change partway through its life), `{number}` is the first number in it (the key itself when it has none) and `{work}` is the Work ID. The inputs are the acceptance-ordered `RunRequest.Inputs`. The default renderer knows no service schema: it shows each Event's type, the common payload fields (author, kind, number, path, line, state, title, URL) and the body or text. A payload with neither a body nor text shows its other fields as compact JSON, clipped at 2000 bytes, so an Event no renderer knows (an operator's `POST /v1/events` to a Work, say) still reaches the turn with its data. A tracker extension may supply a richer `Renderer`; GitHub's adds reply hints (see [playbooks and rendering](#playbooks-and-rendering)).

`Inject` writes the Event as a stream-json user message (`## Arrived during this turn …`). claude takes it in before its next model call and echoes it back (`--replay-user-messages`). The echo is recorded as consumption. The result data reports `injected: {accepted, consumed}`, and the echoed Event IDs are returned as `Result.ConsumedInputs`, so core consumes them with the turn and starts no further attempt for them. `Inject` success alone still means accepted for delivery: an input that was never echoed stays eligible for the next attempt. A segment that is adopted or already has its result, or an execution this instance does not hold, returns `extension.ErrNotAccepting`. Core then records the intent as not delivered and keeps the input for the next attempt or wake. A retried dispatch of an Event already on stdin is not written twice. The echo proof applies on waiting and failed turns too, and also when the turn ends before core commits the dispatch acknowledgement ([generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement)). Input accepted just before the result, and so never echoed, is presented again in the same way. The executor passes `conformance.CheckInjectExecutor`.

## Outcomes

The executor adds no core state. The structured status uses `--json-schema` (state, summary, handoff, report, result). Its descriptions say only what each state means to Open Conduit: `finished` completes the Work; `building`, `review`, `help` and `done` keep it open for the next input, with `review`, `help` and `done` putting something to the owner. They name no code host, issue, PR, check or preview: what a state means for a kind of Work (that `building` follows a push, say) is its playbook's to say, as `work-issue`'s "Ending a turn" does. A test (`TestNoDomainTerms`, with a negative control) keeps such terms out of the executor's sources. It maps as follows:

| Turn ended | Outcome | Work afterwards |
| --- | --- | --- |
| `finished` | completed | completed |
| `building`, `review`, `help`, `done` | handoff (completed for a `RunOnce` Work kind) | ready: the next accepted input starts attempt N+1 |
| The agent called `wait` through MCP | waiting (core converts the result and keeps its data) | parked; the wake resumes the same attempt as a new `--resume` segment |
| Error, no result, no status, interrupt | failed | retry policy |
| Usage limit | failed with `Result.RetryHint{ResetAt}` | retried at the reset plus two minutes (or after 30 minutes if unknown) |

The outcome data schema is the binding's optional `status_schema`. The default is `core.turn.v1`, which every projector reads and from which the host publishes the turn-ended Event; a source-named `<source>.status.v1` that earlier setups wrote in their config is read as `core.turn.v1`. The five states are this executor's default vocabulary, not the core's: the host publishes a turn's end for any status. A config that cannot be read fails under `open-conduit.claudecode.result.v1`. The projector maps `help`, `review` and `done` to labels. `building` and `finished` project no status label. The other data fields are:

- `reason` and `kind`
- `report`, the status's optional write-up for the operator (at most 20,000 bytes), which the web view shows under the turn's summary
- `result`, the status's answer to the Work that asked for this one ([child Work](../routing-lifecycle/#child-work)), kept whole: the parent receives it in `core.work.child.ended` when the turn ends the Work, and in `core.work.child.reported` when it leaves the Work open (any status but `finished`). An unstructured turn of child Work gives its final text as its result, having no status to carry one. A child's first turn renders its creation as the task it was asked (the context's `task`, else its data), and a parent's turn renders a child's end with its state, summary and result in full, and a child's report with its status, summary and result and that it is still open
- `models`, `model_calls`, `tool_calls` and `tokens` (input, output, cache read, cache creation)
- `limited` and `resets_at`
- `exit_code` and `injected`
- the `transcript` and `log` artifact IDs
- `workspace` `{ref, schema, data}`
- `tasks_unwatched`: how many of the Work's [tasks](#background-tasks) were still running when the turn ended without parking on them, when any were

The data never carries the session or the handoff note. Usage maps to `extension.Usage`, with cost in USD and `CachedTokens` holding the cache-read count. A resumed session reports cumulative totals, so the per-turn delta subtracts the session's previous totals, which are kept in continuity. Totals that went backwards (a fork) are left as reported.

**Usage limits.** The executor detects a limit from `api_error: usage_limit_reached`, the `rate_limit_event`/`api_error_params` reset, or claude's wording ("resets 9:40am (Europe/London)"). A limit records a hold for the whole executor until the reset plus the grace period. While the hold lasts, `Run` returns a failed result with the same hint and launches nothing. `extension.Result.RetryHint` lets a structured failed result carry the hint. Core treats it like a `RetryHintProvider` error, but the observed result also proves the attempt finished, so no uncertain hold remains. A hint on any other outcome is invalid. A hinted failure still uses the bounded failure budget.

## Private continuity

Continuity lives in namespaced coordination KV (the package's own `claude-code`, key `work/<work id>`). Core never interprets it. It holds:

- the session and past sessions
- the count of turns that ended with a status
- the handoff note and the last state
- the last turn's end time
- the session's context when the last turn ended
- the session's cumulative totals
- the in-flight decision
- the attach command

Before launch, `Run` decides between resume and fresh and records that decision in a transaction fenced by the attempt's lease. A crash after the record is written, but before launch, leaves an attempt that cannot be adopted. Its retry takes the recorded decision over (same session and kind) instead of deciding again. Finishing a segment applies the turn exactly once per segment (the same per-turn delta every time), before the result is recorded. Applying it again after a crash changes nothing.

The resume and handoff rules are:

- A session whose transcript exists in the workspace (`<config>/projects/<cwd with non-alphanumerics as ->/<session>.jsonl`) resumes, even when its first turn never ended.
- A session with a handoff note that was never written starts afresh from the note.
- A session is *large* when its last turn ended with more than `handoff_tokens` of context (default 400000; 0 turns this off). A large session starts a fresh session from the note, even while its cache is warm: one cache write of the small new context soon costs less than reading the large one on every call. The context is the last main-thread model call's input (cached or not) and output, from that message's `usage` in the stream-json output; a subagent's calls don't count.
- A session is *cold* when its last turn ended more than `handoff_minutes` ago (default 55). A cold session follows `cold_turns`: `handoff` (the default, and what the playbook tells agents to expect) starts a fresh session from the note, `resume` resumes it.
- `Attach` returns `claude --resume <session>` in the workspace and clears the note, so the next turn resumes even when cold. It implements the public `extension.AttachExecutor`; Work without a session returns `ErrNoSession`, which wraps `extension.ErrNothingToAttach`.

**Release.** `Maintain` releases terminal Work's continuity. Records written while the handoff experiment ran may still carry a trial and feedback questions; they are ignored.

Telemetry goes to `Options.Report`. The executor's extension (`claudecode.NewExtension`, served as a package through `inproc.Executing`) writes it to its own log, `logs/claude-code.jsonl` in its package's state directory (`<state>/extensions/claude-code`, through `inproc.Host.OpenLog`, rotated by size like the daemon's own logs, `sdk.RotatingLog`), outside core, as one `agent_turn` record per turn. A record carries `context_tokens` (the session's context when the turn ended) and, for a handoff turn, `handoff_reason`: `size`, `cold` or `missing` (the session's transcript was gone). Its housekeeping (`Maintain`) is the extension's `maintain` task, every 30 seconds.

Like core telemetry, it is at most once across a kill.

## Toolbelt over MCP

When the attempt has a toolbelt token and the `os` binary is available, the segment's `mcp.json` defines the stdio server `open-conduit`: `<toolbelt dir>/os mcp`, with `OS_ENDPOINT` and `OS_TOKEN_FILE` (a private 0600 file: the host-kept `ToolbeltTokenFile` in the execution's run directory when the host gives one, see [execution endpoints](../toolbelt-management/#execution-endpoints), otherwise one in the segment directory). The token itself never appears in the file or in claude's environment. Adoption replaces the token file, and `os mcp` re-reads it on every call.

`os mcp` speaks newline-delimited JSON-RPC 2.0 (`initialize` with version negotiation, `ping`, `tools/list` and `tools/call`). Its `initialize` answer's `instructions` are a fixed sentence, then an `## Operations` list of each operation the execution is granted with its one-line summary and input fields (`body`, `number`? for an optional one), then the guidance of each, all fetched from `GET /v1/toolbelt/guidance`; an extension supplies the summary, input and guidance for its own operations (`extension.Operation.Summary`, `Input` and `Guidance`), so a playbook need not restate them. When the fetch fails (an older daemon) the sentence stands alone. It offers these tools:

- `read_context`
- `emit`
- `create_work`
- `cancel_work`
- `wait`
- `declare_outcome`
- `call` (registered extension operations)

Their inputs are the toolbelt wire inputs, and every call goes through `POST /v1/toolbelt/call`, the CLI's authority path. Refusals come back as `isError` tool results carrying only the safe error code. A call that fails without the daemon's answer says why, also as an `isError` result: no answer within the call's 60 seconds (the operation may still finish, and a retry with the same `request_id` never repeats it), the management endpoint unreachable (nothing listening on the socket, with the cause), or the connection breaking mid-call. Only the second reads "unreachable", so a slow operation, such as one that reads an issue and every page of its comments, is not mistaken for a daemon that is down. claude's own environment gets the execution identity (`OS_SCOPE_ID`, `OS_WORK_ID`, `OS_EXECUTION_ID`, `OS_ATTEMPT`) but no toolbelt credential.

## Background tasks

A segment's wrapper signals its whole process group when claude exits, so whatever the agent left running in the background (a backgrounded shell command, a Monitor, an in-process Workflow) is stopped with the turn (#556). Work that should outlive the turn runs as a *task* instead. The executor starts it, the runtime does the waiting, and nothing has to stay alive for it.

| Operation | Input | Does |
| --- | --- | --- |
| `claude-code.task.start` | `command`, `name`?, `timeout_minutes`? | starts the command as a task of the calling Work |
| `claude-code.task.list` | | the Work's tasks: state, exit code, signal, last line, log |
| `claude-code.task.stop` | `task` (ID or name) | stops a running task |
| `claude-code.task.wait` | `tasks`?, `until`? | parks the Work until one of the tasks ends (default: every running one), or until a time |

- **Where it runs.** `task.start` runs only from a live segment that this executor instance launched, at the caller's lease epoch. It starts `/bin/sh -c <command>` under a wrapper of its own (`open-conduit-task:<nonce>` in `$0`, the segment wrapper's shape), as a new session and process-group leader. That puts it outside the segment's group and outside the daemon's. It runs in the segment's working directory, with the segment's environment minus every `OS_*` name: a task outlives the segment's authority, so it has no toolbelt. Output goes to `<state>/tasks/<id>/log`. The wrapper writes `pid` and, atomically, `exit`.
- **What it refuses.** It fails closed with `not_supported` in these cases:
  - a container binding (the container ends with the segment)
  - a sandboxed binding (the task would run outside the sandbox)
  - a `fresh` or `shared` workspace (given up when the segment ends)
  - an adopted segment (its environment went with the daemon that launched it)
  - a turn that has ended

  It also refuses a ninth running task, or a running task's name used again.
- **Identity.** The task ID derives from the execution and the call's request ID, so a retried start returns the same task and starts nothing more. A record (`task/<id>` in the `claude-code` namespace) holds the Work, scope, execution, the attempt's trigger, the nonce, the state and the deadline. It is written before the process starts, so a crash leaves a record that is counted lost, never a process nothing knows of. The task's own key is `claude-code`/`task/<id>`; no Work owns it.
- **Ending.** The `tasks` background task (every 15 seconds, and at once when a task this instance started exits; one sweep at a time) reconciles each running record with its files and process:
  - a recorded `exit`: `succeeded` for 0, `failed` otherwise, or `stopped`/`timed_out` when a stop was requested, with the signal named for an exit over 128
  - no live wrapper (a group leader carrying the nonce) and no `exit`: `lost`, or `stopped`/`timed_out` once a requested stop had to kill it
  - past `timeout_minutes` (default 240, at most 1440): SIGTERM to its group, then SIGKILL after the grace period, so a wait on a task always ends
  - its Work finished: stopped the same way

  The end is accepted as a derived `claude-code.task.ended` Event (schema `claude-code.task.v1`: task, name, Work, outcome, exit code, signal, last line, log, start and end), in one transaction with the record's update. The task's key carries it, its delivery is `task/<id>/ended`, and its parent is the trigger of the attempt that started the task. A loop of tasks and wakes with no outside input therefore deepens and stops at `causal_max_depth`. A causal refusal is recorded by the host and the task still ends. A task of finished Work is released (record and files) once announced.
- **Waking.** `task.wait` is a translated wait. It adds one dependency per task: `claude-code.task.ended` on the task's key, aliased as `claude-code.task.finished`, scanning all history, so a task that has already ended wakes the Work at once. The Wait matches only that alias, so other input becomes context for the wake rather than waking it early, and `until` bounds it. The extension renders both Events for the next turn (outcome, exit code, last line, log).
- **Flagging.** `task.wait` records that the segment parked on its tasks. A turn that ends while the Work has tasks running and no such record reports `tasks_unwatched` in its outcome data and in its `agent_turn` telemetry, because nothing will tell the Work when those tasks end.
- **Telemetry.** `task_started`, `task_start_failed` and `task_ended` (with outcome and exit code) go to the executor's log beside `agent_turn`.

The operations' guidance, served only to an execution granted them, tells an agent to run anything that outlives the turn as a task and to end the turn with `task.wait`. The `work-issue`, `work-tracker-issue` and `slack-thread` playbooks name no executor: they say not to leave work running in the background when a turn ends, and to use whatever the toolbelt offers for work that outlives it.

## Playbooks and rendering

The playbooks, waits and GitHub tools themselves are described in [engineering tools, waits and playbooks](../engineering-tools/). The executor's side is:

- **Playbooks.** Bootstrap writes the playbook plugin to `<state>/playbooks/plugin`, the default `Options.PluginDir` for bindings without `plugin_dir`.
- **Project additions.** Read from the turn's workspace (`.open-conduit/playbooks/<playbook>.md`) and appended to the turn body.
- **Rendering.** The host renders each input through the extension that owns its family (`inproc.Host.Render`, `event.render_owned`); GitHub's adds reply hints naming toolbelt calls.
- **Tool calls.** The MCP `call` tool sends its `input` object as payload data, with the `file_path`/`save_to` file conventions. A `call` that names a builtin (`call {"operation": "read_context"}`) is that builtin's own tool call: its `input` is the tool's arguments, and it shows what the tool shows (the context, the event, the Work), never an empty output (#623).

A `call github.wait` parks the attempt exactly like `wait`, because `Complete` applies the declaration.

## Operator controls

[Operator setup](../operator-setup/) covers attach through `POST /work/{id}/attach` under an operator hold, transcripts reachable as run artifacts, `Usage` (opt-in, doctor), `Version` (doctor), `Options.DryRun` (a stub turn on `haiku`) and the `init --with claude` setup command, which starts through the executor's interactive launch (below). The executor also renders each turn's activity as a run artifact, shown by the TUI and `runs log` ([installed integrations](../installed-integrations/#operator-tools)).

Handed-back Work sits `ready` with no pending input, and the host loop skips it quietly.

## Interactive launch

The executor offers an interactive launch (`extension.InteractiveLauncher`), which `init --with claude` (or `--with claude-code`) starts the setup playbook through ([operator setup](../operator-setup/)). It writes the playbook as the one skill of its own plugin, named `open-conduit`, into the files directory the host gives it (`<state>/playbooks/setup`), removing any other skill an earlier launch left there, and answers `claude /open-conduit:<name> --plugin-dir <dir> --allowedTools Bash(<command>:*)…`, one entry for each command the host allows, so nothing else goes without asking. The prompt comes first because `--allowedTools` takes every argument after it. It declares `claude` as its launch name, and uses `claude` as the program unless the request names another.

## Workspace and environment

With a binding resource, a new segment calls `Acquire({Mode: workspace_mode (default "work"; "clone" for a git clone that shares nothing writable with the checkout), Params: {"branch": …} when branch is set})`. The executor reports that mode to configuration validation (`config.ResourceModer`), so a sandboxed binding can be checked against it ([sandboxed bindings](../workspaces/#sandboxed-bindings)). That request always wins over the git resource's own `mode`, which applies only when the executor requests none, so `config check` warns when the resource names a different mode ([modes](../workspaces/#modes-from-the-real-github-needs)). The default stays `work`: the session transcript is tied to the working directory, and a `fresh` folder per execution would break resume across a Work's attempts. A resumed segment calls `Lookup` on the parked segment's lease reference, which never creates anything. claude then runs in `Resource.Dir`. The reference is kept with the segment and reported in the result. A setup failure or an unavailable workspace is a structured failure with no process. Without a resource, the session lives in the binding's `dir`, used as-is and shared by every Work on the binding, or else in a private directory that stays the same for the Work. A `dir` that is gone at turn start is a structured `workspace_unavailable` failure, and a binding with both a resource and `dir` fails its turns as `invalid_config`. An `invalid_config` result, or a workspace the provider refused as `extension.ErrInvalid`, is marked `Permanent` and not retried. The executor never removes workspaces; the host reconciles them (B07).

A binding with a sandbox ([B09](../sandboxes/)) wraps claude through `extension.WrapLaunch`, needing its workspace, its own config directory (`CLAUDE_CONFIG_DIR` in the binding's `env`, apart from the operator's `~/.claude` and `~/.claude.json`, which its needs deny; [B09](../sandboxes/#executors)), its MCP config, the playbooks, its token directory, the host's toolbelt directory (`<state>/toolbelt`, [daemon](../daemon-core-gate/)) and toolbelt socket, and `api.anthropic.com`. Sandboxed, `TMPDIR` and `CLAUDE_CODE_TMPDIR` are the segment's own `tmp` directory, while srt itself keeps `/tmp` for its sockets. A refusal is a structured `sandbox_denied` or `sandbox_unavailable` failure with no process, `Recover` adopts a live segment only once `extension.AdoptSandboxed` proves it is in its sandbox, and an interrupted turn's result says whether it got `term` or `kill`. Claude Code's own `sandbox.*` settings are executor config, not a provider; they can be added inside an srt sandbox with `extra_args` as defence in depth.

A binding with a container ([containers](../sandboxes/#claude-code-in-a-container)) launches through `ExecutionContext.Container` and advertises `Capabilities.Container`. Its claude keeps `HOME` and `CLAUDE_CONFIG_DIR` in the Work's own home (`ExecutionContext.ContainerHome`), seeded from `home_seed` on first use, so a session resumes across segments. Its sign-in, the binding's selected secrets and any variable named as a credential (ending `_TOKEN`, `_API_KEY`, `_SECRET` or `_PASSWORD`) go in the launch's `Secrets`, which the provider keeps out of the container's environment; segments set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`. The executor sets no credential helper for any one host: a run layer's helpers (the GitHub extension's, say) come first, then the toolbelt's `!os git-credential` as git's general `credential.helper` unless the run already sets one. `Recover` adopts through the broker as shell does. Attach for such a session returns `open-conduit container attach <work>`, and `ContainerAttach` gives the interactive `claude --resume` the daemon launches in an attach container ([attach in a container](../sandboxes/#attach-in-a-container)).

The environment follows B05:

1. **Sanitized baseline.** The shell baseline of the executor's own process, plus `CLAUDE_CONFIG_DIR` and the executor config's `pass_env` names from that process. Out of process that environment is sanitized, so the daemon's own variables (a proxy, the daemon's `CLAUDE_CONFIG_DIR`) come from the binding's `pass_env`: the operator names them, the host validates them with the configuration and passes its values as `ExecutionContext.HostEnv`, which the executor adds here. Validating an executor config that sets `pass_env` warns that it reaches only the executor's own process. `ANTHROPIC_API_KEY`, `CLAUDECODE`, `CLAUDE_CODE_*` and other credentials are dropped.
2. **Provider layer.** Any `OS_*` names are dropped from it.
3. **Run layer.** The extensions' run environment (`ExecutionContext.Env`, [extension protocol](../extension-protocol/)), applied with `extension.ApplyEnv`: its `GIT_CONFIG_*` entries go after the provider layer's, and `OS_*` names are dropped. A host run with a run layer and a toolbelt also gets the toolbelt client (`OS_ENDPOINT`, `OS_TOKEN_FILE` and the toolbelt directory first on `PATH`, never `OS_TOKEN`), so a layer can call `os`; a container run always has it. Other host runs keep the toolbelt out of claude's environment, and only the MCP server has it.
4. **Binding `env`.**
5. **Selected secrets.** An API key comes only from a secret mapped with `secret_env`.
6. **Execution identity.**

## Binding config

The config is strict and validated at load. A refusal names each field that is wrong as a [config diagnostic](../extension-protocol/#config-diagnostics), such as `extra_args/1` with `reserved_argument`, `secret_env/<secret>` with `secret_not_selected` for a secret the binding doesn't select, or `unknown_field`, and never echoes the value. Every field is optional:

| Field | Meaning |
| --- | --- |
| `binary` | The Claude Code CLI (default `claude`). |
| `model` | The model (default `opus`). |
| `permission_mode` | Default `bypassPermissions`. |
| `playbook` | The skill turns start on. `-`, or leaving it out, means none: turns start on the rendered Work alone. `init github` writes it. |
| `plugin_dir` | The playbook skills directory. |
| `args` | Default `{number}`. |
| `structured` | Default true. |
| `status_schema` | The outcome data schema (default `core.turn.v1`). |
| `branch`, `workspace_mode` | The workspace request. |
| `dir` | An absolute folder a workspace-free binding's sessions run in, used as-is. A folder that has gone fails only that binding's turns, as `workspace_unavailable`, never the config. In an [included folder](../config-management/#included-folders), `dir` defaults to the folder (unless the binding has a resource), and a relative `dir` or `plugin_dir` resolves against it. |
| `handoff_minutes` | How long after a turn the session counts as cold (default 55). |
| `cold_turns` | What a cold turn does: `handoff` (default) or `resume`. `handoff_share`, the removed experiment's setting, is ignored with a `config check` warning from the executor (`ConfigWarnings`) for one release, then rejected. |
| `handoff_tokens` | How much context a session may end a turn with before the next turn starts a fresh one from the note (default 400000; 0 turns it off). |
| `auto_compact_tokens` | Where Claude Code compacts a session in the middle of a turn, set as `CLAUDE_CODE_AUTO_COMPACT_WINDOW`: it compacts a little before this, or before the model's window if that is smaller (default 600000; 0 leaves Claude Code's own threshold). `env` setting the variable overrides it, and so does `pass_env` naming it when the variable is set. |
| `env`, `pass_env`, `secret_env` | The environment. `pass_env` here admits variables of the executor's own process only; name the daemon's variables in the binding's `pass_env`. |
| `extra_args` | Extra arguments. These may not override `-p`, the session or the MCP flags, nor `--tools` when `builtin_tools` is set. |
| `builtin_tools` | The only built-in tools Claude Code offers, passed as `--tools`. An empty list turns them all off, so the agent has only its MCP toolbelt: no shell, file access or web. Unset keeps Claude Code's default set. Attaching to the session keeps the same limit. |
| `strict_mcp` | `true` passes `--strict-mcp-config`, so the toolbelt is the agent's only MCP server: none of the operator's own, a project's or claude.ai's servers and connectors load. Attaching keeps it. Default `false`. |
| `usage` | `true` lets doctor's `claude-code` check read the subscription's 5-hour and 7-day usage windows with this binding's Claude login (its `CLAUDE_CONFIG_DIR`, else the default), warning at 90%. Off by default, because it reads the login token. |
| `home_seed` | For a binding with a container: an absolute directory (a `settings.json`, plugins) copied into the Work's home on its first use, its `.credentials.json` left out. A copy, never a mount. Use plain files: a symbolic link is copied as a link, and one to a host path dangles inside the container. |
| `models` | Models a Work may pick for its turns through `model` in its context, for example one an operator chose when starting it. A Work that names none, or one not listed, uses `model`. |

## Setup probe

`claude-code.status` is the executor's probe for [setup sessions](../setup-sessions/) (`extension.SetupProbes`). It says whether `claude` is on the daemon's `PATH`, its version, and whether it is signed in and how: `subscription` when a Claude login is found (the macOS Keychain item or `~/.claude/.credentials.json`), `api_key` when `ANTHROPIC_API_KEY` is set in the daemon's environment. It never returns the token or account detail, and it starts no session.

## Verification

- [Unit tests](https://github.com/open-conduit/open-conduit/tree/main/extensions/claudecode) run a fake `claude`, which records its invocation, echoes stream-json replays, writes the transcript and reports cumulative totals. They cover:
  - the shared executor conformance case
  - the invocation, the MCP config and the sanitized environment
  - resumed and fresh sessions with per-turn usage deltas
  - the port of `TestColdSessionHandsOffToAFreshOne`: warm resume, a cold handoff, a cold resume under `cold_turns: resume`, attach forcing resume and release of terminal Work
  - a warm but large session handing off by size, a smaller one resuming and `handoff_tokens: 0` turning it off (`TestLargeSessionHandsOff`), and `auto_compact_tokens` reaching claude's environment (`TestAutoCompactTokens`)
  - the cold decision across executor restart: adopting the running turn and taking over an undone decision
  - continuity left by the handoff experiment loading and releasing without error
  - interjection consumption
  - usage-limit delay and the executor-wide hold
  - failures, interrupt and missing binary
  - detached adoption with credential replacement and lower-epoch and superseded refusals
  - work-workspace acquisition and resume by lookup
  - config validation, and the playbook a binding that names none starts on
  - no code-host or issue terms in the executor's sources
  - the ported parser tests (parse, limits, reset text, call counts, `perTurn`)
  - background tasks (`tasks_test.go`): a task outliving the turn whose wrapper signals its group, with a background child of claude's own as the negative control; one announcement per end, derived from the attempt's trigger; a retried start; stop, timeout and lost; a task stopped and released with its Work; the wait plan; refusals (input, stale epoch, an unknown execution, an ended turn, a sandbox); a task that exits between the sweep's liveness check and its read of the exit record, announced with its exit rather than as lost; a task released while a sweep is looking at it, whose directory that sweep doesn't bring back; and `tasks_unwatched`, with a parked turn as the negative control
- [`TestProductionSchedulerResultRetryHint`](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/scheduler_test.go) checks that a result hint delays the retry and leaves no uncertain hold.
- [`TestDaemonClaudeCodeExecution`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_claudecode_test.go) runs against the composed daemon:
  - a fake claude drives the real `os mcp` (initialize, tools/list, read_context, wait) and parks
  - the wake resumes the same session
  - an event mid-turn is injected and consumed
  - a turn running at a daemon SIGKILL is adopted with one launch
  - no runtime API shows the session
- [`TestDaemonClaudeCodeContinuityKill`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_claudecode_test.go) uses the `kv` fault boundary to SIGKILL the daemon before and after the decision commit. The retry launches once and resumes the Work's session.

## Limitations

- **Tasks need a host segment.** Tasks run only for bindings without a sandbox or container, because the task would outlive the one and run outside the other. A container or sandbox that a task joins is future work.
- **Tasks don't join tracker waits.** A task is waited on with `claude-code.task.wait`, not as a condition of a tracker's wait (`github.wait`'s `pr:`, `issue:` and `job:`), so other news queues as context until a task ends or the wait's `until`.
- **Long Workflows.** An in-process Workflow can't be a task. Running one headless (`claude -p`) as a task is the way to keep it past the turn. Finding a Workflow's journal by run ID from a later session is Claude Code's to provide.

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
- **#428** moved it out of process. It runs as a package bundled with the release (`extensions/claudecode/pkg`, `open-conduit extension-run claude-code`), the executor itself served through `inprocsdk`, with its live output and container attach crossing the wire (`executor.live`, `executor.container_attach`). A run takes the toolbelt and playbook directories the host names for it (`ExecutionContext.ToolbeltDir`, `PlaybookDir`). Its continuity moved from `executor.claudecode` to the package's own `claude-code` namespace; the daemon moved existing records once at start until #497 dropped that step, so continuity an older release left in `executor.claudecode` is no longer carried over. Its doctor check is its own (`claude-code`, formerly `claude_code` in bootstrap), and usage reading moved from `serve --claude-usage` to the binding's `usage`. Certification runs it against a fake `claude` the fixture command writes.
- **#556** added background tasks: the `claude-code.task.*` operations, the `claude-code.task.ended` Event, the task sweep and the `tasks_unwatched` flag.
