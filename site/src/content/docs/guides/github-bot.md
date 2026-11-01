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

Git is separate. By default:

- **An unsandboxed run or a process-sandboxed run** pushes with your own git credentials, and commits carry your `user.name` and `user.email`. Only the PR is opened by the bot.
- **A container run** has no credential of its own. When it pushes, Open Conduit mints a short-lived token from the bot App for that one repository ([git credentials](../sandboxes/#git-credentials)). Its commits still carry the name and email the container's git has.

To have git work as the bot too, see [push and commit as the bot](#push-and-commit-as-the-bot).

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

## Push and commit as the bot

You can also have git work as the bot. Set `git` to `bot` on the GitHub connection that has the bot App (here `github`). Each binding with a git workspace also needs `github.git.credential` in its `operations` and in the scope's `authorization.operations`. `open-conduit init github` and `open-conduit migrate` grant it to the bindings they write with a workspace; add it yourself to a binding you wrote by hand:

```sh
open-conduit config set /connections/github/config/git '"bot"'
```

Every run of Work on that connection's repositories, unsandboxed, in a process sandbox or in a container, then works like this:

- **Pushes come from the bot.** When git needs a credential, Open Conduit mints a token from the bot App. The token works only for the Work's one repository, with `contents: write`, and Open Conduit revokes it when the run ends. Your own credential helpers, such as the macOS keychain or `gh`, aren't asked.
- **Commits come from the bot.** The author and committer are `your-app[bot]` with the bot's `users.noreply.github.com` address, so GitHub shows the bot's name and avatar on them.
- **Commits are unsigned.** Signing is turned off for these runs, because your signing key isn't the bot's.

Agents don't need to know about any of this. They run `git commit` and `git push` as usual.

### Choose per binding

A binding whose `connection` is another GitHub connection, such as [a second App](#a-second-app-for-a-reviewing-agent), takes git from that connection instead of `github`. Its connection's `git` decides: with `bot`, the binding's runs push and commit as that connection's App, with tokens that App mints; without it, git stays yours, even when `github` sets `bot`. Other bindings in the scope keep following `github`. For example, to have a binding push as `github-builder` while the rest of the scope works as you:

```sh
open-conduit config set /connections/github-builder/config/git '"bot"'
```

and give that binding `"connection": "github-builder"`. The App needs **Contents** read and write to push, and the binding still needs `github.git.credential`. A binding connection on another GitHub than the repository's stops the binding's runs from starting (`run environment unavailable`), rather than letting them push as `github`.

`open-conduit config check` refuses `git` `bot` on a connection that signs in with a token and has no `bot_app_id`, because there is no App to mint from. Leave `git` out, or set it to `owner`, to keep git as yourself.

A few things to know:

- **Use an HTTPS remote.** Only `https://` remotes go through Open Conduit's credential helper. An `ssh://` or `git@github.com:` remote still pushes with your own SSH key. `open-conduit doctor` warns about this in its `git_bot` check. Change the remote with `git remote set-url origin https://github.com/acme/web.git` in the checkout.
- **Grant `github.git.credential`.** Without it, the push is refused and git has no other helper to fall back on. `open-conduit doctor` warns about a git workspace binding that isn't granted it, in its `git_bot` check.
- **A run that can't get the bot's identity doesn't start.** It fails as `run environment unavailable`, for example when GitHub can't be reached or the bot App's key is wrong.
- **Without a sandbox, your credentials are still there.** An unsandboxed run uses the bot's credentials by default, but it runs as you. An agent that goes looking can still find your SSH key, your keychain or `gh`'s token. To keep it to the bot's access, run the binding in a [sandbox](../sandboxes/).

## A second App for a reviewing agent

An agent that reviews another agent's PR writes as the same bot, so GitHub sees the PR's author reviewing it and accepts only comments. To let it approve or request changes, give the reviewing binding its own App.

1. Create and install a second App as in steps 1 to 3, for example `acme-reviewer`. It needs **Issues** and **Pull requests** read and write. Store its key, for example as `github-reviewer-key`.
2. Add it as its own connection, beside `github`, with the same `api_url` and sign-in, and the reviewer App as its bot:

   ```json
   {
     "id": "github-reviewer",
     "type": "github",
     "config": {"bot_app_id": "654321"},
     "secret_refs": {"token": "cmd:gh auth token", "bot_private_key": "keyring:github-reviewer-key"}
   }
   ```

3. Add `github-reviewer` to the scope's `authorization.connections`, and make it the reviewing binding's connection:

   ```json
   "connection": "github-reviewer"
   ```

Use `open-conduit config edit` for all three, then `config check`, and apply the file as in step 4. `config check` refuses a connection that isn't granted to the scope. Keep the same `api_url` as the `github` connection: a reviewer connection on another GitHub is refused when the binding first writes, and `doctor --check-secrets` reports it.

That binding's comments, replies and reviews now show as `acme-reviewer[bot]`, and its git follows `github-reviewer`'s `git` ([choose per binding](#choose-per-binding)). Everything else stays on the `github` connection: Open Conduit still reads and claims each issue and PR once, sets the labels as the first bot, and the builder's PRs are still opened by it. Other bindings are unchanged. `doctor --check-secrets` lists it under each repository's entry, as `binding connection github-reviewer: bot writer authenticated`.

Which binding reviews which PR is up to your routes and playbooks; the writer only changes whose name is on the review.

## What changes for you

- **You can approve agents' PRs.** The bot opened them, so a review from you counts towards branch protection.
- **The bot can't approve its own PRs.** GitHub never lets an author approve their own PR. An agent whose binding has no GitHub connection of its own, asked to review a PR another agent opened, can only comment on it. Give the reviewing binding [a second App](#a-second-app-for-a-reviewing-agent) and it can approve and request changes too.
- **A reviewer App's approval may not be enough.** It counts towards a required number of approvals when the App can write to the repository. An App can't be a code owner, though, so where branch protection requires a code owner's review, or a particular person's, you still have to approve.
- **Agents still recognise each other.** Open Conduit marks each agent comment, so a comment from another agent wakes the agent it was meant for, whoever posted it.
- **Comments from before stay yours.** Only writes made after the change come from the bot.
