---
name: triage-reports
description: Act on the issue reports and problems agents have filed in this scope, the defects they hit in daily use. Group them by cause, request fix Work for what is plainly wrong in the project's own files, file or write up each remaining cause, and resolve every report and problem you handled. Open Conduit runs it once a day as the scope's standing triage Work (or by hand as ordinary Work), started as /open-conduit:triage-reports.
---

# Triage issue reports

Agents report each defect they hit with `introspection.report-issue`: a `kind` (`env`, `docs`, `tooling`, `service`, `runtime` or `other`), a one-line `title` and a `body` with what they tried, the exact error, their workaround and where they think the fix belongs. Each report is a durable Event linked to the execution that hit it; the host watch reports the machine running out of load, memory or disk the same way. Agents also log the problems they work around with `introspection.problem` (a note with a `kind` and a body). Find the causes behind them and act on each one.

## Your tools

Call an operation with the `call` tool (`mcp__open-conduit__call`), giving the `operation` and its `input` object:

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.evidence` | `for`: `triage` | this scope's open reports and unhandled problems, each grouped by kind and leading line (count, ids, the Work they came from, sample bodies), the reports a requested fix already covers (`fixing`), and the settings: `land`, `remote` and `file_issues` |
| `introspection.reports` | `state`? (`open`, `fixing`, `resolved` or `all`; default `open`) | this scope's reports, oldest first, in full |
| `introspection.fix` | `reports`? (ids), `notes`? (problem ids), `title`, `body` | request fix Work for one cause: a fix Event caused by the earliest report, which gives the fix its own Work on this scope's fix binding; returns the fix and its Work |
| `introspection.report.resolve` | `id`, `summary`, `link`? | record how you handled a report (the issue you filed, or why you dropped it); a resolved report is no longer open |
| `introspection.notes.resolve` | `notes` (ids), `summary` | record the problems you handled, so the next triage leaves them out |

File issues with whatever tracker access this triage has, when `file_issues` is true. The `github.*` operations work only when the triage runs as a GitHub issue's Work; a scheduled triage usually has none, and then every remaining cause goes in your final summary, which the operator reads in the run's history.

## Steps

1. `introspection.evidence` with `for` `triage`. If there are no open reports and no problems, say "No reports to triage" and stop. Note the exact lists of ids: agents may report more while you work, and those wait for the next triage.
2. Read every report and problem. Group them by cause, not by wording: twenty sessions that each tripped over the same missing variable are one cause. For each cause, note how many reports and problems and which Work they came from, and keep one or two exact errors.
3. Decide where each cause's fix belongs:
   - **This project's own files**: a skill, rule or script in the checkout that is plainly wrong: a stale command, a path that has moved, a variable that isn't set. You request fix Work for these in step 4.
   - **This project, but not plainly**: a fix that needs a decision, a design or more than a small edit. File it.
   - **The machine or an account**: a missing tool, credential or permission only the owner can add, or the host running out of space. File it, and say what the owner has to do.
   - **Open Conduit itself** (`runtime`, or a toolbelt operation that fails or is missing): file it, saying which reports it came from.
   - **A passing outage**: a service that failed once and worked on a retry. Drop it unless several sessions hit it.
4. For each cause in the project's own files, call `introspection.fix` once: the `reports` and `notes` it covers, a `title` for the cause and a `body` saying exactly what is wrong, where, the smallest edit that removes it, every other place with the same mistake, and how to land it (the settings' `land` and `remote`). The fix Work resolves those reports when its fix lands; don't resolve them yourself.
5. For each remaining cause, when `file_issues` is true and you can file issues, look for an open issue first: comment on it with today's count and an exact error if one covers the cause, otherwise open one titled for the cause, not the symptom ("Skills still call `reply.sh`", not "reply.sh not found"). Otherwise write the cause in your final summary instead. Either way, each cause says what the agents were trying to do, how many reports and which Work, the exact error, the workaround they used, where the fix belongs and what you think the fix is.
6. Resolve what you handled, and only that: each report of a filed, written-up or dropped cause with `introspection.report.resolve` (the issue's link, "in the triage summary", or why you dropped it), and every problem you handled, including those a fix covers, with `introspection.notes.resolve`. If a step failed partway, leave the reports and problems for the causes it didn't handle open, so the next triage picks them up.
7. Finish in one line: how many reports and problems were read, how many causes they came to, which fixes you requested, how many issues you opened or updated.

## Don't

- Don't edit or commit anything in a triage: fixes are their own Work.
- Don't request a fix for anything that needs a judgement call: file it.
- Don't write an issue, comment or PR body into the checkout: pass it on stdin (`-` for `<file>` or `--body-file`), or write it in a temporary directory outside the checkout. A stray file there leaves the checkout dirty, so the fixes this triage requests can't land.
- Don't report a defect about this triage with `introspection.report-issue` unless it is a real new defect: a report made here is a report about a report, and Open Conduit's chain limits stop such loops.
