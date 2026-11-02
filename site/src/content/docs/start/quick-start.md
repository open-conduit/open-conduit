---
title: Quick start
description: See how Open Conduit routes work without any credentials, then let Claude Code set it up with you, talk it through in the browser, or set up a GitHub repository and a Slack support channel by hand.
---

## Look around without credentials

You can see how Open Conduit routes work before you give it any GitHub access. Set up a repository that doesn't need to exist, start the daemon in dry-run mode, and ask it what it would do with a sample event. Everything lives in `/tmp/oc-demo`, away from your own config and state:

```sh
mkdir -p /tmp/oc-demo/repo && git -C /tmp/oc-demo/repo init -q
open-conduit init github --repo acme/web --checkout /tmp/oc-demo/repo --config /tmp/oc-demo/config.json
open-conduit --data-dir /tmp/oc-demo/state serve --config /tmp/oc-demo/config.json --dry-run
```

Leave that running, and in a second terminal:

```sh
open-conduit --data-dir /tmp/oc-demo/state route --samples --scope web             # the sample events this scope's routes offer
open-conduit --data-dir /tmp/oc-demo/state route --trace issue-opened --scope web  # what a newly opened issue would do
```

The trace shows each route's verdict and the effect, and records nothing:

```json
{
  "scope_id": "web",
  "generation": "1",
  "routes": [
    { "route": "issues", "handler": "github", "binding": "agent", "verdict": "match", "reason": "an open issue the labels admit" }
  ],
  "effect": { "kind": "start", "binding": "agent", "detail": "starts new github.issue Work owning issue/1 on agent" }
}
```

Try `--trace comment` too: a comment on an issue that nobody is working on starts nothing, because only an opened issue starts Work. `serve --dry-run` makes no external writes, so nothing reaches GitHub. When you're done, stop the daemon and delete `/tmp/oc-demo`.

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

Using another coding agent, or one you already have open? Give it [Point your agent here](/open-conduit/start/agents/), which installs Open Conduit and follows the same guide.

Claude Code must be installed and signed in. The session can run only `open-conduit config`, `doctor`, `health`, `secret path` and `init github --dry-run` without asking you. `--config PATH` sets up a config other than the default.

## Set it up in the browser

The web view has a **Setup** page where you start a session with a setup assistant and talk to it in the browser. It needs a scope with a `setup` entry:

- **A fresh install has one already.** With no config file and nothing stored yet, the daemon (`open-conduit serve`, or the service) starts on a default `setup` scope as generation 1. It doesn't write the file; the first `config apply` does.
- **With a config, add it** with `open-conduit init setup` (`--scope ID` names it, `setup` by default; `--dry-run` prints it instead). Before the daemon has ever run, that's all. Once it has, it keeps running the configuration it last applied, so make the file the next generation: `open-conduit config validate FILE` prints the current generation, then `open-conduit config apply FILE --generation N`. `open-conduit config path` prints the file's path.

Then run `open-conduit web`, open the Setup page and start a session, choosing the model. You can also give that one session a shell, so it can look around your machine itself, or your own MCP servers and claude.ai connectors; both are off by default, and it still proposes config changes for you to apply. `open-conduit actions run setup start --scope setup --input "opus +shell"` starts one from a terminal.

**What a session can do.** The assistant has no shell and no file access. It looks around through named probes (whether Claude Code is signed in, your folders, your `gh` logins and repositories, what `init` would write) and never sees a secret. It never edits the config either: it proposes each change with its reason, and the page shows you each one's before and after. Approve or reject them one at a time, or approve all, validate, then apply. Apply is yours alone; the assistant hears when you have, and checks the result with you. Credentials still go in from your own terminal with `open-conduit secret set NAME`; the assistant proposes only the reference.

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

`open-conduit serve` runs the same daemon in the foreground, which is handy for watching its output. `serve --dry-run` makes no external writes and runs agents as cheap stubs, a safe way to watch a new setup before it touches anything. `route --trace` (see [Look around without credentials](#look-around-without-credentials)) shows how an event would route, without recording it.

## Next steps

- Pick a workflow: [GitHub engineering](/open-conduit/workflows/github/), [Slack support](/open-conduit/workflows/slack/), [scheduled jobs](/open-conduit/workflows/scheduled-jobs/) or [introspection](/open-conduit/workflows/introspection/).
- Learn how [configuration and secrets](/open-conduit/guides/configuration/) work.
