---
title: "Durable routing and Work lifecycle"
description: "How Open Conduit routes accepted Events to Work, manages the Work lifecycle and bounds derived Events."
---

The router turns accepted Events into Work: it matches each Event against the Scope's routes, creates or attaches Work, queues input and records control intents for running attempts. Alongside it, the Work lifecycle operations and causal limits implement the routing, lifecycle and causal parts of [B04](../runtime-semantics/). The [runtime](https://github.com/open-conduit/open-conduit/tree/main/internal/runtime) uses public storage and extension contracts only. In the daemon, the host loop in [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/tree/main/internal/bootstrap) routes every pending Event in acceptance order and hands ready Work to the [scheduler](../execution-scheduling/); the router itself makes no execution or control calls. [Integration tests](https://github.com/open-conduit/open-conduit/blob/main/integration/routing_test.go) compose it with real SQLite.

## Routing

`NewRouter(store, now, RouterOptions)` takes a trusted clock, a configuration generation, deterministic route handlers and registered executor implementations/fallbacks. `RouterOptions.Bindings` is keyed by executor registration, while routes refer to effective Scope binding names. The router checks implementations with `extension.ValidateBinding`; capability flags alone cannot establish control support. Queue-only fixtures may omit an executor, but cannot claim optional capabilities. Router option maps are copied at construction. Stored Scope routes resolve root to leaf, preserving override slots, and inherited routes use the target Scope's effective named bindings. Configuration loading, authorization ceilings and reload publication belong to the [configuration loader](../config-management/), which supplies each generation's router.

`Route(ctx, eventID)` reads durable accepted input and evaluates handlers outside storage transactions. Each handler receives a detached snapshot. The first match wins, including an effective drop; handler errors stop processing. The commit revalidates Scope and Work snapshots, correlation ownership and input order. A changed snapshot requires reevaluation. Conflicting identities leave the Event unrouted for diagnosis. A blocked handler holds no global coordination lock, so independent Work can progress.

Work scope and owned correlations stay fixed. Attachment adds keys atomically with route effects and processing checkpoints. Earlier accepted, still-unrouted input for newly attached keys joins the same Work in acceptance order; previously checkpointed unmatched/drop/terminal input stays historical. Already owned keys cannot transfer between Work records. Explicit Work, Execution and correlation targets must agree.

The route transaction persists a `RouteRecord` with Event/Work, selected route/binding, generation, effective policy and reason alongside `MarkRouted`. Queue input retains acceptance order. Active inject/interrupt policies persist stable intents targeting the specific attempt and lease epoch, while retaining input for later presentation until consumption is proved. The router makes no execution or control calls. Unsupported controls require an explicit queue/drop fallback; a route selecting a different binding from the active attempt also requires an explicit fallback. Ready or waiting Work queues ordinary matching input instead of applying running-attempt controls. The scheduler must pin attempt configuration and validate dispatch against that generation, including changes to an executor behind the same binding name.

`Trace(ctx, scope, event, filter)` is a dry run of routing for a hypothetical Event, and it writes nothing. The management endpoint first refuses an Event acceptance would refuse (`CheckTraceEvent`, `Ingress.AdmitsTrace`), so a trace never explains an Event that could not happen. It reads the state routing reads: the scope's effective routes and bindings, the owning Work, its active wait and the active dependency subscriptions. It then calls the same code routing does:
- the handlers, outside the transaction;
- `waitWinnerInTx`, the winner selection `WakeWaitInTx` uses;
- `policyInTx`, the policy decision `apply` records.

Each route gets a verdict: `match`, `skip`, `not_reached` after the first match, or `bypassed` when no route is consulted. A handler explains match and skip through `RouteDecision.Reason`, a short phrase that never changes the decision; `extension.Skip(reason)` builds a non-match. Because traces call handlers, `Match` must stay free of side effects.

- **Waiting Work.** Routes are not consulted when the Event's own Work is waiting, so every route is `bypassed`. The effect is `wake` when the Event wins the wait, or `queue` when it is kept as context: it does not match, the wait is blocked by the causal limits, or the causal limits would refuse the `core.wait.woken` bridge that wakes its parked attempt (`wakeRefusalInTx`, the check `WakeWaitInTx` makes, without writing). That refusal applies equally when an earlier accepted Event would win. When an earlier accepted Event or the due timeout wins first, `also` reports that wake and the Event is routed to the ready Work as usual.
- **Dependency subscriptions.** An Event that matches an active dependency subscription from its own scope is reported in `effect` or `also` as a `wake` of the waiting Work, naming that Work's scope and wait. The handlers' own verdicts on the Event are kept.
- **Terminal Work.** Every route is `bypassed` and the effect is `drop`.
- **Otherwise.** The effect is `start` for new Work, or the policy routing would record for existing Work, including its fallbacks.

