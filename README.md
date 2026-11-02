# Open Conduit

**An agent-native orchestration platform for work of every kind.**

***Open Conduit builds itself!*** Its own issues are worked by Open Conduit agents: they claim them, open the pull requests, answer the reviews and fix the problems they run into along the way.

Work turns up in many places: an issue on GitHub, a ticket in Linear or Jira, a question in a Slack thread, a job due at 7am, a defect an agent ran into yesterday. Open Conduit picks each one up, gives it to the right agent or command, and sees it through. It waits on checks, reviews and replies, survives restarts and failures, and keeps going until the work is done. You watch and steer it all from one terminal UI.

- **One daemon, every source.** GitHub, Linear, Jira, Slack, schedules and webhooks all feed the same core. That means one history, one retry and timeout model, and one view of everything in flight.
- **Built for agents.** Each run gets a toolbelt of outcomes it can ask for, like "comment on this issue", "open a pull request" or "wait until that PR merges". Agents never deal with labels, markers or a tracker's conventions.
- **Durable by design.** Every event is recorded before anything acts on it. Work can park for hours or days and wake on the event it is waiting for, even after a restart.
- **It improves itself.** Agents report what trips them up and log the corrections they are given. A daily triage turns the reports into fixes, and a sweep folds the corrections into your project's rules.
- **Any executor you can imagine.** An Execution is just one attempt to move Work forward. Claude Code is one executor, a shell command is another, and an extension can add any other agent, model or tool. Swap them per workflow without touching the rest.
- **Runs on your machine.** It is a single local daemon that uses your own CLI and keyring logins. Nothing is hosted for you and no credential ever sits in the config.

## Why it exists

Alex was building a product with a co-founder, and the work kept arriving on WhatsApp. Every request meant copying a message into Claude Code, waiting, then copying the answer back. That got old fast.

So GitHub took the place of WhatsApp. Ideas became issues, and a few webhooks started Claude Code on each one and posted back when it was done. The webhooks grew into an orchestrator: it claimed issues, opened pull requests, waited on checks and answered review comments. Then the orchestrator started improving itself, with agents filing the problems they hit and other agents fixing them.

But the primitives were wrong. Everything was shaped around GitHub, and GitHub was never the point. What mattered was the **work**, and the **executions** that move it forward, wherever the work came from. Slack threads, scheduled jobs and other trackers kept being bolted onto a GitHub tool.

Open Conduit is the heart of that orchestrator, re-architected around the right core:

- **Work** is a durable objective, such as an issue to fix or a thread to answer.
- **Event** is an immutable record of something that happened.
- **Execution** is one attempt to advance Work, through whichever executor fits: Claude Code, a shell command, or one you plug in.
- **Timer** and **Wait** make time and suspension durable.
- **Scope** holds the configuration, routes and bindings for one area of work.

GitHub, Slack, schedules and introspection are integrations that plug in around that core, and Claude Code is just one executor among any you care to add. None of them is the core itself. And it builds itself: this README was written by an Open Conduit agent working [issue #26](https://github.com/open-conduit/open-conduit/issues/26).

## Get going

**[Read the docs at open-conduit.github.io/open-conduit](https://open-conduit.github.io/open-conduit/)**: install, a quick start, each workflow and how to operate it day to day.

The quickest way in, on macOS or Linux, is to let Claude Code set it up with you:

```sh
brew install open-conduit/tap/open-conduit
open-conduit init --with claude
```

`init --with claude` opens a Claude Code session with Open Conduit's setup guide. Claude looks at your checkouts and logins, then asks which repositories to watch, how issues map to scopes and how the agent should run. It writes the config and validates it, and checks the result with `open-conduit doctor`. It never asks for a token in the chat: credentials stay as references to your `gh` login, keyring or environment. It ends with the commands to start the daemon, which you run yourself. You need [Claude Code](https://docs.anthropic.com/en/docs/claude-code) installed and signed in.

To talk it through in the browser instead, start the daemon with no config (`open-conduit serve`), or add a setup scope to an existing one with `open-conduit init setup`, and open the Setup page of `open-conduit web`. A browser session explains Open Conduit and plans the setup with you, then gives you the commands; in this release it has no shell and can't change the config itself. See [Set it up in the browser](https://open-conduit.github.io/open-conduit/start/quick-start/#set-it-up-in-the-browser).

Prefer to do it by hand? The same setup takes four commands:

```sh
open-conduit init github --repo acme/web --checkout ~/Code/web
open-conduit service install                         # run the daemon in the background
open-conduit doctor --check-secrets                  # every connection, credential and binding
open-conduit tui                                     # watch and control runs
open-conduit web                                     # the same in a browser, on open-conduit.localhost
```

The GitHub setup runs its agents with the Claude Code executor, and the GitHub CLI's (`gh`) login is its default credential. To run something else, point a binding at a shell command or at an executor from an extension. Release binaries, an install script and `go install` are covered in [Install](https://open-conduit.github.io/open-conduit/start/install/).

Where to go next:

- [Quick start](https://open-conduit.github.io/open-conduit/start/quick-start/): set it up with Claude, in the browser or by hand, then start the daemon and check it.
- Workflows: [GitHub engineering](https://open-conduit.github.io/open-conduit/workflows/github/), [Slack support](https://open-conduit.github.io/open-conduit/workflows/slack/), [scheduled jobs](https://open-conduit.github.io/open-conduit/workflows/scheduled-jobs/) and [introspection](https://open-conduit.github.io/open-conduit/workflows/introspection/).
- [Extensions](https://open-conduit.github.io/open-conduit/reference/extensions/): add integrations and executors of your own.
- [The core in one page](https://open-conduit.github.io/open-conduit/architecture/core/): the design in depth.

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for setting up a checkout (its [Toolchain](CONTRIBUTING.md#toolchain) section lists what to install), the checks a change must pass and the rules the code follows. [The architecture section of the docs site](https://open-conduit.github.io/open-conduit/architecture/) describes how each part is built and what it guarantees, and [the docs index](docs/README.md) lists the build and check commands. AI coding agents should also follow [AGENTS.md](AGENTS.md).

Report bugs and ask questions in [GitHub issues](https://github.com/open-conduit/open-conduit/issues). Ask before starting a large change.

## Security

Report vulnerabilities privately, as [SECURITY.md](SECURITY.md) describes. Please don't open a public issue for one.

## License

Open Conduit's core (the daemon, the orchestration engine, the first-party extensions and the web UI) is released under the [Functional Source License, Version 1.1, ALv2 Future License](LICENSE) (FSL-1.1-ALv2). Each release becomes available under the Apache License 2.0 two years after it is published.

What you build on Open Conduit is yours. The extension SDKs, the public contracts they build on and the example extensions are under the MIT License: each of these directories holds its own `LICENSE` file, and that file applies to it instead of the FSL.

You are welcome to use Open Conduit for your own work, including commercial work: run it inside your company, build and sell extensions and applications on it, or offer professional services around it. The one thing the FSL doesn't allow is offering Open Conduit, or something substantially like it, to others as a competing commercial product or hosted service. A commercial licence for that is available separately; ask in a [GitHub issue](https://github.com/open-conduit/open-conduit/issues).

[Licensing](https://open-conduit.github.io/open-conduit/reference/licensing/) on the docs site lists which directories are under which licence.
