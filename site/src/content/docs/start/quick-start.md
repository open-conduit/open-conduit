---
title: Quick start
description: Let Claude Code set Open Conduit up with you, or set up a GitHub repository and a Slack support channel by hand.
---

## Set it up with Claude

The quickest start is one command:

```sh
open-conduit init --with claude
```

It opens a Claude Code session with Open Conduit's setup guide, and the two of you set it up together:

- Claude looks around first: your checkouts and their GitHub remotes, the `claude` and `gh` CLIs, and any config or daemon you already have. An existing config is built on, never replaced.
- It asks a few concrete questions: which repositories to watch, how issues map to scopes, the model and permission mode for the agent, whether to hold scheduled issues, webhooks, and how many runs to allow at once.
- It writes the config with `open-conduit config`, validates every edit, and checks the result with `open-conduit doctor`.
- Secrets never pass through the chat. Credentials go in the config as references, such as `cmd:gh auth token` or a `keyring:` entry you store yourself with `open-conduit secret set`.
- It ends with the commands to start the daemon, which you run yourself: `serve --dry-run` to watch safely first, then `service install` to run it in the background.

Claude Code must be installed and signed in. The session can run only `open-conduit config`, `doctor`, `health`, `secret path` and `init github --dry-run` without asking you. `--config PATH` sets up a config other than the default.

## Set it up by hand

Set up a GitHub repository and a Slack support channel, start the daemon, and check it:

```sh
open-conduit init                                    # list the available setups
open-conduit init github --repo acme/web --checkout ~/Code/web
open-conduit init slack                              # creates the Slack app, asks for its tokens, picks a channel
open-conduit config check                            # validate the config offline
open-conduit service install                         # run the daemon in the background
open-conduit doctor --check-secrets                  # every connection, credential and binding
open-conduit tui                                     # watch and control runs
```

### What each step does

- `init TYPE` adds one scope to the config and never replaces an existing scope without `--force`. `--dry-run` prints the result instead of saving it, and `open-conduit init --help` lists every setup's flags.
- `config check` validates the config offline, with no daemon running.
- `service install` runs the daemon in the background and starts it at login. See [Run it in the background](/open-conduit/guides/background-service/).
- `doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs, and says what to fix.
- `tui` opens the terminal UI. See [Watch and steer runs](/open-conduit/guides/operating/).

## Try it out first

`open-conduit serve` runs the same daemon in the foreground, which is handy for watching its output. `serve --dry-run` makes no external writes and runs agents as cheap stubs, a safe way to watch a new setup before it touches anything.

## Next steps

- Pick a workflow: [GitHub engineering](/open-conduit/workflows/github/), [Slack support](/open-conduit/workflows/slack/), [scheduled jobs](/open-conduit/workflows/scheduled-jobs/) or [introspection](/open-conduit/workflows/introspection/).
- Learn how [configuration and secrets](/open-conduit/guides/configuration/) work.
