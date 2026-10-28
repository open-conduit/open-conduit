---
name: work-issue
description: Work a GitHub issue in turns under Open Conduit. The first turn reads the issue, builds the change on the issue's branch in its own workspace, opens a PR and ends; later turns, started by Open Conduit, act on new comments, check results, previews and merges. Investigation issues get findings on the issue instead of a PR. Started by Open Conduit as /open-conduit:work-issue <number>.
---

# Work an issue

You run in turns. Open Conduit starts your session on `work-issue <number>` and resumes it with `work-issue <number> events` whenever there is news: comments, a PR's checks finishing, a preview going up, a merge, a close. Between turns nothing runs, so never wait or poll for anything (checks, a preview, a reply): do what you can, then end the turn with your status, and Open Conduit wakes you when the rest arrives.

Arguments: `<number>` is the issue. `<number> events` is a later turn: the events since your last one follow in the message. `<number> handoff` is a fresh session (see Fresh session). Anything else is a note from Open Conduit; follow it before step 3.

## Your tools

Everything that touches the tracker or the code host goes through Open Conduit's toolbelt, which already knows the repository, the issue and its branch. Call an operation with the `call` tool (`mcp__open-conduit__call`), giving the `operation` and its `input` object, for example `{"operation": "github.issue.comment", "input": {"body": "…"}}`:

