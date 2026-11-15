---
title: Use models from other providers
description: Point a Claude Code or Codex binding at OpenRouter, LiteLLM or Ollama with its provider setting, and what changes when you do.
---

A Claude Code or Codex binding sends its turns to Anthropic or OpenAI by default. Give it a `provider` and it sends them to another endpoint instead: a gateway such as OpenRouter, a proxy you run such as LiteLLM, or a local model server such as Ollama. The agent works the same way either way. It gets the same toolbelt, playbook and turns, and doesn't need to know which model it's running on.

The executor sets everything the CLI needs from `provider`: the endpoint, the key and, for a sandboxed binding, the host it may reach. Don't set `ANTHROPIC_BASE_URL`, `-c model_provider=…` or similar in `env` or `extra_args` as well. A binding with a `provider` refuses them.

## Store the key

Most providers need a key. Store it as a secret and select it on the binding, as for any secret ([configuration and secrets](../configuration/)):

```sh
open-conduit secret set openrouter
```

The binding lists it in `secret_names`, and the scope's `secret_refs` names it. The `provider` then says which selected secret is the key. A local server that needs no key needs no secret.

## Claude Code

Claude Code speaks Anthropic's Messages API, so the endpoint must serve that API. OpenRouter, LiteLLM and Ollama all do.

| Setting | What it does |
| --- | --- |
| `provider.base_url` | The endpoint, set as `ANTHROPIC_BASE_URL`. Required. |
| `provider.auth_token_secret` | The selected secret sent as the bearer token (`ANTHROPIC_AUTH_TOKEN`). Leave it out for an endpoint that needs none. |

Set `model` (and any `models`) to the provider's model IDs. Claude Code's own aliases, such as `opus`, mean Anthropic's models.

### OpenRouter

```json
"secret_names": ["openrouter"],
"config": {
  "playbook": "work-issue",
  "model": "anthropic/claude-opus-5-5",
  "provider": { "base_url": "https://openrouter.ai/api", "auth_token_secret": "openrouter" }
}
```

OpenRouter's [Claude Code guide](https://openrouter.ai/docs/guides/guides/coding-agents) lists the models that work this way.

### LiteLLM

Run the LiteLLM proxy with the models you want in its config, then point the binding at it. The key is the proxy's own (its master key or a virtual key), not the upstream provider's:

```json
"secret_names": ["litellm"],
"config": {
  "playbook": "work-issue",
  "model": "my-model",
  "provider": { "base_url": "http://localhost:4000", "auth_token_secret": "litellm" }
}
```

LiteLLM translates to providers that don't speak Anthropic's API, so this is the route to most other models.

### Ollama

Ollama serves Anthropic's API on its usual port and needs no key:

```json
"config": {
  "playbook": "work-issue",
  "model": "qwen3-coder",
  "provider": { "base_url": "http://localhost:11434" }
}
```

Use a model with a context window large enough for a coding agent. A small one runs out partway through a turn.

### What changes

- **Your Claude login isn't used.** Claude Code sends the provider's key, or a placeholder when there is none, so neither your Claude login nor an API key reaches the provider.
- **Usage windows don't apply.** The 5-hour and 7-day windows belong to a Claude subscription. Doctor says usage doesn't apply to a binding with a `provider` and reads nothing, and `config check` warns if the binding sets `usage`. The provider's own dashboard shows what you've used.
- **Sandboxes reach the provider.** A sandboxed or container binding asks for the provider's host, including the port when `base_url` names one, instead of `api.anthropic.com`. Add that host to the sandbox's `network.allow` ([sandbox your agents](../sandboxes/)). From inside a container, `localhost` is the container itself, so a container binding needs a server it can reach by name.

## Codex

Codex speaks only OpenAI's Responses API. It dropped the Chat Completions protocol in early 2026 ([the deprecation](https://github.com/openai/codex/discussions/7782)). A provider that offers only Chat Completions needs a proxy that translates, such as LiteLLM, in front of it.

| Setting | What it does |
| --- | --- |
| `provider.id` | The provider's name in Codex's config: lower-case letters, digits, dashes and underscores. It can't be one Codex builds in (`openai`, `ollama`, `lmstudio`), because Codex won't let config replace those. |
| `provider.base_url` | The API's base, such as `https://openrouter.ai/api/v1`, not an endpoint under it. |
| `provider.api_key_secret` | The selected secret Codex sends as its key. Leave it out for an endpoint that needs none. |
| `provider.wire_api` | `responses`, the default and the only value Codex accepts. |

The executor writes Codex's `model_provider` and `model_providers.<id>` overrides itself, so `extra_args` may not set them. It refuses a setup it can tell won't work: `wire_api` set to `chat`, a `base_url` ending in `/chat/completions`, or `extra_args` that set a provider's `wire_api` to `chat`. It can't tell whether a server supports the Responses API from its URL alone, so a server that doesn't fails on the first turn.

### LiteLLM

LiteLLM serves the Responses API for any model it proxies, so it is the dependable route to a Chat-only provider:

```json
"secret_names": ["litellm"],
"config": {
  "playbook": "work-issue",
  "model": "my-model",
  "provider": { "id": "litellm", "base_url": "http://localhost:4000/v1", "api_key_secret": "litellm" }
}
```

### OpenRouter

```json
"secret_names": ["openrouter"],
"config": {
  "playbook": "work-issue",
  "model": "openai/gpt-5",
  "provider": { "id": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key_secret": "openrouter" }
}
```

OpenRouter's Responses API is newer than its Chat Completions API. Try a turn before relying on it, and put LiteLLM in front if Codex can't use it.

### Ollama

Codex has an Ollama provider of its own, but its settings can't be changed from config. To point a binding at an Ollama server, give it another `id`:

```json
"config": {
  "playbook": "work-issue",
  "model": "gpt-oss:20b",
  "provider": { "id": "local", "base_url": "http://localhost:11434/v1" }
}
```

An Ollama release without the Responses API needs LiteLLM in front, as for any Chat-only server.

The Codex executor runs without a process sandbox or container, so there is no network allowance to change ([run agents on Codex](../codex/)).
