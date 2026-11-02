---
title: Pull request review
description: Review every pull request with an agent that writes as its own GitHub App, using the open review bot's playbook.
---

Open Conduit can review each pull request in a repository: an agent reads the PR, posts one review with inline comments, waits until something changes, and reviews again until the PR merges or closes. The reviewer is ordinary config plus a playbook; there is no bot code to run.

The easiest way to get one is the [review bot](https://github.com/open-conduit/internal-review-bot), the reviewer Open Conduit uses on its own repositories. Its README covers every step; this page says what you are setting up.

## What it is

The review bot is a folder holding two things:

- **A playbook.** `plugin/skills/review-pr/` holds the `review-pr` playbook, how the reviewer reads a PR, when it approves and how it follows up, with the review bar it applies (`guidelines.md`) and the architecture rules it checks against (`principles.md`). Change those files to change how it reviews; a repository can add to the playbook in `.open-conduit/playbooks/review-pr.md`.
- **A config.** `.open-conduit/config.json` holds one scope per repository it reviews. Each scope has a `github` entry that tracks the repository's PRs, a route that claims them and a `reviewer` binding that may read, review and reply but never push, merge or open PRs.

## Set it up

1. **Get the bot.** Fork or clone the repository somewhere the daemon's user owns, such as `~/review-bot`. Open Conduit reads it in place, so your edits are your bot.
2. **Point it at your repositories.** In its `.open-conduit/config.json`, give each repository you want reviewed a scope, with the repository's name and the path of a local clone.
3. **Give it its own GitHub App**, so its approvals and change requests count as a reviewer's rather than the PR author's. See [a second App for a reviewing agent](../../guides/github-bot/#a-second-app-for-a-reviewing-agent).
4. **Include the folder in your config** beneath a scope that grants the reviewer's connection and operations, then check and apply:

   ```sh
   open-conduit config include add ~/review-bot --parent review-bot
   open-conduit config check
   ```

## How the playbook and routes fit

Two pieces of the folder's config do the work.

The binding runs the `review-pr` playbook from the folder's `plugin` directory. `plugin_dir` is resolved against the included folder, so the playbook goes wherever the folder goes:

```json
"config": {"playbook": "review-pr", "plugin_dir": "plugin"}
```

The route claims each newly opened pull request as Work of its own and sends it to the reviewer. Add `"drafts": true` to its config to review drafts too; otherwise a draft is reviewed once it is marked ready.

```json
{"name": "pull-requests", "handler": "github", "binding": "reviewer", "active_policy": "queue", "config": {"claim": "pull_requests"}}
```

The repository's `github` entry sets `"track_pull_requests": true`, so each PR's news reaches the reviewer under the PR's own key. If an agent of yours is also working the issue a PR belongs to, the two hear each other's comments and pushes and can go back and forth until the PR is ready.

To add a reviewer to a scope you already have instead of including the bot's folder, follow [a review bot for every PR](../../guides/github-bot/#a-review-bot-for-every-pr).