A handler may also implement `extension.RouteSampler` to offer named sample Events built from the scope's configuration; `Samples(ctx, scope)` collects them in route order. A sampler is given the router's clock, so a sample whose data carries a time (a Slack message written just now) stays inside the handler's window under a test clock or skew.

`PendingInputsInTx` and `ConsumeInputsInTx` let a scheduler join selection, claim and consumed-input history in one guarded transaction. `PendingInputs`, `Decision` and `Intents` inspect durable coordination records. Consumption removes pending entries but preserves their attempt association; replaying a route cannot enqueue them again. These are trusted internal APIs, not an execution-facing claim or acknowledgement protocol.

## Work lifecycle

`Complete` requires current attempt authority and refuses to overtake accepted, unrouted input belonging to that Work. Remaining queued or unconfirmed injected input leaves Work ready; otherwise it becomes completed. `Cancel` atomically cancels active attempts and Waits, removes owned timers and queue eligibility, and terminalizes Work. Physical process cleanup follows that commit in the scheduler. Cancellation needs no derived-event token. Terminal records remain immutable and late input is checkpointed as terminal.

`CreateChild` selects parent Work, Scope and trigger from the leased attempt. `CreateSuccessor` is a trusted policy operation requiring terminal predecessor ownership and distinct external keys. Each publishes a new Work and a causally linked `core.work.created` Event atomically. The creation payload records the child or predecessor link; a successor has no child `ParentID`, and old keys stay with the predecessor. The extension supplies a consistent generation-qualified identity for subsequent external deliveries.

## Causal limits

`NewCausal(store, now, limits)` coordinates derived acceptance. `Derive` and the transaction-level helpers require a parent chosen by trusted core code. `Emit` selects it from the fenced attempt's immutable trigger and rejects a supplied conflicting parent, reserved core families and cross-Work identity. Authentication, event declarations and operation authorization belong to the [toolbelt and management boundary](../toolbelt-management/). Existing `Ingress.Accept`/`AcceptInTx` remain trusted acceptance primitives whose callers must enforce this distinction; registered ingress adapters can supply only external roots.

Defaults are depth 32 and 128 new derived Events per root per fixed UTC minute. Root depth is zero; deduplication spends no token. Root limits are pinned durably on first derived acceptance, and siblings/children retain those limits across runtime recreation and configuration changes. Event ancestry, bucket increments and associated effects commit together. Missing parents, depth excess and rate excess produce durable redacted diagnostics without accepting another Event or consuming a token. The next minute restores only rate capacity. A timer firing with a causation parent goes through the same derived boundary rather than raw storage acceptance ([timers](../timers-waits/)).

## Limits

The router inspects accepted history and coordination KV for routing and duplicate resolution. It establishes correctness, not a high-volume throughput or retention guarantee. Retention must preserve causal metadata and deduplication evidence. Lifecycle telemetry and operator history are described in [observability and history](../observability-history/); executor delivery and consumption of queued and injected input are described in [execution scheduling](../execution-scheduling/) and the [B04 inject acknowledgement](../runtime-semantics/).

## Evidence

The SQLite integration adapter runs nine applicable shared runtime cases over persistent connections and independently reopened handles. Its `Start` method only installs an execution fixture through public storage operations; it does not implement a scheduler or claim production dispatch coverage. Direct regressions cover attachment backlog and nonresurrection, blocked handlers and cross-key ordering, detached snapshots, ancestor revisions, execution-only targets, effective binding overrides, intent identity/fallback, cancellation, conflicting input isolation and successor target Scope. Separate causal and linked-Work tests cover restart-pinned limits, concurrent sibling quotas, replay, forged ancestry, stale leases and atomic child/successor publication.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 02-03 implemented routing, the Work lifecycle and causal limits. At that stage a host still needed the scheduler, timer driver and authenticated management service, which later tickets delivered; the daemon now composes them in `internal/bootstrap`.
- Configuration loading, authorization ceilings, reload publication, authentication and event declarations were assigned to 02-06. Timer due-event and rejection handling through the derived boundary was assigned to 02-05.
- OpenTelemetry export and operator history presentation were assigned to 02-08, and real executor delivery/consumption semantics to 02-04 and 04-02.
- The 02-03 evidence covered routing and causal cases only; full wait, retry and recovery cases were assigned to their implementation tickets.
