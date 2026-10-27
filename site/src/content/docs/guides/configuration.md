---
title: Configuration and secrets
description: Where the config lives, what a scope holds, how to edit it and how credentials are referenced.
---

## Where the config lives

The config is one JSON file at `~/.config/open-conduit/config.json` (or `$XDG_CONFIG_HOME/open-conduit/config.json`). `open-conduit config path` prints where it is, and `--config` or `OPEN_CONDUIT_CONFIG` points elsewhere.

`open-conduit adapters [TYPE]` lists every integration and the fields it takes.

## What a scope holds

Each workflow is a **scope**. A scope always names what it may use:

- **`connections`** (top level) are the services Open Conduit talks to, such as GitHub or Slack, each with references to its credentials.
- **`authorization`** is the ceiling of connections and operations the scope may use.
- **`extensions`** are the integrations that turn what happens in a service into Events.
- **`routes`** send those Events to a binding, and choose what happens when one arrives while an attempt is running: `queue`, `inject`, `interrupt` or `drop`.
- **`bindings`** name an executor (`claude-code` or `shell`), the operations its runs may call, and optionally a `resource` such as a Git checkout to work in.

The [workflow pages](/open-conduit/workflows/github/) each show a complete, minimal example.

## Edit it

```sh
open-conduit config show
open-conduit config get /scopes/0/name
open-conduit config set /settings/webhooks '{"addr": "127.0.0.1:8787", "tunnel": "ngrok"}'
open-conduit config unset /settings/webhooks
open-conduit config edit                             # a full-screen editor
open-conduit config check                            # validate offline
```

`get`, `set` and `unset` take a JSON pointer and validate before saving. A running daemon picks up a changed file only when you restart it or run `config apply FILE --generation N`.

## Secrets

The config never holds a credential, only a reference to one:

| Reference | Reads the secret from |
| --- | --- |
| `env:NAME` | an environment variable |
| `file:NAME` | a file |
| `keyring:NAME` | the OS keyring |
| `cmd:COMMAND` | a command's output |

By default GitHub uses `cmd:gh auth token`. To store a token instead, enter it with `secret set` (it is read from stdin and never shown) and point the connection at the reference it prints:

```sh
open-conduit secret set github-token
open-conduit config set /connections/github/secret_refs/token '"keyring:github-token"'
```

Secrets go to the macOS Keychain or, on Linux, the Secret Service through `secret-tool`. Without either, `secret set` falls back to an owner-only `secrets.json` beside the config and says so.

## Webhooks

Without webhooks the daemon reconciles with each service on an interval, so nothing is lost. For lower latency, store a listener and tunnel in `settings.webhooks`, for example `{"addr": "127.0.0.1:8787", "tunnel": "ngrok"}`. Any free local port works. `serve --webhook-addr … --tunnel …` overrides it for one run.

For the full contract, see [configuration, secrets and local management](../../architecture/config-management/) and [the configuration schema](https://github.com/open-conduit/open-conduit/blob/main/docs/architecture/config.schema.json).
