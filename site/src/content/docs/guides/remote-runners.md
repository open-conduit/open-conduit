---
title: Run agents on another machine
description: Run a binding's executions on a remote machine over SSH, so heavy builds and untrusted repositories stay off the machine Open Conduit runs on.
---

A binding can run its executor on another machine instead of the one Open Conduit runs on. Use one to give builds a bigger machine, to keep agents off a laptop, or to work an untrusted repository on a machine you can throw away.

Open Conduit stays where it is. For each run it copies the run's workspace and its own files to the remote over SSH, runs the executor there, and copies back what the run changed. The run reaches its toolbelt over the same SSH connection, so every call is still checked by Open Conduit on your machine. Agents don't need to know: they run `git push` and call the toolbelt as before.

The remote is a [container provider](../sandboxes/#containers), `ssh`, so it works with the shell and Claude Code executors and needs the same `clone` workspace a container does.

## What the remote gets

| | |
| --- | --- |
| Files | the run's workspace, its run directory, the Work's own home, and what you list in `read` and `write`, at the same paths as on your machine; nothing else of yours |
| Back on your machine | what the run changed under `write`, the run directory and the Work's home; never `.git/config`, `.git/hooks` or `.git/info` |
| Credentials | the run's toolbelt token, the secrets the binding selects (such as Claude's sign-in), and through the toolbelt a GitHub token for the Work's one repository, revoked when the run ends; never your SSH key or Open Conduit's own token |
| Network | whatever the remote's own firewall allows |
| Machine | pinned by its host key: a different key refuses every run |

**Open Conduit can't limit the remote's network or its resources.** It still checks the hosts an executor says it needs against `network.allow`, but nothing on the remote enforces that list. Give the remote a firewall that does, and use a machine whose size is the limit.

## Set up the remote

This walks through a Linux machine, `runner.example.com`, with an account `agent` for the runs.

### 1. The account and its tools

On the remote:

```sh
sudo useradd --create-home --shell /bin/bash agent
```

The account needs `sh`, `setsid`, `ps`, `tar`, `find` and `env`, which any Linux distribution has, and whatever the binding runs. For Claude Code, install `claude` where the account's `PATH` finds it, such as `/usr/local/bin`.

For the toolbelt, the remote needs Open Conduit's `os` on `/usr/local/bin:/usr/bin:/bin`. Take it from the Linux release archive for the remote's architecture:

```sh
version=1.14.0   # what `open-conduit version` prints on your machine
arch=amd64       # or arm64
curl -fsSLO "https://github.com/open-conduit/open-conduit/releases/download/v$version/open-conduit_${version}_linux_$arch.tar.gz"
tar -xzf "open-conduit_${version}_linux_$arch.tar.gz" os
sudo install -m 0755 os /usr/local/bin/os
```

### 2. Your machine's paths

A run sees the same paths on the remote as on your machine, so nothing inside it needs rewriting. The account must be able to write Open Conduit's state directory's path there. With the default state directory, on a Mac whose user is `alex`:

```sh
sudo mkdir -p /Users/alex/.local/state/open-conduit
sudo chown -R agent: /Users/alex
```

Doctor names the path if it isn't writable.

### 3. A key for Open Conduit

On your machine, make a key only Open Conduit uses, and authorise it for the account:

```sh
ssh-keygen -t ed25519 -N '' -f ~/.ssh/open-conduit
ssh-copy-id -i ~/.ssh/open-conduit.pub agent@runner.example.com
```

Open Conduit forwards the run's toolbelt socket over the connection, so the remote's `sshd` must allow `AllowStreamLocalForwarding`, as it does by default.

### 4. The remote's host key

Open Conduit trusts one host key for the machine and no other. Read it from the remote itself, not over the network you are trying to trust:

```sh
cat /etc/ssh/ssh_host_ed25519_key.pub
# ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI… root@runner
```

Keep the first two fields, `ssh-ed25519 AAAA…`.

## The binding

A remote binding declares a `container` with the `ssh` provider:

```json
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
               "secret_env": {"claude-token": "CLAUDE_CODE_OAUTH_TOKEN"}},
    "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "clone"}},
    "container": {"provider": "ssh", "config": {
      "image": "runner.example.com@sha256:…",
      "write": ["{workspace}"],
      "network": {"allow": ["api.anthropic.com", "github.com"]},
      "provider": {
        "host": "runner.example.com",
        "user": "agent",
        "host_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI…",
        "identity": "~/.ssh/open-conduit",
        "network": "host"
      }
    }}
  }
]
```

| Field | What it does |
| --- | --- |
| `image` | Pins the machine: `<host>@sha256:` and the SHA-256 of its host key. Leave it as above and run `open-conduit config check`, which gives the value to set. |
| `write` | Paths copied to the remote and back. `{workspace}` is the run's workspace. The run directory and the Work's home always are. |
| `read` | Paths only copied to the remote, such as `~/.gitconfig`. Each run copies them again, so keep them small. |
| `network.allow` | The hosts the executor may say it needs. It isn't enforced on the remote. |
| `provider.host`, `user`, `port` | Where to connect (port 22 unless you set one). |
| `provider.host_key` | The remote's host key, `<type> <base64>`. |
| `provider.identity` | The key to sign in with. Without it, ssh uses your agent and `~/.ssh/config`. |
| `provider.network` | Must be `host`: you accept that the remote's own network applies. |

Leave `limits` out: the provider can't enforce them and refuses a config with them. Your `~/.ssh/config` still applies, so a `ProxyJump` to reach the remote works. Nothing there can change the host key Open Conduit checks.

## Check it

```sh
open-conduit config check
open-conduit doctor
```

Doctor's `container` check connects to each remote with its pinned key. It checks that the remote has the tools a run needs and can write the run's records and your state directory's path. It also says whether `os` is on the remote's `PATH`. While a check fails, the remote's bindings run nothing.

## Stopping and restarts

Stopping a run from the terminal UI, an interrupt and a run's deadline reach the run on the remote. If the SSH connection drops while a run is going, Open Conduit stops the run on the remote, copies back what it changed and fails the run. A daemon restart doesn't end a run whose helper is still going. A run that ended while nothing was watching is reported with its exit status. What it changed stays on the remote until the Work's next run replaces it.

## What it doesn't cover

- **Every run copies its files again.** A clone workspace is fine. A large `read` grant makes every run slow.
- **No cloud machines yet.** Open Conduit doesn't create or destroy the remote; point it at a machine you manage.
- **The remote is trusted with what it gets.** Anyone with access to the account can read a running run's files, its toolbelt token and its secrets. Give each binding an account of its own, on a machine nobody else uses.

For how it works, see [the SSH provider](../../architecture/sandboxes/#the-ssh-provider).
