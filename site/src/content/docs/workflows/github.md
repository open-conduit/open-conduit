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

- Issues with a skip label are left alone, and you can require a label before an issue is claimed.
- `init github --holds` adds a hold route that parks issues until you claim them by hand.
- `init github --no-workspace` writes a scope whose turns need no checkout.
- `open-conduit init --help` lists every flag.

## How a turn goes

1. A new issue is claimed and the agent starts in its own Git worktree on a branch such as `agent/issue-12`.
2. It reads the issue, does the work, pushes and opens a pull request, then ends its turn.
3. When the checks finish, a comment or review arrives, or the PR merges, Open Conduit resumes the agent with what happened. It never polls.
4. An agent can park an issue until another PR merges, another issue closes or a time comes, and Open Conduit wakes it when that happens.

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
