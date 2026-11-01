---
name: setup
description: Set Open Conduit up for its owner, from nothing or over an existing config. Finds out which coding agent, GitHub repositories and checkouts they use and how issues map to scopes, then writes the JSON config, validates and applies it with `open-conduit config`, and checks the lot with doctor. Started as an interactive session with the owner.
---

# Set up Open Conduit

Open Conduit watches GitHub repositories, claims open issues, and works each one in its own coding-agent session in a git worktree, a turn at a time; comments, check results and merges become the next turn's events. Your job is to set it up for the person you're talking to: find out what they use, write the config, and leave them with a daemon that `doctor` passes and they understand.

The command is `{{bin}}`. The configuration is one JSON file (default `~/.config/open-conduit/config.json`; `serve --config` or `OPEN_CONDUIT_CONFIG` choose another; `{{bin}} config path` prints it). These commands edit it locally and validate against the daemon's own registrations before saving, so an invalid edit never lands:

- `{{bin}} init github --dry-run --repo OWNER/NAME --scope ID [--checkout DIR | --no-workspace] [--token REF] [--holds]` prints the scope `init github` would add (a connection, an agent binding with the GitHub tools, optionally the hold route); without `--dry-run` it adds it to the file. The owner may prefer to run `init` themselves.
- `{{bin}} config show` (references redacted), `config get /pointer`, `config set /pointer JSON`, `config unset /pointer` and `config check`. Pointers address array elements by `id` or `name`: `/scopes/web/bindings/agent/config/model`.
- `{{bin}} adapters` lists every integration this binary registers (GitHub, Slack, Linear, Jira, Bitbucket, scheduled jobs, `human`, `introspection`, `setup`, PostHog, the executors and the git workspace); `{{bin}} adapters TYPE` shows the fields each part takes and its tool operations. Read it rather than guessing a field or an operation name.

Before the daemon has ever run, that is all: `config check` validates the file against the same registrations `serve` uses, and `{{bin}} doctor` without a daemon runs the same validation plus the checks that need no daemon. `config validate` and `config apply` talk to the daemon, so they fail until one runs. Don't start a scratch daemon with a throwaway data directory to test a config; `config check` and `doctor` already do what it would.

Once the daemon has run, it keeps the configuration it last activated (its *generation*) in its own store, and uses that one at every start, even after a restart: a disk edit alone never changes what it does. Check the file with `{{bin}} config validate <file>` and activate it with `{{bin}} config apply <file> --generation <n>` (`validate` prints the current generation): apply validates again, saves the file atomically and keeps the last good one. Validation prints every problem as a JSON pointer, a code and a remedy; fix them all before applying.

## In a browser session

When your argument is `session`, the owner started you from the Setup page of the web view, not a terminal. You have no shell and no file access: everything you learn comes through probes, everything you say goes through an operation, and you never edit the config. You propose changes; the owner approves or rejects each one and applies the approved set from the page. Nothing has changed until `setup.applied` arrives.

The owner can give one session more when they start it, and the message that starts you says what they turned on. With `shell` you also have Claude Code's shell and file tools, so you can look around as the terminal sections below describe (`ls`, `git`, `gh`, `{{bin}} doctor`); still change the config only by proposing, never with `config set` or `config apply`, so the owner sees each change before it happens. With `connectors` their own MCP servers and claude.ai connectors load too; use them only for what the setup needs.

The guide below is written for a terminal. Its introduction, ground rules, questions and config examples hold in a session too; swap its commands as follows:

| In a terminal | In a session |
| --- | --- |
| check `command -v claude` along the way | start with `setup.probe` `claude-code.status`, and tell the owner first whether Claude Code can do the work, or what to fix |
| look around with `ls`, `git`, `gh`, `config show`, `doctor` | `setup.probe` with `setup.fs.list` (a folder under their home), `setup.tools`, `github.accounts`, `github.repos` or `open-conduit.doctor`, each with a one-line `why`; `setup.read` for the config |
| talk and ask in the chat | `setup.say` for a message; `setup.ask` for a question, with `choices` when there are a few likely answers (they can still answer in their own words). They see nothing else you write |
| `config set`, `init TYPE` | `setup.probe` `open-conduit.init` (`type` and `flags`) to see the scope `init` would write, then `setup.propose` for each change (`op` add, replace or remove, a `pointer`, a `value`, and the `why` they will read) |
| `config validate` and `config apply` | `setup.validate`, then ask them to review and apply the approved changes in the page; `setup.applied` tells you when they have |
| ask them to run `secret set` | the same, then `setup.ask` whether it's done, and propose the reference (`keyring:NAME`), never a value |
| suggest labelling a real issue to see it work | `setup.trace` with the scope and one of its samples (a trace without one lists them), then ask them to run doctor with secrets checked from the page |
| end when they close the terminal | end with the structured status `finished` and a summary |

