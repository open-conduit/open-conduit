---
title: "Toolbelt authority and local management"
description: "How Open Conduit authorizes toolbelt calls, loads configuration and secrets, and serves the local management API."
---

The toolbelt is how a running execution asks Open Conduit for outcomes, and the local management API is how an operator controls the daemon. Together they implement [B03](../extensions/) toolbelt authority and [B05](../config-management/) configuration, secrets and the local management API. The pieces are the runtime [toolbelt](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/toolbelt.go) and [operator control](https://github.com/open-conduit/open-conduit/blob/main/internal/runtime/manual.go) operations, [configuration loading/validation/secrets](https://github.com/open-conduit/open-conduit/tree/main/internal/config), the [management server](https://github.com/open-conduit/open-conduit/tree/main/internal/management), the public [client](https://github.com/open-conduit/open-conduit/blob/main/client/client.go) and the [`open-conduit` operator commands](https://github.com/open-conduit/open-conduit/blob/main/internal/cli/operator.go). The daemon composes them with the scheduler and poll loops and owns the server's lifetime ([daemon composition](../daemon-core-gate/)); the shell executor builds the execution environment and the `os` CLI transport ([shell executor](../shell-executor/)).

## Toolbelt authority

`runtime.Toolbelt` implements `extension.Toolbelt`, `TokenIssuer` and `Registrar`. Tokens are unpadded base64url encodings of 32 random bytes. Coordination storage keeps only their SHA-256 digest, the pinned authority, binding and generation, a revocation ID and revocation state. Plaintext is returned once to the trusted issuer. With `SchedulerOptions.Toolbelt` set, the scheduler issues a fresh token after every claim, recovery and resume, for executors whose transport is not `none`. It places the token in `RunRequest.Context`, with the execution's own endpoint (see [Execution endpoints](#execution-endpoints)). Issuing a token revokes all lower-epoch tokens for that execution. Token lifetime is limited by the attempt deadline (or an optional `Lifetime`), so it can never outlive active authority.

Every call, reads included, passes these checks in order:

1. Digest lookup, revocation and exclusive expiry.
2. Narrowing `scope_id`/`work_id`/`execution_id` assertions.
3. The operation is declared and in the token allowlist.
4. The current B05 ceiling, checked through `ToolbeltOptions.Authorize` outside storage. A config reload or disabled scope therefore revokes immediately.
5. Under the lease guard in the call's transaction: the execution is running at the token's epoch, the owner matches, the lease is not expired, the Work is running in the token scope and the segment has no pending declaration.
6. For an extension operation, after the replay lookup: every registered gate but the operation owner's (see [Gates](#gates)).

Emit uses only declared and allowed families. It assigns trusted execution source, Work, Execution and continuation causation, rejects forged sources/parents/targets and unowned correlations, and accepts through `AcceptDerivedInTx`, which applies causal bounds. The daemon passes the configured `causal_max_depth`/`causal_per_minute` to the toolbelt, so an execution's first emission off a new root pins the configured limits. `create_work` joins child creation and its causal `core.work.created` in one transaction. Extension handlers run outside storage after a fenced authority check. They receive validated authority and never the token. Handler refusals (`extension.OperationError`) return `422` with the handler's code and message.

Idempotency semantics:

- **emit**: idempotent by delivery identity.
- **Request IDs** (`request_id`, or the HTTP `Idempotency-Key` when absent): replay the committed result for the same execution and operation, and reject a changed body with `ErrRequestConflict`.
- **Children**: allocate stable IDs from the request ID before commit.
- **Extension handlers**: run at most once per request ID.
- **wait and declare_outcome**: release authority, so a duplicate after a lost response fails authorization. Resolve it by inspection, as B05 requires.

`wait` and `declare_outcome` are recorded under the fence as a per-epoch declaration. Toolbelt authority is released at once. A request alone does not register a predicate or establish physical exclusion; this resolves the B03 "atomic release" wording against the scheduler's handshake rule ([execution scheduling](../execution-scheduling/), [timers and waits](../timers-waits/)). When the trusted host observes the segment finish or suspend, `Scheduler.Complete` applies the declaration:

- **Declared wait**: arms its timeout timer and registers the Wait in one transaction (via `WaitWithResult`), keeping executor usage/artifacts. Resume-capable executors park; other executors end waiting.
- **Declared outcome**: replaces the executor's structured outcome.

The declared watermark defaults to the attempt's acknowledged input, so registration backscans every Event accepted in between. A type-only `wait` subscribes on the Work's keys, and an unapplied declaration survives recovery ([daemon composition](../daemon-core-gate/)). Transport/observation errors still follow failure policy.

### Gates

A registration may carry a gate (`extension.Registration.Gate`; an installed package declares `provides.operation_gate` and serves `operation.gate`). The toolbelt asks every gate, in registration-name order, before it runs another registration's operation; a gate never sees its own registration's operations, and builtins (`emit`, `create_work`, `wait`, `declare_outcome`, `read_context`) pass no gate. A gate answers in one of three ways:

- **Let the call through**: the next gate is asked, and once all have, the operation runs.
- **Park the caller**: it returns a `WaitPlan`, which the toolbelt declares exactly like a translated wait, owned by the gating registration (so its dependencies use that registration's dependency filter). The operation does not run, authority is released, and the call returns the plan's output and the Wait. The caller is resumed by whatever the plan waits on, or its timeout, and makes the call again.
- **Refuse**: its error reaches the caller verbatim (an `extension.OperationError` is `422` with its code and message), wrapped in `extension.GateRefusal`, which names the gate. Authority is kept for other calls.

A gate that panics, or an installed one that times out, crashes or answers with anything but an operation error, refuses with `gate_failed`: the host fails closed. A parked call records `toolbelt.called` with `toolbelt.gate` naming the gate; a refused one records `toolbelt.rejected` with `toolbelt.result` `gate_refused` and the same attribute. The core knows nothing of why a gate decides as it does. The `human` extension's approval gates are the first ([human decisions](../../workflows/human-decisions/#approval-gates)).

### Execution endpoints

`SchedulerOptions.ToolbeltEndpoints` is required whenever `SchedulerOptions.Toolbelt` is set (`NewScheduler` refuses the options otherwise), so each execution that uses the toolbelt gets its own toolbelt endpoint in a host-owned run directory, `<state>/runs/<key>/`, where the key is 16 hex characters of the execution ID's SHA-256 ([`management.Endpoints`](https://github.com/open-conduit/open-conduit/blob/main/internal/management/endpoints.go)):

- `toolbelt.sock`, owner-only `0600`, served by `Server.ExecutionHandler`. It answers only `POST /v1/toolbelt/call` and `GET /v1/toolbelt/guidance` (the agent guidance of the token's granted operations, which `os mcp` serves as its instructions); every other path is `404 not_found` before authentication, so operator endpoints are not served there to either credential class. An operator credential on the toolbelt call is `403 wrong_credential_class`, as an execution credential is on the management socket. Each request asserts the socket's execution (as `execution_id` would), so a token issued to any other execution fails the toolbelt's narrowing check (`403`), as does a call that asserts another execution.
- `token/token`, the execution's current token. After every claim, recovery and resume the scheduler issues the fresh token and calls `ToolbeltEndpoints` before the executor's `Run`, `Recover` or `Resume`. It writes the token through a temporary file in `token/` and a rename, so a process that sees `token/` through a directory mount sees the rotated file (a single-file bind mount pins the old inode). The executor receives the endpoint as `ExecutionContext.ToolbeltEndpoint` and the file as `ToolbeltTokenFile`, and points its process at both (`OS_ENDPOINT`, `OS_TOKEN_FILE`) instead of writing a token file of its own.
- `execution`, the execution ID, so a restarted daemon can tell whose run directory it is. A run directory that names another execution is refused.

The token, the lease epoch and the binding's allowed operations are checked on every call. The daemon's loop closes the socket and removes the run directory of every execution that is terminal or gone, including run directories an earlier daemon left. A parked execution keeps its run directory. On shutdown the daemon stops serving every socket and keeps the run directories; the next daemon serves a socket again when it adopts that attempt (recovery calls `ToolbeltEndpoints` with the rotated token).

A Unix socket path is limited to 104 bytes on macOS. When `<state>/runs/<key>/toolbelt.sock` would pass 100 bytes, the socket goes in `/tmp/open-conduit-<uid>/<key>/toolbelt.sock` instead, both directories owner-only and refused if they are not; the token stays in the run directory. If that path is too long as well, the attempt fails with `toolbelt endpoint unavailable` and the error names the path and asks for a shorter state directory.

The management socket is operator-only. An execution token is `403 wrong_credential_class` on every route there, `/v1/toolbelt/call` included, so it is served only by its execution's own `toolbelt.sock`. In-process executors (the GitHub hold executor) dial the endpoint in their `RunRequest` like any other, which is that socket. Test harnesses that call the toolbelt in process pass a `ToolbeltEndpoints` of their own. Earlier releases also served execution tokens there, so that an attempt launched by a daemon from before per-execution sockets (v1.8.0) kept its toolbelt when a newer daemon adopted it. An attempt still running from such a daemon loses its toolbelt on upgrade.

### Registered operations

The daemon registers each extension's operations on every generation's toolbelt and lists them in the config registry (`Registry.Operations`), so a binding may grant them:

- GitHub tool operations, with translated wait operations declared like `wait` ([engineering tools, waits and playbooks](../engineering-tools/)).
- Slack operations (`slack.context`, `slack.thread`, `slack.reply`) as a second extension registration ([Slack extension](../slack-extension/)).
- Introspection operations, only for a generation with an enabled `introspection` entry.

Translated emissions (`Operation.Emit`) are accepted under the same fence, replay and causal checks as `emit` ([issue reporting](../issue-reporting/)). The daemon narrows each binding's grant to the operations registered on the generation's toolbelt, so a binding that still lists the operations of a disabled extension keeps running (its calls are `undeclared`) instead of failing token issuance.

## Configuration and secrets

`config.Validate` strictly parses version 1 JSON: duplicate keys, unknown fields, nulls, types and the 1 MiB limit. It then applies defaults, validates the scope graph, inheritance, tombstones and stable slots, checks registrations and owners, references and authorization, and mechanical capabilities. It returns every independent diagnostic as a safe JSON pointer/code/remedy in deterministic order and never echoes values or reference text. It ports every rule and negative mutation of `scripts/check-config-contract.py`, and the exact invalid-capability diagnostic. A snapshot carries:

- a canonical hash
- redacted normalized config
- `DomainScopes` (scope metadata without reference text)
- `AuthorizeOperation`/`AuthorizeRead` ceiling checks

`config.Resolver` accepts only `keyring:`, `file:` (key in an owner-only `secrets.json`), `env:` and `cmd:`. The default keyring uses the platform helper; tests inject a backend. `cmd:` runs `sh -c` under a 10-second process-group deadline with 64 KiB bounded stdout. Stderr is discarded, the environment is minimal with no toolbelt or Open Conduit variables, exactly one trailing LF/CRLF is trimmed and empty output is rejected. Errors carry stable codes only. No resolution is cached across consumers. `SchedulerOptions.Secrets` resolves only the binding-selected names that both the pinned and the current generation grant. It runs immediately before Run/Recover/Resume, outside transactions; failure blocks that attempt with a safe diagnostic.

`management.ConfigService` restores the committed generation from coordination storage. The first start activates the config file as generation 1. With no config file either, it activates the default configuration (`bootstrap.DefaultConfig`: what `init setup` writes into an empty file, a [setup scope](../setup-sessions/#a-fresh-install)) as generation 1 without writing a file; the first apply writes it. A config file that exists but doesn't validate still stops the start. If the committed generation no longer validates at start (a release stopped registering an operation or executor it names), the config file is published over it as the next generation when the file validates, and the daemon logs the adoption; otherwise start fails naming both errors. Apply proceeds in this order:

1. Revalidate and check the candidate hash and expected generation.
2. Save the file atomically (same-directory 0600 temp, fsync, rename, parent fsync, `.last-good` backup).
3. Publish the generation, its normalized snapshot and changed scope records in one transaction.

A crash before that commit keeps the old generation, and a disk edit alone never changes running behavior. Removing or reparenting a scope with nonterminal Work returns `scope_in_use`. `OnApply` lets the host rebuild generation-pinned components. Toolbelt grants are the pinned binding operations, plus those of the [binding grants](../config-management/#binding-grants) the attempt's Work took, intersected with the current ceiling.

## Local management API

The server implements B05 API version 1 over HTTP/1.1 JSON on an owner-only Unix socket. `AcquireLock` takes an exclusive `flock` and rejects group/world-accessible or foreign-owned state directories. `Listen` removes a stale socket only after lock and ownership checks, rejects over-long paths and sets mode 0600.

Authentication:

- Every endpoint requires a bearer token.
- Operator credentials (digest-only records, bootstrapped under the lock into `private/operator.token`) never reach `/v1/toolbelt/call` or `/v1/toolbelt/guidance`. Execution tokens reach only those endpoints.
- Rotation atomically revokes the caller and returns a replacement once; it is never stored in idempotency history. `RecoverOperator` is the locked local path after a lost response.
- Revocation by token ID covers execution tokens and the caller's own operator token.

Request handling:

- Mutations require `application/json`, a body of at most 1 MiB, strict fields (no unknown, duplicate or null fields) and no query string.
- Every mutating POST except validate, rotate, route trace and toolbelt calls requires `Idempotency-Key`. The key is bound to principal, method, path and canonical body hash, and kept for 30 days. A replay returns the original status and body; a changed body is `409 idempotency_conflict`.
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

**Route trace.** `POST /v1/scopes/{id}/route/trace` takes a hypothetical Event (`type`, optional `source`, which defaults to `core.manual`, `correlation_key`, `work_id` and `data`) and returns how the router would handle it now, against the active generation and current Work state. It returns each effective route in order with its verdict (`match`, `skip`, `not_reached`, or `bypassed` when the Work is waiting or finished) and the handler's reason, the Work the Event resolves to, and the `effect` (`start`, `queue`, `inject`, `interrupt`, `wake`, `drop` or `unmatched`) with a short detail. `also` lists other wakes: Work it would wake through a dependency subscription, or another input waking its own Work first. Nothing is accepted, routed or recorded, so it takes no `Idempotency-Key`. An Event acceptance would refuse is refused with `422 invalid_request` rather than explained: a type or payload schema that is not namespaced, data that is not JSON, a core type or a core source other than `core.manual` and `core.action` (an operator's Event and an action's, which the host accepts for the operator; `runtime.CheckTraceEvent`), a registered source that belongs to another scope (`Ingress.AdmitsTrace`), a package payload the package may not write, and an unknown source: one that is not `core.manual`, no ingress registers and whose type no in-process extension or package owns. `GET /v1/scopes/{id}/route/samples` lists the named sample Events the scope's route handlers offer, so a view can show "Suppose…" choices without hard-coding them. `open-conduit route --trace TYPE|SAMPLE --scope S` and `route --samples --scope S` call these endpoints.

Rebind, hold/release, attach and scope reconcile are described in [operator setup and controls](../operator-setup/).

Control checkpoints keep the Work in the envelope but use an empty routed association, so earlier runnable input cannot delay fencing. `artifacts/{id}/content` streams through `storage.Artifacts` and supports one byte range.

The client dials only the configured socket, with the fixed host `localhost`, no proxy and no redirects. `open-conduit` resolves the endpoint (`--endpoint`, then `OPEN_CONDUIT_ENDPOINT`, then socket discovery) and the token file (`--token-file`, then `OPEN_CONDUIT_TOKEN_FILE`, then `state/private/operator.token`). It supports, among other commands:

- health
- scopes, work, events (including causal `events chain`), executions, waits and artifacts (`artifacts cat` for logs)
- emit, work create/cancel/retry/wake and execution cancel
- config validate/apply
- auth rotate/revoke
- unified run history (`runs list|get|table|log`), `doctor` and `tui` ([observability and history](../observability-history/))

## Evidence

- The [toolbelt integration cases](https://github.com/open-conduit/open-conduit/blob/main/integration/toolbelt_test.go) run every shared toolbelt conformance case against real scheduled attempts on persistent and reopened SQLite. They add issue-on-claim/recovery supersession, digest-only storage, live ceiling narrowing, revocation by ID, request replay and conflict, extension handler at-most-once, forged source/correlation refusal, and declared wait/outcome handshakes.
- The [execution endpoint cases](https://github.com/open-conduit/open-conduit/blob/main/integration/endpoints_test.go) run real scheduled attempts through the per-execution sockets: each socket serves only its own token, another execution's token and a call asserting another execution are refused, operator endpoints are not served and operator credentials are refused, recovery rotates the token in its token directory and refuses the old one, the socket is served again after a restart for the adopted attempt, and a terminal attempt's run directory goes while a live one's stays. The [endpoint unit tests](https://github.com/open-conduit/open-conduit/blob/main/internal/management/endpoints_test.go) cover the short-path fallback, the over-long path error and an unsafe fallback directory. The daemon kill drill checks the real daemon: a detached shell process's socket and token file are in one run directory, the restarted daemon serves the socket with a rotated token and refuses the old one, and the run directory is removed once the attempt ends.
- The [management integration cases](https://github.com/open-conduit/open-conduit/blob/main/integration/management_test.go) start the real server on a Unix socket and drive it only through the CLI and client. They cover socket permissions and lock exclusion, credential-class separation, the causal chain from operator root to toolbelt emission, execution cancel with idempotent response-loss replay, confirmed-stop hold and manual retry, artifact range reads, Work cancel, rotation/revocation/lease expiry, config validate/apply with diagnostics, stale-generation conflict, immediate narrowing of a live token, atomic save/backup, restart restoring the committed generation despite a disk edit, pagination with tamper/filter mismatch, strict request validation, toolbelt wait plus operator wake, and the absence of secrets/references/tokens in responses and coordination storage.
- [Config tests](https://github.com/open-conduit/open-conduit/tree/main/internal/config) cover the four examples, the invalid-capability diagnostic, every oracle mutation, all four secret schemes and file safety.

These cases are local in-process and socket evidence; they do not exercise a real keyring. Daemon restart and multi-process drills are described in [daemon composition](../daemon-core-gate/).

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 02-06 implemented toolbelt authority, configuration and secrets, the management server, the client and the operator commands. Composing a long-running daemon was left to the core gate (02-09), which also clarified that a type-only `wait` subscribes on the Work's keys and that an unapplied declaration survives recovery. Shell environment construction and the `os` CLI transport were left to 02-07. The 02-06 evidence made no daemon-restart, multi-process or real-keyring claims.
- Ticket 02-08 added `runs`, `doctor` and `tui`. Ticket 03-05 added rebind and scope reconcile endpoints, together with hold/release and attach.
- Ticket 03-04 registered the GitHub tool operations, added translated wait operations and made handler refusals return `422`.
- Ticket 04-01 registered the Slack operations.
- Issue #162 gave each execution its own toolbelt socket and token directory in a host-owned run directory, a prerequisite for process sandboxes (#160). Before it, every execution's toolbelt was the shared `management.sock`, and executors wrote the token to a single file in their own segment directory.
- Issue #364 added gates, so an extension can put an operator's policy (a person's approval) in the host's path for any operation.
- Ticket 03-08 added translated emissions, passed the configured causal limits to the toolbelt and narrowed binding grants to registered operations. Before it, an execution's first emission off a new root pinned the default causal limits.
