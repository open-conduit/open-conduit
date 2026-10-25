# Toolbelt authority and local management

Ticket 02-06 implements [B03](extensions.md) toolbelt authority and [B05](config-management.md) configuration, secrets and the local management API. The pieces are the runtime [toolbelt](../../internal/runtime/toolbelt.go) and [operator control](../../internal/runtime/manual.go) operations, [configuration loading/validation/secrets](../../internal/config), the [management server](../../internal/management), the public [client](../../client/client.go) and the [`open-conduit` operator commands](../../internal/cli/operator.go). Composing a long-running daemon (scheduler/poll loops plus server lifetime) is left to the core gate. Shell environment construction and the `os` CLI transport are delivered by 02-07 ([shell executor](shell-executor.md)).

## Toolbelt authority

`runtime.Toolbelt` implements `extension.Toolbelt`, `TokenIssuer` and `Registrar`. Tokens are unpadded base64url encodings of 32 random bytes. Coordination storage keeps only their SHA-256 digest, the pinned authority, binding and generation, a revocation ID and revocation state. Plaintext is returned once to the trusted issuer. With `SchedulerOptions.Toolbelt` set, the scheduler issues a fresh token after every claim, recovery and resume, for executors whose transport is not `none`. It places the token in `RunRequest.Context`. Issuing a token revokes all lower-epoch tokens for that execution. Token lifetime is limited by the attempt deadline (or an optional `Lifetime`), so it can never outlive active authority.

Every call, reads included, passes these checks in order:

1. Digest lookup, revocation and exclusive expiry.
2. Narrowing `scope_id`/`work_id`/`execution_id` assertions.
3. The operation is declared and in the token allowlist.
4. The current B05 ceiling, checked through `ToolbeltOptions.Authorize` outside storage. A config reload or disabled scope therefore revokes immediately.
5. Under the lease guard in the call's transaction: the execution is running at the token's epoch, the owner matches, the lease is not expired, the Work is running in the token scope and the segment has no pending declaration.

Emit uses only declared and allowed families. It assigns trusted execution source, Work, Execution and continuation causation, rejects forged sources/parents/targets and unowned correlations, and accepts through `AcceptDerivedInTx`, which applies causal bounds. `create_work` joins child creation and its causal `core.work.created` in one transaction. Extension handlers run outside storage after a fenced authority check. They receive validated authority and never the token.

Idempotency semantics:

- **emit**: idempotent by delivery identity.
- **Request IDs** (`request_id`, or the HTTP `Idempotency-Key` when absent): replay the committed result for the same execution and operation, and reject a changed body with `ErrRequestConflict`.
- **Children**: allocate stable IDs from the request ID before commit.
- **Extension handlers**: run at most once per request ID.
- **wait and declare_outcome**: release authority, so a duplicate after a lost response fails authorization. Resolve it by inspection, as B05 requires.

`wait` and `declare_outcome` are recorded under the fence as a per-epoch declaration. Toolbelt authority is released at once. A request alone does not register a predicate or establish physical exclusion; this resolves the B03 "atomic release" wording against the 02-04/02-05 handshake rule. When the trusted host observes the segment finish or suspend, `Scheduler.Complete` applies the declaration:

- **Declared wait**: arms its timeout timer and registers the Wait in one transaction (via `WaitWithResult`), keeping executor usage/artifacts. Resume-capable executors park; other executors end waiting.
- **Declared outcome**: replaces the executor's structured outcome.

The declared watermark defaults to the attempt's acknowledged input, so registration backscans every Event accepted in between. Transport/observation errors still follow failure policy.

## Configuration and secrets

`config.Validate` strictly parses version 1 JSON: duplicate keys, unknown fields, nulls, types and the 1 MiB limit. It then applies defaults, validates the scope graph, inheritance, tombstones and stable slots, checks registrations and owners, references and authorization, and mechanical capabilities. It returns every independent diagnostic as a safe JSON pointer/code/remedy in deterministic order and never echoes values or reference text. It ports every rule and negative mutation of `scripts/check-config-contract.py`, and the exact invalid-capability diagnostic. A snapshot carries:

- a canonical hash
- redacted normalized config
- `DomainScopes` (scope metadata without reference text)
- `AuthorizeOperation`/`AuthorizeRead` ceiling checks

`config.Resolver` accepts only `keyring:`, `file:` (key in an owner-only `secrets.json`), `env:` and `cmd:`. The default keyring uses the platform helper; tests inject a backend. `cmd:` runs `sh -c` under a 10-second process-group deadline with 64 KiB bounded stdout. Stderr is discarded, the environment is minimal with no toolbelt or Open Conduit variables, exactly one trailing LF/CRLF is trimmed and empty output is rejected. Errors carry stable codes only. No resolution is cached across consumers. `SchedulerOptions.Secrets` resolves only the binding-selected names that both the pinned and the current generation grant. It runs immediately before Run/Recover/Resume, outside transactions; failure blocks that attempt with a safe diagnostic.

`management.ConfigService` restores the committed generation from coordination storage. The first start activates the config file as generation 1. Apply proceeds in this order:

