---
title: "Setup sessions and config change sets"
description: "Setup in the browser: a setup extension whose agent talks in typed events and proposes config change sets that only the operator applies."
---

This page describes setup in the browser as built for [#127](https://github.com/open-conduit/open-conduit/issues/127) and [#153](https://github.com/open-conduit/open-conduit/issues/153), from the design approved in #132. Where the build differs from that design, the page says so.

`open-conduit init --with claude` (or `--with codex`) runs the [setup playbook](https://github.com/open-conduit/extension-setup/blob/main/playbooks/setup.md) as an interactive Claude Code session in a terminal ([operator setup](../operator-setup/)). There the agent edits the config file itself with `open-conduit config set`. A setup session runs the same playbook from the web view:

- The agent's output arrives as typed events the page renders: a message, a probe and its result, a question with choices, a proposed change, checks, and an applied generation. There is no terminal text to parse.
- The agent never writes the config. It proposes changes, the operator approves or rejects each one, and only the operator applies the approved set.

## Shape

Setup is an installed extension package, `setup`, built on `protocol/sdk` like any third party's. It lives in its own repository, [open-conduit/extension-setup](https://github.com/open-conduit/extension-setup), and speaks only the [extension protocol](../extension-protocol/) ([#243](https://github.com/open-conduit/open-conduit/issues/243); until then it was an in-process extension under `extensions/setup`). Nothing in this repository imports it, and the boundary checker fails if anything does (see [where it lives and how it ships](#where-it-lives-and-how-it-ships)). A setup scope is ordinary config:

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
    "secret_names": [], "config": {"playbook": "setup", "args": "session", "builtin_tools": [], "strict_mcp": true, "models": ["opus", "sonnet"]}}]
}
```

`open-conduit init setup` writes this scope, and a daemon with no config file and no stored generation runs on it.

- **Work.** A session is Work of kind `setup.session` in the setup scope, on the existing [Claude Code executor](../claude-code-executor/). Work gives the session everything it needs as it is: detached segments that survive a daemon restart, `--resume` continuity, waits that park it until the operator answers, an authorised toolbelt over MCP, and run history. The attempt pins its generation and binding like any other ([execution scheduling](../execution-scheduling/)).
- **Route.** The `setup` route handler matches only Events the extension's own actions produce (`setup.*`, source `core.action`) on a session key of its own scope (`setup`, `session/<scope>/<id>`). A `setup.start` with an unowned key creates the session's Work, with the chosen model as the Work's context. Routes set a new Work's context through `RouteDecision.Context`, a new field, because a route had no way to before. Every later operator Event carries the session's key. During a running turn the route injects and falls back to queue. While the session waits on a question, only the answer wakes it, and a message is queued until the turn the answer starts. The agent's own Events carry no key, so they are recorded on the session and never route back to it.
- **No shell.** The binding turns Claude Code's built-in tools off. That is a new executor option, `builtin_tools`: a list passed as `--tools`, where an empty list means none. The extension's config check refuses a setup route whose bound agent is not on `claude-code`, does not set `builtin_tools` to `[]`, or is granted operations beyond `read_context`, `wait`, `declare_outcome` and `setup.*` (`binding_refused`, at the route's `binding`). A route handler's type can now check the binding each of its routes binds (`inproc.ConfigType.Binding`, or `routes[].binding` in a package's manifest); before, a handler saw only its own route config. The scope `init setup` writes also sets `strict_mcp`, so the operator's own MCP servers and claude.ai connectors (Slack, Drive and the like) don't load into a session either; `builtin_tools` alone turns off only Claude Code's own tools. The check doesn't require `strict_mcp`, so a setup scope written before it still validates. Everything the agent learns comes through a probe, and everything it says goes through an operation.
- **More, for one session.** What a session may have beyond talking is the setup binding's [grants](../config-management/#binding-grants), which the binding check doesn't limit: the config file is their ceiling. The scope `init setup` writes offers `shell` (`builtin_tools` with `Bash`, `Read`, `Edit`, `Write`, `Glob` and `Grep`) and `connectors` (`strict_mcp: false`). An operator who wants a session to reach GitHub adds a grant with the GitHub operations (and a GitHub entry and connection in the setup scope). The Setup page's start form shows each grant as a checkbox, all off. The `start` action's input is the model, then `+name` for each grant to turn on (`opus +shell`); the start Event and the session's context carry them as `grants`, the route gives them to the session's Work, and the agent is told what is on when it starts. The session panel shows them under Access.
- **Model.** The operator picks the model when starting a session. The `start` action offers the binding's `models` as choices, and the route puts the choice in the Work's context. That is a second new executor option: a binding with `models` takes the Work context's `model` if it is one of them, and otherwise uses the binding's `model`. The model is never written to the config.
- **One session at a time.** The `start` action is listed only while the scope has no open session.
- **Playbook.** The setup playbook is the package's (`playbooks/setup.md`, declared as the manifest's `playbooks`), because it covers Slack, Linear and Jira as well as GitHub. The daemon puts it in the turn plugin like any package playbook, and `init --with claude` takes it from the first-run package (see [the playbook](#the-setup-playbook-under-a-session)).
- **The agent's events.** `setup.say`, `setup.ask`, the proposals, decisions, checks and probes are recorded with `event.accept` in the same commit as the change set they touch, derived from the turn's trigger, so a session's events count against the causal depth and per-minute limits like any package's. `setup.applied` is derived from the trigger of the session's latest attempt, and a task checks for it every second rather than after each configure. They carry no execution ID, and their IDs are the host's (`ext/…`), not `setup/…` as in-process. A package can't read Waits, so whether the answer action is offered comes from the record: the session waits, its latest question is unanswered and no attempt has started since. The probes `setup.fs.list` and `setup.tools` run inside the package's process.

### Where it lives and how it ships

- **Repository.** [open-conduit/extension-setup](https://github.com/open-conduit/extension-setup) is its own Go module: the extension, `cmd/setup-extension` on `protocol/sdk`, the playbook and a certification fixture. Its CI runs `make check` and `make certify` against the latest Open Conduit release and against `main`.
- **Pin.** This repository pins a version of it with a `tool` directive in `go.mod`. Nothing imports it: `make packages` and `scripts/bundle-packages.sh` build it from the module cache. Go allows the module cycle (setup requires open-conduit for the SDK); a build here uses this repository's own packages.
- **Release.** GoReleaser runs `scripts/bundle-packages.sh` and puts `extensions/setup/` (the binary, its manifest, certification and playbooks) in every archive beside `open-conduit` and `os`, so install.sh, `open-conduit upgrade` and the Homebrew cask all carry it. The daemon finds it there as a [bundled package](../extension-protocol/#loading). install.sh and `upgrade` replace that directory with the release's, so the bundled copy always matches the binary.
- **An operator's own copy.** `open-conduit extension install` of a setup release into `<config dir>/extensions/setup` replaces the bundled one. A copy that needs a newer host is refused with `needs_newer_host` rather than swapped for the bundled one; `extension remove setup` brings the bundled one back.
- **State.** The change sets keep their KV namespace, `setup.changesets`, so a session's change set carries over from the in-process extension to the package.
- **Tests.** The package's own repository tests the session and the change set (`TestDaemonSetupSession` and `TestDaemonSetupChangeSet` on `extensiontest.Daemon`), its store on `sdktest`, and its playbook with `open-conduit extension check`. Core keeps what is its own behaviour: `TestDaemonDefaultConfiguration` and the offline checks of the playbook's example config build the pinned package and lay it out beside the test daemon binary as the archive does, with no network.

### Why not a reserved scope

An earlier draft put the session in a `core.setup` scope that no config file contains, with its binding fixed in code. That broke more written rules than it was worth:

- B05 requires at least one scope and no implicit one, constrains scope IDs, and keeps a stored generation self-contained.
- Execution scheduling pins every attempt to a binding in its generation.
- B04 needs a route before an Event can reach Work.
- B01 keeps domain behaviour out of management.

The extension needs none of those exceptions. Everything the agent may do sits in a file the operator can read. The one thing the reserved scope bought was that config couldn't widen the agent. Here the extension's config check enforces the same limits, and only the operator can apply a config that would change them.

## A fresh install

`open-conduit web` stays a client: it writes no config and starts no daemon. The daemon gets a **default configuration** instead:

- **No config file and no stored generation.** `serve` builds the default document: the first-run setup's `extension.Setup` result with no flags (the scope above), merged into an empty config by the same host that `init` uses. The first-run setup is the one marked `FirstRun` (`setup.first_run` in a package's manifest), so core names no extension; today it is the bundled `setup` package's. With no such package, `serve` refuses and names the remedy. It validates that document and publishes it as generation 1. The file is not written. Generation 1 is stored and self-contained like any other, so the validator still requires at least one scope, and no generation 0 is needed. The first apply writes the file. `open-conduit init setup` writes the same scope to a file for an operator who wants it there first.
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
| `setup.decision` | operator `approve_*`, `reject_*`, `approve_all_*` actions; `setup.withdraw` | `changes`, `decision`, `reason` |
| `setup.checks` | `setup.validate`, operator `validate` action | `source`, `candidate_hash`, `valid`, `approved`, `diagnostics` |
| `setup.applied` | the extension, on the generation that applies the set | `generation`, `hash`, `changes` |

- **Questions** work like `human.ask`. `setup.ask` is an `Operation.Wait`: it records the question, parks the session, and the operator's `answer` action (one button per choice, or a text box) wakes it. Every ask has a 24-hour wait timeout. When it expires, the agent sums up where it got to and finishes.
- **Messages** from the operator are the `message` action on the session's Work. The route injects a message into a running turn. Inject success means accepted for delivery, not read. The agent has read the message only once Claude Code echoes it back ([injection](../claude-code-executor/#prompt-inputs-and-injection)). A segment adopted after a daemon restart takes no injected input, so its messages are presented at the next turn or wake instead. Either way, none is lost.
- **Ending.** A session ends as any Work does. The agent's structured status `finished` (or `declare_outcome`) completes it, once the config is applied and checked or the operator says they're done. The operator's End session is the operator's cancellation of the session's Work (`POST /v1/work/{id}/cancel`), as for any Work, after which late Events are checkpointed `terminal` as B04 says. The extension offers no end action, because an action's `Invoke` may not write state. The page shows that the Work ended, from its state. No `setup.ended` Event has to race the cancellation. An open change set is abandoned when its session ends.
- **Reloading.** The page holds no state. It reads the Work's panels, actions and candidate on every load and redraws them as live regions. A running turn's text comes from `GET /v1/runs/{id}/live`. A reload, a closed tab, or a restart of `open-conduit web` loses only an unsent draft.
- **The page.** The Setup page is the web view's generic [extension page](../extension-protocol/#pages): the extension declares it (`Pages`: entry and route `setup`, Work kind `setup.session`, start `start`, compose `answer` then `message`), and `internal/web` names nothing of it. The session draws itself as two panels: `conversation`, whose messages are its Events in acceptance order (the assistant's messages and questions, the operator's messages and answers, and a note for each `setup.probe`, `setup.proposal`, `setup.decision`, `setup.checks` and `setup.applied`, the latest 200 kept), and `model and access`, what the operator chose at the start. Core reviews the change set from `ConfigCandidate` and owns Apply.

## What the agent may read and run

The setup playbook's ground rules say the agent probes and never handles a secret. Here the toolbelt enforces them. An execution token reaches only `/v1/toolbelt/call` (B05), so every probe is an operation of the setup extension. The extension runs each probe under the session's own authority, never as a proxy for an operator read.

**The candidate, and nothing else of the config.** The agent can read three things, all redacted the way `config get` is (secret references become their names with `[redacted]`):

- `setup.read` returns the outline of the candidate: connection IDs and types, scope IDs and names, entry types, binding names. This is the same wiring `GET /config` shows, with no opaque config and no references.
- `setup.read` also returns, in full, the scopes and connections the session's change set touches, and the settings.
- All of it is read from the candidate, the base plus the approved changes, with every change and its state.

To work on an existing scope, the agent proposes a change to it; the outline names every scope. Naming scopes when the session starts is not built: `start` takes only the model.

**Named probes, owned by what they're about.** `setup.probe` takes a probe name and arguments, never a command line. The probes come from `extension.SetupProbes`, a new optional interface beside `extension.Setup`, so each one lives with the code that knows its domain. Each probe declares its arguments, runs without a shell, has a short timeout and an output cap, and returns safe output plus a display command. Adding one is a code change with a test that its output carries no secret.

| Owner | Probe | Returns |
| --- | --- | --- |
| `setup` | `fs.list` | a directory's entries under the operator's home, names and kinds only, two levels at most. Hidden entries are skipped, and the result says how many. A directory that is a git checkout lists its remotes' URLs with any user-info stripped (an `https://token@…` remote shows the host only) |
| `setup` | `tools` | whether each of `gh`, `git`, `ngrok` and `cloudflared` is on the daemon's `PATH`, with its path and whether it's a symlink or a script |
| `open-conduit` (the host) | `init` | `init TYPE --dry-run` for every setup but the first-run one, registered (`github`, `slack`, `linear`, `jira`) or installed. It calls the type's `extension.Setup` with a non-interactive host: `Prompt` returns `ErrSetupNoInput`, `StoreSecret` stores nothing and returns the reference it would use, and `Run` and `OpenURL` refuse. Flags take references as `init` does, and refuse literals |
| `open-conduit` (the host) | `doctor` | the daemon checks that need no secret (`config`, `webhooks`, `service`, the executors'). This is a new narrow host function over the same checks as `GET /v1/doctor`, without `check_secrets` |
| `claude-code` | `status` | whether `claude` is on the daemon's `PATH`, its version, and whether it is signed in and how (subscription login or API key), with no account detail or key. The `claude_code` doctor check doesn't report it yet |
| `github` | `accounts` | `gh auth status --json hosts`: each account's host, login, active flag, token source and state |
| `github` | `repos` | `gh repo list OWNER --json nameWithOwner` |

The playbook starts with `claude-code.status`, so its first words to the operator are that Claude Code is installed and signed in, so it can do the work, or what to fix first.

**No probe takes a reference or a command.** There is nothing that reads an arbitrary file, prints the environment, runs a `cmd:` reference, or resolves a secret. The earlier draft's `gh.login`, which resolved `gh auth token --user LOGIN` with a login the agent chose, is dropped: B05 lets no request supply a command or reference to resolve. Proving a credential works is `doctor` with `check_secrets`, which stays an operator action. Once a change set is applied, the Setup page shows doctor's checks (`GET /v1/doctor`). Running doctor with secrets checked, and recording the result for the agent as `setup.checks`, is not built yet: the playbook asks the operator to run `open-conduit doctor --check-secrets` and tell it the outcome.

**Tracing the candidate.** `setup.trace` runs the route trace from [#126](https://github.com/open-conduit/open-conduit/issues/126) with one of a scope's named samples, against the session's candidate only. It never traces other scopes, and it writes nothing, like the trace itself. This is the last step of setup: it proves that a synthetic event, such as "a new issue is opened", would be picked up, without the operator labelling a real issue. The runtime's `TraceCandidate` reads the candidate's scopes in place of the stored ones, through the host's config service; it is an internal function, not a new route, and a trace's generation reads `candidate`.

## Secrets and references

- **No values.** No probe reads or resolves a secret (above). `setup.propose` applies `config set`'s rule: a value under `secret_refs`, or in any declared secret slot, must be a reference, and anything else is refused `not_a_reference`.
- **No reference text in Events.** A reference is sensitive in views, errors and telemetry (B05). An Event for a pointer inside `secret_refs` stores `before` and `after` as the names with `[redacted]`. So does everything the agent reads back. The reference text the agent proposed is kept only in the change set, in the extension's KV namespace.
- **The extension never holds the config's references.** It works through the host's [config host](../extension-protocol/#the-config-host), as an installed package would: the host keeps the config text behind handles and answers only copies with references redacted, and builds each candidate from the edits the extension proposes against a handle.
- **The operator sees what they approve.** The change set's review shows the full reference. `GET /v1/work/{id}/config-candidate` is an operator-class read, so the reference reaches the operator and not the execution. This is the one view that shows reference text, and it shows only references proposed in this change set.
- **A last check.** Before accepting any Event, the extension scans its strings for the token shapes Open Conduit knows (GitHub, Slack, Linear, Anthropic, private-key blocks). A match in probe output replaces the output with `redacted: true`. A match in text the agent wrote refuses the call with `looks_like_secret`. This is a backstop for mistakes, not the guard: the structural rules above are the guard.

Credentials still go in from the operator's own terminal. When a connection needs one, the agent asks the operator to run `open-conduit secret set NAME`, asks whether it's done, and proposes the reference.

## Change sets

A change set is the session's list of JSON-pointer edits to the config file, each with a reason, waiting for the operator. It lives in the setup extension's KV namespace (`setup.changesets`), keyed by session. There is no core table.

- **Base.** A change set is based on the committed generation and the config file's text when the session starts, which it names by the host's handle; it keeps only the redacted text. A change edits the file text with `config set`'s editor, which addresses array elements by `id` or `name` and appends with `-`. A pointer into a scope from an [included folder](../config-management/#included-folders) is refused `in_folder`, because that text lives in another file. Running on the default configuration with no file, the base text is the default document.
- **Proposing.** `setup.propose {op, pointer, value?, why}` adds a `proposed` change. The extension fills in `before`, checks the pointer resolves for `replace` and `remove`, and validates the base plus the approved changes plus this one, attaching the diagnostics. A proposal that doesn't validate on its own is still recorded, since a scope and its binding may come in two proposals, and the review shows its diagnostics. `setup.withdraw` takes back an undecided change.
- **Deciding.** The session's candidate lists each change, naming `approve_<change>` and `reject_<change>` on each open one, and the web view's review draws them as buttons. Reject prompts for an optional reason. With more than one open, the candidate also offers Approve all, labelled "Approve all N". Its ID carries no list, only the revision of the open proposals it was listed for (`approve_all_<revision>`), so it approves exactly the proposals the operator saw and refuses `change_set_moved` if any changed. One click is one action and one `setup.decision`, whose `changes` names every change it approved. Decisions are Events, since an action may not write state: a change's state is its operator's latest decision after its proposal, folded from the session's `setup.decision` Events. A rejected change stays in the list, so the agent doesn't propose it again.
- **Validating.** `setup.validate` (agent) and the operator's `validate` action build the candidate (the base text plus the approved changes, in proposal order, which the host builds and holds) and validate it. The host runs the same side-effect-free validation as `POST /v1/config/validate`. The result and its `candidate_hash` are recorded as `setup.checks`.
- **Applying.** Apply is not an action and not an operation. The web view's Apply button is an operator client doing what `config apply` does:
  1. It reads the candidate from a new operator-class route, `GET /v1/work/{id}/config-candidate`, served by an optional `extension.ConfigCandidate` interface on the Work's extension. The extension lists its changes and, once the approved changes validate, names its candidate's handle; the host serves the text behind it, with `base_generation` and `candidate_hash`.
  2. It calls the existing `POST /v1/config/apply` with that `expected_generation` and `candidate_hash`.

  The extension's `Configurer` receives every applied generation while the host holds the config service, so it only notes the generation; its `applied` task, run by the host loop straight after, compares each open session's candidate with the active hash. On a match it marks those changes `applied`, moves the change set's base to the new generation, and accepts `setup.applied` on the session's key, which reaches the agent as input. Whatever reads a change set first after an apply moves it onto the new generation, and the page reloads its review straight away, usually before the task has looked. So every read makes the same comparison before it moves the change set: on a match it marks the changes `applied` and leaves `setup.applied` pending, and the task sends it ([#261](https://github.com/open-conduit/open-conduit/issues/261)).
- **Key order.** `POST /config/apply` saves `config` in compact form (B05). To keep the operator's layout, apply takes `config_text`, the file text as a JSON string, in place of `config`. The daemon validates the parsed text and saves the text byte for byte. `open-conduit config apply FILE` sends it too.
- **When the config moved.** If the generation changed after the page read the candidate, apply fails with `generation_conflict` and the page asks the operator to reload and review. There is no `rebase` action: the next time the agent proposes, reads, validates or traces, or the operator reads the candidate, the change set moves onto the configuration now. Each change's `before` is recomputed, approvals whose `before` is unchanged stand, and the rest return to `proposed`.

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

- **[B05 configuration](../config-management/)** and **[toolbelt and management](../toolbelt-management/):** "The first start activates the config file as generation 1" gains the default configuration when there is no file and no stored generation. `POST /config/apply` gains `config_text`, saved as given. New: `GET /v1/work/{id}/config-candidate`. The change set's review is the one view that shows reference text, and only to the operator.
- **[Claude Code executor](../claude-code-executor/):** new `builtin_tools` and `models` options, and the `claude-code.status` probe.
- **[Extensions](../extensions/):** new optional `extension.SetupProbes` and `extension.ConfigCandidate`, and in-process host functions for validation, narrow doctor checks and setup registrations.
- **[Operator setup](../operator-setup/):** `init setup`, the playbook's new home and its session mode, the `default_config` doctor code, and the `claude_code` check's sign-in state.
- **[Observability](../observability-history/):** the web view's Setup page, since [#242](https://github.com/open-conduit/open-conduit/issues/242) a generic [extension page](../extension-protocol/#pages).
- **[#126 route trace](https://github.com/open-conduit/open-conduit/issues/126):** tracing against a candidate snapshot.

## How it was built

Each step landed with its tests and the page amendments above:

1. The `setup` extension with its scope, `setup.start`, `setup.say` and `setup.ask`, messages, the route, and the executor's `builtin_tools` and `models` ([#136](https://github.com/open-conduit/open-conduit/pull/136), [#142](https://github.com/open-conduit/open-conduit/pull/142)). Then the config check on the setup route's binding ([#143](https://github.com/open-conduit/open-conduit/pull/143)), the default configuration at first start with `init setup` ([#144](https://github.com/open-conduit/open-conduit/pull/144); `doctor` also reports `disk_missing` when a generation that is not the default runs without its file), and the playbook's move to `extensions/setup`, which adds it to the turn plugin.
2. Change sets: propose, withdraw, decide (including Approve all), validate, the candidate route, `config_text`, and the applied hook, with the operator-only test (`TestDaemonSetupChangeSet`, now in the package's repository: the session's own execution token is refused by apply, the candidate route, validate and the action routes, apply-shaped operation input is refused, and the generation is unchanged).
3. Probes: `extension.SetupProbes`, the setup, `claude-code` and `github` probes, `setup.read`, the secret scanner, and `setup.trace` against the candidate.
4. The playbook's session mode.
5. The web view's Setup page, built from the session's Events and panels ([observability](../observability-history/)). [#242](https://github.com/open-conduit/open-conduit/issues/242) made it a generic extension page the extension declares, with the conversation drawn as the extension's panel and the change set's review and Apply core's. The conversation and the session panel redraw as live regions of their own, with the message box outside them, so typing never holds back a reply. The page stays at the newest message while the operator is at the bottom and leaves them where they are once they scroll up. The message and answer boxes are textareas that grow, sending on Enter, with Shift+Enter for a new line, and an operator's line breaks are kept in the conversation.

6. The move to an installed package in its own repository ([#243](https://github.com/open-conduit/open-conduit/issues/243)): core gained `entry_gated`, route binding checks, `setup.first_run`, package playbooks and bundled packages first ([#248](https://github.com/open-conduit/open-conduit/pull/248)); then the extension moved to open-conduit/extension-setup on `protocol/sdk`, and the release archive bundles it.

Not built: naming scopes when a session starts, the operator's `doctor` action with secrets checked recorded as `setup.checks`, and the sign-in state on the `claude_code` doctor check.

## Decided in review

- **Daemon-owned session, typed events as toolbelt calls, no shell, named probes, operator-only apply:** approved on #127.
- **Model:** chosen when the session starts and stored on its Work, not in config.
- **Fresh install:** the daemon starts without a file, and `open-conduit web` stays a client.
- **Approve all:** one action per click, recorded as one `setup.decision` naming every change it approved.
- **Extension over reserved scope:** chosen after review, for the reasons [above](#why-not-a-reserved-scope).
