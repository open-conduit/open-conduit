---
title: Fix red CI on the default branch
description: When a check suite on main finishes failing, file or update one issue about it, and let the issue's agent open the fix PR.
---

When the checks on your default branch fail, Open Conduit files an issue saying so, and the agent that works your issues picks it up, finds the cause and opens a fix PR. While main stays red, each further failing commit is added to that one issue rather than opening another. Once the fix merges and closes the issue, the next failure on main files a fresh one.

It is built from three pieces you may already have:

- a [scheduled job](../scheduled-jobs/) that runs when a check suite on `main` finishes with a failing conclusion;
- an agent turn for that job, which files or updates the issue;
- your [GitHub engineering](../github/) route, which claims the issue like any other.

## What you need

- **Webhooks.** The job runs on GitHub's `check_suite` deliveries, so the daemon needs its webhook listener (or tunnel). The webhook Open Conduit manages already subscribes to check suites. Run reconcile-only and the job never runs.
- **A GitHub scope for the repository**, as in [GitHub engineering](../github/).

## Example config

The `red-main` job runs when a check suite on `main` completes with `failure` or `timed_out`. Add `cancelled` or `action_required` to `conclusions` if those should count as red too. Its route comes before the `issues` route and starts the `red-main` binding: an agent with no checkout, allowed only to list, read, file and comment on issues.

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
        "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view", "github.issue.list",
                       "github.issue.create", "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web"}},
        {"name": "jobs", "type": "schedule", "secret_names": [],
         "config": {"namespace": "schedule/web", "scope": "web", "jobs": [
           {"name": "red-main", "on": {"events": ["check_suite.completed"], "branches": ["main"], "conclusions": ["failure", "timed_out"]}}
         ]}}
      ],
      "routes": [
        {"name": "red-main", "handler": "schedule", "binding": "red-main", "active_policy": "queue", "config": {"jobs": ["red-main"]}},
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "red-main",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "workspace": false},
          "operations": ["read_context", "declare_outcome", "github.issue.list", "github.issue.view",
                         "github.issue.create", "github.issue.comment"],
          "secret_names": [],
          "config": {"playbook": "red-main", "plugin_dir": "/path/to/red-main/plugin"}
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

## The job's playbook

Write the playbook as `skills/red-main/SKILL.md` in the binding's `plugin_dir`. Each run starts as `/open-conduit:red-main`, followed by what started it: the job, the event, the branch, the commit and the conclusion.

```md
---
name: red-main
description: File or update the one issue about main being red, when a check suite on main has failed.
---

# Main is red

A check suite on main has just finished failing. The message names the commit and how the suite ended.

1. List the open issues with `github.issue.list` and look for one titled "Main is red".
2. If there is one, read it with `github.issue.view`. Unless it already names this commit, comment on it with
   `github.issue.comment` (its `number`): the commit, the conclusion, and that main is still red. Don't file another.
3. Otherwise file one with `github.issue.create`, titled "Main is red". In the body, give the commit and conclusion,
   and ask for the failing check to be found and reproduced at that commit, then fixed in a PR that closes this issue.
   If the failure is flaky rather than a real break, say so on the issue and close nothing.
4. End with `done`. You only file and update; the issue's own agent does the fix.
```

## How it goes

1. A push lands on `main` and its checks fail. GitHub sends a `check_suite` delivery with conclusion `failure`.
2. The job's run reads it, finds no open "Main is red" issue, and files one.
3. The `issues` route claims that issue. Its agent works it in its own worktree as usual: it reproduces the failure at the commit, pushes a fix branch and opens a PR that closes the issue.
4. While the fix is under way, another failing push on `main` runs the job again. It finds the open issue and comments the new commit on it, which reaches the issue's agent as another agent's comment.
5. The fix PR merges and closes the issue, and its agent finishes. If `main` is still red after the merge, the next failing suite files a new issue.

Runs coalesce: several failing suites that arrive while a run is going become one more run, not one each ([the one-pending-run rule](../../architecture/schedule-extension/#the-one-pending-run-rule-coalescing-decision)).

## Limits

- **Every check suite counts, required or not.** GitHub sends one suite per app (Actions, a third-party CI), and the delivery doesn't say whether its checks are required. Filter with `conclusions`, and tell the playbook to ignore an app whose failures don't matter.
- **The issue's agent gets the commit, not the log.** It reproduces the failure in its checkout. `github.checks` reads a PR's checks, not a commit's on main.
- **A missed delivery is not replayed.** GitHub deliveries are [one-shot](../../architecture/schedule-extension/#recoverability-of-trigger-families-x04): if the listener was down when the suite finished, that failure runs nothing, though the next one will.
