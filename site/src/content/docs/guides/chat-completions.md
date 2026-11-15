---
title: Run toolbelt-only agents on any model
description: Run Work that needs judgement but no shell or files, such as triage, replies and summaries, on a model from OpenRouter, Ollama or any other OpenAI-compatible endpoint.
---

Some Work needs judgement and the toolbelt, but no shell, files or checkout: triaging an issue, replying in a Slack thread, summarising a thread, labelling. The `chat-completions` executor runs that Work on any endpoint that speaks the OpenAI Chat Completions API, such as OpenRouter, Ollama, OpenAI itself or a LiteLLM proxy. Each turn is a series of HTTP requests with no CLI, workspace or container, so cheap models and many parallel turns cost little.

The agent's tools are the toolbelt operations its binding is granted, one function each, and nothing else. It can't run commands or read files. Every call goes through the toolbelt, so the grant, gates and run history apply exactly as they do for Claude Code. For Work that changes code, keep using Claude Code or [Codex](../codex/).

## What you need

- **An endpoint and a model that supports tool calling.** Most current models on OpenRouter do. On Ollama, pick a model whose page lists tools.
- **The package.** Homebrew, the release archives and the install script bundle it in `extensions/chat-completions/`. Check it's there with `open-conduit extension list`. A source build brings no packages: run `make packages` and then `open-conduit extension install dist/packages/chat-completions` ([install](/open-conduit/start/install/#from-source)).

## On OpenRouter

Store your OpenRouter key as a secret:

```sh
open-conduit secret set openrouter
```

Then add a binding to the scope, name the secret in the scope's `secret_refs` (with the reference `secret set` printed) and point a route at the binding:

```json
{
  "name": "triage",
  "executor": "chat-completions",
  "requires": { "toolbelt": "sdk" },
  "operations": ["read_context", "wait", "declare_outcome", "github.issue.view", "github.issue.comment"],
  "secret_names": ["openrouter"],
  "config": {
    "base_url": "https://openrouter.ai/api/v1",
    "model": "openai/gpt-4o-mini",
    "api_key_secret": "openrouter",
    "playbook": "triage",
    "plugin_dir": "/Users/you/playbooks",
    "headers": { "HTTP-Referer": "https://example.com", "X-Title": "Open Conduit" },
    "request": { "usage": { "include": true } }
  }
}
```

`playbook` is what the agent works to: write it as a skill, `/Users/you/playbooks/skills/triage/SKILL.md`, saying what to look at, what to do and how to end the turn. Without one the agent gets only the events. `headers` are optional: OpenRouter uses `HTTP-Referer` and `X-Title` to attribute your traffic. `request` is passed through in every request's body. Here it asks OpenRouter to report each request's cost, which then shows on the run and counts towards [budgets](../budgets/). Check the config with `open-conduit config check`.

## On Ollama

Ollama serves the Chat Completions API on its own port, with no key:

```json
"config": {
  "base_url": "http://localhost:11434/v1",
  "model": "qwen3:8b",
  "playbook": "triage",
  "plugin_dir": "/Users/you/playbooks",
  "handoff_tokens": 24000
}
```

Ollama's default context window is small, and a playbook, the toolbelt's guidance and a few events soon fill it. Raise it on the Ollama server (for example `OLLAMA_CONTEXT_LENGTH=32768`) and set `handoff_tokens` below it, so a long conversation starts afresh from the agent's last note before the model starts dropping the beginning.

LiteLLM, vLLM and OpenAI work the same way: their API root as `base_url` (`https://api.openai.com/v1` for OpenAI), and `api_key_secret` when they need a key.

## How a turn ends

The agent ends each turn by calling `declare_outcome` (completed, handoff or failed, with a summary and, when its playbook asks for one, a status such as `review`) or `wait`, which parks the Work until something happens. A reply without either gets one reminder, then the attempt fails. The next turn carries the same conversation on, with the events since.

The first message is built as it is for Claude Code and Codex: the playbook, its arguments and the events. Two things differ, so word a playbook for this executor: each operation is a tool of its own (`github_issue_comment`) rather than an argument to a `call` tool, and the turn's status goes in `declare_outcome` rather than in a final status message.

## Settings

| Setting | What it does |
| --- | --- |
| `base_url`, `model` | Required: the API root (requests go to `<base_url>/chat/completions`) and the model. |
| `api_key_secret` | A selected secret sent as the bearer token. |
| `headers` | Extra request headers. They can't carry credentials: use `api_key_secret`. |
| `playbook`, `args`, `plugin_dir` | The playbook a turn starts on, as for Codex. |
| `operations` | Offer only these of the granted operations as tools (`github.*` for a namespace). It grants nothing: the binding's own `operations` do. |
| `max_rounds` | The most requests one attempt makes (default 30). |
| `timeout_seconds` | One request's time limit (default 300). |
| `handoff_tokens` | When a turn's last request carried more prompt tokens than this, the next turn starts afresh from the agent's note (default 100000; 0 never does). |
| `request` | Passed through in each request's body: `temperature`, `max_tokens`, provider routing. |

The [Chat Completions executor](/open-conduit/architecture/chat-completions-executor/#binding-config) page has the detail.

## When the provider says no

A rate limit ends the attempt, and Open Conduit retries it when the provider said to come back (`Retry-After`), plus a margin. A provider outage or a timeout is an ordinary failure, retried on the runtime's retry delays (`settings.retry_delays_seconds`). A bad key or an unknown model fails at once without retrying: fix the config or the secret, then retry the Work. Nothing sleeps in the meantime, so a rate-limited provider holds no capacity.

Each run keeps the conversation as its transcript artifact, and its data records every request's model, tokens and cost, where the provider reports one.

## What it can't do

- **No shell, files or workspace.** Use Claude Code or Codex for that.
- **No sandbox or container.** There is no process to confine, so a binding with either is refused.
- **No streaming.** Each request waits for the whole reply.
