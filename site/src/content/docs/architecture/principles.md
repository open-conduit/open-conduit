---
title: "Core principles"
description: "The principles Open Conduit is built on: Work is durable, executions are attempts, and everything domain-specific is an extension."
---

Work is the fundamental abstraction. Everything else exists to discover it, advance it, observe it or manage its lifecycle.

An LLM isn't fundamental. Neither is GitHub, Slack, a human, a shell command, or even an execution. They are participants in the lifecycle of Work.

These principles are how we decide what belongs where. [The core in one page](../core/) describes the primitives that put them into practice, and the design decisions B01 to B09 record the rules that follow from them.

## The philosophy in five sentences

1. Work is durable; executions are attempts.
2. Events describe reality; the runtime manages consequences.
3. The core owns orchestration semantics; extensions own domain semantics.
4. Any suitably capable participant can execute, observe, initiate or influence Work, subject to explicit authority.
5. Complex behaviour emerges through composition, not through special cases in the core.

## The principles

### 1. Work is first-class; execution is transient

Work has an identity and lifecycle independent of whoever or whatever is executing it. An Execution is merely an attempt to advance that Work.

Work can survive executor replacement, failure, suspension, restart, handoff and human intervention. An unsuccessful attempt doesn't necessarily mean the Work has failed.

The executor is replaceable. The Work isn't.

### 2. The core orchestrates work; it doesn't understand how to do it

The core knows about Work, Events, Executions, Timers/Waits and Scopes.

It does not need to know what a pull request, Slack conversation, customer, model or codebase is. Domain semantics belong outside the core.

Its responsibility is lifecycle, scheduling, persistence, authority, correlation and recovery, not domain-specific business logic.

This is a crucial restraint: a feature shouldn't enter the core merely because it's widely useful.

### 3. Everything domain-specific is a composable extension

GitHub, Slack, Jira, Linear, Jiminny, setup and introspection are examples of behaviours composed around generic primitives.

Extensions can provide integrations, executors, operations, resources and interfaces to the outside world.

Crucially, a capability implemented in a bundled extension must also be implementable by an independently installed extension without changing the core. Extracting setup into its own repository is meant to prove exactly this: no privileged integration architecture disguised as extensibility.

### 4. Executors are interchangeable and capability-driven

Claude Code, another LLM, a shell script, a browser automation, a conventional service, or even a human can advance Work.

The orchestration model doesn't change with the executor.

Executors declare capabilities such as running, resuming, injecting context or interrupting. The runtime binds them appropriately rather than assuming every executor behaves like an LLM.

This is why agent-native is a useful description but LLM-centric isn't.

### 5. Events are immutable facts; the core makes them durable

Something happens. That observation becomes an Event. The Event is accepted durably, correlated with Work, and routed.

Event identity, delivery deduplication, correlation and causation are separate concepts.

An executor can report what it observes without being responsible for routing, interpreting or acting on it. The observer and the performer need not be the same executor.

What an executor can observe, Open Conduit can potentially manage durably.

### 6. Time, waiting and recovery are platform concerns

An agent shouldn't have to remain alive to wait for something.

It should be able to express intent ("wake me when this pull request merges") and leave the mechanics to the runtime and extensions.

Durable timers, event-matched waits, retries, leases, recovery and resumption are generic machinery.

This lets Work span seconds, days or weeks without coupling its lifecycle to the lifetime of a process.

### 7. Authority belongs at explicit boundaries, not inside agent instructions

Executors receive narrowly scoped toolbelts granting the operations appropriate to their Work, binding and current authority.

The host checks operations. An executor cannot simply forge sources and causation.

The core enforces durable authority and safety invariants; extensions implement domain-specific operations and their additional checks.

Sandboxing is separately composable with executors and resources, and preferably fails closed. The security boundary must survive process failure, reconfiguration and recovery.

### 8. Humans are participants in Work, not a special orchestration mechanism

Human interaction is a composable extension.

An executor can request a decision, park its Work, and resume when the decision arrives.

The core doesn't need to understand a Slack DM, an approval panel or any particular human interface. It needs to manage waiting, authority and the resulting Event.

Human-in-the-loop becomes a reusable capability rather than custom logic threaded through every workflow.

### 9. Mechanisms should be interchangeable behind stable contracts

Boundaries are more important than particular implementations.

An extension communicating over JSON-RPC, a shell invoking the toolbelt CLI and an LLM using MCP can all participate in the same orchestration model.

Resources (where work happens), executors (how work happens) and sandboxes (what an execution is allowed to access) are distinct and compose.

Composition is preferable to another special-purpose implementation in the core. Portability must be demonstrated, not merely claimed.

### 10. Autonomous operation requires inspectability, evidence and correction

Autonomous systems must be observable, recoverable and steerable.

Work, executions, triggering Events, operations, outcomes and emitted Events should be traceable through a unified history.

Introspection and self-improvement emerge from observing this system, not from granting an agent mysterious privileged access to the runtime.

Agents can identify problems, propose changes and repair their own tooling, while humans keep control over consequential decisions.

## The architectural litmus test

If a new capability requires the core to understand a particular domain, integration or executor, first ask whether the abstraction is wrong.

Introducing Slack didn't justify teaching the core about conversations. Introducing GitHub dependencies didn't justify teaching it about pull requests. Introducing human decisions didn't require building humans into the scheduler. And introducing agent observations shouldn't require the core to understand what the observer saw.

The proper question is whether a reusable primitive is missing, or whether an extension should translate that behaviour into existing primitives.
