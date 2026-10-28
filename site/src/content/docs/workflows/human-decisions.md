---
title: Human decisions
description: Let agents ask a person for a private decision, answered from the TUI, without anything reaching the issue or thread.
---

An agent working a GitHub issue or a Slack thread sometimes needs a person's decision that shouldn't go on the public record: whether to spend money, which credential to use, whether to go ahead tonight. With a `human` entry in the scope, the agent asks through `human.ask`. Its Work parks, the question appears on the Work in the TUI and the web view, and the first valid answer resumes the agent.

- Add `human.ask` to a binding's operations, and a `human` entry to its scope.
- The question and the answer stay in Open Conduit's store. Nothing about them is posted to the issue or thread the Work came from.
- In the TUI and the web view, a parked Work's page shows a panel with the question, its context and options, who it is for and its deadline, with the answer action beside it. Elsewhere, such as the run list and the CLI, the Work is annotated with the question alone. From the CLI, run `open-conduit actions list --work <id>` and `open-conduit actions run <entry> answer --work <id> --input <answer>`.
- When the agent gives options, answer with an option's number or its text. Any other answer is refused.

## Example config

```json
{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [
    {
      "id": "eng",
      "name": "Engineering",
      "secret_refs": {},
      "authorization": {
        "connections": [],
        "operations": ["read_context", "declare_outcome", "human.ask"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "decisions", "type": "human", "secret_names": [],
         "config": {"responders": ["operator:local"], "surfaces": ["tui"]}}
      ],
      "routes": [],
      "bindings": [
        {"name": "agent", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
         "operations": ["read_context", "declare_outcome", "human.ask"],
         "secret_names": [], "config": {}}
      ]
    }
  ]
}
```

In practice the `human` entry sits in the same scope as your GitHub or Slack agent, and `human.ask` goes in that agent's binding.

## The entry's config

| Field | Default | Meaning |
| --- | --- | --- |
| `responders` | `["operator:local"]` | Who may answer: `operator:local` is you at the TUI or CLI. An answer from anyone else is refused. |
| `surfaces` | `["tui"]` | Where the question is shown. The TUI is the only surface so far. |

## What the agent sends

`human.ask` takes a `question`, and optionally `options` (two to twenty choices), `context` (what the person needs to know) and a `deadline` (an ISO time such as `2026-10-28T09:00Z`). If nobody answers by the deadline, the agent is resumed without an answer and the question closes. The agent never learns where the question went or who answered it.

## What counts as an answer

Only the entry's **answer** action counts, and only while the question is open and the Work is parked on it. The action checks who is answering and validates the input. An Event posted with `open-conduit events emit`, even with the same type and key, doesn't count. If two answers race, the first one accepted wakes the agent, and the record lists the other as a late answer. Once a question is answered, expired or withdrawn, further answers are refused.
