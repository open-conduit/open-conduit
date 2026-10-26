---
name: setup
description: Set Open Conduit up for its owner, from nothing or over an existing config. Finds out which coding agent, GitHub repositories and checkouts they use and how issues map to scopes, then writes the JSON config, validates and applies it with `open-conduit config`, and checks the lot with doctor. Started as an interactive session with the owner.
---

# Set up Open Conduit

Open Conduit watches GitHub repositories, claims open issues, and works each one in its own coding-agent session in a git worktree, a turn at a time; comments, check results and merges become the next turn's events. Your job is to set it up for the person you're talking to: find out what they use, write the config, and leave them with a daemon that `doctor` passes and they understand.

The command is `{{bin}}`. The configuration is one JSON file (default `~/.config/open-conduit/config.json`; `serve --config` or `OPEN_CONDUIT_CONFIG` choose another; `{{bin}} config path` prints it). These commands edit it locally and validate against the daemon's own registrations before saving, so an invalid edit never lands:

- `{{bin}} init github --dry-run --repo OWNER/NAME --scope ID [--checkout DIR | --no-workspace] [--token REF] [--holds]` prints the scope `init github` would add (a connection, an agent binding with the GitHub tools, optionally the hold route); without `--dry-run` it adds it to the file. The owner may prefer to run `init` themselves.
- `{{bin}} config show` (references redacted), `config get /pointer`, `config set /pointer JSON`, `config unset /pointer` and `config check`. Pointers address array elements by `id` or `name`: `/scopes/web/bindings/agent/config/model`.

Before the daemon has ever run, that is all. Once it runs, a disk edit alone never changes what it does: check the file with `{{bin}} config validate <file>` and activate it with `{{bin}} config apply <file> --generation <n>` (`validate` prints the current generation): apply validates again, saves the file atomically and keeps the last good one. Validation prints every problem as a JSON pointer, a code and a remedy; fix them all before applying.

This is a conversation. Open by saying in two or three sentences what you'll do together, then look around (below) before your first question, so the questions you ask are about what you found. Go one topic at a time, and wait for answers.

## Ground rules

