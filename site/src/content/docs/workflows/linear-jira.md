---
title: Linear and Jira
description: Claim Linear or Jira issues, work each with Claude Code, and open pull requests on Bitbucket Cloud.
---

Open Conduit works Linear and Jira issues the way it works GitHub ones: it claims each open issue, starts an agent on it, follows the issue's comments and status, and resumes the agent when there is news. With Bitbucket Cloud as the code host, the agent works on its own branch, opens a pull request and hears its checks, comments and merge.

:::caution
The Linear, Jira and Bitbucket integrations are tested against fakes of each service, not real accounts. Expect rough edges, and [tell us](../../reference/help/) what you hit.
:::

## Set it up

Linear needs a personal API key (Settings → Security & access → Personal API keys); Jira needs an API token. `init` asks for the key on a terminal and stores it in the keyring, or takes a reference with `--token`.

```sh
open-conduit init linear --teams ENG --checkout ~/Code/web
open-conduit config check
```

```sh
open-conduit init jira --site https://acme.atlassian.net --projects WEB --email me@acme.com --checkout ~/Code/web
open-conduit config check
```

- `--teams` (Linear) and `--projects` (Jira) take a comma-separated list. Jira's `--jql` narrows the issues further, and `--auth bearer` uses a Data Center personal access token instead of an email and API token.
- `--bitbucket WORKSPACE/SLUG --bitbucket-username EMAIL --bitbucket-token REF` adds the Bitbucket Cloud repository the agent opens pull requests on. Without it the agent can comment on the issue and change its checkout, but has no pull request tools and hears nothing from a code host.
- `--checkout DIR` gives each issue its own worktree of that checkout, on a branch named after the issue (`agent/ENG-12`). `--no-workspace` writes a scope whose turns need no checkout.
- `open-conduit init --help` lists every flag.

The scope's agent runs the `work-tracker-issue` playbook. It refers to the tracker's and the code host's operations by whatever the toolbelt lists, and leaves your house rules (how to report, what must pass before a PR) to the repository's `CLAUDE.md` or `AGENTS.md`.

## What gets picked up

An issue is claimed when all of these hold:

- **It is open and ready.** For Linear, its status is one of the claim states, `unstarted` or `started` by default (`init linear --states` changes them). For Jira, it is in one of the projects and matches the `jql` if you set one.
- **It has no skip label.** `backlog`, `human` and `milestone` keep an issue back. The entry's `labels.skip` changes the list.
- **It has a required label, if you set one.** With `labels.require`, only issues carrying one of those labels are picked up.
- **No one else has claimed it.** An issue already labelled `agent-in-progress` belongs to another owner.

To split a team's or project's issues between scopes, give each scope's route a `match` on labels, status, title, team, project, priority, assignee, components or Jira custom fields (`fields` on the entry names the custom fields routes can match). Keep the matches from overlapping.

## How a turn goes

1. The agent starts on a newly claimed issue, does the work, pushes and opens a pull request when there is a code host, and ends its turn.
2. A comment on the issue, the pull request's checks finishing, a review comment or the merge resumes it. A comment that arrives while it works is injected into the running session.
3. The issue's labels follow the state the agent reports at the end of each turn: `ready-for-review`, `agent-needs-help` or `might-be-done`.
4. To move the issue through your workflow as well, set the entry's `transitions`, mapping each state (`claimed`, `building`, `review`, `help`, `done`, `finished`) to the status to move it to, for example `{"claimed": "In Progress", "review": "In Review"}`. A state you leave out leaves the status alone.

An agent can park its issue until a time comes with the toolbelt's `wait`. Waiting on another issue or pull request, and holding a new issue until something happens, are GitHub-only for now.

## Webhooks

Without webhooks the daemon reconciles with Linear, Jira and Bitbucket on an interval, so nothing is lost. For news within seconds, set up a [webhook listener and tunnel](../../guides/configuration/#webhooks), give the entry a `webhook_secret` and set `manage_webhook` so Open Conduit creates the hook. That needs admin rights in Linear or Jira; `open-conduit doctor` says when they are missing.

For the details of each integration, see [installed integrations](../../architecture/installed-integrations/).
