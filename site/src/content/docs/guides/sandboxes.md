---
title: Sandbox your agents
description: Run Claude Code or shell commands in a process sandbox or a Docker container, so a run reaches only its workspace, the hosts you allow and its own toolbelt.
---

Without a sandbox, an agent runs as you. It can read anything your account can read, including your SSH keys, your cloud credentials and Open Conduit's own operator token. It can reach any host. Anything it writes in your shell or editor config runs the next time you do.

A binding can instead run its executor in a sandbox. Its runs can then write only their workspace, reach only the hosts you list and call only their own toolbelt. Whatever the policy says, they never see Open Conduit's state directory, its config or another run's files. Agents don't need to know about any of this: they run `git push` or call the toolbelt as before.

You pick the sandbox per binding, so one scope can say "Claude Code, in a container" and another "a shell job, unsandboxed".

## Choose a sandbox

| | No sandbox | Process sandbox (`sandbox`, srt) | Container (`container`, Docker) |
| --- | --- | --- | --- |
| Filesystem | everything you can reach | what you can reach, minus `read.deny`; writes only to `write` | nothing from the host unless granted: the workspace, the Work's own home and what you list in `read` and `write` |
| Network | any host | only `network.allow`, through srt's proxy | only `network.allow`, through a gateway container; no route out without it |
| Open Conduit's state, config and other runs | readable | denied | not mounted |
| GitHub credential for git | yours, or the bot's with the connection's `git` `bot` | yours, if you grant it, or the bot's with the connection's `git` `bot` | none with a GitHub App connection: a token minted for the one repository and revoked when the run ends ([below](#git-credentials)) |
| Your own claude config (`~/.claude`) | shared | a config directory of its own | a home of its own for each Work |
| `work attach` | on your machine | refused | in a container of its own |
| Runs on | macOS, Linux | macOS, Linux | Linux Docker Engine, OrbStack on macOS |
| Needs | nothing | srt (Node), and bubblewrap, socat and ripgrep on Linux | Docker Engine 28 or later, and an image for the agent |

**For untrusted work, use a container.** Its filesystem is an allowlist rather than a denylist, it has its own home, attach stays inside it, and with a GitHub App connection no long-lived GitHub credential enters it. A process sandbox is lighter. It suits a machine without Docker, and work you trust but want fenced in.

To run a binding on another machine over SSH instead, see [Run agents on another machine](../remote-runners/).

Both kinds fail closed. If a policy can't be enforced, nothing runs (see [When a run is refused](#when-a-run-is-refused)).

Both kinds also need the binding's Git workspace in `clone` mode, without `setup`. A worktree commits through your checkout's own `.git`, and an agent that could write there could leave a hook that runs the next time you use git. Setup would run on the host, in files the agent wrote. A clone is a repository of its own. Its `.git/config`, `.git/hooks` and `.git/info` stay read-only to the agent, and commits on its branch are saved back to your checkout when the workspace is removed.

## Containers

Each run gets its own container, made from an image you choose and pinned by digest. A small gateway container is its only way out: to the hosts in `network.allow`, and to the run's own toolbelt. The container drops every capability, has a read-only root and a tmpfs `/tmp`, runs as your uid and gid with a process limit, and has no Docker socket. The run's network has no route to the internet, your machine's ports, the LAN or a cloud metadata service. When the run ends, Open Conduit removes the container, its gateway and its networks.

### 1. Docker

You need Docker Engine 28 or later.

- **Linux:** Docker Engine. This setup is tested in CI.
- **macOS:** [OrbStack](https://orbstack.dev). This setup is tested by hand on each change to the provider.
- **Docker Desktop** is untested. `open-conduit doctor` says so when it finds Docker Desktop. The same checks still apply, and a failed check blocks the binding.

A binding names the Docker context it uses (`docker context ls`), so changing your CLI's current context never moves its runs. The context is usually `default` on Linux and `orbstack` on macOS.

### 2. The gateway image

A released `open-conduit` binary carries the digest of the gateway image built with it. Pull the image once, using the exact command `open-conduit doctor` prints:

```sh
open-conduit doctor        # the container check names the image to pull
docker pull ghcr.io/open-conduit/open-conduit-gateway@sha256:…
```

A build from source has no pinned gateway image. Build one for your Docker and point Open Conduit at it:

```sh
make gateway-image                               # prints the image ID
export OPEN_CONDUIT_GATEWAY_IMAGE=sha256:…       # set it where the daemon runs
```

### 3. An image for the agent

The image needs `/bin/sh`, `git`, CA certificates and whatever the agent runs: `claude` for Claude Code, plus your project's toolchain. It runs as your uid with a read-only root, so install everything at build time. Open Conduit mounts its toolbelt client into every container, so the image doesn't need to include Open Conduit.

```dockerfile
FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
```

A binding takes the image only by digest, so a tag can't change under it. Push the image to a registry you use and take the digest it reports:

```sh
docker build -t registry.example.com/acme/agent .
docker push registry.example.com/acme/agent
docker inspect --format '{{index .RepoDigests 0}}' registry.example.com/acme/agent
# registry.example.com/acme/agent@sha256:…
```

If you'd rather not push anywhere, a local registry works too: `docker run -d -p 5000:5000 registry:2`, then tag and push the image as `localhost:5000/agent`.

### 4. The binding

This is the [GitHub workflow](../../workflows/github/)'s binding in a container, on a GitHub App connection:

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "github", "type": "github", "config": {"auth": "app", "app_id": "123456"},
     "secret_refs": {"app_private_key": "keyring:github-app-key"}}
  ],
  "scopes": [
    {
      "id": "web",
      "name": "acme/web",
      "secret_refs": {"claude-token": "keyring:claude-token"},
      "authorization": {
        "connections": ["github"],
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                       "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait",
                       "github.git.credential"],
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
                         "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait",
                         "github.git.credential"],
          "secret_names": ["claude-token"],
          "config": {"branch": "agent/issue-{number}", "workspace_mode": "clone",
                     "secret_env": {"claude-token": "CLAUDE_CODE_OAUTH_TOKEN"},
                     "home_seed": "/path/to/agent-home"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "clone"}},
          "container": {"provider": "docker", "config": {
            "image": "registry.example.com/acme/agent@sha256:3f1c2a9e8b7d6c5f4e3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e",
            "write": ["{workspace}"],
            "network": {"allow": ["api.anthropic.com", "github.com", "registry.npmjs.org"]},
            "limits": {"cpus": 4, "memory": "8g", "pids": 2048},
            "provider": {"context": "orbstack"}
          }}
        }
      ]
    }
  ]
}
```

The container config is exactly what runs get:

| Field | What it grants |
| --- | --- |
| `image` | The image, as `name@sha256:…`. A tag alone is refused. |
| `write` | Host paths mounted read-write, at the same path as on the host. `{workspace}` is the run's workspace. |
| `read` | Host paths mounted read-only, such as `~/.gitconfig`. Nothing else from your machine is visible. |
| `network.allow` | The only hosts a run may reach. An entry is a host (any port), `host:port` (that port only) or `*.domain` (any name under it). Everything else gets `403` from the gateway. |
| `limits` | `cpus`, `memory` and `pids`. Without `pids`, the limit is 4096. |
| `provider.context` | The Docker context every call uses. |

Paths start with `/`, `~/` or a placeholder: `{workspace}` for the workspace, or `{run}` for the run's own directory, which is always writable.

Claude Code needs `api.anthropic.com`, and git needs `github.com`. Add the package registries your project installs from. Keep the list short: the gateway doesn't inspect TLS, so an allowed host can receive anything the container can read.

A binding declares `sandbox` or `container`, never both. `config check` refuses a binding that declares both.

### Claude Code's home

Each Work gets its own home directory, which is both `HOME` and `CLAUDE_CONFIG_DIR` in its containers. Claude keeps its settings, its session transcripts and `.claude.json` there, so a Work's later turns resume the same session. No other Work and no process on your machine uses it. Open Conduit deletes it when the Work finishes.

To start each Work's home with your own `settings.json` or plugins, point `home_seed` at a directory holding them. On a Work's first run, it is copied into the empty home, leaving out any `.credentials.json`. It is a copy, never a mount, so the agent can't change your files. Use plain files rather than symbolic links: a link to a host path doesn't resolve inside the container.

### Claude's sign-in

Make a long-lived token with `claude setup-token` and store it as a secret:

```sh
open-conduit secret set claude-token
```

Then select it in the binding's `secret_names` and map it to `CLAUDE_CODE_OAUTH_TOKEN` in `secret_env`, as in the example above. An API key works the same way, mapped to `ANTHROPIC_API_KEY`. The sign-in and every other selected secret reach the container in a file that its entry point reads and deletes before claude starts. They never appear in `docker inspect`, the run's plan or its history.

### Git credentials

The agent runs `git fetch` and `git push` as usual. How git signs in depends on the GitHub connection.

**With a GitHub App connection** (`"auth": "app"`), no GitHub credential enters the container. When git needs one, its credential helper asks the run's toolbelt. Open Conduit then mints an installation token for the Work's one repository, with only `contents: write`. The bot App mints it if you've configured one. Open Conduit revokes the token when the run ends. Revoking the run's toolbelt token at the same time means the helper can't mint another. So code in the container can only push to that repository, and only while the run lasts. For this to work, `github.git.credential` must be in the binding's `operations` and the scope's `authorization.operations`. Agents never call it themselves. If it's missing, `git push` fails to authenticate.

Open Conduit keeps the tokens it has minted in memory. If the daemon restarts during a run, it can't revoke tokens minted before the restart, but they expire on their own within the hour.

To have the container's commits carry the bot's name too, set `git` to `bot` on the GitHub connection ([push and commit as the bot](../github-bot/#push-and-commit-as-the-bot)).

**With a personal access token** (`GH_TOKEN` or `GITHUB_TOKEN` in the binding's secrets), the token itself goes into the container. Anything in the container can read it, and it is valid for as long and for as many repositories as you made it. If you use one, make a fine-grained token for the one repository with only the permissions the work needs.

### Attach

`open-conduit work attach <work>` opens the Work's Claude session in a container of its own, with the same image, mounts and network policy, on your terminal. Nothing the agent wrote, such as a hook, an MCP server or `apiKeyHelper`, runs on your machine. Claude first asks whether to trust the folder and then asks you to confirm bypass-permissions mode. Answer both: pressing Enter at the first prompt picks "No, exit". When you exit, the attach container is removed.

During an attach, the container has no toolbelt. So on a GitHub App connection, git has no GitHub credential there. Let the agent push on its next turn.

### Check it

```sh
open-conduit config check
open-conduit doctor
```

Doctor's `container` check runs for each Docker context and image your bindings use. It checks that:

- the daemon is reachable and runs version 28 or later;
- a file written in a container is owned by you;
- the gateway image is present;
- a container on a run's network can reach its gateway but can't resolve outside names or reach a port on your machine.

Each failure names its fix. While any check fails, container bindings run nothing.

## Process sandboxes

A process sandbox runs the executor under Anthropic's [sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime) (`srt`), which uses Seatbelt on macOS and bubblewrap on Linux. Install it (it needs Node) and, on Linux, bubblewrap, socat and ripgrep:

```sh
npm install -g @anthropic-ai/sandbox-runtime
sudo apt-get install bubblewrap socat ripgrep   # Linux only
open-conduit doctor                       # the sandbox check runs srt once
```

On Ubuntu 24.04, AppArmor stops bubblewrap from making user namespaces. Either run `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0` or give bwrap an AppArmor profile. Doctor's `sandbox` check tells you when this is the problem.

This is the GitHub workflow's binding in a process sandbox:

```json
{
  "name": "agent",
  "executor": "claude-code",
  "connection": "github",
  "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
  "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                 "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
  "secret_names": [],
  "config": {"branch": "agent/issue-{number}", "workspace_mode": "clone",
             "env": {"CLAUDE_CONFIG_DIR": "/path/to/sandboxed-claude"}},
  "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "clone"}},
  "sandbox": {"provider": "srt", "config": {
    "write": ["{workspace}", "/path/to/sandboxed-claude", "~/.npm"],
    "read": {"deny": ["~/.ssh", "~/.aws", "~/.config/gh"]},
    "network": {"allow": ["api.anthropic.com", "github.com", "*.githubusercontent.com", "registry.npmjs.org"]},
    "sockets": ["{toolbelt}"],
    "unix_sockets": "all"
  }}
}
```

The policy is exactly what runs get:

| Field | What it grants |
| --- | --- |
| `write` | Paths a run may write. Its workspace is `{workspace}`. Claude Code also needs its config directory (below). |
| `read.deny` | Paths a run may not read. Everything else is readable, so deny your credentials. `read.allow` makes exceptions beneath a deny. |
| `network.allow` | Hosts a run may reach (`github.com`, `*.githubusercontent.com`). Everything else is refused. |
| `sockets` | Unix sockets a run may use. `{toolbelt}` is the run's own toolbelt, which the agent needs. |
| `unix_sockets` | Set it to `"all"` on Linux, which can't allow a socket by path. Any socket a run can see is then usable, so the daemon gives each run a socket of its own and hides the Docker, containerd and podman sockets. |

`{run}` names the run's own directory. Paths start with `/`, `~/` or a placeholder.

Unlike a container, a process sandbox reads broadly by default. **List your credentials in `read.deny`**: `~/.ssh`, `~/.aws`, `~/.config/gh`, and any token file or keychain export you keep.

**A sandboxed Claude Code binding needs a config directory of its own.** Anything a run writes in claude's config, such as a hook, an MCP server or a plugin, would run as you in every claude session you start. So `~/.claude` and `~/.claude.json` stay out of a sandbox's reach. Set `CLAUDE_CONFIG_DIR` in the binding's `env` to an absolute path that no claude of yours uses, create it, and grant it in `write`. `config check` refuses a sandboxed Claude Code binding without one, or with one that is `~/.claude`, contains it or is inside it.

Claude signs in separately there, since its credentials belong to the config directory (on macOS, in a Keychain entry of its own). Sign in once with `CLAUDE_CONFIG_DIR=/path/to/sandboxed-claude claude` and `/login`. Or make a token with `claude setup-token` and select it as a secret mapped to `CLAUDE_CODE_OAUTH_TOKEN` in `secret_env`.

**To push as the bot rather than as you**, set `git` to `bot` on the GitHub connection ([push and commit as the bot](../github-bot/#push-and-commit-as-the-bot)). git then signs in through the run's own toolbelt with a token minted for the one repository, so you can keep `~/.config/gh` and your other GitHub credentials in `read.deny`. Add `github.com` to `network.allow` and use an HTTPS remote.

`work attach` is refused for Work that ran in a process sandbox. On your machine, claude would load hooks and MCP servers the agent wrote into the workspace. If you want to attach to sandboxed Work, run it in a container instead.

`git push -u` in a process sandbox prints that it couldn't write the clone's config. The push still succeeds, because the clone already tracks the branch.

## When a run is refused

A sandbox that can't be enforced runs nothing. The run fails with a reason, and `open-conduit runs get ID` and the terminal UI show it:

| Reason | Meaning | Fix |
| --- | --- | --- |
| `sandbox_denied` | The run needed a path, host or socket the policy doesn't grant, which the reason names. Or the policy reached into Open Conduit's state, or the workspace holds a hard link to a file outside it. | Grant what it names, if you mean to. A policy can never grant Open Conduit's state. |
| `sandbox_unavailable` | The sandbox couldn't be set up. srt is missing or too old. The kernel won't isolate. Docker is unreachable. The gateway image is missing. A doctor check is failing. | Run `open-conduit doctor` and follow its fix. |
| `container_running` | A container from an earlier attempt of the same run is still running. | Open Conduit stops it before a retry. If it doesn't, stop the run from the terminal UI. |
| `sandbox_config`, `sandbox_workspace`, `sandbox_conflict` | `config check` refused the binding: no config directory of its own for sandboxed Claude Code, a workspace that isn't a `clone` or uses `setup`, or both `sandbox` and `container`. | The diagnostic names the field. |
| `attach_sandboxed` | You tried to attach to Work that ran in a process sandbox, or ran a host command for Work that ran in a container. | Attach with `open-conduit work attach`, which uses a container when the Work ran in one. |

Every run's history records the exact policy it ran under: for a container, its image, plan and Docker daemon. A policy you change applies from the next run, never to one that is already running.

## What a sandbox doesn't cover

- **Allowed hosts can receive what the run can read.** Neither kind of sandbox inspects TLS. A run that can reach `github.com` can send the files and secrets it can see there. Keep `network.allow` to the hosts the work needs.
- **The run's own credentials are readable inside.** That includes the Claude sign-in, any secret you select, and a static GitHub token if you use one. With a GitHub App connection, the GitHub token only reaches the one repository, and only while the run lasts.
- **The agent can change its own sign-in.** It can write its home's `settings.json`, for example to plant an `apiKeyHelper`. That only affects its own runs, inside the boundary.
- **The tools are trusted.** srt, Docker and the gateway run as you, outside the sandbox.
- **Process sandboxes deny, rather than allow.** Anything you don't list in `read.deny` is readable. On Linux, any unix socket the run can see is usable.
- **pnpm's hard links are refused.** In a process sandbox, a package manager that hard-links files from a store outside the workspace is refused. Set pnpm's `package-import-method=copy`.

For the full contract, see [process sandboxes and containers](../../architecture/sandboxes/). It covers what the host protects whatever the policy says, how a run's policy is pinned and checked on recovery, and the isolation tests that run in CI.
