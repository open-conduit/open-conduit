---
title: Quick start
description: Let Claude Code set Open Conduit up with you, talk it through in the browser, or set up a GitHub repository and a Slack support channel by hand.
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

## Set it up in the browser

The web view has a **Setup** page where you start a session with a setup assistant and talk to it in the browser. It needs a scope with a `setup` entry:

- **A fresh install has one already.** With no config file and nothing stored yet, the daemon (`open-conduit serve`, or the service) starts on a default `setup` scope as generation 1. It doesn't write the file; the first `config apply` does.
- **With a config, add it** with `open-conduit init setup` (`--scope ID` names it, `setup` by default; `--dry-run` prints it instead). Before the daemon has ever run, that's all. Once it has, it keeps running the configuration it last applied, so make the file the next generation: `open-conduit config validate FILE` prints the current generation, then `open-conduit config apply FILE --generation N`. `open-conduit config path` prints the file's path.

Then run `open-conduit web`, open the Setup page and start a session, choosing the model. `open-conduit actions run setup start --scope setup --input opus` starts one from a terminal.

**What a session can do in this release.** The assistant has no shell and no file access, and proposing config changes for you to approve isn't built yet. It can explain Open Conduit, ask you the setup questions and plan the setup with you, but it can't look at your checkouts or logins or change the config. It ends with the commands to run yourself: `init --with claude` above, or the `init` commands below.

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
