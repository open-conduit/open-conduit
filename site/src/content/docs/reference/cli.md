---
title: Command line
description: "Every open-conduit command and flag, as the CLI's own help prints them."
---

This page is generated from the CLI's help (`open-conduit --help` and `open-conduit COMMAND --help`) by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.

Every command takes the global flags before it, and `--help` after it:

```text
Usage: open-conduit [--endpoint unix:///path] [--token-file PATH] [--socket PATH] [--data-dir DIR] COMMAND
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
           [--repo OWNER/NAME] [--scope ID] [--checkout DIR | --no-workspace] [--token REF] [--connection ID] [--holds] [--require LABEL] [--executor claude-code|codex]
  jira     add a scope that works Jira projects' issues (package jira 0.1.0)
           [--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF] --site https://acme.atlassian.net --projects PROJ[,WEB] [--email E | --auth bearer] [--jql JQL] [--fields customfield_1,…] [--scope ID] [--token REF] [--connection ID] [--checkout DIR | --no-workspace]
  linear   add a scope that works Linear teams' issues (package linear 0.1.0)
           [--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF] --teams ENG[,OPS] [--scope ID] [--token REF] [--connection ID] [--checkout DIR | --no-workspace] [--states unstarted,started] [--agent-account NAME]
  setup    a scope for setting Open Conduit up from the web view (package setup 0.1.0)
           [--scope ID]
  slack    add a workspace-free scope whose agent works Slack threads (package slack 0.1.0)
           [--scope ID] [--name APP] [--channel C…] [--start all|mention] [--bot-token REF --app-token REF --team T…] [--via slack-cli] [--no-open] [--connection ID] [--dir FOLDER]
```

### `open-conduit config`

```text
config check [--file F]    validate the config file offline (no daemon needed)
config path | config show [--file F]
config get POINTER | config set POINTER JSON | config unset POINTER   [--file F] (local file, validated before saving)
config include add [FOLDER] --parent SCOPE | config include remove [FOLDER]   [--file F] (compose a folder's .open-conduit/config.json)
config edit [--file F]     full-screen editor with the adapters' documented fields, validated before saving
config validate FILE       validate against the running daemon's registrations (needs the daemon)
config apply FILE [--generation N] [--key K]   make FILE the running daemon's next generation (needs the daemon;
                           --generation applies only if N is still current, for scripted compare-and-swap)
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
      [--webhook-addr 127.0.0.1:8787] [--tunnel TYPE [--tunnel-config JSON] | --public-url URL]
                           (override the config's settings.webhooks for this run; TYPE is a tunnel
                           a package provides, such as ngrok or cloudflared, and JSON its config)
      [--no-tunnel] [--dry-run] [--no-telemetry]
      (--webhook-addr takes any free local port; 8787 is only an example)
      (--no-telemetry, OPEN_CONDUIT_NO_TELEMETRY=1 or DO_NOT_TRACK=1 turn off the anonymous usage reports)
      [--web] [--web-addr 127.0.0.1:7420] [--web-host open-conduit.localhost]
                           also serve the web view from the daemon (open-conduit web url prints its sign-in URL)
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
health                     whether the daemon answers
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
work attach ID [--keep-held]   hold, attach to the executor's session, release on exit
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
web [--addr 127.0.0.1:7420] [--host open-conduit.localhost]   the same view in a browser on this machine (prints a sign-in URL)
web url [--addr ADDR] [--host NAME]   print the web view's sign-in URL (it lasts across restarts)
```

## Control

### `open-conduit container`

```text
container attach ID        attach to held Work's session in a container, as work attach does for Work that ran in one
```

### `open-conduit claim`

```text
claim [SCOPE#]N [--scope S] [--entry NAME]   claim an issue by hand (the scope's claim action)
```

## Maintain

### `open-conduit auth`

```text
auth rotate | auth revoke TOKEN_ID [--reason TEXT] [--key K]
auth operators | auth add NAME --out FILE [--role admin|operate|view]   list operator credentials; issue one for a named operator (default role operate), recorded as operator:NAME
auth recover               (daemon stopped) revoke every operator credential and write a fresh token file
```

### `open-conduit backup`

```text
backup create DEST | backup verify BUNDLE | backup restore BUNDLE --to DIR   (daemon stopped; restore writes a new state directory)
```

### `open-conduit state`

```text
state check [--json]       (daemon stopped) diagnose the state directory: permissions, schema, integrity, artifacts, drain status
```

### `open-conduit extension`

```text
Usage: open-conduit extension COMMAND [--config PATH]
  new NAME [--dir DIR] [--module PATH] [--replace DIR] [--json]
                                start a package repository in DIR (default ./NAME) whose make check passes first time:
                                one operation, NAME.hello, its tests, a Makefile, CI, a playbook and a README;
                                go.mod requires this release of open-conduit (--replace builds against a checkout instead)
  list [--json]                 installed packages (in <config dir>/extensions)
  inspect DIR [--json]          check a package directory offline
  check DIR [--json] [--allow NAME,…]
                                lint a package without starting it: manifest, compatibility with this binary,
                                the operations its playbooks name, certification.json; says what to change for each problem
  install SOURCE | update SOURCE [--sha256 HEX]
                                install a package, or replace an installed one (the old release is kept for rollback);
                                SOURCE is a package directory, or a release .tar.gz as a path or URL (plain http needs --sha256)
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
Usage: open-conduit upgrade [--check] [--version VERSION] [--no-restart] [--json]

Installs the latest release in place of this open-conduit and the os beside
it, after checking the archive against the release's checksums.txt, then
restarts the background service so the daemon runs the new version.

  --check       only say whether a newer release is out
  --version V   install release V (for example 1.9.0) instead of the latest
  --no-restart  leave the background service running the old version
  --json        print the result as JSON

A Homebrew install is upgraded with brew: brew upgrade --cask open-conduit.
```

### `open-conduit version`

```text
open-conduit test (docs)
```
