---
title: Point your agent here
description: A setup guide written for coding agents. Give your own agent this page and it installs and sets up Open Conduit with you.
---

If you already use a coding agent (Claude Code, Codex, Cursor or another), let it do the setup. Paste this into it:

```text
Install and set up Open Conduit for me by following
https://open-conduit.github.io/open-conduit/start/agents/
```

It installs Open Conduit, looks at your checkouts, asks you a few questions, writes the config and checks it. You still store credentials and start the daemon yourself.

The rest of this page is for the agent.

## For the agent

You're setting up Open Conduit for the person you're working with. It's a local daemon that watches GitHub repositories (and Slack, Linear or Jira), claims the issues they allow, and has a Claude Code agent work each one on its own branch, returning to it whenever a comment, a check result or a merge arrives. Do these steps in order, and tell the owner what you're doing as you go.

### 1. Check what's there

- `command -v open-conduit`: if it's already installed, skip to step 3. If a config already exists (`open-conduit config path` prints where), build on it and never replace it.
- `command -v claude`: Open Conduit's agents run on Claude Code, even if you are a different agent. It must be installed and signed in on this machine. If it isn't, tell the owner and point them at [Claude Code's docs](https://docs.anthropic.com/en/docs/claude-code).
- `command -v gh`: optional. A signed-in GitHub CLI is the simplest GitHub credential. If it's missing, mention it; the owner can install it or use another credential later.

### 2. Install

Ask the owner which method to use. Suggest the first one that fits:

- **Homebrew** (macOS or Linux with `brew`): `brew install open-conduit/tap/open-conduit`
- **Install script** (no Homebrew): `curl -fsSL https://raw.githubusercontent.com/open-conduit/open-conduit/main/install.sh | sh`. It verifies the release checksum and links `open-conduit` into `~/.local/bin`. Check that's on their `PATH`.
- **Go 1.24.2 or later**: `go install` both `github.com/open-conduit/open-conduit/cmd/open-conduit@latest` and `.../cmd/os@latest`. They must end up in the same directory.

[Install](/open-conduit/start/install/) has the details of each. Then confirm it runs with `open-conduit --help`.

### 3. Follow the setup guide

Fetch the setup guide and follow it as your instructions for the rest of the session:

**[/open-conduit/setup-guide.md](/open-conduit/setup-guide.md)** (plain Markdown: `https://open-conduit.github.io/open-conduit/setup-guide.md`)

It's the same guide `open-conduit init --with claude` gives Claude Code. It covers what to look at, the questions to ask, the config to write and how to check it. Where it says "the agent running you", read that as Claude Code: that's the agent Open Conduit runs, whichever agent you are.

The rules that matter most, from that guide:

- **Secrets never pass through you.** Don't ask for a token, put one in the config or on a command line, or echo one. Credentials go in the config as references such as `cmd:gh auth token`. For a stored secret, the owner runs `open-conduit secret set NAME` in their own terminal.
- **Ask, don't assume.** Confirm which repositories it watches, how issues map to scopes, and anything that labels or comments on issues.
- **Edit the config only with `open-conduit config` or `open-conduit init`.** Both validate before they save. Check the result with `open-conduit config check` and `open-conduit doctor`.
- **Don't start the daemon yourself.** End by giving the owner the commands: `open-conduit serve --dry-run` to watch safely first, then `open-conduit service install` to run it in the background.

### If you are Claude Code

You can follow this page as it is. If the owner would rather start a fresh session with the guide preloaded and `open-conduit config` and `doctor` pre-allowed, they can run `open-conduit init --with claude` themselves instead.
