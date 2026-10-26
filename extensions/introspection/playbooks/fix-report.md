---
name: fix-report
description: Make one fix a triage requested for the defects agents reported: the smallest edit to the project's own files that removes the cause, landed on the main branch (a push, or a PR), then resolve the reports it fixes. Open Conduit runs it as the fix's own Work, started as /open-conduit:fix-report.
---

# Fix a reported cause

A triage grouped the defects agents reported and asked for this fix. Your input is the fix: its title, a body saying what is wrong, where, the smallest edit and how to land it, and the reports (and problem notes) it fixes. The fix Event is caused by the earliest of those reports, so everything you do here traces back to it.

## Your tools

Call an operation with the `call` tool (`mcp__open-conduit__call`), giving the `operation` and its `input` object:

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.reports` | `state`? (`fixing` for the reports a fix covers) | the reports in full, to read their exact errors |
| `introspection.report.resolve` | `id`, `summary`, `link`? | record how a report was fixed: the commit or PR as link |

## Steps

1. Read the fix and the reports it names (`introspection.reports` with `state` `fixing`).
2. Check the checkout you run in is on the main branch with no uncommitted changes, or is this Work's own branch. If it isn't, change nothing: say why in one line and stop; the reports stay open for the next triage.
3. Make the smallest edit that removes the cause, and fix every other place with the same mistake. Don't change anything else.
4. Commit only those files, with a message naming the fix and the reports it removes, and land it the way the fix says (a push to the main branch, or a PR). If the push is rejected, `git pull --rebase` and push again; if it still fails, stop and say so.
5. Resolve each report the fix names with `introspection.report.resolve`: a one-line summary and the commit or PR as `link`.
6. Finish in one line: what you changed, where it landed, and which reports you resolved.

## Don't

- Don't fix anything that needs a judgement call: say so in your summary and resolve nothing, so the next triage files it.
- Don't report a defect about this fix with `introspection.report-issue` unless it is a real new defect: Open Conduit's chain limits stop report loops.
