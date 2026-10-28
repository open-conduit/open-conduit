---
title: "Setup sessions and config change sets"
description: "Proposal for setup in the browser: a setup extension whose agent talks in typed events and proposes config change sets that only the operator applies."
---

This is a proposal for [#127](https://github.com/open-conduit/open-conduit/issues/127). It describes intended behaviour, not the code as it is. When it is built, this page is rewritten to describe what was built, and every page named under [Rules this changes](#rules-this-changes) is amended in the same change.

`open-conduit init --with claude` runs the [setup playbook](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/playbooks/setup/setup.md) as an interactive Claude Code session in a terminal ([operator setup](../operator-setup/)). There the agent edits the config file itself with `open-conduit config set`. This proposal moves the same setup into the web view:

- The agent's output arrives as typed events the page renders: a message, a probe and its result, a question with choices, a proposed change, checks, and an applied generation. There is no terminal text to parse.
- The agent never writes the config. It proposes changes, the operator approves or rejects each one, and only the operator applies the approved set.

## Shape

Setup is an in-process extension, `setup`, on `extension/inproc`, built the way `human` and `introspection` are. It lives in `extensions/setup`. A setup scope is ordinary config:

```json
{
  "id": "setup",
  "name": "Setup",
  "authorization": {"connections": [], "read_scopes": [], "operations": [
    "read_context", "wait", "declare_outcome",
    "setup.say", "setup.ask", "setup.probe", "setup.read", "setup.propose", "setup.withdraw", "setup.validate", "setup.trace"
  ]},
  "extensions": [{"name": "setup", "type": "setup", "secret_names": [], "config": {}}],
  "routes": [{"name": "setup", "handler": "setup", "binding": "agent", "active_policy": "inject",
    "unsupported_fallback": "queue", "error_fallback": "queue", "config": {}}],
  "bindings": [{"name": "agent", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
    "operations": ["read_context", "wait", "declare_outcome",
      "setup.say", "setup.ask", "setup.probe", "setup.read", "setup.propose", "setup.withdraw", "setup.validate", "setup.trace"],
    "secret_names": [], "config": {"playbook": "setup", "args": "session", "builtin_tools": [], "models": ["opus", "sonnet"]}}]
}
```

The `setup` type, handler and operations don't exist yet, so this block is a sketch, not a validated fixture.

- **Work.** A session is Work of kind `setup.session` in the setup scope, on the existing [Claude Code executor](../claude-code-executor/). Work gives the session everything it needs as it is: detached segments that survive a daemon restart, `--resume` continuity, waits that park it until the operator answers, an authorised toolbelt over MCP, and run history. The attempt pins its generation and binding like any other ([execution scheduling](../execution-scheduling/)).
- **Route.** The `setup` route handler matches only Events the extension's own actions produce (`setup.*`, source `core.action`). A `setup.start` with an unowned correlation key creates the session's Work. Every later operator Event is addressed to that Work. During a running turn the route injects and falls back to queue. While the session waits, the Event wakes it, or it is queued until the next turn.
- **No shell.** The binding turns Claude Code's built-in tools off. That is a new executor option, `builtin_tools`: a list passed as `--tools`, where an empty list means none. Today the executor has only `permission_mode` and `extra_args` ([options](../claude-code-executor/)). The extension's config check refuses a setup scope whose bound agent has built-in tools on, or whose operations reach beyond `read_context`, `wait`, `declare_outcome` and `setup.*`. Everything the agent learns comes through a probe, and everything it says goes through an operation.
- **Model.** The operator picks the model when starting a session. The `start` action offers the binding's `models` as choices, and the choice goes in the Work's context. That is a second new executor option: a binding with `models` takes the Work context's `model` if it is one of them, and otherwise uses the binding's `model`. The model is never written to the config.
- **One session at a time.** The `start` action is listed only while the scope has no open session.
- **Playbook.** The setup playbook moves from `extensions/github/playbooks/setup` to `extensions/setup`, because it already covers Slack, Linear and Jira as well as GitHub. `init --with claude` and the setup binding both use it from there (see [the playbook](#the-setup-playbook-under-a-session)).

### Why not a reserved scope

An earlier draft put the session in a `core.setup` scope that no config file contains, with its binding fixed in code. That broke more written rules than it was worth:

- B05 requires at least one scope and no implicit one, constrains scope IDs, and keeps a stored generation self-contained.
- Execution scheduling pins every attempt to a binding in its generation.
- B04 needs a route before an Event can reach Work.
- B01 keeps domain behaviour out of management.

The extension needs none of those exceptions. Everything the agent may do sits in a file the operator can read. The one thing the reserved scope bought was that config couldn't widen the agent. Here the extension's config check enforces the same limits, and only the operator can apply a config that would change them.

## A fresh install

`open-conduit web` stays a client: it writes no config and starts no daemon. The daemon gets a **default configuration** instead:

- **No config file and no stored generation.** `serve` builds the default document: the `setup` extension's `extension.Setup` result with no flags (the scope above), merged into an empty config by the same host that `init` uses. It validates that document and publishes it as generation 1. The file is not written. Generation 1 is stored and self-contained like any other, so the validator still requires at least one scope, and no generation 0 is needed. The first apply writes the file. `open-conduit init setup` writes the same scope to a file for an operator who wants it there first.
- **What still refuses to start.** A config file that exists but doesn't validate, with no stored generation, still stops `serve` with its diagnostics. Falling back to the default would hide the operator's mistake.
- **What the views show.** `doctor`'s `config` check reports `default_config` (info) while the daemon runs on it. The web view shows "running, nothing configured but setup" and offers to start a session. With no daemon, the web view says so and gives the commands to start one (`open-conduit serve`, or `open-conduit service install`). It runs neither.

## Typed events

Every event is an operation or an action. The extension accepts each one as an Event of its own family on the session's Work, through `AcceptInTx`. The page renders the Work's Events in acceptance order. Claude Code's transcript stays in the run's artifacts, as for any turn.

| Event | From | Data |
| --- | --- | --- |
| `setup.start` | operator `start` action (scope level) | `model` |
| `setup.message` | `setup.say` (agent), operator `message` action | `role`, `text` |
| `setup.probe` | `setup.probe`, `setup.read`, `setup.trace` | `probe`, `args`, `command` (for display), `why`, `result` |
| `setup.question` | `setup.ask` | `question_id`, `text`, `context`, `choices` |
| `setup.answer` | operator `answer` action | `question_id`, `answer` |
| `setup.proposal` | `setup.propose` | `change`, `op`, `pointer`, `before`, `after`, `why`, `diagnostics` |
| `setup.decision` | operator `approve_*`, `reject_*`, `approve_all` actions; `setup.withdraw` | `change`, `decision`, `reason` |
| `setup.checks` | `setup.validate`, operator `doctor` action | `source`, `candidate_hash`, `diagnostics` or `checks` |
| `setup.applied` | the extension, on the generation that applies the set | `generation`, `hash`, `changes` |

- **Questions** work like `human.ask`. `setup.ask` is an `Operation.Wait`: it records the question, parks the session, and the operator's `answer` action (one button per choice, or a text box) wakes it. Every ask has a 24-hour wait timeout. When it expires, the agent sums up where it got to and finishes.
- **Messages** from the operator are the `message` action on the session's Work. The route injects a message into a running turn. Inject success means accepted for delivery, not read. The agent has read the message only once Claude Code echoes it back ([injection](../claude-code-executor/#prompt-inputs-and-injection)). A segment adopted after a daemon restart takes no injected input, so its messages are presented at the next turn or wake instead. Either way, none is lost.
- **Ending.** A session ends as any Work does. The agent's structured status `finished` (or `declare_outcome`) completes it, once the config is applied and checked or the operator says they're done. The operator's End session is the extension's `CancelWork` on the session, after which late Events are checkpointed `terminal` as B04 says. The page shows that the Work ended, from its state. No `setup.ended` Event has to race the cancellation. An open change set is abandoned when its session ends.
- **Reloading.** The page holds no state. It reads the Work's Events (`GET /v1/events?work_id=…&after_sequence=…`) and its panels, and polls with `after_sequence`. A running turn's text comes from `GET /v1/runs/{id}/live`. A reload, a closed tab, or a restart of `open-conduit web` loses only an unsent draft.

## What the agent may read and run

The setup playbook's ground rules say the agent probes and never handles a secret. Here the toolbelt enforces them. An execution token reaches only `/v1/toolbelt/call` (B05), so every probe is an operation of the setup extension. The extension runs each probe under the session's own authority, never as a proxy for an operator read.

**The candidate, and nothing else of the config.** The agent can read three things, all redacted the way `config get` is (secret references become their names with `[redacted]`):

- `setup.read` returns the outline of the base config: connection IDs and types, scope IDs and names, entry types, binding names. This is the same wiring `GET /config` shows, with no opaque config and no references.
- `setup.read` also returns, in full, the scopes the operator named when they started the session, and the scopes the session's change set touches.
- `setup.read` also returns the candidate, which is the base plus the approved changes.

To work on an existing scope, the operator names it at the start, or approves a change to it.

**Named probes, owned by what they're about.** `setup.probe` takes a probe name and arguments, never a command line. The probes come from `extension.SetupProbes`, a new optional interface beside `extension.Setup`, so each one lives with the code that knows its domain. Each probe declares its arguments, runs without a shell, has a short timeout and an output cap, and returns safe output plus a display command. Adding one is a code change with a test that its output carries no secret.

| Owner | Probe | Returns |
| --- | --- | --- |
| `setup` | `fs.list` | a directory's entries under the operator's home, names and kinds only, two levels at most. Hidden entries are skipped, and the result says how many. A directory that is a git checkout lists its remotes' URLs with any user-info stripped (an `https://token@…` remote shows the host only) |
| `setup` | `tools` | whether each of `gh`, `git`, `ngrok` and `cloudflared` is on the daemon's `PATH`, with its path and whether it's a symlink or a script |
| `setup` | `init` | `init TYPE --dry-run` for every registered setup (`github`, `slack`, `linear`, `jira`). It calls the type's `extension.Setup` with a non-interactive host: `Prompt` returns `ErrSetupNoInput`, `StoreSecret` stores nothing and returns the reference it would use, and `Run` and `OpenURL` refuse. Flags take references as `init` does, and refuse literals |
| `setup` | `doctor` | the daemon checks that need no secret (`config`, `webhooks`, `service`, the executors'). This is a new narrow host function over the same checks as `GET /v1/doctor`, without `check_secrets` |
| `claude-code` | `status` | whether `claude` is on the daemon's `PATH`, its version, and whether it is signed in and how (subscription login or API key), with no account detail or key. The `claude_code` doctor check gains the same sign-in state |
| `github` | `accounts` | `gh auth status --json hosts`: each account's host, login, active flag, token source and state |
| `github` | `repos` | `gh repo list OWNER --json nameWithOwner` |

The playbook starts with `claude-code.status`, so its first words to the operator are that Claude Code is installed and signed in, so it can do the work, or what to fix first.

**No probe takes a reference or a command.** There is nothing that reads an arbitrary file, prints the environment, runs a `cmd:` reference, or resolves a secret. The earlier draft's `gh.login`, which resolved `gh auth token --user LOGIN` with a login the agent chose, is dropped: B05 lets no request supply a command or reference to resolve. Proving a credential works is `doctor` with `check_secrets`, which stays an operator action. The page offers it as a `doctor` action once a change set is applied, and records the result as `setup.checks`. That result is checks and codes, as `GET /v1/doctor` returns, so the agent can read it.

**Tracing the candidate.** `setup.trace` runs the route trace from [#126](https://github.com/open-conduit/open-conduit/issues/126) with one of a scope's named samples, against the session's candidate only. It never traces other scopes, and it writes nothing, like the trace itself. This is the last step of setup: it proves that a synthetic event, such as "a new issue is opened", would be picked up, without the operator labelling a real issue. It needs the trace to take a candidate snapshot as well as the active generation. That is an internal function, not a new route.

## Secrets and references

- **No values.** No probe reads or resolves a secret (above). `setup.propose` applies `config set`'s rule: a value under `secret_refs`, or in any declared secret slot, must be a reference, and anything else is refused `not_a_reference`.
- **No reference text in Events.** A reference is sensitive in views, errors and telemetry (B05). An Event for a pointer inside `secret_refs` stores `before` and `after` as the names with `[redacted]`. So does everything the agent reads back. The reference text the agent proposed is kept only in the change set, in the extension's KV namespace.
- **The operator sees what they approve.** The proposal panel on the session's Work shows the full reference. `GET /v1/work/{id}/panels` is an operator-class read, so the reference reaches the operator and not the execution. This is the one view that shows reference text, and it shows only references proposed in this change set.
- **A last check.** Before accepting any Event, the extension scans its strings for the token shapes Open Conduit knows (GitHub, Slack, Linear, Anthropic, private-key blocks). A match in probe output replaces the output with `redacted: true`. A match in text the agent wrote refuses the call with `looks_like_secret`. This is a backstop for mistakes, not the guard: the structural rules above are the guard.

Credentials still go in from the operator's own terminal. When a connection needs one, the agent asks the operator to run `open-conduit secret set NAME`, asks whether it's done, and proposes the reference.

## Change sets

A change set is the session's list of JSON-pointer edits to the config file, each with a reason, waiting for the operator. It lives in the setup extension's KV namespace (`setup.changesets`), keyed by session. There is no core table.

- **Base.** A change set is based on the committed generation and the config file's text when the session starts. A change edits the file text with `config set`'s editor, which addresses array elements by `id` or `name` and appends with `-`. A pointer into a scope from an [included folder](../config-management/#included-folders) is refused `in_folder`, because that text lives in another file. Running on the default configuration with no file, the base text is the default document.
- **Proposing.** `setup.propose {op, pointer, value?, why}` adds a `proposed` change. The extension fills in `before`, checks the pointer resolves for `replace` and `remove`, and validates the base plus the approved changes plus this one, attaching the diagnostics. A proposal that doesn't validate on its own is still recorded, since a scope and its binding may come in two proposals, and the panel shows its diagnostics. `setup.withdraw` takes back an undecided change.
- **Deciding.** The session's panel lists each open proposal with `approve_<change>` and `reject_<change>` actions. Reject prompts for an optional reason. With more than one open, the panel also offers `approve_all`, labelled "Approve all N". Its ID carries no list. Instead, `Invoke` checks the change set's revision against the one it listed, so it approves exactly the proposals the operator saw and refuses `change_set_moved` if any changed. Each decision is still recorded as its own `setup.decision`. A rejected change stays in the list, so the agent doesn't propose it again.
- **Validating.** `setup.validate` (agent) and the operator's `validate` action build the candidate (the base text plus the approved changes, in proposal order) and validate it. A new host function runs the same side-effect-free validation as `POST /v1/config/validate`. The result and its `candidate_hash` are recorded as `setup.checks`.
- **Applying.** Apply is not an action and not an operation. The web view's Apply button is an operator client doing what `config apply` does:
  1. It reads the candidate from a new operator-class route, `GET /v1/work/{id}/config-candidate`, served by an optional `extension.ConfigCandidate` interface on the Work's extension. The route returns the text, `base_generation` and `candidate_hash`.
  2. It calls the existing `POST /v1/config/apply` with that `expected_generation` and `candidate_hash`.

  The extension's `Configurer` receives every applied generation. When one's hash matches the candidate, it marks those changes `applied`, accepts `setup.applied` (which wakes the agent), and moves the change set's base to the new generation.
- **Key order.** `POST /config/apply` saves what it is given in compact form (B05). To keep the operator's layout, apply gains an optional `config_text`, the file text as a JSON string, used instead of `config`. The daemon validates the parsed text and saves the text byte for byte. `open-conduit config apply FILE` can send the same.
- **When the config moved.** If the generation or the file changed after the set's base, apply fails with `generation_conflict` and the panel marks the set stale. The operator's `rebase` action re-reads both, recomputes each change's `before`, keeps approvals whose `before` is unchanged, and returns the rest to `proposed`.

### Only the operator applies

The agent can propose, withdraw and validate. It cannot apply:

- No operation applies config, and the setup scope's config check refuses a binding that grants anything beyond `setup.*` and the builtins.
- `POST /v1/config/apply` and `GET /v1/work/{id}/config-candidate` need the operator credential. The agent's execution token reaches only `/v1/toolbelt/call` (B05).
- Actions are invoked through the management API with the operator credential, so the agent can't approve its own proposals either.
- The agent has no shell, so it can't run `open-conduit config apply` or read the operator token file, as the terminal setup could.

The acceptance test runs a session's execution token against all of them: every `setup.*` operation with apply-shaped input, `POST /v1/config/apply`, the candidate route, and the action routes. Each must be refused, and the generation must be unchanged afterwards.

## The setup playbook under a session

There is one playbook, rendered from a template as `{{bin}}` already is. Session mode is on when the args are `session`. The introduction, the ground rules, the questions to settle and the config examples stay shared. What changes:

| In the terminal | In a session |
| --- | --- |
| checks `command -v claude` along the way | starts with `claude-code.status`, and tells the operator first whether Claude Code can do the work |
| reads with `ls`, `git`, `gh`, `config show`, `doctor` | `setup.probe` and `setup.read` |
| talks and asks in the chat | `setup.say`, and `setup.ask` with the likely answers as choices |
| `config set`, `init TYPE` | the `init` probe to build a scope, then `setup.propose` for each change, with its reason |
| `config validate` and `config apply` | `setup.validate`, then asks the operator to review and apply in the page; `setup.applied` wakes it |
| asks the owner to run `secret set` | the same, then `setup.ask` whether it's done |
| suggests labelling a real issue to see it work | `setup.trace` with a sample, then asks the operator to run `doctor` with secrets checked |
| ends when the owner closes the terminal | ends with the structured status `finished` and a summary |

Session mode tells the agent it has no shell, and that a proposal is a request, not an edit: nothing has changed until `setup.applied` arrives. It proposes in small, explained steps (a connection, then a scope, then its binding), so each can be judged on its own.

## Rules this changes

The build amends each of these pages in the same change:

- **[B05 configuration](../config-management/)** and **[toolbelt and management](../toolbelt-management/):** "The first start activates the config file as generation 1" gains the default configuration when there is no file and no stored generation. `POST /config/apply` gains `config_text`, saved as given. New: `GET /v1/work/{id}/config-candidate`. The change-set panel is the one view that shows reference text, and only to the operator.
- **[Claude Code executor](../claude-code-executor/):** new `builtin_tools` and `models` options, and the `claude-code.status` probe.
- **[Extensions](../extensions/):** new optional `extension.SetupProbes` and `extension.ConfigCandidate`, and in-process host functions for validation, narrow doctor checks and setup registrations.
- **[Operator setup](../operator-setup/):** `init setup`, the playbook's new home and its session mode, the `default_config` doctor code, and the `claude_code` check's sign-in state.
- **[Observability](../observability-history/):** the web view's Setup page.
- **[#126 route trace](https://github.com/open-conduit/open-conduit/issues/126):** tracing against a candidate snapshot.

## Building it

Each step lands with its tests and the page amendments above:

1. The `setup` extension with its scope, `setup.start`, `setup.say` and `setup.ask`, messages, the route, and the executor's `builtin_tools` and `models`. This step also writes the default configuration at first start and moves the playbook.
2. Change sets: propose, withdraw, decide (including `approve_all`), validate, the candidate route, `config_text`, and the applied hook. The operator-only test lands here.
3. Probes: `extension.SetupProbes`, the setup, `claude-code` and `github` probes, `setup.read`, and the secret scanner. Then `setup.trace` once #126 has merged.
4. The playbook's session mode.
5. The web view's Setup page, built from the session's Events and panels.

## Decided in review

- **Daemon-owned session, typed events as toolbelt calls, no shell, named probes, operator-only apply:** approved on #127.
- **Model:** chosen when the session starts and stored on its Work, not in config.
- **Fresh install:** the daemon starts without a file, and `open-conduit web` stays a client.
- **Approve all:** one action per click. Each decision is still its own Event.
- **Extension over reserved scope:** chosen after review, for the reasons [above](#why-not-a-reserved-scope).
