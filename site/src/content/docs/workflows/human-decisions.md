---
title: Human decisions
description: Let agents ask a person for a private decision, and make named operations wait for a person's approval, answered from the TUI, without anything reaching the issue or thread.
---

An agent working a GitHub issue or a Slack thread sometimes needs a person's decision that shouldn't go on the public record: whether to spend money, which credential to use, whether to go ahead tonight. With a `human` entry in the scope, the agent asks through `human.ask`. Its Work parks, the question appears on the Work in the TUI and the web view, and the first valid answer resumes the agent.

- Add `human.ask` to a binding's operations, and a `human` entry to its scope.
- The question and the answer stay in Open Conduit's store. Nothing about them is posted to the issue or thread the Work came from.
- In the TUI and the web view, a parked Work's page shows a panel with the question, its context and options, who it is for and its deadline, with the answer action beside it. Elsewhere, such as the run list and the CLI, the Work is annotated with the question alone. From the CLI, run `open-conduit actions list --work <id>` and `open-conduit actions run <entry> answer --work <id> --input <answer>`.
- When the agent gives options, the web view shows a button for each; click one to answer. In the TUI and the CLI, answer with an option's number or its text. Any other answer is refused.

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
| `gates` | none | Operations that need a person's approval before they run (see [Approval gates](#approval-gates)). |

## What the agent sends

`human.ask` takes a `question`, and optionally `options` (two to twenty choices), `context` (what the person needs to know) and a `deadline` (an ISO time such as `2026-10-28T09:00Z`). If nobody answers by the deadline, the agent is resumed without an answer and the question closes. The agent never learns where the question went or who answered it. The agent learns how to use `human.ask` from the extension itself: its guidance arrives in the toolbelt's MCP instructions whenever the binding grants the operation, whichever playbook the agent runs.

## What counts as an answer

Only the entry's **answer** action counts, and only while the question is open and the Work is parked on it. The action checks who is answering and validates the input. An Event posted with `open-conduit events emit`, even with the same type and key, doesn't count. If two answers race, the first one accepted wakes the agent, and the record lists the other as a late answer. Once a question is answered, expired or withdrawn, further answers are refused.

## Approval gates

`human.ask` is the agent's choice: it asks when it thinks it should. A gate is yours. Name an operation in a gate and every call of it, by an agent, a shell script or anything else holding a toolbelt token, waits for a person's approval before it runs. Open Conduit checks it where it serves the call, so nothing in a prompt or playbook can get around it.

```json
{"name": "decisions", "type": "human", "secret_names": [],
 "config": {
   "responders": ["operator:local"],
   "gates": [
     {"operations": ["github.pr.create"]},
     {"operations": ["deploy.*"], "responders": ["operator:local"], "timeout": "2h"}
   ]
 }}
```

| Field | Default | Meaning |
| --- | --- | --- |
| `operations` | required | Operation names, such as `github.pr.create`, or a prefix ending in `.*` for every operation under it, such as `deploy.*`. Any extension's operations can be gated, installed ones included, except `human`'s own. |
| `responders` | the entry's | Who may approve or refuse. |
| `timeout` | `24h` | How long a request waits for an answer, from `1m` to `8760h`. With no answer by then, the call is refused. |

The binding doesn't need `human.ask` for its calls to be gated: a gate applies to every call in the scope, whatever the binding grants.

When a gated call comes in:

1. The operation does not run. Open Conduit records an approval request and parks the Work, the same way `human.ask` does. The request shows on the Work as **Approval needed**, with the operation, its input and the options **approve** and **refuse**. Answer it from the TUI, the web view or the CLI, as for a question.
2. The answer resumes the caller. It is told whether it was approved, and makes the same call again.
3. An approval lets that exact call (the same operation with the same input, in the same Work) through once. A different input asks again, and so does the same call made a second time.
4. A refusal returns `approval_refused` to the caller, naming who refused; no answer by the timeout returns `approval_timed_out`. Either way the operation never runs, and the caller keeps its toolbelt for anything else.

Each request and its answer are in the Work's run history: the answer Event and the alias that woke the Work, linked to the Work, and the toolbelt telemetry records the call as gated (`toolbelt.gate`) or refused by a gate (`toolbelt.result` `gate_refused`). Work that has no conversation to resume (no correlation key) can't wait for an answer, so a gated call there is refused with `approval_unavailable`.

Approvals can be answered only where questions can be so far: the TUI, the web view and the CLI, by the entry's responders. Answering from Slack or GitHub, a timeout that escalates to someone else rather than refusing, and gates on route or kind transitions rather than operations are not built yet.
