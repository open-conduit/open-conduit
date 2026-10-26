# Migration from Open Supervisor, ownership transfer and recovery

Ticket 06-02 makes the switch from Open Supervisor v1 reproducible and keeps the fallback installation working. It delivers four things:

- explicit, non-destructive config conversion (`open-conduit migrate config`);
- the scope-by-scope drain and transfer procedure, with its rollback;
- offline backup, restore, credential recovery and state diagnosis (`backup`, `state check`, `auth recover`);
- the runbooks for SQLite migration recovery and corrupted state.

All evidence is fixtures, fakes and drills on copied or synthetic state. No Open Supervisor config, state, keyring entry, tunnel or process was read or touched. Live rehearsal on a controlled scope belongs to the deferred 03-07 (first switch) and 06-04 (daily cutover), by the user's sequencing decision of 2026-10-26.

The PRD takes precedence over v1 throughout: Open Conduit owns independent state, and v1 stays installed and untouched as the fallback.

## Decision: drain and reconcile, no state import (provisional)

This finalizes the plan's "migration of active work versus drain-and-reconcile; fallback support window" decision. It is **provisional**: it was made while the user was away and is recorded for their review.

- **No v1 state is imported.** v1 records (issues, sessions, waits, job last slots, run history) stay in v1's own state directory. Open Conduit starts each scope from its config and from what reconcile reads from the services. Importing v1 state would need a tested translation of v1's per-issue store into Work, Executions, Waits and timers. Nothing in the four workflows needs that, and draining avoids it.
- **One owner per scope, never two.** A scope moves only after v1 has drained it: no v1 turn running, no v1 job running, and every waiting or held issue either resolved or explicitly released. v1 must not run the project while Open Conduit owns it. Both daemons may run at the same time on *different* scopes.
- **Shared projections are the ownership signal.** Both runtimes use the same label names by default, and the converter keeps custom ones. An open issue with the claimed label and no local record belongs to someone else in both runtimes (v1 `TestClaimRunAndLabel`; Open Conduit's GitHub and tracker claim policies). So neither runtime picks up the other's issues. A transfer is an explicit manual claim.
- **Marked writes are recognised both ways.** Open Conduit reads v1's comment markers (`<!-- open-supervisor:agent -->`, `<!-- claude-agent -->`, Linear's visible footer, Jira's entity property). Since 06-02, every GitHub comment Open Conduit writes also carries v1's own-note marker after its own (`github.LegacyMarker`). A v1 that takes a scope back therefore reads Open Conduit's comments as agent notes, never as human input that would wake its agents. Linear and Jira already wrote v1's marks (06-01).
- **Fallback support window.** v1 stays installed, with its config and state untouched, until one release after the daily cutover (06-04) has succeeded, as the PRD requires. Its binary is pinned at the baseline revision.
  - Each scope switch has an observation window of **7 days** (provisional default).
  - The rollback triggers are any of:
    - a duplicate claim, comment or agent on one issue;
    - accepted work lost (a reconcile failing for more than one hour, or an undeclared loss);
    - a timed job slot run twice or missed;
    - a doctor `fail` not cleared within one hour;
    - the operator's judgment.
  - The window succeeds with zero duplicate writes, every due slot run exactly once, and no unresolved `fail` checks.

Record the actual window, thresholds and triggers in the switch evidence before switching (03-07, 06-04).

## Configuration conversion

```sh
open-conduit migrate config --from ~/.config/open-supervisor/config.yaml              # preview: report + config, nothing written
open-conduit migrate config --from ~/.config/open-supervisor/config.yaml --write      # save to Open Conduit's config path
open-conduit migrate config --from V1 --out FILE [--force] [--no-repo-files] [--time-zone Europe/London] [--shell-jobs] [--detached-timeout 21600] [--json]
```

The conversion is non-destructive:

- It reads the v1 config and, unless `--no-repo-files` is given, each project checkout's `.open-supervisor.yaml` and `.open-supervisor/playbooks/*.md`. Nothing else is read.
- It never resolves a secret, never copies a credential and never writes inside a checkout.
- It validates the result offline against the daemon's registrations (`bootstrap.ValidateConfig`) and writes only a valid config. The written file is owner-only (0600, through an atomic rename).
- It refuses to replace an existing file without `--force`, and refuses the source path and Open Supervisor's config directory.
- Views (the preview and `--json`) redact `cmd:` reference text, as `config show` does.

The code is [`internal/migrate`](../../internal/migrate). It has its own boundary rule: extension and executor names only, never runtime, SQLite or composition. The CLI reaches it through the injected `cli.Env.Migrate`.

### Mappings

| v1 | Open Conduit |
| --- | --- |
| `settings.maxTurns`, `maxOpenPerProject`, `reconcileMinutes` | `settings.concurrency`, `admission_limit` (recorded, not yet enforced), `reconcile_seconds`. Only values that differ from the defaults are written. |
| `settings.maxAttempts` (3) | `retry_delays_seconds`: N attempts become N−1 delays (300 s, 600 s, …); 3 is the default `[300,600]`. Jobs keep v1's no-retry rule. |
| `settings.port`, `tunnel` | Serve flags (`--webhook-addr`, `--tunnel …`), reported as an action. With a tunnel, entries get a fresh `webhook_secret` and `manage_webhook`. |
| `settings.agent` / project `agent` (a claude-code connection) | Each scope's `claude-code` binding config: `binary` (with `~` expanded), `model`, `permission_mode`, `extra_args`, plus `handoff_minutes` and `handoff_share`. `useApiKey` becomes `pass_env: [ANTHROPIC_API_KEY]`. |
| `settings.labels`, project and source `labels` | The entry's `labels`, written only when they differ from v1's defaults (which are Open Conduit's defaults). For Jira, spaces become dashes. `unrouted` is not projected (reported). |
| `settings.branch`, project `branch` | GitHub: `{key}` → `{number}`, giving the same branch names. Trackers: the default becomes 06-01's `agent/{key}` (`agent/issue/ENG-12`). A custom tracker template is kept, with a warning that `{key}` is now `issue/KEY`. |
| `settings.git`, project `git`, `baseBranch`, `worktree` | The git resource: `remote`, `sign`, `base_branch`, `copy`, `setup`, `setup_when`. `dotenv: true` keeps v1's checkout `.env` layer, which is opt-in in Open Conduit. |
| `settings.sweepAt`/`triageAt` and project `sweep`/`triage` | The `introspection` entry's `sweep` and `triage` (a daily cron in `--time-zone`, else the machine's zone, else UTC with a warning), with `rules`, `guide`, `land` and `file_issues`. Adds claude-code `sweep`, `triage` and `fix` bindings and routes (playbooks `sweep`, `triage-reports`, `fix-report`). |
| `settings.host` | `host_watch` in the first converted scope (it is machine-wide). v1 1.3.0 (#63) also uses `host.disk` as the worktree disk threshold, whether or not the watch runs. A value other than its default of 90 becomes each converted git resource's `disk_full_percent`, capped at 100. v1's six-hourly spare-worktree sweep is the git provider's (`serve --sweep-interval`, default 6h) ([disk safety](workspaces.md#disk-safety-and-the-spare-workspace-sweep)). |
| `settings.telemetry` (PostHog) | A `posthog` connection, a `posthog` entry in each scope, and introspection `telemetry: true`. |
| `settings.usageMinutes`, `checksDebounceSeconds`, `dryRun` | Reported: `serve --claude-usage`; no debounce (03-04); `serve --dry-run`. |
| Connections `github` (gh, token, app, bot app), `linear`, `jira`, `bitbucket`, `posthog` | Connections of the same type with snake_case config. The ids are made valid (`[a-z][a-z0-9_-]{0,62}`), and a rename is reported. `auth: gh` becomes `cmd:gh auth token`. |
| `ngrok`, `cloudflared`, `static` connections | Serve flags (reported). |
| A project with a GitHub repo and tracker (same repository) | A scope with a GitHub entry (`github/<host>/<repo>`); the `agent` binding (claude-code, the GitHub tools, a `work` workspace); the `hold` binding with the `held` route (v1's scheduled label); and the `issues` route. |
| A project with a Linear/Jira tracker, or fed by a v1 `source` | A tracker entry `<type>/<scope>` (`teams`/`states` or `projects`/`jql`/`fields`, `transitions` from `stateX`/`transitionX`), the `work-tracker-issue` playbook, and `args {key}`. A source becomes one entry per scope with a route `match` (06-01). `any`/`all`/`not` and glob values are reported as unsupported. |
| A Bitbucket repo for a tracker | The `bitbucket` code-host entry (tracker namespace and keys, `Closes {key}`) and the `prs` route. A GitHub repo for a tracker is reported: its PRs do not correlate. |
| A GitHub tracker in another repository, or a shared GitHub source | Not converted (reported); configure the tracker entry by hand. |
| Project `env` | Plain values go in the binding `env`. Secret references become scope `secret_refs` (`env-<name>`), selected by `secret_names` and mapped by `secret_env`. |
| Project `disabled` | `enabled: false`. |
| Project `jobs` plus the checkout's `.open-supervisor.yaml` | One `schedule` entry (`schedule/<scope>`) with snake_case timing and `on`; the names are made valid. Each job gets a binding and routes (below). `settings.jobs: false` converts no jobs (reported). Open Conduit never reads the repository file again. |

**Jobs** follow the 05-01 and 05-02 targets:

- **Executor.** Each job runs on `legacy-job` (`{"shell": {"argv": command, …}}`), so scripts keep the `OPEN_SUPERVISOR_*` names, which sit below the binding env and never over `OS_*`. `--shell-jobs` writes plain `shell` bindings instead, for jobs without a status folder, when the scripts are known not to use those names.
- **Checkout.** `checkout: fresh` gives `mode: fresh`; anything else gives `mode: shared` on the project checkout.
- **Timeout.** A job without a status folder gets an explicit shell `timeout_seconds` (`timeoutMinutes`, else v1's implicit 5). A status job gets `legacy-job` `status` plus a binding `timeout_seconds` (default six hours, `--detached-timeout`; review it), and two routes: timer slots `queue`, then its other trigger kinds and manual `drop`.
- **Kept runs.** A `keepRuns` above `settings.kept_executions` (20) is reported, not dropped.
- **Webhooks.** A webhook job gets a new `job-webhook` secret. Its URL changes to `/schedule/<scope>/jobs/<job>/<token>`, and the senders must be updated (`doctor --check-secrets` prints the URL).

### Secret references

No secret value is read, resolved or copied. The report lists each mapping by v1 location and scheme, never reference text or value:

| v1 reference | Open Conduit reference | Operator action |
| --- | --- | --- |
| `env:VAR`, `cmd:…` | unchanged | none (Open Conduit resolves them itself; `cmd:` text is redacted in views) |
| `keyring:name` (service `open-supervisor`) | `keyring:<connection>-<field>` (service `open-conduit`) | Copy the value: `security find-generic-password -s open-supervisor -a NAME -w \| open-conduit secret set ID` (macOS) or `secret-tool lookup service open-supervisor username NAME \| open-conduit secret set ID` (Linux). |
| `file:name` (v1 `secrets.yaml`) | `file:<id>` (Open Conduit `secrets.json`) | `open-conduit secret set ID --store file` (value on stdin). |
| A literal secret | `keyring:<id>` | Store it with `secret set` and rotate it at the provider; the converter warns and never writes it. |
| (new) webhook secrets | `keyring:<scope>-<kind>-webhook` | `openssl rand -hex 32 \| open-conduit secret set ID`. |

### Independent paths and names

Open Conduit keeps its own paths: `${XDG_CONFIG_HOME:-~/.config}/open-conduit/config.json`, `${XDG_STATE_HOME:-~/.local/state}/open-conduit`, `secrets.json` beside the config, keyring service `open-conduit`, and `--config`/`OPEN_CONDUIT_CONFIG`, `--data-dir`/`OPEN_CONDUIT_DATA_DIR`. It never reads `OPEN_SUPERVISOR_CONFIG`, `OPEN_SUPERVISOR_STATE`, `.open-supervisor.yaml` or v1's directories.

Playbook additions are read from `.open-conduit/playbooks/<playbook>.md`. v1's `.open-supervisor/playbooks/<playbook>.md` is a fallback when the name is the same (`work-issue`, `sweep`). The converter reports the renamed ones: v1's `triage.md` becomes `triage-reports.md`, and a tracker scope's `work-issue.md` becomes `work-tracker-issue.md`. Copy those yourself; the converter never writes in a checkout.

Turn variables (`OPEN_SUPERVISOR_PROJECT`, `_ISSUE`, `_BRANCH`) are not set for agent turns: the playbook reads its context through the toolbelt. Job variables are kept by `legacy-job`.

## Scope-by-scope transfer (v1 → Open Conduit)

Move one scope at a time, smallest and least active first. For each scope:

1. **Prepare, with nothing switched yet.**
   - Convert into a new file (`--out`) and complete the reported actions: secrets, serve flags, playbook renames.
   - Run `open-conduit config check --file FILE`, then start Open Conduit on a separate state directory with the scope **disabled** (or `serve --dry-run`, which makes no service writes and arms no job timers). Run `doctor --check-secrets` until every check is `ok`.
   - Record the observation window and the rollback triggers.
   - Take `open-conduit backup create` of any existing Open Conduit state.
2. **Drain v1 for this project.**
   - Let running turns finish (watch v1's TUI). Do not stop a turn mid-write.
   - Waiting issues (PR, checks, `--until`) and held (`scheduled`) issues either resolve in v1 first, or are released: drop them in v1, which keeps the claimed label, and note their keys for the manual claim in step 4. Their Claude session is not carried over; the new turn starts fresh from the issue, its comments and its branch.
   - Let a running job finish, including a detached job's run (its `running.json` gone, `last.json` written). Choose a switch time **between** timed slots, after v1's last run and well before the next one. A converted timed job never re-runs a slot v1 already ran, because its first run waits for the next slot. So a slot that falls between stopping v1 and starting Open Conduit is not run by either.
   - Then set the project `disabled: true` in v1's config and restart v1, or stop v1 when this is its last project. From here v1 must not run this project.
3. **Hand over the external surfaces.**
   - Webhooks: with `manage_webhook`, Open Conduit re-points the repository or tracker hook when the scope starts. v1 must not manage it any more (it no longer runs the project).
   - Tunnel: an ngrok free endpoint can serve one process. Move it only when its last v1 project transfers; until then, run Open Conduit reconcile-only or use cloudflared or a static URL on another port (v1 holds 8787).
   - Worktrees: v1's worktrees live under `~/.local/share/open-supervisor/worktrees/<project>`. A branch checked out there blocks Open Conduit's workspace for that issue. Before transferring an issue, push its branch and remove v1's worktree (`git -C CHECKOUT worktree remove PATH`, then `git worktree prune`).
4. **Switch.**
   - Enable the scope in Open Conduit (`config apply`), or restart without `--dry-run`. Run `open-conduit scopes reconcile SCOPE`.
   - Transfer each released issue explicitly with `open-conduit claim SCOPE#N`. For a tracker, the manual claim is an operator Event: `open-conduit events emit --scope-id SCOPE --type linear.claim.requested --delivery-id claim/KEY --key-source <entry namespace> --key-external-id issue/KEY` (`jira.claim.requested` for Jira). Issues still carrying v1's claimed label without a claim are never picked up.
   - Check `open-conduit tui`, `runs list`, `doctor` and `route KEY` for the first claims, and the next timed slot of each job.
5. **Observe** through the window: duplicate writes, missed or doubled slots, `fail` checks.

| State | What happens at transfer |
| --- | --- |
| Active runs (turns, jobs, detached jobs) | Finished in v1 before the switch; never adopted across runtimes. |
| Waits and holds | Resolved in v1, or released and claimed again by hand in Open Conduit, where the agent sets a new wait if it still needs one. |
| Timed schedule slots | Not imported. The first converted slot is the next one after the switch, and interval jobs run once at start. Switch between slots. |
| Pending job runs (v1's one waiting rerun) | Drained in v1. A webhook POST during the gap is lost (declared one-shot loss). |
| Correlation state (issue ↔ Work, PR ↔ issue, sessions) | Rebuilt by reconcile: PRs correlate by branch or body, and Linear/Jira by key or generation. Sessions start fresh. |
| External projections (labels, comments, statuses) | Shared label names; marker recognition both ways; Open Conduit converges labels only for Work it owns. |
| Run history | Stays in v1's state, readable through v1's TUI during the fallback window. Open Conduit's history starts at the switch. |

## Rollback (Open Conduit → v1) and forward return

**Rollback** returns one scope to a working v1:

1. Record the trigger.
2. In Open Conduit, disable the scope (`config set /scopes/ID/enabled false` and `config apply`). Ingress, claims, timers and toolbelt calls stop; parked waits and timers stay durable for a forward return.
   - Wait for live executions to end, or cancel them with `executions cancel` or `work cancel`: disabling fences their authority but does not kill a running process.
   - Confirm with `executions list --state running`, `waits list --state active` and, once the daemon is stopped, `state check` (the scope is `drained`).
3. Take `open-conduit backup create DEST` (daemon stopped) and `backup verify DEST`.
4. Before starting v1 for the project, check each timed job's last Open Conduit run (`runs list --work-kind schedule.job`). v1 catches up the latest missed slot within six hours from its own last-slot record. If Open Conduit ran a slot less than six hours ago, start v1 only after that slot is six hours old, or remove that job from v1's config until then.
5. Hand external surfaces back: point the hook at v1's tunnel (v1 manages its own hook again) and move the tunnel.
   - Open Conduit's issues keep the claimed label, which v1 treats as someone else's. For each one v1 should continue, claim it in v1's TUI (`c`).
   - Remove Open Conduit's worktree for an issue first: `<state>/resources/git/…`, after pushing its branch. Issues v1 still has records of are resumed by v1 itself; they were not worked by Open Conduit unless you claimed them there.
6. Set `disabled: false` for the project in v1 and restart v1. It reads Open Conduit's comments as agent notes, through the legacy marker.

**Forward return** (v1 → Open Conduit again) repeats the transfer above.

- **Re-enabling the kept state** keeps every completed run, comment and claim recorded. External writes are not repeated: comments are found by their markers and labels converge level-triggered. But a re-enabled timer catches up a missed slot within six hours, and that slot may be one v1 ran during the rollback. Before re-enabling, apply the config once **without** the scope's timed jobs and then again with them: a re-added timed job waits for its next slot, as on first conversion.
- **When the state directory is lost**, restore the latest bundle into a new directory (`backup restore BUNDLE --to DIR`, then `serve --data-dir DIR`). `TestDaemonMigrationRehearsal` shows that a restored instance repeats no external write, claim or job run.

## Backup, restore and credential recovery

These commands are offline. They take the state directory's exclusive daemon lock, so they refuse while a daemon owns it ("stop the daemon first"). Use `doctor` while it runs.

```sh
open-conduit backup create ~/backups/oc-2026-10-26        # an absent path; the database snapshot plus referenced artifacts, with a SHA-256 manifest
open-conduit backup verify ~/backups/oc-2026-10-26        # restores into a private temporary directory, checks every digest and reference, removes it
open-conduit backup restore ~/backups/oc-2026-10-26 --to ~/.local/state/open-conduit-restored
open-conduit --data-dir ~/.local/state/open-conduit-restored serve
open-conduit auth recover                                 # a lost or ambiguous operator token: revokes every operator credential, writes a fresh one
```

- **What a bundle holds.** A bundle holds `conduit.db` and the artifacts the database references ([backup bundles](backup-bundles.md)). Keep bundles owner-only: they hold private Work, Events, config snapshots and executor continuity.
- **What a bundle does not hold.** It holds no credentials, config, secrets, executor run directories (`executors/`), workspaces (`resources/git/`) or logs.
- **Restore.**
  - A restore never replaces an existing directory.
  - It fences every backed-up lease, so an old execution credential can never commit.
  - It revokes the bundle's operator credential and writes a fresh `private/operator.token` (`auth recover` under the lock).
  - A process running at backup time is not adopted after a restore: its attempt recovers or retries through ordinary recovery. Take bundles after a drain.
  - Worktrees of the old state directory stay registered in the checkout: run `git worktree prune` once the old directory is gone.
- **Timing.** Back up before every binary upgrade, before and after every scope switch, and before a rollback. A daily bundle with the daemon briefly stopped is the minimal routine; online bundles through the management API are not provided.

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
| `artifacts` | Referenced artifacts missing. A backup refuses missing content, so restore the directory or the latest bundle. |
| `timers` | The number of durable timers that resume on start. |

Per scope it also gives Work by state, live executions, active waits, and `drained` (no running or waiting Work, live execution or active wait), which is the drain check before a switch or rollback.

Recovery order:

1. Copy the whole state directory aside (daemon stopped).
2. If `database` fails, restore the newest bundle that `backup verify` accepts into a new directory, and start there. Reconcile then re-reads the services: work accepted after the bundle is re-derived from them, apart from declared one-shot deliveries.
3. If only artifacts are missing, the coordination state is sound. The run content of those executions is lost; take a fresh bundle once `state check` passes.
4. A stale socket or lock needs nothing: start the daemon.

## Rehearsal evidence

- **`TestMigrateConfigCommand`** (`integration/migrate_test.go`) converts the synthetic v1 fixture (`internal/migrate/testdata`: a GitHub project with four jobs and a repository file, a Linear source feeding a Bitbucket project, a disabled Jira project, an unsupported cross-repository tracker, and all secret schemes). It shows that:
  - a preview writes nothing, and the result validates offline;
  - refusals keep the v1 directory and the source intact, and a second write needs `--force`;
  - the written config keeps references only: the literal never reaches any output;
  - every input file is byte-for-byte unchanged.
- **`internal/migrate` unit tests** check each mapping above, the report, the defaults and the refusals.
- **`TestDaemonMigrationRehearsal`** (in `make drill`) runs on the composed daemon with the GitHub fake, a local git origin and a fake Claude Code. It shows:
  - conversion through the CLI;
  - an issue v1 still owns, never claimed;
  - a new issue worked once, and a released issue transferred by manual claim;
  - the converted timed job not re-running the slot v1 ran an hour earlier, and the interval job running once;
  - offline commands refused beside the daemon;
  - after a graceful stop: `state check` drained, backup created and verified;
  - a restore into a new directory with a fresh credential (the bundle's refused), then no repeated external write, claim or job run after a reconcile;
  - corrupted and newer-schema databases diagnosed, and a tampered bundle refused.
- **`TestMarkers`** checks that every Open Conduit GitHub comment carries v1's own-note marker.

**Deferred to 03-07 and 06-04.** Those tickets hold everything that needs real operation:

- a rehearsal on a live controlled scope, and the real v1 drain and rollback;
- real webhook re-pointing and a tunnel move;
- the recorded observation window;
- confirmation of which integrations the operator actually runs.
