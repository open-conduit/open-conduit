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

## Lost operator credentials

The operator client talks to the daemon with a token. If it is lost, stop the daemon and run `open-conduit auth recover`, which revokes every operator credential and writes a fresh token file. `auth rotate` replaces the token while the daemon runs.
