# Operator setup, doctor and engineering controls

Ticket 03-05 makes the GitHub path operable for the first switch. It adds setup and config commands over scopes, connections and bindings, local secret entry, GitHub/Claude Code doctor checks, operator controls in the management API, the CLI and the TUI, the webhook listener with tunnels, and dry run. All of it uses public surfaces:

- the management API ([B05](config-management.md));
- the extension contracts;
- composition in [`internal/bootstrap`](../../internal/bootstrap).

Core gains only domain-free controls (rebind, hold/release, scope reconcile). The Open Supervisor references were `internal/wizard`, `internal/configcli`, `internal/forms`, `internal/tui` and `internal/adapters/tunnel` at the pinned baseline.

## Setup from nothing

These are the documented steps, which [`TestDaemonFreshInstallGitHubIssue`](../../integration/daemon_operator_test.go) runs end to end:

```sh
open-conduit init github --repo acme/web --scope web --checkout ~/Code/web   # or --no-workspace; --holds; --token REF
open-conduit secret set github-token                                   # only when the token is not cmd:gh auth token
open-conduit config set /connections/github/secret_refs/token '"keyring:github-token"'
open-conduit config check
open-conduit serve                                                     # --webhook-addr … for webhooks, --dry-run to watch
open-conduit doctor --check-secrets
open-conduit tui
```

