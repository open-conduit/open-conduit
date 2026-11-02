---
title: Slack to PR
description: Someone asks for a change in a Slack thread; the agent files the issue, the issue's agent opens the PR, and the thread hears back when it is up for review and when it merges.
---

Someone asks for a change in a Slack thread. The thread's agent files an issue in the repository the change belongs to and says so in the thread. The issue's agent opens the PR. The thread hears back when the PR is ready for review and again when it merges, which can be days later.

The two agents never talk directly. The Slack agent files the issue and waits on it, and the issue's agent works it like any other issue:

- the [Slack support](../slack/) workflow answers the thread;
- `github.issue.create` with a `project` files the issue in the repository's scope, whose [GitHub engineering](../github/) route claims it;
- `github.wait` with that project's issue parks the thread's agent until the issue's agent reports or the issue closes.

## Example config

The `support` scope answers Slack. Its `authorization` lets it file issues in `web` (`file_scopes`) and wait on them (`read_scopes`), and its binding has the three GitHub operations it needs. The `web` scope is the GitHub workflow as usual. The Slack scope needs no GitHub entry of its own.

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "slack", "type": "slack", "config": {},
     "secret_refs": {"bot_token": "keyring:slack-bot-token", "app_token": "keyring:slack-app-token"}},
    {"id": "github", "type": "github", "config": {}, "secret_refs": {"token": "cmd:gh auth token"}}
  ],
  "scopes": [
    {
      "id": "support",
      "name": "Slack requests",
      "secret_refs": {},
      "authorization": {
        "connections": ["slack"],
        "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply",
                       "github.issue.list", "github.issue.view", "github.issue.create", "github.wait"],
        "read_scopes": ["web"],
        "file_scopes": ["web"]
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
          "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply",
                         "github.issue.list", "github.issue.view", "github.issue.create", "github.wait"],
          "secret_names": [],
          "config": {"playbook": "slack-thread", "args": "{key}"}
        }
      ]
    },
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

## The thread's instructions

`slack-thread` leaves what a thread is for to your instructions. Put these in the `CLAUDE.md` of the thread's folder ([give the agent a folder](../slack/#give-the-agent-a-folder-of-instructions-and-knowledge)), adjusted to your projects:

```md
# Change requests

When someone in the thread asks for a change to the web app, file it rather than discuss it at length.

1. Check `github.issue.list` with `{"project": "web"}` for an open issue that already covers it. If there is one,
   use it instead of filing another.
2. Otherwise file it with `github.issue.create` and `"project": "web"`. Write the ask as an issue: what they want, why,
   and anything they said about how. End the body with a link to this thread.
3. Reply in the thread with the issue's link, and say you will post here when the PR is up and when it ships.
4. Instead of waiting on the thread, wait on the issue: `github.wait` with
   `{"conditions": ["issue:web/<number>:review", "issue:web/<number>:closed"], "until": "<two weeks from now>",
   "reason": "the change asked for in Slack"}`, then end the turn with `done`.
5. When you are woken, read the issue with `github.issue.view` (`project` and `number`).
   - If its agent has reported a PR ready for review, reply in the thread with the PR's link, then wait again on
     `issue:web/<number>:closed` alone.
   - If it closed because its PR merged, reply that the change has merged, with the PR's link. Then wait on the
     thread as usual.
   - If it closed without a merge, say so and why, if the issue says.
   - If the time came first, reply only if something is worth saying, then wait on the issue again.
```

## How it goes

1. In `#web-requests`, someone writes: "Can we get a CSV export on the orders page?"
2. The thread's agent finds no open issue for it, files "Add a CSV export to the orders page" in `acme/web`, replies with the link, and parks on the issue.
3. The `web` scope's `issues` route claims the new issue. Its agent opens a PR, the checks pass, and it reports. That turn ends in `review`, which wakes the thread's agent: it posts the PR link in the thread and parks on the issue again.
4. Someone merges the PR, which closes the issue. The thread's agent wakes, replies that the export has shipped, and goes back to waiting on the thread.

## Limits

- **A thread message doesn't wake it early.** While the thread's agent is parked on the issue, a new message in the thread is kept and handed to it with the next wake (the PR, the merge or the time). Keep `until` short enough that a question isn't left for long.
- **It waits on the issue, not on a PR.** The PR's number isn't known when the issue is filed, and the issue closes when its PR merges. If the issue's agent opens several PRs and the issue stays open after the first merges, the thread hears only when the issue closes.
- **Filing needs explicit grants.** Without `web` in `file_scopes`, `github.issue.create` is refused and the refusal names the grant to add. Without it in `read_scopes`, the wait is refused.
