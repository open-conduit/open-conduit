---
title: Run agents on Codex
description: Have Open Conduit's agents work your issues on the OpenAI Codex CLI instead of Claude Code.
---

Open Conduit's agents run on Claude Code by default. They can run on the [Codex CLI](https://developers.openai.com/codex/cli) instead. The playbooks, the toolbelt and the way an issue is worked stay the same. Only the agent binding's executor changes.

## What you need

- **The Codex CLI**, installed and signed in on the machine the daemon runs on: `codex login`, or an API key you store as a secret (see [the API key](#use-an-api-key)).
- **The Codex package.** Homebrew, the release archives and the install script bundle it in `extensions/codex/`, so there's nothing to install. Check it's there with `open-conduit extension list`. After a `go install` or a source build, run `make packages` and then `open-conduit extension install dist/packages/codex` ([install](/open-conduit/start/install/#from-source)).

## Set it up

Set the whole thing up with Codex, which opens a Codex session on Open Conduit's setup guide:

```sh
open-conduit init --with codex
```

Codex asks you before each command that changes anything outside the checkout, the config included. Tell it you want the agents on Codex too.

Or add a repository by hand:

```sh
open-conduit init github --repo acme/web --checkout ~/Code/web --executor codex
```

That writes the same scope as `init github` always does, with the agent binding on `codex`. For a scope you already have, change its agent binding's executor, then check the config:

```sh
open-conduit config set /scopes/web/bindings/agent/executor '"codex"'
open-conduit config check
```

Settings only Claude Code reads, such as `permission_mode`, `cold_turns` or `builtin_tools`, are refused on a Codex binding. Remove them, then use the settings below.

## Settings

All of these are optional and go in the binding's `config`:

| Setting | What it does |
| --- | --- |
| `model` | The Codex model, for example `gpt-5-codex`. Unset, Codex uses its own default. |
| `sandbox` | What the agent's commands may touch: `danger-full-access` (the default: it pushes branches and calls the network), `workspace-write` or `read-only`. Codex never stops to ask for approval, because nobody is there to answer. |
| `binary` | The Codex CLI, if it isn't `codex` on the daemon's `PATH`. |
| `pass_env` | Variables from the daemon's environment that Codex should see. `CODEX_HOME` always passes. |
| `extra_args` | More `codex exec` arguments, such as `["--search"]` for web search. |
| `timeout_seconds` | The longest a turn may run. |

`playbook`, `branch`, `args`, `workspace_mode`, `dir`, `env` and `secret_env` work as they do for Claude Code. The [Codex executor](/open-conduit/architecture/codex-executor/#binding-config) page lists every field.

### Use an API key

Codex otherwise uses your `codex login`. To use an OpenAI API key instead, store it, select it on the binding and map it to `OPENAI_API_KEY`:

```sh
open-conduit secret set openai
```

```json
"secret_names": ["openai"],
"config": { "playbook": "work-issue", "secret_env": { "openai": "OPENAI_API_KEY" } }
```

The scope's `secret_refs` names `openai`, as for any secret ([configuration and secrets](/open-conduit/guides/configuration/)).

## What's different from Claude Code

An agent on Codex works an issue in the same turns: it starts on the issue, ends each turn with a status, and picks the thread up again when a comment, a check result or a merge arrives. A comment that arrives while it's working is taken in at the end of that turn, before it reports. These things differ:

- **No attach.** `open-conduit work attach` can't take over a Codex session yet. Run `codex resume` in the workspace if you need to look inside one.
- **Plainer messages.** The agent gets each event's type, author, title, link and text, but not the reply hints Claude Code gets, such as which operation answers an inline review comment. It finds those with the GitHub tools itself.
- **No process sandbox or container.** A binding with `sandbox` or `container` is refused for Codex. Use the `sandbox` setting above, Codex's own sandbox, instead.
- **No fresh session after a break.** Claude Code starts a fresh session from a handoff note when an issue has been quiet for an hour. Codex always resumes the issue's thread.
- **Usage limits** are retried after a backoff, because Codex doesn't say when a limit resets.

Each turn's Codex events are kept with the run as its transcript artifact, and Codex's own error output as its log.
