---
title: "Chat Completions executor"
description: "How the toolbelt-only executor runs a turn as a Chat Completions tool loop against any OpenAI-compatible endpoint, with the granted toolbelt operations as its only tools, and keeps the Work's conversation as private continuity."
---

The Chat Completions executor runs each attempt as a tool loop against an OpenAI-compatible `/chat/completions` endpoint (OpenAI, OpenRouter, Ollama, LiteLLM and others). Its tools are the toolbelt operations the binding is granted, and nothing else. It is for Work that needs judgement and toolbelt operations but no shell or files: triage, replies, summaries, labelling. A run is HTTP requests made from the extension's own process, with no CLI, workspace or container, so a daemon can run many cheaply.

The executor is [`extensions/chatcompletions`](https://github.com/open-conduit/open-conduit/tree/main/extensions/chatcompletions). It reaches the daemon only as a package, [`extensions/chatcompletions/pkg`](https://github.com/open-conduit/open-conduit/tree/main/extensions/chatcompletions/pkg), served by `open-conduit extension-run chat-completions` and bundled with the release like the Codex package. Nothing in the daemon knows about it: a binding names executor `chat-completions`, and the host binds it from the capabilities in the package's manifest. The executor imports only public contracts, `client` and the protocol SDK.

It advertises `interrupt`, `inject`, `resume` and `toolbelt: sdk` (it calls the toolbelt itself), the stop and forget controls, and `playbook`. It needs no `workspace`, and it doesn't advertise `sandbox` or `container`, because it runs no process to confine (see [limitations](#limitations)).

| Capability | How the executor provides it |
| --- | --- |
| run | The tool loop, until the model declares an outcome or calls `wait`. |
| resume | The Work's conversation is private continuity. Every attempt, the wake of a parked one included, carries it on. |
| inject | `Inject` queues the Event. Before its next request the loop folds what has arrived into the conversation. |
| interrupt | Cancels the request in flight. |

## Tools are toolbelt operations

The model gets one function per operation the attempt may call, and nothing else. The executor has no tool system of its own, so authority, secrets, gates and the trail stay at the host: every call is an ordinary toolbelt call, refused or recorded there like any other.

- **From the grant.** At the start of each attempt the executor reads `GET /v1/toolbelt/guidance` with the attempt's token ([execution endpoints](../toolbelt-management/#execution-endpoints)). `granted` names every operation the token is granted, builtins included. `operations` gives each described one's summary and input schema, and `guidance` the extensions' guidance for them.
- **Functions.** An operation's function is its name with `.` and any other character a provider refuses turned into `_` (`github.issue.comment` becomes `github_issue_comment`), with a `_2` suffix in the rare case two collide. Its parameters are the operation's input schema when that is an object, and an empty object otherwise. Its description is the summary.
- **Builtins.** `wait` and `declare_outcome` are offered whenever they are granted. `read_context`, `emit`, `create_work` and `cancel_work` are offered when granted and the binding's `operations` don't narrow them out. They take the wire inputs `os mcp` gives Claude Code. `declare_outcome` also takes an optional `status` (a turn status word, such as `review`, [execution scheduling](../execution-scheduling/#outcomes-handoff-and-retry)) and `result`, which the executor reports in its result rather than sending to the host.
- **Narrowing.** The binding's `operations` (exact names, or `github.*` for a namespace) narrow which granted operations become tools. They grant nothing: the host still checks every call.
- **The call.** An extension's operation is called with the arguments as its input's `data`, and a builtin with the arguments as its wire input. Every call has a request ID derived from the Work, the turn (which never repeats, even on a fresh conversation), the reply's place in the conversation and the call's ID. The host replays a request ID only within one execution ([toolbelt authority](../toolbelt-management/#toolbelt-authority)), so the executor never makes a call again in a later attempt (see [failures and retries](#failures-and-retries)). The model reads an operation's output `data` (a builtin's event, Work, wait or context) as JSON, and a refusal as `error: <the host's message>`.

An older daemon that names no `granted` operations gets the described operations and `read_context`, `wait` and `declare_outcome`.

## A turn

The system message says what the tools are and that a turn ends with `declare_outcome` or `wait`, followed by the guidance of the granted operations. It is written afresh each attempt and isn't stored with the conversation. The first user message is built as Codex's is ([prompt, inputs and injection](../codex-executor/#prompt-inputs-and-injection)), so a playbook reads the same message whichever agent runs it:

| Turn | Message |
| --- | --- |
| A Work's first | The playbook's text (`<plugin dir>/skills/<playbook>/SKILL.md` without its frontmatter), `ARGUMENTS: <args>`, then `## Already waiting` and the rendered inputs |
| A later turn | `ARGUMENTS: <args> events` (`<args> events` without a playbook), then `## Since your last turn` and the inputs, appended to the conversation |
| A later turn on a fresh conversation | The playbook's text, `ARGUMENTS: <args> handoff`, `## Handoff from your last session` with the note, then `## Since your last turn` and the inputs |
| An attempt continuing a turn that hasn't ended | Only the inputs the turn hasn't been shown, as `## Arrived during this turn` |

Inputs use the generic renderer the Codex executor uses. The plugin dir is the binding's `plugin_dir`, or else the host's playbook directory (`ExecutionContext.PlaybookDir`).

The loop then sends the conversation and the tools, appends the reply and runs any tool calls it asks for, in order, appending each result. Then it sends again. A turn ends on the first of these:

| What happened | Outcome |
| --- | --- |
| The model called `declare_outcome` and the host accepted it | The declared kind and summary (the host applies the declaration). With a `status` or `result`, the data is `core.turn.v1`, so the host publishes the turn-ended Event and a child's result reaches its parent. |
| The model called `wait` and the host accepted it | Waiting: the host converts the result and registers the wait. The wake carries the conversation on. |
| A reply with no tool calls, with no toolbelt (nothing to declare with) | Completed, with the reply as its summary |
| A reply with no tool calls, with a toolbelt | The executor reminds the model once to end its turn. A second such reply fails the attempt (`no_outcome`). |
| `max_rounds` requests without an end | Failed (`max_rounds`) |

Calls in the same reply after a `wait` or `declare_outcome` aren't run. Each is answered `not run: your turn ended with …`, so every call in the conversation has its result. A refused `wait` or `declare_outcome` is an error the model reads and the turn goes on.

### Provider quirks

Tool calling differs between providers, and the loop absorbs the differences:

- **Malformed argument JSON** is answered as an error naming the problem, and the model calls again.
- **Missing or repeated tool-call IDs** are replaced with `call_<message>_<index>`, kept in the conversation, so each result answers exactly one call.
- **Arguments as an object** rather than a JSON string, as some servers send them, are read as they are.
- **Parallel calls** run one at a time, in order.
- **Content** may be a string, null or a list of text parts. An assistant message that carries tool calls and no text is sent without `content`.
- **An error in a 200 body** (OpenRouter passes a provider's failure on this way) is classified by its code, as a refused request would be.

## Failures and retries

A request that gets no usable reply ends the attempt as failed, and the loop never sleeps: the runtime decides when to try again. The conversation is saved after every message, so the next attempt carries the open turn on from where it was. Any tool call the last reply asked for that has no result yet is answered as maybe run (`unknown: your last attempt ended while this call was in progress, so it may or may not have taken effect…`), so the model checks before making it again: a later attempt is a new execution, whose calls the host doesn't replay, so making the call again could repeat its effect. A call an interrupt or stop cuts off is left unanswered for this reason rather than saved as failed. The next attempt shows only inputs the turn hasn't seen.

| Reply | Reason | The runtime |
| --- | --- | --- |
| `429`, or an error in a 200 body with code 429 | `rate_limit` | `Result.RetryHint`, with `Retry-After`'s time when the provider sent one. Core schedules the retry at that time plus its margin, or after its backoff without one ([execution scheduling](../execution-scheduling/#outcomes-handoff-and-retry)). |
| `5xx` or `408` with `Retry-After` | `provider_unavailable` | The same retry hint, at that time |
| `5xx` or `408` without one, no connection, no reply within `timeout_seconds`, a reply that isn't a Chat Completions object | `provider_unavailable`, `timeout`, `malformed_reply` | An ordinary failure, retried on the runtime's retry delays |
| A refusal naming the context length | `context_limit` | An ordinary failure. The next turn starts a fresh conversation from the note. |
| `401`, `402`, `403` or `404` | `auth`, `not_found` | `Permanent`: the key or the model is wrong, so no retry is armed |
| Any other `4xx` | `request_refused` | An ordinary failure |
| Interrupt or stop | `interrupted`, `stopped` | As the control decides |
| A bad config, an unresolved key or a header holding a selected secret | `invalid_config` | `Permanent` |

A provider outage that says nothing about when to come back is an ordinary failure rather than a limit: core waits 30 minutes for a limit with no reset time, which is right for a usage limit and wrong for a moment's 503.

## Trail

Each result's data (`open-conduit.chat-completions.result.v1`, or `core.turn.v1` with the same fields when the turn declared a status or result) records:

- `reason`, `detail`, `kind` (`start`, `events`, `handoff` or `continue`) and `handoff_reason`
- `rounds`: each request's model, prompt and completion tokens, cached tokens and, when the provider reports it (OpenRouter's `usage.cost`), its cost
- `model`, `tokens` (summed) and `cost`, which also map to `extension.Usage` (cost in `USD`), so budgets count them
- `tool_calls`, `injected: {accepted, consumed}` and `context_tokens`
- `transcript`, the artifact ID of the conversation as `application/x-ndjson`, system message first
- `status` and `result`, when declared

Every tool call is an ordinary `toolbelt.*` record linked to the Work and the Execution.

## Private continuity

A Work's conversation lives in the executor's state directory (`work/<key>.json`): its messages without the system message, whether a turn is open and which inputs it has shown, the last turn's note (its declared summary, or the wait's description) and the prompt tokens of the last request. Core never reads it.

Context limits follow this policy:

- **Size.** When the last turn's last request carried more than `handoff_tokens` of prompt (default 100000) and there is a note, the next turn starts a fresh conversation from the note, with `handoff_reason: size`. Without a note the conversation goes on.
- **Refusal.** When the endpoint refuses a conversation as longer than the model's context, the attempt fails as `context_limit`, and the next turn starts afresh from the note (`handoff_reason: context`), saying when there is none.
- **Within a turn**, `max_rounds` bounds the growth.

## Injection

`Inject` accepts an Event for an attempt this instance is running, once per Event ID, and refuses any other with `extension.ErrNotAccepting`. Before each request the loop folds what has arrived into the conversation as one `## Arrived during this turn` message after the last reply's tool results. The proof is the provider's answer to the request that carried it: those Events are the result's `ConsumedInputs`. A reply with no tool calls while Events are waiting is not the end: the loop folds them and sends again. An Event that arrives once the turn has ended stays pending and is presented to the next attempt, as the contract requires. The executor passes `conformance.CheckInjectExecutor`.

## Recovery and controls

The attempt's loop lives in the extension's process. Before its first request, `Run` and `Resume` persist `open-conduit.chat-completions.recovery.v1`, naming the execution.

- **Recover** returns the attempt's handle when this instance still runs it, at the same or a newer epoch, and returns a recorded result identically (results are kept under `runs/`). It refuses anything else with `ErrNotAdoptable`: a restarted extension has no loop to adopt, and the runtime's next attempt carries the conversation on.
- **Stop** cancels the attempt at that epoch and returns once its loop has returned. An attempt this instance doesn't hold has nothing running. A newer epoch refuses an older stop.
- **Forget** removes an attempt's recorded result and keeps the Work's conversation.
- **Interrupt** cancels the request in flight. The attempt fails as `interrupted` and its turn stays open.
- **Doctor.** With `check_secrets=true`, the deep `chat-completions-auth` check lists `<base_url>/models` once per binding, with its `api_key_secret` (resolved through `sdk.Host.BindingSecrets`) and `headers`, and reports a safe code by the answer's status alone ([doctor](../observability-history/#doctor)).

## Binding config

The config is strict and validated at load:

| Field | Meaning |
| --- | --- |
| `base_url` | Required. The API root: requests go to `<base_url>/chat/completions` (`https://openrouter.ai/api/v1`, `http://localhost:11434/v1`). `http` or `https`, with a host and no credentials, query or fragment. |
| `model` | Required. The request's model. |
| `api_key_secret` | A secret the binding selects, sent as `Authorization: Bearer`. Unset sends none. |
| `headers` | Added to every request, such as OpenRouter's `HTTP-Referer` and `X-Title`. They may not carry credentials: names that look like an authorization, cookie, key, token, secret, session, password or signature header are refused, and so are the executor's own headers. At run time a header whose value holds a selected secret fails the attempt. |
| `playbook`, `plugin_dir`, `args` | As for Codex. `args` defaults to `{number}`. |
| `operations` | Narrows which granted operations become tools: names, or `name.*` for a namespace. |
| `max_rounds` | The most requests one attempt makes (default 30, at most 500). |
| `timeout_seconds` | One request's time limit (default 300, at most 3600). |
| `handoff_tokens` | See [private continuity](#private-continuity) (default 100000; 0 always carries the conversation on). |
| `request` | Passed through in every request's body (`temperature`, `max_tokens`, provider routing). `model`, `messages`, `tools`, `tool_choice`, `functions`, `function_call`, `stream`, `stream_options` and `n` are the executor's and refused. |

## Verification

- [Unit tests](https://github.com/open-conduit/open-conduit/tree/main/extensions/chatcompletions) run the executor against an `httptest` fake endpoint and a fake toolbelt. They cover:
  - the shared executor and inject conformance cases
  - the tools from the grant, their names, narrowing, the system message and the request's key, headers and pass-through
  - a call's input, its request ID (different on a fresh conversation), and `declare_outcome` with a status and result as `core.turn.v1`
  - malformed arguments, missing IDs, object arguments and calls after a `wait`
  - a 429 with `Retry-After` and the retry carrying the open turn on, and every failure class
  - `wait` and the wake replaying the conversation
  - the reminder and `no_outcome`, the size handoff, interrupt, stop, and a call cut off by an interrupt answered as maybe run, not made again
  - the config's validation
- `open-conduit extension certify` certifies the package (`TestCertificationFirstPartyPackages`). Its fixture command serves a fake endpoint on loopback, so certification never calls a provider.
- [`TestDaemonChatCompletionsExecution`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_chatcompletions_test.go) runs the composed daemon with the package and the notes example installed, against a fake endpoint. The tools are listed from the grant, a call reaches the notes package through the toolbelt, `wait` parks the Work, the wake replays the conversation, a 429 ends that attempt with a retry hint the runtime schedules a retry from, and the retry carries the turn on to its declared outcome.

## Limitations

- **No process sandbox or container.** The executor runs no process: its only network traffic is its requests to `base_url`, made from the extension's process. A binding with `sandbox` or `container` is refused for it.
- **No streaming.** Each request waits for the whole reply, so `interrupt` cancels the request rather than cutting a reply short, and a live view shows nothing until the attempt ends.
- **Reasoning content** some providers return beside a reply is not kept in the conversation.
- **Rendering.** Inputs use the generic renderer, without the reply hints Claude Code gets from `event.render_owned`.
- **One concurrency pool.** Its runs count against `settings.concurrency` like any agent turn's.
