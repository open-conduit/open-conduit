# Optional workspaces and the Git provider

Decision B07 · ticket 03-02. This resolves the PRD's open "workspace provider contract" and the summary's "workspace fresh/shared, copy/setup and cleanup" decision. The public contract is [`extension/resource.go`](../../extension/resource.go). The first provider is [`extensions/git`](../../extensions/git). [`executors/shell`](../../executors/shell) is its first consumer, and [`internal/bootstrap`](../../internal/bootstrap/resources.go) composes them. The Open Supervisor v1 `internal/worktree` package and the fresh-checkout/catch-up parts of `internal/engine/jobs.go` (pinned `c2461e55`) were the behavioral reference.

## What core knows

Core knows nothing about repositories, branches, worktrees or filesystem layout. A Scope may have no resource, and absence is valid. The runtime's only involvement is one generic hook, `SchedulerOptions.Resources`. Immediately before `Run`/`Recover`/`Resume`, outside coordination transactions (like `Secrets`), the host binds the attempt's provider. Core then places the opaque `extension.ResourceBroker` in `ExecutionContext.Resources`, which is nil when the binding declares no resource. A binding failure fails the attempt with `ErrResourceUnavailable` before any executor call. Core never calls a provider, and no lease, reference or path enters coordination storage. The boundary checker still forbids runtime from importing `extensions/git`.

## The contract

