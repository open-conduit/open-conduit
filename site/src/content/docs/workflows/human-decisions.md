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
         "secret_names": [], "config": {"playbook": "-"}}
      ]
    }
  ]
}
```

In practice the `human` entry sits in the same scope as your GitHub or Slack agent, and `human.ask` goes in that agent's binding.

## The entry's config

| Field | Default | Meaning |
| --- | --- | --- |
| `responders` | `["operator:local"]` | Who may answer, each `kind:id`: `operator:local` is you at the TUI or CLI, `github:<login>` is a GitHub user answering in a comment (see [Answering from GitHub](#answering-from-github)), and `slack:<user id>` is a Slack user pressing a button (see [Answering from Slack](#answering-from-slack)). A bare kind, such as `github`, is anyone of that kind. An answer from anyone else is refused. |
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
| `operations` | none | Operation names, such as `github.pr.create`, or a prefix ending in `.*` for every operation under it, such as `deploy.*`. Any extension's operations can be gated, installed ones included, except `human`'s own. The builtins (`emit`, `create_work`, `cancel_work`, `wait`, `declare_outcome`, `read_context`) can't be gated, and naming one is an error. |
| `kinds` | none | Work kinds, such as `github.issue`, or a prefix ending in `.*`, such as `github.*`. Work of a named kind waits for a person's approval before it first starts. See [Approving a start](#approving-a-start). |
| `bindings` | none | Binding names. Work whose first attempt would run on a named binding waits for approval before it starts. |
| `responders` | the entry's | Who may approve or refuse. |
| `timeout` | `24h` | How long a request waits for an answer, from `1m` to `8760h`. With no answer by then, the call is refused, unless the gate escalates. An approval also holds only this long. |
| `escalate` | none | Who an unanswered request goes to next, rather than being refused at `timeout`: `{"responders": [...], "timeout": "24h"}`. See [Escalation](#escalation). |

The binding doesn't need `human.ask` for its calls to be gated: a gate applies to every call in the scope, whatever the binding grants.

When a gated call comes in:

1. The operation does not run. Open Conduit records an approval request and parks the Work, the same way `human.ask` does. The request shows on the Work as **Approval needed**, with the operation, its input and the options **approve** and **refuse**. Answer it from the TUI, the web view or the CLI, as for a question.
2. The answer resumes the caller. It is told whether it was approved, and makes the same call again.
3. An approval lets that exact call (the same operation with the same input, in the same Work) through once, if it is made within the gate's `timeout` of the answer. A different input asks again, and so does the same call made a second time or after that window.
4. A refusal returns `approval_refused` to the caller, naming who refused; no answer by the timeout returns `approval_timed_out`. Either way the operation never runs, and the caller keeps its toolbelt for anything else.

Each request and its answer are in the Work's run history: the answer Event and the alias that woke the Work, linked to the Work, and the toolbelt telemetry records the call as gated (`toolbelt.gate`) or refused by a gate (`toolbelt.result` `gate_refused`). Work that has no conversation to resume (no correlation key) can't wait for an answer, so a gated call there is refused with `approval_unavailable`.

## Answering from GitHub

When the Work belongs to a GitHub issue or PR, a person can answer a question or an approval request in a comment or review on it. The comment's first line is `/answer` followed by the answer: `/answer approve`, `/answer refuse`, or one of the question's options or its number. Anything after the first line is ignored, so you can give your reasons below it.

The GitHub extension hands the comment to Open Conduit as its author, `github:<login>`, and the `human` entry checks that login against the responders as it would anyone else's. List the logins you trust:

```json
{"operations": ["deploy.*"], "responders": ["github:octocat", "operator:local"]}
```

A taken answer gets a 👍 reaction on the comment, and a refused one (a login that isn't a responder, an answer that isn't an option, a question already answered) gets 😕. The extension can only vouch for GitHub logins: a comment can never answer as `operator:local` or as anyone on another surface. Editing a comment answers nothing.

## Answering from Slack

When the Work is a Slack conversation, the question (with its context) or the approval request (with the call and its input) is posted once in its thread, with a button per option, such as **approve** and **refuse**. Pressing one answers as `slack:<your user id>`, which the `human` entry checks against the responders, and Slack shows you, and only you, whether it was taken. List the Slack user IDs you trust (a member ID such as `U0123ABCD`, from a profile's **Copy member ID**):

```json
{"operations": ["deploy.*"], "responders": ["slack:U0123ABCD", "operator:local"]}
```

A question that takes free text, with no options, has no buttons; answer it from the TUI, the web view or the CLI. The buttons work only with Socket Mode, the default receiver, and the Slack app needs **Interactivity** turned on (apps created by `init slack` from now on have it).

## Escalation

A gate with `escalate` doesn't refuse a request nobody answered by its `timeout`. It hands the request on:

```json
{"operations": ["deploy.*"], "responders": ["slack:U0123ABCD"], "timeout": "2h",
 "escalate": {"responders": ["slack:U0456EFGH", "operator:local"], "timeout": "4h"}}
```

Here, nobody approving a deploy within 2 hours escalates it. For the next 4 hours, the escalation's responders can answer it as well as the gate's own. The call is refused with `approval_timed_out` only if that time passes too. The caller stays parked throughout, so escalating costs no agent turn. `escalate.responders` is required, and `escalate.timeout` defaults to `24h`. The two timeouts together may be at most `8760h`.

Once it has escalated, the request is offered as **Approve or refuse … (escalated)**. Its text says nobody answered in time, who it now goes to and when it will be refused. Because the offer has changed, Slack posts it in the thread again. The panel lists the wider responders and the time it escalated. The escalation is recorded on the request and as a `human_gate_escalated` telemetry event, which carries the Work, the execution, the operation and who it went to.

## Approving a start

A gate can also name Work, rather than an operation: Work of a kind (`kinds`) or Work that would run on a binding (`bindings`) waits for a person's approval before its first attempt starts. A gate needs at least one of `operations`, `kinds` or `bindings`.

```json
{"kinds": ["github.issue"], "responders": ["github:octocat", "slack:U0123ABCD"], "timeout": "4h"}
```

When such Work would first start:

1. It doesn't start. Open Conduit holds it and records an approval request on it, shown as **Approval needed**: what would start (the Work, its title and links, its binding and what triggered it) and the options **approve** and **refuse**. Its input keeps queueing while it is held.
2. Answer it as you would any approval: from the TUI, the web view, the CLI, a GitHub `/answer` comment or a Slack button.
3. An approval lets the Work start, once. Its later turns aren't asked again. A refusal, or no answer by the timeout, cancels the Work, with the reason in its history. A gate with `escalate` escalates first, as above.

The hold and its settlement are in the Work's history (`core.gate.held`, then `core.gate.released` or `core.gate.refused`). An operator can't release the hold with `open-conduit work release`, because the gate's responders decide. They can cancel the Work. Only the first start is gated: Work that has already run once starts its later turns without asking.
