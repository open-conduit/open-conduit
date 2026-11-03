---
title: "Codex executor"
description: "How the Codex CLI executor runs agent turns as detached segments on a Work's thread, takes injected input as follow-up turns, and gives the agent its toolbelt over MCP, all as a bundled package."
---

The Codex executor runs each agent turn as a detached `codex exec --json` segment on the Work's Codex thread, resumes that thread on the Work's next turn, takes Events injected mid-turn as follow-up turns on the same thread, and gives the agent its toolbelt over MCP. The executor is [`extensions/codex`](https://github.com/open-conduit/open-conduit/tree/main/extensions/codex). It reaches the daemon only as a package, [`extensions/codex/pkg`](https://github.com/open-conduit/open-conduit/tree/main/extensions/codex/pkg), served over [`protocol/sdk`](../extension-packaging/) like the shell package. Nothing in the daemon knows about Codex: a binding names executor `codex`, and the host binds it from the capabilities in the package's manifest. The release archive bundles the package as `extensions/codex/` beside the binaries ([install](../../start/install/)). The executor imports only public contracts. Its process identity helpers are copied from `extensions/shell`, because B01 forbids one executor importing another.

It advertises `interrupt`, `inject`, `resume`, `workspace` and `toolbelt: mcp`, the stop, forget and interactive launch controls, and `resource_mode`. It does not advertise `sandbox`, `container` or attach (see [limitations](#limitations)).

## Why Codex, and how its capabilities map

The issue asked for the agent CLI that supports run, resume and inject best. Codex's non-interactive `codex exec` has run (`--json` events, `--output-schema` for a structured last message), resume by thread ID (`codex exec resume <thread>`) and interrupt (it stops on SIGTERM). It has no way to take a message into a turn that is already running: its experimental app server has a `turn/steer` request, but that protocol is marked experimental and changes between releases. So the executor builds inject from what is stable, as the shell executor builds it from an inbox:

| Capability | How the Codex executor provides it |
| --- | --- |
| run | A segment runs `codex exec` on the turn's prompt. |
| resume | The Work's thread ID is private continuity; a later attempt, or a wake that resumes a parked attempt, runs `codex exec resume <thread>`. |
| inject | `Inject` writes the Event to the segment's inbox. When the running turn ends, the segment runs a follow-up `codex exec resume` turn with what arrived, then acknowledges it. |
| interrupt | SIGTERM to the segment's process group, SIGKILL after the grace period, as for the shell executor. |

## A segment

A segment is a detached process group like the shell executor's:

- **Recovery intent first.** It persists `open-conduit.codex.recovery.v1` (naming the segment directory) through `PersistRecovery` before it touches anything else.
- **Process group.** A `/bin/sh` wrapper runs as a new session and process-group leader. Its `$0` carries an unguessable nonce, so identity is checked beyond the PID. The wrapper records its PID and exit status, and signals the remaining group members (including the MCP server) when the driver exits.
- **The driver.** The wrapper runs the extension again as `open-conduit extension-run codex segment <run dir>` (`codex.RunSegment`; the command comes from `sdk.Self`, so a build of the package as its own binary runs `<binary> segment <run dir>`). The driver reads the turn's prompt and options from the run directory and runs one `codex exec` turn:

```
codex exec [resume] --skip-git-repo-check -c sandbox_mode="S" -c approval_policy="never"
  [--model M] [-c mcp_servers.open-conduit.command="<toolbelt dir>/os"
  -c mcp_servers.open-conduit.args=["mcp"] -c mcp_servers.open-conduit.env={OS_ENDPOINT=…,OS_TOKEN_FILE=…}]
  [extra_args…] --json --output-last-message <run>/last.json [--output-schema <run>/status-schema.json]
  [<thread>] -
```

The prompt goes on stdin. Events go to `events.jsonl` and stderr to `err.log`. When the turn ends with status 0 and the inbox holds Events not yet acknowledged, the driver runs a follow-up turn on the same thread whose message is `## Arrived during this turn` and those Events, then acknowledges them (`inbox/consumed/<digest>`). It repeats until nothing is pending (at most 20 follow-ups), and exits with the last turn's status.

`Recover` follows the shell executor's rules. It adopts a verified live leader at a higher epoch, replacing the token file first. It returns a recorded result identically. It refuses lower epochs, tampered metadata and a dead process with no recorded exit or stop (`ErrNotAdoptable`), and it never launches. The inbox is on disk, so an adopted segment still takes injected input. `Stop`, `Interrupt` and `Forget` behave as they do for the shell executor.

## Prompt, inputs and injection

Codex has no plugin skills to start a turn on, so the playbook's text starts the prompt. The executor reads `<plugin dir>/skills/<playbook>/SKILL.md` without its frontmatter and follows it with `ARGUMENTS: <args>` and the body, where a skill invocation would put them. The plugin dir is the binding's `plugin_dir`, or else the host's own playbook directory (`ExecutionContext.PlaybookDir`, [execution endpoints](../toolbelt-management/#execution-endpoints)). The arguments and body are the same as Claude Code's, so a playbook reads the same message whichever agent runs it:

| Turn | Arguments | Body |
| --- | --- | --- |
| A Work's first (no thread yet) | `<args>` | `## Already waiting` + the rendered inputs |
| Any later turn | `<args> events` | `## Since your last turn` + inputs |
| A later turn on a new thread, after a large or cold one | `<args> handoff` | `## Handoff from your last session` + note + `## Since your last turn` + inputs |

`args` and `branch` are the same templates (`{key}`, `{number}`, `{work}`). A project's additions in `.open-conduit/playbooks/<playbook>.md` in the turn's workspace are appended. Inputs are rendered by the generic renderer, which shows each Event's type and its common payload fields (author, kind, number, path, line, state, title, URL, body or text).

Injection follows the [generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement):

- `Inject` writes the Event to `inbox/<order>-<digest>.json` under the segment's finalize lock. A retried dispatch of the same Event is not written twice.
- The proof is the driver's acknowledgement, written once the follow-up turn that carried the Event has run. The result reports acknowledged Events as `Result.ConsumedInputs`, so core consumes them with the turn and starts no further attempt for them. The data reports `injected: {accepted, consumed}`.
- An Event accepted after the driver's last look at the inbox has no proof. It stays pending and is presented to the next attempt, as the contract requires.
- After the result is recorded, or for an execution this instance does not hold, `Inject` returns `extension.ErrNotAccepting`.

The executor passes `conformance.CheckInjectExecutor`.

## Outcomes

A structured turn (the default) passes `--output-schema` with the status schema: `state`, `summary`, `handoff` and `report`. Codex checks the schema strictly, so all four are required, and `handoff` and `report` may be empty. The states and their descriptions are Claude Code's: `finished` completes the Work, while `building`, `review`, `help` and `done` hand it back for the next accepted input. The status of the segment's last turn is the outcome:

| Turn ended | Outcome |
| --- | --- |
| `finished` | completed |
| `building`, `review`, `help`, `done` | handoff |
| The agent called `wait` through MCP | waiting (core converts the result and keeps its data); the wake resumes the thread as a new segment |
| A status that isn't one of these, or none | failed (`no_status`) |
| `turn.failed`, an error or a non-zero exit | failed (`error`) |
| An error naming a usage, rate or quota limit | failed with `Result.RetryHint` and no reset time (`usage_limit`) |
| Interrupt or timeout | failed (`interrupted`, `timeout`) |

A turn with a status records its data under `core.turn.v1`, which every projector reads and from which the host publishes the turn-ended Event. Any other result uses `open-conduit.codex.result.v1`. The fields are:

- `status`, `reason` and `kind` (`start` or `events`)
- `report`, at most 20,000 bytes
- `turns` and `tool_calls` (command, MCP, file-change and web-search items)
- `tokens` (input, output and cached input, summed over the segment's turns), which also map to `extension.Usage`
- `limited`, `exit_code`, `signal` and `injected`
- the `transcript` (`application/x-ndjson`, codex's events) and `log` (stderr) artifact IDs
- `workspace`

An unstructured binding (`structured: false`) completes with codex's last agent message as its summary.

## Private continuity

A Work's thread lives in the executor's state directory (`work/<key>.json`, holding the thread ID, the time of its last turn, the last handoff note and the context the last turn ended with). Core never reads it. A segment records the thread when it finishes, from the `thread.started` event of its first turn, or the thread it resumed. The handoff note is the last status's `handoff`; a turn without a status keeps the earlier note.

The context is the `total_tokens` of the last `token_count` event's `last_token_usage` in the session file codex keeps for the thread (`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<time>-<thread>.jsonl`, `CODEX_HOME` defaulting to `~/.codex` in codex's environment). That file is codex's own, not an interface, so a missing or unreadable one counts as 0. The segment's result data carries it as `context_tokens`.

A thread whose last turn ended with more than `handoff_tokens` of context (default 200000) is left: the next turn starts a new thread from the note, with `handoff_reason: size` in its result data. Starting over costs one cache fill of the small new context, which soon costs less than reading the large one on every call. A thread is *cold* when its last turn ended more than `handoff_minutes` ago (default 55), as a Claude Code session is; a cold thread follows `cold_turns`: `handoff` (the default) starts a new thread from the note, with `handoff_reason: cold`, and `resume` resumes it. Either handoff needs a note: without one the thread resumes. Otherwise every later turn resumes the thread. `Forget` removes an execution's segments and keeps the Work's thread.

## Toolbelt over MCP

When the attempt has a toolbelt token and the host names its toolbelt directory (`ExecutionContext.ToolbeltDir`), every turn's `-c mcp_servers.open-conduit.*` overrides define the stdio server `open-conduit`: `<toolbelt dir>/os mcp`, with `OS_ENDPOINT` and `OS_TOKEN_FILE`. That file is the host-kept `ToolbeltTokenFile`, or else a private 0600 file in the segment directory. The token itself is never in codex's arguments or environment. Adoption replaces the token file, and `os mcp` re-reads it on every call. The tools are those `os mcp` serves to Claude Code ([toolbelt over MCP](../claude-code-executor/#toolbelt-over-mcp)), and Codex names them the same way (`mcp__open-conduit__call`), so the playbooks need no change.

## Workspace and environment

With a binding resource, a segment calls `Acquire({Mode: workspace_mode (default "work"), Params: {"branch": …} when branch is set})`, and reports that mode to configuration validation (`resource_mode`). A setup failure or an unavailable workspace is a structured failure with no process. Without a resource the turn runs in the binding's `dir` (which must exist) or in a private folder that stays the same for the Work. A binding with both a resource and `dir` fails as `invalid_config`. An `invalid_config` result, or a workspace the provider refused as `extension.ErrInvalid`, is marked `Permanent` and not retried.

The environment follows B05:

1. **Sanitized baseline.** `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `TERM` and `CODEX_HOME` of the executor's own process, plus the executor config's `pass_env` names from that process. Out of process that environment is sanitized, so the daemon's own variables (a proxy, the daemon's `CODEX_HOME`) come from the binding's `pass_env`: the operator names them, the host validates them with the configuration and passes its values as `ExecutionContext.HostEnv`, which the executor adds here, never as an `OS_*` name. Validating an executor config that sets `pass_env` warns that it reaches only the executor's own process. `OPENAI_API_KEY` and other credentials are dropped.
2. **Provider layer.** `OS_*` names are dropped.
3. **Run layer** (`ExecutionContext.Env`), with `extension.ApplyEnv`. When there is one and a toolbelt, codex also gets the toolbelt client (`OS_ENDPOINT`, `OS_TOKEN_FILE` and the toolbelt directory first on `PATH`, never `OS_TOKEN`), so the layer can call `os`.
4. **Binding `env`.**
5. **Selected secrets.** An API key comes only from a secret mapped with `secret_env` (for example `OPENAI_API_KEY`). A Codex login lives in `CODEX_HOME` (default `~/.codex`).
6. **Execution identity** (`OS_SCOPE_ID`, `OS_WORK_ID`, `OS_EXECUTION_ID`, `OS_ATTEMPT`).

## Binding config

The config is strict and validated at load. Every field is optional:

| Field | Meaning |
| --- | --- |
| `binary` | The Codex CLI (default `codex`). |
| `model` | Passed as `--model`; unset keeps Codex's own default. |
| `sandbox` | Codex's `sandbox_mode` for the commands the agent runs: `read-only`, `workspace-write` or `danger-full-access` (the default, like Claude Code's `bypassPermissions`, since the agent pushes and calls the network). Approvals are always `never`: nobody is there to answer them. |
| `playbook` | The playbook turns start on. `-` or unset means none. |
| `plugin_dir` | Where the playbooks are (`skills/<name>/SKILL.md`). Default: the host's. |
| `args` | Default `{number}`. |
| `structured` | Default true. |
| `branch`, `workspace_mode` | The workspace request. |
| `dir` | An absolute folder a workspace-free binding's turns run in. |
| `env`, `pass_env`, `secret_env` | The environment. `pass_env` here admits variables of the executor's own process only, with a `config check` warning (`ConfigWarnings`); name the daemon's variables in the binding's `pass_env`. |
| `extra_args` | Added to every `codex exec`. They may not set the session, the output, the sandbox, the approval policy, the model or the toolbelt server, whether as flags (`--full-auto` and `--dangerously-bypass-approvals-and-sandbox` included) or as `-c`/`--config` overrides of `sandbox_mode`, `approval_policy`, `model` or `mcp_servers.open-conduit`. |
| `timeout_seconds` | A segment's deadline, persisted across restart. |
| `handoff_minutes` | How long after a turn the thread counts as cold (default 55). |
| `cold_turns` | What a cold turn does: `handoff` (default) starts a new thread from the note, `resume` resumes the thread. |
| `handoff_tokens` | How much context a thread may end a turn with before the next turn starts a new thread from the handoff note (default 200000; 0 always resumes). |
| `auto_compact_tokens` | Where codex compacts a thread in the middle of a turn, passed as `-c model_auto_compact_token_limit=<n>` (default 250000, under the context windows of Codex's models; 0 leaves codex's own). A `-c` for the same key in `extra_args` comes later and wins. |

## Setup

`open-conduit init --with codex` starts Codex on the setup guide through the executor's interactive launch: `codex --sandbox workspace-write --ask-for-approval on-request <guide>`, the guide being the playbook without its skill frontmatter as the first message. Codex loads no plugin skills and has no list of commands to allow ahead of time, so it writes no files and the host's allowed commands still ask: Codex's own sandbox asks the owner before any command that writes outside the working directory, the config included. The launch needs none of the executor's state, so it answers before anything has run. `init github --executor codex` writes the agent binding on Codex (both in [operator setup](../operator-setup/)). The daemon's service install looks for `claude` or `codex` on `PATH` and warns only when neither is there.

## Verification

- [Unit tests](https://github.com/open-conduit/open-conduit/tree/main/extensions/codex) run a fake `codex` script, with the test binary as the segment driver. They cover:
  - the shared executor and inject conformance cases (the injected Event is taken in by a resume turn, and no further turn runs)
  - the first turn on the playbook text, its arguments and inputs, and a later turn resuming the thread on the events since
  - the host's playbook directory when the binding names none
  - per-segment usage
  - the MCP overrides, with no token in codex's arguments or environment
  - failures, a usage limit, a missing binary, a missing status and an unstructured turn
  - adoption by a second instance without a second launch, interrupt and lower-epoch refusal
  - config validation and the resource mode
  - a cold thread left for a new one from the note, a warm one resuming and `cold_turns: resume` resuming a cold one (`TestColdThreadHandsOff`)
  - a large thread left for a new one from the note, a smaller one resuming, `handoff_tokens: 0` always resuming, and `auto_compact_tokens` in codex's arguments (`TestLargeThreadHandsOff`)
- `open-conduit extension certify` certifies the package (`TestCertificationFirstPartyPackages`). Its fixture command writes a fake codex into the runner's directory, so certification never runs the real CLI.
- [`TestDaemonCodexExecution`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_codex_test.go) runs the composed daemon with the package installed and `extensiontest.FakeCodex`. The fake reads its context through the real `os mcp`. An Event submitted mid-turn is taken in by a follow-up turn and starts no attempt, and the next Event resumes the thread as attempt 2.

## Limitations

- **Rendering.** Inputs use the generic renderer. Claude Code asks the host to render each input through the extension that owns it (`event.render_owned`), so it gets the GitHub and Slack renderers with their reply hints; the Codex executor doesn't ask yet.
- **No attach.** There is no `codex resume` takeover through `open-conduit work attach`.
- **No process sandbox or container.** The package advertises neither, though the extension protocol carries both host brokers (`sandbox.plan`, `container.launch`) as Claude Code uses them. Codex's own `sandbox_mode` is the binding's `sandbox`.
- **Limits.** A usage limit is retried on core's backoff, because Codex's error gives no reset time the executor can read.
