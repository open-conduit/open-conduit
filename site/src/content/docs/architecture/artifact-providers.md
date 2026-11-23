---
title: "Artifact provider protocol"
description: "The wire protocol between Open Conduit and a separately installed artifact store."
---

An artifact provider is an executable that serves Open Conduit's artifact store, so content can live in S3, GCS or anywhere else without changing or rebuilding the host. [`storage.json`](../domain-storage/#backend-lifecycle) names it. **The protocol is provisional**: it may still change, and the version below changes with it.

In Go, a provider passes its `storage.ArtifactBackend` to [`artifactstore.Serve`](https://github.com/open-conduit/open-conduit/blob/main/protocol/artifactstore/artifactstore.go), which implements everything here and exports the method names and wire types. In any other language, implement this page.

## Process and framing

The host starts the executable with no arguments and the host's environment. Messages are JSON-RPC 2.0 on stdin and stdout, one JSON object per line, each at most 16 MiB. Stderr is the provider's log, and nothing else may be written to stdout.

The provider exits when its stdin closes. After 5 seconds the host kills it. If it dies, the host starts it again on the next call, and handles from the old process are gone. The host may run several provider processes against one location, one for each command or daemon that has the store open.

The host sends up to 32 requests at once, but only one at a time on any handle. It may cancel a request with the `$/cancel` notification, `{"id": <request id>}`. Each call has a 2 minute limit, and `artifactstore.initialize` has 30 seconds.

## Version handshake

`artifactstore.initialize` is the first call on a connection and comes once. The provider opens `location`, creating the store if it is absent, and answers with the version it speaks. Any version other than 1 is refused with `unsupported_protocol_version`. It may add `"shared": true` or `"shared": false` to say whether another host can reach what the store holds, which `doctor` reports on shared state. A provider that leaves it out makes no claim.

```text
{"version": 1, "location": "s3://bucket/prefix"}
```

## Methods

A **ref** is `{"id", "size", "media_type", "sha256"}`. `sha256` is 64 lower-case hex characters. It is absent on a ref recorded before refs carried digests, and the store must still open that ref. A **handle** is `{"handle": "<string>"}`, chosen by the provider and valid until the call that ends it. **Chunks** are base64 strings of at most 1 MiB (`MaxChunk`) of content.

| Method | Params | Result | Does |
| --- | --- | --- | --- |
| `artifactstore.create` | `{"media_type"}` | handle | Starts a writer |
| `artifactstore.write` | `{"handle", "chunk"}` | `{}` | Appends to a writer |
| `artifactstore.commit` | handle | ref | Publishes the content and ends the writer |
| `artifactstore.abort` | handle | `{}` | Discards the content and ends the writer |
| `artifactstore.open` | ref | handle | Starts a reader. A ref that doesn't match the stored content is `invalid`, and an unknown ID `not_found` |
| `artifactstore.read` | handle | `{"chunk"?, "eof"?}` | Returns the next bytes. An answer must have a chunk, `eof: true` or both |
| `artifactstore.close_reader` | handle | `{}` | Ends a reader |
| `artifactstore.delete` | ref | `{}` | Removes content. An absent ID succeeds, and a ref that doesn't match is `invalid` and removes nothing |
| `artifactstore.list` | `{"after"?, "limit"}` | `{"items": [{"ref", "published_at"}]}` | Returns committed content with IDs after `after`, in byte order, at most `limit` (1 to 1000). An empty page is the end |
| `artifactstore.import_begin` | ref | handle | Starts publishing content under exactly this ref. An ID the store holds is `conflict` |
| `artifactstore.import_write` | `{"handle", "chunk"}` | `{}` | Appends to an import |
| `artifactstore.import_finish` | handle | `{}` | Publishes the import and ends it. Content that doesn't match the ref is `artifact_corrupt` and publishes nothing |
| `artifactstore.import_abort` | handle | `{}` | Discards the import and ends it |

Uncommitted content is never readable or listed, and a provider clears up what a dead process left unfinished. The full guarantees are those of [`storage.ArtifactStore`](https://github.com/open-conduit/open-conduit/blob/main/storage/artifactstore.go), and the [conformance suite](https://github.com/open-conduit/open-conduit/blob/main/conformance/artifacts.go) proves them.

## Errors

A refusal is a JSON-RPC error with code `-32000` and `data.code`:

| `data.code` | Means |
| --- | --- |
| `not_found` | No such artifact |
| `conflict` | The ID already exists |
| `invalid` | A malformed request, an unknown handle, or a ref that doesn't match |
| `artifact_corrupt` | Stored or imported content doesn't match its ref |
| `artifact_in_use` | Another owner holds a store that allows only one |
| `unsupported_protocol_version` | The handshake's version isn't one the provider speaks |

## What the host checks itself

The host doesn't rely on a provider for the integrity of content:

- It hashes and counts what it writes. A commit whose ref has another size, media type or digest is refused as corrupt, and the host deletes what was stored under the ref that describes the real bytes.
- It hashes and counts what it reads. A read that runs long or ends with the wrong size or digest is `ErrCorrupt`, never a clean end of file. For a ref with no digest it can check only the size, and the provider verifies the content.
- It checks an import's source against the ref before asking the provider to publish it.
- A choice in `storage.json` that can't be honoured stops the daemon. Nothing falls back to the file store.

A provider runs with the host's privileges. Process separation is fault isolation and not a sandbox.
