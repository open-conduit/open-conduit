---
title: Command line
description: "Every open-conduit command and flag, as the CLI's own help prints them."
---

This page is generated from the CLI's help (`open-conduit --help` and `open-conduit COMMAND --help`) by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.

Every command takes the global flags before it, and `--help` after it:

```text
Usage: open-conduit [--endpoint unix:///path | https://host:port] [--token-file PATH] [--socket PATH] [--data-dir DIR]
```

```text
Lists print a table; --json prints the records as JSON. Lists accept --limit and --cursor.
Controls without --revision act on the current revision; pass --revision N to act only if it is still N.
Mutations accept --key to retry safely with the same Idempotency-Key.
Run open-conduit COMMAND --help for one command.
```

## Set up

### `open-conduit init`

```text
Usage: open-conduit init TYPE [flags] [--config PATH] [--force] [--dry-run]
       open-conduit init --with EXECUTOR   start an executor's CLI on the setup guide (--with claude, --with codex)

Types (from the registered extensions):
  github   add a scope that works a GitHub repository's issues (package github 0.1.0)
           [--repo OWNER/NAME] [--scope ID] [--checkout DIR | --no-workspace] [--token REF | --broker URL --broker-id ID --broker-key REF] [--connection ID] [--holds] [--require LABEL] [--executor claude-code|codex]
  jira     add a scope that works Jira projects' issues (package jira 0.1.0)
           [--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF] --site https://acme.atlassian.net --projects PROJ[,WEB] [--email E | --auth bearer] [--jql JQL] [--fields customfield_1,…] [--scope ID] [--token REF] [--connection ID] [--checkout DIR | --no-workspace]
  linear   add a scope that works Linear teams' issues (package linear 0.1.0)
           [--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF] --teams ENG[,OPS] [--scope ID] [--token REF] [--connection ID] [--checkout DIR | --no-workspace] [--states unstarted,started] [--agent-account NAME]
  setup    a scope for setting Open Conduit up from the web view (package setup 0.1.0)
           [--scope ID]
  slack    add a workspace-free scope whose agent works Slack threads (package slack 0.1.0)
           [--scope ID] [--name APP] [--channel C…] [--start all|mention] [--receiver socket|http] [--public-url URL] [--bot-token REF (--app-token REF | --signing-secret REF) --team T…] [--via slack-cli] [--no-open] [--connection ID] [--dir FOLDER]
```

### `open-conduit config`

```text
config check [--file F]    validate the config file offline (no daemon needed)
config path | config show [--file F]
config get POINTER | config set POINTER JSON | config unset POINTER   [--file F] (local file, validated before saving)
config include add [FOLDER] --parent SCOPE | config include remove [FOLDER]   [--file F] (compose a folder's .open-conduit/config.json)
config include add --name NAME --parent SCOPE | config include remove --name NAME   [--file F] (declare an include whose fragment is submitted to the daemon)
config include show NAME | config include apply NAME FILE [--hash H] [--key K]   (read or apply a submitted include's fragment; apply needs configure covering its parent)
config edit [--file F]     full-screen editor with the adapters' documented fields, validated before saving
config validate FILE       validate against the running daemon's registrations (needs the daemon)
config apply FILE [--generation N] [--key K]   make FILE the running daemon's next generation (needs the daemon;
                           --generation applies only if N is still current, for scripted compare-and-swap)
config export [FILE]       write the running generation as a config file, secret references as references
                           (needs the daemon and the admin role; FILE defaults to the daemon's config file, - prints it;
                           never replaces a file)
```

### `open-conduit secret`

```text
secret set NAME [--store keyring|file] | secret path   (value read from stdin, never an argument)
```

### `open-conduit adapters`

```text
adapters [TYPE] [--json]   the registered integrations and the fields each takes
```

### `open-conduit serve`

