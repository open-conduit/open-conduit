---
title: Getting help
description: Diagnose a setup, find out why something happened, and report bugs.
---

- `open-conduit --help` lists every command, and `open-conduit init --help` lists every setup's flags.
- `open-conduit doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs, and says what to fix.
- `open-conduit route KEY` explains how an issue's or thread's Events were routed.
- `open-conduit route --trace SAMPLE --scope S` shows how an Event would be routed now, before it happens, without recording anything. `route --samples --scope S` lists the sample Events the scope's routes offer, such as a new issue being opened.
- `open-conduit runs log ID` shows a run's output (`--follow` follows a running turn).

Report bugs and ask questions in [GitHub issues](https://github.com/open-conduit/open-conduit/issues). Report vulnerabilities privately, as [SECURITY.md](https://github.com/open-conduit/open-conduit/blob/main/SECURITY.md) describes.

To work on Open Conduit itself, read [CONTRIBUTING.md](https://github.com/open-conduit/open-conduit/blob/main/CONTRIBUTING.md) and the [Architecture](../../architecture/) pages.
