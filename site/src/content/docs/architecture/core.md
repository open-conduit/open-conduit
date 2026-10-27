---
title: "The core in one page"
description: "The five primitives the Open Conduit core coordinates, and what it leaves to extensions and executors."
---

This page summarises the core of Open Conduit: the five primitives it coordinates and what it leaves to behaviour supplied outside it. The primitive records are in [`domain`](https://github.com/open-conduit/open-conduit/tree/main/domain), the public contracts in [`extension`](https://github.com/open-conduit/open-conduit/tree/main/extension), [`storage`](https://github.com/open-conduit/open-conduit/tree/main/storage) and [`observability`](https://github.com/open-conduit/open-conduit/tree/main/observability), and the runtime that drives them in [`internal/runtime`](https://github.com/open-conduit/open-conduit/tree/main/internal/runtime). Every workload (trackers, chat support, scheduled jobs, self-reporting) is an extension, an executor or both, registered by the composition root alone; the core packages import none of them.

The core coordinates five primitives: **Work, Event, Execution, Timer/Wait and Scope**. The user binds behavior to a Scope. Behavior interprets opaque data; the core manages durability and progress.

**Work** is a durable objective. It has an identity, an opaque kind and context, a Scope, correlation keys, a revision and a lifecycle. Work becomes ready, running or waiting, and eventually completed, failed or cancelled. A failed attempt does not by itself fail Work. A child is another Work with its own identity and correlation keys.

**Event** is an immutable record of something that happened. Its envelope holds identity, source, delivery identity, time, correlation and causation; its data is opaque. Source plus delivery identity deduplicates an Event. Correlation finds the Work it concerns, so distinct Events can share one correlation key. The core durably accepts an Event before acknowledging it or taking action. Route evaluation is serialized per correlation key and processing is ordered per Work, with idempotent replay. Routes select a Scope and create or advance Work. During an active attempt a route explicitly chooses queue, inject, interrupt or drop. Derived Events carry causation; the core limits chain depth and rate.

**Execution** is one attempt to advance Work through a bound executor. The core schedules the attempt under a renewable, fenced lease, observes its outcome, and applies retry policy. The executor performs the attempt and owns any private continuity. Run is required; interrupt, inject and resume are declared capabilities checked when binding. An outcome is completed, failed, waiting or handoff with a summary and opaque data. All attempts use the same history, timeout, retention and inspection model. Recovery either re-establishes authority over a surviving attempt or records a recoverable failure before retrying; stale authority cannot commit progress.

**Timer/Wait** makes time and suspension durable. A Timer emits an Event at its due time; firing and rearming commit atomically. A Wait records an Event match against Work correlation keys, optionally restricted by type and bounded by a timeout. Parking releases the lease and makes Work waiting. A matching Event or timeout Event wakes it through the same routing path. A capable executor resumes the parked Execution; otherwise the waiting attempt is terminal and the wake starts the next attempt with accumulated context.

**Scope** holds configuration, secret references, routes and executor bindings, optionally inheriting from a parent. A Scope may request optional resources through behavior supplied outside the core. Resource absence is valid. The core does not inspect resource internals or interpret domain payloads.

Ingress combines push with reconcile: it supplies current external observations and the core diffs them against durable observations to emit Events. Non-re-derivable deliveries must be declared. All producers use the same acceptance path. Executions receive a scoped toolbelt to emit Events, create Work, wait, declare outcomes and read context, with additional operations supplied outside the core. Storage provides atomic updates, revision checks, leases, Timers and namespaced coordination KV; a separate streaming artifact store carries larger outputs. Lifecycle telemetry links the triggering Event, Work, Scope, binding and Execution through to emissions and outcome.

The core's additions beyond these primitives are extension-neutral: resource brokering, translated waits and emissions, cited causation, calendar recurrences, standing Work kinds and binding deadlines. None of them names a service or a domain type.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **Stage 02 gate.** The page was reviewed against the implemented runtime. The daemon then composed only these primitives, a shell executor and a generic operator route ([daemon composition](../daemon-core-gate/)).
- **Stage 05 gate (05-04).** The page was reviewed again with four workloads composed on one daemon: a tracker workload, a chat support workload, scheduled jobs and self-reporting. Each was an extension, an executor or both, registered by the composition root alone. The core packages imported none of them, and the page needed no change: every workload reached the core only as Events, routes, Work, Executions, Timers, Waits and Scope configuration. The core's additions during stages 03–05 were all extension-neutral (resource brokering, translated waits and emissions, cited causation, calendar recurrences, standing Work kinds, binding deadlines). See [the stage 05 gate](../daemon-core-gate/#all-four-workflows-on-one-daemon).
