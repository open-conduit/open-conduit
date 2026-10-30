---
title: "Operator setup, doctor and engineering controls"
description: "How an operator sets up, checks and controls Open Conduit: init and config commands, secrets, doctor checks, Work controls, webhooks and tunnels, and dry run."
---

This page covers what an operator uses to set up and run Open Conduit on a repository: the `init`, `config` and `secret` commands, the GitHub and Claude Code doctor checks, the Work controls in the management API, the CLI and the TUI, the webhook listener with its tunnels, and dry run. All of it uses public surfaces:

- the management API ([B05](../config-management/));
- the extension contracts;
- composition in [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/tree/main/internal/bootstrap).

Core has only domain-free controls (rebind, hold/release, scope reconcile).

## Setup from nothing

These are the documented steps, which [`TestDaemonFreshInstallGitHubIssue`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_operator_test.go) runs end to end:

```sh
open-conduit init github --repo acme/web --scope web --checkout ~/Code/web   # or --no-workspace; --holds; --token REF
open-conduit secret set github-token                                   # only when the token is not cmd:gh auth token
open-conduit config set /connections/github/secret_refs/token '"keyring:github-token"'
open-conduit config check
open-conduit serve                                                     # settings.webhooks for webhooks, --dry-run to watch
open-conduit doctor --check-secrets
open-conduit tui
```

