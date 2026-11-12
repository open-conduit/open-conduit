---
title: Glossary
description: The terms the rest of the docs use, each in a sentence or two.
---

[What Open Conduit does](../overview/) introduces Work, Event, Execution, Timer and Wait, Scope, routes, bindings and the toolbelt. This page adds the other terms you meet in the docs, the config and the CLI.

## Work and its attempts

- **Work:** a durable objective, such as an issue to fix or a thread to answer. It outlives any one attempt at it.
- **Execution:** one attempt to advance a Work, by Claude Code, a shell command or another executor. An attempt that fails doesn't by itself fail the Work.
- **Turn:** one Execution of an agent. The agent does what it can, ends the turn with a status (such as `building` or `review`) and Open Conduit starts the next turn when there is news.
- **Inject:** deliver an Event to an attempt that is still running, such as a comment that arrives while the agent works. A route's `active_policy` chooses inject, queue, interrupt or drop for news that arrives mid-attempt, and the executor must declare that it can take injected input.
- **Resume:** start the next turn in the previous turn's session, so the agent keeps what it knew. An executor declares whether it can resume; one that can't starts each turn afresh from the Work's context.
- **Handoff:** a short note an agent leaves at the end of a turn for whoever takes the Work up next, used when a fresh session starts instead of resuming a cold one.
- **Wait:** a Work parked until a matching Event arrives (another PR merges, an issue closes) or a time comes. Nothing runs or polls while it waits.

## Config

- **Scope:** one area of work in the config: its connections, secrets, integrations, routes and bindings, and the most its agents may do (`authorization`).
- **Connection:** the credentials for one service, such as a GitHub token or a Linear API key. A scope must be granted a connection before it can use it.
- **Route:** the rule that sends a kind of Event, such as a newly opened issue, to a binding.
- **Binding:** an executor and exactly what it may do: the operations it may call, its connection, its workspace and its executor's settings.
- **Generation:** one applied version of the config. The daemon runs one generation at a time and keeps it across restarts; `config apply FILE --generation N` makes the file the next one, so an edit to the file changes nothing until it is applied. A GitHub, Linear or Jira issue also gets a new generation when it is reopened, so its reopened life is new Work.
- **Playbook:** the instructions an agent's turn starts with, such as `work-issue` for a GitHub issue or `slack-thread` for a Slack thread. A binding names it in `config.playbook` (`"-"` for none). Playbooks are Claude Code skills: Open Conduit ships its own, a binding's `plugin_dir` points at a folder of your own, and a repository can add to one in `.open-conduit/playbooks/<name>.md`.
- **Toolbelt:** the operations a run may call, such as `github.issue.comment` or `wait`, limited to what its binding allows. Open Conduit checks each call where it serves it, whatever the agent's instructions say.

## GitHub and trackers

- **Claim:** taking an issue or PR on as Work. Open Conduit labels it (`agent-in-progress` by default) so another owner leaves it alone.
- **Hold:** an issue claimed but not started until something happens, such as a time or another PR merging. Held issues carry the `scheduled` label; take it off to start one early. `init github --holds` turns holds on.
- **Marker:** a hidden line in an issue or comment that Open Conduit reads or writes. A hold's marker, such as `<!-- waitFor: pr:12:merged -->`, says what the issue waits for; the marker on each agent comment says which Work posted it, so an agent never answers itself and a retried post isn't made twice. Agents never write markers: they ask for an outcome through the toolbelt.

## Other names

- **`os`:** the toolbelt's command-line client, installed beside `open-conduit` and kept off your `PATH`. Agents and shell commands reach the toolbelt through it.
