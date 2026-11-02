---
title: GitHub engineering
description: Claim GitHub issues, work each on its own branch with Claude Code, open a pull request and answer checks and review comments.
---

Open Conduit claims open issues, works each one on its own branch and checkout with Claude Code, opens a pull request and waits on checks and review comments. A comment that arrives while the agent is working is injected into the running session.

## Set it up

The quickest way is `open-conduit init --with claude`: Claude Code finds your checkouts, asks which repositories to watch, and writes and checks the GitHub scopes for you. See the [quick start](/open-conduit/start/quick-start/#set-it-up-with-claude).

To write a scope yourself:

```sh
open-conduit init github --repo acme/web --checkout ~/Code/web
open-conduit config check
```

- `init github --require agent` picks up only the issues you label `agent` (see below).
- `init github --holds` adds a hold route that parks issues until you claim them by hand.
- `init github --no-workspace` writes a scope whose turns need no checkout.
- `open-conduit init --help` lists every flag.

## What gets picked up

Once the daemon runs, it starts an agent on each open issue that passes all of these:

- **Its author is trusted.** By default that is the repository's owner, its organisation's members and its collaborators (GitHub's `OWNER`, `MEMBER` and `COLLABORATOR`). An issue anyone else opens is left alone, so a stranger on a public repository can't put instructions in front of an agent that runs with your access. Issues your own agents file for follow-up work are picked up too.
- **It has no skip label.** `backlog`, `human` and `milestone` keep an issue back, and so does `scheduled` while it waits for its hold. The entry's `labels.skip` changes the list.
- **It has a required label, if you set one.** With `labels.require` (for example `["agent"]`), only issues carrying one of those labels are picked up. On a repository with a backlog, set it before the first run, or the daemon picks up every open issue at once.
- **No one else has claimed it.** An issue already labelled `agent-in-progress` with no Work here belongs to another owner.

A route that claims pull requests applies the same author rule, and never picks up a PR from a fork.

To trust more people, widen the entry's `admit`, for example `"admit": {"authors": ["OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR"]}` (`["*"]` for anyone), and `"admit": {"forks": true}` for PRs from forks. Claiming an issue by hand starts it whatever its author or labels.

To see what would be picked up before anything happens, run `open-conduit serve --dry-run`: it makes no GitHub writes and runs agents as stubs. Each event's route trace says why an issue was or wasn't picked up, such as "the issue's author mallory is NONE on the repository".

`open-conduit doctor --check-secrets` warns when the repository is public, sets no `labels.require` and runs its agent without a sandbox, and when the branch PRs target has no branch protection or ruleset, since nothing on GitHub would then stop a push straight to it.

## How a turn goes

1. A new issue is claimed and the agent starts in its own Git worktree on a branch such as `agent/issue-12`.
2. It reads the issue, does the work, pushes and opens a pull request, then ends its turn.
3. When the checks finish, a comment or review arrives, or the PR merges, Open Conduit resumes the agent with what happened. It never polls.
4. An agent can park an issue until another PR merges, another issue closes or a time comes, and Open Conduit wakes it when that happens.

With holds on, an issue carrying the `scheduled` label waits for whatever its marker names (a time, a PR merging) before its agent starts. To start a held issue early, take the `scheduled` label off it: the agent starts at once, and Open Conduit doesn't put the label back.

The issue's status labels follow the state the agent reports at the end of each turn.

## Example config

Each workflow is a scope in the config. The example below is a complete, minimal config that passes `open-conduit config check`; see [configuration and secrets](/open-conduit/guides/configuration/) for what each part means.

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

Leave out `resource` (and set `workspace` to `false`) for a scope whose turns need no checkout.

## Linear, Jira and Bitbucket

Linear and Jira trackers, with GitHub or Bitbucket as the code host, have their own setups (`init linear`, `init jira`). See [installed integrations](../../architecture/installed-integrations/).
