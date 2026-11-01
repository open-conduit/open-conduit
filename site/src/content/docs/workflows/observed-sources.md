---
title: Observed sources
description: Turn what an agent or a script sees in an outside source into Work, one Work per thread, without an integration for that source.
---

Some sources can only be read with tools Open Conduit doesn't have: a Slack channel read through your claude.ai Slack connector because the workspace won't approve a Slack app, a page a browser script scrapes, an inbox a Python job polls. An executor that can read the source reports what it sees with `observe.record`, and Open Conduit turns it into Work:

- **The observer** reads the source on a schedule and records every item it sees, with the source's own ID for the item and its thread. It doesn't keep track of what it has already seen, and it doesn't answer anything.
- **Open Conduit** keeps one Event per item, however many runs record it, and gives each thread its own Work.
- **The responder** works one thread. It parks after each turn and wakes when the thread's next item is recorded.

The observer and the responder are separate bindings, and the source's identity belongs to neither. You can replace an agent observer with a script, or run both, and items and threads carry on as before.

## Example config

The source `slack-help` is read every 15 minutes by an agent with your Slack connector (`watcher`). Each thread goes to `responder`.

```json
{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [
    {
      "id": "help",
      "name": "Help channel",
      "secret_refs": {},
      "authorization": {"connections": [], "operations": ["read_context", "wait", "declare_outcome", "observe.record"], "read_scopes": []},
      "extensions": [
        {"name": "watch", "type": "schedule", "secret_names": [],
         "config": {"namespace": "schedule/help", "scope": "help", "jobs": [{"name": "read-help", "every_minutes": 15}]}},
        {"name": "help", "type": "observe", "secret_names": [],
         "config": {"sources": [{"name": "slack-help", "reporters": ["watcher"]}]}}
      ],
      "routes": [
        {"name": "read-help", "handler": "schedule", "binding": "watcher", "active_policy": "queue",
         "config": {"jobs": ["read-help"]}},
        {"name": "threads", "handler": "observe", "binding": "responder", "active_policy": "queue",
         "config": {"source": "slack-help"}}
      ],
      "bindings": [
        {"name": "watcher", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
         "operations": ["read_context", "observe.record"], "secret_names": [],
         "config": {"playbook": "-", "structured": false, "dir": "/path/to/help-watcher"}},
        {"name": "responder", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
         "operations": ["read_context", "wait", "declare_outcome"], "secret_names": [],
         "config": {"playbook": "-", "structured": false, "dir": "/path/to/help-responder"}}
      ]
    }
  ]
}
```

- The `observe` entry declares each source and the bindings that may report for it. A binding not listed is refused, even when its operations include `observe.record`.
- An `observe` route takes one source's threads. A thread's first item starts Work on the route's binding, and each later item is given to that Work.
- Both bindings run in a folder of their own (`dir`), whose `CLAUDE.md` holds their instructions. Claude Code loads your claude.ai connectors unless the binding sets `strict_mcp`.

## The watcher's instructions

A starting `CLAUDE.md` for `/path/to/help-watcher`:

```md
Read the #help channel with the Slack connector: every message from the last two hours, and every reply in those threads.

For each message, call `observe.record` with:
- `item`: the message's `ts`
- `thread`: its `thread_ts`, or its own `ts` if it starts a thread
- `text`: the message text
- `revision`: the message's `edited.ts`, only if it was edited

Skip any message that starts with `[agent]`: those are replies posted on your behalf.

Don't answer anything and don't keep notes of what you've recorded. Recording a message again is safe.
```

The operation's own guidance reaches the agent through the toolbelt, so the instructions only have to say where to read and how the source names things.

## The responder's instructions

A starting `CLAUDE.md` for `/path/to/help-responder`:

```md
You handle one thread in the #help Slack channel. Your input is the thread's newest messages; each gives the thread's `ts`.

1. Read the whole thread with the Slack connector and reply in it. Start every reply with `[agent]`.
2. If the thread may need you again, call `wait` with `{"match": {"types": ["observe.item.recorded"]}, "timeout_seconds": 604800}` and end your turn. You'll be woken with the next message in the thread.
3. If it's resolved, call `declare_outcome` with `completed`.
```

## Replace the watcher with a script

A script can report for the same source. Give it a `shell` binding with `observe.record`, add it to the source's `reporters`, and point the schedule route at it. A shell binding gets the `os` CLI on its `PATH`:

```sh
os call observe.record '{"item": "1730451600.000100", "thread": "1730451600.000100", "text": "How do I reset my VPN password?"}'
```

Items the agent already recorded come back as `"duplicate": true`, and new items in an existing thread reach the Work the agent's items started. You can list both bindings as reporters while you switch over.

## What to expect

- **Follow-ups arrive on the next read.** A reply in a thread reaches its Work when the watcher next runs, up to 15 minutes later here. The [Slack app](../slack/) picks messages up at once.
- **A finished thread stays finished.** Once a thread's Work has completed, later items in that thread start nothing. The responder should wait rather than complete while the thread may continue; the wait above lasts seven days.
- **Edits need a revision.** Recording an item again with different text and no new `revision` is refused, so an observer that can see edits must pass the source's edit mark. An edit back to earlier text is still a new revision.
- **Deletions aren't reported.** An observer rereading a source can't tell a deleted item from one it didn't see.
- **Replies come back as items.** A reply posted through your connector is a message like any other, so the next read would record it and wake the thread. The `[agent]` prefix and the watcher's rule to skip it stop that loop.

See [the observe extension](../../architecture/observe-extension/) for how items, sources and reporters are kept apart.
