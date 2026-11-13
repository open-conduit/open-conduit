---
title: "Webhook extension"
description: "How a signed HTTP POST becomes Work of its own: hooks, token URLs, delivery keys, correlation keys and routing."
---

The [`webhook` extension](https://github.com/open-conduit/open-conduit/tree/main/extensions/webhook) turns each accepted POST to a signed URL into Work of its own. It is first-party, runs out of process as a package bundled with the release (`open-conduit extension-run webhook`), and is written only on `protocol/sdk`, as a third party would write it. It uses only public hooks: an ingress type with one one-shot family, webhook paths, a route handler (which asks for successors), a Work kind, an Event renderer, one toolbelt operation and two doctor checks.

A [job webhook](../schedule-extension/) is a different kind of POST. It triggers the job's one standing Work, so POSTs that arrive while a run is active or waiting join one batch, and the batch sees only the newest body. That is right for a job, which is one recurring task. Use a webhook entry when each delivery is a task of its own, such as one alert for each failed deploy or one form submission for each request. Each delivery then has its own history, retries and outcome, and deliveries run in parallel up to the binding's capacity.

## Configuration

```json
{"name": "inbox", "type": "webhook", "secret_names": ["hook"], "config": {
  "scope": "ops", "webhook_secret": "hook",
  "hooks": [
    {"name": "tickets", "delivery_key": "header:Idempotency-Key", "correlation_key": "body:/ticket/id"},
    {"name": "forms"}
  ]}}
```

| Field | Meaning |
| --- | --- |
| `scope` | The scope the entry is in. |
| `webhook_secret` | A selected secret name. Each hook's URL token is derived from it, so rotating it revokes every URL of the entry. To revoke one hook's URL, bump that hook's `version`. |
| `hooks[].name` | Lower case letters, digits, `_` and `-`, at most 64. It is in the URL and in the Work's title. |
| `hooks[].delivery_key` | Optional. Where a delivery's identity is read: `header:<name>` or `body:<JSON pointer>`. |
| `hooks[].correlation_key` | Optional. Where a delivery's thread is read, in the same form. Best a value that names something that ends and may start again, such as a ticket or an incident (see [Routing and Work](#routing-and-work)). |
| `hooks[].version` | Optional, an integer, 0 by default. It is signed into the hook's URL token, so bumping it revokes that hook's URL alone, keeping its name, its Work IDs and its threads. After applying the bump, `doctor --check-secrets` prints the new URL to hand to the sender. |
| `hooks[].fan_out` | Optional. Up to 4 targets, named like hooks, that each delivery goes to (see [Fan-out](#fan-out)). |

A key is `header:<name>` (the header's value) or `body:<pointer>` (an RFC 6901 pointer into a JSON body, naming a string, number or boolean). A value is at most 256 bytes.

The route is `{"handler": "webhook", "config": {"hooks": ["deploys"], "targets": ["triage"]}}`. `hooks` limits the route to those hooks; empty takes every hook of the scope's webhook entries. `targets` limits it to those fan-out targets; empty takes every delivery of its hooks, whatever the target. One route per binding gives each hook, or each target, its own command or agent.

## Ingress

The webhook listener serves `POST /webhook/<scope>/<entry>/<hook>/<token>`. The token is an HMAC of the entry's secret over the namespace (`webhook/<scope>/<entry>`), the hook and the hook's version, so it is stable across restarts and distinct for each hook. Version 0 signs only the namespace and the hook, so a hook that never sets a version keeps the URL it had before versions existed. The handler:

1. Answers 404 for an unknown hook or a wrong token, compared in constant time before the body is read.
2. Answers 413 for a body over 1 MiB, or for one the hook's fan-out Events couldn't carry (see [Fan-out](#fan-out)).
3. Reads the hook's keys. A hook with a delivery key refuses a POST without it, or with a value over 256 bytes, with 400. A missing correlation value is not an error: that delivery is a thread of its own.
4. Hands the delivery to the host and answers 202 only after durable acceptance, or 503 when it wasn't accepted (or the secret is unavailable). The 202's body is the delivery's receipt (`application/json`), which names its Events.

Hooks and entries follow the configuration: one added by `config apply` serves its URL as soon as the apply is done, and one removed answers 404, with no restart.

Ingress turns it into one `webhook.delivery` Event, or one for each target of a hook that fans out, with payload `webhook.delivery.v1`: `{hook, target?, delivery?, correlation?, content_type?, body | text}`. A JSON body is kept as JSON, and any other body as text.

**Deduplication.** The Event's delivery ID is the hook and a digest of the delivery key's value, so the core's `(source, delivery_id)` deduplication ([event ingress](../event-ingress/)) makes a sender's retry the first delivery again, and the first envelope is kept even when a retry's body differs. A hook without a delivery key gives each POST a fresh ID, so each POST is a new delivery. A sender that retries needs a delivery key.

**Correlation.** The Event's correlation key is `webhook/<scope>/<entry>` with the external ID `<hook>/c/<digest of the correlation value>`, or `<hook>/d/<delivery ID>` when there is none. A delivery with a correlation value already seen therefore names the Work the first one opened.

**The receipt.** The 202's body lists the delivery's Events, one for each [fan-out](#fan-out) target or else one, in target order: `{"events": [{"id": "<id>", "target": "triage", "duplicate": false}], "duplicate": false}`. `target` is left out for a hook that doesn't fan out. The host's `ingress.deliver` reports whether the delivery added each Event. An Event's `duplicate` is true when it was already accepted, and its `id` is then the first delivery's. The top-level `duplicate` is true only when every Event was, as with a sender's retry of the same delivery key, so a retrying sender can stop. A host that doesn't report it is taken to have added each Event, so a delivery is never claimed as a duplicate wrongly. The receipt doesn't name the Work: routing happens after the 202, and the route decides which Work an Event reaches, even when a correlation value has Work already. An operator follows each Event to its Work with `GET /v1/events/{id}/route` (or `open-conduit events get`). A sender without an operator token has no read of its own delivery's route yet.

### Fan-out

A delivery goes to the first route that takes it, never to every route that would ([runtime semantics](../runtime-semantics/), rule 3), so a second route for the same hook never sees it. A hook that should start Work for several bindings lists them as targets instead:

```json
{"name": "tickets", "delivery_key": "header:Idempotency-Key", "correlation_key": "body:/ticket/id", "fan_out": ["triage", "audit"]}
```

with a route for each target:

```json
{"name": "triage", "handler": "webhook", "binding": "triage", "config": {"hooks": ["tickets"], "targets": ["triage"]}},
{"name": "audit", "handler": "webhook", "binding": "audit", "config": {"hooks": ["tickets"], "targets": ["audit"]}}
```

Ingress accepts one Event for each target, all in one transaction, so the 202 covers every target. Each Event carries the whole body and its `target`, and has its own identity: the delivery ID `<hook>/<target>/<delivery ID>` and the thread `<hook>/<target>/c/<digest>` (or `<hook>/<target>/d/<delivery ID>`). Deduplication, threading and successors therefore work per target: a retry is every target's first delivery again, and a later delivery of the ticket is each target's own Work's input. Each target's Event is routed on its own, first match as ever. A route without `targets` takes every target, each as Work of its own, and an Event no route takes is `unmatched` like any other.

The targets are config, so every delivery goes to all of them. When which agents should take a delivery depends on what it says, bind the hook to one agent that hands the rest on as child Work (issue #568). A hook has at most 4 targets. Each target's Event carries the whole body, and a delivery's Events are accepted in one ingress page (8 MiB by default), so the Events of one delivery may encode to at most 7 MiB together, and a delivery over that is refused with 413 rather than a 503 the sender would retry. JSON can encode a body byte as six (`<`, `>`, `&` and control characters become `\u00XX`), so the body a hook can take depends on its targets and on what the body holds: any 1 MiB body fits one Event, plain text or JSON of 1 MiB fits 4 targets, and a body that is all markup fits about 1.1 MiB ÷ the number of targets.

The family is one-shot, declared with its loss: a POST leaves no history to read back, so reconcile returns an empty complete snapshot, and a POST sent while the listener or tunnel is down is lost (the sender sees the failure and may retry).

## Routing and Work

For a delivery to one of the scope's webhook entries, of a hook the route takes:

- with no owner of its correlation key, it creates `webhook.delivery` Work with the ID `<namespace>/<external ID>`, titled `A delivery to <hook>: <correlation or delivery value>` (`A delivery to <hook> for <target>: …` for a fan-out target);
- with a live `webhook.delivery` owner, it is that Work's input, as a Slack reply is its thread's;
- with a finished `webhook.delivery` owner, it opens the thread's next generation: a successor Work, `<owner ID>/g2` (then `/g3` and on), with the generation key `<external ID>/g2`. The delivery is its first input, and a `core.work.created` Event links it to the finished Work. Later deliveries of the thread route to it, and a type-only wait on it wakes on them. A ticket that is reopened, or an alert that fires again, therefore starts new Work rather than being dropped;
- with any other owner, it is skipped.

The core does this (`RouteDecision.Successor`, [runtime semantics](../runtime-semantics/), rule 9) in the transaction that routes the delivery, so no delivery to a finished thread is lost. Each generation is its own Work with its own history and outcome, and a correlation value may name a thread that ends and starts again. A value that names something long-lived, such as a service name, still gives a new generation each time the last one finishes, so a thread per incident or ticket keeps each generation's Work about one thing.

## The run

An agent's turn reads the delivery rendered by the package: a heading naming the hook, the delivery's Event ID, its target, the delivery and correlation values, the content type, then the body. A body over 8 KiB is cut, with a note naming the operation that returns it whole.

`webhook.delivery.read {event}` returns the delivery the turn names in full: its Event ID, receive time and `webhook.delivery.v1` payload, body included. It reads that one Event and returns it only when it is a delivery routed to the calling Work (bound from durable state), so a Work can't read another's. A binding needs the operation granted (in its `operations` and the scope's `authorization.operations`). A shell binding has the whole Event, payload and body included, in `OS_INPUT_FILE` as well.

## Doctor

- `webhook` lists each hook's path, with `<token>`.
- `webhook-urls` (`doctor --check-secrets`) prints each hook's signed URL at its current version, and fails when an entry's secret is unavailable.

## Evidence

- [`extensions/webhook`](https://github.com/open-conduit/open-conduit/blob/main/extensions/webhook/webhook_test.go) over the SDK host: retries, threads, fan-out Events, routes selecting by target, a 413 for an escape-heavy body a 4-target hook's Events couldn't carry, a missing or long delivery key, a wrong token, a version bump revoking one hook's URL while the entry's other hook keeps its own, an unknown hook, an oversized body, POSTs without a delivery key, JSON pointer edge cases, config refusals, the route's decisions, rendering, the doctor lines, and `webhook.delivery.read` returning a 9 KB body to its own Work while refusing another Work and Work the scope doesn't hold.
- [`TestWebhookDeliveriesThroughRuntime`](https://github.com/open-conduit/open-conduit/blob/main/integration/webhook_deliveries_test.go): the real runtime ingress and router over SQLite. A retry with another body is the first delivery, a delivery with the same correlation value is routed to the first one's Work, and every other delivery has Work of its own.
- [`TestWebhookFanOutThroughRuntime`](https://github.com/open-conduit/open-conduit/blob/main/integration/webhook_deliveries_test.go): a hook that fans out to two targets, with a route and binding for each. One POST, its retry and a later delivery of the same ticket give two Works, one on each binding, each with that target's two deliveries. An escape-heavy delivery just under the encoded bound is accepted in one ingress page, and one just over it is refused with 413.
- [`TestDaemonWebhookThreadGenerations`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_webhook_test.go): a ticket's Work finishes, its next delivery opens a linked successor that parks on a type-only wait, and the delivery after that wakes the successor rather than starting more Work.
- [`TestDaemonWebhookDeliveries`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_webhook_test.go): the composed daemon. A hook and an entry added by `config apply` take deliveries with no restart. Two deliveries and a retry run as two Works, each with its body in `OS_INPUT_FILE` and read back whole through `os call webhook.delivery.read`; a wrong token is refused, and `doctor --check-secrets` prints the URL.
- `TestCertificationFirstPartyPackages/webhook`: ingress conformance, the route's decisions and the operation's refusal of unbound Work.

## History

- Issue #566 added the extension, so that a POST can be Work of its own rather than a trigger merged into a job's one run.
- Issue #572 added fan-out, so that one delivery can start Work for more than one binding without the core broadcasting it.
