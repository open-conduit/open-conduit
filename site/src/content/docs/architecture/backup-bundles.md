---
title: "Local backup bundles"
description: "How Open Conduit captures, verifies and restores a complete local backup of its state directory."
---

A backup bundle is a complete, self-verifying copy of the coordination database and the artifacts it references. [`internal/backup`](https://github.com/open-conduit/open-conduit/blob/main/internal/backup/backup.go) builds and restores bundles from the [SQLite and artifact stores](../sqlite-artifacts/). The operator commands `open-conduit backup create`, `backup verify` and `backup restore` wrap it in [`internal/bootstrap/state.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/state.go), and the [recovery](../recovery/) page holds the runbooks. The backup package itself never starts a daemon or transfers scope ownership.

## Creating a bundle

`backup.Create(ctx, destination, database, artifacts, build, expired)` requires a previously absent destination under an operator-controlled parent directory. It pins artifact deletion **before** taking a SQLite online snapshot, reads Execution references from that snapshot, copies every referenced artifact with streaming integrity checks, and writes a versioned manifest containing the database schema, build identity and SHA-256 digests. References of Executions whose [retention](../observability-history/#retention) has expired are not copied: the manifest lists them under `Omitted`, since the sweep may already have deleted their content. The runtime owns the retention ledger, so the caller passes the `expired` lookup (`internal/bootstrap` wires `runtime.RetentionInTx`); `nil` treats nothing as expired. Any other missing content fails the backup. No logs or artifact bytes enter coordination KV.

The bundle is built in a private sibling staging directory. Files and directories are flushed before rename; the parent is flushed after publication. Until publication, the destination is absent. A crash may leave a `.conduit-snapshot-*` directory: it is incomplete and must not be selected as a restore point. Remove abandoned staging directories only with no backup/restore operation running. A final directory-sync failure may leave a complete published destination with an ambiguous durability result; the API returns the error, and callers must inspect that destination before retrying. Bundle paths must not be shared by concurrent publishers.

## Restoring a bundle

`backup.Restore(ctx, bundle, destination, now, expired)` requires a stopped, isolated, absent destination. It rejects unsupported/incomplete manifests, altered database bytes, altered artifact bytes and omitted or changed references from the captured database. A reference listed as omitted is accepted only on an Execution the restored retention ledger records as expired. It restores into a private staging directory, fences every backed-up lease while preserving its epoch, then publishes the complete state directory. A new Claim must advance that epoch; old execution credentials cannot commit. Failed validation publishes no destination. The resulting layout contains `conduit.db` and `artifacts/`. Credentials and config are not part of the bundle; the operator command writes a fresh operator credential (below) and config is provisioned separately.

## Limits

Digests detect corruption and incomplete copies; they do not authenticate a bundle against an adversary able to replace its manifest and all bytes. Keep bundles owner-only because snapshots contain private Work/Event/configuration and executor continuity. Backups require a compatible schema/build and an operator-controlled local filesystem supporting SQLite locking, atomic rename and directory sync. Power-loss guarantees depend on the filesystem and storage honoring flushes. Restore neither adopts a surviving external process nor authorizes a second scope owner; those remain runtime recovery responsibilities.

## Operator commands

`open-conduit backup create DEST`, `backup verify BUNDLE` and `backup restore BUNDLE --to DIR` run offline under the state directory's daemon lock ([`internal/bootstrap/state.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/state.go)). A restore also revokes the bundle's operator credential and writes a fresh `private/operator.token` through the locked local reset (`auth recover`). `backup.Inspect` reads a manifest without verifying its bytes.

## Evidence

`go test -race ./internal/backup` exercises captured correlation/timer/history/content, exclusion of later live changes, lease fencing, existing-destination refusal, manifest/database/artifact corruption, missing-reference rejection and refusal of an omission on an unexpired Execution. `TestShellRetentionReleasesRunDirectories` runs the retention sweep on real shell attempts, then backs up and restores, with the expired attempt's references omitted. SQLite and artifact suites separately exercise process kill/reopen and transaction/publication fault boundaries. `TestDaemonMigrationRehearsal` restores a bundle and restarts on it with no repeated external write, claim or job run, and refuses a tampered bundle.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 02-01 implemented backup composition alongside SQLite and artifacts as backend APIs and tested bundle mechanics. Operator commands and recovery runbooks were left to 06-02, and instance credentials/config were left to later bootstrap work.
- Ticket 06-02 added the `open-conduit backup` commands, the restore-time operator credential reset, the [recovery](../recovery/) runbooks and `TestDaemonMigrationRehearsal`.
- Until [#37](https://github.com/open-conduit/open-conduit/issues/37), `Create` copied every reference, so a backup failed with "not found" once the retention sweep had released any attempt's content. Expired references are now recorded as omitted.