```text
serve [--config PATH] [--toolbelt-dir DIR] [--lifecycle-log PATH] [--poll-interval 1s] [--lease-duration 1m] [--retention-interval 10m] [--sweep-interval 6h] [--call-timeout 5s]
      [--backup-interval 24h] [--backups-kept 7] [--backup-dir DIR]
                           (scheduled online backups into <data-dir>/backups; --backup-interval 0 turns them off)
      [--upgrade-bundles-kept 3] [--upgrade-bundles-max-age 0] [--no-upgrade-bundle]
                           (a first start on a newer record format takes a verified pre-upgrade-<time> bundle in the
                           backup directory and won't start without one; --no-upgrade-bundle skips it for this run)
      [--restore BUNDLE [--restore-start-config FILE] [--restore-artifacts NAME --restore-artifacts-location LOC [--restore-artifacts-command PATH]] [--restore-trust-key KEYS] [--restore-accept-unsigned]]
                           (restore BUNDLE in place before serving, as backup restore --in-place does, once:
                           a start that finds the directory restored from it already serves it as it is)
      [--webhook-addr 127.0.0.1:8787] [--tunnel TYPE [--tunnel-config JSON] | --public-url URL]
                           (override the config's settings.webhooks for this run; TYPE is a tunnel
                           a package provides, such as ngrok or cloudflared, and JSON its config)
      [--probe-addr 0.0.0.0:8080]
                           (also serve /livez and /readyz, with no credential, for an orchestrator's probes)
      [--no-tunnel] [--dry-run] [--no-telemetry] [--no-updates]
      (--webhook-addr takes any free local port; 8787 is only an example)
      (--no-telemetry, OPEN_CONDUIT_NO_TELEMETRY=1 or DO_NOT_TRACK=1 turn off the anonymous usage reports)
      (--no-updates: nothing installs a release through the daemon or its web view, for a binary a package
      manager, an image or a fleet manages; open-conduit upgrade still works on the machine)
      [--web] [--web-addr 127.0.0.1:7420] [--web-host open-conduit.localhost]
                           also serve the web view from the daemon (open-conduit web url prints its sign-in URL)
      [--web-subject-header HEADER --web-token-file FILE]
                           behind a sign-in proxy that passes each person's own credential in HEADER, act for
                           that person, with FILE's credential (auth add … --act-for operator) in place of yours
      [--management-addr HOST:PORT --management-tls-cert FILE --management-tls-key FILE [--management-client-ca FILE]]
      [--management-addr 127.0.0.1:PORT --management-trusted-proxy]
                           also serve the management API over the network: TLS (mutual TLS with a client CA),
                           or plaintext on loopback to a local proxy that terminates TLS; off by default
```

### `open-conduit service`

```text
Usage: open-conduit service install [--no-start] [serve flags] | uninstall | start | stop | status

Runs the daemon in the background as a per-user service: a launchd LaunchAgent
on macOS (~/Library/LaunchAgents/com.open-conduit.daemon.plist), a systemd user unit on
Linux (~/.config/systemd/user/open-conduit.service). It starts at login and restarts
if the daemon exits with an error.

install takes the same flags as serve (for example --webhook-addr 127.0.0.1:8787
--tunnel cloudflared) and the global --data-dir and --socket, and records the
config path, the state directory and a PATH that finds claude or codex, git and
gh (and keeps the installer's own, where a tunnel's tool is found). Run it again to change the flags or after installing a tool the
daemon needs. Output goes to <state>/logs/service.log.
```

## Observe

### `open-conduit health`

```text
health                     the daemon's state and components; exits 1 unless it is ready
```

### `open-conduit quiescence`

```text
quiescence                 what is in flight, or when the daemon is next due; exits 1 unless it is quiescent
```

### `open-conduit drain`

```text
drain [--reason TEXT] | drain status | drain end
                           stop starting attempts so the state can be moved; exits 1 until it is safe to move
```

### `open-conduit portability`

```text
portability                what is tied to this machine, before a move; exits 1 unless nothing is and it is drained
```

### `open-conduit doctor`

```text
doctor [--check-secrets] [--json]   check the config, its bindings, the agent CLIs and the daemon
```

### `open-conduit scopes`

```text
scopes list [--parent-id ID] [--enabled true|false] | scopes get ID
scopes reconcile ID [--revision N]
```

### `open-conduit work`

