---
title: Human decisions
description: Let agents ask a person for a private decision, and make named operations wait for a person's approval, answered from the TUI, without anything reaching the issue or thread, and tell the people who decide wherever they are.
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
| `for_bindings` | none | Binding names. With it, the gate holds its `operations` only when a run on one of these bindings calls them; calls from the scope's other bindings go through without approval. See [Gating one agent](#gating-one-agent). |
| `input` | none | Top-level fields of the call's input, each with the JSON value it must have, such as `{"role": "write"}`. With it, the gate holds its `operations` only when the input has every one of those fields with exactly that value; any other call goes through without approval. See [Gating some inputs](#gating-some-inputs). |
| `bindings` | none | Binding names. Work whose first attempt would run on a named binding waits for approval before it starts. |
| `responders` | the entry's | Who may approve or refuse. |
| `timeout` | `24h` | How long a request waits for an answer, from `1m` to `8760h`. With no answer by then, the call is refused, unless the gate escalates. An approval also holds only this long. |
| `valid_for` | none | A duration from `1m` to `24h`. With it, an approval is a grant: once the approved call goes through, the same call (the same operation and input) from the same Work passes without asking until `valid_for` has passed. See [Approving for a while](#approving-for-a-while). |
| `escalate` | none | Who an unanswered request goes to next, rather than being refused at `timeout`: `{"responders": [...], "timeout": "24h"}`. See [Escalation](#escalation). |
| `label` | the operation's own | What the call does, in the words the person asked reads: a phrase that follows "wants to", with `{field}` for a top-level value of the call's input, such as `"Sign in to git for {path}"`. See [How a request reads](#how-a-request-reads). |
| `fields` | every top-level value | Which of the call's input fields the person is shown, and under what name: `{"path": "Repository", "host": ""}` shows `path` as **Repository** and `host` under its own name, and nothing else. |
| `confirm` | none | `approve` or `refuse`: that choice asks "Are you sure?" before it is sent, for a call where a slip would cost. |

The binding doesn't need `human.ask` for its calls to be gated: a gate applies to every call in the scope, whatever the binding grants, unless `for_bindings` narrows it.

When a gated call comes in:

1. The operation does not run. Open Conduit records an approval request and parks the Work, the same way `human.ask` does. The request shows on the Work as **Approval needed**, with the operation, its input and the options **approve** and **refuse**. Answer it from the TUI, the web view or the CLI, as for a question.
2. The answer resumes the caller. It is told whether it was approved, and makes the same call again. When the call was one the agent's tools made for it rather than its own (git's sign-in, `github.git.credential`, which the host calls when git opens a session), it is told in those terms: "A person approved git's sign-in to github.com/acme/app. Run the same git command again: it goes through once." The toolbelt refuses any call of such an operation from the run (through `call`, `os call` or a request naming the capability), so the agent isn't sent there and never gets its result.
3. An approval lets that exact call (the same operation with the same input, in the same Work) through once, if it is made within the gate's `timeout` of the answer. A different input asks again, and so does the same call made a second time or after that window, unless the gate has `valid_for` ([approving for a while](#approving-for-a-while)).
4. A refusal returns `approval_refused` to the caller, naming who refused; no answer by the timeout returns `approval_timed_out`. Either way the operation never runs, and the caller keeps its toolbelt for anything else.

Each request and its answer are in the Work's run history: the answer Event and the alias that woke the Work, linked to the Work, and the toolbelt telemetry records the call as gated (`toolbelt.gate`) or refused by a gate (`toolbelt.result` `gate_refused`). Any Work can wait for an answer: Work that has no correlation key of its own (made with `POST /v1/work` or `create_work` and no keys) is given its own key, `core.work`/`<its ID>`, when it first waits, and the answer arrives on that.

### Gating one agent

Agents that hand work to one another share a scope, because `create_work` makes child Work in the caller's scope. To gate one of them and not the others, add `for_bindings` naming its bindings:

```json
{"name": "decisions", "type": "human", "secret_names": [],
 "config": {
   "gates": [
     {"operations": ["github.pr.create", "slack.post"], "for_bindings": ["newbie", "newbie--job"], "responders": ["operator:alice"], "timeout": "24h"}
   ]
 }}
```

Here a run on `newbie` or `newbie--job` waits for `operator:alice` before it opens a PR or posts to Slack, and every other binding in the scope does both without asking. Open Conduit reads the binding from the calling run as it stored it, never from anything in the call. Each name must be a binding of every scope the entry applies to: `open-conduit config check` refuses one that isn't, with `missing_binding`, so a typo can't leave the agent ungated. `bindings` is checked the same way.

When more than one gate holds a call (an agent's own gate, and one for every binding), the call is asked about once. Anyone either gate names may answer it, and the first of those gates in the list sets the `timeout` and the `escalate`. A responder of a gate that doesn't hold the call, such as another agent's gate, can't answer it.

### Gating some inputs

One operation can do both a harmless and a risky thing, depending on its input. `aws.credentials`, say, gives a run credentials for whichever role it names. To ask a person only for the risky input, add `input`:

```json
{"operations": ["aws.credentials"], "input": {"role": "write"}, "responders": ["operator:local"]}
```

Here a run asks for approval before it gets credentials for the `write` role, and gets the `read` role's without asking. The gate compares each field's JSON value exactly (`"write"` doesn't match `["write"]`), and a call whose input lacks the field isn't held. It fails closed on how an operation might read its input: a key that names the field in another case (`Role`), or a repeated key, is held when any of them has the value, and an input that isn't a JSON object is held. Operations written on `protocol/sdk` refuse such keys anyway. To hold several values, add a gate for each. `input` can be combined with `for_bindings`, and both must hold for the gate to hold a call.

### Approving for a while

Some calls are made again and again for one piece of work: a tool that fetches credentials once per process, such as the AWS CLI with a gated `aws` role, calls once for every command. What a person decides there is "this run may do this now", not "this one process may". Give the gate `valid_for`:

```json
{"operations": ["aws.credentials"], "input": {"role": "write"}, "valid_for": "15m", "responders": ["operator:local"]}
```

Once the approved call goes through, the same call from the same Work passes without asking for the next 15 minutes. The grant starts when the approved call is made, not when the person answered. It covers only that operation with that input, in that Work: another input, such as `{"role": "admin"}`, or another Work asks as usual. Once it ends, the next call asks again. When several gates hold a call, the shortest `valid_for` applies, and none at all if one of them has none. Open Conduit keeps the grant, never the call's result. Each call a grant lets through records the `human_gate_grant_used` telemetry event, naming the approval that covered it and who gave it.

## Answering from GitHub

When the Work belongs to a GitHub issue or PR, a person can answer a question or an approval request in a comment or review on it. The comment's first line is `/answer` followed by the answer: `/answer approve`, `/answer refuse`, or one of the question's options or its number. Anything after the first line is ignored, so you can give your reasons below it.

The GitHub extension hands the comment to Open Conduit as its author, `github:<login>`, and the `human` entry checks that login against the responders as it would anyone else's. List the logins you trust:

```json
{"operations": ["deploy.*"], "responders": ["github:octocat", "operator:local"]}
```

A taken answer gets a 👍 reaction on the comment, and a refused one (a login that isn't a responder, an answer that isn't an option, a question already answered) gets 😕. The extension can only vouch for GitHub logins: a comment can never answer as `operator:local` or as anyone on another surface. Editing a comment answers nothing.

### How a request reads

A request reads as one sentence: "**Steve** wants to sign in to git for acme/backend." What the call does comes from the gate's `label`, else from the operation itself (an extension can give each operation a sentence; GitHub's `github.git.credential` says "sign in to git for {path}"), else "call github.git.credential". The `{field}` names are filled with the call's input; when the input lacks one, the next of those is used. The input's top-level values are shown as named fields (**Host** github.com, **Repository** acme/backend), and the whole input only when a value is nested. The choices read **Approve** and **Refuse**. A question reads as the agent's question.

```json
{"operations": ["github.git.credential"], "responders": ["slack:U0123ABCD"],
 "label": "Sign in to git for {path}", "fields": {"path": "Repository", "host": "Host"}, "confirm": "approve"}
```

Where the request is shown lays it out its own way: Slack as below; the TUI, the web view and the CLI show the request's label and its detail, the operation and its input, as before.

## Answering from Slack

When the Work is a Slack conversation, the question or the approval request is posted once in its thread, [as it reads](#how-a-request-reads): "**Steve** wants to sign in to git for acme/backend. @Dana, do you approve?", the input's values as fields, and a button per option, **Approve** in green and **Refuse** in red. Each Slack person among the responders is mentioned, so Slack notifies them; anyone who may answer from outside Slack is only counted ("2 people outside Slack can answer too."), never shown by an ID. Once it is answered, wherever that happens, the buttons give way to one line: "✅ Approved by @Dana at 07:53". Pressing one answers as `slack:<your user id>`, which the `human` entry checks against the responders, and Slack shows you, and only you, whether it was taken. List the Slack user IDs you trust (a member ID such as `U0123ABCD`, from a profile's **Copy member ID**):

```json
{"operations": ["deploy.*"], "responders": ["slack:U0123ABCD", "operator:local"]}
```

When the Work was handed its task by a Slack conversation (`create_work`), at any depth, turn on `"child_requests": true` in the Slack entry to see its requests in that conversation's thread. Each shows in the thread's progress message as "Steve wants to open a pull request (for Dana)", naming the Slack people it is for by their display names, in progress until it is answered, and marked as an error if it is refused or times out. When a Slack person is among the responders, the request is also posted with its buttons, and a press answers the child's request. When only operators or GitHub users may answer, the thread shows only the state, and they answer where they are. Either way the conversation's agent isn't woken, and it can't answer for anyone.

An approver who isn't in the thread, or Work with no thread at all, such as a scheduled job's, needs telling. Turn on `"request_messages": {"enabled": true}` in the Slack entry, and each Slack person a request names (`slack:<member id>`) gets a direct message from the app, laid out as in a thread ("**Steve** wants to sign in to git for acme/backend. Do you approve?"), with a link to the conversation it was asked in when there is one. A press answers as you, as in a thread. Once the request is answered, refused or times out, wherever that happens, the message says so in the same one line and loses its buttons. A bare `slack` responder names nobody, so it is sent nothing. For any other way of telling people, see [Telling people about a request](#telling-people-about-a-request).

A question that takes free text, with no options, has no buttons; answer it from the TUI, the web view or the CLI. The buttons work over Socket Mode, the default receiver, or over HTTP (`receiver: http`, with the app's interactivity Request URL), and the Slack app needs **Interactivity** turned on (apps created by `init slack` have it). An entry with `receiver: none` can't receive a press, so it posts the request without buttons and asks for a reply in the thread instead: a reply that says one of the options answers as you.

## Telling people about a request

A request shows only where someone looks: the TUI, the web view, the CLI, a GitHub issue or a Slack thread. Its responder is often somewhere else, such as an approver who isn't in the conversation that asked, Work a schedule started with no thread at all, or someone who works in another tool. Slack can [message its people directly](#answering-from-slack). For everyone else, Open Conduit records an Event for each change of a request, and your deployment can tell people however it likes:

| Event | When |
| --- | --- |
| `human.request.opened` | A question is asked, or a call or a start needs approval. |
| `human.request.escalated` | Nobody answered by the gate's `timeout`, and the request now goes to its `escalate` responders as well. |
| `human.request.settled` | The request is answered, refused, expired (nobody answered in time) or withdrawn (the asker stopped waiting). |

Every request has `opened` first and `settled` last, each once, with `escalated` between them if it escalates. They are recorded within a few seconds of the change. Each carries the request as it then stood (schema `human.request.v1`):

| Field | Meaning |
| --- | --- |
| `request` | The request's ID. All of a request's Events share the correlation key `human`/`request/<request>`. |
| `work`, `provider`, `action` | The Work that asked, the `human` entry and the action that answers it. To answer, call `POST /v1/work/{work}/actions/{provider}/{action}` with `{"input": "approve"}`, or run `open-conduit actions run <provider> answer --work <work> --input approve`. |
| `execution`, `binding` | The run that asked and its binding (for a start, the binding the Work would start on). |
| `kind` | `question`, `approval` (a gated call) or `start` (a gated start). |
| `operation` | The gated operation, for an approval. |
| `label`, `detail`, `options` | What the person is deciding, as the TUI and Slack show it: the question or the call and its input, and the choices. |
| `responders` | Who may answer it now, each `kind:id` or a bare kind. |
| `asked_at`, `expires_at`, `escalated_at` | When it was asked, when nobody answering ends it, and when it escalated. |
| `state`, `by`, `settled_at` | `open` until it settles; then `answered`, `refused`, `expired` or `withdrawn`, who answered or refused it, and when. |

The Events tell; they don't answer. An answer still goes through the entry's answer action, which checks who is answering, so recording or posting one of these Events gives nobody a way round a gate. They are on the request's own key, so they never wake the Work that asked. Only Open Conduit records them: an agent can't emit them, and a `human` route ignores one posted with `open-conduit events emit`.

To act on them, add a `human` route. It gives each request its own `human.request` Work on the route's binding: `human.request.opened` starts it, and the request's later Events are its next input. The binding does what your deployment needs, such as sending an email or a page, or posting in your own app, through whatever operations you grant it. A shell script is enough:

```json
{"routes": [{"name": "requests", "handler": "human", "binding": "notify", "active_policy": "queue", "config": {}}],
 "bindings": [{"name": "notify", "executor": "shell", "requires": {"toolbelt": "cli"},
   "operations": ["read_context", "declare_outcome"], "secret_names": ["SMTP_PASSWORD"],
   "config": {"command": "./notify-approver \"$OS_INPUT_FILE\" && os outcome handoff --summary told"}}]}
```

The script reads each Event from its input, tells the responders, and hands back, so the Work takes the next Event when it comes. The route takes no config. To read the Events without a route, list them with `GET /v1/events?type=human.request.opened`.

## Escalation

A gate with `escalate` doesn't refuse a request nobody answered by its `timeout`. It hands the request on:

```json
{"operations": ["deploy.*"], "responders": ["slack:U0123ABCD"], "timeout": "2h",
 "escalate": {"responders": ["slack:U0456EFGH", "operator:local"], "timeout": "4h"}}
```

Here, nobody approving a deploy within 2 hours escalates it. For the next 4 hours, the escalation's responders can answer it as well as the gate's own. The call is refused with `approval_timed_out` only if that time passes too. The caller stays parked throughout, so escalating costs no agent turn. `escalate.responders` is required, and `escalate.timeout` defaults to `24h`. The two timeouts together may be at most `8760h`.

Once it has escalated, the request is offered as **Approve or refuse … (escalated)**. Its text says nobody answered in time, who it now goes to and when it will be refused; in Slack, it says more people are asked now and when it will be refused, without their IDs. Because the offer has changed, Slack posts it in the thread again. The panel lists the wider responders and the time it escalated. The escalation is recorded on the request and as a `human_gate_escalated` telemetry event, which carries the Work, the execution, the operation and who it went to.

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
