---
title: Give agents their own GitHub account
description: Create a GitHub App for your agents, so their comments, labels, PRs and reviews show as the bot rather than as you.
---

A GitHub connection signed in with your token, such as `cmd:gh auth token`, writes as you. Every comment, label, PR and review an agent makes then looks like your own, and you can't approve a PR an agent opened, because GitHub sees it as yours.

A bot App fixes that. Add one to the connection and Open Conduit makes every write as the App, shown as `your-app[bot]`. Reads stay on your existing sign-in, so nothing else in the config changes, and agents work exactly as before.

## What the App does

The bot App does these things, shown on GitHub as the App:

- Opens issues and PRs, and comments on them.
- Sets the status labels.
- Submits reviews and replies in review threads.
- Uploads attachments.

Git is separate:

- **An unsandboxed run or a process-sandboxed run** pushes with your own git credentials, and commits carry your `user.name` and `user.email`. Only the PR is opened by the bot.
- **A container run** has no credential of its own. When it pushes, Open Conduit mints a short-lived token from the bot App for that one repository ([git credentials](../sandboxes/#git-credentials)).

## 1. Create the App

On GitHub, open **Settings → Developer settings → GitHub Apps → New GitHub App**. Use your organization's settings for an organization's repositories, or your own for personal ones. Then fill in the form:

- **GitHub App name:** the name people will see, followed by `[bot]`, for example `acme-agents`.
- **Homepage URL:** anything, such as the repository's URL.
- **Webhook:** clear **Active**. Open Conduit gets events from the repository's own webhook, or by reconciling, not from the App.
- **Repository permissions:**
  - **Contents:** Read and write
  - **Issues:** Read and write
  - **Pull requests:** Read and write
  - **Metadata:** Read-only (GitHub sets this one itself)
- **Where can this GitHub App be installed?** Only on this account.

Create the App and note its **App ID**, which is a number near the top of its page. Then, under **Private keys**, choose **Generate a private key**. Your browser downloads a `.pem` file.

## 2. Install it

On the App's page, choose **Install App**, pick the account and select only the repositories Open Conduit works on. Open Conduit can't write to a repository the App isn't installed on, and doctor reports it.

## 3. Store the private key

Pipe the downloaded file into `secret set`, then delete the download:

```sh
open-conduit secret set github-bot-key < ~/Downloads/acme-agents.*.private-key.pem
rm ~/Downloads/acme-agents.*.private-key.pem
```

The command prints the reference to use, such as `keyring:github-bot-key`. The key is a file of several lines, so pipe it in rather than pasting it at the prompt; a pasted value keeps only its first line. A key kept somewhere else works too. For a password manager, use a `cmd:` reference such as `cmd:op read op://Private/acme-agents/private-key`.

## 4. Add the App to the connection

```sh
open-conduit config set /connections/github/config/bot_app_id '"123456"'
open-conduit config set /connections/github/secret_refs/bot_private_key '"keyring:github-bot-key"'
```

Use your App ID and the reference `secret set` printed. If the daemon has run before, apply the file as the next generation. `config validate` prints the current generation:

```sh
open-conduit config validate "$(open-conduit config path)"
open-conduit config apply "$(open-conduit config path)" --generation N
```

## 5. Check it

```sh
open-conduit doctor --check-secrets
```

The `github_auth` check lists each repository's entry, and each should include `bot writer authenticated`. An entry that fails instead says why:

- `credential_unavailable`: the key reference doesn't resolve, or what it holds isn't a private key.
- `repo_unreachable`: the App isn't installed on that repository.

Doctor proves that the App is installed and can read the repository. It doesn't prove the App has the permissions in step 1. A permission that is missing shows up as a refused write the first time an agent needs it, so check the list if a comment or label fails.

## What changes for you

- **You can approve agents' PRs.** The bot opened them, so a review from you counts towards branch protection.
- **The bot can't approve its own PRs.** GitHub never lets an author approve their own PR. An agent asked to review a PR another agent opened can only comment on it; approving it is for you.
- **Agents still recognise each other.** Open Conduit marks each agent comment, so a comment from another agent wakes the agent it was meant for, whoever posted it.
- **Comments from before stay yours.** Only writes made after the change come from the bot.