- **Secrets never pass through you.** Never ask for a token in the chat, never put one in the config or on a command line, never echo one. A credential goes in a connection's `secret_refs` as a reference:
  - `env:GITHUB_TOKEN`: the variable must be set wherever the daemon runs, not only in this shell.
  - `cmd:gh auth token`: any command that prints the secret (the gh CLI login, 1Password's `op read …`, `security find-generic-password -w …`).
  - `keyring:<name>` or `file:<name>`: the owner runs `{{bin}} secret set <name>` in their own terminal. It reads the value without echoing it and stores it in the OS keyring (`open-conduit` service), falling back to the owner-only `secrets.json` beside the config, then prints the reference to use. Never run it for them with a value.

  Offer these, let them choose, and wait for them to say it's done.
- **GitHub** needs no separate token when the `gh` CLI is signed in: `"token": "cmd:gh auth token"`. Check with `gh auth status`; if they're signed out, they run `gh auth login`. A GitHub App (`"auth": "app"` with `app_id` and an `app_private_key` reference) and a bot App for writes (`bot_app_id`, `bot_private_key`) are also supported.
- **Ask, don't assume.** This includes which repositories are in scope, how issues map to scopes, and anything that labels, moves or comments on issues. Read what you can find yourself (git remotes, `gh repo list`) and present it for them to confirm.
- **Explain as you go,** in a sentence or two, what each step means for them ("claimed issues get the `agent-in-progress` label").
- **Keep questions few and concrete.** Group them, and offer the likely answer: "Your checkouts under ~/Code with GitHub remotes are web, api and ios. All three?"

## Find out what's there

Start by looking, not asking:

```
ls ~/.config/open-conduit/config.json
{{bin}} health               # whether a daemon is running
{{bin}} doctor               # what the current config and daemon report
```

An existing config may be half done. Build on it rather than starting over: read it, tell the owner what's already there and what `config validate` says, then ask whether to finish that or change something. Never remove a connection, scope, binding, route or extension entry the owner didn't ask you to remove.

Look for the owner's checkouts. Try the current directory and the usual places (`~/Code`, `~/src`, `~/dev`, `~/projects`), one or two levels deep. Use `git -C <dir> remote get-url origin` to see which repository each one is. Check which tools are installed (`claude`, `gh`) with `command -v`.

## Questions to settle

1. **Coding agent.** The `claude-code` executor, the agent running you. The defaults are a good start: model `opus`, and permission mode `bypassPermissions`, because turns run unattended. Say what that permission mode means and let them choose. Note too that by default sessions use the machine's Claude login rather than an API key.
2. **Repositories.** Which GitHub repositories Open Conduit watches. Each is one `github` extension entry in a scope, with a stable `namespace` (for example `github/github.com/owner/repo`).
3. **Scopes.** A scope is one project: its repository, its checkout, its agent binding and its routes. Usually one scope per repository. A shared tracker repository whose issues several code repositories' PRs close is a `tracker` on each code repository's entry.
4. **Checkouts.** The local clone each scope's worktrees come from, the remote (default `origin`) and, only if it isn't the repository's default, the base branch PRs target. A checkout is optional: a scope without a `resource` is workspace-free, and its turns run in a private directory (fine for triage or questions; code changes need a checkout).
5. **Holds.** Whether issues labelled `scheduled` (opened by an agent with a wait, or by people with a `<!-- runAt: … -->` marker) should be held and then started. That needs the `github-hold` route and binding below.
6. **Webhooks.** Without them the daemon reconciles every 15 minutes (`settings.reconcile_seconds`). Reconcile always runs as well, so nothing is missed either way: webhooks only cut latency. To add them, the entry names a scope secret in `webhook_secret` (select it in the entry's `secret_names`, with a reference in the scope's `secret_refs`), and the daemon runs with `serve --webhook-addr 127.0.0.1:8787`. The port is only an example: any free local port works, so pick another if something already listens on 8787. That listener serves only the webhook routes of the registered extensions (this entry's `POST /github/<scope>/<entry>`, and the Slack, Linear, Jira, Bitbucket, schedule and installed-package routes of any others) and never the management API; anything else is 404. Something must forward a public URL to it: `--tunnel ngrok` (with `--tunnel-domain` for a stable URL), `--tunnel cloudflared` (quick, or `--tunnel-name` with `--tunnel-hostname`), or `--public-url` for a reverse proxy they already run. With `"manage_webhook": true` the daemon creates and re-points the repository's webhook itself, which needs admin access to the repository's hooks; otherwise they add the webhook (JSON, the secret, the issues, comments, pull request, review, check and status events) by hand.
7. **Limits.** How many attempts may run at once (`settings.concurrency`, default 4) and how many issues each scope holds at a time (`settings.admission_limit`, default 10). Suggest starting low, around 2 and 3, so they can watch it work.

## Write the config

Connections first, then one scope per project. A scope's `authorization` is its ceiling: the connections and toolbelt operations its bindings may use. A binding lists exactly the operations its executions get; the agent binding needs the builtins plus the GitHub tools.

```json
{
  "version": 1,
  "settings": {"concurrency": 2, "admission_limit": 3},
  "connections": [
    {"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "cmd:gh auth token"}}
  ],
  "scopes": [
    {
      "id": "web", "name": "Web",
      "authorization": {"connections": ["github"], "read_scopes": [], "operations": [
        "read_context", "emit", "wait", "declare_outcome",
        "github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
        "github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.checks",
        "github.attachment.upload", "github.attachment.get", "github.wait", "github.correction", "github.problem",
        "github.hold.status", "github.hold"]},
      "bindings": [
        {"name": "agent", "executor": "claude-code", "connection": "github",
         "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
         "operations": ["read_context", "wait", "declare_outcome",
           "github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
           "github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.checks",
           "github.attachment.upload", "github.attachment.get", "github.wait", "github.correction", "github.problem"],
         "secret_names": [],
         "config": {"status_schema": "github.status.v1", "branch": "agent/issue-{number}"},
         "resource": {"provider": "git", "config": {"checkout": "/Users/me/Code/web", "mode": "work", "copy": [".env"], "setup": "npm ci", "setup_when": ["package-lock.json"]}}},
        {"name": "hold", "executor": "github-hold", "connection": "github", "requires": {"toolbelt": "sdk"},
         "operations": ["emit", "github.hold.status", "github.hold"], "secret_names": [], "config": {}}
      ],
      "routes": [
        {"name": "held", "handler": "github-hold", "binding": "hold", "active_policy": "queue", "config": {}},
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web"}}
      ],
      "secret_refs": {}
    }
  ]
}
```

Other `github` entry fields:
- `labels`: `claimed`, `scheduled`, `help`, `review`, `done`, `skip[]`, `require[]`. The defaults are Open Supervisor's names (`agent-in-progress`, `scheduled`, …).
- `base_branch`: the branch PRs target, when it isn't the repository's default.
- `branch_pattern`: how a PR's branch names its issue (default `^agent/issue-(\d+)$`, matching the binding's `branch`).
- `no_checks_minutes`: how long a PR head with no checks waits before the agent hears none ran (default 10).
- `projection.dry_run`: watch without labelling or commenting.
- `tracker`: `{repo, namespace}` of a shared tracker repository.

Other `git` resource fields: `remote`, `base_branch`, `copy` (files copied into each worktree), `setup` with `setup_when` (a command rerun when those files change), `dotenv` (opt into the checkout's `.env`), `sign` (keep the checkout's commit signing; off by default). Suggest `copy` and `setup` from what you see in the checkout (`.env` files, lockfiles).

A wait on another project (`pr:api/88:merged`) needs the waiting scope to list that scope in its `authorization.read_scopes`. Ask before adding one.

## Permissions

The connection's identity needs, on each repository: read access to issues, pull requests, commit statuses and contents (and check runs, where the identity can hold Checks) (reconcile and the agent's reads); write access to issues and pull requests (labels, comments, opening PRs) and to contents for the agent's pushes and attachment uploads; and administration of webhooks only for `manage_webhook`. A fine-grained token with Issues, Pull requests and Contents read/write plus Commit statuses, Metadata and Actions read covers it (add Webhooks read/write for `manage_webhook`), but a fine-grained token cannot be granted Checks: CI is then read from Actions workflow runs, falling back to commit statuses, so a PR is never falsely reported as having no checks, and the base branch's required-checks verification is skipped (`doctor` and the `github.checks` tool say so). For real check-run states use a GitHub App with the same grants plus Checks read. A classic token needs `repo` (and `admin:repo_hook` for `manage_webhook`). `{{bin}} doctor --check-secrets` resolves the credential privately and reports what it can do on each repository.

