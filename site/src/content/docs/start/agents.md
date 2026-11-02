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

You're setting up Open Conduit for the person you're working with. It's a local daemon that watches GitHub repositories (and Slack, Linear or Jira), claims the issues they allow, and has a coding agent (Claude Code or the Codex CLI) work each one on its own branch, returning to it whenever a comment, a check result or a merge arrives. Do these steps in order, and tell the owner what you're doing as you go.

### 1. Check what's there

- `command -v open-conduit`: if it's already installed, skip to step 3. If a config already exists (`open-conduit config path` prints where), build on it and never replace it.
- `command -v claude` and `command -v codex`: Open Conduit's agents run on [Claude Code](https://docs.anthropic.com/en/docs/claude-code) or the [Codex CLI](https://developers.openai.com/codex/cli), whichever agent you are. One of them must be installed and signed in on this machine. If both are, ask the owner which their agents should run on; if neither is, tell the owner and point them at both. Claude Code is the default. For Codex, every agent binding names `"executor": "codex"` instead of `"claude-code"` (`init github --executor codex` writes it that way), and the rest of its config is described in [run agents on Codex](/open-conduit/guides/codex/).
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

It's the same guide `open-conduit init --with claude` gives Claude Code and `init --with codex` gives Codex. It covers what to look at, the questions to ask, the config to write and how to check it. Where it says "the agent running you", read that as the agent Open Conduit's bindings run (Claude Code or Codex, as the owner chose), whichever agent you are. Where it writes an agent binding on `claude-code` and the owner chose Codex, write it on `codex`.

The rules that matter most, from that guide:

- **Secrets never pass through you.** Don't ask for a token, put one in the config or on a command line, or echo one. Credentials go in the config as references such as `cmd:gh auth token`. For a stored secret, the owner runs `open-conduit secret set NAME` in their own terminal.
- **Ask, don't assume.** Confirm which repositories it watches, how issues map to scopes, and anything that labels or comments on issues.
- **Edit the config only with `open-conduit config` or `open-conduit init`.** Both validate before they save. Check the result with `open-conduit config check` and `open-conduit doctor`.
- **Don't start the daemon yourself.** End by giving the owner the commands: `open-conduit serve --dry-run` to watch safely first, then `open-conduit service install` to run it in the background.

### If you are Claude Code or Codex

You can follow this page as it is. If the owner would rather start a fresh session with the guide preloaded, they can run `open-conduit init --with claude` (with `open-conduit config` and `doctor` pre-allowed) or `open-conduit init --with codex` themselves instead.
