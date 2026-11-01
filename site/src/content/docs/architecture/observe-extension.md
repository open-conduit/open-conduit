---
title: "Observe extension"
description: "How an executor's report of an outside item becomes an Event and per-thread Work: source, reporter and item identity, de-duplication, revisions and trust."
---

The [`observe` extension](https://github.com/open-conduit/open-conduit/tree/main/extensions/observe) lets any executor report what it observed in an outside source, and turns each thread of that source into Work. It is in-process and first-party, but uses only public hooks: a translated emission with `EmitPlan.Once` ([extensions](../extensions/#translated-emissions)), a route handler, an entry type and a Work kind. The recipe is [observed sources](../../workflows/observed-sources/).

## Three identities

An observation has three identities, and only two of them name it.

| Identity | What it is | Where it comes from |
| --- | --- | --- |
| Source | The logical outside source, such as `slack-help` | The scope's `observe` entry declares it. It is part of every key, so it survives replacing the observer. |
| Item | The source's own ID for the item, and its revision | The executor, from the source. |
| Reporter | The binding whose execution recorded it | The stored Execution, never the caller. It is checked against the entry and kept for audit, but is part of no identity. |

The thread's correlation key is `observe/<scope>/<source>` with the thread ID as its external ID. The de-duplication identity is the digest of source, item and revision, passed as `EmitPlan.Once`. Two bindings reporting the same item of the same source therefore give one Event and the same thread Work.

## Recording

`observe.record {item, thread, text, revision?, source?}`:

1. The input is checked strictly: `item` and `thread` 1 to 256 bytes, `revision` at most 128, `text` at most 16 KiB, all UTF-8.
2. The calling binding is read from the Execution. The scope's `observe` entries give the sources whose `reporters` list it. None is refused with `not_a_reporter`. `source` may be left out when there is exactly one, and must be one of them when given.
3. The plan is an `observe.item.recorded` Event with payload `observe.item.v1` (`{source, item, revision, thread, text}`), addressed to the thread's key, with `Once` set to the item's identity.

The toolbelt accepts it like any translated emission: trusted `execution/<id>` source, causation from the run's trigger, chain-depth and per-chain rate limits. Under `Once`:

- **A repeat** of the same item and revision with the same thread and text is the first Event again. Nothing is accepted and no budget is spent. The output says `"duplicate": true`.
- **The same item and revision with other content** is refused with `once_conflict`. Content is never hashed to tell versions apart, because an edit back to earlier text (A, B, A) would then vanish. An edit is a new revision, and the executor supplies the source's own edit mark.
- **Deletions** are not modelled: an observer rereading a source can't tell a deleted item from one it didn't see.

## Trust

The items are not roots, so the rule that only trusted ingress creates root activity ([runtime semantics](../runtime-semantics/)) still holds. Each item derives from the run that saw it, normally a scheduled job's occurrence. It sits at depth 1 on that chain and spends that chain's per-minute budget.

The boundary is the entry. A binding can only record for a source the entry lists it under, and only into `observe/<scope>/…` keys. The executor names neither the source's authority nor the reporter. The payload is untrusted content, like an ingested Slack message: it is stored on the Event and kept out of diagnostics and logs.

## Routing and Work

The `observe` route's config names one source (`{"source": "slack-help"}`). It takes items only while one of the scope's `observe` entries still declares that source, so removing a source stops it even for items accepted before the removal and not yet routed. For an item of that source:

- with no owner of the thread key, it creates `observe.thread` Work, titled with the first line of the item's text;
- with an `observe.thread` owner, it is that Work's input;
- with any other owner, it is skipped.

A thread's Work is ordinary Work. Between turns its agent parks on a type-only wait for `observe.item.recorded`, which matches the Work's own thread key. A later run's new item in the thread is routed to it and satisfies the wait. Once the Work has completed, later items of the thread start nothing, as for any terminal Work. The responder should therefore wait, with a timeout, while the thread may continue.

Follow-ups reach the thread only when an observer next records them. A responder's replies posted through the same account as the observer's reads are read back as items, and the depth limit can't stop that loop, because each read is a fresh run. The observer must skip them, for example by a marker the responder adds.

## Configuration

| Config | Fields |
| --- | --- |
| Entry (`type: observe`) | `sources`: `[{name, reporters}]`. A name is lower case letters, digits, `.`, `_` and `-`, at most 64. Each source lists at least one binding. The entry takes no secrets. |
| Route (`handler: observe`) | `source`: the source whose threads the route takes. |

Several entries in a scope may declare the same source; their reporters are combined. Removing a scope's last entry removes `observe.record` from the generation's toolbelt.

## Evidence

- [`TestObservedSourceSurvivesReplacingTheObserver`](https://github.com/open-conduit/open-conduit/blob/main/integration/observe_test.go): an agent watcher and the script that replaces it record one source. Repeats are duplicates, there are two thread Works throughout, and a binding not listed as a reporter, or not granted the operation, is refused.
- `TestObservedRevisions`: A, B, A as three revisions are three Events. Changed text, or another thread, under an existing revision is refused.
- `TestObservedSourceRemovedBeforeRouting`: an item accepted while its source was declared, then routed after the source was removed (with the route kept), starts no Work.
- [`TestObservedSourceFromAShellScript`](https://github.com/open-conduit/open-conduit/blob/main/integration/observe_shell_test.go): a shell binding records through the shipped `os` CLI over the Unix-socket toolbelt, with the command the recipe shows. Its repeat is a duplicate, an undeclared source is refused, and the thread gets one Work.
- `TestObservedThreadParksAndWakes`: the responder parks on its thread. A later run by another reporter records the opener again and a follow-up. Only the follow-up is new, it wakes the Work, and it is the next turn's only input.
- [`TestTranslatedEmitOnce`](https://github.com/open-conduit/open-conduit/blob/main/integration/reporting_test.go): the core contract of `EmitPlan.Once` (repeat, conflict, size bound, per scope).

## History

- Issue #318 spiked recording outside items as Work, then built it with the source, reporter and item identities kept apart.
