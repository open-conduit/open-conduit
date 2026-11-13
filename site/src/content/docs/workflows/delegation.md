---
title: Agents asking agents
description: Let one agent hand a task to another as child Work, park, and be resumed with the other agent's answer, with the operator deciding who may ask whom.
---

A front agent in a Slack thread answers simple questions itself. For the rest it says "I've asked the engineering agent, I'll let you know what it says", hands the task over and ends its turn. When the engineering agent answers, the front agent is resumed with the answer and posts it. The engineering agent works the task as its own Work, with its own history, cost and retries, and the hand-off survives restarts: nothing waits in a process while it works.

- The operator decides in config which kinds of task exist, which binding does each, what each is for, and which bindings may ask for which kinds. The host refuses any other `create_work`, whatever an agent's prompt says.
- The asking agent's toolbelt guidance lists the kinds it may ask for, each with its description, and how to hand a task over and wait for the answer.
- The answering agent's guidance says its turn's `result` is what the asker receives. If it completes without one, its attempt fails and is tried again, so the asker never gets an empty success.

## Example scope

```json
{
  "id": "support",
  "name": "Support",
  "authorization": {
    "connections": [],
    "operations": ["create_work", "wait", "read_context", "declare_outcome", "emit"],
    "read_scopes": [],
    "work_kinds": ["agent.task.engineer"]
  },
  "bindings": [
    {"name": "front", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
     "operations": ["create_work", "wait", "read_context"], "work_kinds": ["agent.task.engineer"],
     "secret_names": [], "config": {"playbook": "slack-thread"}},
    {"name": "engineer", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
     "operations": ["read_context", "declare_outcome"], "secret_names": [], "config": {"playbook": "-"}}
  ],
  "routes": [
    {"name": "engineer", "handler": "work", "binding": "engineer", "active_policy": "queue",
     "config": {"kinds": [{"kind": "agent.task.engineer", "description": "The engineering agent: code, builds, deploys and incidents."}]}}
  ]
}
```

- `authorization.work_kinds` is the scope's ceiling: the kinds any of its bindings may be granted. A child scope's can only narrow its parent's.
- A binding's `work_kinds` are the kinds its runs may ask for. It needs `create_work` in its operations, and each kind needs an enabled route that binds it: a kind nothing would run is refused when the config is validated.
- A `work` route binds child Work of the kinds its config lists to its binding, and its `description` of each kind is what an asking agent is shown. Only child Work an agent asked for is taken; nothing else reaches the route.

## What the agents do

The asking agent calls `create_work` with a kind it was given and the task as its context, `{"kind": "agent.task.engineer", "context": {"schema": "agent.task.engineer.v1", "data": {"task": "…"}}}`, then `wait`s and ends its turn. The answering agent's first turn starts with the task. It answers with the `result` of its status (or of `core.turn.v1` data it declares) and ends its Work.

The asking agent is resumed with a `core.work.child.ended` Event: which Work ended, its kind, its state (`completed`, `failed` or `cancelled`), its summary and its `result`. A failed or cancelled agent has no result.

- **Waiting on more than the answer.** One `wait` can name several types. `{"match": {"types": ["core.work.child.ended", "slack.message.human"]}}` wakes the front agent on whichever comes first, the answer or a new message in its thread, and its turn shows what woke it. If the message wins, the child carries on, and its answer arrives with a later wake.
- **Several at once.** Each child's end is its own Event, carrying the child's ID. Ask several, wait, and wait again after each answer until all have come back.
- **Loops.** Every hand-off stays on one causal chain, so agents that keep asking each other (A asks B asks A) stop at `causal_max_depth`.

See [child Work](../../architecture/routing-lifecycle/#child-work) for the design.
