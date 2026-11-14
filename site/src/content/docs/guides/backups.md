---
title: Backups and recovery
description: Back up the state directory, verify a backup and diagnose state.
---

Back up before every upgrade:

```sh
open-conduit service stop
open-conduit backup create ~/backups/open-conduit-2026-10-26   # with the daemon stopped
open-conduit backup verify ~/backups/open-conduit-2026-10-26
open-conduit state check                                       # diagnose the state directory
open-conduit service start
```

`backup create` and `state check` need the daemon stopped. A backup is a verified bundle of the state directory; `backup verify` checks it without restoring anything.

## Restore

`backup restore` writes a new state directory from a bundle; it never overwrites the current one. Point the daemon at it with `--data-dir`:

```sh
open-conduit backup restore ~/backups/open-conduit-2026-10-26 --to ~/.local/state/open-conduit-restored
```

## Move to a new host

Move the state with the config: copy the config file (and whatever its secret references need, such as keyring entries or environment variables), then carry the state across as a backup bundle.

```sh
open-conduit service stop                                      # on the old host
open-conduit backup create ~/backups/open-conduit-move
# copy ~/backups/open-conduit-move to the new host, then there:
open-conduit backup verify ~/backups/open-conduit-move
open-conduit backup restore ~/backups/open-conduit-move --to ~/.local/state/open-conduit   # a directory that does not exist yet
open-conduit service start
```

The bundle holds every Work, its waits and what each integration recorded, so conversations waiting on a reply carry on where they were. Don't run both daemons against the same tracker or workspace at once.

A new host started with an empty state directory doesn't continue anything the old one held. For Slack, an entry claims only threads begun after the new daemon first registers it (`claim_from`, [Slack](../../architecture/slack-extension/#moving-to-a-new-host)), so old threads get no late replies, and a person's reply in a conversation the old daemon was holding reaches nobody. Doctor's `slack` check counts the threads whose replies carry an earlier daemon's marker.

## Lost operator credentials

The operator client talks to the daemon with a token. If it is lost, stop the daemon and run `open-conduit auth recover`, which revokes every operator credential and writes a fresh token file. `auth rotate` replaces the token while the daemon runs.
