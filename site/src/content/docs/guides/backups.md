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

The artifacts go to the restored directory's own `artifacts/`. To put them in another artifact store the binary is built with, name it and an empty location, and the restored directory's `storage.json` records it:

```sh
open-conduit backup restore BUNDLE --to DIR --artifacts files --artifacts-location /srv/open-conduit/artifacts
```

## Move to a new host

The bundle carries the config: the database holds every config generation, secret references included, and the daemon starts from the active one. It doesn't carry what the references point at (keyring entries, `secrets.json`, environment variables) or the packages you installed, so set those up on the new host first.

```sh
open-conduit service stop                                      # on the old host
open-conduit backup create ~/backups/open-conduit-move
# copy ~/backups/open-conduit-move to the new host, set up the secrets and
# install the same packages (open-conduit extension install), then there:
open-conduit backup verify ~/backups/open-conduit-move
open-conduit backup restore ~/backups/open-conduit-move --to ~/.local/state/open-conduit   # a directory that does not exist yet
open-conduit service start
open-conduit config export                                     # writes the config file from the restored generation
open-conduit doctor                                            # secrets shows any reference that doesn't resolve
```

The restore lists any `config_problems`: why the restored config wouldn't validate here, such as a type no installed package provides. Fix those before starting: otherwise the daemon starts from a config file at its path instead, or doesn't start.

The bundle holds every Work, its waits and what each integration recorded, so conversations waiting on a reply carry on where they were. Don't run both daemons against the same tracker or workspace at once.

A new host started with an empty state directory doesn't continue anything the old one held. For Slack, an entry claims only threads begun after the new daemon first registers it (`claim_from`, [Slack](../../architecture/slack-extension/#moving-to-a-new-host)), so old threads get no late replies, and a person's reply in a conversation the old daemon was holding reaches nobody. Doctor's `slack` check counts the threads whose replies carry an earlier daemon's marker.

## Lost operator credentials

The operator client talks to the daemon with a token. If it is lost, stop the daemon and run `open-conduit auth recover`, which revokes every operator credential and writes a fresh token file. `auth rotate` replaces the token while the daemon runs.
