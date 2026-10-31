---
title: What Open Conduit does
description: The daemon, the five primitives everything is built from, and how integrations plug in.
---

Open Conduit runs coding and support agents against the places work arrives: GitHub issues, Slack threads, schedules and its own defect reports. A single local daemon accepts what happens, decides which agent or command handles it, runs that attempt, and keeps going across waits, restarts and failures. You watch and steer it from a terminal UI.

## Five primitives

Everything is built from five primitives:

- A **Work** item is a durable objective, such as an issue to fix or a thread to answer. It becomes ready, running or waiting, and eventually completed, failed or cancelled. A failed attempt does not by itself fail the Work.
- An **Event** is an immutable record of something that happened, and is accepted durably before anything acts on it. Duplicate deliveries are recognised and dropped.
- An **Execution** is one attempt to advance Work through an executor, such as Claude Code or a shell command. Every attempt shares the same history, timeout, retry and inspection model.
- A **Timer** or **Wait** makes time and suspension durable, so Work can park until a matching Event or a timeout wakes it, even across a restart.
- A **Scope** holds the configuration, secret references, routes and executor bindings for one area of work.

## How the pieces fit

Integrations (GitHub, Slack, schedules, introspection, and any extension you install) turn what happens outside into Events. **Routes** send those Events to a **binding**, which names an executor and the operations it may call. When an Event arrives while an attempt is already running, the route decides whether to queue it, inject it into the running session, interrupt the run or drop it.

Each run gets a scoped **toolbelt**: the operations its binding allows, such as commenting on an issue, opening a pull request, replying in a thread or waiting for a check to finish. Agents ask for outcomes through the toolbelt and never deal with the tracker's conventions directly.

A binding can also run its agent in a [sandbox or a container](../../guides/sandboxes/), so a run reaches only its workspace, the hosts you allow and its own toolbelt, never your credentials or Open Conduit's own state.

Because GitHub, Slack, scheduled jobs and introspection all go through the same core, they share one history, one retry and timeout model, and one view in the terminal UI.

For the design in depth, see [the core in one page](../../architecture/core/).
