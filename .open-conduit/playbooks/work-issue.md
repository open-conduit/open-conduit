# How this project works its issues

## Start

Once you have read the issue, post a short start comment with `github.issue.comment`, for example "Working on this on branch `agent/issue-12`. A PR will follow here." For an issue that asks for research, options or an answer, say findings will follow instead.

## Kinds of issue

- **Change:** something is to be built or fixed. Open a PR.
- **Investigation:** the issue asks for research, options or an answer ("research X", "which provider…", "check whether…"). You don't need a PR: post the findings on the issue and end with `review`. If the findings lead to an obvious change the issue asked for, you may make it as a change as well.

## The update and the report

The update you post once the PR is open takes the same shape as the report below, without the preview: the author's mention, `- PR: <url>`, a short paragraph on what was done, and each thing they need to act on with a bold lead. The report, once the checks pass, then says the PR is ready, gives the preview, and mentions only what changed since the update.

When a checks event carries a preview URL, add a preview link to a reply to feedback when it helps.

Keep the report on the issue brief; the PR has the detail. If there is a preview, open it once to check the changed page loads. Then post:

- a mention of the issue's author and the headline of what now exists, in one line
- `- PR: <url>` and `- Preview: <preview url>`, deep-linked to the changed page when there is one
- a short paragraph on what was done
- anything worth flagging (what you couldn't do, data that doesn't exist, a judgement call they might want to reverse), each with a bold lead

## Other agents

When another issue overlaps yours (the same page, component, package or pipeline step), read it and its PR before you build. Build on its branch or wait for its PR to merge rather than making the same change twice.

Comment on another agent's issue rarely, unless you are supervising its work. Do it when your work turns up something that helps its work in progress, or when your work needs it to take an unusual step it wouldn't otherwise see. Don't comment just because you work in the same file, because one of you will need to rebase, or about anything that only depends on who merges first.

You don't need to answer comments from other agents. Act on what they tell you, and reply only in exceptional cases, such as when you disagree outright on the way forward. When you tell the owner what another agent is doing, report only what you've seen happen (its reply, its issue's state), not what you asked it to do.
