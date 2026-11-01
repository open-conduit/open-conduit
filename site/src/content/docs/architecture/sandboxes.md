---
title: "Process sandboxes and the srt provider"
description: "Decision B09: how a binding runs its executor in a process sandbox, what the host protects whatever the policy says, how a plan is pinned and proved on recovery, and how the srt provider enforces it on macOS and Linux. Also the container contract, with its host-owned lifecycle, and the Docker provider with its gateway."
---

:::tip
To set up a sandbox, see [Sandbox your agents](../../guides/sandboxes/). This page is the design record.
:::

This page records decision B09: a binding may run its executor's process in a local process sandbox. The resource says where the agent works (B07), the executor how it runs, and the sandbox what it may reach. The public contract is [`extension/sandbox.go`](https://github.com/open-conduit/open-conduit/blob/main/extension/sandbox.go), and the host's half of it is [`extension/sandboxbind.go`](https://github.com/open-conduit/open-conduit/blob/main/extension/sandboxbind.go). The [srt provider](https://github.com/open-conduit/open-conduit/tree/main/extensions/srt) implements it with Anthropic's sandbox-runtime. The [shell](../shell-executor/) and [Claude Code](../claude-code-executor/) executors consume it, and [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/sandbox.go) composes them.

A sandbox here is local and process-scoped: the sandbox is the process tree the executor starts. Containers (Docker, `sbx`) and remote sandboxes are not providers of this contract. They fail it on stopping, adoption, the stray-process sweep, host sockets and credentials (#161). Containers have a contract of their own, with a lifecycle the host owns ([Containers](#containers)), which shares this one's rules and path checks.

## What core knows

Core knows nothing about sandboxes. Its only involvement is one generic hook, `SchedulerOptions.Sandbox`, called after `Resources` and immediately before `Run`, `Recover` or `Resume`, outside coordination transactions. The host returns the attempt's `extension.SandboxBroker` (nil when the binding declares no sandbox) and the resource broker the executor gets in its place. Core puts the broker in `ExecutionContext.Sandbox`. A binding failure fails the attempt as `sandbox unavailable` (`ErrSandboxUnavailable`) before any executor call. The hook also gets a `pin` function, which records a plan's digest and summary with the attempt's open segment (`Segment.Sandbox`), fenced by the lease. Core stores those two strings and never reads them. A recovered segment carries the pin of the process it adopts.

## The contract

| Type | Role |
| --- | --- |
| `SandboxNeeds` | `Read`, `Write`, `ReadOnly`, `Deny`, `Hosts`, `Sockets`. Where paths overlap, the most specific decides. `ReadOnly` is readable but never writable, even beneath a write grant. `Deny` is neither, whatever the policy grants. |
| `SandboxLaunch` | What the executor would run: the `Argv` its `/bin/sh` wrapper runs as `"$@"`, the `Dir`, the child's `Env`, the segment's `RunDir`, and `WrapperEnv`, overrides for the sandbox tool's own environment that never reach the child. |
| `SandboxPolicy` | The binding's opaque sandbox `Config` and this attempt's `Paths`: `Workspace`, `Run`, `Toolbelt` (the socket) and `Records`, a host-owned directory for the provider's files. The host sets them, never the executor. |
| `SandboxPlan` | The wrapped `Argv` and `Env`, `Signals` (`term` or `kill`), a one-line `Summary` and `Data`, the effective policy. `Digest` is the host's: `sha256` of the argv and `Data`. |
| `SandboxProvider` | Implemented by an extension: `Plan(owner, policy, needs, launch)` and `Adopt(plan, parent, root)`. |
| `SandboxBroker` | What an executor sees: `Plan(needs, launch)` and `Adopt(parent)`. `BindSandbox` makes it. |
| `Resource.Sandbox`, `ResourceRequest.Sandboxed` | A resource's needs, nil when it can't be used in a sandbox; and the host's mark on an acquisition for a sandboxed binding. |

An executor makes two calls, and nothing else in it is about sandboxes:

- `extension.WrapLaunch(ctx, broker, needs, launch)` where it builds the argv for its wrapper. With no broker it returns the launch unchanged. An error ends the segment as `sandbox_denied` or `sandbox_unavailable` (`extension.SandboxReason`) with no process.
- `extension.AdoptSandboxed(ctx, broker, pid)` in `Recover`, before it adopts a live segment. An error is `ErrNotAdoptable`.

The wrapper, its pid stamp, exit file and `ps` marker stay on the host side: only `"$@"` is wrapped. The sandboxed command is still in the executor's process group, so `Stop`, `Interrupt` and `Inject` work unchanged. An executor that implements it advertises `Capabilities.Sandbox`; shell and claude-code do.

## Invariants

### 1. The operator's policy is the grant

The binding's sandbox config is exactly what a sandboxed process is granted. The executor's needs (claude-code's config directory and `api.anthropic.com`), the resource's and the host's are only checked against it. A need outside it fails the launch as `sandbox_denied` and nothing runs. The network is denied unless the policy lists a host. Placeholders name this attempt's paths without the operator knowing them: `{workspace}` is the launch's working directory, `{run}` the segment's run directory, `{toolbelt}` its toolbelt socket. The one grant the host makes itself is the segment's own run directory, which holds only this attempt's files.

### 2. The host merges needs from three places

`BindSandbox` merges the executor's needs with the acquired resource's (`Resource.Sandbox`, kept by wrapping the attempt's resource broker) and the host's protected set, then hands the provider the result.

The protected set is not operator config, and a policy can't lift it:

- **Denied:** the state directory (the operator token, `management.sock`, the database, every other execution's run directory and workspace), the fallback toolbelt socket root (`/tmp/open-conduit-<uid>`) and the config file.
- **The only exceptions:** this attempt's run directory and workspace (read and write), and, read-only, the execution's toolbelt directory (its token and socket) and the published playbooks.

The host refuses, as `sandbox_denied`, an executor need beneath the protected set outside those exceptions, a socket there other than the execution's toolbelt, and a run directory or workspace that would contain a protected path. The provider refuses a policy whose write grant is a protected path, contains one with something writable beneath it (the state directory, which holds the workspace), or reaches into one, or whose read exception reaches into one, other than at those exceptions. A protected path inside a write grant with nothing writable beneath it (the fallback socket root under a granted `/tmp`) is denied inside the grant.

### 3. Fail closed

A declared sandbox that can't be enforced runs nothing. Each of these ends the segment with a structured failure and no process:

- no provider, or an executor without `Capabilities.Sandbox` (refused at config load as well)
- a missing `srt`, an older one than the provider knows, or `bwrap`, `socat` or `rg` missing on Linux
- an unsupported platform
- a failed preflight: `srt` runs `/usr/bin/true` under the real policy before every launch, so a kernel that won't make namespaces (Ubuntu 24.04's AppArmor restriction on unprivileged user namespaces) fails here
- srt degrading: a preflight that warns (seccomp unavailable leaves unix sockets open) is refused
- a policy srt can't express (below)
- a resource without sandbox needs, a launch outside the acquired workspace, and a git workspace in `work`, `fresh` or `shared` mode or with `setup` (refused at config load, and refused by the provider before it makes or runs anything)

### 4. Pinned per attempt

Before a plan reaches the executor, the host records it in the execution's record directory (`<state>/runs/<execution>/sandbox/<segment>/plan.json`, read-only and outside every grant) and pins its digest with the segment. History shows each segment's pin, so it shows the policy each segment ran under. The binding's sandbox is read from the current config at each launch, so a policy change applies from the next segment, which is a new launch with a new digest. The provider never changes a running process's policy: srt's `--control-fd` is never used.

### 5. Signals

A plan declares what a stop can deliver. srt on macOS runs the command in the executor's process group, so `term` reaches it and its handler runs. On Linux bubblewrap starts it in a new session, so it is `kill`: the executor's TERM stops srt, whose sandbox dies with it. The executor keeps the plan's `Signals` with the segment, and an interrupted segment's result says which it got (`interrupt: term` or `kill`, and "killed: its sandbox delivers no TERM" in the summary).

## Recovery proves the boundary

Matching the record's digest is not enough. The root stub the host puts in front of every plan (`sh -c 'printf $$ > root; exec "$@"'`) records the sandbox root's pid as it starts, in the record directory the sandbox can't write. On `Recover` the host's broker:

1. refuses a segment launched without a sandbox when its binding now declares one, and any pin it has no record for;
2. reads the pinned record and checks it still hashes to the pin;
3. asks the provider to prove the process tree.

srt's `Adopt` proves that:

- the executor's wrapper (`parent`) runs nothing but the recorded root;
- the root is the wrapper's child and runs srt with this plan's policy file;
- it started no earlier than the plan was recorded, so a reused pid can't stand in for it;
- on Linux, a process under it is in mount, network, pid and user namespaces that all differ from the host's.

A root that has exited, or a wrapper that runs nothing yet, leaves nothing running outside a sandbox and is adopted. Anything else is `ErrNotAdoptable`, never adoption. On macOS the proof is the ancestry and the start time: Seatbelt is applied before the command runs and is inherited by everything it starts. Calling `sandbox_check` would need cgo, which the release builds don't use.

## The filesystem policy survives symlinks, aliases and mounts

- **Canonical paths.** The host and the provider resolve every path through `extension.CanonicalPath` (symbolic links in its longest existing prefix, so `/var` is `/private/var` on macOS) before granting or checking it. Every path srt receives is marked literal, so a directory name never reads as a pattern.
- **Aliases.** On Linux the host adds every bind mount of a protected path (from `/proc/self/mountinfo`) to the denies. Seatbelt and bubblewrap resolve symlinks, `..` and, on macOS, case variants on case-insensitive APFS to the real path, so a link in the workspace to `~/.ssh` or the state directory, `..` out of the workspace and `/proc/self/root/…` all land on the deny.
- **Hard links.** A hard link is the same file under another path, and no path policy can tell them apart. The host refuses (`sandbox_denied`) a workspace that holds a hard link to a file outside it, found by walking the workspace for files whose link count it can't account for (up to 1,000,000 entries; a larger workspace is refused). A process inside can't make a new one to a denied file.
- **The run directory.** It must be beneath the executor's runs directory and reached without symbolic links, so a linked run directory can't stand for the state directory.

These checks, with the protected-set checks of invariant 2, are one host function, `extension.GuardLaunch` ([`launchguard.go`](https://github.com/open-conduit/open-conduit/blob/main/extension/launchguard.go)), which both the sandbox broker and the [container broker](#containers) call.

## The srt provider

`srt` (Anthropic's [sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime), version 0.0.81 or later) is Seatbelt on macOS and bubblewrap with a seccomp filter on Linux, with an allowlisting HTTP and SOCKS proxy on both. It needs Node, and on Linux `bwrap`, `socat` and `rg` (ripgrep). The provider writes srt's settings into the plan's record directory and wraps the launch in `env PATH=… [WrapperEnv] srt --settings <file> -- env <the child's values for those keys> <argv>`. The child's environment stays the process environment, so a secret never enters an argv or a file.

### The policy

```json
{
  "write": ["{workspace}", "/srv/open-conduit/claude"],
  "read": {"deny": ["~/.ssh", "~/.aws"], "allow": []},
  "network": {"allow": ["api.anthropic.com", "github.com", "*.githubusercontent.com"]},
  "sockets": ["{toolbelt}"],
  "unix_sockets": "all"
}
```

| Field | Meaning |
| --- | --- |
| `write` | Paths the process may write and read. The run directory is always writable. |
| `read.deny`, `read.allow` | Paths it may not read (srt reads broadly by default), and exceptions beneath them. |
| `network.allow` | Host names or `*.domain` it may reach through srt's proxy, with `strictAllowlist`. Everything else is refused before it is resolved. |
| `sockets` | Unix sockets it may connect to. |
| `unix_sockets` | `"all"` accepts that Linux can't allow a socket by path (seccomp blocks `socket(AF_UNIX)` wholesale or not at all), so any socket the filesystem policy leaves visible is reachable; the known Docker, containerd and podman sockets are then hidden. Without it a socket on Linux is refused as unenforceable. The execution's own toolbelt socket makes "all" reach nothing else of the daemon's. |

Paths are absolute, `~/…` or start with a placeholder; patterns are refused. The config is checked strictly at load (`srt.ValidateConfig`), with a fixed, secret-free message.

### How the policy maps to srt

- `denyRead` is the policy's read denies and the protected set. `allowRead` lists the exceptions (the policy's, the attempt's own paths and the shared host paths), but only those beneath a deny, since a broader one would lift the denies inside it. An exception that holds a deeper deny can't be expressed and is refused.
- `allowWrite` is the policy's write grants and the run directory. `denyWrite` is the read-only needs (a clone's `.git/config`, `.git/hooks` and `.git/info`) and the protected paths inside a grant, which srt applies over the write grants. A read-only path that holds a write grant is refused.
- On Linux srt binds a read exception after, and over, the writable paths beneath it, so a write grant beneath a read exception that isn't itself writable is refused. A deny on a path that doesn't exist is masked with `/dev/null`, and a grant on one is ignored: the git provider makes a clone's `hooks` and `info` before handing it out, and a grant should name a path that exists.
- `allowLocalBinding` is off. On macOS that refuses loopback TCP, and on Linux the sandbox has its own network namespace.

### Doctor

The `sandbox` check runs when a binding declares an srt sandbox. It reports `srt <version> at <path>` and, as `fail`:

- `srt_unavailable`: srt, bwrap, socat or rg missing, or srt too old
- `userns_unavailable`: `kernel.apparmor_restrict_unprivileged_userns=1`, `unprivileged_userns_clone=0` or `user.max_user_namespaces=0`
- `seccomp_unavailable`
- `preflight_failed`: `/usr/bin/true` under an empty policy failed or warned

A version newer than the tested one is a `warn` (`untested_version`). The detail adds that sandboxed bindings run nothing until it is fixed.

## Executors

- **Shell** needs its workspace, its outputs and inbox directories, its input file, its token directory and its toolbelt socket. The `os` toolbelt binary runs inside the sandbox.
- **Claude Code** needs its workspace, its own config directory, its MCP config, the playbooks, its token directory and toolbelt socket, and `api.anthropic.com`. Sandboxed, its temporary files go in the segment's own directory (`TMPDIR` and `CLAUDE_CODE_TMPDIR`) while srt keeps `/tmp` for its own sockets (`WrapperEnv`), whose paths would otherwise pass the platform's limit. The policy must grant the config directory and the hosts the work needs (GitHub, a package registry).
- **Claude Code's config directory** is the binding's own. Host-side claude loads user-level hooks, MCP servers and plugins from `~/.claude` and `~/.claude.json` in every session the operator starts, so an agent that could write them would run code as the operator outside the sandbox, and making them read-only doesn't work: claude writes its sessions and state there. So a sandboxed claude-code binding must set `CLAUDE_CONFIG_DIR` in its `env` to an absolute path that is not, doesn't hold and isn't inside `~/.claude` or `~/.claude.json` (`SandboxConfig`, a `config.SandboxChecker`: refused at load as `sandbox_config`). The launch checks the resolved directory again, symbolic links followed, and refuses it as `sandbox_denied`. Its needs also deny `~/.claude` and `~/.claude.json`, under the daemon's home and under any `HOME` the binding's `env` sets in its place, so a policy that still grants them is refused (the provider won't let a grant reach into a deny) and their credentials aren't readable inside. Claude signs in separately there: on macOS its Keychain entry is `Claude Code-credentials-<hash of the directory>`, so the operator's own sign-in isn't shared. A one-off `CLAUDE_CONFIG_DIR=<dir> claude` with `/login`, or a `claude setup-token` token selected as a secret mapped to `CLAUDE_CODE_OAUTH_TOKEN`, signs it in.
- **Claude Code's own sandbox** (`sandbox.*` in its settings) is executor config, not a provider. It wraps only Claude's Bash tool. Under the executor's default `bypassPermissions` its network allowlist doesn't hold, and Open Conduit can't see, pin or verify it. Inside an srt sandbox it can still be turned on with `extra_args: ["--settings", "<file>"]` as defence in depth.

## Workspaces

A sandboxed binding may only take a git workspace in `clone` mode, without `setup`. A worktree commits through the main checkout's `.git`, and setup runs on the host, outside the sandbox, in files the agent wrote ([sandboxed bindings](../workspaces/#sandboxed-bindings)). A clone's sandbox needs keep its `.git/config`, `.git/hooks` and `.git/info` read-only, so the agent can't give the host-side git a hook or fsmonitor to run. The provider's own git in a clone also runs distrusted and with `GIT_DIR` and `GIT_COMMON_DIR` pinned. It sets a new branch's upstream when it makes the clone, since `git push -u` can't record it there; such a push still pushes and exits 0, printing that it couldn't write the config.

## Attach

`open-conduit work attach` runs the executor's session on the host as the operator, outside any sandbox. For claude-code that is `claude --resume` in the workspace, and host-side claude loads the workspace's project config: hooks, `apiKeyHelper` and `statusLine` in `.claude/settings.json` and `.claude/settings.local.json`, and the servers in `.mcp.json`. A sandboxed agent can write all of these, so a hook it planted would run as the operator on attach.

So the management server refuses attach, as `attach_sandboxed`, for Work any of whose segments was launched in a sandbox (a segment with a pinned plan), before the executor is asked for a command. It looks at every attempt, not just the latest, because the workspace outlives them: a binding that has since dropped its sandbox still has the files its sandboxed runs wrote. Unsandboxed Work attaches as before, since its agent already ran as the operator. Running the attach itself under the binding's policy is not built.

Claude Code's own config directory is the same kind of path: a user-level hook, MCP server or plugin planted in `~/.claude` or `~/.claude.json` would run in every claude session the operator starts on the host, not only on attach, and refusing attach doesn't close that. So a sandboxed claude-code binding must have a config directory of its own (`CLAUDE_CONFIG_DIR` in its `env`), and its sandbox denies the operator's `~/.claude` and `~/.claude.json` whatever the policy grants ([Executors](#executors)).

## Evidence

Real isolation tests run in CI on macOS and Linux (the `sandbox` jobs: srt from npm and, on Ubuntu 24.04, bubblewrap, socat and ripgrep, with the AppArmor userns restriction relaxed). They skip without srt, and fail without it under `OPEN_CONDUIT_SANDBOX_TESTS=1`. They call no real service.

- [Isolation tests](https://github.com/open-conduit/open-conduit/blob/main/integration/sandbox_test.go), for the shell executor and for Claude Code's launch path (a stand-in `claude`), in a lab laid out as the daemon's state directory, from inside the sandbox:
  - denied reads of `~/.ssh`, another workspace, `private/operator.token`, another execution's token and the config file, also through a symlink in the workspace, `..`, `/proc/self/root` and an upper-case spelling (which reaches it unsandboxed on macOS's case-insensitive APFS); the attempt's own token readable
  - denied writes outside the workspace, to the home, the state directory, the record directory, `.git/config`, `.git/hooks` and `.git/info`, and renaming `.git`, each checked on the host (on Linux a denied write lands in a throwaway tmpfs); a write and `git commit` in the workspace
  - no new hard link to a protected file
  - a host outside the policy refused by srt's proxy (403) and one in it let through; loopback TCP, a Docker socket, `management.sock` and another execution's toolbelt socket refused; the own toolbelt reachable
  - a control run without a sandbox that reaches everything the sandbox denies
  - fail-closed: a missing binary, an unsupported platform, an old srt, a failed preflight, a degraded srt, a socket by path on Linux, the workspace or socket outside the policy, a policy reaching into or re-reading the state directory and a pre-existing hard link, each with nothing run
  - `Inject` reaching the inbox inside, `Interrupt` reporting `term` (macOS, handler run) or `kill` (Linux), `Stop` leaving nothing
  - `Recover` in a new executor instance adopting its own sandbox, and refusing a changed record, a root outside the sandbox and an unsandboxed segment under a sandboxed binding
  - on Claude Code's launch path, an injected message reaching the sandboxed claude's stdin, recovery under its own pin only, and `Interrupt` and `Stop` ending it
  - on Claude Code's launch path, its own config directory writable while the operator's `~/.claude/settings.json` and `~/.claude.json` can't be written or read, and a binding without its own config directory, or with a policy that still grants `~/.claude`, refused with nothing run
- [Sweep test](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/sandbox_test.go): the git provider's stray-process sweep finds and stops a sandboxed process tied to a workspace only by its working directory.
- `TestManagementAttachRefusesSandboxedWork`: a sandboxed run plants a `SessionStart` hook in `.claude/settings.json` and a server in `.mcp.json`; attach is refused as `attach_sandboxed` without asking the executor and neither runs, while unsandboxed Work still attaches.
- [Contract tests](https://github.com/open-conduit/open-conduit/blob/main/extension/sandbox_test.go), with a fake provider: needs merged with the resource's and the protected set, the plan pinned and recorded read-only, and refusals of a run directory outside the executor's or through a symlink, a workspace holding the state, a need inside the state or through `..`, a socket there, a hard link, a workspace without sandbox needs and an incomplete plan; adoption only under the pinned, unchanged record. [Mount aliases](https://github.com/open-conduit/open-conduit/blob/main/extension/sandboxmount_test.go) from a mountinfo fixture.
- [srt provider tests](https://github.com/open-conduit/open-conduit/blob/main/extensions/srt/srt_test.go), with a stand-in `srt`: strict config, the settings for macOS and Linux, the wrapper's environment, every refusal, `Adopt` refusing another process, and [doctor's health](https://github.com/open-conduit/open-conduit/blob/main/extensions/srt/health_test.go) (also against the real srt in the sandbox jobs).
- [Scheduler hook test](https://github.com/open-conduit/open-conduit/blob/main/integration/scheduler_sandbox_test.go): the pin recorded with the segment and carried into recovery, and a binding failure failing the attempt before any executor call.
- [Config tests](https://github.com/open-conduit/open-conduit/blob/main/internal/config/config_test.go) (`TestSandboxBinding`, with an executor's `sandbox_config` refusal of a binding and a grant), claude-code's `TestSandboxConfig` (no `CLAUDE_CONFIG_DIR`, a relative one, `~/.claude`, inside it, the home that holds it, a link to it, and either home's when the binding's `env` sets `HOME`) and the [git provider's](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/clone_test.go) `TestSandboxedAcquire`: worktree modes and setup refused before anything is made or run.

## Limitations

- Only macOS and Linux, through srt. Remote sandboxes need a contract of their own; [containers](#containers) have one.
- On Linux, `"unix_sockets": "all"` makes every unix socket the filesystem policy leaves visible reachable; only the known container sockets are hidden.
- The hard-link check covers the workspace, not the policy's other write grants. A package manager that hard-links from a store outside the workspace (pnpm) is refused until it is set to copy (`package-import-method=copy`).
- On Linux srt grants only paths that exist when the segment starts.
- srt itself runs as the operator, outside its sandbox, and is trusted.
- Attach to Work that ran sandboxed is refused, not sandboxed ([attach](#attach)). Work that ran in a container attaches in one ([attach in a container](#attach-in-a-container)).

## Containers

A binding may instead run its executor in a container. It declares `container: {provider, config}` in place of `sandbox`; a binding with both is refused at load (`sandbox_conflict`). The contract is [`extension/container.go`](https://github.com/open-conduit/open-conduit/blob/main/extension/container.go) and the host's half [`extension/containerbind.go`](https://github.com/open-conduit/open-conduit/blob/main/extension/containerbind.go). The [Docker provider](#the-docker-provider) ships with [its gateway](#the-gateway), which carries the network allowlist and the toolbelt. The [shell executor](../shell-executor/) and [Claude Code](#claude-code-in-a-container) run in containers.

### The config is the grant

```json
"container": {
  "provider": "docker",
  "config": {
    "image": "ghcr.io/acme/agent@sha256:…",
    "write": ["{workspace}"],
    "read": ["~/.gitconfig"],
    "network": {"allow": ["api.anthropic.com", "github.com"]},
    "limits": {"cpus": 4, "memory": "8g", "pids": 2048},
    "provider": {"context": "orbstack"}
  }
}
```

The host checks its shape whatever the provider (`extension.ParseContainerConfig`), and refuses at load and at launch an image not pinned by digest. A container's filesystem is an allowlist: nothing from the host is visible unless granted. The rules are B09's:

- **The policy is the grant.** An executor's `Write` needs must fall in `write` (or the run directory, always granted), its `Read` and `ReadOnly` needs in `read`, `write` or what the host shares, its hosts in `network.allow`, and its only socket is the execution's toolbelt. Anything else is `sandbox_denied` and nothing is created.
- **The three-way merge.** The executor's needs, the acquired resource's and the host's protected set are merged as for a sandbox, and the host refuses a grant that reaches into the protected set other than at the attempt's own run directory, workspace and shared paths.
- **Canonical paths.** A container mounts canonical paths, so the host rewrites each argv element and environment value that names the launch's workspace, run directory, home, shared paths or needs through a symbolic link (macOS's `/tmp` is `/private/tmp`) to the canonical path.
- **The same path checks** (`GuardLaunch`): the run directory, exposure of a protected path, needs beneath the protected set and hard links.
- **Fail closed.** No provider, an executor without `Capabilities.Container`, a config that doesn't parse, a listing that fails, a provider error or an incomplete plan or container each refuse the launch.

### What executors see

`ExecutionContext.Container` is a `ContainerBroker`, nil unless the binding declares a container. An executor makes two calls:

- `Launch(needs, launch)` where it would build its wrapper's argv. The host checks the needs, lists the execution's containers, has the provider plan and create the container (not started), pins it and returns the argv the wrapper runs to start it, attached, and wait for its exit. `Signals` is `term`.
- `Adopt(alive)` in `Recover` ([Recover](#recover)).

There is no `Signal` call: `Interrupt` sends TERM to the executor's group as it does now, and the start helper passes it to the container. Stopping, reconciling and removing containers are the host's, and executors never see them. An executor that implements this advertises `Capabilities.Container`; shell and claude-code do.

`ExecutionContext.ContainerHome` comes with the broker: the Work's own home, `<state>/homes/<work key>/`, host-owned and made empty on first use. It is per Work, not per execution, so a session resumes across segments. The host grants it read-write and mounts it at the same path, and it is an exception to the protected set for that Work's containers only (`ContainerBinding.WorkHome`, `GuardLaunch`'s `Home`): no other Work's container, and no process sandbox, reaches it. The host removes it, with the `<key>.work` file beside it naming the Work, once the Work is finished or gone, which is when claude-code forgets the Work's continuity. A launch's `Secrets` are its credentials: the provider keeps them out of the container's environment and out of the plan ([the entry stub](#the-docker-provider)).

### The provider

`ContainerProvider` has `Plan(owner, policy, needs, launch)`, `Create(plan, labels)`, `Signal`, `Stop(ref, grace)`, `Inspect(ref)`, `List(owner)` and `Remove(ref)`, all keyed on a `ContainerRef` (the container's ID and its labels). `Inspect` names the container daemon that answered, and says `Gone` when that daemon holds no such container; an error means it couldn't be asked. `List` fails as an error, never as an empty list. Every container carries four labels: the host (a hash of the daemon's state directory), the execution, the segment and the plan's digest.

### Pinned per segment

Before the argv reaches the executor, the host pins the container with the open segment (`Segment.Container`): the provider, the plan's digest and summary, the image digest, the container daemon's ID, the container's ID, the run directory and the time. The digest is `sha256` of the image and the plan's `Data`. A recovered segment carries its pin. History shows each segment's container: its plan, image and daemon.

### No second container

Before any launch, the host lists the execution's containers. One running refuses the launch as `container_running`, so a retry can never start beside a live one; stopped ones are removed first. A listing error refuses the launch: it never counts as "none running".

### Host-side lifecycle

- **Stop.** After the executor's `Stop`, the daemon's stop confirmation stops the pinned container (`StopContainer`) and confirms only once `Inspect` shows it isn't running. Until then the attempt isn't confirmed stopped (`container_stop_unverified`) and replacement stays blocked.
- **Reconcile.** Each pass, the daemon removes its labelled containers (with the provider's gateways and networks) whose execution is terminal or gone (`ReconcileContainers`).
- **Remove before release.** The resource owner status removes a finished execution's containers before its workspace may be released; a failure keeps the workspace. For a live execution it reports the containers' mounts (`ResourceOwnerStatus.Mounts`), and the git provider's spare sweep keeps a workspace a mount holds as `in_use`. The git provider knows nothing of containers.

### Recover

The broker's `Adopt` first proves the container is the pinned one: on the pinned daemon, the pinned ID, the host's, execution's and digest labels, the pinned image, and a `StartedAt` later than the pin.

| Wrapper | Container | Adopt | The host's Stop |
| --- | --- | --- | --- |
| alive | the pinned one | adopted | |
| dead | exited | its exit code returned as the result | confirmed |
| dead | running | `ErrNotAdoptable` | stops it before a retry |
| either | gone, on the same reachable daemon | `ErrNotAdoptable` | confirmed not running, so replacement may go ahead |
| either | daemon unreachable, or another daemon's ID | `ErrNotAdoptable` | can't confirm, so replacement stays blocked |

### A stop the host didn't cause

The host records every TERM and stop it sends a container, in the execution's container records (`<state>/runs/<execution>/container`, outside every grant), before sending it. The container's entry stub writes `ContainerTermMarker` in the segment's run directory when it gets a TERM. A segment that exits after a TERM the host didn't send is reported as interrupted, whatever its code: the host's `Settle` turns its result into a failure, keeping the executor's data, and `Adopt` reports it on an exited container.

### What core knows

As for sandboxes, one hook: `SchedulerOptions.Container`, called after `Resources`. It returns the broker, the resource broker the executor gets and `Settle`, which core applies to the segment's result. Its `pin` records the pin with the open segment, fenced by the lease. `SchedulerOptions.Signalled` tells the host before core asks an executor to interrupt. A binding failure fails the attempt as `container unavailable`.

### Attach in a container

Work that ran in a container attaches in one, so nothing the agent wrote is read or run on the host. Its executor's `Attach` returns `open-conduit container attach <work>` (claude-code does when the session's last turn ran in a container), and the management server refuses any other command for such Work as `attach_sandboxed`. `open-conduit work attach` runs it in-process; it can also be run by hand on held Work. It asks the daemon (`POST /work/{id}/container-attach`) to:

- make a container under the binding's current container policy, as a segment's is made: the same image, mounts (the Work's home and workspace), gateway and `--internal` network, labelled `dev.open-conduit.segment=attach`, with a terminal (`--tty`) and no toolbelt token, since there is no attempt to issue one to;
- launch in it what the executor's `ContainerAttach` says (`extension.ContainerAttachExecutor`): for claude-code, `claude --resume <session>` in the workspace, with the binding's sign-in in the secrets file;
- pin the plan and record it with the latest attempt (`AttemptState.Attaches`), so history shows an attach as it shows a segment's container.

The CLI starts it with `docker start -a -i` on the operator's terminal. There claude asks, as in any folder it hasn't seen, whether to trust the workspace and then to confirm bypass-permissions mode; the operator answers both, and nothing pre-trusts a folder the agent wrote. On exit the CLI asks the daemon (`POST /work/{id}/container-detach`) to remove the container, its gateway and its networks. `ErrNothingToAttach` applies as before, and the shell executor has no attach. A running attach container is left alone by reconcile; one left running when its terminal went away is removed by the Work's next segment launch, which runs only once the hold is released. **Git during an attach:** the attach container sees the workspace but not the main clone's `.git` behind a worktree, since the workspace resource isn't acquired for it, so on a worktree workspace git commands that need the main clone (`git status`, `git commit`, `git fetch`) fail during an attach. Claude itself resumes, and the next segment can commit.

### Claude Code in a container

- **Its home.** A container binding needs no `CLAUDE_CONFIG_DIR` of its own (config load's check for a sandboxed binding's config directory is for process sandboxes). `HOME` and `CLAUDE_CONFIG_DIR` both point at the Work's home, so claude's `settings.json`, `projects/` (the session transcripts resume reads) and `.claude.json` live there. On first use, while it is empty, the home is filled with a copy of the binding's `home_seed` directory (a `settings.json`, plugins), leaving out any `.credentials.json` (use plain files: a symbolic link is copied as a link, and one to a host path dangles inside the container), and a minimal `.claude.json` that marks onboarding done unless the seed brought one. It is a copy, never a mount, so the agent can't change the operator's own files, and a home with anything in it is the agent's and is never seeded again. Its `PATH` is a Linux one unless the binding's `env` sets one, and `TMPDIR` is the container's `/tmp`.
- **Credentials.** The Claude sign-in (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`), every secret the binding selects, and any variable named as a credential (ending `_TOKEN`, `_API_KEY`, `_SECRET` or `_PASSWORD`, such as a static `GH_TOKEN`) go in the launch's `Secrets`, never its environment, so they are not in `docker inspect`, the plan record or history.
- **git's GitHub credential.** The agent runs `git fetch` and `git push` as it would anywhere; how git signs in depends on the connection:
  - **A GitHub App connection** (`"auth": "app"`, or a `bot_app_id`): no GitHub credential crosses in. A segment with a toolbelt gets git's credential helper set to the toolbelt's `os git-credential`, which calls `github.git.credential` (add it to the binding's `operations`). It goes after the `GIT_CONFIG_*` entries the workspace and the run's environment layer set, so a helper they set for one host is asked first, unless they already set git's general `credential.helper` (the GitHub extension's layer does for `git` `bot`). The host mints an installation token, from the bot App when there is one, limited to the Work's one repository with `contents: write` only. It answers only for `https` on the connection's web host (and, when git sends one, that repository's path), and it hands out the same token again while it has more than five minutes left. The token never enters the plan, the toolbelt's replay records or history. When the segment ends the host revokes every token minted for it (`DELETE /installation/token`), and the reconcile loop revokes any it missed for executions that aren't running. The execution's toolbelt token is revoked too, so the helper can't mint another. The host keeps minted tokens in memory: after a daemon restart, any it minted before the restart can't be revoked and expire on their own, within the hour. An attach has no toolbelt, so git there has no GitHub credential.
  - **A static token (a PAT)**, set as `GH_TOKEN` or `GITHUB_TOKEN` in the binding: the token itself crosses into the container in the secrets file, and git reaches the connection's web host through a credential helper that reads it. The GitHub extension's run environment layer sets that helper, for `credential.https://<host>`, on every run of a binding whose connection doesn't set `git` `bot`; it says nothing when the run has neither variable, so git goes on to its next helper (the toolbelt's, after it, which mints for an App connection). The executor knows nothing about it. That token is whatever the operator made it, often good for every repository the account can reach and for months.
- **What the credentials expose.** Anything in the container can read them, and the gateway doesn't inspect TLS, so a host in `network.allow` can receive them. With an App, the GitHub token it can read only pushes to the Work's repository, and only until the segment ends. With a static token it is the token itself: use a fine-grained one for the one repository. Use a `claude setup-token` token for claude. Injecting the GitHub credential at the gateway instead would need either TLS interception (a per-execution CA trusted in every image) or a plain-HTTP git rewrite. Either way, something in the container could still push with it, so the gateway doesn't do this.
- **A planted `apiKeyHelper` overrides the sign-in token.** The agent can write its home's `settings.json`, and claude prefers a helper's key to `CLAUDE_CODE_OAUTH_TOKEN`. That only breaks or swaps the agent's own sign-in, inside the boundary; it never runs on the host.
- **Telemetry.** Segments set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, so claude doesn't try its telemetry and update checks on every call, which the gateway would refuse.
- **The toolbelt.** The MCP server is the container's own `os` (first on its `PATH`), which inherits `OS_ENDPOINT`, moved to the gateway's forward, and the token file's path from claude's environment.
- **Stream-json stdin** goes through `container run` to `docker start -a -i`. A segment adopted after a daemon restart has no stdin, so `Inject` is `ErrNotAccepting`, as for a process segment.
- **Signals.** `Interrupt` sends TERM, which the helper relays and the entry stub passes to claude. Claude runs as a background job of the stub's non-interactive shell, which starts it with INT ignored, so nothing relies on INT; an attach runs claude in the stub's place on its terminal.

### The Docker provider

[`extensions/docker`](https://github.com/open-conduit/open-conduit/tree/main/extensions/docker) runs containers on Docker Engine 28 or later. Its settings name the Docker CLI context every call uses, `"provider": {"context": "<name>"}`, so the daemon never changes with the CLI's current context. A container's ID is `<context>/<docker ID>`.

**Create.** `docker create --name oc-<execution key>-<segment>`, where the key hashes the host label and the execution, with the four labels, `--init`, `--cidfile` in the execution's container records and `--restart no`. It never starts it. A container that `docker create` made before failing is removed by the ID it wrote.

**Hardening.** `--cap-drop ALL`, `no-new-privileges`, a read-only root with a tmpfs `/tmp`, `--user` the operator's uid and gid, a process limit (4096 unless `limits.pids` says otherwise) and no `docker.sock`. The environment goes through an env file in the records, readable only by the operator and removed once the container is created; the plan records only its keys.

**Mounts, at the same paths as on the host.** The write grant, the run directory and the Work's home read-write; the read grant, what the host shares (the toolbelt token directory) and the records read-only. Every read-only need and every protected path inside a mount is covered: a read-only bind over it, or an empty file or directory bound over it to hide it. So the clone's `.git/config`, `hooks` and `info` are read-only over the read-write workspace. A path that can't be covered (one that doesn't exist, or a symlink, which the host refuses first) refuses the launch as `sandbox_unavailable` rather than leave it writable. A write grant that doesn't exist is refused; a read grant that doesn't exist is left out.

**Networks.** Each execution has its own `--internal` bridge network, created with `com.docker.network.bridge.inhibit_ipv4=true` and without IPv6 on every platform: without that option a Linux host's ports are reachable through the bridge's IP (#176). A container on it has no route out and reaches no host port; its only peer is its [gateway](#the-gateway). `Remove` removes the network, and the gateway's egress network, with the container, unless a later segment's container is already on them.

**The start helper.** `Launch` returns the argv of a hidden `open-conduit container run [--records <dir>] <context>/<id>`, not a bare `docker start -a`, which can't carry TERM (#176). The helper stays in the executor's process group, so `Interrupt` reaches it. It runs `docker start -a -i` in a group of its own, turns TERM, INT and HUP into `docker kill -s` (retrying until the container runs) after recording each as one the host sent, and exits with the container's code from `docker wait`, or 125 if the container never ran. Every signal that reaches the container through the host is recorded, whichever path sent it: `Interrupt`, the executor's own deadline or the host's stop. A TERM from a Docker restart never passes through the helper, so it counts as unrequested. The helper is Go, not a shell relay: on Linux, `dash` ignores `set -m`.

**The entry stub.** The container's entry point, under Docker's init, is a `/bin/sh` stub, so an image must have one. When the launch has `Secrets`, `Plan` writes them as shell exports to `.open-conduit-secrets` in the run directory, readable only by the operator, and records only their names; each name must be upper case, so none can stand for one of the stub's own variables. The stub reads that file and removes it before anything else. It then runs the launch's argv with the container's stdin, writes `ContainerTermMarker` in the run directory when it gets TERM, passes TERM, INT and HUP on and exits with the command's status.

**Recover.** `Inspect` asks the context's daemon for its ID and then the container: a daemon that holds no such container gives `Gone`, and one that can't be reached is an error, so each row of the [Recover](#recover) table holds against real Docker. `List` fails if any context the config uses fails.

**Doctor.** The `container` check runs, for each context and image the enabled bindings use:

- the daemon reachable on the named context, with its ID;
- a server version of 28 or later;
- a shared-path test, in which a container running as the operator writes a file into the workspace root and the host checks its owner;
- the gateway image present by digest (`gateway_unset`, `gateway_unpinned` or `gateway_missing`, with the `docker pull` to run);
- the internal-network preflight. It makes a network as an execution's is made, starts the gateway image on it as `gateway`, and runs the image's probe there. The probe must resolve `gateway`, must fail to resolve an outside name, and must not reach a port the host is listening on through `host.docker.internal`, the network's bridge address or the default bridge's. It fails (`ipv6_gateway`) if the daemon gives the network IPv6, which the gateway can't hold.

Any failure blocks the provider, on Docker Desktop as on anything else: `Plan` runs the same checks (a pass stands for ten minutes) and refuses as `sandbox_unavailable` when one fails.

### The gateway

Each launch's container gets a gateway container, created and started with it and removed with it (`oc-<execution key>-<segment>-gw`). The gateway is the container's only way out: to the hosts its grant allows, through the gateway's proxy, and to its own toolbelt, through the gateway's forward.

- **Networks.** The gateway joins the execution's egress network, an ordinary bridge only gateways join, then the internal network with the alias `gateway`. It runs hardened as the container does (`--cap-drop ALL`, `no-new-privileges`, a read-only root, the operator's uid and gid), with small process and memory limits. It carries the host, execution and segment labels and `dev.open-conduit.role=gateway`. An execution's listing leaves it out, so it is never "a second container"; the host's whole listing has it, so reconcile removes the gateway of an execution that is over.
- **The allowlist.** The gateway's proxy (port 3128) allows only the grant's `network.allow`: a host (any port), `host:port` (that port only) or `*.domain` (anything beneath it). It takes `CONNECT` for TLS and absolute-form `http://` requests, checks the host and port against the list, then resolves the name itself. It refuses with `403` a name any of whose addresses is loopback, link-local (a cloud metadata service), private (RFC 1918 or ULA), shared (CGNAT), unspecified, multicast or in `0.0.0.0/8` (where OrbStack puts the host), or is `host.docker.internal`'s or the gateway's own default route's. So neither a wildcard entry covering names someone else controls nor a rebinding DNS answer leads to the host, the LAN or a metadata service. It then dials the addresses it checked, never the name again. An IP address is allowed only if it is listed, and a listed one is reached as it is, private or not. Every refusal is logged in the gateway's output (`docker logs`). The container's environment points every client at it: `HTTP_PROXY`, `HTTPS_PROXY` and their lower-case forms are `http://gateway:3128`, with `NO_PROXY` for loopback, replacing any proxy the launch had. A tool that ignores them has no route: the internal network has none, and no DNS for outside names.
- **The toolbelt forward.** A unix socket can't be bind-mounted across Docker's VM on macOS (it gets `ECONNREFUSED`), but it works between containers. So the gateway serves `/run/open-conduit/toolbelt/toolbelt.sock` in a small tmpfs volume owned by the operator, which the container mounts read-only, and the container's `OS_ENDPOINT` points there; the client and MCP server don't change. The gateway copies each connection to the execution's toolbelt on the host:
  - **Linux:** to the execution's `toolbelt.sock`, whose directory is mounted read-only into the gateway (a directory, so a restarted daemon's new socket is seen).
  - **macOS:** to a loopback port the host serves for that execution only (`Endpoints.ServeTCP`), reached as `host.docker.internal`. The host writes the port in `toolbelt.port` beside the socket and serves a new one on each launch and recovery; the gateway reads it for each connection. Calls on the port are authorised by the execution's token alone. Unlike the socket, behind its owner-only directories, the port can be reached by any local process and by any other container on the machine with a route to the host, so the token is the only thing between them and the execution's toolbelt.

  `OPEN_CONDUIT_CONTAINER_FORWARD=tcp` forces the macOS mode on Linux, where the port is on the default bridge's address and reached through `host-gateway`.
- **The `os` binary.** The gateway image carries the Linux `os` in `/toolbelt`. Every container mounts it read-only with an image mount (Docker 28 or later) at `/run/open-conduit/bin`, which goes first on its `PATH`, so any image can use the toolbelt without bundling Open Conduit.
- **The image.** `ghcr.io/open-conduit/open-conduit-gateway`, `FROM scratch`, holding the gateway program (`cmd/open-conduit-gateway`) and `os`, for `linux/amd64` and `linux/arm64`. Each release pushes it at the release's version (`scripts/publish-gateway.sh`) and pins its digest in the `open-conduit` binary (`extensions/docker.GatewayImage`). A build without one, such as a source build, runs no container binding unless `OPEN_CONDUIT_GATEWAY_IMAGE` names a gateway image pinned by digest or an image ID; `make gateway-image` builds one for the local Docker and prints its ID.

**What the gateway doesn't do.** It doesn't inspect TLS, so an allowed host can receive anything the container can read, including its token and its binding's secrets. Keep `network.allow` to the hosts the work needs.

**Platforms.** Linux is checked in CI by the `container` job on the runner's own Docker Engine, with a second pass of the gateway, shell and git credential tests in the tcp forward mode macOS uses. macOS isolation is checked by a local `make container-test` run on OrbStack, posted on each PR that touches the provider or the gateway, not in CI. Docker Desktop is unverified until someone runs `make container-test` on it; doctor names it when it finds it.

### Evidence

[Contract tests](https://github.com/open-conduit/open-conduit/blob/main/extension/container_test.go) against a fake provider: the container created unstarted, labelled and pinned; refusals of an image not pinned by digest, a failed listing, a second container, needs outside the grant, a grant reaching the state and the shared path checks; each Recover path with the host's Stop; an unrequested TERM; reconcile, removal and mounts failing closed. `TestGuardLaunchIsShared` runs one refusal through both brokers. `TestContainerBinding` covers the config, `TestSweepKeepsMountedWorkspaces` the sweep and `TestProductionSchedulerContainerHook` the scheduler hook.

[Isolation tests](https://github.com/open-conduit/open-conduit/blob/main/integration/container_test.go) against real Docker (`make container-test`; the `container` CI job on Linux), with containers created through the host's binding and started by the helper as an executor's wrapper would:

- `TestDockerIsolation`: from inside, only the workspace, run directory and `/tmp` are writable; writing or moving `.git/config`, `.git/hooks` and `.git/info` fails; the own token is readable and nothing else of the state, home or config is; no default route, no host port through `host.docker.internal`, the bridge's gateway or `docker0`, and the gateway reachable; the gateway image's `os` first on `PATH`, and neither it nor the forward's directory writable; no capabilities and the operator's uid, and the workspace write owned by the operator on the host. The created container's and gateway's hardening, labels and networks.
- `TestDockerGateway`: from inside, an allowed host reached through the proxy by plain HTTP and by `CONNECT`, a denied one getting `403`, a tool that ignores the proxy getting no route by name or address and no outside DNS, and `os context` reaching the execution's toolbelt through the forward. The hosts are stand-in containers on the egress network.
- `TestDockerShell`: the shell executor runs a binding's command in a container end to end, with its workspace, toolbelt, an allowed host and its outputs, and completes.
- `TestDockerFailClosed`: needs outside the grant, a grant reaching the state, a host outside `network.allow`, a socket, an unreachable context, an image not pinned by digest, no gateway image, a symlinked `.git/config`, a missing read-only path and a second container, each refused with nothing created.
- `TestDockerProcessControl`: the container's exit code and stdin through the helper; `Inject` through the run directory; `Interrupt` reaching the command's own trap and settling as the host's; a TERM sent outside the host settling as interrupted; the host's `Stop` confirmed.
- `TestDockerRecover`: each Recover row: wrapper alive, wrapper dead with the container exited (its code) or running (stopped by the host), the container gone, another daemon and an unreachable one.
- `TestDockerClaudeCode`: a claude-code turn runs in a container with the Work's home as `HOME` and `CLAUDE_CONFIG_DIR`, its sign-in present inside but not in `docker inspect`, the secrets file gone once read, and nonessential traffic off; the next segment resumes the session from the home. An attach runs `claude --resume` in a container of its own on a terminal, and a `PreToolUse` and a `SessionStart` hook, an `apiKeyHelper`, a `statusLine` and an MCP server planted in the home's `settings.json` and `.claude.json` and the workspace's `.claude/settings.json`, `.claude/settings.local.json` and `.mcp.json` each run inside it and none on the host. The attach container and its gateway go on detach. The claude is a stand-in in the test image that runs what it finds as claude would.
- `TestDockerGitCredential`: claude-code in a container, on a GitHub App connection, pushes to the Work's repository with plain `git push` through the toolbelt's helper, over TLS through the gateway to a stand-in GitHub on the egress network (`integration/fakegithub`, which holds tokens to what they were minted for, as GitHub does). The minted token can't push to or read a second repository the App is installed on. Once the execution has ended (its toolbelt token revoked and its git tokens revoked, as the segment's end does), neither that token nor the helper works, and the next execution pushes with a token of its own.
- `TestDockerRemoveAndReconcile` and `TestDockerHealth`: removal and reconcile take the container, its gateway, volume and networks and keep a live execution's; doctor's checks pass, including the preflight, which leaves nothing behind, and fail on an unreachable context, no gateway image and a missing one.

[Contract tests](https://github.com/open-conduit/open-conduit/blob/main/extension/container_test.go) also cover the Work's home (granted, and an exception for its own launch only) and attach containers (labelled, left by reconcile while they run, removed by a segment's launch). [Claude Code's](https://github.com/open-conduit/open-conduit/blob/main/executors/claudecode/container_test.go) cover the home, its seed, the secrets and the environment, the MCP server, resume, `Attach` and `ContainerAttach` against a fake broker. [`TestGitCredential`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/gitcred_test.go) covers the mint (the one repository, `contents: write` only), reuse within an execution, no credential for another protocol, host or repository, revocation at a segment's end and in the sweep, and a sweep that can't list executions revoking nothing; `TestGitCredentialStaticToken` covers the refusal on a token connection, and `TestToolbeltGitCredential` covers `os git-credential`. `TestManagementAttachInContainer` covers the refusal of a host command for Work that ran in a container, and `work attach` and `container attach` making and removing the attach container.

[Unit tests](https://github.com/open-conduit/open-conduit/blob/main/extensions/docker/docker_test.go) cover the settings, the mount plan with its covers and refusals, the names, the container's environment, the refusals without a gateway, the Work's home mount, and the secrets file with the entry and attach stubs reading and removing it. [The gateway's](https://github.com/open-conduit/open-conduit/blob/main/extensions/docker/gateway/gateway_test.go) cover the allowlist (host, port-pinned and wildcard entries, IP literals, `CONNECT` after a half-close), plain HTTP, both forward modes with a port that changes, and the probe. `TestEndpointsServeTCP` covers the host's loopback port.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **#160** proposed a sandbox as a launch wrapper beside `resource`. The owner approved B09 as a process-scoped primitive, with a deny-by-default network, sandboxing as a security requirement and real isolation tests in CI.
- **#161** spiked it on real srt (macOS and Linux), Claude Code's built-in sandbox and `docker run`. It showed one provider serving shell and claude-code, Claude Code's sandbox as executor config, and Docker failing the process contract. It also found the needs-versus-policy, protected-set, worktree, Linux socket, token-file and signal findings this contract is built on.
- **#162** gave each execution its own toolbelt socket and token directory, and **#163** the git `clone` mode and the refusal of worktree modes: the two prerequisites.
- **#164** delivered B09: the contract, host-side merging and pinning, recovery's boundary proof, the srt provider, both executors, doctor and the isolation tests in CI.
- **#173** gave a sandboxed claude-code binding a config directory of its own, refused one that would share the operator's `~/.claude`, and denied the operator's claude config to its sandbox, since a user-level hook planted there would run in every host-side claude session.
- **#171** proposed the container contract, and **#176** spiked it. **#177** added the contract, its host-side lifecycle and the shared path checks, tested against a fake provider. **#178** added the Docker provider, the `container run` helper, the doctor checks and the `container` CI job. **#179** added the gateway (its proxy, the toolbelt forward in both modes, the `os` image mount), the gateway image as a release artifact, the gateway's doctor checks and the shell executor in containers. **#180** ran claude-code in containers: the per-Work home and its seed, credentials in a secrets file the entry stub reads, nonessential traffic off, and attach in a container of its own. **#181** replaced the static GitHub token with tokens a GitHub App connection mints for the Work's repository through the toolbelt's git credential helper, revoked when the segment ends; it chose the helper over injecting the credential at the gateway.
