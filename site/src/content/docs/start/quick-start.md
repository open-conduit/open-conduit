---
title: Quick start
description: Set up a GitHub repository and a Slack support channel, start the daemon and check it.
---

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

## What each step does

- `init TYPE` adds one scope to the config and never replaces an existing scope without `--force`. `--dry-run` prints the result instead of saving it, and `open-conduit init --help` lists every setup's flags.
- `init --with claude` instead starts Claude Code with a setup guide that walks you through the same steps.
- `config check` validates the config offline, with no daemon running.
- `service install` runs the daemon in the background and starts it at login. See [Run it in the background](/open-conduit/guides/background-service/).
- `doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs, and says what to fix.
- `tui` opens the terminal UI. See [Watch and steer runs](/open-conduit/guides/operating/).

## Try it out first

`open-conduit serve` runs the same daemon in the foreground, which is handy for watching its output. `serve --dry-run` makes no external writes and runs agents as cheap stubs, a safe way to watch a new setup before it touches anything.

## Next steps

- Pick a workflow: [GitHub engineering](/open-conduit/workflows/github/), [Slack support](/open-conduit/workflows/slack/), [scheduled jobs](/open-conduit/workflows/scheduled-jobs/) or [introspection](/open-conduit/workflows/introspection/).
- Learn how [configuration and secrets](/open-conduit/guides/configuration/) work.
