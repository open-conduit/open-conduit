---
title: "Setup sessions and config change sets"
description: "Proposed design for setup in the browser: a daemon-owned setup session whose agent talks in typed events, and config change sets the operator approves and applies."
---

:::caution[Proposed]
This page is a design for review ([#127](https://github.com/open-conduit/open-conduit/issues/127)). Nothing on it is built yet.
:::

`open-conduit init --with claude` runs the [setup playbook](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/playbooks/setup/setup.md) as an interactive Claude Code session in a terminal ([operator setup](../operator-setup/)). The agent looks at the machine, asks questions and edits the config file itself with `open-conduit config set`. This page designs the same setup in the web view, where:

- the agent's output arrives as typed events the page can render (a message, a probe and its result, a question with choices, a proposed change, checks, an applied generation), not terminal text;
- the agent never writes the config. It proposes changes; the operator approves or rejects each one, and only the operator applies the approved set.

Two new things carry it: a **setup session**, and a config **change set**.

## Where the session runs

The daemon owns the session. It runs as Work in a reserved scope, on the existing [Claude Code executor](../claude-code-executor/), so the browser is only a view of it.

- **Why the daemon, not `open-conduit web`.** `open-conduit web` is a client only: it holds no state, serves loopback behind a sign-in URL and can be stopped at any time ([observability](../observability-history/)). A session it owned would die with it, and it would need its own process supervision, recovery and toolbelt. The daemon already has all of these for agent turns: detached segments that survive a daemon restart, `--resume` continuity, injection of new input into a live turn, waits that park a session until the operator answers, and an authorised toolbelt over MCP.
- **A reserved scope.** Every generation gets one extra scope, `core.setup`, that the config file never contains. Scope IDs in the file can't contain `.`, so it can't collide with one. Its authorisation and its one binding are fixed in code, not config: a fresh install has no binding to borrow, and the operator can't widen what the setup agent may do by editing the file. The scope has no extensions, routes or resource. `GET /config` and `config show` leave it out; `GET /scopes` lists it as `system: true`.
- **The binding.** `setup` on `claude-code`, workspace-free (turns run in a private directory), playbook `setup`, args `session`. Its operations are exactly `read_context`, `wait`, `declare_outcome` and the `setup.*` operations below. Claude Code runs with its built-in tools turned off (`--tools ""`, or every built-in tool in `--disallowedTools` where the installed version lacks the flag) and only `mcp__open-conduit__*` allowed. The agent has no shell, no file reads and no network of its own: everything it learns comes through a probe, and everything it says goes through an operation.
- **Starting one.** `POST /v1/setup/sessions {model?}` (operator) starts a session, or returns the open one. There is one config file, so there is at most one open session. `model` is optional and defaults to the executor's default. It is stored on the session's Work and passed to each of its turns. It never goes in the config.

### A fresh install

`open-conduit web` stays a client. It writes no config file and starts no daemon. Instead, the daemon can start with nothing configured:

- **No config file.** Today `serve` refuses to start without a valid config file (`management.OpenConfig` reads it when there is no stored generation). With this change, a missing file, when there's also no stored generation, starts the daemon at **generation 0**: the empty configuration (no connections or scopes), plus `core.setup`. Generation 0 isn't stored, and the file isn't created. The first `config apply`, which a change set's apply makes with `expected_generation` `0`, writes the file and publishes generation 1. An empty file (`version`, `settings`, `connections` and `scopes`, all empty) validates and starts the same way, as generation 1.
- **What still refuses to start.** A config file that exists but doesn't validate, when there is no stored generation to fall back on, still stops `serve` with its diagnostics, as it does today. Starting empty over a file the operator wrote but got wrong would hide the mistake.
- **What the web view shows.** `GET /v1/health` reports generation `0`. The web view shows "running, nothing configured" and offers to start a session. With no daemon at all, the web view says so and gives the commands to start one (`open-conduit serve`, or `open-conduit service install`). It doesn't run them.

## Typed events

The agent doesn't produce typed events by printing text that something parses afterwards. Each one is a toolbelt call, and the call's result is recorded as an accepted Event on the session's Work. The page renders the Work's events in acceptance order. Claude Code's own transcript stays in the run's artifacts like any other turn's, but the page doesn't show it.

| Event | Emitted by | Data |
| --- | --- | --- |
| `setup.message` | `setup.say` (agent), `POST …/messages` (operator) | `role` (`agent` or `operator`), `text` (Markdown) |
| `setup.probe` | `setup.probe` | `probe`, `args`, `command` (the equivalent command line, for display), `why`, `result` (`exit`, `output`, `truncated`, `redacted`) |
| `setup.question` | `setup.ask` | `question_id`, `text`, `context`, `choices` (empty for a free answer) |
| `setup.answer` | `POST …/answers` (operator) | `question_id`, `choice` or `text` |
| `setup.proposal` | `setup.propose` | `change_set`, `change`, `op` (`add`, `replace`, `remove`), `pointer`, `before`, `after`, `why`, `diagnostics` |
| `setup.decision` | operator approve/reject; agent `setup.withdraw` | `change`, `decision` (`approved`, `rejected`, `withdrawn`), `reason` |
| `setup.checks` | `setup.validate`, operator validate, the `doctor` probe | `source` (`validate` or `doctor`), `candidate_hash`, `diagnostics` or `checks` |
| `setup.applied` | operator apply | `change_set`, `generation`, `hash`, `changes` |
| `setup.ended` | `setup.finish`, operator end, idle deadline | `reason`, `summary` |

Every event family has a schema checked at acceptance, like any declared Event. Free text appears only in `text`, `why`, `context` and `summary`, and goes through the secret check below.

## What the agent may run

The setup playbook's ground rules already say the agent probes and never handles a secret. In a session the toolbelt enforces it instead of the agent's good behaviour.

`setup.probe` takes a probe **name** and arguments, never a command line. The daemon runs each named probe itself, with an argv it builds (no shell), a short timeout and an output cap, and records the equivalent command in the event so the operator sees what was run. The first set covers what the playbook reads today:

| Probe | Runs |
| --- | --- |
| `config.show`, `config.get`, `config.check` | the same as `config show` (references redacted), `config get POINTER` and `config check` on the config file |
| `doctor` | `GET /v1/doctor`; `check_secrets` is allowed, since it reports names and codes only |
| `health`, `adapters`, `service.status`, `state.check` | the commands of the same names |
| `init.dry_run` | `init TYPE --dry-run` for every type `init` lists (`github`, `slack`, `linear`, `jira`), with the flags that type documents. References are taken as `init` takes them: a flag such as `--token` or `--bot-token` accepts a reference and refuses a literal. Flags that act outside the dry run are refused: `--via slack-cli` (which creates the Slack app) and opening a browser (`--no-open` is always passed) |
| `route.samples` | `GET /v1/scopes/{id}/route/samples` ([#126](https://github.com/open-conduit/open-conduit/issues/126)): the named sample events a scope's extensions offer |
| `route.trace` | `POST /v1/scopes/{id}/route/trace` with a sample by name, or a synthetic event: each route's verdict and reason, and the effect. With `change_set`, it traces against the change set's candidate rather than the active generation (see below) |
| `claude.status` | the executor itself: whether `claude` is on the daemon's `PATH` (and the path), `claude --version`, and whether it is signed in and how (subscription login or API key), with no account token or key. It extends the existing `claude_code` doctor check |
| `fs.list` | a directory's entries, names and kinds only, two levels at most, under the operator's home. It skips hidden entries (`.ssh`, `.config`, …) and says how many it skipped |
| `git.remote` | `git -C DIR remote get-url NAME` |
| `tool.find` | the path of `claude`, `gh`, `git`, `ngrok` or `cloudflared`, and whether it's a symlink or a script |
| `gh.accounts` | `gh auth status --json hosts --jq '…'`, the host, login, active flag, token source and state of each account |
| `gh.login` | which login a pinned `gh auth token --user LOGIN` authenticates as; the token is resolved and used inside the daemon and never reaches the agent or the event |
| `gh.repos` | `gh repo list OWNER --json nameWithOwner` |

`route.trace` is the setup's last step: it proves a synthetic event (for example "a new issue is opened") would be picked up by the scope just configured, without the operator labelling a real issue. #126 traces against the active generation, so this probe needs one addition there: an optional candidate (the change set's validated config and `candidate_hash`) to trace against instead. Like the trace itself, it writes nothing. After apply, the probe traces the active generation, which is then the same config.

There is no probe that reads an arbitrary file, prints the environment, runs `gh auth status` without `--json`, passes `--show-token`, or runs `secret set`. A probe that needs a credential (`gh.login`, `doctor` with `check_secrets`) resolves it in the daemon and reports only the outcome. Adding a probe is a code change with its own test that its output carries no secret.

## No secret in any event

Events carry references (`env:GITHUB_TOKEN`, `cmd:gh auth token --user octocat`, `keyring:github-token`), never resolved values. Three layers keep it that way:

1. **Nothing hands the agent a value.** It has no shell or file access, and no probe prints a credential (above).
2. **Proposals refuse literals.** `setup.propose` applies `config set`'s rule: a value under `secret_refs` must be a reference, and anything else is refused `not_a_reference`.
3. **A last check on every event.** Before an event is accepted, every string in it goes through the secret scanner: the token shapes Open Conduit knows (GitHub, Slack, Linear, Anthropic and private-key blocks) and any value the daemon resolved while running that probe. A match in probe output replaces the output with `redacted: true` and a code. A match in agent-written text (`setup.say`, `why`) refuses the call with `looks_like_secret`, so the agent rephrases it.

Credentials themselves still go in from the operator's own terminal. When a connection needs one, the agent tells them to run `open-conduit secret set NAME` there, asks with `setup.ask` whether it's done, and proposes the reference. A write-only secret field in the page could come later; it isn't part of this design.

## The operator's side

The web view gets a Setup page. It is built from the session's events, so it holds no state of its own.

- **Messages.** `POST /v1/setup/sessions/{id}/messages {text}` accepts a `setup.message` Event on the session's Work. If a turn is running, the daemon injects it ([injection](../claude-code-executor/#prompt-inputs-and-injection)) and the agent reads it before its next model call. If the session is parked on a question, the message wakes it like an answer. Otherwise it starts the next turn.
- **Questions.** `setup.ask` records the question and parks the session, like `human.ask`. The page shows a button per choice, or a text box for a free answer. `POST /v1/setup/sessions/{id}/answers` wakes the agent with the answer. An agent that has nothing left to say but wants the operator's next move asks a free question.
- **Proposals.** Each proposal shows the pointer, the before and after values and the reason, with Approve and Reject (and an optional reason). When several are open, the page also offers "Approve all N", which approves the open proposals in one request (below). The change set's state sits beside the conversation: which changes are approved, the last validation, and Validate and Apply.
- **Reloading.** The page reads the session's events (`GET /v1/events?work_id=…&after_sequence=…`) and the change set, and polls for more with `after_sequence`. A running turn's live text comes from `GET /v1/runs/{id}/live`, as on the run page. Reloading the page, closing the tab, or restarting `open-conduit web` loses nothing except an unsent draft. A daemon restart adopts or resumes the turn as it would any other.

### How a session ends

- The agent calls `setup.finish {summary}` once the config is applied and `doctor` passes, or when the operator says they're done. That records `setup.ended` and completes the Work.
- The operator clicks End session, which cancels the Work (`POST /v1/work/{id}/cancel`) and records `setup.ended`.
- Every `setup.ask` carries a 24-hour deadline. At the deadline the agent is woken, summarises where it got to and finishes.

An open change set is abandoned when its session ends. Applied changes stay applied.

## Change sets

A change set is a list of JSON-pointer edits to the config file, each with a reason, waiting for the operator. It lives in the daemon's store, beside the config generations.

```json
{
  "id": "cs_01",
  "session": "work_7f3",
  "base_generation": "12",
  "base_hash": "sha256:…",
  "state": "open",
  "changes": [
    {
      "id": "c1",
      "op": "replace",
      "pointer": "/connections/github/secret_refs/token",
      "before": "cmd:gh auth token",
      "after": "cmd:/opt/homebrew/bin/gh auth token --user octocat",
      "why": "Pins the account the agent acts as, so a GH_TOKEN in the daemon's environment can't change it.",
      "state": "approved",
      "proposed_by": "agent",
      "decided_by": "operator:local"
    }
  ]
}
```

- **What a change addresses.** Pointers use `config set`'s grammar, with array elements addressed by `id` or `name` and `-` to append. A change edits the config file the daemon applies, keeping its key order and untouched text as `config set` does. A pointer into a scope from an [included folder](../config-management/#included-folders) is refused `in_folder`, since that scope's text lives in another file.
- **Proposing.** `setup.propose {op, pointer, value?, why}` (agent) adds a `proposed` change. The daemon fills in `before` from the base, checks the pointer resolves (for `replace` and `remove`) and runs offline validation of the base plus the approved changes plus this one, attaching the diagnostics. A proposal that doesn't validate on its own is still recorded, so the agent can propose a scope and then its binding, but the page shows the diagnostics against it. The agent can `setup.withdraw` a change it proposed until it's decided.
- **Deciding.** `POST /v1/config/change-sets/{id}/decide {decisions: [{change, decision, reason?}]}` (operator) approves or rejects one or more changes in one request. One change is a list of one, and "Approve all N" sends every open proposal. The request applies all its decisions or none: a change that isn't `proposed` any more (withdrawn, or already decided) refuses the whole request with `change_not_open`, so the page never approves something the operator didn't see. Each decision is still recorded as its own `setup.decision` on the session, so the agent hears about each one. A rejected change stays in the list, so the agent doesn't propose it again.
- **Validating.** `setup.validate` (agent) and `POST /v1/config/change-sets/{id}/validate` (operator) build the candidate (the base file plus the approved changes, in proposal order) and run the same `POST /v1/config/validate` the CLI uses. The result, with its `candidate_hash`, is recorded as `setup.checks`.
- **Applying.** `POST /v1/config/change-sets/{id}/apply {expected_generation, candidate_hash}` (operator only) builds the same candidate and runs `config apply` with `expected_generation` set to the change set's base. Applied changes become `applied`; the set's base moves to the new generation, so later proposals in the session build on it. The daemon records `setup.applied`, which wakes the agent to run `doctor`.
- **When the config moved.** If the generation or the file changed after the set's base (the operator ran `config apply` or edited the file), apply fails with `generation_conflict` and the set goes `stale`. `POST …/rebase` re-reads the current file and generation, recomputes each change's `before`, keeps approvals whose `before` didn't change and returns the rest to `proposed`.

### Only the operator applies

The agent can propose, withdraw and validate. It can't apply, by construction rather than by instruction:

- There is no `setup.apply` operation, and the `core.setup` binding's operations are fixed in code, so no config can grant one.
- Every change-set route that decides or applies requires the operator credential class. The agent's MCP server holds an execution token, which the management API refuses with `403` on operator routes ([authority](../config-management/#local-transport-discovery-and-authority)).
- The agent has no shell, so it can't run `open-conduit config apply` or read the operator token file, which the terminal setup can.

The acceptance test drives a session's agent through every route it can reach: `setup.*` with an apply-shaped input, `POST /v1/config/apply` and the change-set apply and decide routes with its execution token. Each must be refused, and the generation must stay the same.

## The setup playbook under a session

One playbook, two modes. The playbook is rendered from a template, as `{{bin}}` already is, and the session mode is turned on by the session's args (`/open-conduit:setup session`). The introduction, the ground rules, the questions to settle and the config examples stay shared. What changes:

| In the terminal | In a session |
| --- | --- |
| checks `command -v claude` along the way | starts with `claude.status`, and tells the operator first that Claude Code is installed and signed in, so it can do the work (or what to fix) |
| reads with `ls`, `git`, `gh`, `config show`, `doctor` | `setup.probe` with the matching probe |
| talks and asks in the chat | `setup.say`, and `setup.ask` with the likely answers as choices |
| `config set`, `init github` (without `--dry-run`) | `init.dry_run` to build the scope, then `setup.propose` for each change, with the reason the playbook already asks it to explain |
| `config validate` and `config apply` | `setup.validate`, then ask the operator to review and apply in the page; `setup.applied` wakes it |
| asks the owner to run `secret set` | the same, then `setup.ask` whether it's done |
| suggests labelling a real issue to see it picked up | `route.trace` with a sample, against the candidate before apply and the active generation after |
| ends when the owner closes the terminal | `setup.finish` with a summary |

The session-mode section tells the agent it has no shell and that a proposal is a request, not an edit: until `setup.applied` arrives, nothing has changed. It proposes in small, explained steps (a connection, then a scope, then its binding) so each one can be judged on its own.

## Building it

The smallest cut that works end to end, in order:

1. Starting with no config file at generation 0.
2. Change sets in the store and their management routes (propose, decide, validate, apply, rebase), with the operator-only apply test. Usable from the CLI before any session exists.
3. The `core.setup` scope and binding, `POST /v1/setup/sessions` with its `model`, the `setup.*` operations and event families, the probes and the secret scanner. `route.trace` needs #126 merged and its candidate input added.
4. The playbook's session mode.
5. The web view's Setup page.

## Decided in review

- **Reserved scope.** `core.setup` outside the config file, so the session reuses Work, runs, waits, injection and recovery unchanged, rather than a separate session resource with its own process handling.
- **Probes.** The first set above, with hidden entries skipped by `fs.list`, the `claude.status` executor check, every `init` type, and the route trace as the last step.
- **Model.** Chosen when the session starts and stored on its Work, not in config.
- **Fresh install.** The daemon starts empty at generation 0; `open-conduit web` stays a client.
