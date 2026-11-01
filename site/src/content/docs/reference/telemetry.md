---
title: Usage telemetry
description: What anonymous usage data the daemon sends, and how to turn it off.
---

A release build of the daemon sends a few anonymous events to the project's PostHog (EU cloud), so we can see how many installs there are, how much they run and which extensions they use. It's on by default and easy to turn off.

## Turn it off

Any one of these turns it off:

- `open-conduit serve --no-telemetry`, or `open-conduit service install --no-telemetry` for the background service (the flag goes into the service's command line);
- `OPEN_CONDUIT_NO_TELEMETRY=1` in the daemon's environment;
- `DO_NOT_TRACK=1` in the daemon's environment.

A build without a release version (`go install` from a checkout, `make build`) never sends anything.

## What is sent

| Event | When |
| --- | --- |
| `install` | the first time a daemon starts on a state directory |
| `daemon_started` | each time the daemon starts |
| `daemon_heartbeat` | once a day while it runs |
| `daemon_stopped` | when it stops |

Every event carries:

- a random install ID, kept in `telemetry-id` in the state directory (delete the file to get a new one);
- the Open Conduit version and commit, and the operating system and architecture.

The start, heartbeat and stop events also carry the shape of the active configuration:

- the number of scopes, and how many are enabled;
- the types of the first-party extensions, connections and executors the config uses, such as `github`, `slack` or `claude-code`;
- how many types come from installed third-party packages (a count, never their names).

The heartbeat and stop events add what happened since the last report:

- how many Work items were created;
- how many runs finished, in total and in each state (`completed`, `failed`, `waiting`, `handoff`, `cancelled`).

The stop event also says whether the daemon stopped on an error, and only its kind (`install_removed` or `other`), never its message.

## What is never sent

No names of scopes, bindings, connections or extension entries, no repositories, issues, paths, hostnames, prompts, outcomes, logs, error text or secrets. The events are anonymous: PostHog stores no person profile for them, and the install ID isn't tied to a person or a machine.

Telemetry never affects the daemon: a report that fails is dropped and not retried.
