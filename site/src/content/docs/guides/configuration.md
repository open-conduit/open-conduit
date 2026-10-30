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
- **`bindings`** name an executor (`claude-code` or `shell`), the operations its runs may call, optionally a `resource` such as a Git checkout to work in, and optionally a `sandbox` that limits what its runs can reach.

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

`get`, `set` and `unset` take a JSON pointer and validate before saving. A running daemon picks up a changed file only when you run `config apply FILE --generation N`. Restarting does not apply it: the daemon resumes its active stored generation and logs a warning at startup when the file differs.

## Config in a working folder

A folder can hold everything one workflow needs: its files, the agent's instructions (`CLAUDE.md`, `AGENTS.md`), its skills (`.claude/skills`) and its Open Conduit scopes in `.open-conduit/config.json`. The main config includes the folder beneath one of its own scopes:

```sh
cd ~/Code/web
open-conduit config include add --parent personal
```

The folder's file holds only scopes, in the same shape as the main config's:

```json
{
  "version": 1,
  "scopes": [{
    "id": "web", "name": "Web",
    "bindings": [{"name": "agent", "executor": "claude-code", "requires": {"toolbelt": "mcp"}, "operations": ["emit", "read_context", "declare_outcome"], "secret_names": [], "config": {}}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "agent", "active_policy": "queue", "config": {}}]
  }]
}
```

- **Beneath the parent.** A scope without a `parent` sits under the include's parent. It inherits that scope's authorization and secrets, and can only narrow them.
- **No credentials.** A folder can't define connections, settings or secret references. It selects the secret names its parent already has, so a cloned repository can't run a `cmd:` reference of its own.
- **Runs in the folder.** A `claude-code` binding without a resource or `dir` runs in the folder, and a relative `dir` or `plugin_dir` is resolved against it.
- **Safe to delete.** If the folder or its file goes, the configuration still applies. Its scopes stay as last applied, but disabled, until the folder is back. `config check` warns about it. To drop them for good, run `open-conduit config include remove FOLDER`.

As with the main file, edits to a folder's config take effect on `config apply`.

## Sandboxes

Without a sandbox, an agent runs as you: it can read anything your account can, including Open Conduit's own operator token, and reach any host. A binding's `sandbox` runs its executor inside Anthropic's [sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime) (`srt`), so a run can write only its workspace and the paths you grant, reach only the hosts you list, and never read Open Conduit's state directory or config, whatever you grant.

Install srt (it needs Node) and, on Linux, bubblewrap, socat and ripgrep:

```sh
npm install -g @anthropic-ai/sandbox-runtime
sudo apt-get install bubblewrap socat ripgrep   # Linux only
open-conduit doctor                       # the sandbox check runs srt once
```

On Ubuntu 24.04, AppArmor stops bubblewrap making user namespaces: `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`, or give bwrap an AppArmor profile. Doctor's `sandbox` check says when that's the problem.

A sandboxed binding takes its Git workspace in `clone` mode, a clone of its own rather than a worktree of your checkout, and can't use `setup`, which would run outside the sandbox. This is the GitHub workflow's binding, sandboxed:

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "cmd:gh auth token"}}
  ],
  "scopes": [
    {
      "id": "web",
      "name": "acme/web",
      "secret_refs": {},
      "authorization": {
        "connections": ["github"],
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                       "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web"}}
      ],
      "routes": [
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "agent",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
          "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                         "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
          "secret_names": [],
          "config": {"branch": "agent/issue-{number}", "workspace_mode": "clone"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "clone"}},
          "sandbox": {"provider": "srt", "config": {
            "write": ["{workspace}", "~/.claude", "~/.claude.json", "~/.npm"],
            "read": {"deny": ["~/.ssh", "~/.aws", "~/.config/gh"]},
            "network": {"allow": ["api.anthropic.com", "github.com", "*.githubusercontent.com", "registry.npmjs.org"]},
            "sockets": ["{toolbelt}"],
            "unix_sockets": "all"
          }}
        }
      ]
    }
  ]
}
```

The policy is exactly what runs get:

| Field | What it grants |
| --- | --- |
| `write` | Paths a run may write. Its workspace is `{workspace}`; Claude Code also needs `~/.claude` and `~/.claude.json`. |
| `read.deny` | Paths a run may not read. Everything else is readable, so deny your credentials. `read.allow` makes exceptions beneath a deny. |
| `network.allow` | Hosts a run may reach (`github.com`, `*.githubusercontent.com`). Everything else is refused. |
| `sockets` | Unix sockets a run may use. `{toolbelt}` is the run's own toolbelt, which the agent needs. |
| `unix_sockets` | `"all"` on Linux, which can't allow a socket by path: any socket a run can see is then usable, so the daemon gives each run a socket of its own. |

`{run}` names the run's own directory. Paths start with `/`, `~/` or one of the placeholders.

If the sandbox can't be enforced, nothing runs: a missing srt, a run that needs a path or host you didn't grant (`sandbox_denied`, naming it), or a kernel that won't isolate (`sandbox_unavailable`). The run's history shows the policy each run had. A changed policy applies from the next run, never to one already running. `git push -u` in a sandbox prints that it couldn't write the clone's config; the push succeeds, because the clone already tracks the branch.

The [sandbox architecture page](../../architecture/sandboxes/) has the full contract.

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