- **`init`** lists the setups the registered extensions offer, and **`init <type>`** runs one (04-02: setup is extension-owned; see [Extension-owned setup](#extension-owned-setup-04-02)). **`init github`** ports the v1 wizard's outcomes:
  - It detects the current directory's GitHub remote (v1 found-git).
  - It adds one scope for a repository: a `github` connection (default secret `cmd:gh auth token`), an agent binding on `claude-code` with the GitHub tools, the `github` route, and the `github-hold` binding and route with `--holds`.
  - It adds a `git` work resource only when there is a checkout. `--no-workspace` writes a workspace-free scope whose turns run in a private directory.
  - It merges into an existing config and never replaces a scope without `--force`.
  - `--dry-run` prints the result without writing it.
  - It refuses a token literal: `--token` takes only a reference.
- **`config path|show|get|set|unset|check`** port v1 `config show/get/set/unset` onto JSON pointers:
  - Array elements are addressed by `id` or `name`, for example `/scopes/web/bindings/agent/config/model`.
  - Edits keep the owner's key order and untouched text, including handwritten routes.
  - `show` and `get` redact `secret_refs`.
  - `set` refuses a value that is not a reference under `secret_refs`.
  - Every edit is validated offline by `bootstrap.ValidateConfig` before saving: the same registrations as `serve`, without a daemon, storage or secret resolution. An invalid edit leaves the file unchanged. A running daemon changes only through `config apply`, so a disk edit never alters it.
- **`secret set NAME`** ports v1's secret entry:
  - It reads the value from stdin, with no echo on a terminal, and never takes it as an argument.
  - It stores the value in the platform keyring under the `open-conduit` service: on macOS through `security -i`, so the value is not in any argv; on Linux through `secret-tool`.
  - Otherwise it falls back, and says so, to the owner-only `secrets.json` beside the config.
  - It prints only the reference. `--store keyring|file` forces one store.
  - The daemon's resolver now reads `file:` references from that file. Before this ticket it had no secrets file configured.
- **`init --with claude`** ports v1 `init --with`:
  - The setup guide ([`playbooks.Setup`](../../extensions/github/playbooks/setup/setup.md)) is written as its own one-skill plugin under `<state>/playbooks/setup`, apart from the turn plugin.
  - The command is an interactive `claude /open-conduit:setup --plugin-dir … --allowedTools …` with `OPEN_CONDUIT_CONFIG` set.
  - Only `open-conduit config`, `doctor`, `health`, `secret path` and `init github --dry-run` are pre-allowed, and `OS_*` credentials are removed from the environment.
  - The guide now teaches `init`, `config set`, `secret set`, webhooks, permissions, dry run and the TUI controls. Its example config still validates against the daemon registry.

A schema-driven interactive editor (v1 `forms`/editor) is not ported. The commands above cover its outcomes: validated edits, add/remove bindings, references for secrets and handwritten routes kept. The full-screen editor is deferred to 06-01.

## Doctor

`GET /v1/doctor` (`open-conduit doctor [--check-secrets]`) keeps the 02-08 checks and adds extension checks contributed by bootstrap. Details carry codes, identifiers and counts, never references or values.

| Check | Status and codes |
| --- | --- |
| `github` | Per entry: registration failures (`fail`, for example `github_connection_required`), rate-limit pauses, reconcile state (`reconcile_failing` with the scan error code and failure count, first scan pending, or the age of the last complete scan), projection failures (`projection_failing`) and dry run. No service calls. |
| `github_auth` | Only with `check_secrets=true`. Resolves each entry's credentials privately, authenticates the reader and any bot writer, and reads the repository (`Client.Verify`). Codes: `verified`; `credential_unavailable`, `credential_rejected`, `repo_unreachable` or `rate_limited` (`fail`); `read_only` when the identity cannot push and projections are live; `hook_admin_missing` when `manage_webhook` lacks hook admin. Reports `pull/push/admin` when GitHub returns them. |
| `github_capability` | Config gaps the validator accepts but that stop GitHub work: no `github` route, a route whose binding has no toolbelt, a claude-code binding without `github.context`/`declare_outcome`, a hold route that does not bind `github-hold` or comes after `github`, and (info) scheduled issues not claimed without a hold route. |
| `webhooks` | `reconcile_only` (info) with no listener; otherwise the bound address, the tunnel and public URL (`tunnel_down`, `tunnel_failed`), and per entry its route, a missing `webhook_secret` (not mounted) and the managed hook state (`ok`, `dry_run`, `hook_permission_denied`, …). |
| `claude_code` | Runs `--version` for each claude-code binding's binary (`installed`, or `claude_missing` as `fail`); reports dry run. With `serve --claude-usage` it also reads the subscription's 5-hour and 7-day usage windows (v1 `Usage`, the undocumented OAuth endpoint, with the machine's Claude login) and warns at 90%. A read failure is "usage unknown", never a fault. Usage reading is opt-in because it reads the login token. |

## Operator controls

Every control is a management call with the operator credential and an `Idempotency-Key`. It commits an accepted `core.manual.*` control Event, marked routed as `control`, in the same transaction as its state change. History, `events chain` and telemetry therefore explain it. None launches an executor or forces a wake.

| Endpoint | Semantics |
| --- | --- |
| `POST /work/{id}/rebind` `{expected_revision,binding,reason}` | B05 rebind. Ready Work with no live or parked attempt only. The binding must be an enabled binding of the Work's scope. Moves every queued input (and the retry state) to it. `core.manual.rebind` records the binding, the previous bindings and the scheduler's generation. Never moves scope. |
| `POST /work/{id}/hold`, `/release` `{expected_revision,reason}` | A domain-free scheduling hold (`core.manual.hold`/`release`), held in its own KV record. While held, `Start` refuses (`not_eligible`), including a parked attempt's resume, but input keeps queueing and Waits keep resolving. Release lets the next attempt see everything that arrived. Allowed on nonterminal Work with no pending or running attempt. `GET /work/{id}` shows `hold`. |
| `POST /work/{id}/attach` `{}` | Held Work with no pending or running attempt. Returns the attach command (`argv`, `dir`) of the executor registration that ran the latest attempt, through the optional public `extension.AttachExecutor` (`attach_unsupported`, `nothing_to_attach`; the code was `no_session` before 07-01). For claude-code this is `claude --resume <session>` in the workspace, and the handoff note is cleared, so the next turn continues the attached session. No environment is returned: the operator's own shell supplies it. |
| `POST /scopes/{id}/reconcile` `{expected_revision}` | B05 operator reconcile. Accepts `core.manual.reconcile` in the scope and makes every ingress source registered for the scope due now. The scan is the normal asynchronous reconcile. A generation already under way when the request commits (a page in flight, or part way through its pages) completes and leaves the source due, so a fresh generation always starts after the request (05-04 fix; previously such a request was absorbed and the next scan waited a full interval). Returns 202 with the Event and the source names. |

`GET /work/{id}` also carries `annotations`: display-only lines an extension contributes. For GitHub these are the issue, its URL, a held issue's state, projection dry run and the counts of corrections and problems the agent logged (`github.Annotate`, durable state only).

**CLI.** The CLI adds:

- `work rebind|hold|release`;
- `work attach ID [--keep-held]`, which holds (refetching the revision when the host moved the Work), attaches, runs the command with the terminal and releases the hold it placed;
- `scopes reconcile ID`;
- `claim [SCOPE#]N [--entry NAME]`.

A manual claim is an operator Event `github.claim.requested` keyed to the entry's `issue/N` (schema `github.claim.v1`). The `github` route creates `github.issue` Work for it whatever the labels (v1 claim by hand overriding routes). It never feeds existing Work, ignores non-operator sources and generation keys, and renders for the agent as "claimed by hand".

**TUI.** The shared view keeps one run model for every executor. With a controlling source (`tui.Operator`) it adds these keys:

- `c` claims (`scope#N` prompt);
- `R` runs or retries;
- `H` holds or releases;
- `a` attaches, through `tea.Exec` with the same hold/release flow;
- `x` cancels the attempt and `X` the Work, each after a y/n prompt;
- `l` opens a run's log; on a running run it shows the output so far and follows it;
- `t` opens a run's transcript (live or final) from the list or detail, and in the log view steps to the next output (log, stream-json transcript, stderr).

Detail shows the Work's correlation keys, hold, annotations and active or woken Waits as data, with no executor or domain branch. A read-only source shows none of this.

## Webhooks, tunnels and reconcile-only operation

Webhooks are an accelerator: reconcile always runs, so with no listener the daemon is **reconcile-only** and loses only latency (`settings.reconcile_seconds`, default 15 minutes). `serve --no-tunnel` forces this.

- **Listener.** `serve --webhook-addr HOST:PORT` binds a separate HTTP server on its own TCP address. It serves only the routes that registered entries name:
  - `POST /github/<scope>/<entry>` for each GitHub entry that names a `webhook_secret`;
  - `POST /slack/<scope>/<entry>` for each Slack entry with `receiver: http` ([Slack extension](slack-extension.md));
  - `POST /linear/…`, `POST /jira/…` and `POST /bitbucket/<scope>/<entry>` for tracker and code-host entries ([installed integrations](installed-integrations.md));
  - `POST /schedule/<scope>/<entry>/<job>/<token>`, a schedule job's signed URL ([scheduled jobs](schedule-extension.md));
  - the paths an installed extension package registers when it is configured ([extension packaging](extension-packaging.md)).

  Anything else is 404, including `/v1/...`. It never sees management credentials and never forwards to the Unix socket.
  - Each request is verified by its entry's adapter against the entry's secret from the current generation before anything is parsed or accepted. For GitHub that is `github.WebhookHandler` (HMAC before parsing, 401 on a bad signature).
  - A delivery is pushed with `Ingress.Push`, and 202 is returned only after durable acceptance. A refusal is 503: GitHub may redeliver, and reconcile recovers the state either way.
- **Tunnel.** `extensions/tunnel` ports v1's providers: `ngrok` (`--tunnel-domain` for a reserved domain), `cloudflared` (a quick tunnel, or `--tunnel-name` with `--tunnel-hostname`) and `static` (`--public-url`, for a reverse proxy or Tailscale Funnel).
  - A provider process that exits is restarted with a growing wait (5 seconds, doubling to 5 minutes), and each public URL it reports is passed to the host.
  - It forwards only to the webhook listener.
- **Managed hooks.** An entry with `"manage_webhook": true` (which requires `webhook_secret`) has its repository webhook created or re-pointed at `<public URL>/github/<scope>/<entry>`, with the secret and the events the normalizer understands (`github.HookEvents`). The host finds the hook by its path suffix after a restart, makes no write when it already matches, and retries failures after a minute. Without `manage_webhook` the owner adds the webhook by hand.

**Minimum GitHub permissions.** These apply to the connection identity on each repository:

- **Reads.** Issues, pull requests, checks, commit statuses, contents and metadata, for reconcile, push enrichment and the agent's reads.
- **Writes.** Issues and pull requests (labels, comments, reactions, opening PRs), and contents (attachment uploads).
- **Hooks.** Administration only for `manage_webhook`.

That maps to these grants:

- **Fine-grained token.** Issues, Pull requests and Contents read/write; Commit statuses, Metadata and Actions read; Webhooks read/write for `manage_webhook`. A fine-grained token cannot be granted Checks. CI is then read from Actions workflow runs and commit statuses, and doctor says so (03-06).
- **GitHub App.** The same, plus Checks read.
- **Classic token.** `repo`, plus `admin:repo_hook` for `manage_webhook`.
- **Bot App (`bot_app_id`).** It performs the writes and needs only the write permissions.

## Dry run

`serve --dry-run` makes no external writes (v1 `--dry-run`):

- Every GitHub entry runs with `projection.dry_run`, so labels, comments, reactions and the GitHub tools' writes report `dry_run`.
- Managed hooks are only reported (`dry_run`).
- Claude Code turns run as stubs on a cheap model (`claudecode.Options.DryRun`, model `haiku`), told to do nothing and call no tools.

Continuity, outcomes and history behave as usual. Shell executions are not altered, since they are the operator's own commands. Job dry-run semantics belong to 05-02. Doctor reports dry run on `github` and `claude_code`.

## Smaller changes

- The host loop no longer reports idle `ready` Work (handed back, with no input) as `invalid_state` on every pass: Work without pending input is skipped quietly.
- `runtime.AttemptStateInTx` exposes an attempt's pinned executor registration to composition and management.

## Verification

```sh
go test -race ./internal/cli ./internal/tui ./extensions/tunnel ./extensions/github/... ./executors/claudecode
go test -race -run 'TestManagementOperatorControls|TestOfflineValidation|TestSetupCommand|TestSetupGuide|TestDaemonFreshInstall|TestDaemonGitHubWebhookListener|TestDaemonGitHubDryRun' ./integration
make check && GOTOOLCHAIN=go1.24.2 make check && make cross-build && make drill
```

- **Fresh installation.** [`TestDaemonFreshInstallGitHubIssue`](../../integration/daemon_operator_test.go) runs the following steps against the composed daemon, the GitHub fake and a fake claude:
  1. `init`, `secret set` (file store, never echoed), `config set` and `config check`.
  2. Daemon start, and doctor with `--check-secrets` passing (`github_auth verified`, `claude_code installed`, `github_capability ok`, `webhooks reconcile_only`), with no reference or value in the report.
  3. The issue claimed and worked, with labels converging and annotations shown.
  4. `work attach` holding during the session and releasing after, both as `core.manual` Events in the Work's history.
  5. A manual claim of a skip-labelled issue, and a scope reconcile.
  6. A revoked token diagnosed as `reconcile_failing` and `credential_rejected`.
- **Webhooks.** [`TestDaemonGitHubWebhookListener`](../../integration/daemon_operator_test.go) checks the listener on an ephemeral port, a static public URL creating the managed hook, a signed `issues` delivery accepted (202) and worked well before the next reconcile, and refusals for a bad signature (401), an unknown route and `/v1/health` (404).
- **Dry run.** [`TestDaemonGitHubDryRun`](../../integration/daemon_operator_test.go) checks that the issue is still claimed and worked while GitHub records no writes and no hook, and that doctor reports the dry run.
- **Controls.** [`TestManagementOperatorControls`](../../integration/operator_controls_test.go) runs on SQLite through the real server and CLI. It covers rebind (refused for an unknown binding or a stale revision, queued input moved, the Event payload), hold blocking `Start` and release allowing it, attach refused without executor support and leaving an existing hold in place, and scope reconcile making a source due.
- **Offline validation and setup.** [`setup_guide_test.go`](../../integration/setup_guide_test.go) checks that offline validation accepts the guide's example and every scope `init` writes and rejects an unknown executor, and that the setup command builds the right plugin, arguments and environment.
- **Unit tests.** They cover JSON pointer editing, `init`/`secret`/`config` behavior, the tunnel providers (fake ngrok and cloudflared processes, restart, static), `Verify`, `EnsureWebhook` (create, unchanged, re-point by suffix, dry run), the manual claim route, annotations, Claude Code `Version`, `Usage` against a fake endpoint, the dry-run turn, and TUI controls with confirmation, the claim prompt and Work context.

## Limitations and follow-on work

- **Fixtures only.** No real GitHub write, webhook, tunnel process or Claude Code session ran. Real evidence (a real repository, a real tunnel and a real agent turn) belongs to 03-06; `LoginToken` is not exercised in tests because on macOS it reads the real Keychain.
- **Not ported.** The full-screen config editor (06-01), a rendered transcript and activity view (raw transcripts are reachable as artifacts with `t`), and v1's automatic headless/TUI selection on the root command (06-03 packaging). `settings.admission_limit` is still not enforced (02-09 open item).
- **Rebind and reload.** A rebind takes effect at the next scheduling decision, which pins the binding from the generation the scheduler runs.

04-01 update: the webhook listener also mounts `POST /slack/<scope>/<entry>` for Slack entries with `receiver: http`. The default Slack receiver is Socket Mode, which needs no listener or tunnel; see the [Slack extension](slack-extension.md), including the intended setup flow (04-02): a generic optional extension `Setup` hook that core `init <type>` dispatches to, with core supplying only the setup host.

## Extension-owned setup (04-02)

Setup moved out of core CLI. The public, optional [`extension.Setup`](../../extension/setup.go) contract has two methods. `SetupInfo` gives the type, a summary and a usage line. `Setup(ctx, host, args)` returns a `SetupResult`: connections, one scope, summary lines and next steps.

`open-conduit init` lists the registered setups. `init <type> [flags]` runs one. Core keeps only the host:

- **Its own flags.** `--config`, `--force`, `--dry-run` and `--with` are removed wherever they appear; every other argument goes to the extension.
- **The `SetupHost`.** It offers prompts (a secret prompt does not echo on a terminal), a numbered choice, a browser opener and a `LookPath`. It runs a helper command attached to the terminal. `StoreSecret` uses the `secret set` store (keyring, else the owner-only `secrets.json`) and returns only the reference. A dry run stores nothing and returns the reference it would use.
- **Merge and save.** `MergeSetup` keeps an existing connection with the same id and replaces a scope only with `--force`. The result is validated offline against the daemon registry, then saved 0600 (or printed with `--dry-run`).

An `extension.ErrSetupUsage` refusal is a usage error (exit 2).

The entrypoint injects the setups through `cli.Env.Setups` (`bootstrap.Setups()`: GitHub and Slack). `internal/cli` imports no extension. The B01 checker already forbids it, and `scripts/check-boundaries` now has explicit direct and transitive cases for core CLI and TUI. The GitHub init (remote detection, `ScopeConfig`, tool lists) lives in [`extensions/github/setup.go`](../../extensions/github/setup.go) as `init github`, with the same flags and behavior. Slack's setup is described in the [Slack extension](slack-extension.md#setup-init-slack).


06-01 adds `init linear`, `init jira` (each with an optional `--bitbucket` code host), `adapters [TYPE]`, `route KEY`, `config edit` (the full-screen editor) and the tracker, Bitbucket and PostHog doctor checks and webhook routes. See [installed integrations](installed-integrations.md).

06-02 adds explicit conversion and offline recovery commands ([migration and recovery](migration-recovery.md)): `migrate config --from V1 [--out FILE | --write]`, `backup create|verify|restore`, `state check` and `auth recover`. The state commands take the daemon lock and refuse while a daemon owns the directory.