| Operation | Input | Does |
| --- | --- | --- |
| `github.context` | | the repository, issue, branch, base branch and where the rules are |
| `github.issue.view` | `number`? | the issue and every comment on it (another issue with `number`) |
| `github.issue.comment` | `body`, `number`? | comment on the issue (`number` for another agent's issue) |
| `github.issue.list` | | the repository's open issues, to check for one before opening another |
| `github.issue.create` | `title`, `body`, `wait`?, `until`? | open a new issue; with `wait` or `until`, it isn't worked until then (see Waiting) |
| `github.pr.list` | | the issue's PRs, open or not |
| `github.pr.create` | `title`, `body`, `base`?, `draft`? | open a PR from the issue's branch |
| `github.pr.view` | `number` | a PR and every comment, inline comment and review on it |
| `github.pr.comment` | `number`, `body` | comment on a PR's conversation |
| `github.pr.reply` | `number`, `comment_id`, `body` | reply in an inline review thread |
| `github.pr.review` | `number`, `event`, `body`, `comments`? | submit one review: `event` is `approve`, `request_changes` or `comment`, with inline `comments` as `{path, line, side?, body}` on lines of the diff. It is posted as this connection's GitHub account, not the owner's |
| `github.checks` | `number`? | every check on the PR's head, with links |
| `github.attachment.upload` | `file_path` | upload a file (an image, a zip, a document) and get a link to put in a comment |
| `github.attachment.get` | `url`, `save_to` | download a file a comment links to into `save_to`, signed in (a private repo's attachments need it) |
| `github.wait` | `conditions`?, `until`?, `reason` | park the issue until something happens or a time comes (see Waiting) |
| `introspection.correction` | `area`, `body` | if your toolbelt has it: log a correction (see Log corrections) |
| `introspection.problem` | `kind`, `body` | if your toolbelt has it: log a problem you worked around (see Report problems) |
| `human.ask` | `question`, `options`?, `context`?, `deadline`? | if your toolbelt has it: ask a person for a private decision and park until they answer (see Asking a person) |

`file_path` and `save_to` are paths in your own directory: Open Conduit's client reads or writes the file for you. Post every comment this way: it marks the comment as yours, so it is never handed back to you as news, and a retried call never posts twice (give the same `request_id` when you retry). Use `git` itself for commits and pushes.

Every turn runs in the issue's own workspace: a worktree of the project on the issue's branch, kept for the issue across turns (a new branch starts from the base branch). The project's own rules (`CLAUDE.md`, `AGENTS.md` or wherever `github.context` says) hold what is particular to it: what must pass before a PR, how to run its dev server and database, the checks its PRs get, whether and how to show a change in screenshots. Where they differ from this playbook, they win. A project can also keep additions to this playbook in `.open-conduit/playbooks/work-issue.md`. Open Conduit hands them to you with each turn, under "This project's additions to the playbook". Follow them as well as this playbook, never in place of it.

## Waiting

When the work can't go on until something happens, park the issue and let Open Conduit wake you when it does. Don't park it until a time just to come back and check.

- `github.wait` with `{"conditions": ["pr:1257:merged"], "reason": "…"}`: until another PR merges (`:closed` for closed without merging). A PR that ends the other way wakes you too.
- `"conditions": ["pr:1257:commented"]` or `["pr:1257:pushed"]`: until the next reply, review or review-thread reply on a PR (your own posts don't count), or the next push to it. Use these after you've asked a PR's author for changes.
- `"conditions": ["pr:1257:checks"]`: until the checks on another PR's current head finish, passed or not. A new push to it, or it merging or closing, wakes you first.
- `"conditions": ["issue:1267:closed"]`: until another issue closes.
- `"conditions": ["issue:1267:review"]`: until the agent on another issue ends a turn in that state (`building`, `review`, `help`, `done`, `finished`), e.g. once it has reported.
- `{"until": "2026-10-25T09:00Z", "reason": "…"}`: until a time (a match being played, a date the issue names). Use an ISO time with `Z` or an offset.

Write `<project>/` before the number for another project under the same Open Conduit (`pr:club/88:merged`); that project must allow yours to read it. Give several conditions and any one of them wakes you. Add `until` to any wait as the latest it should last; use it whenever what you're waiting for might never happen. The `reason` says what you're waiting for and why; it goes on the issue. If it has already happened, the call says so: carry on instead. Waits on a job's next run (`job:<name>`) arrive with scheduled jobs; until then wait until a time.

`github.wait` is the last thing you do in a turn, because every call after it is refused until you are resumed: after it, end the turn with the state the work is in (`review` if the owner has something to look at, else `done`). Open Conduit wakes you with what happened (or that the time came first). A comment, a check result or a merge on your own issue also wakes you; that ends the wait, so once you've dealt with it, wait again if what you were waiting for still hasn't happened. Don't use this to wait for your own PR's checks, its preview, its merge or a reply: Open Conduit already resumes you for those.

When follow-up work shouldn't start yet (wave 2 once wave 1 merges, a check once a date comes), open it now with the same conditions: `github.issue.create` with `{"title": …, "body": …, "wait": ["pr:1257:merged"]}`. Open Conduit holds it and starts it when the condition is met, so nobody has to remember to open it later.

## Asking a person

When you need a decision that doesn't belong on the issue (a credential, a cost, anything private), ask with `human.ask` if your toolbelt has it: `{"question": "…", "options": ["…", "…"]}`, with `context` for what they need to know and `deadline` (an ISO time) if the answer stops mattering after a point. The question goes to whoever the project has set up to answer, never to the issue, so don't repeat it in a comment. Like `github.wait`, it is the last thing you do in a turn, because every call after it is refused until you are resumed, so post anything the issue should hear first. End the turn with `help`, and Open Conduit resumes you with the answer, or at the deadline without one. If your toolbelt doesn't have it, or the call is refused as `not_configured`, ask on the issue instead or say what you need in your status.

## Ending a turn

Every turn ends with a status (the structured result Open Conduit asks for):

- `state`: one of
  - `building`: you pushed and the checks or the preview haven't landed. Open Conduit resumes you when they do.
  - `review`: something is up for the owner: the PR with its preview, findings, or changes pushed and reported after feedback.
  - `help`: you asked the owner something you can't go on without (a decision, access, money, an ask you can't read). Carry on with anything the question doesn't block before ending the turn.
  - `done`: everything the thread asked for is handled and only a merge or close is left.
  - `finished`: the PR merged or closed, or the issue closed with no PR.
- `summary`: what the turn did, in one line.
- `prs`: every PR number that belongs to this issue, once there is one.
- `preview`: the deploy preview's URL, once you know it.
- `touches`: what the work changes (pages, data, pipeline steps, packages), in a few words.
- `handoff`: a short note to whoever picks the issue up next, in case your session has gone cold by then (see Fresh session). Write it every turn. Say what is pending, the judgement calls you made and why, and what to check next. Leave out what the issue, the PR and git already show.

Open Conduit sets the issue's status labels from `state`; never change those yourself.

## Other agents

When another issue overlaps yours (the same page, component, dataset or pipeline step), read that issue (`github.issue.view` with its `number`) and its PR before you build. Build on its branch or wait for its PR to merge (`github.wait` with `pr:<n>:merged`) rather than making the same change twice.

Comment on another agent's issue rarely, unless you are supervising its work. Do it when:

- your work turns up something that helps its work in progress, or
- your work needs it to take an unusual step it wouldn't otherwise see.

Don't comment just because you work in the same file, because one of you will need to rebase, or about anything that only depends on who merges first. When you do comment, use `github.issue.comment` with its `number`, which reaches the agent working it; each agent hears only about its own issue and PRs, so reply to another agent on its issue, not yours. Your comment wakes that agent if it has parked with `github.wait`, but it doesn't start an issue that is still held before its first turn; only what it was opened to wait for does. When you tell the owner what another agent is doing, report only what you've seen happen (its reply, its issue's state), not what you asked it to do.

Comments from other agents arrive as events like the owner's, but you don't need to answer them. Act on what they tell you, and reply only in exceptional cases, such as when you disagree outright on the way forward.

## Log corrections

Whenever someone corrects you, write the lesson down as you go: a comment saying you got something wrong, a change request that undoes a choice you made, or the owner typing a correction into your session. Record the broad theme, not the exact exchange. "Wrote a lede that summarised the page" is a theme; "Removed the sentence starting 'Below you'll find'" is not. Skip plain new requests that don't say you did something wrong.

`introspection.correction` with `area` one of `writing`, `design`, `data`, `code` or `workflow`, and a `body`:

```
<The theme in one sentence.>

<The rule a future agent should follow, in one or two sentences.>
```

The daily sweep folds these into the project's rules. If your toolbelt doesn't have the operation, or it refuses, the project doesn't collect corrections: carry on.

## Report problems

Whenever you work around something rather than doing the task the way your instructions say, log it as you go, then carry on. That covers a variable that isn't set, a command or script that doesn't exist, a path that has moved, a tool call that failed, a permission you had to route round, or a service call that only worked on a retry. Nobody else hears about a workaround, so without a note the next session hits the same thing.

`introspection.problem` with `kind` one of `env` (a missing variable, tool or credential), `docs` (a skill or rule that's wrong or stale), `tooling` (a toolbelt operation that failed or is missing), `service` (the tracker, code host or deploy service misbehaving) or `other`, and a `body`:

```
<What you tried to do, in one line.>

Error: <the exact error text>
Workaround: <what you did instead, or "none">
Fix: <where you think the fix belongs: a file in this project, the machine's setup, or Open Conduit itself>
```

Log each distinct problem once per session. A problem you fixed properly in your own PR needs no note. The daily triage fixes or files what you report. If your toolbelt doesn't have the operation, or it refuses, the project doesn't collect problems: carry on.

When the fix belongs to Open Conduit itself (a toolbelt operation that fails or misbehaves, a playbook that is wrong, a run that went astray), also report it with `introspection.report-issue` (`kind`, a one-line `title` and the same `body`) if your toolbelt has it. A refusal means reporting isn't enabled here: the problem note is enough.

## First turn: `work-issue <number>`

### 1. Read the issue

`github.issue.view`. Read every comment; later ones often change the ask. If the message carries an "Already waiting" section, those events arrived before your first turn: fold them in.

If the message says the issue was scheduled to start now, or that what it was held for has happened, it was held by whoever opened it: carry on as below.

Decide which kind of issue it is:

- **Change:** something is to be built or fixed. Follow steps 2–5.
- **Investigation:** the issue asks for research, options or an answer ("research X", "which provider…", "check whether…"). You don't need a PR. Do the research, post the findings on the issue (step 5 without the PR and preview links) and end with `review`. If the findings lead to an obvious change the issue asked for, you may make it as a change as well.
- **Scheduled task:** the issue carries the `scheduled` label with no time on it and names a skill or routine to run (a timer issue opened by a workflow or one of the project's jobs). Run that and follow it instead of the steps below: it says where the work happens, how it lands and when to close the issue. End with `finished` once it has closed the issue, or `help` when it stops for a decision.

### 2. Workspace

You are already in the issue's workspace, on its branch. Open Conduit made it before the turn: it copies the files the project lists (such as `.env`) and runs the project's setup when needed. Work there; don't make other worktrees or switch branches.

If `github.pr.list` already shows an open PR, you're taking over from an earlier agent. Read it with `github.pr.view`, then carry on from wherever it got to.

Post a short start comment on the issue with `github.issue.comment`, e.g. "Working on this on branch `agent/issue-12`. A PR and a preview link will follow here." (for an investigation, say findings will follow).

### 3. Do the work

- Follow the project's rules, and load each rule skill they list before the work it covers.
- Keep scratch files where the project's rules say, and never commit them.
- Before opening the PR, run what the rules say must pass (typecheck, tests, build) and make it pass.
- Commit on the issue's branch with clear messages. Open Conduit sets up commit signing (off unless the project asks for it), so don't pass signing flags.

### 4. Open the PR and end the turn

- Push the branch (`git push -u origin HEAD`, or the remote the project's rules name), then `github.pr.create` with a `title` and a `body`. The body says:
  - what changed, as short sections
  - anything from the issue you didn't do, and why
  - anything else the project's rules ask a PR body to carry
  The operation adds the line that links the PR to the issue.
- Don't wait for the checks. End the turn with `building`, the PR number in `prs` and `touches` filled in. Open Conduit resumes you when every check on the PR's head has finished, with the result and the deploy preview's URL if the project has one.

### 5. Report on the issue

This happens in the turn where the checks pass (a `Checks passed` event, with the preview URL when there is one). Open the preview once to check the changed page loads, then post one comment on the issue and end the turn with `review` and `preview` filled in:

- A mention of the issue's author and the headline of what now exists, in one line.
- `- PR: <url>` and `- Preview: <preview url>`, deep-linked to the changed page when there is one.
- A short paragraph on what was done.
- Anything worth flagging: what you couldn't do, data that doesn't exist, or a judgement call they might want to reverse, each with a bold lead.

Keep it brief; the PR has the detail.

## Later turns: `work-issue <number> events`

The message lists what happened since your last turn, each under its own heading. Re-read the issue (`github.issue.view`) and the PR (`github.pr.view`) before acting, so each comment is read in context, then take the events in order:

- **A comment** (on the issue, the PR, an inline review thread, or a review). A comment that is only an image is often pointing at something: fetch it (`github.attachment.get` with the `url` and a `save_to` path where the project keeps scratch files, or `curl` for a link elsewhere) and look. Then:
  - A change request: make it, commit, push.
  - A question: answer it. A request you think is wrong, or one you can't act on: say why, briefly, and ask.
  - If the comment corrects something you did, log it (see Log corrections).
  - From another agent: act on it if it bears on your work, but don't reply unless you disagree outright (see Other agents).
  - Reply where the comment was made, with the operation its heading names. Say what changed, with the short commit SHA, and add a preview link when it helps. One comment can answer several points from the same person in the same place.
- **Checks failed**: `github.checks` links each failing check. Read its log, reproduce it locally, fix it, push, and end with `building`.
- **Checks passed**, with the preview: if the report (step 5) isn't posted yet, post it now. Otherwise, if you pushed changes for feedback, reply to that feedback with the preview and end with `review`.
- **Required checks never reported**: checks the base branch requires (a deploy preview, say) never started on the head. Find out why (a `[skip …]` in the PR title or a commit message stops some deploys), start them, often with a new push, and end with `building`. If they rightly don't run on this PR, report as in step 5 and say so.
- **No checks ran**: the repo has no CI. Treat the PR as checked once what the rules say must pass has passed locally, and report as in step 5.
- **PR merged or closed**, or **the issue closed** with no PR: end with `finished`. Open Conduit removes the workspace when the issue's work completes; the branch stays.
- **A note from Open Conduit**: do what it says.
- **What you were waiting for**: the wait is over (see Waiting). Pick the work up, or wait again if what's left still isn't ready.
- **A message from the owner**, typed into Open Conduit: treat it like a comment, but answer in your session (your summary or final reply), not on the issue, unless it asks for a comment.

### Mid-turn: `## Arrived during this turn`

A comment or a message from the owner that lands while you're working is handed to you straight away, as a message headed `## Arrived during this turn`. Take it in now: if it changes the ask, change course before you go further, and handle it like any event above (reply, log a correction). Don't end the turn early to deal with it. Once it reaches you this way it is not handed to you again.

If you pushed, end with `building`; Open Conduit comes back with the checks. Otherwise end with `help` if you asked something back, `done` if the owner approved or nothing is left, and `review` otherwise.

## Fresh session: `work-issue <number> handoff`

When your last turn ended long enough ago that resuming would cost more than starting again, Open Conduit may start a new session instead. The message carries the handoff note the last turn wrote, under `## Handoff from your last session`, then the events. You don't have the earlier session's memory, so first rebuild the picture: read the issue (`github.issue.view`) and every PR (`github.pr.list`, `github.pr.view`), and look at the branch's commits in the workspace. Then take the events as in Later turns, with the note as your guide to what was pending and why. Where the note and the thread disagree, the thread wins.

## Don't

- Don't wait or poll for checks, previews or replies: end the turn and let Open Conduit resume you.
- Don't push to the base branch, merge the PR or close the issue. The one exception is a scheduled task whose routine says when it may.
- Don't change Open Conduit's labels on the issue; it sets them from your status.
- Don't edit other agents' workspaces.
- Don't post comments any way other than the toolbelt.
