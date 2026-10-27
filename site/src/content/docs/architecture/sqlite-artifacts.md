---
title: "SQLite coordination and file artifacts"
---

Ticket 02-01 implements [B02](../domain-storage/) after the [contracts gate](../contracts-gate/). The public coordination and artifact interfaces remain separate. This supplies durable storage, not event routing, execution scheduling, provider clients or an operational ownership transfer.

## Coordination backend

`sqlite.Open(ctx, path)` opens an independent Open Conduit database; `Close()` releases its connections. The backend uses the pure Go `modernc.org/sqlite` driver pinned to v1.38.2, preserving the Go 1.24.2 compatibility floor and Darwin/Linux amd64/arm64 targets. SQLite WAL mode and `synchronous=FULL` are required, not optional throughput flags. Each Store uses one pooled connection, and `BEGIN IMMEDIATE` serializes transaction admission across independently opened Store handles before callback evaluation. A callback is never transparently retried.

Indexed tables store the five primitive records, delivery identity, exclusive correlation ownership, processing checkpoints, timer revision tombstones and namespaced coordination KV. The database is not a serialized in-memory state blob. Revision/lease checks and event/KV/timer changes share the same transaction. A successful callback result is acknowledged only after commit; callback errors, panics and cancellation before commit roll back.

The persisted schema starts at version 1. Opening a newer schema fails without modifying it; migrations are ordered and transactional and failure must preserve the prior schema and records. SQL data is coordination/history, not a place for transcripts or large binary artifacts.

`Store.Backup(ctx, destination)` requires a previously absent destination and uses SQLite's online backup API for a consistent standalone snapshot, including committed WAL data. Copying a live `.db` file directly is not a backup. `sqlite.Restore(ctx, snapshot, destination, now)` validates schema/integrity, requires a new isolated destination and fences retained lease authority before the restored database can be used. Restoration is a stopped-state operation; lease fencing alone does not stop old external processes or authorize two daemons to own the same external scope.

## Artifact publication and integrity

[`internal/artifacts`](https://github.com/open-conduit/open-conduit/blob/main/internal/artifacts/files.go) implements `storage.Artifacts`. `artifacts.Open(root)` creates owner-only directories and takes an exclusive nonblocking filesystem lock on `.lock`; another process or Store handle cannot own the same artifact tree. `Close()` aborts active writers and releases ownership. Lock lifetime includes backup pins. Directory ownership and modes are checked; symlink roots, object directories and content/metadata files are rejected. Artifact IDs are random 256-bit hex identifiers; caller paths never select a file.

The layout contains `pending/{id}/data` for unpublished streams and `objects/{id}/{data,meta.json}` for committed immutable objects. Files are `0600`; directories are `0700`. Publication performs these steps:

1. Stream directly to a pending file while computing its length and SHA-256; no whole-file buffer or coordination KV is used.
2. On `Commit(ctx)`, flush and close content, write/fsync immutable reference/hash/publication-time metadata, and fsync the staging directory.
3. Atomically rename the staging directory into `objects`, then fsync the objects and pending directories before returning the reference.

These guarantees rely on normal local filesystem rename/fsync semantics and functioning storage hardware; the tests do not simulate physical power failure. An acknowledged commit has flushed content and directory entries. Before acknowledgement, cancellation or any publication error yields no usable reference. Best-effort cleanup can leave an orphan after an interrupted/ambiguous filesystem operation, which is why retention cannot assume every object has a coordination reference. `Close` without Commit aborts; repeated commit, close or write-after-close returns `ErrClosed`.

`Open(ctx, ref)` checks the exact reference metadata and content length, then streams while verifying SHA-256 at EOF. Full consumers and backup copies must read to EOF and check the final read/copy error; closing a partially read reader is not an integrity check. Missing/retained-away content returns `storage.ErrNotFound`; same-size corruption returns `artifacts.ErrCorrupt`. Content loss does not rewrite or invalidate coordination history. The artifact store does not implement producer redaction: executors must redact before publication as required by B03/B05.

## Recovery and retention

After process death, acquire exclusive ownership again and call `Recover(ctx)`. Recovery removes unpublished pending directories and preserves all committed objects. It never removes streams owned by active writers in the current Store. A SIGKILL after successful Commit preserves readable content on reopen; a SIGKILL while streaming exposes no reference/content and the pending bytes are cleaned on recovery.

`Collect(ctx, before, liveIDs)` removes only committed objects published before `before` that are absent from the supplied protected set, plus aged stale pending streams. Active writers are always preserved. The caller must supply a complete reference set and coordinate against attaching an old orphan during collection. Runtime retention policy must first release references or explicitly record their expiration; this backend cannot infer the live set from a partial database scan. Metadata/history remains in SQLite after content deletion. `Delete(ctx, ref)` is idempotent and checks reference identity before removing content.

## Complete backup bundles

[`internal/backup`](https://github.com/open-conduit/open-conduit/tree/main/internal/backup) composes the SQLite snapshot and artifacts with a versioned, complete manifest. Its `Create` operation acquires an artifact `Pin()` **before** the SQLite snapshot, enumerates references from that exact snapshot, copies them through `Pinned.Open` with streaming integrity checks, and publishes the manifest only after all required copies are durable. A pin holds retention/deletion until closed; use `Pinned.Open`, not nested `Store.Open`, while pinned. Concurrent producers can publish new objects, but they are outside the captured database reference set. Missing referenced content fails the backup rather than silently claiming a complete restore point.

`Store.Import(ctx, ref, reader)` restores an immutable reference ID and metadata in a new artifact store, streaming bytes and requiring exact length and successful source EOF. Root backup composition additionally verifies SHA-256 against the bundle manifest. A failed/partial copy is unpublished. Restore uses new isolated destinations, validates manifest/schema/content and fences SQLite leases before exposing a completed state directory. Partial staging directories are not restore points. Operational conversion and disaster-recovery commands remain 06-02; these are backend APIs and tested bundle mechanics.

## Verification scope

The unchanged shared storage suite runs against SQLite. Backend tests cover reopen, competing connections, migration failure/newer schema refusal, callback error/panic/cancellation rollback, process death at transaction boundaries and backup restoration of correlation ownership/pending timers. The deliberately weakened reference controls continue to fail their selected shared cases.

Artifact tests exercise normal publication/abort/reopen, streaming concurrency, injected data-flush/publication/directory-flush failures, cancellation, same-size corruption, reference/path/permission checks, directory ownership, retention/live-set protection and a pinned copy racing deletion. Subprocess tests synchronize with a pipe and SIGKILL immediately before or after Commit, then reopen/recover. They do not use elapsed sleeps as correctness barriers. Filesystem/device power-loss testing and production retention scheduling remain later operational/runtime evidence.
