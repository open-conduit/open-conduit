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

Restore a bundle into the daemon's own state directory with the daemon stopped:

```sh
open-conduit service stop
open-conduit backup verify ~/backups/open-conduit-2026-10-26
open-conduit backup restore ~/backups/open-conduit-2026-10-26 --in-place
open-conduit service start
```

`--in-place` moves the database, artifacts and operator token it replaces into a `before-restore-<time>/` directory in the state directory, puts the bundle's in their place, writes a fresh operator token and ends with `state check`. It reports what it moved aside and what stays from after the bundle (executor run directories, workspaces, extensions' caches), which it never deletes. Delete `before-restore-*` once the restored daemon has run for a while. The [recovery runbook](../../architecture/recovery/#restoring-after-a-bad-release-or-lost-state) has each step, and what to do if the restore stops part way.

In a container, where the daemon is the container's process, start it with `serve --restore BUNDLE` and a mounted bundle: it restores in place before serving, once per bundle ([in a container](../../architecture/recovery/#in-a-container)).

The artifacts go to the state directory's own `artifacts/`. To put them in another artifact store the binary is built with, name it and an empty location, and the restored `storage.json` records it:

```sh
open-conduit backup restore BUNDLE --in-place --artifacts files --artifacts-location /srv/open-conduit/artifacts
```

When `storage.json` names an artifact store outside the state directory, `--in-place` refuses until you name one of these: it never moves the daemon onto the directory's own store unasked. Name `--artifacts files` alone to move the artifacts into the state directory on purpose.

`backup restore BUNDLE --to DIR` writes a new directory instead, never over an existing one, to inspect a bundle or to restore on a new host (below).

## Sign your bundles

Sign bundles if they leave this machine, so a restore can tell yours from one someone planted:

```sh
open-conduit backup keygen ./backup-signing.key   # prints the public key
open-conduit secret set backup-signing < ./backup-signing.key   # prints the reference
rm ./backup-signing.key
```

`secret set` prints the reference to use. Put it and the public key in the config, and keep a copy of the private key somewhere that isn't this disk:

```json
{ "settings": { "backup": { "signing_key": "keyring:backup-signing", "trust_keys": ["ed25519:..."] } } }
```

With trust keys set, `backup verify` and `backup restore` refuse a bundle that a trusted key didn't sign. Pass `--accept-unsigned` to take one with no signature, such as a bundle from before you turned signing on. On a host that has lost its config, name the key with `--trust-key ed25519:...`.

## Ship bundles off this machine

Name a destination in the config, and the daemon uploads each scheduled bundle to it. The bundled `aws-s3` type uses the daemon's own AWS identity, so grant the package the variables that give it one:

```json
{ "settings": {
  "extensions": { "aws": { "pass_env": ["AWS_REGION", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"] } },
  "backup": { "destinations": [
    { "name": "offsite", "type": "aws-s3", "config": { "bucket": "acme-conduit-backups", "region": "eu-west-2", "prefix": "prod" } }
  ] }
} }
```

Bundles hold private Work and config, so turn on default encryption for the bucket, and set its lifecycle rules: Open Conduit never deletes anything there. `open-conduit doctor` shows whether the latest bundle landed. To restore on a new host:

```sh
open-conduit backup list offsite
open-conduit backup fetch offsite conduit-20261102T090000Z ./bundle --trust-key ed25519:...
open-conduit backup restore ./bundle --in-place --trust-key ed25519:...
```

A fetch needs a trust key, because anyone who can write to the bucket can put a bundle there. `--accept-unsigned` takes a bundle with no signature instead.

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
