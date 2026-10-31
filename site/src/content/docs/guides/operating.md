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

The web view has five pages, with the daemon's state, the active generation and the agent and job slots in use along the bottom.

- **Runs** lists one row per Work, grouped by what it needs from you: first what needs a person (a decision an extension offers, such as answering an agent's question, or a repair such as a turn that ran out of retries), then running, queued, waiting, held and closed. Queued is Work with input waiting for a turn, and says so when a concurrency limit is holding it back. Work that handed its turn back and has nothing new to act on is waiting, for its next event, not queued, so raising `settings.concurrency` never leaves it behind. Each row shows what the Work is doing now in its source's words (what a Wait is for, such as "checks on PR #137"), its lifetime as a line with a bar for each stretch of active time, and its turns. The state strip, search, filters, grouping and sort are all in the URL. Select rows to hold or release several at once; running Work is skipped and the result says how many.
- **A Work's page** shows what the Work is and is doing now, any panel an extension shows that offers an action (such as the open question with its options), its turns as a score with the waits between them, and one turn at a time with its transcript read as a conversation: the agent's and people's messages as chat, what the agent sent elsewhere with its text, and tool calls as quiet rows you can expand. The Events tab lists every Event with how it was routed. The facts beside it give the Work's IDs, routing, the turn's executor and workspace, and the totals.
- **Needs you** gathers what waits on a person, what stopped and needs a repair, what an operator holds, and what has sat unchanged for a while, with the changes you made in this session.
- **Configuration** draws each scope's routing as a flow chart: the sources that bring events in, the routes tried in order with what each does while its Work is busy, and the binding and executor each route hands its Work to. Pick a sample event to trace how it would route now, without sending it. It is read-only; change the configuration with `open-conduit config set` or `config edit`, or set up with `open-conduit init --with claude`.
- **Setup** runs a setup session in the browser, in a scope with a `setup` entry. A daemon started with no config file runs a default `setup` scope; otherwise `open-conduit init setup` adds one, applied as a new generation with `config validate` and `config apply` once the daemon has run (see [set it up in the browser](/open-conduit/start/quick-start/#set-it-up-in-the-browser)). Start one (choosing the model, when the binding offers more than one, and ticking any grants the binding offers for that session only: the default scope offers a shell and your own connectors), then read what the assistant says, answer its questions with a button per choice and send it messages. A message sent while it waits on a question reaches it with the answer. The assistant has no shell: it looks around through probes and proposes config changes, each with its reason. The Changes section shows each proposal's before and after, with Approve and Reject, Approve all and Validate; Apply applies the approved changes as the next generation, with the generation the page read, and shows doctor's checks afterwards. Nothing in a session changes the configuration until you apply. `open-conduit actions run setup start --scope setup --input "MODEL +shell"` starts a session from a terminal. The conversation follows the newest message while you are at the bottom, and the message box takes several lines (Shift+Enter). End a session by cancelling it. Without a setup scope, the page points to `open-conduit init --with claude` in a terminal.

Every change (run again, rebind, send a wake event, hold, release, cancel a turn or the Work, and the actions extensions offer) asks first, says what it will do, and sends the Work's current revision; the confirmation names the call and shows the event the daemon accepted. Every page works without JavaScript. With it, `⌘K` jumps to any Work, scope or page, `g` then `r`, `a`, `c` or `s` goes to a page, `/` searches, `j`/`k` move through rows or turns, `x` selects, `e` jumps to the first error in a transcript, and `?` lists the keys. To attach to an agent's session, use the terminal.

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
open-conduit route --samples --scope eng             # sample Events to trace
open-conduit route --trace issue-opened --scope eng  # how a new issue would be routed now (records nothing)
open-conduit events chain ID
```

- `work retry|hold|release|cancel` take the Work's current `--revision`, which `work get ID` shows.
- `runs log ID` shows a run's output; `--transcript` shows the agent's transcript.
- `events chain ID` shows the chain of Events that led to something.
- `doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs.
