---
title: Run agents on Codex
description: Have Open Conduit's agents work your issues on the OpenAI Codex CLI instead of Claude Code.
---

Open Conduit's agents run on Claude Code by default. They can run on the [Codex CLI](https://developers.openai.com/codex/cli) instead. The playbooks, the toolbelt and the way an issue is worked stay the same. Only the agent binding's executor changes.

## What you need

- **The Codex CLI**, installed and signed in on the machine the daemon runs on: `codex login`, or an API key you store as a secret (see [the API key](#use-an-api-key)).
- **The Codex package.** Homebrew, the release archives and the install script bundle it in `extensions/codex/`, so there's nothing to install. Check it's there with `open-conduit extension list`. A `go install` or a source build brings no packages: run `make packages` and then `open-conduit extension install dist/packages/codex`, beside the other packages you use ([install](/open-conduit/start/install/#from-source)).

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

Settings only Claude Code reads, such as `permission_mode` or `builtin_tools`, are refused on a Codex binding. Remove them, then use the settings below.

## Settings

All of these are optional and go in the binding's `config`:

| Setting | What it does |
| --- | --- |
| `model` | The Codex model, for example `gpt-5-codex`. Unset, Codex uses its own default. |
| `sandbox` | What the agent's commands may touch: `danger-full-access` (the default: it pushes branches and calls the network), `workspace-write` or `read-only`. Codex never stops to ask for approval, because nobody is there to answer. |
| `binary` | The Codex CLI, if it isn't `codex` on the daemon's `PATH`. |
| `extra_args` | More `codex exec` arguments, such as `["--search"]` for web search. |
| `timeout_seconds` | The longest a turn may run. |
| `handoff_minutes`, `cold_turns` | As for Claude Code: a turn more than `handoff_minutes` (55) after the last starts a new thread from the agent's handoff note, unless `cold_turns` is `resume`. |
| `handoff_tokens` | Once a turn ends with more context than this, the next turn starts a new thread from the agent's handoff note instead of resuming (default 200000; 0 always resumes). |
| `auto_compact_tokens` | Where Codex compacts a thread in the middle of a turn (default 250000; 0 leaves Codex's own threshold). |
| `provider` | A model provider other than OpenAI, such as OpenRouter, LiteLLM or Ollama. It must speak the Responses API. See [use models from other providers](/open-conduit/guides/third-party-models/#codex). |

`playbook`, `branch`, `args`, `workspace_mode`, `dir`, `env` and `secret_env` work as they do for Claude Code. The [Codex executor](/open-conduit/architecture/codex-executor/#binding-config) page lists every field.

Codex runs with a clean environment of its own. For it to see variables from the daemon's environment, such as a proxy or a `CODEX_HOME` that keeps its login somewhere other than `~/.codex`, name them in the binding's own `pass_env`, beside `config` rather than inside it:

```sh
open-conduit config set /scopes/web/bindings/agent/pass_env '["CODEX_HOME", "HTTPS_PROXY"]'
```

### Use an API key

Codex otherwise uses your `codex login`. To use an OpenAI API key instead, store it, select it on the binding and map it to `CODEX_API_KEY`, the variable `codex exec` signs in with (it doesn't read `OPENAI_API_KEY` without a login):

```sh
open-conduit secret set openai
```

```json
"secret_names": ["openai"],
"config": { "playbook": "work-issue", "secret_env": { "openai": "CODEX_API_KEY" } }
```

The scope's `secret_refs` names `openai`, as for any secret ([configuration and secrets](/open-conduit/guides/configuration/)).

`open-conduit doctor --check-secrets` checks the key: `codex-auth` lists OpenAI's models with it (or the provider's, for a binding with a `provider`) and says `ok` or `unauthorised` for each binding. A binding that signs in with your `codex login` shows `not_checked`, because a ChatGPT sign-in has no free request to try; `codex login status` shows whether you're signed in.

### Run Codex in a container

A binding with a `container` runs each turn in a container, so the agent can reach only what the container's config grants: not your home, your own Codex login, or Open Conduit's state. [Process sandboxes and containers](/open-conduit/architecture/sandboxes/#containers) describes the config and the Docker provider.

- **The image** needs Codex on its `PATH` (or at the binding's `binary`) and `/bin/sh`, `sed` and `head`. `open-conduit doctor` runs `codex --version` in it and fails if Codex isn't there.
- **Sign in with a secret.** Your `codex login` stays on the host. Select an API key and map it to `CODEX_API_KEY` as [above](#use-an-api-key), or use a `provider` with its `api_key_secret`. Codex's sessions live in the Work's own home, so its next turn resumes the thread in a new container.
- **Allow the model's API.** The container's `network.allow` must hold `api.openai.com`, or the provider's host, or the turn is refused before it starts. Add the hosts the work needs as well, such as `github.com`.
- **Leave `sandbox` at its default**, `danger-full-access`: the container is the boundary, and Codex's own sandboxes may not work inside one.

## What's different from Claude Code

An agent on Codex works an issue in the same turns: it starts on the issue, ends each turn with a status, and picks the thread up again when a comment, a check result or a merge arrives. A comment that arrives while it's working is taken in at the end of that turn, before it reports. These things differ:

- **No attach.** `open-conduit work attach` can't take over a Codex session yet. Run `codex resume` in the workspace if you need to look inside one.
- **Plainer messages.** The agent gets each event's type, author, title, link and text, but not the reply hints Claude Code gets, such as which operation answers an inline review comment. It finds those with the GitHub tools itself.
- **Containers, not process sandboxes.** A binding can run Codex in a [container](#run-codex-in-a-container). A binding with a process `sandbox` is refused for Codex; use a container, or the `sandbox` setting above, Codex's own sandbox.
- **Usage limits** are retried after a backoff, because Codex doesn't say when a limit resets.

Each turn's Codex events are kept with the run as its transcript artifact, and Codex's own error output as its log.
