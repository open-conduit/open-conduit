---
title: "Optional workspaces and the Git provider"
description: "Decision B07: how executors acquire optional workspaces from a resource provider, and how the Git provider creates, reuses and cleans up checkouts and worktrees."
---

This page records decision B07: the workspace provider contract, and how workspaces are made fresh or shared, copied into, set up and cleaned up. A Scope may bind a resource provider; an executor acquires a workspace from it for each attempt, and the host reconciles and removes workspaces once their owners finish. The public contract is [`extension/resource.go`](https://github.com/open-conduit/open-conduit/blob/main/extension/resource.go). The Git provider in [`extensions/git`](https://github.com/open-conduit/open-conduit/tree/main/extensions/git) implements it. The [shell](https://github.com/open-conduit/open-conduit/tree/main/executors/shell) and [Claude Code](../claude-code-executor/) executors consume it, and [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/resources.go) composes them.

## What core knows

Core knows nothing about repositories, branches, worktrees or filesystem layout. A Scope may have no resource, and absence is valid. The runtime's only involvement is one generic hook, `SchedulerOptions.Resources`. Immediately before `Run`/`Recover`/`Resume`, outside coordination transactions (like `Secrets`), the host binds the attempt's provider. Core then places the opaque `extension.ResourceBroker` in `ExecutionContext.Resources`, which is nil when the binding declares no resource. A binding failure fails the attempt with `ErrResourceUnavailable` before any executor call. Core never calls a provider, and no lease, reference or path enters coordination storage. The boundary checker still forbids runtime from importing `extensions/git`.

## The contract

| Type | Role |
| --- | --- |
| `ResourceProvider` | Implemented by an extension: `Acquire`, `Lookup`, `Reconcile`. |
| `ResourceBroker` | What an executor sees: `Acquire(ResourceRequest)`, `Lookup(ref)`. `BindResources(provider, owner, config)` creates it. Only the host binds, so an executor cannot act for another owner. |
| `ResourceOwner` | Trusted `{ScopeID, WorkID, ExecutionID}` taken from the attempt, never from the executor. |
| `ResourceRequest` | Executor-chosen `Mode` (empty selects the binding's default), provider-defined opaque `Params`, and an optional `Log` writer for human-readable progress. |
| `Resource` | `Kind`, opaque lease `Ref`, `Dir`, `Env` (the provider's environment layer, never persisted), `Data` (a description for history) and `Summary` (a one-line description for history). |
| `ResourceSweeper` | Optional `Sweep(status)`: removes spare resources owner-driven release doesn't reach and reports those it kept. |
| `ResourceStatusFunc` | Host-reported durable owner state for `Reconcile`. An execution is *done* only when it is terminal **and** its physical resource exclusion is confirmed (`AttemptState.Excluded`). |

These are the guarantees a provider must meet:

1. Acquire is idempotent per owner and mode. A resumed segment or a retried launch gets the same lease and never a duplicate resource.
2. A lease is durably recorded before anything is created, so a crash at any point leaves something `Reconcile` can find.
3. `Lookup` never creates, fetches or runs setup, and it refuses leases the caller does not own (`ErrResourceNotFound`).
4. A setup failure returns the usable resource together with `ErrResourceSetup`, so it can be inspected.
5. `Reconcile` is repeatable and restart-safe. It releases leases whose owners finished, applies retention and retries failed cleanup.

## Modes, from the real GitHub needs

| Mode | Owner | Behavior |
| --- | --- | --- |
| `shared` (default) | execution | Runs in the scope's main checkout. The only change it ever makes is a fast-forward of the base branch, and only when no other execution holds the checkout. It never resets, stashes, merges or rebases. Git refuses an update that would overwrite an uncommitted edit, and that is reported. The run is told how far behind it is and why: `on feature, not main`, `uncommitted changes to files the update touches`, `main has commits origin/main doesn't`, `another execution is using the checkout` or `couldn't fetch: …`. This appears in the log and in `Data` (`updated`, `behind`, `stale`, `dirty`). |
| `work` | Work | Creates a worktree on a branch private to the Work, reused by each of its attempts until the Work is terminal. An existing local branch is checked out. A remote-only branch is tracked from the configured remote. Otherwise a new branch starts at `remote/base` (or the local base). Reuse never resets: local commits and uncommitted edits stay, and the run is told how many uncommitted paths it inherited. Copied files do not count. A branch checked out in a retained workspace of another Work (one that failed or was cancelled, then started again for the same issue) is taken over: the worktree is moved to the new lease's path with its commits and edits, and the old lease is dropped. A branch checked out in a live Work's workspace fails naming that Work, and one checked out anywhere else fails with git's reason. Executors may request a branch through `Params` (`{"branch": …}`); otherwise `branch_template` applies. |
| `fresh` | execution | Creates a detached worktree at the base branch's freshly fetched remote head, isolated per execution. The main checkout never moves. |
| `clone` | Work | Like `work` (the same branch choice, reuse without reset, take-over of a retained one and `Params`), but a separate clone of the checkout instead of a worktree, so it shares nothing writable with the checkout. See [clone workspaces](#clone-workspaces). This is the Git provider's own mode, not one of the contract's. |

Claude Code issue work uses `work` (or `clone` with `workspace_mode: "clone"`): a new segment acquires and a resumed segment uses `Lookup` ([Claude Code executor](../claude-code-executor/#workspace-and-environment)). Fresh-checkout and in-checkout jobs use `fresh` and `shared` through ordinary bindings ([job runs](../schedule-extension/#job-runs)). Slack and research work have no resource.

The executor's request wins: a resource config's `mode` applies only when the executor requests none. Claude Code always requests one, so a `mode` on its binding's git resource never applies. `config check` warns about such a binding, naming the scope, the binding, both modes and the fix (remove the resource's `mode`, or set the mode in the executor's config). The provider says which mode its config names through the optional `config.Registry.ResourceModeDefault` (`git.ModeDefault`, which reads `mode` and is empty when the config names none), the executor says which it requests through `config.ResourceModer`, and `config.ModeWarnings` warns when both are set and differ. A binding a scope inherits unchanged is reported once, at the scope that declares it. The warning never fails the check and changes nothing at run time.

## Clone workspaces

A worktree commits through the main checkout's common `.git`: its index lock under `.git/worktrees/<name>`, plus the shared `objects/`, `refs/` and `config`. A sandbox (B09) that can write there could set a hostile `core.hooksPath` or `core.fsmonitor`, which then runs unsandboxed the next time the operator uses git, or move `main`. A sandbox that can't write there can't commit. A `clone` workspace is a repository of its own, so a sandbox can be given the whole of it.

- **Made by a local clone with its objects copied.** `git clone --no-hardlinks --no-checkout` of the checkout's path, with no `--shared`, `--reference` or alternates. A hardlinked object would be the checkout's own file, which a process that can write the clone could `chmod` and rewrite, and no path policy can see that. So every object is copied. Nothing in the clone is the checkout's, so a `gc`, `prune` or repack in the checkout can never take an object away from an active clone.
- **The real remote.** The clone's remote (named as `remote`, default `origin`) is set to the checkout's URL and push URL for that remote, never the checkout's path, and its remote-tracking branches and tags are the checkout's. A URL with embedded credentials is used without them. Pushes and fetches use the operator's own (global or system) credential helpers, as the provider's other git commands do. The checkout's local `user.name`, `user.email`, `user.signingkey`, `gpg.format`, `commit.gpgsign` and `tag.gpgsign` are copied in, so commits are made as the checkout's would be.
- **The branch.** As for `work`: the checkout's existing local branch (copied into the clone), a remote-only branch tracked, or a new branch from `remote/base` (or the local base). A new branch's upstream (`branch.<name>.remote` and `.merge`) is set as a first `git push -u` would set it, since in a sandbox the clone's config is read-only. The checkout's other local branches are not copied.
- **Reuse.** The lease belongs to the Work and the clone is reused, never reset, by each attempt. A reused one must still be a repository of its own: its git directory is its `.git`, with no alternates. A retained clone of another Work on the same branch is taken over by renaming its directory.
- **Copy, setup, `.env`, signing, retention and the sweep** behave as for `work`. The setup stamp is in the clone's `.git`.
- **Removal keeps the branch.** Removing a worktree leaves its branch in the checkout; a clone's branch lives only in the clone. So before a clone is removed, if its branch has commits no remote-tracking branch has, they are fetched into the checkout's branch of that name, or into `<branch>-<ref>` when that can't take them (it has moved on, or is checked out). If that fails, the removal fails and is retried as `cleanup_failed`. An orphaned clone (no lease) is removed by the sweep only when it has nothing unpushed, as before.
- **Host-side git never runs the clone's config.** A sandboxed agent can write its clone's config, so every git command the provider runs in a clone (reading status for inherited edits and the sweep, `rev-list`, `rev-parse`, the branch save's checks) is pinned with `-c core.fsmonitor=false -c core.hooksPath=/dev/null -c gc.auto=0 -c maintenance.auto=false`. The transport is pinned to a local path with `-c protocol.allow=never -c protocol.file.allow=always` and `GIT_PROTOCOL_FROM_USER=0`, so an `insteadOf` can't route a fetch to `core.sshCommand`, `core.gitProxy` or a remote helper. It also empties `clean`, `smudge` and `process`, and clears `required`, for every filter driver the clone's own config defines, including through `include.path`. These are read with `git config --show-scope`, which runs nothing. Each command runs with `GIT_OPTIONAL_LOCKS=0` and with `GIT_DIR`, `GIT_COMMON_DIR` and `GIT_WORK_TREE` pinned to the clone, so a `commondir` file the agent wrote can't point it at another repository's config. `status` runs with `--ignore-submodules=all`. A driver name `-c` can't express makes the command fail, which leaves the clone unreadable (kept). Saving the branch fetches *from* the clone into the checkout, so the clone's side is `upload-pack`, which ignores repository-level hook config.
- **Disk.** The full-disk refusal adds the copied objects to the checkout's files: the checkout's whole object store (`git count-objects -v`).

### Sandboxed bindings

A binding whose executor runs in a sandbox ([B09](../sandboxes/)) may only take a git workspace in `clone` mode, and without `setup`: `work` and `fresh` are worktrees, `shared` is the checkout itself, and setup runs on the host, outside the sandbox, in files the agent wrote in an earlier attempt (its hooks, its package scripts). The check is `git.ValidateSandbox(config, mode)`, where `mode` is what the executor requests (Claude Code's `workspace_mode`, through the optional `config.ResourceModer`) and empty means the resource config's `mode`. It is registered as `config.Registry.ResourceSandbox`, and the validator turns a refusal into a `sandbox_workspace` diagnostic at `…/resource/config` naming the fix, for every binding that declares a sandbox.

The provider checks it again at acquisition: the host marks a sandboxed binding's request `Sandboxed`, and the provider refuses anything else (`ErrUnsupported`) before it makes or runs anything. A clone's `Resource.Sandbox` (`git.CloneSandboxNeeds`) has the clone writable and its `.git/config`, `.git/hooks` and `.git/info` read-only, so the agent can't give the host-side git a hook, fsmonitor or filter to run or an include to follow. The provider makes `hooks` and `info` before handing a clone out, because bubblewrap can't protect a path that doesn't exist. A worktree's `Resource.Sandbox` is nil, so a sandbox refuses it even if validation was bypassed.

## Lease ownership, recovery and cleanup

The provider keeps a private ledger (`<state>/resources/git/leases/<ref>.json`, written atomically with fsync) and owns its workspace root (`<state>/resources/git/workspaces/<ref>`). References are deterministic: a hash of mode with scope plus execution, or scope plus Work for `work` and `clone`. Ledger states are `acquiring → active | setup_failed → retained → (removed) | cleanup_failed`. Environment values are never stored. Per-lease and per-repository locks serialize concurrent users and `git worktree` registration.

- **Around executor launch.** The shell executor persists its recovery intent, then acquires, then starts the process. A crash before the process starts leaves an unadoptable attempt. Core fails it, the host confirms the stop, and `Reconcile` then sees a finished, failed owner.
- **Daemon restart.** A live process is adopted without relaunch and keeps running in its workspace. Nothing is removed while the owner is not done. A new provider instance reads the same ledger, and `Lookup` and re-`Acquire` return the existing lease.
- **Release.** Shared leases are dropped when their execution is done, and the checkout is never removed. A fresh workspace is removed when its execution succeeded (completed, waiting or handoff). Otherwise it is retained (failed or cancelled run, failed setup or an unknown owner). A work or clone workspace is removed when its Work completes and retained when the Work fails or is cancelled. Retention lasts `retain_failed_seconds` (default one day), and removal follows on a later pass.
- **Removal.** Removal first stops stray processes whose command line names the workspace or whose working directory is inside it, with their descendants. It spares the daemon and its ancestors and is the backstop for anything that escaped the execution's process group. Removal then runs `git worktree remove --force`, deletes the directory (only inside the provider's root) and prunes. A clone is deleted once its branch is saved (see [clone workspaces](#clone-workspaces)). Branches are always kept.
- **Failed cleanup.** A failed removal becomes `cleanup_failed`, is reported in the reconcile result and the doctor `resources` check (`warn`), and is retried on every pass, including after a restart.

The daemon reconciles every `ResourceInterval` (default 5s) in the background. Doctor shows `ok`, `info retained` with each retained lease and its expiry, or `warn cleanup_failed/reconcile_failed`.

## Disk safety and the spare-workspace sweep

- **Full-disk refusal.** Before it makes a `work`, `fresh` or `clone` workspace (agent and job checkouts alike), the provider measures the target filesystem at the workspace root and sizes the checkout (`git ls-tree -r -l` of the start ref), plus, for a clone, the objects it copies. It refuses when the checkout would leave less free than `100 − disk_full_percent` of the disk keeps, counting at most `disk_reserve_bytes`. The defaults are 90% and 10 GiB. `disk_full_percent: 100` turns the check off. A disk it can't read doesn't stop it. Re-acquiring an existing workspace and `shared` mode need no room. The refusal is a `NoSpaceError` wrapping the public `extension.ErrResourceExhausted` (protocol code `resource_exhausted`, which carries the message). It says how much is free, what the checkout needs and what is kept, then lists which of the scope's workspaces can go: those retained after their owner finished, or with failed cleanup, plus workspace directories no lease holds. Nothing is created and the lease is dropped, so trying again is refused the same way until space is freed. The executor's normal retry budget still applies. Shell and Claude Code report it as a structured failed result, `workspace_no_space`, whose summary is the message, and the run log carries it too. The free-space probe is injectable (`Options.DiskSpace`; the daemon's `ResourceDiskSpace`), so tests never fill a disk.
- **Partial-failure cleanup.** When creation fails partway, for example when git leaves a half-made checkout on a full disk, the provider stops anything in the directory, runs `git worktree remove --force`, deletes the directory and prunes. It then drops the lease, so the workspace is never reused as if it were whole. If that cleanup itself fails, the lease becomes `cleanup_failed` and `Reconcile` retries it. A partial directory left by a crash is cleared before the next creation at that path.
- **Sweep.** The optional public `extension.ResourceSweeper` covers spare workspaces that owner-driven release doesn't reach. The daemon calls it every `--sweep-interval` (default 6h; the first sweep runs one interval after start). The Git provider's `Sweep` removes these:
  - workspaces of finished Work it still keeps: a failed or cancelled Work's, inside its retention, and a fresh run's once both its execution and its Work are done
  - workspaces whose owner no record holds
  - directories under its workspace root that no lease holds

  The last two must be idle for a day (directory mtime). The sweep never removes a workspace that has uncommitted changes (copied files aside), that has commits on `HEAD` no remote-tracking branch has (with no remote, every commit counts), that git can't read, or that a running process uses (by command line or working directory). It reports each of these as kept, with `uncommitted`, `unpushed`, `unreadable` or `in_use`. Branches are always kept. Leases that are acquiring or failed cleanup, and shared checkouts, are left to `Reconcile`. The doctor `resources` check adds the last sweep's time and counts to its detail, with a `swept <mode> <ref> <reason>` item for each removal and a `kept <mode> <ref> <reason>: <why>` item for each kept workspace. Kept workspaces show as `info spare_kept` when nothing else needs attention, and a failed sweep as `warn sweep_failed`.

## Copy, setup and environment

- **`copy`.** Checkout-relative clean paths (never absolute, `..` or `.git/…`) are copied into `work`/`fresh`/`clone` workspaces on every acquisition, so a reused workspace picks up keys added since. A destination that is a symlink, or whose directory resolves outside the workspace, is skipped and logged. Unchanged files are not rewritten. File modes are preserved.
- **`setup` / `setup_when`.** Setup runs `/bin/sh -c` in the workspace when the command or any watched file changed since it last succeeded there. A stamp in the worktree's git directory records this. With watched files configured and none present, setup is skipped. Setup runs in its own process group with `setup_timeout_seconds` (default 1800), and the whole group is stopped on timeout. Its output goes to the request log. Setup is trusted operator configuration and runs with the daemon environment minus `OS_*`, plus the provider layer. `shared` mode copies nothing and runs no setup.
- **Environment precedence** follows B05: sanitized process baseline → provider layer → binding `config.env` → binding secrets → trusted `OS_*`. The provider layer is the opted-in (`dotenv: true`) checkout `.env`, then the signing policy. It never contains `OS_*`, and the shell executor additionally drops any reserved or invalid name from it. With no resource there is no `.env` layer.
- **Signing.** With `sign: false` (the default) the layer sets `GIT_CONFIG_COUNT/KEY_n/VALUE_n` to `commit.gpgsign=false` and `tag.gpgsign=false`, so commits never wait on a signer. A `.env` cannot undo this. `sign: true` leaves the checkout's own configuration.
- **Credentials.** The provider's git commands use the checkout's own git configuration and credential helpers, with `GIT_TERMINAL_PROMPT=0` (never prompting) and `LC_ALL=C`. No token is embedded in remotes, written into a workspace or added to the environment. Connection credentials are not copied into resources (B05). URL userinfo is redacted from any git output that is logged or returned. The provider resolves no secrets.
- **Remote and base.** `remote` defaults to `origin`. `base_branch`, when empty, is taken from `refs/remotes/<remote>/HEAD`, then `ls-remote --symref`, then `main`.

## Git provider config

`bindings[].resource = {"provider": "git", "config": {…}}`, strictly validated at config load through `config.Registry.ResourceConfig`; diagnostics never echo values.

| Field | Meaning |
| --- | --- |
| `checkout` | Required main checkout (work-tree root). A relative path resolves against the config file's directory. |
| `mode` | Default mode when the executor requests none: `shared` (default), `work`, `fresh` or `clone`. An executor's own request overrides it, and `config check` warns when it does ([modes](#modes-from-the-real-github-needs)). |
| `remote`, `base_branch` | Remote name (default `origin`) and base branch (default as above). |
| `branch_template` | `work` and `clone` branch name with `{work}`/`{scope}` (sanitized IDs). Default `agent/work-{work}`. |
| `sign` | Keep signing (default false). |
| `copy`, `setup`, `setup_when`, `setup_timeout_seconds` | See above. |
| `dotenv` | Opt into the checkout `.env` layer (default false). |
| `retain_failed_seconds` | Failure retention (default 86400). |
| `disk_full_percent`, `disk_reserve_bytes` | Full-disk refusal threshold (default 90; 100 turns it off) and the most it keeps free (default 10 GiB). See [disk safety](#disk-safety-and-the-spare-workspace-sweep). |

## Shell executor

A shell binding with a resource runs in `Resource.Dir` with the provider layer. `dir` and a bound workspace together are rejected (`invalid_config`). Acquisition happens after recovery intent and before the process deadline starts. The provider's explanation (fast-forward, behind/stale reason, copied files, setup output) is written to the run's log artifact. The structured result carries `workspace: {ref, schema, data}`. A setup failure or unavailable workspace becomes a structured failed result with no process (`workspace_setup_failed`, `workspace_unavailable`, and `workspace_no_space` for a full-disk refusal), so no uncertain hold remains. The `workspace_unavailable` summary carries the first line of the provider's error. The shell executor advertises the mechanical `workspace` capability.

## Run summaries

`Resource.Summary` is the provider's one-line description for history. The Git provider's summaries read like these:

- `main checkout, fast-forwarded 2 commit(s) to origin/main`
- `main checkout, 1 commit(s) behind origin/main: another execution is using the checkout`
- `fresh worktree at origin/main 1a2b3c4`
- `worktree on branch agent/work-x, reused`
- `clone on branch agent/issue-12, reused`

Executors report the acquired resource (never its environment) as `extension.Result.Workspace`, and run history and the TUI show it.

## Agents and workspaces

Workspaces are made by executor acquisition and removed by host reconcile, so agents need no worktree commands: turns run in the acquired workspace and no workspace operation is registered ([engineering tools](../engineering-tools/)).

## Verification

- [Provider tests](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/provider_test.go):
  - shared catch-up with dirty-elsewhere, dirty-in-the-way, other-branch, diverged and no-remote cases, with nothing stashed or rewritten
  - concurrent shared users only compare
  - fresh at the remote head, isolation, idempotent re-acquire, `Lookup` ownership, success removal, failure retention and expiry
  - eight concurrent fresh acquisitions (two per execution)
  - work branch reuse keeping local commits and edits, a retained workspace taken over by a new Work on its branch, copied-file refresh, Work-completed removal with the branch kept, failed-Work retention and a branch busy elsewhere
  - tracking a branch from a configured non-origin remote
  - `setup_when` skip, run-once and rerun, setup failure retained after success, and setup timeout killing its group
  - `.env`/signing layer precedence, with no values persisted
  - restart with a crashed `acquiring` lease, failed cleanup retried after restart, unknown owners retained
  - strict config, symlink copy escapes and credential redaction
- [Clone tests](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/clone_test.go):
  - a clone created on its branch with the real remote, the checkout's remote-tracking branches and identity, and no path under it resolving (through symlinks, by `realpath`) into the checkout's `.git`; reused across attempts with its commit and edit kept; pushed to the real remote; its unpushed branch saved into the checkout when the completed Work's clone is removed
  - its objects copied, not hardlinked (`os.SameFile`), then the checkout's branch deleted, `gc --prune=now`, `repack -a -d -f` and `prune` run, and the clone still passing `fsck --full`, committing, pushing and being reused
  - a hostile clone config: an fsmonitor; hooks (`reference-transaction`, `post-index-change`, `pre-auto-gc` and others) through `core.hooksPath`; a required filter set directly and another through `include.path`, with edited files to trigger them; and `gc.auto`. It also gives the clone an `insteadOf` that rewrites the checkout's path to `ssh://` with a `core.sshCommand`. Plain `git status` and `git fetch` run them, but re-acquiring, a provider fetch from the checkout, the sweep and removal with its branch save never do
  - failed-Work retention and expiry, the sweep removing a finished clean clone and an idle orphan and keeping an unpushed one, take-over of a retained clone and a live Work's branch refused
  - the disk refusal counting the clone's files plus all the checkout's objects
  - `ValidateSandbox` allowing only `clone` without setup, and remote URLs losing embedded credentials
  - `TestSandboxedAcquire`: a sandboxed request for `work`, `fresh`, `shared` or a clone with setup refused with nothing made and setup not run; a clone's sandbox needs, its `hooks` and `info` made, a new branch's upstream set, and an agent-written `commondir` ignored by the provider's git
  - the [sandboxed sweep test](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/sandbox_test.go): a process in an srt sandbox, tied to the workspace only by its working directory, found and stopped
- [Mode warning tests](https://github.com/open-conduit/open-conduit/blob/main/internal/config/modes_test.go) (`TestModeWarnings`, with the offline Claude Code and git check in `internal/bootstrap/modes_test.go`): a resource mode the executor overrides warned once at its declaring scope, and agreeing, unset or unhooked modes quiet.
- [Sandboxed binding tests](https://github.com/open-conduit/open-conduit/blob/main/internal/config/config_test.go) (`TestSandboxedResource`, `TestSandboxBinding`): the executor's requested mode (or the resource default) reaching the provider's check and a refusal reported at `resource/config`.
  - the ported [stray-process test](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/kill_unix_test.go)
- [Shell broker test](https://github.com/open-conduit/open-conduit/blob/main/executors/shell/shell_test.go): workspace cwd, layer order and reserved-name drop, the result/log explanation and structured failures.
- [Scheduler hook test](https://github.com/open-conduit/open-conduit/blob/main/integration/scheduler_resources_test.go): the broker is passed opaquely, a nil broker without a resource, and a binding failure fails the attempt before any executor call.
- [Composed daemon test](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_workspace_test.go), using local bare repositories only:
  - a fresh run at the remote head with the copied `.env` layer and setup survives a daemon SIGKILL by adoption (one attempt), and its worktree is removed and deregistered after success
  - a failed fresh run is retained and reported by doctor
  - a shared run fast-forwards the main checkout and keeps an unrelated local edit
  - a scope with no resource runs with no `.env` layer
- [Disk safety tests](https://github.com/open-conduit/open-conduit/blob/main/extensions/git/disk_test.go):
  - full-disk refusal through the injected probe, with the actionable message, the scope's spare workspaces and nothing made or leased
  - reuse without a check, `disk_full_percent: 100` and a capped reserve
  - a half-made workspace cleared, deregistered and unleased
  - the sweep removing finished, owner-unknown and orphaned clean workspaces (branches kept) and keeping dirty, unpushed, in-use, unreadable, live and still-retained ones
- [Disk safety drill](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_workspace_disk_test.go) on the composed daemon: a structured `workspace_no_space` refusal with nothing run, then the sweep removing a cancelled Work's workspace and an idle orphan while keeping a dirty orphan, all reported by doctor.
- [Job tests](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_job_parity_test.go): `TestDaemonJobParity` covers a job catching up a stale shared checkout (the ported `TestJobCatchesUpTheMainCheckout`), and [`TestDaemonWorkflowC`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_workflow_c_test.go) the fresh-checkout job.

## Limitations

- Windows has no process-group setup cleanup or stray-process search (B06).
- A clone costs a full copy of the checkout's objects on disk.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **03-02** recorded decision B07, resolving the open "workspace provider contract" and "workspace fresh/shared, copy/setup and cleanup" questions from planning. It delivered the contract, the Git provider and the shell executor as its first consumer. At that point Claude Code's binding to `work`, the agents' view of workspaces and pending-run coalescing for in-checkout jobs were deferred.
- **03-03** bound Claude Code to `work`: a new segment acquires and a resumed segment uses `Lookup`.
- **03-04** settled that agents need no worktree commands and that no workspace operation is registered.
- **05-01** resolved pending-run coalescing for in-checkout jobs (one standing Work per job).
- **05-02** added `Resource.Summary` and `extension.Result.Workspace`, shown in run history and the TUI, and moved jobs onto `fresh` and `shared` through ordinary bindings.
- **08-02** added worktree disk safety: full-disk refusal, partial-failure cleanup, `ResourceSweeper` and the spare-workspace sweep, with the disk safety tests and drill.
- **#163** added the `clone` mode and the sandboxed-binding refusal, the second prerequisite of the B09 sandbox (#160, with evidence from the #161 spike).
- **#164** (B09) made the sandboxed-binding check run for every binding that declares a sandbox, added the refusal of `setup` and the provider's own refusal of a sandboxed request, the clone's sandbox needs, the pinned git directories and a new branch's upstream.
