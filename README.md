# Open Conduit

Open Conduit runs coding and support agents against the places work arrives: GitHub issues, Slack threads, schedules and its own defect reports. A single local daemon accepts what happens, decides which agent or command handles it, runs that attempt, and keeps going across waits, restarts and failures. You watch and steer it from a terminal UI.

Everything is built from five primitives:

- A **Work** item is a durable objective, such as an issue to fix or a thread to answer.
- An **Event** is an immutable record of something that happened, and is accepted durably before anything acts on it.
- An **Execution** is one attempt to advance Work through an executor, such as Claude Code or a shell command.
- A **Timer** or **Wait** makes time and suspension durable, so Work can park until a matching Event or a timeout wakes it.
- A **Scope** holds the configuration, secret references, routes and executor bindings for one area of work.

Integrations and executors plug in around that core, so GitHub, Slack, scheduled jobs and introspection all share one history, one retry and timeout model, and one view. See [the core in one page](docs/architecture/core.md).

Open Conduit succeeds [Open Supervisor](https://github.com/alsiola/open-supervisor). It keeps its own configuration and state, so Open Supervisor stays installable alongside it as a fallback while you move scopes across. See [Moving from Open Supervisor](#moving-from-open-supervisor).

## Install

**Homebrew**:

```sh
brew install open-conduit/tap/open-conduit
```

The cask puts only `open-conduit` on your PATH. `os` stays beside it in the cask's own directory, where the daemon finds it, so it can't clash with another command of that name. Open Supervisor's `open-supervisor` cask installs side by side.

**Release binaries**: archives for macOS and Linux, on amd64 and arm64, are on the [releases page](https://github.com/open-conduit/open-conduit/releases) as `open-conduit_<version>_<os>_<arch>.tar.gz`, with a `checksums.txt`. Each holds `open-conduit` and `os`. Keep them in the same directory. The install script downloads the latest one, verifies its checksum against `checksums.txt`, puts both binaries in `~/.local/lib/open-conduit` and links `open-conduit` into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/open-conduit/open-conduit/main/install.sh | sh
```

Set `OPEN_CONDUIT_VERSION=v0.1.0` (or another tag) to install a particular release rather than the latest, and `BIN_DIR` or `INSTALL_DIR` to change where the link and the binaries go.

**go install**: you need Go 1.24.2 or later. Install both commands, which puts them side by side in `$(go env GOBIN)` (default `~/go/bin`):

```sh
go install github.com/open-conduit/open-conduit/cmd/open-conduit@latest
go install github.com/open-conduit/open-conduit/cmd/os@latest
```

`os` must sit beside `open-conduit` for the daemon to find it, and it lands on your PATH there under that generic name. Install both into the same directory, or point the daemon elsewhere with `serve --toolbelt-dir DIR`.

**From source**. You need Go 1.24.2 or later, Git and Make:

```sh
git clone https://github.com/open-conduit/open-conduit.git
cd open-conduit
make build                 # writes bin/open-conduit and bin/os
cp bin/open-conduit bin/os /usr/local/bin/   # or anywhere on your PATH, side by side
```

`open-conduit` is the daemon and operator client. `os` is the small toolbelt that shell commands call from inside a run; the daemon looks for it beside its own executable (`serve --toolbelt-dir DIR` points elsewhere).

You also need the CLIs of the services you use: [Claude Code](https://docs.anthropic.com/en/docs/claude-code), signed in, for agent bindings, and optionally the GitHub CLI (`gh`), whose login is the default GitHub credential.

## Quick start

Set up a GitHub repository and a Slack support channel, start the daemon, and check it:

```sh
open-conduit init                                    # list the available setups
open-conduit init github --repo acme/web --checkout ~/Code/web
open-conduit init slack                              # creates the Slack app, asks for its tokens, picks a channel
open-conduit config check                            # validate the config offline
open-conduit serve                                   # run the daemon (leave it running)
```

In another terminal:

```sh
open-conduit doctor --check-secrets                  # every connection, credential and binding
open-conduit tui                                     # watch and control runs
```

- `init TYPE` adds one scope to the config (`open-conduit config path` shows where it lives) and never replaces an existing scope without `--force`. `--dry-run` prints the result instead of saving it, and `open-conduit init --help` lists every setup's flags. `init --with claude` instead starts Claude Code with a setup guide that walks you through the same steps.
- The config never holds a credential, only a reference to one: `env:NAME`, `file:NAME`, `keyring:NAME` or `cmd:COMMAND`. By default GitHub uses `cmd:gh auth token`. To store a token instead, enter it with `secret set` (it is read from stdin, never shown) and point the connection at the reference it prints:

  ```sh
  open-conduit secret set github-token
  open-conduit config set /connections/github/secret_refs/token '"keyring:github-token"'
  ```

- `config show`, `config get POINTER`, `config set POINTER JSON` and `config unset POINTER` edit the file by JSON pointer and validate before saving. `config edit` opens a full-screen editor. A running daemon picks up a changed file only when you restart it or run `config apply FILE --generation N`.
- `serve --dry-run` makes no external writes and runs agents as cheap stubs, which is a safe way to watch a new setup. `serve --webhook-addr 127.0.0.1:8787 --tunnel ngrok|cloudflared|static` adds webhooks for lower latency. Without webhooks the daemon reconciles on an interval, so nothing is lost.
- In the TUI, `c` claims an issue by hand, `R` runs or retries, `H` holds or releases, `a` attaches to the agent's session, and `x`/`X` cancel a run or its Work. The same controls exist on the command line: `claim`, `work retry|hold|release|attach|cancel`, `runs list`, `runs log ID` and `events chain ID`.

## Workflows

Each workflow is a scope in `~/.config/open-conduit/config.json` (or `$XDG_CONFIG_HOME/open-conduit/config.json`). The examples below are complete, minimal configs that pass `open-conduit config check`. `init` writes the GitHub and Slack ones for you, with fuller tool lists. `open-conduit adapters [TYPE]` lists every integration and the fields it takes.

A scope always names what it may use: `authorization` is the ceiling of connections and operations, and each binding lists the operations its executor may call. Routes send Events to a binding, and choose what happens when one arrives while an attempt is running (`queue`, `inject`, `interrupt` or `drop`).

### GitHub engineering

Claims open issues (skipping those with a skip label, and optionally requiring a label), works each in its own branch and checkout with Claude Code, opens a pull request and waits on checks and review comments. A comment that arrives mid-turn is injected into the running session.

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "cmd:gh auth token"}}
  ],
  "scopes": [
    {
      "id": "web",
      "name": "acme/web",
      "secret_refs": {},
      "authorization": {
        "connections": ["github"],
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                       "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web"}}
      ],
      "routes": [
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "agent",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
          "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",
                         "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
          "secret_names": [],
          "config": {"branch": "agent/issue-{number}"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "work"}}
        }
      ]
    }
  ]
}
```

Leave out `resource` (and set `workspace` to `false`) for a scope whose turns need no checkout; `init github --no-workspace` writes that shape. `init github --holds` adds a hold route that parks issues until you claim them. Linear and Jira trackers, with GitHub or Bitbucket as the code host, have their own setups (`init linear`, `init jira`); see [installed integrations](docs/architecture/installed-integrations.md).

### Slack support

Answers threads in a channel with a workspace-free agent: no repository or checkout is involved. Each thread is one Work item, follow-ups are injected into a running turn, and replies are marked so the agent never answers itself. The connection uses Socket Mode, so no public URL is needed.

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "slack", "type": "slack", "config": {},
     "secret_refs": {"bot_token": "keyring:slack-bot-token", "app_token": "keyring:slack-app-token"}}
  ],
  "scopes": [
    {
      "id": "support",
      "name": "Slack support",
      "secret_refs": {},
      "authorization": {
        "connections": ["slack"],
        "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "support", "type": "slack", "connection": "slack", "secret_names": [],
         "config": {"namespace": "slack/T0123456789", "scope": "support", "team": "T0123456789",
                    "channels": ["C0123456789"], "start": "all", "receiver": "socket"}}
      ],
      "routes": [
        {"name": "conversations", "handler": "slack", "binding": "support", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "support",
          "executor": "claude-code",
          "connection": "slack",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": false},
          "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"],
          "secret_names": [],
          "config": {"playbook": "support-conversation", "args": "{key}"}
        }
      ]
    }
  ]
}
```

`init slack` creates the app from a manifest, verifies both tokens, stores them in the keyring and fills in the team, channel and bot user. To answer only when mentioned, set `"start": "mention"` with the app's `bot_user_id`.

### Scheduled jobs

Runs commands on a cron schedule, at fixed times, every N minutes, on code-host events, on a signed webhook or by hand. Each job keeps one run going and at most one waiting, and a slot missed while the daemon was down is caught up if it is less than six hours late.

```json
{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [
    {
      "id": "ops",
      "name": "Scheduled jobs",
      "secret_refs": {},
      "authorization": {"connections": [], "operations": ["read_context", "declare_outcome"], "read_scopes": []},
      "extensions": [
        {"name": "jobs", "type": "schedule", "secret_names": [],
         "config": {"namespace": "schedule/ops", "scope": "ops", "jobs": [
           {"name": "nightly-report", "cron": "0 7 * * *", "time_zone": "Europe/London"},
           {"name": "refresh", "every_minutes": 60}
         ]}}
      ],
      "routes": [
        {"name": "report", "handler": "schedule", "binding": "report", "active_policy": "queue",
         "config": {"jobs": ["nightly-report"]}},
        {"name": "refresh", "handler": "schedule", "binding": "refresh", "active_policy": "queue",
         "config": {"jobs": ["refresh"]}}
      ],
      "bindings": [
        {"name": "report", "executor": "shell", "requires": {"toolbelt": "cli"},
         "operations": ["read_context", "declare_outcome"], "secret_names": [],
         "config": {"command": "./scripts/nightly-report.sh", "dir": "/path/to/repo"}},
        {"name": "refresh", "executor": "shell", "requires": {"toolbelt": "cli"},
         "operations": ["read_context"], "secret_names": [],
         "config": {"command": "make refresh", "dir": "/path/to/repo"}}
      ]
    }
  ]
}
```

A job can also run an agent turn: point its route at a `claude-code` binding instead of a `shell` one. Run a job by hand with `open-conduit events emit --scope-id ops --type schedule.run --delivery-id run-1 --key-source schedule/ops --key-external-id job/nightly-report`. See [scheduled jobs](docs/architecture/schedule-extension.md) for every trigger.

### Introspection

Agents report what breaks for them (add `introspection.report-issue` to a binding's operations). A daily triage reads those reports and the agents' notes, groups them, and opens fix Work that lands a change. An optional sweep folds the agents' corrections into your rule files, and an optional host watch reports load, memory and disk pressure.

```json
{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [
    {
      "id": "eng",
      "name": "Engineering",
      "secret_refs": {},
      "authorization": {
        "connections": [],
        "operations": ["read_context", "declare_outcome", "introspection.evidence", "introspection.reports",
                       "introspection.fix", "introspection.report.resolve", "introspection.notes.resolve"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "introspection", "type": "introspection", "secret_names": [],
         "config": {"triage": {"cron": "51 8 * * *", "time_zone": "UTC"}}}
      ],
      "routes": [
        {"name": "triage", "handler": "introspection", "binding": "triage", "active_policy": "queue",
         "config": {"work": "triage"}},
        {"name": "fixes", "handler": "introspection", "binding": "fix", "active_policy": "queue",
         "config": {"work": "fix"}}
      ],
      "bindings": [
        {"name": "triage", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
         "operations": ["read_context", "declare_outcome", "introspection.evidence", "introspection.reports",
                        "introspection.fix", "introspection.report.resolve", "introspection.notes.resolve"],
         "secret_names": [], "config": {"playbook": "triage-reports"}},
        {"name": "fix", "executor": "claude-code", "requires": {"toolbelt": "mcp", "workspace": true},
         "operations": ["read_context", "declare_outcome", "introspection.reports", "introspection.report.resolve"],
         "secret_names": [], "config": {"playbook": "fix-report"},
         "resource": {"provider": "git", "config": {"checkout": "/path/to/repo", "mode": "work"}}}
      ]
    }
  ]
}
```

In practice the introspection entry sits in the same scope as your GitHub agent. See [introspection](docs/architecture/issue-reporting.md) for the sweep, the host watch and by-hand runs.

## Extensions

Integrations outside the built-in set run as separate processes that speak a JSON-RPC protocol over stdio. A package is a directory holding a manifest, an executable and a certification fixture:

```sh
open-conduit extension inspect ./my-extension        # check a package offline
open-conduit extension certify ./my-extension        # run the conformance suite against it
open-conduit extension install ./my-extension        # takes effect at the next serve
open-conduit extension list
open-conduit extension update ./my-extension         # the replaced release is kept
open-conduit extension rollback my-extension
open-conduit extension remove my-extension           # refused while the config uses it
```

A package can contribute integrations, executors, toolbelt operations, setups and doctor checks, and its types are known to `config check` once installed. The protocol and packaging are provisional and may still change. See [extension packaging](docs/architecture/extension-packaging.md) for writing one with the Go SDK, and [the extension protocol](docs/architecture/extension-protocol.md) for the wire format.

## Moving from Open Supervisor

Open Conduit never reads or changes Open Supervisor's files or state. Convert the config explicitly:

```sh
open-conduit migrate config --from ~/.config/open-supervisor/config.yaml           # preview the result and a report
open-conduit migrate config --from ~/.config/open-supervisor/config.yaml --write   # save it to Open Conduit's config path
open-conduit config check
```

The report lists what converted, what needs a decision and what is unsupported. In-flight state is not imported: move one scope at a time, let it drain in Open Supervisor, then start it in Open Conduit, which reconciles current issues, threads and jobs from the services. Back up before every upgrade or switch:

```sh
open-conduit backup create ~/backups/open-conduit-2026-10-26   # with the daemon stopped
open-conduit backup verify ~/backups/open-conduit-2026-10-26
open-conduit state check                                       # diagnose the state directory
```

[Migration and recovery](docs/architecture/migration-recovery.md) covers the scope-by-scope transfer, rolling back to Open Supervisor, restoring a backup and recovering credentials (`auth recover`).

## Platforms

Open Conduit runs on macOS and Linux, on amd64 and arm64. Windows is not supported. Secrets go to the macOS Keychain or, on Linux, the Secret Service through `secret-tool`; without either, `secret set` falls back to an owner-only `secrets.json` beside the config and says so.

State lives in `~/.local/state/open-conduit` (or `$XDG_STATE_HOME/open-conduit`), and the daemon is controlled over a private Unix socket in that directory. `--config`, `--data-dir` and `--socket` (or `OPEN_CONDUIT_CONFIG`, `OPEN_CONDUIT_DATA_DIR` and `OPEN_CONDUIT_SOCKET`) override the defaults.

## Getting help

- `open-conduit --help` lists every command, and `open-conduit init --help` lists every setup's flags.
- `open-conduit doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs, and says what to fix.
- `open-conduit route KEY` explains how an issue's or thread's Events were routed, and `open-conduit runs log ID` shows a run's output.
- Report bugs and ask questions in [GitHub issues](https://github.com/open-conduit/open-conduit/issues). Report vulnerabilities privately, as [SECURITY.md](SECURITY.md) describes.

To work on Open Conduit itself, read [CONTRIBUTING.md](CONTRIBUTING.md) and [the docs index](docs/README.md).
