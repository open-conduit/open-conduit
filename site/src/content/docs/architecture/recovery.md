---
title: "Backup, restore and state recovery"
---

This page covers recovering a daemon's progress from one bad Event or a wedged dependency, and Open Conduit's offline recovery commands. The offline commands are backup bundles with verify and restore, a locked local credential reset (`auth recover`) and a read-only state diagnosis (`state check`). This page describes them, with the runbooks for SQLite migration recovery and corrupted state. The commands live in [`internal/backup`](https://github.com/open-conduit/open-conduit/tree/main/internal/backup) and [`internal/cli`](https://github.com/open-conduit/open-conduit/tree/main/internal/cli).

## A stuck Event or a wedged dependency

The daemon is built so that one bad input can't stop it:

- **A route that keeps failing.** An Event whose route errs, times out or panics is retried with a growing backoff and, after five failures over at least 15 minutes or at once when retrying can't help, routed as `failed` ([route failures](../routing-lifecycle/#route-failures)). The Work it was holding then takes its later input, and an agent that has finished can complete. The failed Event stays in the Work's history with its error. `open-conduit events reroute ID` accepts it again as a copy once the cause is fixed.
- **An Event stuck for another reason.** Doctor's `routing` check lists Events unrouted for over ten minutes, such as one whose identity no longer resolves. `open-conduit events skip ID --reason …` routes it as skipped so its Work moves on; the web view's Work page and the TUI's run detail offer the same control.
- **A hung extension or container runtime.** A route's decision and the Docker provider's `info`, `ps` and `inspect` each have a deadline (`serve --call-timeout`, default 5s), and container reconcile runs off the host loop. A deadlocked extension or a wedged Docker Desktop therefore can't stop routing, timers, wakes, recovery or new starts.
- **A clock step or a sleep past the lease.** A renewal after the wall clock stepped back (an NTP correction, a VM resume) keeps the later of the stored and the new expiry, so it never fails or shortens a healthy agent's lease. When the host sleeps or stalls past the lease and the renewal is lost, an attempt that is still running with its Work open is left alone: the next poll recovers and adopts the same process (or a newer epoch already has). Only an attempt that has ended (cancelled, timed out, failed, parked) has its process stopped. The daemon logs `lease lost; leaving the running attempt for recovery to adopt`, and the adopted attempt's history shows a `recovered` segment.
- **A slow launch.** An attempt's lease is renewed from its claim, so a launch that takes many lease durations (a long clone, an image pull, a host at several loads per CPU) keeps it. Launches run off the host loop, so a slow one holds up neither routing nor any other Work. When a launch does lose its lease before anything is pinned, the next poll relaunches it under a higher epoch instead of failing it. The daemon logs `lease lost while launching; leaving the attempt for recovery`, no retry is used, and the run's history shows a `relaunched` segment. Each run's launch phases (workspace, container plan and creation, pin, executor) and how long each took are in its history and on the run page, so a slow step can be seen.
- **A panic in extension code.** A route handler's panic fails that Event's route. A panic in a projection, extension task, resource pass or container reconcile is recovered, logged at error with its stack and reported as `panic`; the next pass runs it again.

## Backup, restore and credential recovery

These commands are offline. They take the state directory's exclusive daemon lock, so they refuse while a daemon owns it ("stop the daemon first"). Use `doctor` while it runs.

```sh
open-conduit backup create ~/backups/oc-2026-10-26        # an absent path; the database snapshot plus referenced artifacts, with a SHA-256 manifest
open-conduit backup verify ~/backups/oc-2026-10-26        # restores into a private temporary directory, checks every digest and reference, removes it
open-conduit backup restore ~/backups/oc-2026-10-26 --in-place   # into the daemon's own state directory, moving what it replaces aside
open-conduit backup restore ~/backups/oc-2026-10-26 --to ~/oc-copy   # or into a new directory, to inspect or move it
open-conduit auth recover                                 # a lost or ambiguous operator token: revokes every operator credential, writes a fresh one
```

- **What a bundle holds.** A bundle holds `conduit.db` and the artifacts the database references ([backup bundles](../backup-bundles/)). The database holds every committed config generation, secret references included, each self-contained, so a restored daemon starts from the active one without a config file. With the file missing, doctor reports `disk_missing` and `open-conduit config export` writes the active generation back out ([config management](../config-management/)). It also holds the extensions' KV and every Execution's recovery metadata. Keep bundles owner-only: they hold private Work, Events, config and executor continuity.
- **What a bundle does not hold.**
  - Secret values: keyring entries, `secrets.json` beside the config file, and what `env:`, `cmd:` and provider references read. Doctor's `secrets` check shows the references that don't resolve.
  - Installed packages, which live beside the config file (`extension install`). A restore reports why the restored generation wouldn't validate against the packages installed here (`config_problems`); install the packages it names before starting.
  - The folders an `includes` entry names. The restored generation needs none of them, but `config export` writes the `includes` entries back, so a later `config apply` of that file reads them.
  - Extensions' private state directories (`extensions/`), which are host-local caches ([extension packaging](../extension-packaging/#writing-an-extension-with-the-sdk)).
  - The agents' own logins and sessions (`homes/`), executor run directories (`executors/`), workspaces (`resources/git/`) and logs. [Moving a daemon](#moving-a-daemon-to-another-machine) says which of these to carry. The daemon's logs in `logs/` rotate by size on their own (10 MB, five rotated files kept).
- **Restore.**
  - `--in-place` restores into the daemon's own state directory (`--data-dir`), the one to use to put a bundle back where the daemon runs ([runbook](#restoring-after-a-bad-release-or-lost-state)). `--to DIR` writes a new directory and never replaces an existing one.
  - It fences every backed-up lease, so an old execution credential can never commit.
  - It revokes the bundle's operator credential and writes a fresh `private/operator.token` (`auth recover` under the lock).
  - It reports the config generation it restored (`config_generation`) and validates it as the daemon will at start, against the packages installed beside the config file (`--config FILE`, default the operator's). Every diagnostic is listed under `config_problems`: a type no installed package provides (`unknown_registration`), a package that couldn't start, an entry a package rejects. With any, the daemon won't start from that generation (below).
  - A process running at backup time is not adopted after a restore: its attempt recovers or retries through ordinary recovery. Take bundles after a drain.
  - After `--to`, worktrees of the old state directory stay registered in the checkout: run `git worktree prune` once the old directory is gone. `--in-place` keeps the workspaces where they are, and the git provider reconciles them against the restored database (below).
  - The restored installation keeps its [container host label](../sandboxes/#the-host-label), the ID its containers carry, since it is the same installation: it still recognises the containers its runs left on a shared runtime, and reconciles them. Never run the old installation and the restored one at once against the same container runtime (a remote Docker context, an SSH runner, a cloud account): with one label, each removes the other's live runs as executions it doesn't know. Stop the old daemon for good before starting the restored one; to run both, restore into a fresh installation instead, which means starting without the bundle.
- **Timing.** The running daemon takes a bundle itself on a schedule, and another before its first start on a newer record format (both below), so an upgrade needs no manual step.

A bundle of a database with retired Events carries their records from the [event archive](../backup-bundles/#the-event-archive), and a restore puts them in `archive/` in the restored directory. A restore refuses a bundle that doesn't hold a record for every retired Event.

## Scheduled backups

While it runs, the daemon publishes a bundle of its own state every `--backup-interval` (default `24h`) into `--backup-dir` (default `<data-dir>/backups`) and keeps the newest `--backups-kept` (default 7). `--backup-interval 0` turns them off. Each is named `conduit-<UTC time>` (`conduit-20261102T090000Z`), and `backup verify` and `backup restore` take it as they do a bundle from `backup create`. It differs in one way: its artifacts are stored once, beside the bundles in that directory, and shared between them ([shared artifacts](../backup-bundles/#shared-artifacts)). To carry a bundle to another machine, use `backup create` or `backup fetch`, because copying a scheduled bundle's directory alone leaves its artifacts behind. The database is copied with SQLite's online backup API, which holds the store's connection only while it copies pages. The artifacts are copied after, under a retention hold the backup commits before its snapshot: runs keep reading and writing artifacts, the retention sweep keeps recording expirations but deletes no content until the hold is released, and artifact collection never deletes content an attempt references.

- The first runs at start when the directory has no scheduled bundle, then one interval after the newest; a restart doesn't add one. One already overdue at a start waits for the daemon's first request, or ten minutes from the first start that put it off ([quiescence](../execution-scheduling/#quiescence)).
- Only `conduit-<time>` directories are counted and pruned. Anything else in the directory, such as a `pre-upgrade-<time>` bundle or one you took by hand, is left alone.
- A failed backup is logged as a `backup` runtime diagnostic and tried again 15 minutes later. A staging directory a crash left (`.conduit-snapshot-*`) is removed at the next start.
- **Disk use.** Each scheduled bundle holds its own copy of the database and shares its artifacts, so the default seven take about seven times `conduit.db` plus one copy of the artifacts they reference. An hourly interval therefore costs the database plus new artifacts each time. Each backup still reads every artifact, to check the shared copy. The daemon logs each bundle's own size when it publishes it (`backup published … bytes=`), which leaves out the shared artifacts. Lower `--backups-kept`, or move `--backup-dir`, when that is too much for the disk.
- The default directory is on the same disk as the state. To survive losing the disk, point `--backup-dir` elsewhere (a flag of `serve` and of `service install`), or ship bundles to a [destination](../backup-bundles/#backup-destinations).

## The bundle taken before an upgrade

Going back a release that changed the stored format needs a bundle from before it: the newer binary raises the stored format when it opens the database, and the older one then refuses it (`records_newer`). So when a daemon finds a database whose record format or schema is older than its own, however the new binary was installed, it first takes a bundle of the database exactly as the older binary left it:

- **It is verified before it counts.** The bundle is composed under a staging name, restored into a private directory beside it to check it, and only then published as `pre-upgrade-<UTC time>` in `--backup-dir`. The check needs room for one more copy of the database and artifacts while it runs. A backup directory on a network filesystem can't hold the live database the check opens, so there the check uses the state directory. Nothing has been written to the database by then.
- **Without one, the daemon doesn't start.** If the bundle can't be taken or verified (a read-only or full backup directory, say), the start fails, says why, and leaves the database at its old format. Fix the directory, or run `open-conduit serve --no-upgrade-bundle` once to go ahead deliberately with no way back. `service install` refuses that flag, since a unit would then skip the bundle on every later upgrade.
- **Rotation never touches it.** Only `conduit-<time>` bundles rotate. The daemon keeps the newest `--upgrade-bundles-kept` pre-upgrade bundles (default 3) and, with `--upgrade-bundles-max-age` (default no limit), removes those older than that at each start. The newest is always kept. Each is a full copy, so count them in the disk you plan for.
- **It says what it is.** `backup verify` reports `pre_upgrade` with the `record_format`, `schema` and `from_build` it holds. The label is in the manifest's `Build` text, which an older binary reads and prints as it is, since older binaries refuse a manifest with a field they don't know. `from_build` is `unknown` for a database last run by a build from before this was recorded.
- **Signing.** It is signed with `settings.backup.signing_key` from the config file when that resolves without packages, as `backup create` is. Otherwise it is published unsigned, the log says so, and a restore with trust keys needs `--accept-unsigned`.
- **Offline commands don't raise the format first.** `backup create` from the newer binary bundles the database as it stands. `auth recover` refuses until the daemon has started once on this binary, or is run with the older binary.

## Draining a daemon

`open-conduit drain` stops the daemon starting attempts, so its state can be moved or backed up with nothing running. Attempts already running finish or park. Events, timers, Waits and input keep being recorded, and wait for whichever daemon next runs on the state.

```sh
open-conduit drain --reason "moving to a new host"   # begin; exits 1 until it is safe to move
open-conduit drain status                            # the same report, changing nothing
open-conduit drain end                               # start attempts again
```

The report (`GET /v1/drain`) gives `safe_to_move`, what is still `blocking` it (an attempt in flight, a stop not yet confirmed, an unsettled control or usage), what is `carried` in the state for the next daemon (unrouted Events, unread input, Waits still to judge), and each scope's attempts in flight, parked attempts and active Waits. A parked attempt holds no process, so it doesn't block a move: it resumes on its wake on the new daemon. Beginning and ending a drain are in the audit ledger.

A drain is durable, so a daemon stays drained until you run `drain end`, across a restart, a recreated container or a move of its directory to any path. A copy of the state can't tell that the daemon it came from has stopped, so starting one never ends the drain by itself, and two live copies can't both act on the same Work by accident. The one exception is a restore from a bundle, which ends the drain with an audit record because the restore is itself the deliberate step.

On [shared state](../domain-storage/#instance-lease) the instance lease already proves which daemon may run, so a successor doesn't ask you to confirm the old one has stopped. A drain still holds across a takeover, since it is your decision and not a statement about who is running: the successor starts nothing until you run `drain end`. A replica is a separate copy the lease can't reach, so it always starts drained.

## Moving a daemon to another machine

1. **Ask what won't travel.** `open-conduit portability` (admin) lists what is tied to this machine, and exits 1 until nothing is and the daemon is drained:
   - `secrets`: references that aren't `file:`, by location and scheme, never their text. Keychain entries and `cmd:` programs don't exist elsewhere.
   - `paths`: absolute paths in the active config generation (a checkout, an executor `dir`, a container's `read` and `write`).
   - `workspaces`: workspaces holding uncommitted or unpushed work, from each resource provider that implements `Unsaved`. One that doesn't is listed under `unchecked`.
   - `packages`: packages installed beside the config file, which a bundle doesn't carry.
   - `directories`: what is in the state directory that a bundle leaves out, and what is lost without it.
   - `drain`: the [drain report](#draining-a-daemon).
2. **Drain, and push what's unsaved.** `open-conduit drain`, until it exits 0.
3. **Write a config for the new machine**, with its paths and with secrets it can resolve.
4. **Stop the daemon and carry the state**, in one of two ways:
   - **A bundle:** `backup create`, then on the new machine `backup restore BUNDLE --to DIR --start-config FILE` (or `--in-place`). The config is published as a new generation inside the restore's stage, so one that doesn't validate, or that drops a scope with open Work, publishes nothing and the daemon can't come up on the old machine's paths. Without `--start-config` the restored daemon resumes the bundle's generation, whatever paths it holds.
   - **The whole directory.** Change its owner to the new user first (`chown -R`), because the daemon refuses a state directory it doesn't own. It starts drained, so nothing runs until you say so: if the active config's paths are wrong here, apply a corrected config, then run `drain end` once the old daemon is stopped for good.

   When the paths change and you also want what a bundle leaves out, restore with `--start-config` and copy the directories you want from the table below into the restored directory before the first start.
5. **Install the packages** the report named, and start the daemon on the new machine. A carried directory comes up drained, so run `drain end` once you've checked it (a restored bundle has already ended its drain). Never run the old one again: the two share a [container host label](../sandboxes/#the-host-label). A state path deep enough to take the socket past 100 bytes needs `--socket`.

Parked Work resumes on its wake and each timer fires once, including one that came due during the move.

What each directory is worth carrying:

| Directory | In a bundle | Without it |
| --- | --- | --- |
| `homes/` | No | A containerised agent's session is gone, and its next turn starts from the handoff note. Carried, the session resumes, including at a new path: when the workspace's old path no longer exists, the Claude Code executor refiles the session under the new one (`session_moved` in telemetry) |
| `executors/` | No | Attempts recover or retry. A non-container Claude transcript is outside the state directory anyway (`<CLAUDE_CONFIG_DIR>/projects/<absolute cwd>/`), so its next turn also starts from the note |
| `resources/` | No | Workspaces are made again from the remote, so unpushed work is lost. Carried, a clone is picked up where it lands and a worktree is [reattached or set aside](../workspaces/#a-workspace-that-moved-with-its-state-directory) |
| `extensions/` | No | Packages rebuild their caches |

The GitHub, Slack and schedule extensions keep no filesystem path in their KV (dispatch, progress, claim and run records name repositories, channels and Work), so nothing of theirs needs rewriting. Claude's continuity record keeps the last turn's `attach` directory and binary, which are stale after a move until the Work's next turn replaces them.

## Restoring after a bad release or lost state

Restore puts the bundle's database and artifacts back, and with the database the config generation the bundle was taken on. Installed packages and the config file, beside the config, aren't touched: while the restored generation validates, the daemon starts from it whatever the file says, and doctor reports the file as out of step (`pending_edit`) until you apply one. Applying the file replaces the restored generation, so check the diff first. When the restored generation doesn't validate (the restore listed `config_problems`, say a package removed since the bundle), the daemon publishes the config file over it as a new generation if the file validates, and refuses to start if it doesn't or there is none. Fix the problems before starting to keep the restored config.

```sh
open-conduit service stop                          # or stop serve
ls ~/.local/state/open-conduit/backups              # pick the newest bundle from before the problem
open-conduit backup verify ~/.local/state/open-conduit/backups/conduit-20261102T090000Z
open-conduit backup restore ~/.local/state/open-conduit/backups/conduit-20261102T090000Z --in-place
open-conduit service start
```

To go back a release, install the older binary first and run these commands with it, using the `pre-upgrade-<time>` bundle. A restore opens what it restores, which raises it to the format of the binary doing the restore, so a restore run by the newer binary leaves a database the older one refuses.

`backup restore --in-place` works on the daemon's own state directory (`--data-dir`, or the default) under its lock, so it refuses while the daemon runs. In order, it:

1. Removes the stages earlier restores left when they stopped before their swap began (`.restore-*`, `.conduit-snapshot-*`), and lists them under `removed_stages`.
2. Refuses to move the daemon off a store its `storage.json` names outside the directory without being asked. A database outside the directory needs the empty location the bundle's database goes to (`--database-location LOC`, in the backend's own terms). The replaced database stays where it is, and the report names it. A non-default artifact store needs the store the bundle's artifacts go to, which must be empty (`--artifacts NAME --artifacts-location LOC`), or `--artifacts files` to move them into the directory on purpose. The replaced store stays where it is, and the report names it.
3. Verifies the bundle and restores it into a stage inside the state directory, so every later step is a rename on one filesystem. The stage gets a fresh `private/operator.token` (the bundle's credentials are revoked) and a `restored.json` naming the bundle. A bundle that fails verification, or one a newer binary wrote, changes nothing.
4. Writes a journal, `restore-in-progress.json`, naming the stage, the move-aside directory and every item to move.
5. Moves the state it replaces into `before-restore-<UTC time>/`: `conduit.db` with its `-wal`, `-shm` and lock files, `artifacts/`, `storage.json`, `restored.json` and `private/operator.token`. The directory's other keys (`private/cursor.key`, the web view's sign-in) stay.
6. Renames the staged state into place, then removes the stage and the journal.
7. Removes the toolbelt run directories and sockets (`runs/`) of executions the restored database doesn't know.
8. Runs `state check` on the result and reports it with the rest (`in_place`): what was moved aside (`moved_aside`, `moved_items`), the runs removed (`removed_runs`), and what stays from after the bundle (`later_timeline`). It exits 1 when the check finds problems.

If the restore stops part way through the swap (a crash, a full disk), the journal stays and the daemon and the other offline commands refuse the directory. Run the same `backup restore BUNDLE --in-place` again: it finishes the swap from the journal and doesn't restore a second time.

A restore to a named database (`--database-location`) records that location in `restore-database.json` before it writes there, because it can't take a database back. If it stops before the swap's journal exists, the next run finds the record and the database, and refuses with a message that names the location as safe to clear. It doesn't carry on from it, since the leftover may hold part of the later steps, and it never deletes it. `serve --restore` keeps refusing until the location is cleared, then restores normally. The record is only written while the location is empty, so state that was there before the restore is refused as occupied and never called a leftover. The record goes once the swap has made that database the directory's own.

What stays from after the bundle is reported, never deleted by the restore, since its layout belongs to its owner:

- `executors/<name>/`: the executor's private state. The restored executions recover or retry, and no process from before the bundle is adopted. When the daemon starts, an executor that sweeps its state (shell, claude-code and codex) removes the run directories of executions the restored database doesn't know, except one whose process still runs, and doctor's `executor_state` check lists each execution whose state it released and what it kept and why (the daemon log has every one). Another executor's stay until you remove them.
- `resources/<type>/`: workspaces. The provider reconciles each lease against the restored database when the daemon starts, so a lease whose owner it doesn't know comes back unknown. The git provider removes such a workspace once it is idle, and keeps one with unpushed or uncommitted work, which doctor reports.
- `containers/<type>/`: the container provider reconciles its containers when the daemon starts, removing those of executions the restored database doesn't know.
- `extensions/<name>/`: an extension's private directory, a cache it rebuilds as it needs ([extension packaging](../extension-packaging/#writing-an-extension-with-the-sdk)).
- `homes/` and `logs/`, as they are.
- An artifact store outside the directory that the replaced `storage.json` named, when you chose another (step 2), stays where it is, named by the copy in `before-restore-*/`.

Afterwards:

- A scheduled bundle a newer binary took may hold records the older one refuses (`records_newer`); to go back a release, use the `pre-upgrade-<time>` bundle.
- Clients read the new `operator.token`.
- Reconcile re-reads the services, so what they still hold comes back: issues, PRs, comments and other state an extension can list. [What it can't re-derive](#what-a-restore-to-an-earlier-point-loses) is lost. Attempts running at backup time are recovered or retried, never adopted.
- Keep `before-restore-*/` until the restored instance has run for a while, then delete it. It holds the replaced database, so the state directory needs room for a second copy of it while the restore runs.

### What a restore to an earlier point loses

Everything committed after the bundle is gone from the database. Reconcile brings back only what an outside service still holds. It can't bring back:

- Declared one-shot deliveries: `github.delivery`, `schedule.webhook` and Slack's push-only families.
- Operator and human input: `core.manual` and `core.action` Events, and decisions.
- Child Work an agent created.
- Credentials issued with `auth add`.
- Executor continuity kept in KV.

Work that finished after the bundle runs again. An effect guarded the way [GitHub comments](../github-extension/) are (an `attempted` record, then a marker search) isn't repeated. An effect with no such guard can happen twice.

### In a container

When the daemon is the container's process, there's no shell to stop it and run the restore. Give `serve` the bundle instead, from a mounted volume:

```sh
open-conduit serve --restore /backups/conduit-20261102T090000Z
```

Before serving, it restores the bundle in place exactly as `backup restore --in-place` does, and prints the report to stderr. A restore that fails, or a restored directory whose state check finds problems, stops the start. With an artifact store outside the volume, name the empty store the bundle's artifacts go to with `--restore-artifacts NAME --restore-artifacts-location LOC`, as `--artifacts` and `--artifacts-location` do for `backup restore`. To start on a config for this machine rather than the bundle's generation, as a move to a new path needs, add `--restore-start-config FILE`, which does what `--start-config` does: a config that doesn't validate, or drops a scope with open Work, stops the start with nothing published. It applies only when the bundle is restored, so a restart never puts it back over a config applied since. It restores a given bundle once: a restart that finds the directory restored from it already (`restored.json`) serves the directory as it is, so the container can keep the flag until its next deployment. `service install` refuses `--restore`; for an installed service, stop it and run `backup restore --in-place`.

A container that lost its volume can start from a [replica](#restoring-from-a-replica) the same way:

```sh
open-conduit serve --restore-replica /recovered/conduit.db \
  --restore-artifacts-from /recovered/artifacts --restore-archive-from /recovered/archive
```

It restores only into a state directory that holds no database, so it can stay in the container's command: a restart finds the database, leaves it alone and serves it, even if the replica has since changed or gone. The directory must be owner-only (`0700`), as for any start. The daemon comes up drained, so run `drain end` through its management API once the old host is stopped for good. `--restore-artifacts NAME --restore-artifacts-location LOC` keeps using an object store, and `--restore-start-config FILE` starts on a config for this machine.

## Continuous replication

Bundles and WAL replication do different jobs, and a deployment that can't afford to lose a day should run both:

- **WAL replication keeps the recovery point to seconds.** It is the only protection for what has no outside source between bundles ([what a restore to an earlier point loses](#what-a-restore-to-an-earlier-point-loses)).
- **Bundles are verified points to go back to.** A replica copies a bad write as faithfully as a good one, and a bundle can go back before it.

Open Conduit doesn't ship a WAL replicator. [Litestream](https://litestream.io) is the supported one, tested at v0.5.18. Run it beside the daemon on `conduit.db`:

```yaml
dbs:
  - path: /var/lib/open-conduit/conduit.db
    truncate-page-n: 0
    replica:
      url: s3://my-bucket/open-conduit
```

- **Keep `truncate-page-n: 0`.** Otherwise Litestream can force a blocking checkpoint on a large WAL, and a write that waits longer than the store's 5-second busy timeout fails.
- **Start them in either order.** Litestream waits for a database that doesn't exist yet, and the daemon publishes a new one with its schema already in it.
- **The daemon stays compatible.** Its checkpoints are all passive, so none restarts the WAL under Litestream's read transaction or waits on it. Litestream adds two tables of its own (`_litestream_seq`, `_litestream_lock`), which travel in bundles and do no harm.
- **Replicate the rest separately.** Litestream covers only the database. Keep artifacts in an object-store [artifact backend](../domain-storage/#backend-lifecycle), or copy `artifacts/` on a schedule, and copy `archive/` if `archive_finished_days` is set. What a restore can't find is [reported and recorded as lost](#restoring-from-a-replica).
- **Doctor can't confirm it's running.** Its `recovery_point` check reports `replication_unobserved` with or without Litestream, because SQLite can't see a replicator, and gives the newest scheduled bundle's age. Watch Litestream with its own metrics.
- **Stop Litestream before `backup restore --in-place`,** and point it at an empty replica path afterwards, because the restored database is a new history.

To recover from a lost host:

```sh
litestream restore -config litestream.yml -o /recovered/conduit.db /var/lib/open-conduit/conduit.db
open-conduit backup restore-replica /recovered/conduit.db --to /var/lib/open-conduit-restored \
  --artifacts-from /recovered/artifacts --archive-from /recovered/archive
open-conduit --data-dir /var/lib/open-conduit-restored serve
open-conduit --data-dir /var/lib/open-conduit-restored drain end    # once the old host is stopped for good
```

`make drill-replication` runs this against a real daemon and the pinned Litestream. It is not part of `make check`, because it builds Litestream, but CI runs it as the `replication` job on every code change.

## Restoring from a replica

A replica is a copy of the database that another system kept, such as a WAL replicator. `backup restore-replica` turns one into a new state directory, with the checks a bundle gets:

```sh
open-conduit backup restore-replica /recovered/conduit.db --to ~/oc-restored \
  --artifacts-from /recovered/artifacts --archive-from /recovered/archive
open-conduit --data-dir ~/oc-restored serve
open-conduit --data-dir ~/oc-restored drain end    # once the old host is stopped for good
```

- **The replica is not changed.** A WAL beside it is applied to a private copy, and the restored file is owner-only whatever mode the replicator wrote. Tables a replicator added to the database stay in it.
- **It verifies and fences.** A file that isn't a database, fails its integrity check or was written by a newer binary is refused, and nothing is published. Every lease is fenced, and a fresh operator token replaces the old one.
- **It starts drained.** Nothing in a replica shows that the host it came from has stopped, so no attempt starts until you run `drain end`. The drain is written before the directory is published, so a restore that stops part way can't leave a copy that runs. A restore from a bundle ends the drain instead, because a bundle is a point you chose to go back to.
- **`--in-place` restores into the daemon's own directory,** which may be an empty mounted volume, under its lock. It refuses a directory that holds a database, because a replica restore never replaces one. `artifacts/` and `archive/` already in the directory are used as the surviving copies unless you name others. A restore that stops part way is finished by the next one, and it is drained before the swap begins.
- **The config is checked as for a bundle,** against the installed packages and the daemon's policy. `--start-config FILE` starts on a config for this machine.
- **Artifacts and archived Events are separate from the database,** so a WAL replica doesn't carry them. `--artifacts-from` copies a surviving file store and `--archive-from` a surviving event archive. With an artifact store outside the host, `--artifacts NAME --artifacts-location LOC` keeps using it and checks that each object is there. An archived record later than the replica is left out, because the restored daemon will issue that sequence again.
- **What's missing is reported, not fatal.** `missing_artifacts` and `missing_events` name what couldn't be brought back. The restored database records them as lost, so `state check` passes and later bundles omit them. An artifact store that can't be reached fails the restore instead, since an outage isn't a loss.

It writes a new directory only, so there is no `--in-place` form.

## Taking over on another host

Automatic takeover needs a storage backend that implements `storage.SharedBackend`, because only state several hosts can reach can be fenced from the old host ([instance lease](../domain-storage/#instance-lease)). **No backend in a release implements it, so automatic takeover isn't available yet.** [#912](https://github.com/open-conduit/open-conduit/issues/912) tracks shipping one. The daemon's side is built and drilled against a stand-in, and a backend added later gets it without a change to the core.

| Backend | Automatic takeover | Recovery point (`doctor`'s `recovery_point`) | After losing the host |
| --- | --- | --- | --- |
| `sqlite` | No: it is on one host and takes no instance lease | `replication_unobserved`: a WAL replicator can keep a copy, but SQLite can't see whether one is running | An operator [promotes a replica](#restoring-from-a-replica) or restores a bundle, and loses what the copy hadn't received |
| `faultinject-shared` | Only in the test build | Not a deployment option | Stand-in for the drills: SQLite on one machine |

A backend that implements `SharedBackend` must still state its own recovery point through `storage.RecoveryStater`. The instance lease stops two writers, but it says nothing about how much committed data a failure of that backend can lose.

### What a successor has

Each host has its own state directory, which must be on a local filesystem. Only what is in the shared store moves with the lease.

| What | Where it lives | At a takeover |
| --- | --- | --- |
| Work, Events, timers, config generations, audit ledger, package KV, drain, the record of the enforced policy, the list cursor key | The shared store | **Shared.** The successor continues from the last commit |
| Artifacts | The artifact store | **Shared only if that store is.** The default file store is on one host, and `doctor` warns with `artifacts_on_this_host` |
| Policy file | Each host | **Must be provided.** A daemon whose policy differs from the recorded one [doesn't serve](../domain-storage/#instance-lease) until it has that policy or an operator accepts the change |
| Installed packages (`extensions/` beside the config) | Each host | **Must be provided.** A daemon that can't validate the stored generation doesn't serve, and the shared config is left alone |
| Config file, `secrets.json`, keychain entries and `cmd:` programs | Each host | **Must be provided.** The stored generation is what runs, and its secret references resolve on the host that holds it. `open-conduit portability` lists what is tied to a machine |
| Operator token | Each host's `private/` | **Rebuilt.** A successor without a working one issues itself a credential. Named operators are in the store and keep working |
| `daemon.lock` | Each host's state directory | **Rebuilt.** It only keeps two daemons off one directory on one host. The instance lease, not this file, keeps one writer across hosts |
| Scheduled and pre-upgrade bundles | `--backup-dir`, by default inside the state directory | **Lost with the host unless shipped.** A successor with no bundle in its directory takes one straight away. Bundles at a [backup destination](../backup-bundles/#backup-destinations) survive |
| Packages' private directories (`<state>/extensions/<name>`) | Each host | **Lost.** Packages rebuild their caches. What must survive belongs in package KV |
| Run directories, toolbelt sockets, local processes | Each host | **Lost.** Their attempts are fenced and retried, so the Work survives. An attempt on a remote runner survives only if its executor can prove and adopt it |
| Workspaces, agent homes | Each host | **Lost.** Workspaces are made again from the remote, so unpushed work is gone, and an agent's next turn starts from its handoff note |

### What hasn't been proved

The drills run real daemons over one database on one machine. These have not been run:

- A failover between two real hosts, or against a real shared database.
- A network partition. The drills pause and kill the holder instead.
- A real tunnel moving between hosts.
- A live attempt adopted across a takeover. The drills prove that an attempt which can't be adopted is fenced and retried.
- Two real binary versions side by side. The upgrade tests set the stored format back to stand in for an older binary.

## SQLite migration recovery

Schema migrations run inside one immediate transaction at open (`internal/storage/sqlite`). A failure rolls back and leaves the database unchanged ("migration rolled back"). Open refuses these cases, each before it changes the database:

| Symptom | Cause | Recovery |
| --- | --- | --- |
| `stored schema is newer than this binary supports` (`state check`: `schema_newer`) | A newer binary migrated the database, and an older binary was started on it | Run the newer binary again. To go back to the older binary, restore the bundle taken before the upgrade (`backup restore BUNDLE --in-place`). Never edit `user_version`. |
| `stored records are newer than this binary writes` (`state check`: `records_newer`) | A newer binary, whose records have fields this one doesn't know, wrote the database, and an older binary was started on it. Opening it would drop those fields from every record the older binary rewrote | Run the newer binary again. To go back, restore the bundle taken before the upgrade (`backup restore BUNDLE --in-place`) and start the older binary. Never edit `record_format`. |
| `unversioned nonempty database; explicit migration required` or `schema metadata mismatch` (`schema_unrecognized`) | The file is not an Open Conduit database, or was altered by hand | Point `--data-dir` at the right directory, or restore the latest bundle. |
| `storage.json: storage: no backend "NAME" is built into this binary` or another `storage.json` error (`state check`: `storage_unavailable`) | The state directory's [storage choice](../domain-storage/#backend-lifecycle) names a backend this binary was not built with, or is malformed | Run a binary built with that backend, or fix `storage.json`. Nothing falls back to SQLite. |
| `storage.json: artifacts: storage: no artifact backend "NAME" is built into this binary` or another `storage.json: artifacts` error (`state check`: `artifacts_unavailable`) | The state directory's [artifact choice](../domain-storage/#backend-lifecycle) names an artifact backend this binary was not built with, a provider command that is relative or won't start, a relative location, or is malformed | Run a binary built with that backend, install the provider it names, or fix `storage.json`. Nothing falls back to the file store. |
| `coordination state is in use by another owner` (`state check`: `in_use`) | Another daemon or offline command owns the database, possibly through another state directory whose `storage.json` names the same location | Stop that owner first. Two state directories should never name the same database. |
| `migration rolled back: …` | The upgrade failed part way (disk full, I/O) | Fix the cause and start again: the old schema is intact. If it keeps failing, restore the pre-upgrade bundle and report the defect. |

An upgrade needs no backup step: the first start on a newer format [keeps a verified bundle](#the-bundle-taken-before-an-upgrade) or refuses to start. Keep the old binary until the observation window passes.

## Corrupted-state diagnosis

Run `open-conduit state check [--json]` with the daemon stopped. It examines the database through its backend's `Inspect`, which changes nothing (SQLite's opens a private copy), so diagnosis never changes the state. A replicator can change the file while it is read, so the copy is used only once two copies in a row have identical content. A file that doesn't settle within five copies is refused rather than reported as corrupt. It reports:

| Check | Meaning |
| --- | --- |
| `lock` | Fails when a daemon owns the directory (`in_use_or_unsafe`), or when its permissions are not owner-only. |
| `socket` | A leftover socket from an unclean stop (info: the next start removes it after its ownership checks). |
| `database` | `schema_1` (the full `integrity_check` and foreign keys pass; the daemon's own open runs only `quick_check`), or `corrupt` / `schema_newer` / `records_newer` / `schema_unrecognized` / `open_failed`, with the remedy. |
| `artifacts` | Referenced artifacts missing, not counting content retention released on purpose. A backup refuses missing content, so restore the directory or the latest bundle. |
| `timers` | The number of durable timers that resume on start. |

Per scope it also gives Work by state, live executions, active waits, and `drained`: no attempt pending or running. A parked attempt and its Waits hold no process and travel with the state, so they don't stop a scope being drained. This is the check before an upgrade or restore, and `drain status` is the same question for a running daemon.

Recovery order:

1. Copy the whole state directory aside (daemon stopped).
2. If `database` fails, restore the newest bundle that `backup verify` accepts in place (`backup restore BUNDLE --in-place`), and start. Reconcile then re-reads the services, which brings back what they still hold but not [everything committed after the bundle](#what-a-restore-to-an-earlier-point-loses).
3. If only artifacts are missing, the coordination state is sound. The run content of those executions is lost; take a fresh bundle once `state check` passes.
4. A stale socket or lock needs nothing: start the daemon.

## Evidence

`TestBundleRoundTripAndRefusal` (in `internal/backup`) creates, verifies and restores a bundle (a pre-backup lease fenced off), and refuses an existing destination, a tampered or corrupted bundle and one missing referenced content. `TestShellRetentionReleasesRunDirectories` (in `integration`) backs up and restores after a retention sweep. No composed-daemon drill exercises the offline commands since `TestDaemonMigrationRehearsal` went with the migrator in [#413](https://github.com/open-conduit/open-conduit/pull/413).

`TestScheduledBackupsKeepTheNewest` and `TestScheduledBackupsResumeFromTheNewestBundle` (in `internal/bootstrap`) cover the schedule with a fake clock: a scheduled bundle restores, the newest are kept and others in the directory left alone, a restart resumes from the newest bundle and clears interrupted staging.

`TestDrainStopsNewAttempts` (in `internal/runtime`) refuses a new attempt during a drain, reports the one in flight, lets it launch, and starts Work again once the drain ends. `TestDrainSurvivesAStartAnywhere` (in `internal/bootstrap`) keeps the drain when the state reopens at another path, and checks the warning a daemon logs at start while drained, which names `drain end`.

`TestDaemonDrainAndMove` (in `integration`) drains a real daemon with a Work parked, moves its state to a new directory tree and starts another on it: a wake during the drain launches nothing until the new daemon runs it, and a timer due across the move fires once. With the drain's guards removed it fails. `TestDaemonRestoreOntoAnotherPath` bundles a drained daemon and restores it under another path with `--start-config` and an empty Claude config directory: the one Work is still listed and its next turn starts from the handoff note. `TestRestoreStartsOnTheGivenConfig` refuses a start config that doesn't validate or drops a scope with open Work, publishing nothing, with a restore without one as its negative control. `TestPortabilityNamesEveryItem` covers a fixture with one of each non-portable item and no secret text in the report. `TestMovedWorkspaceKeepsItsWork` and `TestMovedWorktreeThatCantReattachIsKept` (in `extensions/git`) cover workspaces carried with the directory. `TestDockerClaudeSessionSurvivesAMove` (in `integration`, with real Docker) moves a state directory to a new tree and resumes the contained session from its carried home, and `TestRefileSession*` (in `extensions/claudecode`) leaves a path that still exists and refuses a link planted at the transcript, its source or its destination. `TestServeRestoreStartConfigAppliesOnce` publishes `serve --restore`'s start config once, never again on a restart. No automated drill crosses operating systems or users.

`TestUpgradeBundleRestoresOnTheOlderBinary` (in `internal/bootstrap`) simulates the format step: the bundle holds the old format and is restored and opened by the older binary, which refuses the raised live database (the negative control). `TestScheduledRotationKeepsTheUpgradeBundle` runs ten days of scheduled bundles past it, and `TestUpgradeRefusesWithoutABundle` refuses a read-only backup directory with the database left at its old format. The lifecycle suite's `open_prior_preserves_older_state_unchanged` proves the contract for every backend, with a backend that raises the format first as its negative control.

`TestLiveDatabaseKeepsTheWALForAReplicator` (in `internal/storage/sqlite`) holds a read transaction as a WAL replicator does while the store commits, reclaims and rebuilds, and fails if the WAL restarts under it. `TestExclusiveLockingShutsOutAReplicator` is its negative control. `TestRebuildLeavesTheWALInPlace` fails on a truncating checkpoint of the live database, with one as its negative control. `TestNewDatabaseIsNeverVisibleEmpty` covers a replicator adding its tables: a database is published with its schema already in it, and an empty file given those tables is still refused.

`TestReplicaRestoresWhatSurvived` (in `internal/bootstrap`) restores a world-readable replica holding a replicator's table, with its artifacts and archive: the lease is fenced, the directory starts drained and a bundle of it verifies. `TestReplicaReportsWhatIsMissing` restores the same replica with neither, and it still passes `state check` and can be backed up. Removing the record of what was lost is its negative control, which the backup refuses. `TestReplicaIsDrainedBeforeItIsPublished` stops the restore just after publishing and finds the directory drained. `TestReplicaAppliesTheWALBesideIt` keeps a commit that exists only in the WAL.

`TestDaemonReplicationDrill` (in `integration`, run by `make drill-replication`) replicates a real daemon with Litestream through commits, `Reclaim` and `Rebuild`, deletes its state directory, restores the replica and starts on it: the same Work is there, nothing launches until the drain ends, and it then completes. `TestReplicaNeverReplacesADatabase` (in `internal/bootstrap`) commits after an in-place replica restore, removes the replica and restarts: the commit survives, and without `serve`'s once-only rule the same directory is refused. `TestReplicaInPlaceFinishesAnInterruptedSwap` stops the swap part way. `TestInspectRetakesACopyTornByAWriter` (in `internal/storage/sqlite`) changes the file's bytes between a state check's copies while keeping its size and modification time, with a file that never settles as its negative control.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Until [#352](https://github.com/open-conduit/open-conduit/issues/352), backups ran only by hand with the daemon stopped; the daemon now takes them on a schedule.
- Ticket 06-02 delivered the offline recovery commands (backup create, verify and restore, `auth recover`, `state check`) and the runbooks above, with `TestDaemonMigrationRehearsal` as its drill.