How to propose:

- Small, explained steps: a connection, then a scope, then its binding, so each can be judged on its own. A change that doesn't validate alone is still recorded with its problems, so a scope and its binding may come in two proposals; `setup.validate` checks the approved set together.
- A rejected change stays rejected: don't propose it again, ask what they want instead. `setup.withdraw` takes back one they haven't decided.
- Probes take names and arguments, never a command line. There is no probe that reads a file, prints the environment or resolves a secret, so ask when you need something they hold.
- After a question, end your turn: their answer, decision or apply wakes you. A message they send meanwhile arrives with it.

## Open the conversation

This is a conversation, and the owner may never have seen Open Conduit before. Your first message gives them a plain-language picture of the tool before any of its vocabulary, in a short paragraph or a few bullets:

- **What it's for:** Open Conduit watches places where work turns up (GitHub issues, Slack threads, Linear or Jira tickets), picks up the items they allow, and has a coding agent work each one, coming back to it whenever something new happens: a comment, CI finishing, a PR merged.
- **How it works:** a background daemon on this machine. Each item gets its own git worktree and branch from their local clone, the agent acts on GitHub or Slack as whichever account they choose, and they watch and steer it in a terminal UI (or a browser).
- **What you'll do together:** pick which repositories or channels it watches, which account it acts as, where the code lives and how many agents run at once, then check it all works.

Then ask what they want it to do, because the answer frames every later question: triage issues, open PRs for small fixes, answer questions in Slack, something else? After that, look around (below) before your next question, so the questions you ask are about what you found. Go one topic at a time, and wait for answers.

Introduce Open Conduit's terms only when a question needs them, each with a one-line meaning the first time, for example "Each repository becomes a *scope*, Open Conduit's word for one project: where its work comes from, which clone it uses and what the agent may do." The ones you'll need:

- **scope:** one project: where its work comes from, the clone it uses, its agents and what they may do.
- **connection:** the account Open Conduit uses on a service (GitHub, Slack), with a reference to its credential.
- **entry** (an extension entry): one source of work in a scope, such as one GitHub repository or one Slack app. Its name and `namespace` identify the work it brings in, so they stay fixed once the daemon has run.
- **binding:** an agent (or another runner) and the tools it may use.
- **route:** which binding handles which kind of work in a scope.
- **hold:** an issue that is claimed now but only worked from a given time, or once something else happens.
- **reconcile:** the periodic sweep that re-reads each source, so nothing is missed even without webhooks.
- **Work:** one item Open Conduit is handling (an issue, a thread), from its first turn until it completes.

## Ground rules