```text
work list [--scope S --state --kind --parent-id] [--json] | work get ID
work create --scope S --kind K --binding B [--schema S --data JSON] [--key K]
work cancel|retry|hold|release ID [--revision N] [--reason TEXT] [--key K]
work interrupt ID [--revision N] [--reason TEXT] [--key K]   stop the running turn; the Work and its queued input stay for the next
work rebind ID --binding B [--revision N] [--reason TEXT] [--key K]
work attach ID [--keep-held] [--stream]
                               hold, attach to the executor's session, release on exit;
                               --stream (automatic over https) runs it through the daemon
work wake ID --type T --delivery-id D --key-source S --key-external-id X [--revision N] [--schema S --data JSON] [--key K]
```

### `open-conduit runs`

```text
runs list [--scope S --work-id --state --executor-binding --executor --work-kind --outcome] [--json] | runs get ID
runs log ID [--follow] [--transcript] [--range bytes=A-B]
```

### `open-conduit events`

```text
events list [--scope S --work-id --type --after-sequence] [--json] | events get ID | events chain ID
events skip ID [--reason TEXT] | events reroute ID [--reason TEXT]
events emit --scope S --type T --delivery-id D [--key-source S --key-external-id X] [--work-id W] [--causation-id E] [--schema S --data JSON] [--key K]
```

### `open-conduit executions`

```text
executions list [--scope S --work-id --state] [--json] | executions get ID
executions cancel ID [--revision N] [--reason TEXT] [--key K]
```

### `open-conduit waits`

```text
waits list [--scope S --work-id --execution-id --state] [--json] | waits get ID
```

### `open-conduit artifacts`

```text
artifacts list [--work-id --execution-id --kind] [--json] | artifacts cat ID [--range bytes=A-B]
```

### `open-conduit route`

```text
route KEY [--scope S] [--limit N]   explain how a key's Events were routed (KEY: ENG-12, issue/ENG-12, #12 or an external ID)
route --trace TYPE|SAMPLE --scope S [--source SRC] [--key-source S --key-external-id X] [--work-id W] [--schema S --data JSON]
                           dry-run how an Event would route now (nothing is recorded)
route --samples --scope S  the sample Events the scope's routes offer for --trace
```

### `open-conduit actions`

```text
actions list (--scope S | --work W)   the actions extensions offer there now
actions run PROVIDER ID (--scope S | --work W) [--input TEXT] [--key K]   invoke one as the local operator
```

### `open-conduit tui`

```text
tui                        the run history in the terminal
```

### `open-conduit web`

```text
web [--addr 127.0.0.1:7420] [--host open-conduit.localhost] [--subject-header HEADER] [--no-updates]   the same view in a browser on this machine (prints a sign-in URL); with --subject-header, it acts for the person a sign-in proxy passes the credential of in HEADER (its own credential needs --act-for operator); --no-updates leaves out the update page (installing a release from it needs the admin role)
web url [--addr ADDR] [--host NAME]   print the web view's sign-in URL (it lasts across restarts)
```

## Control

### `open-conduit container`

```text
container attach ID [--stream]
                           attach to held Work's session in a container, as work attach does for Work that ran in one
```

### `open-conduit claim`

```text
claim [SCOPE#]N [--scope S] [--entry NAME]   claim an issue by hand (the scope's claim action)
```

## Maintain

### `open-conduit auth`

```text
auth rotate | auth revoke TOKEN_ID [--reason TEXT] [--key K]
auth operators | auth add NAME --out FILE [--role admin|configure|operate|view] [--grants SCOPE=ROLE,...] [--ttl DURATION] [--act-for KIND]   list operator credentials; issue one for a named operator (default role operate, or only its --grants: view, operate or configure at each scope's subtree), recorded as operator:NAME, that expires after --ttl (e.g. 8h) if given, and that may act for KIND:ID subjects with --act-for (operator: for operators, within both's authority)
auth issuers | auth issuer set ID --type TYPE [--config JSON] [--groups JSON] [--grants JSON] | auth issuer remove ID   list, trust or stop trusting an identity provider issuer (admin); its credentials are recorded as operator:ID:SUBJECT, with the role --groups maps their groups to and the scoped roles --grants rows ({group, scope, role}) give them
auth recover               (daemon stopped) revoke every operator credential and write a fresh token file
```

### `open-conduit backup`

