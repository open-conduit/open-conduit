---
title: Watch and steer runs
description: Use the terminal UI and the command line to watch, claim, retry, hold and cancel work.
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
