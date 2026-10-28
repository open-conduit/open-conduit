---
title: Watch and steer runs
description: Use the terminal UI, the web view and the command line to watch, claim, retry, hold and cancel work.
---

## The terminal UI

```sh
open-conduit tui
```

The TUI lists one row per issue or thread, with its latest turn and how many turns it has had. `g` lists every turn, and `enter` shows one with all of its Work's turns.

| Key | Does |
| --- | --- |
| `c` | the scopes' actions, such as claiming an issue by hand |
| `1`–`9` | in a Work's detail, the actions its extensions offer now |
| `R` | run or retry |
| `H` | hold or release |
| `a` | attach to the agent's session |
| `x` / `X` | cancel a run, or its whole Work |

## The web view

```sh
open-conduit web
```

This serves the same view in a browser on this machine, at `http://open-conduit.localhost:7420`. It prints a sign-in URL; open it to start a session in that browser. Browsers resolve every `*.localhost` name to this machine without any setup. If yours doesn't, use the second URL it prints, which names `127.0.0.1`.

The web view lists Work grouped by what it needs from you: failed first, then running, then waiting, then finished, with the same filters as the TUI and a strip showing each Work's turns. A run's page puts any question an extension shows first, then its Work's turns in order, with the controls and the Work's context beside them. It links to the run's log, transcript and outputs, which follow a running turn live, and offers run again, hold/release and cancel, and the actions extensions offer, such as answering an agent's question. Extensions can add panels to a run's page and to Scopes, such as the open question with its options and the answer box beside it; the TUI shows the same panels in a run's detail. Scopes lists each scope's actions, such as claiming an issue by hand. Config draws the running configuration as a flow for each scope: the sources that bring events in (its ingress extensions, and you by hand), the routes tried on each event in order, and the binding and executor each route hands its Work to, with the connections they use. Work that other Work or `open-conduit work create` starts names its binding and skips the routes, so it has its own row. It is read-only; change the configuration with `open-conduit config edit`. To attach to an agent's session, use the terminal.

- `--addr HOST:PORT` (or `OPEN_CONDUIT_WEB_ADDR`) changes where it listens. Only a loopback address is accepted, so the view is never reachable from another machine.
- `--host NAME` (or `OPEN_CONDUIT_WEB_HOST`) changes the name it answers to.

The web view runs as its own process, a client of the daemon like the TUI. It holds the operator token itself and never gives it to the browser. Stop it with Ctrl-C.

## The same controls on the command line

```sh
open-conduit claim 12                                # claim issue #12 by hand
open-conduit actions list --work ID                  # what extensions offer on a Work now
open-conduit work list --state waiting
open-conduit work retry ID --revision N
open-conduit work hold ID --revision N --reason "needs a decision"
open-conduit work attach ID                          # hold, attach to the agent's session, release on exit
open-conduit runs list
open-conduit runs log ID --follow                    # follow a running turn
open-conduit route issue/12                          # explain how an issue's Events were routed
open-conduit events chain ID
```

- `work retry|hold|release|cancel` take the Work's current `--revision`, which `work get ID` shows.
- `runs log ID` shows a run's output; `--transcript` shows the agent's transcript.
- `events chain ID` shows the chain of Events that led to something.
- `doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs.
