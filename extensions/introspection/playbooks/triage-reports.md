---
name: triage-reports
description: Act on the issue reports agents have filed in this scope, the defects they hit in daily use. Group them by cause, fix what is plainly wrong in the project's own files with one commit, file an issue for each remaining cause, and resolve every report you handled. Run by hand under Open Conduit as ordinary Work (open-conduit work create … --binding <triage binding>), started as /open-conduit:triage-reports.
---

# Triage issue reports

Agents report each defect they hit with `introspection.report-issue`: a `kind` (`env`, `docs`, `tooling`, `service`, `runtime` or `other`), a one-line `title` and a `body` with what they tried, the exact error, their workaround and where they think the fix belongs. Each report is a durable Event linked to the execution that hit it. Find the causes behind them and act on each one.

## Your tools

Call an operation with the `call` tool (`mcp__open-conduit__call`), giving the `operation` and its `input` object:

| Operation | Input | Does |
| --- | --- | --- |
| `introspection.reports` | `state`? (`open`, `resolved` or `all`; default `open`) | this scope's reports, oldest first, each with its id, kind, title, body, the Work and execution that reported it, and its report Event |
| `introspection.report.resolve` | `id`, `summary`, `link`? | record how you handled a report (the fix, the issue you filed, or why you dropped it); a resolved report is no longer open |

File issues with whatever tracker access this triage has. The `github.*` operations work only when the triage runs as a GitHub issue's Work (for example an issue that asks for a triage); a triage started by hand usually has none, and then every remaining cause goes in your final summary, which the operator reads in the run's history.

## Steps

1. `introspection.reports`. If there are none, say "No reports to triage" and stop. Note the exact list: agents may report more while you work, and those wait for the next triage.
2. Read every report. Group them by cause, not by wording: twenty sessions that each tripped over the same missing variable are one cause. For each cause, note how many reports and which Work they came from, and keep one or two exact errors.
3. Decide where each cause's fix belongs:
   - **This project's own files**: a skill, rule or script in the checkout that is plainly wrong: a stale command, a path that has moved, a variable that isn't set. You fix these in step 4.
   - **This project, but not plainly**: a fix that needs a decision, a design or more than a small edit. File it.
   - **The machine or an account**: a missing tool, credential or permission only the owner can add. File it, and say what the owner has to do.
   - **Open Conduit itself** (`runtime`, or a toolbelt operation that fails or is missing): file it, saying which reports it came from.
   - **A passing outage**: a service that failed once and worked on a retry. Drop it unless several sessions hit it.
4. Fix the causes in the project's own files, in one commit, when you run in a checkout:
   - Check it is on the main branch with no uncommitted changes. If it isn't, fix nothing: treat these causes as "not plainly" and carry on. Otherwise `git pull --ff-only`.
   - Make the smallest edit that removes the cause, and fix every other place with the same mistake.
   - Commit only those files, with a message listing each fix and the report it removes, and land it the way this project's rules say (a push, or a PR).
5. For each remaining cause, when you can file issues, look for an open issue first: comment on it with today's count and an exact error if one covers the cause, otherwise open one titled for the cause, not the symptom ("Skills still call `reply.sh`", not "reply.sh not found"). When you can't, write the cause in your final summary instead. Either way, each cause says what the agents were trying to do, how many reports and which Work, the exact error, the workaround they used, where the fix belongs and what you think the fix is.
6. Resolve the reports you handled, and only those, with `introspection.report.resolve`: the commit, the issue's link, "in the triage summary", or why you dropped it. If a step failed partway, leave the reports for the causes it didn't handle open, so the next triage picks them up.
7. Finish in one line: how many reports were read, how many causes they came to, what you fixed, how many issues you opened or updated.

## Don't

- Don't commit anything except the fixes in step 4.
- Don't fix anything that needs a judgement call: file it.
- Don't report a defect about this triage with `introspection.report-issue` unless it is a real new defect: a report made here is a report about a report, and Open Conduit's chain limits stop such loops.
