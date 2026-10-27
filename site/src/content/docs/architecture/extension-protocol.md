---
title: "Extension subprocess protocol"
---

Decision B08 · ticket 07-01 · **provisional, for user review**. The executable definition is package [`protocol`](https://github.com/open-conduit/open-conduit/blob/main/protocol/protocol.go): the version, the [manifest](https://github.com/open-conduit/open-conduit/blob/main/protocol/manifest.go) and its validation, and the [method catalogue](https://github.com/open-conduit/open-conduit/blob/main/protocol/methods.go). Each wire method names the public contract it carries and the shipped first-party users that needed it. The [transport prototype](https://github.com/open-conduit/open-conduit/tree/main/prototypes/transport) supplies the evidence (`make protocol-prototype`). 07-02 implemented loading, handshake, supervision, the message types, the trust model and payload schema evolution ([below](#runtime-trust-model-and-schema-evolution-07-02); also provisional). 07-03 added daemon wiring, the remaining adapters, packaging, the SDK's final shape and certification: see [Extension packaging, SDK and certification](../extension-packaging/) (also provisional).

The protocol was extracted from the code that shipped through 05-04 and the 06-01/06-02 code: the GitHub, Slack, Linear, Jira, Bitbucket, schedule, introspection and PostHog extensions, the Git resource provider, and the shell and claude-code executors. It was not designed in the abstract. It adds **no core primitive**. Every method is an adapter over an existing public contract (`extension`, `storage` KV and reads, `observability`), or over a host composition hook the daemon already drives for in-process extensions. First-party extensions stay in-process. The user is the only protocol consumer, so the protocol is cheap to revise.

## Decision summary

| Question | Decision (provisional) |
| --- | --- |
| Transport | JSON-RPC 2.0 over the extension process's stdin/stdout, one JSON object per line, standard library only, symmetric (both sides send requests). Not go-plugin/gRPC. |
| Version | Integer protocol versions. `initialize` picks the highest common version. No common version is a deterministic refusal before any other call (`unsupported_protocol_version`). Version 1 is the first. |
| Manifest | `open-conduit-extension.json` beside the binary: name, own version, command, and `provides` (mechanical declarations only). Validated offline. The handshake must return the same manifest. |
| Capabilities | Executor labels are exactly the PRD's (`interrupt`, `inject`, `resume`, `workspace`, `toolbelt: none\|cli\|mcp\|sdk`), plus optional controls (`stop`, `forget`, `attach`) that are methods, not suitability claims. Unknown fields are rejected, so a semantic label cannot be expressed. |
| Storage | Mandatory namespaced KV under the extension's name, always available and never negotiated (a `storage` field is rejected). A store larger than coordination state is an `external_stores` entry naming a connection type, so it is explicit in config. |
| Transactions | The host never holds a coordination transaction across an open-ended call. Four pure callbacks run inside a transaction today (`recurrence.next`, `work_kind.trigger`, `dependency.filter`, `emit.commit`). They are bounded (250 ms) and may make no callback. KV writes from an extension are one atomic `kv.commit` with preconditions. |
| Process model | Extension processes are disposable. Durable state lives in host KV, executor recovery metadata or the extension's own state directory, so a crash or host restart means restart plus recover. The host never reattaches to an old process. |

## Inventory of shipped interactions

The table groups what the first-party code actually does across the host boundary today, as found in `internal/bootstrap` and the extension packages. Users: GH GitHub, SL Slack, TR tracker kit (Linear and Jira), BB Bitbucket, SC schedule, IS introspection, PH PostHog, GIT Git provider, SH shell, CC claude-code.

| Interaction | In-process surface today | Users | Wire method(s) |
| --- | --- | --- | --- |
| Config validation | `config.Registry` validators (connection, extension, route `HandlerConfig`, `ResourceConfig`, executor `ValidateConfig`) | all | `validate` |
| Config apply and source registration | `register<Extension>` on apply: parse, build client, `ingress.Register(name, Source, adapter, interval)`, mount webhook paths | GH SL TR BB SC IS PH | `configure` (returns sources and webhook paths) |
| Ingress push | `Ingress.Push` behind host-mounted `WebhookHandler`s (verify, then push; 2xx only after durable acceptance) | GH SL TR BB SC | `webhook.handle`, `ingress.push` |
| Ingress reconcile | `Ingress.Reconcile`, driven by `ReconcileDue` with page timeout and item/byte limits | GH SL TR BB SC | `ingress.reconcile` |
| Extension-owned receiver | Slack Socket Mode calls the host push func and acks the envelope only after it returns | SL | `ingress.deliver` (E→H) |
| Routing | `RouteHandler.Match` on a detached snapshot, outside any transaction | GH SL TR BB SC IS | `route.match` |
| Work kind policy | `WorkKindPolicy{Standing, Trigger}`. `Trigger` is pure and runs in the start transaction | SC (IS standing only) | manifest `work_kinds`, `work_kind.trigger` |
| Calendar recurrence | `RecurrenceFunc` in the timer fire-and-rearm transaction. `Armer.ArmRecurrence/Cancel` from `Ensure` | SC IS | `recurrence.next`, `timer.arm`, `timer.cancel` |
| Toolbelt operations | `Registration{Name, EventTypes, Operations, DependencyFilter}`. `Operation.Handler` (all), `.Wait` (GH), `.Emit` with in-transaction `Commit` (IS) | GH SL TR BB IS | `operation.call`, `operation.wait`, `operation.emit`, `emit.commit`, `dependency.filter` |
| Prompt rendering | bootstrap `renderEvents` chain over `<extension>.RenderEvent` | GH SL IS | `event.render` |
| Executor lifecycle | `Executor.Run/Recover`, `RunHandle.Wait`, `Resume`, `Interrupt`, `Inject`, `Forget`, `PersistRecovery` | SH CC LJ (GH hold) | `executor.*`, `execution.persist_recovery` (E→H) |
| Physical-stop evidence | bootstrap's private `stopper` interface (`Stop(execution, epoch)`), so the host can `ConfirmStopped` | SH CC LJ | `executor.stop`. **Promoted** to public `extension.StopExecutor` |
| Operator attach | bootstrap special-cased claude-code (`Attach`, `ErrNoSession`, wire `no_session`) | CC | `executor.attach`. **Generalised** to `extension.AttachExecutor`, `ErrNothingToAttach`, wire `nothing_to_attach` |
| Workspaces | `ResourceBroker.Acquire/Lookup` (executor side). `ResourceProvider.Acquire/Lookup/Reconcile` with the host's `ResourceStatusFunc` | CC LJ, GIT | `resource.*` (E→H), `provider.*`, `provider.owner_status` (E→H) |
| Namespaced KV | `Store.Transact` with `Get/Put/Delete/List` in the extension's namespaces (`github.notes`, `github.projection.*`, `executor.claudecode`, `schedule.runs`, `introspection.*`, `<type>.tools.*` …) | GH SL TR BB SC IS CC | `kv.get`, `kv.list`, `kv.commit` |
| Read-only state | `Work`, `ListWorks`, `ListExecutions`, `Event`, `Events` (cursor), `ListScopes`/`Scope`, `ListTimers` | GH SL TR BB SC IS CC | `state.*` |
| Derived and root events with records | `runtime.Causal.Derive` (GitHub turns), `AcceptDerivedInTx` (schedule translation, job runs), `AcceptInTx` + `PutOriginInTx` (host-watch reports) | GH SC IS | `event.accept` (E→H, KV records join the transaction) |
| Control | `Control.CancelWork` for retired introspection Work | IS | `work.cancel` (E→H) |
| Artifacts | `Artifacts.Create`, write, `Commit` (transcripts, logs, output files) | SH CC LJ | `artifact.*` (E→H) |
| Secrets | `connectionSecrets` and `resolver.ResolveNames` (lazy, current generation) | GH SL TR BB SC PH | `secret.resolve` (E→H) |
| Background passes | `Projector.Converge` (GH SL TR), `Ensure` (SC IS), `Executor.Maintain` (CC), `manageHooks`/`EnsureWebhook` (GH TR BB), `hostWatch` (IS), `publishJobRuns` (SC) | GH SL TR BB SC IS CC | manifest `tasks`, `task.run` |
| Telemetry export | `observability.Exporter.Export/Shutdown` on the pipeline goroutine | PH IS | `telemetry.export`, `shutdown` |
| Doctor | `runtime.Check` functions (`Checks`, `DeepChecks` with secrets) | all but SH | `check.run` |
| Annotations | management `Annotate` | GH | `work.annotate` |
| Setup | `extension.Setup` with `SetupHost` prompts, browser, secret store, `LookPath`, `Run` | GH SL TR | `setup.run` plus `setup.*` callbacks |
| Playbooks | embedded playbooks written into the agent executor's plugin directory | GH SL TR IS | manifest `playbooks` (first-party convention) |

`TestCatalogueCoversThePublicContract` checks mechanically that every method of every public interface an extension implements or consumes is carried by a wire method. `TestCatalogueMapsEveryMethodToAShippedUse` checks that every wire method names at least one shipped user. Core-record writes (`AcceptEvent`, `PutWork`, leases, timers, waits) never cross the boundary. They are reached only through `event.accept`, `timer.arm` and `work.cancel`, which apply the same causal limits and fencing as in-process callers.

### What does not cross the boundary

Some couplings exist only as bootstrap glue between first-party packages. They have no protocol equivalent, because a subprocess extension may depend on another only through events, correlation keys and public state reads:

- claude-code's handoff `FeedbackHook` reads and writes GitHub comments (`githubFeedback`);
- GitHub `job:` waits resolve schedule job keys (`scheduleJob`), and the schedule translator decodes GitHub delivery payloads;
- Bitbucket reads the tracker's reopen generations;
- PostHog receives introspection telemetry through a fan-out;
- bootstrap parses Slack thread keys for the socket's active-thread filter. That one can move into the extension as a `state.works` read.

These stay in-process. They are allowed by the PRD ("first-party extensions in-process behind the public interfaces"). They also decide which integrations 07-03 can round-trip without change.

In-process extensions also hold the whole `storage.Store` and the daemon's clock and dry-run flag. A subprocess gets only its KV namespaces, scoped reads, its own clock, and `dry_run` in `configure`.

## Transport comparison

The prototype runs one workload over both candidates: the same extension-side methods (`scenario.Extension`) and host callbacks (`scenario.Host`). Each transport is a separate binary.

- **JSON-RPC.** 345 lines, standard library only.
- **go-plugin.** 229 lines on go-plugin v1.7.0 with gRPC v1.61.0. protoc is not in the toolchain, so its services are hand-written `grpc.ServiceDesc`s that carry the same JSON envelope in protobuf `BytesValue`s. A real implementation would add generated protobuf types and a protoc step.

The prototype lives in its own Go module (`prototypes/transport`), so go-plugin and gRPC never enter `go.mod`.

Both transports pass the same semantic checks, `TestTransportCompatibility`:

- handshake and manifest;
- deterministic version-mismatch refusal;
- 2,000 sequential echoes, and 32×50 concurrent echoes matched to their responses;
- structured errors carried verbatim, and a distinct unknown-method code;
- 1,000 nested extension-to-host callbacks while serving one host request;
- a 16 MiB chunked artifact upload acknowledged per chunk;
- an extension-initiated push answered by the host;
- prompt cancellation that reaches the extension's handler;
- stray stdout from extension code that leaves the stream intact;
- a crash mid-call reported as a transport error (not a hang, not an authored error), with later calls failing the same way and a restarted process serving again.

Measured on darwin/arm64 with Go 1.27.3, without `-race`. Representative run:

| Transport | Cold start | Warm restart | Version mismatch | Echo p50 | Echo p99 | Host callback | Artifact 16 MiB / 64 KiB chunks | Cancel seen by extension | Crash detected | Binary | Module deps |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| JSON-RPC over stdio | 105 ms | 10 ms | 6 ms | 32 µs | 61 µs | 29 µs | 143 MiB/s | 173 µs | 1.0 ms | 3.8 MiB | 0 |
| go-plugin over gRPC | 231 ms | 20 ms | 12 ms | 51 µs | 179 µs | 55 µs | 92 MiB/s | 155 µs | 1.5 ms | 17.8 MiB | 14 |

Cold start includes the OS's first launch of a new binary and varies between runs (100–350 ms for both). Under `-race`, both slow by about 3× and keep the same ordering.

| Property | JSON-RPC over stdio | go-plugin over gRPC |
| --- | --- | --- |
| Dependencies and toolchain | None. The repository already speaks JSON-RPC 2.0 over stdio, hand-written, in `os mcp` (`internal/cli/mcp.go`) | go-plugin, gRPC, protobuf, yamux and hclog (14 modules), plus protoc and codegen for typed messages |
| Language neutrality | Any language with JSON and pipes. A shell or Python extension is plausible | Needs gRPC and go-plugin's stdout handshake line, mTLS and broker conventions. In practice Go |
| Extension-to-host calls | Native: either side sends requests on one stream. Nested callbacks work, provided the reader is never blocked (see flow control) | Needs the GRPCBroker to open a second multiplexed connection to a host-served service |
| Versioning | Explicit `initialize` negotiation over a version list | Handshake cookie plus one `ProtocolVersion` per client config. Several versions need `VersionedPlugins` or a retry per version |
| Cancellation | `$/cancel {id}` notification, the LSP convention; implemented in the prototype | Native gRPC context propagation |
| Streaming | Request/response per chunk. Simple and acknowledged | Native gRPC streams (not used, so the comparison stays fair) |
| Stdout hazard | Stray stdout corrupts the stream. The SDK moves `os.Stdout` to stderr before serving (proved). Non-Go child processes must be told the same | Stdout is only for the handshake line; after that go-plugin captures it |
| Reattach after host restart | Not offered | `ReattachConfig` is offered, but it is unnecessary here: processes are disposable and executions are adopted through `executor.recover` |
| Debuggability | Human-readable lines; a session can be replayed from a log | Binary frames |

**Decision: JSON-RPC 2.0 over stdio.** It meets every semantic the shipped workloads need with no new dependency. It is faster on this workload, and it is symmetric, which the inventory shows is essential: KV, state reads, artifacts, persist-recovery, resources, push and setup are all extension-to-host calls made while serving a host call. It is language-neutral without codegen, and it matches the repository's existing MCP implementation.

go-plugin's real advantages are native cancellation and streaming, and built-in reattach. The first two cost about 20 lines in the prototype. The third is not needed by this process model.

What would reverse the decision: a need for very high-throughput streaming (the per-chunk acknowledgement already gives 143 MiB/s), or a third-party ecosystem that standardises on go-plugin. Neither exists, and the user is the only consumer.

## Wire format

- **Framing.** UTF-8 JSON, one object per line (`\n`), at most `MaxMessageBytes` (16 MiB; above the runtime's default 8 MiB reconcile page). A longer line or invalid JSON is a protocol error that closes the connection. stderr is the extension's diagnostic log. In 07-02 the host will capture it to a bounded per-extension log under the state directory.
- **Messages.** JSON-RPC 2.0 requests (`id`, `method`, `params`), responses (`result` or `error`) and notifications (no `id`). Ids are unique per sender while in flight. Responses may arrive in any order.
- **Payloads.** Wire types are snake_case JSON defined in package `protocol` by 07-02. They mirror the management API's wire shapes (`client`) where those exist. `domain.Payload.Data` is raw JSON. Byte fields (delivery bodies, artifact chunks) are base64.
- **Errors.** Transport errors use the JSON-RPC reserved codes: `-32700` parse, `-32601` method not found, `-32602` invalid params, `-32603` internal, `-32800` cancelled. Anything authored uses `-32000` with `data.code`, a stable `[a-z][a-z0-9_]*` code. The protocol's codes (`protocol.Code*`) map one-to-one onto the public sentinel errors (table below). An operation handler's `OperationError` code and message cross verbatim. Unauthored errors cross as `internal` with no detail; the detail stays in the side's own log, as the management API already does.

| Code | Go error |
| --- | --- |
| `invalid` | `extension.ErrInvalid` |
| `unsupported` | `extension.ErrUnsupported` |
| `unauthorized` | `extension.ErrUnauthorized` |
| `undeclared` | `extension.ErrUndeclared` |
| `not_found` | `storage.ErrNotFound` |
| `conflict` | `storage.ErrConflict` / lease fence |
| `causal_limit` | runtime causal rejection |
| `not_adoptable` | `extension.ErrNotAdoptable` |
| `not_accepting` | `extension.ErrNotAccepting` |
| `nothing_to_attach` | `extension.ErrNothingToAttach` |
| `resource_setup_failed` | `extension.ErrResourceSetup` (the resource still returned) |
| `resource_not_found` | `extension.ErrResourceNotFound` |
| `resource_exhausted` | `extension.ErrResourceExhausted` (the provider's message, which says what to free, crosses with it; 08-02) |
| `retry_after` | `extension.RetryHint` (`data.reset_at` optional) |

The protocol also has its own codes: `unsupported_protocol_version`, `reentrant_call`, `overloaded`, `cancelled`, `shutting_down` and `internal`.

## Lifecycle

1. **Discovery.** The host reads `open-conduit-extension.json` from each extension directory. 07-02 decides the install location; 07-03 decides install/update/removal. `ParseManifest` validates it strictly and offline, so `config validate` never starts a process. Conflicts with first-party or other registrations (names, types, operations, families) are refused atomically, as `Registrar` does in-process.
2. **Start.** The host runs `command` with a sanitized environment (the B05 baseline, no secrets), the protocol on stdin/stdout, and the extension's private state directory. The process is a child of the daemon, in its own process group.
3. **`initialize`.** The host sends `{protocol_versions, host: {name, version}, state_dir}`. The extension returns `{protocol_version, manifest}`. The host refuses a manifest that differs from the file, or no common version. No other method is valid before `initialize` succeeds.
4. **`configure`.** The host sends the current generation's entries, connections (secret references only), `public_url` and `dry_run`. The extension returns the ingress sources and webhook paths it registers (under its own namespaces). It is sent again on every config apply and tunnel URL change, without a restart.
5. **Running.** The host calls the surfaces the manifest enables (`Manifest.Enabled`): routing, operations, executor and provider methods, and `task.run` at each declared interval. The extension calls the host's KV, state, artifact, secret, timer, event and resource methods, and `ingress.deliver` from its own receivers.
6. **Shutdown.** The host sends `shutdown`. The extension answers `shutting_down` to new requests, finishes or cancels in-flight work, and exits. Then the host closes stdin, sends SIGTERM after a grace period, and SIGKILL. An extension must also exit when stdin reaches EOF, so a host crash never leaves an orphaned extension serving nobody.

An executor extension's *executions* are separate. Its detached processes survive the extension and the daemon, exactly as the shell and claude-code executors' processes do today. They are adopted through `executor.recover` with their recovery metadata, never by reattaching to the old extension process.

## Cancellation

Either side cancels an in-flight request with the `$/cancel {id}` notification. The canceller returns `context.Canceled` at once and discards a late response. The receiver cancels the handler's context and answers `-32800`, if it answers at all. Cancellation means "stop serving this request". It never means "undo":

- cancelling `executor.wait` cancels observation only. The execution keeps running, as `RunHandle.Wait` documents.
- `executor.interrupt` and `executor.stop` are the controls that act on a process.
- a cancelled `operation.call` may already have made its external write. Its `request_id` (`CallRequestID`) makes a retry idempotent, and adapters keep their reconcile markers.

The host cancels everything in flight when it stops. Ingress pages run under the runtime's page timeout, and in-transaction methods under their own deadline (below).

## Flow control

- **Per-call acknowledgement** is the backpressure for streams. An artifact is written in chunks of at most 256 KiB (`MaxChunkBytes`), each acknowledged before the next. The prototype moves 143 MiB/s with 64 KiB chunks. Reconcile is paged by the runtime's existing item and byte limits. Telemetry batches are requests with a deadline: a slow exporter fills the host pipeline's bounded buffer, and the host drops with accounting exactly as it does for in-process exporters, never blocking scheduling.
- **Bounded concurrency without blocking the reader.** Each side serves at most `MaxInflight` (64) requests at once. Over the bound it answers `overloaded` instead of pausing its reader. The reader also delivers the responses that in-flight handlers' nested callbacks wait for, so pausing it deadlocks. The prototype found this, and the bound is implemented that way. The caller retries `overloaded` with backoff.
- **Message bound.** `MaxMessageBytes` caps one message. Anything larger, such as a big transcript, is an artifact, which is the existing rule against large coordination payloads.

## Transactions and in-transaction calls

The host never makes an open-ended call while it holds a coordination (SQLite) transaction. A hung extension would otherwise stall every writer. The inventory found four pure callbacks that core runs inside a transaction today:

- `RecurrenceFunc`, in the fire-and-rearm transaction;
- `WorkKindPolicy.Trigger`, in the start transaction;
- `DependencyFilter`, in the subscription transaction;
- `EmitPlan.Commit`, in the emission transaction. It needs the accepted Event's ID, sequence, root and depth, which exist only inside the transaction.

Over the protocol these are **bounded in-transaction methods** (`Method.InTransaction`):

- they must be pure and deterministic;
- the host enforces a hard deadline (`InTransactionDeadlineMillis`, 250 ms, against a measured p99 round trip of 61 µs);
- any callback while serving one is refused with `reentrant_call`;
- a timeout or transport failure aborts that transaction with a retryable error, and the work stays pending.

07-02 may move the first three to evaluation before the transaction, with in-transaction revalidation, where that is simpler. `emit.commit` keeps the bounded call, because its records must join the emission atomically.

Extension KV writes are never interactive transactions:

- `kv.get` returns a value and a version (a digest of the value, or absent);
- `kv.commit` applies preconditions (`version` or `absent`) and puts/deletes as one atomic unit. A failed precondition is `conflict`, and the extension re-reads and retries;
- `event.accept` takes the same checks and records, so an Event and its index records commit together. That keeps the PRD's "KV writes join the same atomic unit as an event emission".

This is an adapter over `storage.Tx` (`Get`, compare, `Put`/`Delete` in one `Transact`), not a new storage capability.

## Crash and reconnect

A broken pipe or process exit fails every pending request with a transport error (detected in about 1 ms in the prototype). It is never a hang or an authored error. The host restarts the process with backoff, then runs `initialize` and `configure` again. What a crash means depends on the call:

| In flight | Outcome |
| --- | --- |
| `webhook.handle` / `ingress.push` | Nothing accepted, so the delivery is not acknowledged (non-2xx). The service retries, and reconcile is the backstop. |
| `ingress.deliver` (receiver) | The extension acknowledges upstream only after the host's answer. A crash before that leaves the delivery unacknowledged, and the service redelivers. |
| `ingress.reconcile` | Partial scan: no absence is inferred and no checkpoint advances. The runtime retries the generation. |
| `route.match` | The routing pass fails and the Event stays pending. Routing is deterministic and retried. |
| `operation.call` / `.wait` / `.emit` | The toolbelt caller gets a retryable internal error. Its `request_id` makes the retry idempotent. A declared wait or emission either committed or did not. |
| In-transaction method | The transaction aborts and is retried after restart. |
| `executor.*` | The extension's crash is not the execution's failure. After restart the host calls `executor.recover` with the persisted recovery metadata, as after a daemon restart. `not_adoptable` leads to `executor.stop`, `ConfirmStopped` and retry policy. Old per-execution tokens stay fenced by lease epoch. |
| `provider.*` | Acquisition is idempotent per owner and mode, and `provider.reconcile` repairs the rest (the existing contract). |
| `task.run`, `check.run`, `telemetry.export` | Reported, and retried next pass. A dropped telemetry batch is accounted. |

If the *host* crashes, extensions see EOF and exit. The new daemon starts them again. Durable progress is all in host storage, recovery metadata or the extension's own state directory, so there is nothing to reattach to.

## Authority and scoping

The trust model is defined in [07-02's section](#trust-model). Process separation is not sandboxing: an extension runs with the operator's user privileges. 07-01 fixes the protocol-level scoping, which the host applies to every extension-to-host call from the identity it bound at start. The extension never supplies that identity.

- **KV.** Only the extension's own namespaces: its name, or `name.` + suffix (`protocol.Namespace`, the existing `extension.Record` rule).
- **Families, operations, kinds, recurrences, types, handlers, executors, providers, tasks.** Only names under its own name (`Manifest.Validate`).
- **Source namespaces.** Its own name, or `name/` + suffix (`protocol.SourceNamespace`, the `EmitPlan` rule).
- **State reads.** Provisionally limited to scopes whose config uses the extension. This covers every shipped read: projectors filter Works by their own correlation sources, and the schedule translator reads its configured scopes.
- **`secret.resolve`.** Only the connections bound to the extension's configured entries (B05 isolation).
- **`operation.call`.** Receives the validated `Authority`, never the bearer token, exactly as in-process handlers do. Executors receive the per-execution endpoint and token in `RunRequest`, as today.
- **`event.accept` and `work.cancel`.** Apply the same declared-family, causal-limit and own-kind checks as their in-process callers.

## Manifest

`protocol.Manifest` is the specification. Its example manifests are in `protocol/testdata`:

- [`ingress-support.json`](https://github.com/open-conduit/open-conduit/blob/main/protocol/testdata/ingress-support.json): a workspace-free ingress workload shaped like Slack. It declares no executor and no resource, and enables no execution method.
- [`executor-workspace.json`](https://github.com/open-conduit/open-conduit/blob/main/protocol/testdata/executor-workspace.json): an execution workload. A resumable, injectable executor with stop, forget and attach controls, plus the workspace provider it runs in.
- [`scheduled-reporting.json`](https://github.com/open-conduit/open-conduit/blob/main/protocol/testdata/scheduled-reporting.json): timers, standing Work with trigger selection, and a committed emission.

Validation rules:

- the protocol list includes a supported version;
- the name is an identifier and not reserved (`core`, `execution`, `manual`, `os`, `open-conduit`);
- identifier-shaped names are `name` or `name-…`, and dotted names are `name.…`, with no duplicates;
- a non-re-derivable family states its loss, and a re-derivable one cannot;
- an ingress type declares families, and webhooks need an ingress type;
- work kinds need a route handler;
- operation kinds are `handler`, `wait` or `emit`, and only emit may `commit`;
- `dependency_filter` needs a wait operation;
- toolbelt labels are `none`, `cli`, `mcp` or `sdk`;
- resource modes are distinct `shared`, `work` or `fresh`;
- task intervals are at least one second;
- the command and playbooks stay inside the extension directory;
- external stores name a declared connection type;
- payload schema bases (07-02) are dotted names under the extension's name without a version suffix, with positive, strictly increasing readable versions; `kv_version` is not negative;
- unknown fields are rejected.

`TestManifestRejections` covers each rule, including a `storage` capability and a semantic executor label.

The same format and the same catalogue cover both workload shapes. Nothing in core distinguishes an "ingress extension" from an "executor extension". A manifest enables the methods of the surfaces it declares, and the host registers those surfaces through the same runtime interfaces it uses for in-process extensions (`TestExampleManifests`, `TestEveryMethodIsReachable`).

The first-party extensions are not given manifests. They stay in-process. A third-party extension's config entries are validated through `validate`; 07-03 owns any conversion story.

## Contract changes made in 07-01

- `extension.StopExecutor`: the daemon's private `stopper` interface, now public, because an external executor must be able to supply physical-stop evidence. Shell and claude-code implement it unchanged.
- `extension.AttachExecutor`, `extension.AttachSpec`, `extension.ErrNothingToAttach`: attach is now a public optional executor method. Bootstrap no longer branches on claude-code. The management wire code `no_session` becomes `nothing_to_attach` (05-04's note), and `claudecode.ErrNoSession` wraps `ErrNothingToAttach`. The unused attach environment that claude-code computed is gone; the operator's shell supplies it, as before.
- `extension.ContractVersion` still versions the in-process API. `protocol.Version` versions the wire.

## Runtime, trust model and schema evolution (07-02)

Ticket 07-02 · **provisional, for user review**. Code:

- [`protocol`](https://github.com/open-conduit/open-conduit/blob/main/protocol/wire.go): the snake_case wire types and method params ([messages](https://github.com/open-conduit/open-conduit/blob/main/protocol/messages.go)), the [error mapping](https://github.com/open-conduit/open-conduit/blob/main/protocol/errors.go) onto the public sentinels, and [schema evolution](https://github.com/open-conduit/open-conduit/blob/main/protocol/schema.go).
- [`protocol/jsonrpc`](https://github.com/open-conduit/open-conduit/blob/main/protocol/jsonrpc/conn.go): the transport, hardened from the prototype.
- [`protocol/sdk`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/sdk.go): a minimal extension-side runtime that serves the public Go contracts and gives the extension a [`Host`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/host.go) client. 07-03 decides the SDK's final shape.
- [`internal/extproc`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/discover.go): the host runtime. It covers discovery, conflicts, the [supervised process](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/process.go), the [scoped host service](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/service.go) and the [adapters](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/adapters.go).
- Examples:
  - [`protocol/examples/notes`](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/notes/notes.go) is the example external extension. It covers ingress, a route, operations, an emission with committed records, a recurrence, tasks, three releases and a KV migration.
  - [`protocol/examples/runner`](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/runner/runner.go) is the example external executor: detached jobs with inject, interrupt, stop and forget.

### Loading

- **Install location.** `<config dir>/extensions/<name>/open-conduit-extension.json`, beside `config.json` (`extproc.Dir`). The directory must be named after the manifest's name. Install, update, rollback and removal are 07-03's (`open-conduit extension …`).
- **Discovery.** `extproc.Discover` parses every manifest strictly and offline. Invalid or misnamed manifests are reported and skipped. A missing root means no extensions.
- **Conflicts.** `extproc.CheckConflicts` checks a whole set atomically against names already taken in-process, and against each other. The checked kinds are the name, config types, handlers, executors, resource types, operations, families, work kinds, recurrences and schemas. Either every extension is admitted or none is.
- **Start.**
  - The command runs in the extension directory, in its own process group.
  - The environment is the B05 baseline only (`PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `TERM`). No credential or Open Conduit setting reaches it.
  - stderr goes to a bounded log, `<state>/logs/extensions/<name>.log`. It is truncated with a marker at 1 MiB.
  - The private state directory is `<state>/extensions/<name>`.
- **Handshake.**
  - `initialize` refuses the extension when there is no common version, or when the returned manifest differs from the file. The comparison is semantic, not byte-for-byte.
  - `configure` sends the generation's entries and `kv: {stored, declared}`. It checks that every registered source name, source namespace, family and webhook path stays in the extension's namespace and matches the manifest.
  - `Process.Reconfigure` re-sends configure without a restart (config apply, tunnel URL change).
  - Deterministic refusals (`extproc.ErrRefused`) are never restarted: a version mismatch, a manifest mismatch or a KV downgrade.
- **Supervision.**
  - A crash restarts the process with exponential backoff (100 ms to 30 s; the backoff resets after a minute of uptime), followed by `initialize` and `configure`.
  - Open artifact writers are aborted.
  - `Process.Call` waits, bounded by its context, for a running process, but never retries a call. A crash mid-call is `protocol.ErrTransport`, and the caller applies the crash table above.
  - `Close` sends `shutdown`, closes stdin, then sends SIGTERM and SIGKILL to the group.
  - The SDK exits on stdin EOF, so a host crash never orphans an extension.
- **Adapters.** `extproc.Remote` presents a process through the same interfaces in-process extensions implement:
  - `Ingress(SourceDecl)` is an `extension.Ingress`;
  - `RouteHandler`;
  - `Registration()` is an `extension.Registration` for the toolbelt, with handler, wait and emit operations, `emit.commit` and `dependency.filter`;
  - `Executor(name)` implements every optional executor interface;
  - `Recurrence(kind)` is a `runtime.RecurrenceFunc`;
  - `WorkKind(kind)` is a `runtime.WorkKindPolicy`;
  - `Render`, `Task` and `Check`.

  Core registers these exactly as it registers in-process extensions.
- **In-transaction calls.** `Process.CallInTx` enforces the 250 ms deadline, does not wait for a restart, and refuses every extension-to-host callback with `reentrant_call` while one is outstanding. Two outcomes cannot fail a transaction:
  - The `bool` dependency filter counts a failure as "no match". The event is not lost; a later matching event still wakes the wait.
  - Work-kind trigger selection falls back to the runtime's documented first input.

  The other two in-transaction methods return errors and abort the transaction, which is retried.

### Executors across extension restarts

An extension crash is not an execution failure. `remoteHandle.Wait` notices that the process generation changed, either through a transport error or because the new process does not know the handle. It then calls `executor.recover` on the restarted process with the last metadata persisted through its run. Re-adoption therefore needs the run's lease-fenced `PersistRecovery`: it goes through the scheduler's fenced function, so an observer whose lease has been superseded is refused. A crash before anything was persisted, and `not_adoptable`, are returned to the scheduler's stop-and-retry policy.

Runs are addressed by unguessable host-assigned IDs. A closed run refuses persistence and resources, so a stale or restarted process cannot persist for an attempt the host no longer serves through it. When the host refused the persistence that made the extension's `Run` fail, `Run` returns the host's own error, so the conformance check that `PersistRecovery` failures propagate holds across the wire.

### Trust model

An extension process runs with the operator's user privileges. **Process separation is fault isolation, not a sandbox.** An extension can read and write anything the operator's account can, open network connections, and spawn processes. Installing an extension is therefore the same trust decision as installing any program the operator runs. 07-03's certification checks protocol conformance. It cannot make a malicious extension safe.

What the host does enforce is the protocol surface. Every extension-to-host call is bound to the identity the host started the process with, never to anything the extension sends:

- **KV.** Only the extension's namespaces (`name`, `name.*`). `kv.commit` batches are all-or-nothing with version or absence preconditions; a batch touching any foreign namespace applies nothing. The extension's KV layout version lives in the reserved `core.extensions` namespace, which no extension can address.
- **State reads.** Only scopes whose configuration uses the extension. A record outside them is `not_found`, so a read is never an existence oracle. Scope reads drop secret references.
- **`event.accept`.**
  - The scope must be configured. The family must be declared. The source must be the extension's (`name`, `name/*`). The payload must be its current schema version.
  - The host assigns causation, execution and the Event ID.
  - The parent must be an accepted Event of the same scope.
  - Causal depth and per-minute limits are the runtime's (`AcceptDerivedInTx`), and records commit atomically with a newly added Event.
  - **A subprocess never mints a causal root outside ingress.** A root acceptance would let routing → extension → event loops escape the chain bounds. Introspection's host-watch roots, which are in-process, have no subprocess equivalent; a third party would model them as ingress.
- **`timer.*` and `work.cancel`.** Only declared recurrences and the extension's own families and Work kinds in configured scopes. Cancellation keys are prefixed with the extension name.
- **`ingress.deliver`.** Only sources the last `configure` registered. The answer comes after durable acceptance.
- **`secret.resolve`.** Only configured scopes, through a host resolver that must admit only the names bound to the extension's entries (B05).
- **`execution.persist_recovery` and `resource.*`.** Only through a run the host opened for this process (described above).
- **Methods.** Only methods the manifest enables (`Manifest.Enabled`). A host-to-extension method sent to the host is `method_not_found`.

Restarts cannot widen authority. A restarted process gets the same identity. Causal chains, leases, KV layout records and canonical ingress identities all live in host storage, so a restart neither resets a chain nor revives a superseded lease.

### Event payload schema evolution

The payload schema identifier is `<base>.v<N>`. The manifest's `provides.schemas` declares each base, in the extension's namespace, with the versions the running release can read, in strictly increasing order. The last of them is the version it writes. Persisted envelopes are immutable: the host never rewrites a stored Event. `Manifest.PayloadStatus` classifies a payload deterministically:

| Status | Meaning | Outcome |
| --- | --- | --- |
| `foreign` | absent, or outside the extension's namespace | passed verbatim; reading it is a data contract |
| `current` | the version the extension writes | passed |
| `upcast` | an older version still listed | passed verbatim; the SDK chains `Upcaster`s up to the current version before extension code sees it |
| `retired` | own, older than every listed version | not passed for decisions |
| `newer` | own, newer than the written version (persisted before a downgrade) | not passed for decisions |
| `undeclared` | own namespace, undeclared base or no version suffix | not passed for decisions |

The host applies the gate before any call, so an unreadable payload never depends on what a process does with it. `route.match` answers "no match", so the event falls through to later routes or the unmatched disposition, as for any non-matching route. `dependency.filter` answers false, `event.render` falls back to generic rendering, and `Remote.SchemaSkips` counts each answer. Executors receive their inputs verbatim, because executors are generic. **Writes** are strict (`Manifest.CheckWrite`): every payload an extension produces must be absent or the current version of one of its own schemas, otherwise it is refused with `unreadable_schema`. This covers ingress observations and events, `event.accept`, emissions, recurrence occurrences and armed timers.

Ingress identity is part of evolution. Core binds an observation's `(item, version)` and `(source, delivery)` to a canonical signature of the envelope. A release that changes the payload for unchanged items must therefore mint new observation versions and delivery IDs, for example by including the schema version (`rev.s2`). That re-observes each item once, as a visible change. An extension that keeps its identities across a payload change gets core's deterministic `conflict` on every page: nothing is accepted and no checkpoint advances (`TestExternalSchemaEvolution/stable_identity_conflicts`).

**Extension KV layout.**
- The manifest's `kv_version` (default 0) versions the extension's own KV layout. The host records the version the extension last reported from a successful `configure`.
- Starting a manifest with an older `kv_version` is refused before the process starts (`kv_downgrade`).
- For a newer version, `configure` carries `kv.stored < kv.declared`. The SDK runs the extension's `Migrate` before `Configure`, using ordinary precondition-guarded `kv.commit` batches, which must be idempotent. The host records the new version only after `configure` succeeds, so a crash mid-migration is retried at the next start.

**Rolling upgrades and downgrades.**
- There are no rolling upgrades. One daemon owns a state directory and runs one process per extension, so old and new releases never run at once. An upgrade stops the old process before the new one starts.
- There is no automatic downgrade. Once a release has migrated KV, the older release is refused. The operator restores a 06-02 backup or reinstalls the newer release.
- An older release that does run against newer events, with an unchanged `kv_version`, answers them deterministically as `newer`.
- Retiring a version is a release decision, and events still in use should be re-observed first.

### Evidence

- **Same suites, both ways.** `integration/extension_suites_test.go` runs the shared ingress, executor, inject, toolbelt (persistent and reopen) and the new extension KV suites (`conformance.RunExtensionKV`) against the example code in-process and as a subprocess. `TestWeakenedExtensionKVIsRejected` shows that the KV suite catches an unscoped or non-atomic KV.
- **Failures.** `integration/extension_faults_test.go` covers:
  - durable acceptance across a crash and a partial response, plus reconcile across a crash;
  - the in-transaction deadline and the `reentrant_call` refusal;
  - an emission commit that hangs or crashes (nothing accepted, then exactly one Event with its record on retry);
  - protocol mismatches (version, manifest, KV downgrade);
  - schema evolution across three releases;
  - causal limits across a restart;
  - host-surface scoping;
  - discovery and atomic conflicts;
  - executor lease fencing across restarts, through the real scheduler.
- **Unit tests.** `protocol/evolution_test.go`, `protocol/jsonrpc/conn_test.go`, `protocol/sdk/sdk_test.go` and `internal/extproc/process_test.go`.

### Not done in 07-02 (delivered by 07-03)

Both items below were delivered by 07-03: `serve` now discovers, admits, starts and registers installed packages, and every catalogued surface is adapted ([extension-packaging.md](../extension-packaging/)). They are kept here as the 07-02 record.

- **Daemon wiring.** `open-conduit serve` does not discover or start extensions yet. `extproc` provides every piece:
  - `Discover` and `CheckConflicts` against the bootstrap registries;
  - `Load` with `Deps` from runtime ingress, timers, control, artifacts and the secret resolver;
  - registering `Remote`'s adapters next to the first-party ones;
  - feeding config entries through `configure` and `validate`.

  Wiring it into bootstrap is deferred to 07-03, together with install, the `config validate` hook and the round trip of two first-party integrations. It touches config validation and the daemon's composition, which 07-03 reshapes anyway.
- **Surfaces not adapted yet.** These are deferred to 07-03, where the round trip needs them:
  - host-to-extension: `webhook.handle`, `provider.*` (an external resource provider), `telemetry.export`, `work.annotate`, `validate` and `setup.run`;
  - extension-to-host: `provider.owner_status` and the `setup.*` callbacks.

  They stay catalogued. Until then the SDK answers the host-to-extension ones `method_not_found`, and the host answers the extension-to-host ones `unsupported`.

## Provisional decisions for user review

1. JSON-RPC 2.0 over stdio, line-delimited, standard library only, symmetric, rather than go-plugin/gRPC.
2. Integer protocol versions negotiated in `initialize`, with a deterministic refusal on mismatch.
3. The manifest file name, its fields, and the namespacing rule: identifier names are `name`/`name-…`, dotted names `name.…`, sources `name/…`.
4. Bounded in-transaction callbacks (250 ms, no reentrancy) for the four existing pure callbacks, rather than restructuring core.
5. KV through precondition batches (`kv.commit`) rather than interactive transactions.
6. Disposable processes with no reattach. Crash means restart plus `executor.recover`.
7. State reads limited to the extension's configured scopes.
8. Limits: 16 MiB messages, 256 KiB chunks, 64 in flight with an `overloaded` refusal.
9. Playbooks as a manifest-declared directory: a first-party convention for agent executors, not a core concept.
10. The listed cross-extension couplings stay in-process and are not offered to subprocesses.

Added by 07-02, also provisional:

11. The install location `<config dir>/extensions/<name>/`, with the directory named after the manifest.
12. The trust model: process separation is fault isolation, not a sandbox; the host enforces only the protocol surface.
13. A subprocess never mints a causal root outside ingress: `event.accept` requires a parent in a configured scope.
14. Payload schemas `<base>.v<N>` declared per base with readable versions. The host gates reads deterministically (no-match, false, generic render). Writes must be current. Envelopes are never rewritten, and older versions are upcast in the extension.
15. A payload change requires new observation and delivery identities (include the schema version). Otherwise core's canonical-identity conflict applies.
16. `kv_version` with host-recorded migration and no downgrade. There are no rolling upgrades.
17. Executor re-adoption after an extension crash happens inside the host adapter, through the run's fenced persistence.
18. Fallbacks for the in-transaction calls that cannot fail a transaction: the dependency filter counts a failure as false, and trigger selection falls back to the first input.
19. Reentrancy is refused for every callback while any in-transaction call to that extension is outstanding. This is coarse: a concurrent background callback in that 250 ms window is refused too.
20. Daemon wiring, and the remaining host-to-extension surfaces, are deferred to 07-03 (since delivered).

Added by 07-03, also provisional: decisions 21–32 in [extension-packaging.md](../extension-packaging/#provisional-decisions-for-user-review-07-03).

## Follow-on work

- **07-02 (done, see above):** message types, the host runtime, bounded in-transaction calls, the scoped surface, the trust model, payload and KV evolution, and the example extension and executor through the shared suites.
- **07-03 (done, see [extension-packaging.md](../extension-packaging/)):**
  - daemon wiring of discovered extensions (bootstrap registries, `config validate`, configure on apply) and the remaining adapters (`webhook.handle`, `provider.*`, `telemetry.export`, `work.annotate`, `validate`, `setup.*`);
  - SDK and package layout, and the install location;
  - certification;
  - a round trip of two materially different first-party integrations that avoid the in-process couplings. Suggested: Slack (workspace-free ingress, receiver, tools, projector task, setup) and the shell executor (execution with resume, inject, interrupt, stop, artifacts).