- **`init`** lists the setups the registered extensions offer, and **`init <type>`** runs one (setup is extension-owned; see [extension-owned setup](#extension-owned-setup)). **`init github`**:
  - It detects the current directory's GitHub remote.
  - It adds one scope for a repository: a `github` connection (default secret `cmd:gh auth token`), an agent binding on `claude-code` with the GitHub tools, the `github` route, and the `github-hold` binding and route with `--holds`.
  - It adds a `git` work resource only when there is a checkout. `--no-workspace` writes a workspace-free scope whose turns run in a private directory.
  - It merges into an existing config and never replaces a scope without `--force`.
  - `--dry-run` prints the result without writing it.
  - It refuses a token literal: `--token` takes only a reference.
- **`config path|show|get|set|unset|check`** work on JSON pointers:
  - Array elements are addressed by `id` or `name`, for example `/scopes/web/bindings/agent/config/model`.
  - Edits keep the owner's key order and untouched text, including handwritten routes.
  - `show` and `get` redact `secret_refs`.
  - `set` refuses a value that is not a reference under `secret_refs`.
  - Every edit is validated offline by `bootstrap.ValidateConfig` before saving: the same registrations as `serve`, without a daemon, storage or secret resolution. An invalid edit leaves the file unchanged. A running daemon changes only through `config apply`, so a disk edit never alters it.
- **`config include add [FOLDER] --parent SCOPE`** and **`config include remove [FOLDER]`** add or remove an [included folder](../config-management/#included-folders) in the config file's `includes`. FOLDER defaults to the current directory and is written as an absolute path. Both validate before saving, like `set`.
- **`secret set NAME`**:
  - It reads the value from stdin, with no echo on a terminal, and never takes it as an argument.
  - It stores the value in the platform keyring under the `open-conduit` service: on macOS through `security -i`, so the value is not in any argv; on Linux through `secret-tool`.
  - Otherwise it falls back, and says so, to the owner-only `secrets.json` beside the config.
  - It prints only the reference. `--store keyring|file` forces one store.
  - The daemon's resolver reads `file:` references from that file.
- **`init --with claude`**:
  - The setup playbook ([`setup.Playbook`](https://github.com/open-conduit/open-conduit/blob/main/extensions/setup/playbook/setup.md)) is written as its own one-skill plugin under `<state>/playbooks/setup`. The `setup` extension also adds it to the daemon's turn plugin, so a setup session's binding (`playbook: setup`) starts on it. With the argument `session` (the setup binding's `args`) the playbook runs in session mode: it probes instead of using a shell and proposes changes for the operator to approve and apply instead of running `config set` and `config apply` ([setup sessions](../setup-sessions/#the-setup-playbook-under-a-session)).
  - The command is an interactive `claude /open-conduit:setup --plugin-dir … --allowedTools …` with `OPEN_CONDUIT_CONFIG` set.
  - Only `open-conduit config`, `doctor`, `health`, `secret path` and `init github --dry-run` are pre-allowed, and `OS_*` credentials are removed from the environment.
  - The guide teaches `init`, `config set`, `secret set`, webhooks, permissions, dry run and the TUI controls. Its example config still validates against the daemon registry.

`config edit` opens a full-screen editor with the adapters' documented fields, validated before saving ([installed integrations](../installed-integrations/)). The commands above cover the same outcomes from a script: validated edits, add/remove bindings, references for secrets and handwritten routes kept.

## Doctor

`GET /v1/doctor` (`open-conduit doctor [--check-secrets]`) runs the core checks ([doctor](../observability-history/#doctor)) and the extension checks contributed by bootstrap. Details carry codes, identifiers and counts, never references or values.

| Check | Status and codes |
| --- | --- |
| `github` | Per entry: registration failures (`fail`, for example `github_connection_required`), rate-limit pauses, reconcile state (`reconcile_failing` with the scan error code and failure count, first scan pending, or the age of the last complete scan), projection failures (`projection_failing`) and dry run. No service calls. |
| `github_auth` | Only with `check_secrets=true`. Resolves each entry's credentials privately, authenticates the reader and any bot writer, and reads the repository (`Client.Verify`). Codes: `verified`; `credential_unavailable`, `credential_rejected`, `repo_unreachable` or `rate_limited` (`fail`); `read_only` when the identity cannot push and projections are live; `hook_admin_missing` when `manage_webhook` lacks hook admin. Reports `pull/push/admin` when GitHub returns them. |
| `github_capability` | Config gaps the validator accepts but that stop GitHub work: no `github` route, a route whose binding has no toolbelt, a claude-code binding without `github.context`/`declare_outcome`, a hold route that does not bind `github-hold` or comes after `github`, and (info) scheduled issues not claimed without a hold route. |
| `webhooks` | `reconcile_only` (info) with no listener; otherwise the bound address, the tunnel and public URL (`tunnel_down`, `tunnel_failed`), where the address and the tunnel came from (`serve flags` or `settings.webhooks`), `restart_required` when an applied generation changed `settings.webhooks` since `serve` started, and per entry its route, a missing `webhook_secret` (not mounted) and the managed hook state (`ok`, `dry_run`, `hook_permission_denied`, …). |
| `sandbox` | When a binding declares an srt sandbox: `srt <version> at <path>` (`ok`, or `untested_version` as `warn`); `srt_unavailable`, `userns_unavailable`, `seccomp_unavailable` or `preflight_failed` as `fail`, when sandboxed bindings run nothing ([B09](../sandboxes/#doctor)). `not_configured` (info) otherwise. |
| `claude_code` | Runs `--version` for each claude-code binding's binary (`installed`, or `claude_missing` as `fail`); reports dry run. With `serve --claude-usage` it also reads the subscription's 5-hour and 7-day usage windows (the undocumented OAuth endpoint, with the machine's Claude login) and warns at 90%. A read failure is "usage unknown", never a fault. Usage reading is opt-in because it reads the login token. |

## Operator controls

Every control is a management call with the operator credential and an `Idempotency-Key`. It commits an accepted `core.manual.*` control Event, marked routed as `control`, in the same transaction as its state change. History, `events chain` and telemetry therefore explain it. None launches an executor or forces a wake.

| Endpoint | Semantics |
| --- | --- |
| `POST /work/{id}/rebind` `{expected_revision,binding,reason}` | B05 rebind. Ready Work with no live or parked attempt only. The binding must be an enabled binding of the Work's scope. Moves every queued input (and the retry state) to it. `core.manual.rebind` records the binding, the previous bindings and the scheduler's generation. Never moves scope. |
| `POST /work/{id}/hold`, `/release` `{expected_revision,reason}` | A domain-free scheduling hold (`core.manual.hold`/`release`), held in its own KV record. While held, `Start` refuses (`not_eligible`), including a parked attempt's resume, but input keeps queueing and Waits keep resolving. Release lets the next attempt see everything that arrived. Allowed on nonterminal Work with no pending or running attempt. `GET /work/{id}` shows `hold`. |
| `POST /work/{id}/attach` `{}` | Held Work with no pending or running attempt. Returns the attach command (`argv`, `dir`) of the executor registration that ran the latest attempt, through the optional public `extension.AttachExecutor` (`attach_unsupported`, `nothing_to_attach`). Work any of whose segments ran in a sandbox is refused as `attach_sandboxed` before the executor is asked, since the command runs on the host, outside the sandbox ([process sandboxes](../sandboxes/#attach)). For claude-code this is `claude --resume <session>` in the workspace, and the handoff note is cleared, so the next turn continues the attached session. No environment is returned: the operator's own shell supplies it. |
| `POST /scopes/{id}/reconcile` `{expected_revision}` | B05 operator reconcile. Accepts `core.manual.reconcile` in the scope and makes every ingress source registered for the scope due now. The scan is the normal asynchronous reconcile. A generation already under way when the request commits (a page in flight, or part way through its pages) completes and leaves the source due, so a fresh generation always starts after the request. Returns 202 with the Event and the source names. |

`GET /work/{id}` also carries `annotations`: display-only lines an extension contributes. For GitHub these are the issue, its URL, a held issue's state, projection dry run and the counts of corrections and problems the agent logged (`github.Annotate`, durable state only).

**CLI.** The CLI adds:

- `work rebind|hold|release`;
- `work attach ID [--keep-held]`, which holds (refetching the revision when the host moved the Work), attaches, runs the command with the terminal and releases the hold it placed;
- `scopes reconcile ID`;
- `claim [SCOPE#]N [--entry NAME]`, which invokes the scope's `claim` action;
- `actions list (--scope S | --work W)` and `actions run PROVIDER ID (--scope S | --work W) [--input TEXT]`.

**Extension actions.** An extension declares the actions available on a scope or Work now (`extension.ActionProvider`, or `work.actions` and `work.act` for a package), each with a stable ID, a label and an optional input prompt. `GET /v1/scopes/{id}/actions` and `GET /v1/work/{id}/actions` list them by provider (a GitHub entry or a package). Each listing call to a provider has a 2 second timeout, and all of a request's calls 4 seconds; a provider that fails or times out contributes nothing (`GET /v1/work?with=actions,panels` lists them for a page of Work). `POST …/actions/{provider}/{action}` with `{"input": "…"}` and an `Idempotency-Key` invokes one. The call carries the caller (`{kind: "operator", id: "local"}` on the management socket). The provider authorises it and either refuses with its own code and message, leaving no Event, or returns the one Event to accept. The host accepts that Event under the source `core.action`, as the root of its own chain, with delivery `provider/action/key`. An Event that names a correlation key is addressed by that key. Otherwise an action on Work produces an Event for that Work. Only this path assigns `core.action`, so posting an Event of the same family and key through `POST /v1/events` grants none of an action's authority.

A manual claim is the GitHub entry's `claim` scope action. It refuses any caller but the local operator, input that is not an issue number, and an issue that already has Work. Otherwise it produces `github.claim.requested` keyed to the entry's `issue/N` (schema `github.claim.v1`). The `github` route creates `github.issue` Work for it whatever the labels. The route only accepts the Event from `core.action`, never feeds existing Work, ignores generation keys, and renders it for the agent as "claimed by hand".

A `human` entry offers `answer` on Work parked on one of its asks, only while the ask is open (see [human decisions](../../workflows/human-decisions/)). It refuses a caller outside the entry's `responders` and input that matches none of the ask's options. Otherwise it produces `human.answer` (schema `human.answer.v1`) on the ask's own key, `human`/`ask/<id>`, which no Work owns. The ask's wait depends on that key, with a filter that counts only this provider's `core.action` answer, so the asking Work wakes on the alias `human.answered`. The asker's renderer reads the answer through the alias's causation. The ask records live in the coordination namespace `human.asks`, and a daemon pass settles them as answered (the first answer accepted, with later ones recorded as late), expired or withdrawn.

**Extension panels.** An action provider may also show panels (`extension.PanelProvider`, or `view.render` with manifest `panels` for a package). A panel is a constrained document: a title and sections of text, key/value fields, a table, http(s) links and buttons, each button naming one of the provider's action IDs. `GET /v1/scopes/{id}/panels` and `GET /v1/work/{id}/panels` return them by provider. The host leaves out a panel that fails `extension.Panel.Validate` (size limits, control characters, a link that is not http or https) and resolves each button to the action as listed on the same target now, dropping one that is not listed, so a panel offers nothing the action path would refuse. The web view and the TUI render the same document, so no extension code runs in the browser; a button invokes its action through the actions route like any other. The `conformance.CheckPanels` case certifies a provider. The `human` entry shows a question panel on Work with an open ask: the question, its context, the options as a table, who may answer, its status and deadline, and the answer button while the ask can be answered. Its annotation is then just the question, for lists and the CLI.

**TUI.** The shared view keeps one run model for every executor. With a controlling source (`tui.Operator`) it adds these keys:

- `c` lists the scopes' actions (claim among them); a number picks one, then its prompt or a y/n;
- `R` runs or retries;
- `H` holds or releases;
- `a` attaches, through `tea.Exec` with the same hold/release flow;
- `x` cancels the attempt and `X` the Work, each after a y/n prompt;
- `l` opens a run's log; on a running run it shows the output so far and follows it;
- `t` opens a run's transcript (live or final) from the list or detail, and in the log view steps to the next output (log, stream-json transcript, stderr).

Detail shows the Work's correlation keys, hold, annotations and active or woken Waits as data, and its actions numbered `1`–`9`, with no executor or domain branch. A refused action shows the provider's code and message. A read-only source shows none of this.

## Webhooks, tunnels and reconcile-only operation

Webhooks are an accelerator: reconcile always runs, so with no listener the daemon is **reconcile-only** and loses only latency (`settings.reconcile_seconds`, default 15 minutes). `serve --no-tunnel` forces this.

- **Stored settings.** The owner records the listener and tunnel once in the config, and a bare `open-conduit serve` uses them:

  ```json
  "settings": {"webhooks": {"addr": "127.0.0.1:8787", "tunnel": "ngrok", "tunnel_domain": "hooks.example.ngrok.app"}}
  ```

  - `addr` is required. `tunnel` is `ngrok` (optional `tunnel_domain`), `cloudflared` (optional `tunnel_name` with `tunnel_hostname`) or `static` (`public_url`; `public_url` alone means `static`). `config check` rejects a field that does not belong to the chosen tunnel.
  - The serve flags of the same names (`--webhook-addr`, `--tunnel`, `--tunnel-domain`, `--tunnel-name`, `--tunnel-hostname`, `--public-url`) override it for one run. The address and the tunnel override separately, and any tunnel flag replaces the whole stored tunnel. `--tunnel-binary` is a flag only.
  - The listener starts with `serve`, so a change made with `config apply` takes effect at the next restart (doctor reports `restart_required` until then).
- **Listener.** `settings.webhooks.addr` (or `serve --webhook-addr HOST:PORT`) binds a separate HTTP server on its own TCP address. Any free local port works (the docs use 8787 as an example); if the bind fails, `serve` exits saying why (for example, the address is already in use) and suggests another port. It serves only the routes that registered entries name:
  - `POST /github/<scope>/<entry>` for each GitHub entry that names a `webhook_secret`;
  - `POST /slack/<scope>/<entry>` for each Slack entry with `receiver: http` ([Slack extension](../slack-extension/)); the default Slack receiver is Socket Mode, which needs no listener or tunnel;
  - `POST /linear/…`, `POST /jira/…` and `POST /bitbucket/<scope>/<entry>` for tracker and code-host entries ([installed integrations](../installed-integrations/));
  - `POST /schedule/<scope>/<entry>/<job>/<token>`, a schedule job's signed URL ([scheduled jobs](../schedule-extension/));
  - the paths an installed extension package registers when it is configured ([extension packaging](../extension-packaging/)).

  Anything else is 404, including `/v1/...`. It never sees management credentials and never forwards to the Unix socket.
  - Each request is verified by its entry's adapter against the entry's secret from the current generation before anything is parsed or accepted. For GitHub that is `github.WebhookHandler` (HMAC before parsing, 401 on a bad signature).
  - A delivery is pushed with `Ingress.Push`, and 202 is returned only after durable acceptance. A refusal is 503: GitHub may redeliver, and reconcile recovers the state either way.
- **Tunnel.** `extensions/tunnel` provides: `ngrok` (`tunnel_domain` for a reserved domain), `cloudflared` (a quick tunnel, or `tunnel_name` with `tunnel_hostname`) and `static` (`public_url`, for a reverse proxy or Tailscale Funnel).
  - A provider process that exits is restarted with a growing wait (5 seconds, doubling to 5 minutes), and each public URL it reports is passed to the host.
  - It forwards only to the webhook listener.
- **Managed hooks.** An entry with `"manage_webhook": true` (which requires `webhook_secret`) has its repository webhook created or re-pointed at `<public URL>/github/<scope>/<entry>`, with the secret and the events the normalizer understands (`github.HookEvents`). The host finds the hook by its path suffix after a restart, makes no write when it already matches, and retries failures after a minute. Without `manage_webhook` the owner adds the webhook by hand.

**Minimum GitHub permissions.** These apply to the connection identity on each repository:

- **Reads.** Issues, pull requests, checks, commit statuses, contents and metadata, for reconcile, push enrichment and the agent's reads.
- **Writes.** Issues and pull requests (labels, comments, reactions, opening PRs, submitting reviews with `github.pr.review`), and contents (attachment uploads). A review is submitted as the writer (the bot App, or else the token's account), which must not be the account that opened the PR to approve it or request changes ([whose review it is](../engineering-tools/#tool-operations)).
- **Hooks.** Administration only for `manage_webhook`.

That maps to these grants:

- **Fine-grained token.** Issues, Pull requests and Contents read/write; Commit statuses, Metadata and Actions read; Webhooks read/write for `manage_webhook`. A fine-grained token cannot be granted Checks. CI is then read from Actions workflow runs and commit statuses, and doctor says so.
- **GitHub App.** The same, plus Checks read.
- **Classic token.** `repo`, plus `admin:repo_hook` for `manage_webhook`.
- **Bot App (`bot_app_id`).** It performs the writes and needs only the write permissions.

## Dry run

`serve --dry-run` makes no external writes:

- Every GitHub entry runs with `projection.dry_run`, so labels, comments, reactions and the GitHub tools' writes report `dry_run`.
- Managed hooks are only reported (`dry_run`).
- Claude Code turns run as stubs on a cheap model (`claudecode.Options.DryRun`, model `haiku`), told to do nothing and call no tools.

Continuity, outcomes and history behave as usual. Shell executions are not altered, since they are the operator's own commands. Jobs follow their own dry-run rules ([scheduled jobs](../schedule-extension/#dry-run)). Doctor reports dry run on `github` and `claude_code`.

## Host loop and attempt state

- The host loop skips idle `ready` Work (handed back, with no input) quietly rather than reporting it as `invalid_state` on every pass.
- `runtime.AttemptStateInTx` exposes an attempt's pinned executor registration to composition and management.

## Extension-owned setup

Setup belongs to extensions, not the core CLI. The public, optional [`extension.Setup`](https://github.com/open-conduit/open-conduit/blob/main/extension/setup.go) contract has two methods. `SetupInfo` gives the type, a summary and a usage line. `Setup(ctx, host, args)` returns a `SetupResult`: connections, one scope, summary lines and next steps.

`open-conduit init` lists the registered setups. `init <type> [flags]` runs one. Core keeps only the host:

- **Its own flags.** `--config`, `--force`, `--dry-run` and `--with` are removed wherever they appear; every other argument goes to the extension.
- **The `SetupHost`.** It offers prompts (a secret prompt does not echo on a terminal), a numbered choice, a browser opener and a `LookPath`. It runs a helper command attached to the terminal. `StoreSecret` uses the `secret set` store (keyring, else the owner-only `secrets.json`) and returns only the reference. A dry run stores nothing and returns the reference it would use.
- **Merge and save.** `MergeSetup` keeps an existing connection with the same id and replaces a scope only with `--force`. The result is validated offline against the daemon registry, then saved 0600 (or printed with `--dry-run`).

An `extension.ErrSetupUsage` refusal is a usage error (exit 2).

The entrypoint injects the setups through `cli.Env.Setups` (`bootstrap.Setups()`: GitHub, Slack, Linear, Jira and setup). `init setup [--scope ID]` writes the scope a [setup session](../setup-sessions/) runs in, the same scope the daemon runs on when it starts with no config file. `internal/cli` imports no extension. The B01 checker forbids it, and `scripts/check-boundaries` has explicit direct and transitive cases for core CLI and TUI. The GitHub init (remote detection, `ScopeConfig`, tool lists) lives in [`extensions/github/setup.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/setup.go) as `init github`, with the same flags and behavior. Slack's setup is described in the [Slack extension](../slack-extension/#setup-init-slack).

## Other setup and recovery commands

- **Trackers and code hosts.** `init linear` and `init jira` (each with an optional `--bitbucket` code host), `adapters [TYPE]`, `route KEY`, `config edit`, and the tracker, Bitbucket and PostHog doctor checks and webhook routes are described in [installed integrations](../installed-integrations/).
- **Offline recovery.** `backup create|verify|restore`, `state check` and `auth recover` are described in [recovery](../recovery/). The state commands take the daemon lock and refuse while a daemon owns the directory.

## Verification

```sh
go test -race ./internal/cli ./internal/tui ./extensions/tunnel ./extensions/github/... ./executors/claudecode
go test -race -run 'TestManagementOperatorControls|TestOfflineValidation|TestSetupCommand|TestSetupGuide|TestDaemonFreshInstall|TestDaemonGitHubWebhookListener|TestDaemonGitHubDryRun' ./integration
make check && GOTOOLCHAIN=go1.24.2 make check && make cross-build && make drill
```

- **Fresh installation.** [`TestDaemonFreshInstallGitHubIssue`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_operator_test.go) runs the following steps against the composed daemon, the GitHub fake and a fake claude:
  1. `init`, `secret set` (file store, never echoed), `config set` and `config check`.
  2. Daemon start, and doctor with `--check-secrets` passing (`github_auth verified`, `claude_code installed`, `github_capability ok`, `webhooks reconcile_only`), with no reference or value in the report.
  3. The issue claimed and worked, with labels converging and annotations shown.
  4. `work attach` holding during the session and releasing after, both as `core.manual` Events in the Work's history.
  5. A manual claim of a skip-labelled issue, and a scope reconcile.
  6. A revoked token diagnosed as `reconcile_failing` and `credential_rejected`.
- **Webhooks.** [`TestDaemonGitHubWebhookListener`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_operator_test.go) checks the listener on an ephemeral port, a static public URL creating the managed hook, a signed `issues` delivery accepted (202) and worked well before the next reconcile, and refusals for a bad signature (401), an unknown route and `/v1/health` (404).
- **Dry run.** [`TestDaemonGitHubDryRun`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_operator_test.go) checks that the issue is still claimed and worked while GitHub records no writes and no hook, and that doctor reports the dry run.
- **Controls.** [`TestManagementOperatorControls`](https://github.com/open-conduit/open-conduit/blob/main/integration/operator_controls_test.go) runs on SQLite through the real server and CLI. It covers rebind (refused for an unknown binding or a stale revision, queued input moved, the Event payload), hold blocking `Start` and release allowing it, attach refused without executor support and leaving an existing hold in place, and scope reconcile making a source due.
- **Offline validation and setup.** [`setup_guide_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/setup_guide_test.go) checks that offline validation accepts the guide's example and every scope `init` writes and rejects an unknown executor, and that the setup command builds the right plugin, arguments and environment.
- **Unit tests.** They cover JSON pointer editing, `init`/`secret`/`config` behavior, the tunnel providers (fake ngrok and cloudflared processes, restart, static), `Verify`, `EnsureWebhook` (create, unchanged, re-point by suffix, dry run), the manual claim route, annotations, Claude Code `Version`, `Usage` against a fake endpoint, the dry-run turn, and TUI controls with confirmation, the claim prompt and Work context.

`LoginToken` is not exercised in tests, because on macOS it reads the real Keychain.

## Limitations

- **Root command.** A bare `open-conduit` prints help; it does not choose between the daemon and the TUI.
- **Unenforced setting.** `settings.admission_limit` is validated but not enforced; `config check` warns where a config sets it, and `init` no longer writes it.
- **Rebind and reload.** A rebind takes effect at the next scheduling decision, which pins the binding from the generation the scheduler runs.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **03-05** made the GitHub path operable for the first deployment: `init github`, the `config` and `secret` commands, `init --with claude`, the GitHub and Claude Code doctor checks, the operator controls, the webhook listener and tunnels, and dry run. Before it the daemon's resolver had no secrets file configured. It also stopped the host loop reporting idle `ready` Work as `invalid_state` and exposed `runtime.AttemptStateInTx`.
- At 03-05 no real GitHub write, webhook, tunnel process or Claude Code session ran; all evidence used fixtures. Real evidence (a real repository, a real tunnel and a real agent turn) was left to 03-06.
- At 03-05 the schema-driven interactive config editor was not ported (deferred to 06-01), nor a rendered transcript and activity view (raw transcripts were reachable as artifacts with `t`), nor automatic headless/TUI selection on the root command (left to 06-03 packaging). `settings.admission_limit` was a 02-09 open item.
- **03-06** added the fine-grained-token fallback to Actions workflow runs and commit statuses, and doctor's note about it.
- **04-01** mounted `POST /slack/<scope>/<entry>` on the webhook listener for Slack entries with `receiver: http`, and set out the intended setup flow: a generic optional extension `Setup` hook that core `init <type>` dispatches to, with core supplying only the setup host.
- **04-02** moved setup out of the core CLI into the extension-owned `extension.Setup` contract, with `init github` keeping the same flags and behaviour. At that point the injected setups were GitHub and Slack.
- **05-04** fixed scope reconcile: before it, a request that arrived while a generation was under way was absorbed, and the next scan waited a full interval.
- **06-01** added `init linear`, `init jira`, `adapters`, `route`, `config edit` (the full-screen editor) and the tracker, Bitbucket and PostHog doctor checks and webhook routes. A rendered activity artifact for Claude Code turns, shown by the TUI and `runs log`, was added alongside them.
- **06-02** added the offline recovery commands.
- **07-01** renamed the attach refusal `no_session` to `nothing_to_attach`.
