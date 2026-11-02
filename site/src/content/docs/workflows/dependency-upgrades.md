---
title: Dependency upgrades
description: Give every Dependabot or Renovate PR an agent that gets it green, fixing breaking changes on the PR's own branch, then says it is ready to merge.
---

Dependabot or Renovate opens the upgrade PRs, and Open Conduit gives each one an agent. If the checks pass, the agent says so on the PR. If they fail, it fixes the breaking change on the PR's own branch, waits for the checks again, and reports what it changed. You still merge.

This page uses the bots you may already run, rather than a scheduled job that upgrades dependencies itself. They already know your ecosystems, group and schedule upgrades, and open one PR per change. The part they can't do is fix what an upgrade breaks, and that is what the agent does.

It is [pull request Work](../../architecture/github-extension/#pull-request-work): a `pull_requests` route whose `authors` are the bots claims each of their PRs as its own Work. PRs by anyone else fall through to the next route.

## Example config

The `github` entry tracks pull requests. The `upgrades` route claims PRs opened by `dependabot[bot]` or `renovate[bot]` and starts the `upgrader` binding. Each PR gets its own worktree, and the agent can push to the PR's branch. The `issues` route works your issues as usual.

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
                       "github.issue.comment", "github.pr.view", "github.pr.create", "github.pr.comment", "github.checks", "github.wait"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web", "track_pull_requests": true}}
      ],
      "routes": [
        {"name": "upgrades", "handler": "github", "binding": "upgrader", "active_policy": "queue",
         "config": {"claim": "pull_requests", "authors": ["dependabot[bot]", "renovate[bot]"]}},
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "upgrader",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "resume": true, "workspace": true},
          "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.pr.view",
                         "github.pr.comment", "github.checks", "github.wait"],
          "secret_names": [],
          "config": {"playbook": "upgrade-pr", "plugin_dir": "/path/to/upgrades/plugin", "branch": "agent/upgrade-{number}"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "work"}}
        },
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

To take only one bot's PRs, list only its login. To review your team's PRs with a different agent, add another `pull_requests` route without `authors` after this one ([a review bot for every PR](../../guides/github-bot/#a-review-bot-for-every-pr)).

## The playbook

Write the playbook as `skills/upgrade-pr/SKILL.md` in the binding's `plugin_dir`. A turn starts as `/open-conduit:upgrade-pr <number>`, with the PR's number. Each repository can add its own notes in `.open-conduit/playbooks/upgrade-pr.md`, such as which commands to run.

```md
---
name: upgrade-pr
description: Get a dependency upgrade PR green, fixing what the upgrade breaks on the PR's own branch, then report.
---

# Get an upgrade PR green

You own one dependency upgrade PR opened by Dependabot or Renovate. Your worktree starts from the base branch.

1. Read the PR with `github.context` and `github.pr.view` (its `branch` is the head), and its checks with `github.checks`.
2. If the checks are still running, wait for them: `github.wait` with a `reason`, then end the turn.
3. If they passed, comment on the PR with `github.pr.comment`: the upgrade looks safe to merge, and anything in the
   changelog worth knowing. End with `review`.
4. If they failed, bring the PR's branch into your worktree (`git fetch origin <branch>`, then
   `git reset --hard FETCH_HEAD`). Read the failing check's log, find what the upgrade broke, and fix it with the
   smallest change that keeps the upgrade. Run the project's checks locally, commit, and push to the PR's branch with
   `git push origin HEAD:<branch>`. Then `github.wait` and end the turn; the checks finishing wakes you.
5. If the fix needs more than a small change (an API redesign, a major version with a migration guide), don't push it.
   Comment on the PR with what breaks and what fixing it would take, and end with `review`.
6. When you are woken by a push, a comment or the checks, go back to step 1. When the PR merges or closes, end with
   `finished`.
```

## How it goes

1. Dependabot opens "Bump eslint from 8.57.0 to 9.0.0". The `upgrades` route claims it.
2. The agent reads the PR. The lint check failed: ESLint 9 needs a flat config. It writes `eslint.config.js` from the old `.eslintrc`, runs the lint locally, and pushes to Dependabot's branch.
3. The checks pass on the new head, which wakes the agent. It comments that the PR is green, with what it changed and why. You merge it.

A Renovate PR that passes first time gets only the comment.

## Limits

- **Dependabot stops rebasing a PR once someone else pushes to it.** After the agent's push, Dependabot no longer rebases that PR on its own. Renovate likewise stops updating a branch someone else has changed. If the base branch moves on, close the PR and let the bot open a fresh one.
- **Checks on bot PRs may not see your secrets.** GitHub runs Dependabot PRs' Actions with Dependabot's secrets, not the repository's. A check that fails for want of a secret isn't the agent's to fix; the playbook should say so on the PR.
- **Several PRs run side by side.** Each PR is its own Work in its own worktree. Use the scope's `limits.concurrency` to cap how many turns run at once.