```text
backup keygen FILE | backup create DEST [--unsigned] | backup verify BUNDLE | backup restore BUNDLE (--to DIR | --in-place) [--start-config FILE] [--artifacts NAME --artifacts-location LOC [--artifacts-command PATH]] [--trust-key KEYS] [--accept-unsigned] [--config FILE] | backup list DESTINATION | backup fetch DESTINATION BUNDLE DIR [--trust-key KEYS] [--accept-unsigned]   (create, verify and restore need the daemon stopped; list and fetch read a destination in settings.backup.destinations, and fetch needs a trust key or --accept-unsigned; a bundle is signed with settings.backup.signing_key and checked against settings.backup.trust_keys or --trust-key; --to writes a new state directory, --in-place replaces the daemon's own and moves what it replaces to before-restore-*; both put the artifacts into an empty store, write a fresh operator token and report why the config generation would not validate against the installed packages)
```

### `open-conduit state`

```text
state check [--json]       (daemon stopped) diagnose the state directory: permissions, schema, integrity, artifacts, drain status
```

### `open-conduit extension`

```text
Usage: open-conduit extension COMMAND [--config PATH]
  new NAME [--lang go|ts] [--dir DIR] [--module PATH] [--replace DIR] [--json]
                                start a package repository in DIR (default ./NAME) whose make check passes first time:
                                one operation, NAME.hello, its tests, a Makefile, CI, a playbook and a README;
                                it requires this release of open-conduit (Go) or @open-conduit/sdk (--lang ts);
                                --replace builds against a checkout instead
  list [--json]                 installed packages (in <config dir>/extensions)
  inspect DIR [--json]          check a package directory offline
  check DIR [--json] [--allow NAME,…]
                                lint a package without starting it: manifest, compatibility with this binary,
                                the operations its playbooks name, certification.json; says what to change for each problem
  install SOURCE | update SOURCE [--sha256 HEX] [--signature PATH|URL]
                                install a package, or replace an installed one (the old release is kept for rollback);
                                SOURCE is a package directory, or a release .tar.gz as a path or URL (plain http needs --sha256);
                                under settings.extension_trust, a trusted publisher must have signed the archive
                                (its bundle is SOURCE.sigstore.json unless --signature names it) or its digest be pinned
  rollback NAME                 swap back the release the last update replaced
  remove NAME [--force]         remove a package (refused while the config uses its types; its state is kept)
  certify DIR [--json] [--report FILE]   run the conformance runner on a package
Changes take effect at the next open-conduit serve.
```

### `open-conduit dev`

```text
Usage: open-conduit dev [DIR] [--secret NAME=VALUE|NAME=env:VAR]... [--entry TYPE=JSON]... [--agent PATH] [--web-addr 127.0.0.1:PORT]
Builds the extension package in DIR (default .), starts a daemon on a config and state directory of
its own in a temporary directory, with the scope extensiontest generates from the package's manifest
(an entry per extension type, a claude-code binding granted its operations, its routes and a manual
route), serves the web view and prints its URL. When the package's files change it rebuilds the
package and restarts the daemon on the same state; Ctrl-C stops everything and removes the directory.
  --secret NAME=VALUE    a secret every generated entry selects (NAME=env:VAR reads it from the environment)
  --entry TYPE=JSON      the config of the generated entry for TYPE ({} when not given)
  --agent PATH           the Claude Code CLI the binding runs (default: claude on PATH)
  --web-addr ADDR        where the web view listens (default: a free port on 127.0.0.1)
```

### `open-conduit upgrade`

```text
Usage: open-conduit upgrade [--check] [--version VERSION [--allow-unsigned]] [--no-restart] [--json]

Installs the latest release in place of this open-conduit and the os beside
it, then restarts the background service so the daemon runs the new version.
The release's checksums.txt must carry a signature made by its release
workflow on main, and the archive must match checksums.txt; otherwise
nothing is installed.

  --check           only say whether a newer release is out
  --version V       install release V (for example 2.0.0) instead of the latest
  --allow-unsigned  with --version, allow a release from before v1.15.0,
                    which has no signature, on its checksums alone
  --no-restart      leave the background service running the old version
  --json            print the result as JSON

A Homebrew install is upgraded with brew: brew upgrade --cask open-conduit.
```

### `open-conduit version`

```text
open-conduit test (docs)
```