1. Revalidate and check the candidate hash and expected generation.
2. Save the file atomically (same-directory 0600 temp, fsync, rename, parent fsync, `.last-good` backup).
3. Publish the generation, its normalized snapshot and changed scope records in one transaction.

A crash before that commit keeps the old generation, and a disk edit alone never changes running behavior. Removing or reparenting a scope with nonterminal Work returns `scope_in_use`. `OnApply` lets the host rebuild generation-pinned components. Toolbelt grants are the pinned binding operations intersected with the current ceiling.

## Local management API

The server implements B05 API version 1 over HTTP/1.1 JSON on an owner-only Unix socket. `AcquireLock` takes an exclusive `flock` and rejects group/world-accessible or foreign-owned state directories. `Listen` removes a stale socket only after lock and ownership checks, rejects over-long paths and sets mode 0600.

Authentication:

- Every endpoint requires a bearer token.
- Operator credentials (digest-only records, bootstrapped under the lock into `private/operator.token`) never reach `/v1/toolbelt/call`. Execution tokens reach only that endpoint.
- Rotation atomically revokes the caller and returns a replacement once; it is never stored in idempotency history. `RecoverOperator` is the locked local path after a lost response.
- Revocation by token ID covers execution tokens and the caller's own operator token.

Request handling:

- Mutations require `application/json`, a body of at most 1 MiB, strict fields (no unknown, duplicate or null fields) and no query string.
- Every mutating POST except validate, rotate and toolbelt calls requires `Idempotency-Key`. The key is bound to principal, method, path and canonical body hash, and kept for 30 days. A replay returns the original status and body; a changed body is `409 idempotency_conflict`.
- Operator actions derive durable identities from the key, so retrying a timed-out request completes the same operation.
- Opaque IDs may contain `/`. Routes match escaped path segments.

Reads:

- Lists use HMAC-authenticated cursors bound to endpoint, filters, a captured upper bound and a 15-minute expiry. Events use acceptance-sequence keyset traversal; other primitives use `(created_at,id)`.
- Single reads expose revision ETags and answer `If-None-Match` with 304.
- Errors use the B05 envelope with safe codes and never include backend/provider text.

Operator controls go through `runtime.Control`:

- **Manual events**: `core.manual` root, or a derived event when a readable parent is supplied. Routed immediately when possible.
- **Manual Work**: atomic ready Work, root `core.work.created` and explicitly bound queued input. Nothing is launched directly.
- **Work cancel**: a control Event plus full fencing in one transaction.
- **Execution cancel**: fences one attempt and holds scheduling (`BlockedReason=operator_cancelled`) until a manual retry. Physical stop still needs trusted `ConfirmStopped`.
- **Retry**: `core.manual.retry` processed by retry policy.
- **Wake**: an ordinary owned-key event that reports whether it matched.

Control checkpoints keep the Work in the envelope but use an empty routed association, so earlier runnable input cannot delay fencing. `artifacts/{id}/content` streams through `storage.Artifacts` and supports one byte range.

The client dials only the configured socket, with the fixed host `localhost`, no proxy and no redirects. `open-conduit` resolves the endpoint (`--endpoint`, then `OPEN_CONDUIT_ENDPOINT`, then socket discovery) and the token file (`--token-file`, then `OPEN_CONDUIT_TOKEN_FILE`, then `state/private/operator.token`). It supports:

- health
- scopes, work, events (including causal `events chain`), executions, waits and artifacts (`artifacts cat` for logs)
- emit, work create/cancel/retry/wake and execution cancel
- config validate/apply
- auth rotate/revoke
- since 02-08: unified run history (`runs list|get|table|log`), `doctor` and `tui` ([observability and history](observability-history.md))

Rebind and scope reconcile endpoints are deferred to operator controls in 03-05.

## Evidence

- The [toolbelt integration cases](../../integration/toolbelt_test.go) run every shared toolbelt conformance case against real scheduled attempts on persistent and reopened SQLite. They add issue-on-claim/recovery supersession, digest-only storage, live ceiling narrowing, revocation by ID, request replay and conflict, extension handler at-most-once, forged source/correlation refusal, and declared wait/outcome handshakes.
- The [management integration cases](../../integration/management_test.go) start the real server on a Unix socket and drive it only through the CLI and client. They cover socket permissions and lock exclusion, credential-class separation, the causal chain from operator root to toolbelt emission, execution cancel with idempotent response-loss replay, confirmed-stop hold and manual retry, artifact range reads, Work cancel, rotation/revocation/lease expiry, config validate/apply with diagnostics, stale-generation conflict, immediate narrowing of a live token, atomic save/backup, restart restoring the committed generation despite a disk edit, pagination with tamper/filter mismatch, strict request validation, toolbelt wait plus operator wake, and the absence of secrets/references/tokens in responses and coordination storage.
- [Config tests](../../internal/config) cover the four examples, the invalid-capability diagnostic, every oracle mutation, all four secret schemes and file safety.

This is local in-process and socket evidence. It makes no daemon-restart, multi-process or real-keyring claims.
