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
| `webhook_secret` | A selected secret name. Each hook's URL token is derived from it, so rotating it revokes every URL of the entry. |
| `hooks[].name` | Lower case letters, digits, `_` and `-`, at most 64. It is in the URL and in the Work's title. |
| `hooks[].delivery_key` | Optional. Where a delivery's identity is read: `header:<name>` or `body:<JSON pointer>`. |
| `hooks[].correlation_key` | Optional. Where a delivery's thread is read, in the same form. Best a value that names something that ends and may start again, such as a ticket or an incident (see [Routing and Work](#routing-and-work)). |

A key is `header:<name>` (the header's value) or `body:<pointer>` (an RFC 6901 pointer into a JSON body, naming a string, number or boolean). A value is at most 256 bytes.

The route is `{"handler": "webhook", "config": {"hooks": ["deploys"]}}`. `hooks` limits the route to those hooks; empty takes every hook of the scope's webhook entries. One route per binding gives each hook its own command or agent.

## Ingress

The webhook listener serves `POST /webhook/<scope>/<entry>/<hook>/<token>`. The token is an HMAC of the entry's secret over the namespace (`webhook/<scope>/<entry>`) and the hook, so it is stable across restarts and distinct for each hook. The handler:

1. Answers 404 for an unknown hook or a wrong token, compared in constant time before the body is read.
2. Answers 413 for a body over 1 MiB.
3. Reads the hook's keys. A hook with a delivery key refuses a POST without it, or with a value over 256 bytes, with 400. A missing correlation value is not an error: that delivery is a thread of its own.
4. Hands the delivery to the host and answers 202 only after durable acceptance, or 503 when it wasn't accepted (or the secret is unavailable).

Hooks and entries follow the configuration: one added by `config apply` serves its URL as soon as the apply is done, and one removed answers 404, with no restart.

Ingress turns it into one `webhook.delivery` Event with payload `webhook.delivery.v1`: `{hook, delivery?, correlation?, content_type?, body | text}`. A JSON body is kept as JSON, and any other body as text.

**Deduplication.** The Event's delivery ID is the hook and a digest of the delivery key's value, so the core's `(source, delivery_id)` deduplication ([event ingress](../event-ingress/)) makes a sender's retry the first delivery again, and the first envelope is kept even when a retry's body differs. A hook without a delivery key gives each POST a fresh ID, so each POST is a new delivery. A sender that retries needs a delivery key.

**Correlation.** The Event's correlation key is `webhook/<scope>/<entry>` with the external ID `<hook>/c/<digest of the correlation value>`, or `<hook>/d/<delivery ID>` when there is none. A delivery with a correlation value already seen therefore names the Work the first one opened.

The family is one-shot, declared with its loss: a POST leaves no history to read back, so reconcile returns an empty complete snapshot, and a POST sent while the listener or tunnel is down is lost (the sender sees the failure and may retry).

## Routing and Work

For a delivery to one of the scope's webhook entries, of a hook the route takes:

- with no owner of its correlation key, it creates `webhook.delivery` Work with the ID `<namespace>/<external ID>`, titled `A delivery to <hook>: <correlation or delivery value>`;
- with a live `webhook.delivery` owner, it is that Work's input, as a Slack reply is its thread's;
- with a finished `webhook.delivery` owner, it opens the thread's next generation: a successor Work, `<owner ID>/g2` (then `/g3` and on), with the generation key `<external ID>/g2`. The delivery is its first input, and a `core.work.created` Event links it to the finished Work. Later deliveries of the thread route to it, and a type-only wait on it wakes on them. A ticket that is reopened, or an alert that fires again, therefore starts new Work rather than being dropped;
- with any other owner, it is skipped.

The core does this (`RouteDecision.Successor`, [runtime semantics](../runtime-semantics/), rule 9) in the transaction that routes the delivery, so no delivery to a finished thread is lost. Each generation is its own Work with its own history and outcome, and a correlation value may name a thread that ends and starts again. A value that names something long-lived, such as a service name, still gives a new generation each time the last one finishes, so a thread per incident or ticket keeps each generation's Work about one thing.

## The run

An agent's turn reads the delivery rendered by the package: a heading naming the hook, the delivery's Event ID, the delivery and correlation values, the content type, then the body. A body over 8 KiB is cut, with a note naming the operation that returns it whole.

`webhook.delivery.read {event}` returns the delivery the turn names in full: its Event ID, receive time and `webhook.delivery.v1` payload, body included. It reads that one Event and returns it only when it is a delivery routed to the calling Work (bound from durable state), so a Work can't read another's. A binding needs the operation granted (in its `operations` and the scope's `authorization.operations`). A shell binding has the whole Event, payload and body included, in `OS_INPUT_FILE` as well.

## Doctor

- `webhook` lists each hook's path, with `<token>`.
- `webhook-urls` (`doctor --check-secrets`) prints each hook's signed URL, and fails when an entry's secret is unavailable.

## Evidence

- [`extensions/webhook`](https://github.com/open-conduit/open-conduit/blob/main/extensions/webhook/webhook_test.go) over the SDK host: retries, threads, a missing or long delivery key, a wrong token, an unknown hook, an oversized body, POSTs without a delivery key, JSON pointer edge cases, config refusals, the route's decisions, rendering, the doctor lines, and `webhook.delivery.read` returning a 9 KB body to its own Work while refusing another Work and Work the scope doesn't hold.
- [`TestWebhookDeliveriesThroughRuntime`](https://github.com/open-conduit/open-conduit/blob/main/integration/webhook_deliveries_test.go): the real runtime ingress and router over SQLite. A retry with another body is the first delivery, a delivery with the same correlation value is routed to the first one's Work, and every other delivery has Work of its own.
- [`TestDaemonWebhookThreadGenerations`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_webhook_test.go): a ticket's Work finishes, its next delivery opens a linked successor that parks on a type-only wait, and the delivery after that wakes the successor rather than starting more Work.
- [`TestDaemonWebhookDeliveries`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_webhook_test.go): the composed daemon. A hook and an entry added by `config apply` take deliveries with no restart. Two deliveries and a retry run as two Works, each with its body in `OS_INPUT_FILE` and read back whole through `os call webhook.delivery.read`; a wrong token is refused, and `doctor --check-secrets` prints the URL.
- `TestCertificationFirstPartyPackages/webhook`: ingress conformance, the route's decisions and the operation's refusal of unbound Work.

## History

- Issue #566 added the extension, so that a POST can be Work of its own rather than a trigger merged into a job's one run.
