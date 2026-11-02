---
title: "Backup, restore and state recovery"
---

This page covers recovering a daemon's progress from one bad Event or a wedged dependency, and Open Conduit's offline recovery commands. The offline commands are backup bundles with verify and restore, a locked local credential reset (`auth recover`) and a read-only state diagnosis (`state check`). This page describes them, with the runbooks for SQLite migration recovery and corrupted state. The commands live in [`internal/backup`](https://github.com/open-conduit/open-conduit/tree/main/internal/backup) and [`internal/cli`](https://github.com/open-conduit/open-conduit/tree/main/internal/cli).

## A stuck Event or a wedged dependency

The daemon is built so that one bad input can't stop it:

- **A route that keeps failing.** An Event whose route errs, times out or panics is retried with a growing backoff and, after five failures over at least 15 minutes or at once when retrying can't help, routed as `failed` ([route failures](../routing-lifecycle/#route-failures)). The Work it was holding then takes its later input, and an agent that has finished can complete. The failed Event stays in the Work's history with its error. `open-conduit events reroute ID` accepts it again as a copy once the cause is fixed.
- **An Event stuck for another reason.** Doctor's `routing` check lists Events unrouted for over ten minutes, such as one whose identity no longer resolves. `open-conduit events skip ID --reason …` routes it as skipped so its Work moves on; the web view's Work page and the TUI's run detail offer the same control.
- **A hung extension or container runtime.** A route's decision and the Docker provider's `info`, `ps` and `inspect` each have a deadline (`serve --call-timeout`, default 5s), and container reconcile runs off the host loop. A deadlocked extension or a wedged Docker Desktop therefore can't stop routing, timers, wakes, recovery or new starts.
- **A panic in extension code.** A route handler's panic fails that Event's route. A panic in a projection, extension task, resource pass or container reconcile is recovered, logged at error with its stack and reported as `panic`; the next pass runs it again.

## Backup, restore and credential recovery

These commands are offline. They take the state directory's exclusive daemon lock, so they refuse while a daemon owns it ("stop the daemon first"). Use `doctor` while it runs.

```sh
open-conduit backup create ~/backups/oc-2026-10-26        # an absent path; the database snapshot plus referenced artifacts, with a SHA-256 manifest
open-conduit backup verify ~/backups/oc-2026-10-26        # restores into a private temporary directory, checks every digest and reference, removes it
open-conduit backup restore ~/backups/oc-2026-10-26 --to ~/.local/state/open-conduit-restored
open-conduit --data-dir ~/.local/state/open-conduit-restored serve
open-conduit auth recover                                 # a lost or ambiguous operator token: revokes every operator credential, writes a fresh one
```

- **What a bundle holds.** A bundle holds `conduit.db` and the artifacts the database references ([backup bundles](../backup-bundles/)). Keep bundles owner-only: they hold private Work, Events, config snapshots and executor continuity.
- **What a bundle does not hold.** It holds no credentials, config, secrets, executor run directories (`executors/`), workspaces (`resources/git/`) or logs.
- **Restore.**
  - A restore never replaces an existing directory.
  - It fences every backed-up lease, so an old execution credential can never commit.
  - It revokes the bundle's operator credential and writes a fresh `private/operator.token` (`auth recover` under the lock).
  - A process running at backup time is not adopted after a restore: its attempt recovers or retries through ordinary recovery. Take bundles after a drain.
  - Worktrees of the old state directory stay registered in the checkout: run `git worktree prune` once the old directory is gone.
- **Timing.** Back up before every binary upgrade. A daily bundle with the daemon briefly stopped is the minimal routine; online bundles through the management API are not provided.

## SQLite migration recovery

Schema migrations run inside one immediate transaction at open (`internal/storage/sqlite`). A failure rolls back and leaves the database unchanged ("migration rolled back"). Open refuses three cases:

| Symptom | Cause | Recovery |
| --- | --- | --- |
| `schema N is newer than supported` (`state check`: `schema_newer`) | A newer binary migrated the database, and an older binary was started on it | Run the newer binary again. To go back to the older binary, restore the bundle taken before the upgrade into a new directory. Never edit `user_version`. |
| `unversioned nonempty database; explicit migration required` or `schema metadata mismatch` (`schema_unrecognized`) | The file is not an Open Conduit database, or was altered by hand | Point `--data-dir` at the right directory, or restore the latest bundle. |
| `migration rolled back: …` | The upgrade failed part way (disk full, I/O) | Fix the cause and start again: the old schema is intact. If it keeps failing, restore the pre-upgrade bundle and report the defect. |

The upgrade routine is: stop the daemon, run `backup create` and `backup verify`, install the new binary, then start it. Keep the old binary and the bundle until the observation window passes.

## Corrupted-state diagnosis

Run `open-conduit state check [--json]` with the daemon stopped. It examines a private copy of the database, so diagnosis never changes the state. It reports:

| Check | Meaning |
| --- | --- |
| `lock` | Fails when a daemon owns the directory (`in_use_or_unsafe`), or when its permissions are not owner-only. |
| `socket` | A leftover socket from an unclean stop (info: the next start removes it after its ownership checks). |
| `database` | `schema_1` (integrity and foreign keys pass), or `corrupt` / `schema_newer` / `schema_unrecognized` / `open_failed`, with the remedy. |
| `artifacts` | Referenced artifacts missing, not counting content retention released on purpose. A backup refuses missing content, so restore the directory or the latest bundle. |
| `timers` | The number of durable timers that resume on start. |

Per scope it also gives Work by state, live executions, active waits, and `drained` (no running or waiting Work, live execution or active wait), which is the drain check before an upgrade or restore.

Recovery order:

1. Copy the whole state directory aside (daemon stopped).
2. If `database` fails, restore the newest bundle that `backup verify` accepts into a new directory, and start there. Reconcile then re-reads the services: work accepted after the bundle is re-derived from them, apart from declared one-shot deliveries.
3. If only artifacts are missing, the coordination state is sound. The run content of those executions is lost; take a fresh bundle once `state check` passes.
4. A stale socket or lock needs nothing: start the daemon.

## Evidence

`TestDaemonMigrationRehearsal` (in [`integration/daemon_migration_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_migration_test.go), part of `make drill`) exercises the commands on the composed daemon:

- offline commands refused beside the daemon;
- `state check` drained after a graceful stop;
- a backup created and verified;
- a restore into a new directory with a fresh credential (the bundle's refused) and no repeated external write, claim or job run after a reconcile;
- corrupted and newer-schema databases diagnosed;
- a tampered bundle refused.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 06-02 delivered the offline recovery commands (backup create, verify and restore, `auth recover`, `state check`) and the runbooks above, with `TestDaemonMigrationRehearsal` as its drill.
