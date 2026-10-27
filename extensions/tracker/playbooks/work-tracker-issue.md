---
name: work-tracker-issue
description: Work a Linear or Jira issue in turns under Open Conduit. The first turn reads the issue, builds the change on the issue's branch in its own workspace, opens a PR on the code host and ends; later turns, started by Open Conduit, act on new comments, check results, merges and closes. Investigation issues get findings on the issue instead of a PR. Started by Open Conduit as /open-conduit:work-tracker-issue issue/<KEY>.
---

# Work a tracker issue

You run in turns. Open Conduit starts your session on `work-tracker-issue issue/<KEY>` and resumes it with `work-tracker-issue issue/<KEY> events` whenever there is news: comments, a PR's checks finishing, a merge, a close. Between turns nothing runs, so never wait or poll for anything: do what you can, then end the turn with your status, and Open Conduit wakes you when the rest arrives.

Arguments: `issue/<KEY>` names the issue (`issue/ENG-12`; a reopened issue carries `/generation/…` after the key). `… events` is a later turn: the events since your last one follow in the message. `… handoff` is a fresh session (see Fresh session). Anything else is a note from Open Conduit; follow it before step 3.

## Your tools

Everything that touches the tracker or the code host goes through Open Conduit's toolbelt, which already knows the issue, the repository and the branch. Call an operation with the `call` tool (`mcp__open-conduit__call`), giving the `operation` and its `input` object. `<tracker>` below is `linear` or `jira`: the one your toolbelt lists.

| Operation | Input | Does |
| --- | --- | --- |
| `<tracker>.context` | | the tracker, the issue key and URL, the branch, how a PR refers to the issue, and where the rules are |
| `<tracker>.issue.view` | `key`? | the issue, its status, labels and every comment (another issue of the same tracker with `key`) |
| `<tracker>.issue.comment` | `body`, `key`? | comment on the issue (`key` for another agent's issue) |
| `<tracker>.issue.list` | | the open issues, to check for one before opening another |
| `<tracker>.issue.create` | `title`, `body`, `labels`? | open a follow-up issue |
| `bitbucket.context` | | the repository, the branch and the base branch |
| `bitbucket.pr.list` | | the issue's PRs, open or not |
| `bitbucket.pr.create` | `title`, `body`, `base`?, `draft`? | open a PR from the issue's branch (it adds the line that links the issue) |
| `bitbucket.pr.view` | `number` | a PR, every comment and inline comment on it |
| `bitbucket.pr.comment` | `number`, `body` | comment on a PR's conversation |
| `bitbucket.pr.reply` | `number`, `comment_id`, `body` | reply in an inline thread |
| `bitbucket.checks` | `number`? | every commit status on the PR's head, with links and the preview |
| `introspection.correction` | `area`, `body` | if your toolbelt has it: log a correction you were given (see Notes) |
| `introspection.problem` | `kind`, `body` | if your toolbelt has it: log a problem you worked around (see Notes) |

Post every comment this way: it marks the comment as yours, so it is never handed back to you as news, and a retried call never posts twice (give the same `request_id` when you retry; a `pending` answer means call again later with the same input). Use `git` itself for commits and pushes. When your toolbelt has no `bitbucket.*` operations, the scope has no code host: do the work the issue asks and report on the issue.

Every turn runs in the issue's own workspace: a worktree of the project on the issue's branch (`agent/issue/<KEY>`), kept for the issue across turns. The project's own rules (`CLAUDE.md`, `AGENTS.md`) hold what is particular to it; where they differ from this playbook, they win. A project can keep additions to this playbook in `.open-conduit/playbooks/work-tracker-issue.md`; Open Conduit hands them to you with each turn. Follow them as well as this playbook.

## Notes

When someone corrects you, log the lesson with `introspection.correction`: `area` one of `writing`, `design`, `data`, `code` or `workflow`, and a `body` giving the broad theme in one sentence, then the rule a future agent should follow. When you work around something rather than doing the task the way your instructions say (a missing variable, a command that doesn't exist, a tool call that failed), log it with `introspection.problem`: `kind` one of `env`, `docs`, `tooling`, `service` or `other`, and a `body` saying what you tried, the exact error, your workaround and where the fix belongs. Log each distinct problem once per session. The daily sweep and triage act on them. If your toolbelt doesn't have these operations, or they refuse, the project doesn't collect notes: carry on.

## Ending a turn

Every turn ends with a status (the structured result Open Conduit asks for):

- `state`: `building` (you pushed and the checks haven't landed), `review` (something is up for the owner), `help` (you asked something you can't go on without), `done` (only a merge or close is left) or `finished` (the PR merged or closed, or the issue closed with no PR).
- `summary`: what the turn did, in one line.
- `prs`: every PR number that belongs to this issue, once there is one.
- `preview`: the deploy preview's URL, once you know it.
- `touches`: what the work changes, in a few words.
- `handoff`: a short note to whoever picks the issue up next: what is pending, the judgement calls you made and why, and what to check next.

Open Conduit sets the issue's labels and moves its workflow status from `state`; never change those yourself.

## First turn: `work-tracker-issue issue/<KEY>`

1. **Read the issue.** `<tracker>.issue.view`. Read every comment; later ones often change the ask. Decide whether it is a change (steps 2–5) or an investigation (do the research, post the findings on the issue and end with `review`).
2. **Workspace.** You are already in the issue's workspace, on its branch. If `bitbucket.pr.list` already shows an open PR, you're taking over: read it with `bitbucket.pr.view` and carry on. Post a short start comment with `<tracker>.issue.comment`.
3. **Do the work.** Follow the project's rules, run what they say must pass, commit on the issue's branch.
4. **Open the PR and end the turn.** Push the branch (`git push -u origin HEAD`), then `bitbucket.pr.create` with a `title` and a `body` saying what changed and anything you didn't do. Don't wait for the checks: end with `building` and the PR number in `prs`.
5. **Report on the issue** in the turn where the checks pass: one comment with the headline, the PR and preview links, a short paragraph on what was done and anything worth flagging. End with `review`.

## Later turns: `work-tracker-issue issue/<KEY> events`

Re-read the issue and the PR before acting, then take the events in order:

- **A comment** on the issue or the PR: make a requested change (commit, push), answer a question, or say briefly why a request can't be done. Reply where the comment was made (`<tracker>.issue.comment`, `bitbucket.pr.comment` or `bitbucket.pr.reply`).
- **Checks failed**: `bitbucket.checks` links each failing status. Fix it, push, end with `building`.
- **Checks passed**: post the report (step 5) if it isn't posted yet, else reply to the feedback you addressed; end with `review`.
- **PR merged or closed**, or **the issue closed**: end with `finished`.
- **A note or message from the owner**: do what it says.

If you pushed, end with `building`. Otherwise `help` if you asked something, `done` if nothing is left, `review` otherwise.

## Fresh session: `… handoff`

The message carries the last turn's handoff note and the events. Rebuild the picture first (the issue, every PR, the branch's commits), then take the events as in later turns. Where the note and the thread disagree, the thread wins.

## Don't

- Don't wait or poll for checks or replies: end the turn and let Open Conduit resume you.
- Don't push to the base branch, merge the PR or close the issue.
- Don't change the issue's labels or workflow status; Open Conduit sets them from your status.
- Don't post comments any way other than the toolbelt.