| Type | Role |
| --- | --- |
| `ResourceProvider` | Implemented by an extension: `Acquire`, `Lookup`, `Reconcile`. |
| `ResourceBroker` | What an executor sees: `Acquire(ResourceRequest)`, `Lookup(ref)`. `BindResources(provider, owner, config)` creates it. Only the host binds, so an executor cannot act for another owner. |
| `ResourceOwner` | Trusted `{ScopeID, WorkID, ExecutionID}` taken from the attempt, never from the executor. |
| `ResourceRequest` | Executor-chosen `Mode` (empty selects the binding's default), provider-defined opaque `Params`, and an optional `Log` writer for human-readable progress. |
| `Resource` | `Kind`, opaque lease `Ref`, `Dir`, `Env` (the provider's environment layer, never persisted) and `Data` (a description for history). |
| `ResourceStatusFunc` | Host-reported durable owner state for `Reconcile`. An execution is *done* only when it is terminal **and** its physical resource exclusion is confirmed (`AttemptState.Excluded`). |

These are the guarantees a provider must meet:

1. Acquire is idempotent per owner and mode. A resumed segment or a retried launch gets the same lease and never a duplicate resource.
2. A lease is durably recorded before anything is created, so a crash at any point leaves something `Reconcile` can find.
3. `Lookup` never creates, fetches or runs setup, and it refuses leases the caller does not own (`ErrResourceNotFound`).
4. A setup failure returns the usable resource together with `ErrResourceSetup`, so it can be inspected.
5. `Reconcile` is repeatable and restart-safe. It releases leases whose owners finished, applies retention and retries failed cleanup.

## Modes, from the real GitHub needs

| Mode | Owner | v1 source | Behavior |
| --- | --- | --- | --- |
| `shared` (default) | execution | non-fresh jobs in the main checkout | Runs in the scope's main checkout. The only change it ever makes is a fast-forward of the base branch, and only when no other execution holds the checkout. It never resets, stashes, merges or rebases. Git refuses an update that would overwrite an uncommitted edit, and that is reported. The run is told how far behind it is and why: `on feature, not main`, `uncommitted changes to files the update touches`, `main has commits origin/main doesn't`, `another execution is using the checkout` or `couldn't fetch: …`. This appears in the log and in `Data` (`updated`, `behind`, `stale`, `dirty`). |
| `work` | Work | issue worktree on its own branch | Creates a worktree on a branch private to the Work, reused by each of its attempts until the Work is terminal. An existing local branch is checked out. A remote-only branch is tracked from the configured remote. Otherwise a new branch starts at `remote/base` (or the local base). Reuse never resets: local commits and uncommitted edits stay, and the run is told how many uncommitted paths it inherited. Copied files do not count. A branch checked out elsewhere fails clearly. Executors may request a branch through `Params` (`{"branch": …}`); otherwise `branch_template` applies. |
| `fresh` | execution | `fresh: true` jobs (`TestJobFreshCheckout`) | Creates a detached worktree at the base branch's freshly fetched remote head, isolated per execution. The main checkout never moves. |

Claude Code issue work (03-03) uses `work`, with `Lookup` after restart. Fresh-checkout and in-checkout jobs (05-02) use `fresh` and `shared`. Slack (04) and research work have no resource.

## Lease ownership, recovery and cleanup

The provider keeps a private ledger (`<state>/resources/git/leases/<ref>.json`, written atomically with fsync) and owns its workspace root (`<state>/resources/git/workspaces/<ref>`). References are deterministic: a hash of mode with scope plus execution, or scope plus Work for `work`. Ledger states are `acquiring → active | setup_failed → retained → (removed) | cleanup_failed`. Environment values are never stored. Per-lease and per-repository locks serialize concurrent users and `git worktree` registration.

- **Around executor launch.** The shell executor persists its recovery intent, then acquires, then starts the process. A crash before the process starts leaves an unadoptable attempt. Core fails it, the host confirms the stop, and `Reconcile` then sees a finished, failed owner.
- **Daemon restart.** A live process is adopted without relaunch and keeps running in its workspace. Nothing is removed while the owner is not done. A new provider instance reads the same ledger, and `Lookup` and re-`Acquire` return the existing lease.
- **Release.** Shared leases are dropped when their execution is done, and the checkout is never removed. A fresh workspace is removed when its execution succeeded (completed, waiting or handoff). Otherwise it is retained (failed or cancelled run, failed setup or an unknown owner). A work workspace is removed when its Work completes and retained when the Work fails or is cancelled. Retention lasts `retain_failed_seconds` (default one day, as in v1), and removal follows on a later pass.
- **Removal.** Removal first stops stray processes whose command line names the workspace or whose working directory is inside it, with their descendants. It spares the daemon and its ancestors and is the backstop for anything that escaped the execution's process group. Removal then runs `git worktree remove --force`, deletes the directory (only inside the provider's root) and prunes. Branches are always kept.
- **Failed cleanup.** A failed removal becomes `cleanup_failed`, is reported in the reconcile result and the doctor `resources` check (`warn`), and is retried on every pass, including after a restart.

The daemon reconciles every `ResourceInterval` (default 5s) in the background. Doctor shows `ok`, `info retained` with each retained lease and its expiry, or `warn cleanup_failed/reconcile_failed`.

## Copy, setup and environment

- **`copy`.** Checkout-relative clean paths (never absolute, `..` or `.git/…`) are copied into `work`/`fresh` workspaces on every acquisition, so a reused workspace picks up keys added since (v1 `Refresh`). A destination that is a symlink, or whose directory resolves outside the workspace, is skipped and logged. Unchanged files are not rewritten. File modes are preserved.
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
| `mode` | Default mode when the executor requests none: `shared` (default), `work` or `fresh`. |
| `remote`, `base_branch` | Remote name (default `origin`) and base branch (default as above). |
| `branch_template` | `work` branch name with `{work}`/`{scope}` (sanitized IDs). Default `agent/work-{work}`. |
| `sign` | Keep signing (default false). |
| `copy`, `setup`, `setup_when`, `setup_timeout_seconds` | See above. |
| `dotenv` | Opt into the checkout `.env` layer (default false). |
| `retain_failed_seconds` | Failure retention (default 86400). |

## Shell executor

A shell binding with a resource runs in `Resource.Dir` with the provider layer. `dir` and a bound workspace together are rejected (`invalid_config`). Acquisition happens after recovery intent and before the process deadline starts. The provider's explanation (fast-forward, behind/stale reason, copied files, setup output) is written to the run's log artifact. The structured result carries `workspace: {ref, schema, data}`. A setup failure or unavailable workspace becomes a structured failed result with no process (`workspace_setup_failed`, `workspace_unavailable`), so no uncertain hold remains. The shell executor now advertises the mechanical `workspace` capability.

## Verification

- [Provider tests](../../extensions/git/provider_test.go):
  - shared catch-up with dirty-elsewhere, dirty-in-the-way, other-branch, diverged and no-remote cases, with nothing stashed or rewritten
  - concurrent shared users only compare
  - fresh at the remote head, isolation, idempotent re-acquire, `Lookup` ownership, success removal, failure retention and expiry
  - eight concurrent fresh acquisitions (two per execution)
  - work branch reuse keeping local commits and edits, copied-file refresh, Work-completed removal with the branch kept, failed-Work retention and a branch busy elsewhere
  - tracking a branch from a configured non-origin remote
  - `setup_when` skip, run-once and rerun, setup failure retained after success, and setup timeout killing its group
  - `.env`/signing layer precedence, with no values persisted
  - restart with a crashed `acquiring` lease, failed cleanup retried after restart, unknown owners retained
  - strict config, symlink copy escapes and credential redaction
  - the ported [stray-process test](../../extensions/git/kill_unix_test.go)
- [Shell broker test](../../executors/shell/shell_test.go): workspace cwd, layer order and reserved-name drop, the result/log explanation and structured failures.
- [Scheduler hook test](../../integration/scheduler_resources_test.go): the broker is passed opaquely, a nil broker without a resource, and a binding failure fails the attempt before any executor call.
- [Composed daemon test](../../integration/daemon_workspace_test.go), using local bare repositories only:
  - a fresh run at the remote head with the copied `.env` layer and setup survives a daemon SIGKILL by adoption (one attempt), and its worktree is removed and deregistered after success
  - a failed fresh run is retained and reported by doctor
  - a shared run fast-forwards the main checkout and keeps an unrelated local edit
  - a scope with no resource runs with no `.env` layer

## Deferred

- 03-03 binds Claude Code to `work`: a new segment acquires and a resumed segment uses `Lookup` ([Claude Code executor](claude-code-executor.md)).
- v1's agent-driven `worktree setup/remove` toolbelt commands are replaced by executor acquisition and host reconcile. 03-04 decided the playbook needs neither: turns run in the acquired workspace and no workspace operation is registered ([engineering tools](engineering-tools.md)).
- Legacy `OPEN_SUPERVISOR_BASE_BRANCH`, `OPEN_SUPERVISOR_WORKTREE` and similar variables belong to explicit compatibility adapters (C06). `Resource.Data` carries the base, branch and directory.
- Pending-run coalescing for in-checkout jobs stays with 05-01/05-02.
- Windows has no process-group setup cleanup or stray-process search (B06).
