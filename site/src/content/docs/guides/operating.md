---
title: Watch and steer runs
description: Use the terminal UI, the web view and the command line to watch, claim, retry, hold and cancel work.
---

## The terminal UI

```sh
open-conduit tui
```

The TUI lists one row per issue or thread, with its latest turn and how many turns it has had. `g` lists every turn, and `enter` shows one with all of its Work's turns.

<!-- generated: tui keys (go test ./integration -run TestDocsTUIKeys -update-docs) -->

| Key | Does |
| --- | --- |
| `enter` | show a run with all of its Work's turns |
| `g` | switch between one row per issue or thread and one row per turn |
| `l` | the run's log |
| `t` | the agent's transcript; in a log, the run's next output |
| `f` | cycle the state filter (running, parked, failed, …) |
| `r` | refresh now |
| `esc` | in a detail or log, go back |
| `q` | quit from the list; in a detail or log, go back (`ctrl+c` quits from anywhere) |
| `c` | the scopes' actions, such as claiming an issue by hand |
| `1`–`9` | in a Work's detail, the actions its extensions offer now |
| `R` | run or retry, after asking, since it starts a new turn |
| `H` | hold, asking for a reason (`enter` to skip), or release |
| `S` | in a Work's detail, skip the first Event routing is stuck on, after asking |
| `E` | in a Work's detail, route the first stuck or failed Event again |
| `a` | attach to the agent's session |
| `x` | cancel a live run, after asking |
| `X` | cancel the whole Work, after asking |

<!-- end generated: tui keys -->

## The web view

```sh
open-conduit web
```

This serves the same view in a browser on this machine, at `http://open-conduit.localhost:7420`. It prints a sign-in URL; open it to start a session in that browser. Browsers resolve every `*.localhost` name to this machine without any setup. If yours doesn't, use the second URL it prints, which names `127.0.0.1`. Prefer the first: browsers don't keep cookies apart by port, so a browser signed in on `127.0.0.1` sends its session to every other server on that address, such as an agent's dev server.

To have the daemon serve the view itself, start it with `--web`, or install the service that way:

```sh
open-conduit serve --web
open-conduit service install --web
```

The sign-in URL lasts across restarts, so you can bookmark it, and `open-conduit web url` prints it at any time. Each sign-in starts a session of its own, which lasts across restarts too until the browser has been idle for 12 hours or signed in for 7 days; then open the URL again. **Sign out everywhere**, at the end of the status line, ends every browser's session at once and leaves the URL working. To change the URL as well, delete `private/web.signin` in the state directory and restart the view.

The web view has five pages, with the daemon's state, the active generation and the agent and job slots in use along the bottom.