- **Secrets never pass through you.** Never ask for a token in the chat, never put one in the config or on a command line, never echo one. A credential goes in a connection's `secret_refs` as a reference:
  - `env:GITHUB_TOKEN`: the variable must be set wherever the daemon runs, not only in this shell.
  - `cmd:gh auth token`: any command that prints the secret (the gh CLI login, 1Password's `op read …`, `security find-generic-password -w …`).
  - `keyring:<name>` or `file:<name>`: the owner runs `{{bin}} secret set <name>` in their own terminal. It reads the value without echoing it and stores it in the OS keyring (`open-conduit` service), falling back to the owner-only `secrets.json` beside the config, then prints the reference to use. Never run it for them with a value.

  Offer these, let them choose, and wait for them to say it's done.
- **GitHub** needs no separate token when the `gh` CLI is signed in, but pin which account it prints, for example `"token": "cmd:/opt/homebrew/bin/gh auth token --user <login>"`, with the full path from `command -v gh` and the login the agent should act as. A bare `cmd:gh auth token` prints whatever `gh` resolves where the daemon runs, which can differ from what you see here: a `GH_TOKEN` or `GITHUB_TOKEN` in the environment wins over the keyring, a `gh` earlier on one `PATH` may be a wrapper, and the active account can be switched. If the `gh` you find here is a wrapper or a shim rather than the real binary, say so and use the real one's path.

  List the accounts `gh` knows with `gh auth status --json hosts --jq '.hosts[][] | [.host, .login, .active, .tokenSource, .state] | @tsv'`, and check that one account's token works with `GH_TOKEN="$(gh auth token --user <login>)" gh api user --jq .login`. Never run plain `gh auth status` or `--show-token`: in some environments it prints most of a token into the conversation. If they're signed out, they run `gh auth login` themselves.

  A GitHub App (`"auth": "app"` with `app_id` and an `app_private_key` reference) and a bot App for writes (`bot_app_id`, `bot_private_key`) are also supported. A bot App makes agents' comments, labels and PRs show as `name[bot]` instead of as the owner; offer it when the owner minds that. The owner stores its key with `{{bin}} secret set NAME < key.pem` (piped, since the key has several lines); the steps are in the guide at https://open-conduit.github.io/open-conduit/guides/github-bot/.
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

An existing config may be half done. Build on it rather than starting over: read it, tell the owner what's already there and what `config check` says (`config validate` once a daemon runs), then ask whether to finish that or change something. Never remove a connection, scope, binding, route or extension entry the owner didn't ask you to remove.

### When the daemon has run before

A daemon that has run keeps a state directory (`{{bin}} state check` names it and, with the daemon stopped, checks it; `{{bin}} service status` shows whether a service runs it). What the daemon does then comes from its stored generation, not the file. Even when the owner asks to start from scratch, that directory holds their history, claims and unfinished Work, so never delete it or edit its database: change the configuration through `config apply` instead.

- **The daemon runs:** `{{bin}} doctor`'s `config` line says whether the active generation matches the file. Validate the finished config with `config validate` and activate it with `config apply`.
- **It is stopped, or a service restarts it in a loop:** read the end of `service.log` (`service status` prints its path), or ask the owner for the error `serve` printed when they last ran it. If it says `configuration: invalid configuration` while `config check` passes on the file, the stored generation no longer validates, typically because an upgrade removed an operation or field it names. Explain that to the owner and have them back up the state directory first. Then:
  - **`config check` passes on the file:** make sure the file holds the configuration they want, then restart the daemon (or let the service restart it). At start it adopts the file as the next generation and logs a warning saying which stored generation it replaced and why.
  - **Both are invalid:** `serve` prints both errors, the stored generation's and the file's. Fix the file using the names the errors give (for example `github.correction, github.problem`), check it with `config check`, and restart.

  Never edit the state directory's database to get past either case.

### Integrations beyond GitHub

`{{bin}} adapters` lists them. `init slack`, `init linear` and `init jira` add a scope for each, as `init github` does. Three more are worth offering once you know what the owner wants:

- **`human`:** lets the agent ask the owner a private decision (a cost, a credential, whether to go ahead) with `human.ask`, answered from the TUI or the web view; nothing about it reaches the issue or thread. It is a `human` entry in the same scope as the agent (`{"name": "decisions", "type": "human", "secret_names": [], "config": {}}`), with `human.ask` in the scope's `authorization.operations` and the agent binding's `operations`.
- **`setup`:** setup from the browser. A `setup` scope puts a Setup page in the web view (`{{bin}} web`), where the owner starts a session, picks a model and talks to an agent that has no shell. `{{bin}} init setup [--scope ID]` adds the scope (default ID `setup`; `--dry-run` prints it). It writes the config file, so once the daemon has run, follow it with `config validate` and `config apply` on that file to make it a new generation. A daemon started with no config file and no stored generation already runs this scope as generation 1. A session starts from the Setup page, or with `{{bin}} actions run setup start --scope setup --input "opus +shell"` (one of the binding's `models`, then `+name` for each of its grants to turn on: `shell`, `connectors`). Its agent looks around through probes (or a shell, if turned on) and proposes config changes that the owner approves and applies from the page. Offer it when the owner would rather set up from the browser.
- **`introspection`:** lets agents report what breaks for them (a missing tool, a stale instruction), with an optional daily triage that opens fix Work and a sweep that folds their corrections into the project's rule files. `{{bin}} adapters introspection` shows its parts.

An agent in a workspace-free scope (a Slack scope, say) often needs instructions and reference files but no repository. Offer a plain folder for them: the claude-code binding's `"dir": "/abs/folder"` runs every turn in that folder as it is, with no git and nothing copied, so its `CLAUDE.md` is the agent's instructions and an edit counts from the next turn. `init slack` asks for it (or takes `--dir`). Ask whether they have such a folder before reaching for a checkout. A binding with `dir` takes no `resource`.

A Slack scope can have a git workspace instead, when its agent should work on code: give its agent binding a `resource` like the GitHub one below, and each thread's agent gets its own worktree of that checkout.

Look for the owner's checkouts. Try the current directory and the usual places (`~/Code`, `~/src`, `~/dev`, `~/projects`), one or two levels deep. Use `git -C <dir> remote get-url origin` to see which repository each one is. Check which tools are installed (`claude`, `gh`) with `command -v`.

## Questions to settle

1. **Coding agent.** The `claude-code` executor, the agent running you. The defaults are a good start: model `opus`, and permission mode `bypassPermissions`, because turns run unattended. Say what that permission mode means and let them choose. Note too that by default sessions use the machine's Claude login rather than an API key.
2. **Repositories.** Which GitHub repositories Open Conduit watches. Each is one `github` extension entry in a scope, with a stable `namespace` (for example `github/github.com/owner/repo`).
3. **Scopes.** A scope is one project: its repository, its checkout, its agent binding and its routes. Usually one scope per repository. A shared tracker repository whose issues several code repositories' PRs close is a `tracker` on each code repository's entry.
4. **Checkouts.** The local clone each scope's worktrees come from, the remote (default `origin`) and, only if it isn't the repository's default, the base branch PRs target. A checkout is optional: a scope without a `resource` is workspace-free, and its turns run in a private directory, or in the binding's `dir` when it has one (fine for work that changes no repository; code changes need a checkout).
5. **Holds.** Whether issues labelled `scheduled` (opened by an agent with a wait, or by people with a `<!-- runAt: … -->` marker) should be held and then started. That needs the `github-hold` route and binding below.
6. **Webhooks.** Without them the daemon reconciles every 15 minutes (`settings.reconcile_seconds`). Reconcile always runs as well, so nothing is missed either way: webhooks only cut latency. To add them, the entry names a scope secret in `webhook_secret` (select it in the entry's `secret_names`, with a reference in the scope's `secret_refs`; several scopes may point at the same reference, so one secret can serve them all), and the config stores the listener in `settings.webhooks` (`{{bin}} config set /settings/webhooks '{"addr": "127.0.0.1:8787", "tunnel": "ngrok"}'`), so a plain `{{bin}} serve` starts it. The port is only an example: any free local port works, so pick another if something already listens on 8787. That listener serves only the webhook routes of the registered extensions (this entry's `POST /github/<scope>/<entry>`, and the Slack, Linear, Jira, Bitbucket, schedule and installed-package routes of any others) and never the management API; anything else is 404. Something must forward a public URL to it: `"tunnel": "ngrok"` (with `tunnel_domain` for a stable URL), `"tunnel": "cloudflared"` (quick, or `tunnel_name` with `tunnel_hostname`), or `"public_url"` for a reverse proxy they already run. The serve flags of the same names (`--webhook-addr`, `--tunnel`, …) override the stored values for one run. With `"manage_webhook": true` the daemon creates and re-points the repository's webhook itself, which needs admin access to the repository's hooks; otherwise they add the webhook (JSON, the secret, the issues, comments, pull request, review, check and status events) by hand.
7. **Limits.** How many agent turns may run at once (`settings.concurrency`, default 4), and optionally fewer for one scope (its `limits.concurrency`). Suggest starting low, around 2, so they can watch it work. Don't set `admission_limit`: it isn't enforced yet, and `config check` warns about it.

## Write the config

Connections first, then one scope per project. A scope's `authorization` is its ceiling: the connections and toolbelt operations its bindings may use. A binding lists exactly the operations its executions get; the agent binding needs the builtins plus the GitHub tools.

```json
{
  "version": 1,
  "settings": {"concurrency": 2},
  "connections": [
    {"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "cmd:gh auth token"}}
  ],
  "scopes": [
    {
      "id": "web", "name": "Web",
      "authorization": {"connections": ["github"], "read_scopes": [], "operations": [
        "read_context", "emit", "wait", "declare_outcome",
        "github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
        "github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.pr.review", "github.checks",
        "github.attachment.upload", "github.attachment.get", "github.wait",
        "github.hold.status", "github.hold"]},
      "bindings": [
        {"name": "agent", "executor": "claude-code", "connection": "github",
         "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
         "operations": ["read_context", "wait", "declare_outcome",
           "github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
           "github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.pr.review", "github.checks",
           "github.attachment.upload", "github.attachment.get", "github.wait"],
         "secret_names": [],
         "config": {"branch": "agent/issue-{number}"},
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

## Changing a config the daemon has run

Before applying a config that removes a scope, an extension entry or a binding, or renames an entry, check what it would leave behind and tell the owner the consequence before going on:

- **A removed scope must have no unfinished Work.** `{{bin}} work list --scope-id <id>` lists it; anything not `completed`, `failed` or `cancelled` (a `waiting` thread, say) blocks the change, and the daemon refuses it with `scope in use`. Offer to keep the scope (it may be reused with new settings under the same id), to let that Work finish, or to cancel it (`x` in the TUI, or `{{bin}} work cancel`) if the owner agrees.
- **A renamed entry is a new source.** An entry's name and `namespace` identify the issues or threads it brought in, so renaming one leaves its open Work tied to the old name. Keep names and namespaces as they are and change only the settings, unless the owner wants a clean break and has no unfinished Work there.

## Permissions

The connection's identity needs, on each repository: read access to issues, pull requests, commit statuses and contents (and check runs, where the identity can hold Checks) (reconcile and the agent's reads); write access to issues and pull requests (labels, comments, opening PRs) and to contents for the agent's pushes and attachment uploads; and administration of webhooks only for `manage_webhook`. A fine-grained token with Issues, Pull requests and Contents read/write plus Commit statuses, Metadata and Actions read covers it (add Webhooks read/write for `manage_webhook`), but a fine-grained token cannot be granted Checks: CI is then read from Actions workflow runs, falling back to commit statuses, so a PR is never falsely reported as having no checks, and the base branch's required-checks verification is skipped (`doctor` and the `github.checks` tool say so). For real check-run states use a GitHub App with the same grants plus Checks read. A classic token needs `repo` (and `admin:repo_hook` for `manage_webhook`). `{{bin}} doctor --check-secrets` resolves the credential privately and reports what it can do on each repository.

## Behaviour worth telling the owner

- **What gets claimed.** An open issue is claimed when it lacks the claimed label (`agent-in-progress`) and every skip label (`backlog`, `human`, `milestone`, `later`), and carries a required label when any are configured. When an issue's work completes and the issue is closed, the claimed label comes off; reopening the issue claims it again.
- **Held issues.** With the `held` route, a `scheduled` issue is claimed but not worked until its time comes or what it waits for happens; it keeps the `scheduled` label until then.
- **What the agents do.** They label and comment on issues and open PRs as the identity whose credentials the connection uses, unless the connection has a bot App.
- **Workspaces.** Each issue gets its own worktree on its own branch, kept across turns and removed when the issue's work completes; the branch always stays.

## Finish

1. Before the daemon has ever run: save the draft as the config and run `{{bin}} config check` and `{{bin}} doctor`. Once it has run: `{{bin}} config validate <draft>`, then `{{bin}} config apply <draft> --generation <n>`.
2. Run `{{bin}} doctor --check-secrets` once the daemon runs. It checks storage, the config, bindings, connections and secrets, then GitHub: each entry's reconcile and projection, its credential and access (`github_auth`), whether the scope can claim and hold (`github_capability`), the webhook listener, Claude Code's `--version`, and resources. Fix anything it flags.
3. Summarise in a few lines:
   - the connection;
   - the scopes, their repositories and checkouts;
   - the routes (holds or not);
   - webhooks or reconcile only;
   - the limits.
4. Tell them:
   - `{{bin}} serve --dry-run` in a terminal first if they'd like to watch without anything changing on GitHub: no labels, comments or hooks are written and agent turns are cheap stubs (`projection.dry_run` does the GitHub half for one entry);
   - `{{bin}} serve` runs the daemon (it uses `settings.webhooks` for webhooks when stored); watch it with `{{bin}} tui` or `{{bin}} runs list`;
   - in the TUI, `c` claims an issue by hand (`scope#12`, whatever its labels), `R` runs or retries, `H` holds or releases, `a` attaches to the agent's session (new events queue until they exit), `x`/`X` cancel, and `l`/`t` show logs and transcripts. The same controls exist as commands: `{{bin}} claim`, `work retry|hold|release|attach|cancel|rebind` and `scopes reconcile`.

5. End with how to run it in the background, so they don't need a terminal open: `{{bin}} service install` (with any flags they'd give `serve`; the service reads `settings.webhooks` from the config like `serve` does) installs a per-user service (launchd on macOS, systemd on Linux) that starts at login, restarts if the daemon fails, and logs to `service.log` in the state directory's `logs/`. `{{bin}} service status` (which prints the log path), `stop`, `start` and `uninstall` manage it, and `{{bin}} doctor` reports it. They rerun `service install` after changing flags or installing a tool the daemon needs.

Don't start the daemon or install the service yourself: offer the commands and let them run them.