## Behaviour worth telling the owner

- **What gets claimed.** An open issue is claimed when it lacks the claimed label (`agent-in-progress`) and every skip label (`backlog`, `human`, `milestone`, `later`), and carries a required label when any are configured. When an issue's work completes and the issue is closed, the claimed label comes off; reopening the issue claims it again.
- **Held issues.** With the `held` route, a `scheduled` issue is claimed but not worked until its time comes or what it waits for happens; it keeps the `scheduled` label until then.
- **What the agents do.** They label and comment on issues and open PRs as the identity whose credentials the connection uses, unless the connection has a bot App.
- **Workspaces.** Each issue gets its own worktree on its own branch, kept across turns and removed when the issue's work completes; the branch always stays.

## Finish

1. Run `{{bin}} config validate <draft>`, then `{{bin}} config apply <draft> --generation <n>` (or, before the first start, save the file as the config).
2. Run `{{bin}} doctor --check-secrets` once the daemon runs. It checks storage, the config, bindings, connections and secrets, then GitHub: each entry's reconcile and projection, its credential and access (`github_auth`), whether the scope can claim and hold (`github_capability`), the webhook listener, Claude Code's `--version`, and resources. Fix anything it flags.
3. Summarise in a few lines:
   - the connection;
   - the scopes, their repositories and checkouts;
   - the routes (holds or not);
   - webhooks or reconcile only;
   - the limits.
4. Tell them:
   - start it with `{{bin}} serve` (add `--webhook-addr` and a tunnel for webhooks), and watch it with `{{bin}} tui` or `{{bin}} runs list`;
   - `{{bin}} serve --dry-run` first if they'd like to watch without anything changing on GitHub: no labels, comments or hooks are written and agent turns are cheap stubs (`projection.dry_run` does the GitHub half for one entry);
   - in the TUI, `c` claims an issue by hand (`scope#12`, whatever its labels), `R` runs or retries, `H` holds or releases, `a` attaches to the agent's session (new events queue until they exit), `x`/`X` cancel, and `l`/`t` show logs and transcripts. The same controls exist as commands: `{{bin}} claim`, `work retry|hold|release|attach|cancel|rebind` and `scopes reconcile`.

Don't start the daemon yourself: it's long-running, and they should run it in their own terminal.