- **Runs** lists one row per Work, grouped by what it needs from you: first what needs a person (a decision an extension offers, such as answering an agent's question, or a repair such as a turn that ran out of retries), then running, queued, waiting, held and closed. Queued is Work with input waiting for a turn, and says so when a concurrency limit is holding it back. Work that handed its turn back and has nothing new to act on is waiting, for its next event, not queued, so raising `settings.concurrency` never leaves it behind. Each row shows what the Work is doing now in its source's words (what a Wait is for, such as "checks on PR #137"), its lifetime as a line with a bar for each stretch of active time, and its turns. The state strip, search, filters, grouping and sort are all in the URL. Select rows to hold or release several at once; running Work is skipped and the result says how many.
- **A Work's page** shows what the Work is and is doing now, any panel an extension shows that offers an action (such as the open question with its options), its turns as a score with the waits between them, and one turn at a time with its transcript read as a conversation: the agent's and people's messages as chat, what the agent sent elsewhere with its text, and tool calls as quiet rows you can expand. The Events tab lists every Event with how it was routed. The facts beside it give the Work's IDs, routing, the turn's executor and workspace, and the totals.
- **Needs you** gathers what waits on a person, what stopped and needs a repair, what an operator holds, and what has sat unchanged for a while, with the changes you made in this session.
- **Configuration** draws each scope's routing as a flow chart: the sources that bring events in, the routes tried in order with what each does while its Work is busy, and the binding and executor each route hands its Work to. Pick a sample event to trace how it would route now, without sending it. It is read-only; change the configuration with `open-conduit config set` or `config edit`, or set up with `open-conduit init --with claude`.
- **Setup** runs a setup session in the browser, in a scope with a `setup` entry. A daemon started with no config file runs a default `setup` scope; otherwise `open-conduit init setup` adds one, applied as a new generation with `config validate` and `config apply` once the daemon has run (see [set it up in the browser](/open-conduit/start/quick-start/#set-it-up-in-the-browser)). Start one (choosing the model, when the binding offers more than one, and ticking any grants the binding offers for that session only: the default scope offers a shell and your own connectors), then read what the assistant says, answer its questions with a button per choice and send it messages. A message sent while it waits on a question reaches it with the answer. The assistant has no shell: it looks around through probes and proposes config changes, each with its reason. The Changes section shows each proposal's before and after, with Approve and Reject, Approve all and Validate; Apply applies the approved changes as the next generation, with the generation the page read, and shows doctor's checks afterwards. Nothing in a session changes the configuration until you apply. `open-conduit actions run setup start --scope setup --input "MODEL +shell"` starts a session from a terminal. The conversation follows the newest message while you are at the bottom, and the message box takes several lines (Shift+Enter). End a session by cancelling it. Without a setup scope, the page points to `open-conduit init --with claude` in a terminal.

When a newer release is out than the daemon runs, the status line says so. Its link opens the Update page, which shows the running and latest versions with a link to the release notes, and installs the new release as `open-conduit upgrade` does, then restarts the background service and reloads until the daemon answers on the new version. Without a running service it says to restart `open-conduit serve`; for a Homebrew install it gives the `brew upgrade` command instead. The view checks GitHub for the latest release at most every six hours.

Every change (run again, rebind, send a wake event, hold, release, cancel a turn or the Work, and the actions extensions offer) asks first, says what it will do, and sends the Work's current revision; the confirmation names the call and shows the event the daemon accepted. Every page works without JavaScript. With it, `⌘K` jumps to any Work, scope or page, `g` then `r`, `a`, `c` or `s` goes to a page, `/` searches, `j`/`k` move through rows or turns, `x` selects, `e` jumps to the first error in a transcript, and `?` lists the keys. To attach to an agent's session, use the terminal.

- `--addr HOST:PORT` (or `OPEN_CONDUIT_WEB_ADDR`) changes where it listens. Only a loopback address is accepted, so the view is never reachable from another machine.
- `--host NAME` (or `OPEN_CONDUIT_WEB_HOST`) changes the name it answers to.
- With `serve`, the same choices are `--web-addr` and `--web-host`. Either one also turns the view on. Pass the same values to `web url`.

`open-conduit web` runs the view as its own process, a client of the daemon like the TUI; stop it with Ctrl-C. With `serve --web` it runs inside the daemon and stops with it, but it is still a client of the management socket. Either way it holds the operator token itself and never gives it to the browser. If the view can't start (its port is taken, say), the daemon logs why and keeps running without it.

## The same controls on the command line

```sh
open-conduit claim 12                                # claim issue #12 by hand
open-conduit actions list --work ID                  # what extensions offer on a Work now
open-conduit work list --state waiting
open-conduit work retry ID
open-conduit work hold ID --reason "needs a decision"
open-conduit work attach ID                          # hold, attach to the agent's session, release on exit
open-conduit runs list
open-conduit runs log ID --follow                    # follow a running turn
open-conduit route issue/12                          # explain how an issue's Events were routed
open-conduit route --samples --scope eng             # sample Events to trace
open-conduit route --trace issue-opened --scope eng  # how a new issue would be routed now (records nothing)
open-conduit events chain ID
open-conduit events skip ID --reason "bad payload"   # route an Event routing is stuck on as skipped
open-conduit events reroute ID                       # route a failed or skipped Event again
```

- `work retry|hold|release|cancel` act on the Work's current revision. Pass `--revision N` (from `work get ID`) to act only if nothing has changed it since.
- Lists print a table; `--json` prints the records. `--scope` filters by scope everywhere.
- `runs log ID` shows a run's output; `--transcript` shows the agent's transcript.
- `events chain ID` shows the chain of Events that led to something.
- An Event whose route keeps failing is retried with a growing backoff and, after five failures over at least 15 minutes (or at once when retrying can't help, such as a route whose extension is gone), routed as `failed`. Doctor's `routing` check lists Events still unrouted after ten minutes. `events skip` lets the Work behind one move on; `events reroute` tries an unrouted one now, or accepts a failed or skipped one again as a copy.
- `doctor --check-secrets` checks connections, credentials, webhooks, bindings and the executors' CLIs.

## More than one operator

By default there is one operator: you, with the token the daemon writes to `private/operator.token` in its state directory. Everything you do is recorded as `operator:local`. To let someone else run controls under their own name, issue them a credential of their own:

```sh
open-conduit auth add alice --out /path/to/alice.token   # an owner-only token file for alice, role operate
open-conduit auth operators                              # every operator credential, active or revoked
open-conduit auth revoke TOKEN_ID                        # revoke one (the IDs are in auth operators)
```

Alice then points the CLI at her file with `--token-file` or `OPEN_CONDUIT_TOKEN_FILE`, on a machine that can reach the management socket. Everything she does (an Event she sends, a hold, a cancel, a rebind, an action such as answering an approval) is recorded as `operator:alice`. The web view's **Events** tab shows who sent each Event, and `events get` and `--json` lists show it as `actor`. An Event a person sent from Slack or GitHub shows their identity there, such as `github:octocat`. **By default only you (`operator:local`) may answer asks and approvals.** To let others answer, add `operator:alice`, or a bare `operator` for any operator, to the `human` entry's `responders`.

- A name is lower case letters, digits and dashes, up to 32 characters. A name has one active credential at a time; revoke it before you issue another.
- `auth rotate` replaces the caller's own credential and keeps its name.
- Each credential has a role, set with `--role` when you add it (default `operate`). To change a role, revoke the credential and add it again.

  | Role | May |
  | --- | --- |
  | `view` | read everything: Work, Events, runs and their logs, config, doctor, the operator list; trace a route and validate config, which change nothing |
  | `operate` | also send Events and run every control and action: create, cancel, retry, wake, hold, release, rebind and attach Work, cancel executions, skip and reroute Events, reconcile scopes, answer asks; rotate and revoke their own credential |
  | `admin` | also apply config, add operators and revoke other operators' credentials |

  You are always `admin`. A call the role doesn't allow is refused with `forbidden_role` before it does anything.
- No one but you can revoke your own credential, not even an `admin`. `auth recover` (with the daemon stopped) revokes every operator credential, named ones included, and writes you a fresh one.
- The web view still signs browsers in with its one sign-in URL and acts as `operator:local`.
