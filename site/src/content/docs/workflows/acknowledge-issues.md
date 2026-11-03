---
title: Acknowledge outside issues
description: Thank whoever opens an issue from outside the project with a short comment, without an agent claiming the issue.
---

When someone outside the project opens an issue, post a short comment so they know it was seen: "Thanks for raising this issue, a maintainer will look at it soon." Nobody's agent picks the issue up. A maintainer can still claim it later, by hand or by widening `admit`.

It is [acknowledgement Work](../../architecture/github-extension/#acknowledgement-work). The `github` entry observes each new issue's opening a second time under a key of its own, and an `acknowledgements` route claims that copy as its own Work when `admit.authors` doesn't admit the issue's author. The Work never owns the issue, so the issue gets no `agent-in-progress` or status labels from it, and a later claim isn't blocked.

## Example config

The `github` entry sets `acknowledge_issues`. The `issues` route claims issues from the people `admit` trusts (the repository's owner, members and collaborators by default) and starts the `agent` binding. The `acks` route takes every other new issue and runs the `thanks` shell binding, which comments once and exits. Exit status 0 completes the Work.

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
        "operations": ["@github.author"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "web", "type": "github", "connection": "github", "secret_names": [],
         "config": {"namespace": "github/github.com/acme/web", "scope": "web", "repo": "acme/web", "acknowledge_issues": true}}
      ],
      "routes": [
        {"name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject", "config": {}},
        {"name": "acks", "handler": "github", "binding": "thanks", "active_policy": "queue",
         "config": {"claim": "acknowledgements"}}
      ],
      "bindings": [
        {
          "name": "thanks",
          "executor": "shell",
          "requires": {"toolbelt": "cli"},
          "operations": ["github.issue.comment"],
          "secret_names": [],
          "config": {"command": "os call github.issue.comment --data '{\"body\": \"Thanks for raising this issue, a maintainer will look at it soon.\"}'"}
        },
        {
          "name": "agent",
          "executor": "claude-code",
          "connection": "github",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": true},
          "operations": ["@github.author"],
          "secret_names": [],
          "config": {"playbook": "work-issue", "branch": "agent/issue-{number}"},
          "resource": {"provider": "git", "config": {"checkout": "/path/to/web", "mode": "work"}}
        }
      ]
    }
  ]
}
```

The script needs to know nothing about keys or issue numbers: `github.issue.comment` from acknowledgement Work comments on the issue it acknowledges. The binding is granted only that operation, so the script can do nothing else. Swap the command for a script of your own to vary the message, say by reading the issue first with `os call github.issue.view` (grant that operation too).

## What gets acknowledged

- **Acknowledged:** a new, open issue whose author's association isn't in `admit.authors`, such as `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR` or `NONE` with the default `admit`.
- **Skipped by the route:** an issue by someone `admit` trusts, which the `issues` route claims instead, and an issue by any bot, including this connection's own follow-ups. `open-conduit route '#12'` shows the `acks` route's verdict on issue 12 and its reason.
- **Never observed:** an issue opened more than 72 hours before Open Conduit saw it, so turning this on doesn't thank the authors of old issues, and, until a later read succeeds, an issue whose author's repository permission couldn't be read, so a maintainer isn't mistaken for an outsider. Nothing reaches the route for these, so `route` shows no `acks` verdict for them.
- **Once per issue.** A webhook redelivery, a reconcile or an edit doesn't comment again, and neither does closing and reopening the issue.

The comment carries the acknowledgement Work's marker, and run history links the issue's Event, the acknowledgement Work and its Execution to it.

## Claiming an acknowledged issue

The acknowledgement doesn't own the issue, so a maintainer can still hand it to an agent: claim it with `open-conduit claim <number>` or the TUI's `c` key, or add the author's association to `admit.authors` before they open their next one.
