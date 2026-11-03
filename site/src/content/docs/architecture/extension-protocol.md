---
title: "Extension subprocess protocol"
description: "How third-party extensions run as separate processes that speak JSON-RPC 2.0 over stdio: transport, manifest, lifecycle, runtime, trust model and schema evolution."
---

This page records decision B08: an extension that is not built into Open Conduit runs as a separate process and speaks JSON-RPC 2.0 to the daemon over its stdin and stdout. **The protocol is provisional**: it may still change, and the decisions it rests on are listed for review at the end of this page.

The executable definition is package [`protocol`](https://github.com/open-conduit/open-conduit/blob/main/protocol/protocol.go): the version, the [manifest](https://github.com/open-conduit/open-conduit/blob/main/protocol/manifest.go) and its validation, the [method catalogue](https://github.com/open-conduit/open-conduit/blob/main/protocol/methods.go) and the [wire types](https://github.com/open-conduit/open-conduit/blob/main/protocol/wire.go). Each wire method names the public contract it carries and the shipped first-party users that need it. The host side, [`internal/extproc`](https://github.com/open-conduit/open-conduit/tree/main/internal/extproc), loads, supervises and scopes extension processes and adapts them onto the same interfaces in-process extensions implement ([below](#runtime-trust-model-and-schema-evolution)). Packaging, installation, the Go SDK and certification are covered in [Extension packaging, SDK and certification](../extension-packaging/), which is provisional too. The [transport prototype](https://github.com/open-conduit/open-conduit/tree/main/prototypes/transport) supplies the evidence for the transport choice (`make protocol-prototype`).

The protocol was extracted from the shipped first-party code: the GitHub, Slack, Linear, Jira, Bitbucket, schedule, introspection and PostHog extensions, the Git resource provider, and the shell and claude-code executors. It was not designed in the abstract. It adds **no core primitive**. Every method is an adapter over an existing public contract (`extension`, `storage` KV and reads, `observability`), or over a host composition hook the daemon already drives for in-process extensions. First-party extensions stay in-process. The protocol has no consumers outside this project, so it is cheap to revise.

## Decision summary

| Question | Decision (provisional) |
| --- | --- |
| Transport | JSON-RPC 2.0 over the extension process's stdin/stdout, one JSON object per line, standard library only, symmetric (both sides send requests). Not go-plugin/gRPC. |
| Version | Integer protocol versions. `initialize` picks the highest common version. No common version is a deterministic refusal before any other call (`unsupported_protocol_version`). Version 1 is the first. A version changes only for a breaking change; additive methods and manifest fields ship in a release without one, and an older host refuses a manifest that uses them as needing a newer Open Conduit. |
| Manifest | `open-conduit-extension.json` beside the binary: name, own version, command, and `provides` (mechanical declarations only). Validated offline. The handshake must return the same manifest. |
| Capabilities | Executor labels are exactly the core's mechanical labels (`interrupt`, `inject`, `resume`, `workspace`, `toolbelt: none\|cli\|mcp\|sdk`), plus optional controls (`stop`, `forget`, `attach`, `container_attach`, `live`) that are methods, not suitability claims. Unknown fields are rejected, so a semantic label cannot be expressed. |
| Storage | Mandatory namespaced KV under the extension's name, always available and never negotiated (a `storage` field is rejected). A store larger than coordination state is an `external_stores` entry naming a connection type, so it is explicit in config. |
| Transactions | The host never holds a coordination transaction across an open-ended call. Four pure callbacks run inside a transaction today (`recurrence.next`, `work_kind.trigger`, `dependency.filter`, `emit.commit`). They are bounded (250 ms) and may make no callback. KV writes from an extension are one atomic `kv.commit` with preconditions. |
| Process model | Extension processes are disposable. Durable state lives in host KV, executor recovery metadata or the extension's own state directory, so a crash or host restart means restart plus recover. The host never reattaches to an old process. |

## Inventory of shipped interactions

The table groups what the first-party code actually does across the host boundary today, as found in `internal/bootstrap` and the extension packages. Users: GH GitHub, SL Slack, TR tracker kit (Linear and Jira), BB Bitbucket, SC schedule, IS introspection, PH PostHog, GIT Git provider, SRT srt sandbox provider, SH shell, CC claude-code, HU human.

| Interaction | In-process surface today | Users | Wire method(s) |
| --- | --- | --- | --- |
| Config validation | `config.Registry` validators (connection, extension, route `HandlerConfig`, `ResourceConfig`, executor `ValidateConfig`), and the workspace checks of a binding with a resource (`ResourceModeDefault`, `ResourceSandbox`, executor `config.ResourceModer`); what a valid entry or route sets up (`inproc.ConfigType.Describe`) | all | `validate` (its `describe` lines, kept by the host for the config view) |
| Config apply and source registration | `register<Extension>` on apply: parse, build client, `ingress.Register(name, Source, adapter, interval)`, mount webhook paths | GH SL TR BB SC IS PH | `configure` (returns sources and webhook paths) |
| Ingress push | `Ingress.Push` behind host-mounted `WebhookHandler`s (verify, then push; 2xx only after durable acceptance) | GH SL TR BB SC | `webhook.handle`, `ingress.push` |
| Ingress reconcile | `Ingress.Reconcile`, driven by `ReconcileDue` with page timeout and item/byte limits | GH SL TR BB SC | `ingress.reconcile` |
| Extension-owned receiver | Slack Socket Mode calls the host push func and acks the envelope only after it returns | SL | `ingress.deliver` (E→H) |
| Routing | `RouteHandler.Match` on a detached snapshot, outside any transaction and free of side effects, with an optional Work display. `RouteSampler.Samples` offers named sample Events for a route trace | GH SL TR BB SC IS | `route.match`, `route.samples` |
| Work kind policy | `WorkKindPolicy{Standing, Trigger}`. `Trigger` is pure and runs in the start transaction. A timed kind (`inproc.WorkKind.Timed`) runs on a clock, so a dry run starts none of its Work | SC (IS standing only) | manifest `work_kinds`, `work_kind.trigger` |
| Calendar recurrence | `RecurrenceFunc` in the timer fire-and-rearm transaction. `Armer.ArmRecurrence/Cancel` from `Ensure` | SC IS | `recurrence.next`, `timer.arm`, `timer.cancel` |
| Toolbelt operations | `Registration{Name, EventTypes, Operations, DependencyFilter, Gate, StartGate}`. `Operation.Handler` (all), `.Wait` (GH), `.Emit` with in-transaction `Commit` (IS), `Gate` and `StartGate` (HU) | GH SL TR BB IS HU | `operation.call`, `operation.wait`, `operation.emit`, `emit.commit`, `dependency.filter`, `operation.gate`, `work.admit`, `work.settle_hold` |
| Wait progress | `runtime.SetWaitProgressInTx` (GitHub's `ToolsOptions.WaitProgress`), `inproc.Host.WaitProgress` | GH | `wait.progress` (E→H) |
| Prompt rendering | bootstrap `renderEvents` chain over `<extension>.RenderEvent`; an executor renders its prompt's Events through it (`inproc.Host.Render`) | GH SL IS, CC | `event.render`, `event.render_owned` (E→H, with manifest `executors`) |
| Wait targets | `inproc.Targeter` resolves `<kind>:<name>` wait targets (schedule's `job:`), which a wait operation asks for through `inproc.Host.WaitTarget` | SC, GH | `target.resolve` (with manifest `targets`), `wait.target` (E→H, with a wait operation) |
| Cross-scope authority | `inproc.Host.Authorize`: may one of the extension's scopes read (`authorization.read_scopes`) or file in (`authorization.file_scopes`) another | GH | `scope.authorize` (E→H) |
| Container segment end | `inproc.SegmentEnder`, told before a container segment settles so it revokes what it issued (git tokens) | GH | `segment.ended` (with manifest `segment_end`) |
| Executor lifecycle | `Executor.Run/Recover`, `RunHandle.Wait`, `Resume`, `Interrupt`, `Inject`, `Forget`, `PersistRecovery` | SH CC (GH hold) | `executor.*`, `execution.persist_recovery` (E→H) |
| Physical-stop evidence | `extension.StopExecutor` (`Stop(execution, epoch)`), so the host can `ConfirmStopped` | SH CC | `executor.stop` |
| Operator attach | `extension.AttachExecutor` (`Attach`, `ErrNothingToAttach`, wire `nothing_to_attach`), and `extension.ContainerAttachExecutor` for Work that ran in a container | CC | `executor.attach`, `executor.container_attach` (controls `attach`, `container_attach`) |
| Live output | `extension.LiveExecutor` (`Live`, `ErrNotLive`, wire `not_live`): a running attempt's output so far | CC | `executor.live` (control `live`) |
| Workspaces | `ResourceBroker.Acquire/Lookup` (executor side). `ResourceProvider.Acquire/Lookup/Reconcile` with the host's `ResourceStatusFunc` | CC, GIT | `resource.*` (E→H), `provider.*`, `provider.owner_status` (E→H) |
| Sandboxes and containers | `SandboxBroker.Plan/Adopt` and `ContainerBroker.Launch/Adopt` (executor side): a run's launch in its binding's sandbox or container, through the provider the host bound to the attempt | SH CC | `sandbox.*`, `container.*` (E→H, executor capabilities `sandbox`, `container`) |
| Sandbox providers | `SandboxProvider.Plan/Adopt` (provider side), which the host binds to an attempt with `extension.BindSandbox` | SRT | `sandbox_provider.plan`, `sandbox_provider.adopt` (with manifest `sandboxes`), `validate` kind `sandbox` |
| Namespaced KV | `Store.Transact` with `Get/Put/Delete/List` in the extension's namespaces (`introspection.notes`, `github.projection.*`, `claude-code`, `schedule.runs`, `introspection.*`, `<type>.tools.*` …) | GH SL TR BB SC IS CC | `kv.get`, `kv.list`, `kv.commit` |
| Read-only state | `Work`, `ListWorks`, `ListExecutions`, `ListWaits`, `Event`, `Events` (cursor), `LatestSequence`, `ListScopes`/`Scope`, `ListTimers` | GH SL TR BB SC IS CC HU | `state.*` (`state.waits` is a Work's Waits, for human's asks; `state.sequence` is the log's latest sequence, the cursor human records with an ask) |
| Derived and root events with records | `runtime.Causal.Derive` (GitHub turns), `AcceptDerivedInTx` (schedule translation, job runs), `AcceptInTx` + `PutOriginInTx` (host-watch reports) | GH SC IS | `event.accept` (E→H, KV records join the transaction) and `kv.commit` Emissions (a transaction's root and derived Events, accepted with its writes) |
| Control | `Control.CancelWork` for retired introspection Work | IS | `work.cancel` (E→H) |
| Artifacts | `Artifacts.Create`, write, `Commit` (transcripts, logs, output files) | SH CC | `artifact.*` (E→H) |
| Secrets | `connectionSecrets`, `inproc.Host.Secrets` and `resolver.ResolveNames` (lazy, current generation) | GH SL TR BB SC PH | `secret.resolve` (E→H) |
| Background passes | `Projector.Converge` (GH SL TR), `Ensure` (SC IS), `Executor.Maintain` (CC), `manageHooks`/`EnsureWebhook` (GH TR BB), `hostWatch` (IS), `publishJobRuns` (SC) | GH SL TR BB SC IS CC | manifest `tasks`, `task.run` |
| Telemetry export | `observability.Exporter.Export/Shutdown` on the pipeline goroutine | PH IS | `telemetry.export`, `shutdown` |
| Telemetry events | `inproc.Host.Capture` writes a named event to the host's telemetry events log; `inproc.Sink` receives every one, from every scope | IS PH | `telemetry.capture` (E→H, permission `telemetry.capture`), `telemetry.sink` (permission `telemetry.sink`) |
| Setup probes | `inproc.Host.Probes`: the read-only probes the host, extensions and executors offer setup sessions (`claude-code.status`); `extension.SetupProbes`: the probes an extension offers | setup; GH CC | `probe.list`, `probe.run` (E→H, permission `probes`); `probe.serve` (H→E, manifest `probes`) |
| Config host | `inproc.Host.Config` (`ConfigHost`): the configuration now, behind a handle with a redacted copy; pointer edits that build a candidate the host holds; validation and route traces against a candidate. Never applies | setup | `config.base`, `config.edit`, `config.validate`, `config.trace` (E→H, permission `config.read`) |
| Operator's view of an entry | `inproc.ConfigType.Settings` (fields safe to show, typed) and `Describe` (what an entry sets up) | GH SC | manifest `extensions[].settings`, `extensions[].describe` |
| Doctor | `runtime.Check` functions (`Checks`, `DeepChecks` with secrets), each on the extension's configuration view | all but SH | `check.run` ([doctor checks](#doctor-checks)) |
| Run environment | `extension.RunEnvironment`: an environment layer for each run of Work in a scope that uses the extension, told the connection the run's binding names (`connection`, as an operation's authority carries it), merged into `ExecutionContext.Env` before the executor starts (`extension.ApplyEnv`: later layers win, git's `GIT_CONFIG_*` entries are appended). An error fails the run before launch. Executors apply it after the resource's layer and give the process the toolbelt client, so a layer can name `os` (the GitHub extension's git as the bot uses `!os git-credential`) | GH | `run.environment` |
| Annotations | management `Annotate` | GH | `work.annotate` |
| Actions | `extension.ActionProvider` (list, authorise, plan one Event the host accepts under `core.action`); `inproc.Host.Act` invokes one for a person the extension identified on its own surface, as a caller of its own kind, and `inproc.Host.Actions` lists what is offered. A package's provider goes by its name, or with manifest `entry_named` by the name of its first entry on the scope, which its annotations' source takes too | GH SL HU | `work.actions`, `work.act`, `action.submit` and `action.list` (E→H, permission `action.submit`) |
| Panels | `extension.PanelProvider` (a constrained document on a scope or Work page, its buttons naming the provider's actions; a section of `messages` is a conversation) | HU setup | `view.render` |
| Pages | `inproc.Paged` (`extension.Page`): a page of the extension's own in the web view, which core draws from its panels and actions ([pages](#pages)) | setup | manifest `pages` |
| Config candidates | `extension.ConfigCandidate`: what a Work proposes, its changes with the actions that decide each and the candidate's handle, for core's review and the operator's apply | setup | `config.candidate` (permission `config.read`) |
| Setup | `extension.Setup` with `SetupHost` prompts, browser, secret store, `LookPath`, `Run` | GH SL TR | `setup.run` plus `setup.*` callbacks |
| Playbooks | embedded playbooks written into the agent executor's plugin directory | GH SL TR IS | manifest `playbooks` (first-party convention) |

`TestCatalogueCoversThePublicContract` checks mechanically that every method of every public interface an extension implements or consumes is carried by a wire method. `TestInprocSurfacesAreCarried` checks the in-process contract the same way: it reads every interface of `extension/inproc`, every field of `inproc.Host` and every field of `inproc.ConfigType` from the source, and fails unless each maps to a wire method, a manifest field, or something a process has of its own (its clock, its stderr). A surface nothing carries yet would be listed with the open issue that will carry it; none is listed now. Entry-gated operations and route binding checks (`EntryGated`, `ConfigType.Binding`) became the manifest's `entry_gated` and `routes[].binding` for the setup extension to become a package (#243), and the last gaps (wait targets, cross-scope authority, rendering through the owner and container segment ends) became wire methods in #357. `TestCatalogueMapsEveryMethodToAShippedUse` checks that every wire method names at least one shipped user. Core-record writes (`AcceptEvent`, `PutWork`, leases, timers, waits) never cross the boundary. They are reached only through `event.accept`, `timer.arm`, `work.cancel` and `wait.progress`, which apply the same causal limits and fencing as in-process callers.

### What does not cross the boundary

Some couplings exist only as bootstrap glue between first-party packages. They have no protocol equivalent, because a subprocess extension may depend on another only through events, correlation keys and public state reads:

- The schedule translator reads `github.delivery.v1` payloads from the event log by their public schema, without importing the GitHub extension.

Bitbucket used to read the tracker's reopen generations through an in-process registry the tracker kit kept; it now reads them from the tracker's Work in its scope, through public state reads (#428).

This stays in-process, which the design allows: first-party extensions run in-process behind the public interfaces. They also decide which first-party integrations can run as a subprocess package without change.

In-process extensions also hold the whole `storage.Store` and the daemon's clock and dry-run flag. A subprocess gets only its KV namespaces, scoped reads, its own clock, and `dry_run` in `configure`.

The host's telemetry events, setup probes and config host used to be in-process only too. They now cross the boundary behind [permissions](#permissions).

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

What would reverse the decision: a need for very high-throughput streaming (the per-chunk acknowledgement already gives 143 MiB/s), or a third-party ecosystem that standardises on go-plugin. Neither exists, and the protocol has no consumers outside this project.

## Wire format

- **Framing.** UTF-8 JSON, one object per line (`\n`), at most `MaxMessageBytes` (16 MiB; above the runtime's default 8 MiB reconcile page). A longer line or invalid JSON is a protocol error that closes the connection. stderr is the extension's diagnostic log. The host captures it to a bounded per-extension log under the state directory ([Loading](#loading)).
- **Messages.** JSON-RPC 2.0 requests (`id`, `method`, `params`), responses (`result` or `error`) and notifications (no `id`). Ids are unique per sender while in flight. Responses may arrive in any order.
- **Payloads.** Wire types are snake_case JSON defined in package `protocol` ([`wire.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/wire.go), [`messages.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/messages.go)). They mirror the management API's wire shapes (`client`) where those exist. `domain.Payload.Data` is raw JSON. Byte fields (delivery bodies, artifact chunks) are base64.
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
| `not_live` | `extension.ErrNotLive` |
| `resource_setup_failed` | `extension.ErrResourceSetup` (the resource still returned) |
| `resource_not_found` | `extension.ErrResourceNotFound` |
| `resource_exhausted` | `extension.ErrResourceExhausted` (the provider's message, which says what to free, crosses with it) |
| `sandbox_denied` | `extension.ErrSandboxDenied` (the refusal, naming the need outside the policy, crosses with it) |
| `sandbox_unavailable` | `extension.ErrSandboxUnavailable` (with the provider's reason) |
| `container_running` | `extension.ErrContainerRunning` (with the reason) |
| `container_unconfirmed` | `extension.ErrContainerUnconfirmed` (with the reason) |
| `retry_after` | `extension.RetryHint` (`data.reset_at` optional). It is the only code that crosses as a retry hint (`protocol.RetryAfter`); every other code is a plain refusal, so an executor's `not_adoptable` never delays its retry. |

The protocol also has its own codes: `unsupported_protocol_version`, `reentrant_call`, `overloaded`, `cancelled`, `shutting_down` and `internal`.

## Lifecycle

1. **Discovery.** The host reads `open-conduit-extension.json` from each extension directory. The install location is described under [Loading](#loading); installing, updating and removing packages is in [packaging](../extension-packaging/#install-update-rollback-and-removal). `ParseManifest` validates it strictly and offline. `config check` without a daemon starts a package only when the config has an object of one of its types, for `validate` alone (see [Doctor checks](#doctor-checks)). Conflicts with first-party or other registrations (names, types, operations, families) are refused atomically, as `Registrar` does in-process.
2. **Start.** The host runs `command` (its own executable for `open-conduit extension-run <name>`, [served by open-conduit itself](../extension-packaging/#served-by-open-conduit-itself)) with a sanitised environment (the B05 baseline, no secrets), the protocol on stdin/stdout, and the extension's private state directory. The process is a child of the daemon, in its own process group.
3. **`initialize`.** The host sends `{protocol_versions, host: {name, version}, state_dir, executor_dirs}`. `executor_dirs` maps each executor the package declares to the host's `<state>/executors/<name>`, the directory that executor keeps its runs in whether it runs in process or out: the binding's sandbox and container guards accept launches only from runs under it, and a package that takes over from an in-process executor finds that executor's runs there. The extension returns `{protocol_version, manifest}`. The host refuses a manifest that differs from the file, or no common version. No other method is valid before `initialize` succeeds.
4. **`configure`.** The host sends the current generation's entries, connections (secret names only, never references), `public_url` and `dry_run`. `disabled` lists the extension's entries in scopes that are configured but disabled: it starts nothing for them but keeps what it holds for them (the schedule extension keeps their timers armed). Each entry carries the connection it names (`connection`) and the connections the bindings in its scope name (`binding_connections`), each when it is of one of the extension's declared connection types and the scope authorization grants it. The extension returns the ingress sources and webhook paths it registers (under its own namespaces); a webhook path covers the paths beneath it, so a schedule entry's `/schedule/<scope>/<entry>` serves `.../<job>/<token>`. It is sent again on every config apply and tunnel URL change, without a restart.
5. **Running.** The host calls the surfaces the manifest enables (`Manifest.Enabled`): routing, operations, executor and provider methods, and `task.run` at each declared interval. The extension calls the host's KV, state, artifact, secret, timer, event and resource methods, and `ingress.deliver` from its own receivers.
6. **Shutdown.** The host sends `shutdown`. The extension answers `shutting_down` to new requests, finishes or cancels in-flight work, and exits. Then the host closes stdin, sends SIGTERM after a grace period, and SIGKILL. An extension must also exit when stdin reaches EOF, so a host crash never leaves an orphaned extension serving nobody.

An executor extension's *executions* are separate. Its detached processes survive the extension and the daemon, exactly as the shell and claude-code executors' processes do today. They are adopted through `executor.recover` with their recovery metadata, never by reattaching to the old extension process.

## Cancellation

Either side cancels an in-flight request with the `$/cancel {id}` notification. The canceller returns `context.Canceled` at once and discards a late response. The receiver cancels the handler's context and answers `-32800`, if it answers at all. Cancellation means "stop serving this request". It never means "undo":

- cancelling `executor.wait` cancels observation only. The execution keeps running, as `RunHandle.Wait` documents.
- `executor.interrupt` and `executor.stop` are the controls that act on a process.
- a cancelled `operation.call` may already have made its external write. A retry carries the same `request_id` (`CallRequestID`), and the handler uses it to avoid making the write twice ([below](#operation-handlers-and-request-ids)); adapters keep their reconcile markers.

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

All four run as bounded calls (`Process.CallInTx`). `emit.commit` in particular needs one, because its records must join the emission atomically. Two of the four have fallbacks that cannot fail a transaction ([Loading](#loading)).

Extension KV writes are never interactive transactions:

- `kv.get` returns a value and a version (a digest of the value, or absent);
- `kv.commit` applies preconditions (`version` or `absent`) and puts/deletes as one atomic unit. A failed precondition is `conflict`, and the extension re-reads and retries;
- `event.accept` takes the same checks and records, so an Event and its index records commit together. KV writes join the same atomic unit as an event emission.
- A `kv.commit` batch may also carry `emissions`: the Events of the extension's own families a transaction returns for the host to accept in its own transaction, a root Event of the scope or one derived from `parent`. They are the wire form of `inproc.Host.AcceptInTx` and `AcceptDerivedInTx`: no transaction handle crosses the wire, the transaction returns what it accepts. The host applies the checks, then accepts each Emission, then the writes, all in one transaction, and answers each Emission's accepted Event and whether it was new. A failed check or a refused Emission (undeclared family, foreign source, bad schema, a parent outside the scope) applies nothing. An Emission over its chain's causal bounds is refused alone (`refused: causal_limit`, recorded for the operator) and the rest commits, as an in-process transaction may after `inproc.ErrCausalLimit`. The host names an emitted Event `protocol.EmittedEventID(source, delivery_id)`, so the transaction's own records can name it before it commits, and emitting it again is the same Event. `conformance.RunExtensionEmissions` checks this, against the host's service in process and over the wire, with a non-atomic and an unchecked emitter as its negative controls.

This is an adapter over `storage.Tx` (`Get`, compare, `Put`/`Delete` in one `Transact`), not a new storage capability.

## Doctor checks

`check.run` takes `{check, deep, config}`. `config` is the read-only view of the current configuration (`protocol.CheckConfig`) that a first-party extension's `inproc.Check.Run` receives too: `dir` (the config file's directory, for relative paths), and each enabled scope that uses the extension (an entry of its types, a route on its handlers, a binding granted its operations, naming its connections, or on its executors or resource providers) with that scope's routes, bindings (executor, its toolbelt capability, granted operations, connection, resource provider and config, and, for a binding of one of the extension's own executors, the binding's executor config) and the extension's own entries; then the connections of its own types those name, config only. It never carries secret values or reference text. A check can therefore judge how the configuration uses the extension, as `github-capability`, `github-git-bot` and the claude-code executor's `claude-code` (each binding's binary, and the logins whose usage it may read) do.

The result is `{ok, detail}` or, with `status` (`ok`, `info`, `warn`, `fail`), a full check: `status`, `code`, `detail` and `items`. Without `status`, `ok` gives `ok`, and its absence `warn` with `extension_check_failed`; an unknown status is `warn` with `extension_check_invalid`. Doctor lists each declared check under its own name: the check's name when it is the package's name or starts with `<package>_` or `<package>-`, else `<package>_<check>`. The `extensions` check keeps the packages' own health (started, version, protocol, restarts).

Offline validation. `config check`, offline doctor and the removal check have no daemon, so a package whose types the config uses is started for `validate` alone: no coordination store (KV, state and event methods refuse), a temporary state directory removed afterwards, an empty `configure` that reports the KV layout as current so nothing migrates, and a 15 s start timeout. It is stopped once the document is checked. A package that cannot start refuses its objects, and the diagnostics add `extension_unavailable` naming it. A declared schema in the manifest was the other option, but the first-party validators check more than a schema can (secret names, cross-field rules, patterns that must compile), so the host asks the package.

## Crash and reconnect

A broken pipe or process exit fails every pending request with a transport error (detected in about 1 ms in the prototype). It is never a hang or an authored error. The host restarts the process with backoff, then runs `initialize` and `configure` again. What a crash means depends on the call:

| In flight | Outcome |
| --- | --- |
| `webhook.handle` / `ingress.push` | Nothing accepted, so the delivery is not acknowledged (non-2xx). The service retries, and reconcile is the backstop. |
| `ingress.deliver` (receiver) | The extension acknowledges upstream only after the host's answer. A crash before that leaves the delivery unacknowledged, and the service redelivers. |
| `ingress.reconcile` | Partial scan: no absence is inferred and no checkpoint advances. The runtime retries the generation. |
| `route.match` | The routing pass fails and the Event stays pending. Routing is deterministic and retried. |
| `route.samples` | That handler's samples are left out of the list, which the operator can ask for again. |
| `operation.call` / `.wait` / `.emit` | The toolbelt caller gets a retryable internal error. The retry carries the same `request_id`, which the handler must be idempotent on ([below](#operation-handlers-and-request-ids)). A declared wait or emission either committed or did not. |
| In-transaction method | The transaction aborts and is retried after restart. |
| `executor.*` | The extension's crash is not the execution's failure. After restart the host calls `executor.recover` with the persisted recovery metadata, as after a daemon restart. `not_adoptable` leads to `executor.stop`, `ConfirmStopped` and retry policy. Old per-execution tokens stay fenced by lease epoch. |
| `provider.*` | Acquisition is idempotent per owner and mode, and `provider.reconcile` repairs the rest (the existing contract). |
| `sandbox_provider.*` | Fails closed: the launch or adoption is refused as `sandbox_unavailable` and nothing runs. A plan is pure, so the executor's next attempt plans again. |
| `task.run`, `check.run`, `telemetry.export` | Reported, and retried next pass. A dropped telemetry batch is accounted. |

### Operation handlers and request IDs

The contract for every operation handler, in-process (`extension.Operation.Handler`) or over the protocol (`operation.call`): **a handler must be idempotent on the call's `request_id`.**

The host runs a handler outside any transaction, then commits its result under the request ID, and a retry with that ID replays the committed result without calling the handler. A crash in between, after the handler's effect and before that commit, leaves no result, so the caller's retry runs the handler again. The same happens when the call is cancelled in that window or the extension restarts mid-call. A handler whose effect isn't naturally idempotent (posting a comment, opening an issue) derives an intent identity from `CallRequestID` and checks for it before acting: the GitHub extension writes its intent ID into what it creates and looks for it first. A call without a request ID has no replay at all, so a handler can't tell a retry from a new call. `TestProductionToolbeltReplayWindow` drills the window: the call ends after the effect and before the commit, the retry runs the handler again, and a handler keyed by the request ID makes its effect once.

If the *host* crashes, extensions see EOF and exit. The new daemon starts them again. Durable progress is all in host storage, recovery metadata or the extension's own state directory, so there is nothing to reattach to.

## Authority and scoping

The trust model is defined [below](#trust-model). Process separation is not sandboxing: an extension runs with the operator's user privileges. The protocol fixes the scoping, which the host applies to every extension-to-host call from the identity it bound at start. The extension never supplies that identity.

- **KV.** Only the extension's own namespaces: its name, or `name.` + suffix (`protocol.Namespace`, the existing `extension.Record` rule).
- **Families, operations, kinds, recurrences, types, handlers, executors, providers, tasks.** Only names under its own name (`Manifest.Validate`).
- **Source namespaces.** Its own name, or `name/` + suffix (`protocol.SourceNamespace`, the `EmitPlan` rule).
- **State reads.** Provisionally limited to scopes whose config uses the extension, and Work of the extension's own declared kinds wherever it is (which `work.cancel` may also cancel), so it can retire Work left where its entry was removed. This covers every shipped read: projectors filter Works by their own correlation sources, and the schedule translator reads its configured scopes. `state.sequence` answers `{sequence}`, the latest accepted Event's sequence in the whole log, in one call: a cursor for `state.events`, which says how far the log reaches but nothing of what is in it. `state.events` filters by scope before it applies its limit (at most 1000), so a page comes back short only at the end of the log, however many other scopes' events sit between the extension's own, and `sdk.Store` pages on that (#251). Each event that `state.event` and `state.events` return carries `scope_id`, the scope it was accepted into, so an extension configured in several scopes knows which one an event belongs to.
- **`secret.resolve`.** Only the connections bound to the extension's configured entries, and in a scope with one of its entries the connections that scope's bindings name when they are of one of its declared connection types (B05 isolation).
- **`operation.call`.** Receives the validated `Authority`, never the bearer token, exactly as in-process handlers do. The authority's `connection` is the connection the calling Execution's binding names (empty when it names none), so an operation can act as it: its config arrives in `configure` and its secrets through `secret.resolve`. Executors receive the per-execution endpoint and token in `RunRequest`, as in-process executors do.
- **`event.accept` and `work.cancel`.** Apply the same declared-family, causal-limit and own-kind checks as their in-process callers.

## Permissions

Some host services reach beyond an extension's own surfaces: they show it other scopes' activity or let it run things on the machine. A manifest asks for each in `permissions`, the operator sees them at install and in `open-conduit extension list` and `inspect`, and the host neither serves nor calls a method a permission gates unless the manifest declares it (`Method.Permission`, `Manifest.Enabled`); an undeclared call is `undeclared`. An unknown permission is refused as needing a newer Open Conduit, as an unknown field is.

| Permission | Methods | What it allows |
| --- | --- | --- |
| `telemetry.sink` | `telemetry.sink` (H→E) | Receives every named telemetry event the host records, from every scope, with the scope as `scope_id`. It sees the whole machine's activity. |
| `telemetry.capture` | `telemetry.capture` (E→H) | Records a named telemetry event for one of its configured scopes, or for the machine (`scope_id` empty). The name is at most 128 bytes, the properties at most 64 KiB and never carry `scope`. Every sink receives it. |
| `probes` | `probe.list`, `probe.run` (E→H) | Lists and runs the read-only setup probes the host, its extensions and its executors offer, by full name. The host checks the arguments against the probe's declaration and bounds the run to 30 seconds. A probe that fails answers its reason in `error`, cut to 300 bytes. |

| `config.read` | `config.base`, `config.edit`, `config.validate`, `config.trace` (E→H), and `config.candidate` (H→E, with manifest `config_candidate`) | Reads the whole configuration and builds candidates of it, which it validates and traces routes against, and offers them for the operator's review. It never applies one, and never receives reference text: see [the config host](#the-config-host). |
| `work.start_gate` | `work.admit` (H→E, with manifest `start_gate`), `work.settle_hold` (E→H) | Is asked before the first attempt of any Work in a scope the package is configured for starts, with the Work, its binding and its first input, and may hold it (`{hold?: {reason}}`). An error refuses the start for now. `work.settle_hold` `{work_id, admit, reason, key}` settles only a hold the package placed: admit lets the next start through once, and otherwise the Work is cancelled ([start gates](../execution-scheduling/#start-gates)). The Go SDK's `Extension.StartGate` and `Host.SettleHold` declare and use them. |
| `operation.gate` | `operation.gate` (H→E, with manifest `operation_gate`) | Is asked about every call of another extension's toolbelt operation in a scope the package is configured for, with the call's input and authority, and may park or refuse it ([toolbelt gates](../toolbelt-management/#gates)). Calls in other scopes never reach it. |
| `action.submit` | `action.submit`, `action.list` (E→H) | Invokes an action for a person the package identified on its own surface, in one of its configured scopes (`{scope_id, work_id?, provider?, action, input?, caller, key}`, answered with `{event, added, message?}`), and lists what every provider offers on a target there (`{scope_id, work_id?}`, answered with `{actions: [{provider, id, label, prompt?, choices?, detail?}]}`, where `detail` is what a person is deciding, as the provider's `work.actions` gives it). The caller is always `<package name>:<caller>`, and the action's provider decides whether that caller may act ([acting for people](../operator-setup/#acting-for-people-on-other-surfaces)). |

Packages are not sandboxed (see the [trust model](#trust-model)), so a permission is the operator's consent and a guard against accidental leaks, not a security boundary. It is enforced as though it were one, because it becomes one if packages are ever sandboxed.

### The config host

The config host lets an extension propose configuration for the operator to apply, as the setup extension does. Bundled and installed extensions use one contract (`inproc.ConfigHost`, and the `config.*` methods that carry it):

- **The host keeps the text.** `config.base` answers the committed generation and hash, the scopes from included folders, and a handle on the config file's text. The text it answers is a copy with every `secret_refs` value replaced by `[redacted]` (an unset `false` stays). The real text stays with the host, behind the handle.
- **Edits go against a handle.** `config.edit {handle, op, pointer, value?}` makes one `config set`/`config unset` edit (`add`, `replace`, `remove`; `-` appends, array elements by `id` or `name`) to the text behind the handle. The host holds the result as a new candidate and answers its handle, its redacted text, and the value that was at the pointer, also redacted. A value may set a new reference; it is redacted in everything the host answers back. The pointer is at most 512 bytes and the value at most 64 KiB.
- **Validate and trace a handle.** `config.validate {handle}` runs the validation of `POST /config/validate` and answers the candidate hash or the diagnostics. `config.trace {handle, scope, sample?}` runs a route trace against the candidate and writes nothing; without a sample it lists the scope's samples, and a trace that can't run answers its fixed reason in `error`.
- **Handles are the host's.** They are random, the same text keeps its handle while it is held, and the host holds the 256 most recently used. A handle it no longer holds (after a restart, or long unused) is `not_found`: read the base again.
- **Never apply.** Nothing on the wire applies a configuration. An extension whose Work proposes one says so in `extension.Candidate` (`config.candidate {work}`, manifest `config_candidate`): every change with its pointer, state, reason, redacted before and after, problems and the IDs of its own actions that approve and reject it, and, once the approved changes make a candidate, its handle and hash. `GET /work/{id}/config-candidate`, an operator-class read, serves the changes and the text behind the handle, core draws the review, and only the operator applies it with `POST /config/apply`. An operator's approval stays the extension's action, a `core.action` Event that the host records.

### Pages

An extension may contribute a page to the web view (issue #242): manifest `pages` (`extension.Page`, `inproc.Paged` in process), at most four, each `{id, title, summary?, entry, work_kind, start, compose?, route?}`. The ID is the page's path (`/pages/<id>`) and follows the identifier rule; `entry`, `work_kind` and `route` name the package's own extension type, Work kind and route handler, and the page needs `actions`. Core draws the page with no extension code and no markup from the package:

- With no session open, the `start` action on a scope with the entry, and the grants the binding of the scope's first `route` offers as checkboxes, each one turned on added to the start input as `+name`.
- With a session open (Work of `work_kind` in such a scope), the Work's panels: the first section with `messages` is the conversation (`{from: operator|participant|note, label?, text, at?, choices?, chosen?}`, a participant's text read as Markdown; `agent`, the old name for `participant`, is still accepted), and the rest stand beside it. One message box sends the first of `compose` the entry offers on the Work, its choices as buttons.
- The session's change set, from `config.candidate`, when the package proposes configuration: the review and Apply are core's.

`GET /pages` lists every extension's pages. The bundled setup extension declares its page this way, so the setup assistant in the browser is an ordinary extension page.

## Manifest

`protocol.Manifest` is the specification. Its example manifests are in `protocol/testdata`:

- [`ingress-support.json`](https://github.com/open-conduit/open-conduit/blob/main/protocol/testdata/ingress-support.json): a workspace-free ingress workload shaped like Slack. It declares no executor and no resource, and enables no execution method.
- [`executor-workspace.json`](https://github.com/open-conduit/open-conduit/blob/main/protocol/testdata/executor-workspace.json): an execution workload. A resumable, injectable executor with stop, forget and attach controls, plus the workspace provider it runs in.
- [`scheduled-reporting.json`](https://github.com/open-conduit/open-conduit/blob/main/protocol/testdata/scheduled-reporting.json): timers, standing Work with trigger selection, and a committed emission.

Validation rules:

- the protocol list includes a supported version;
- the name is an identifier and not reserved (`core`, `execution`, `manual`, `os`, `open-conduit`);
- the optional `title`, the display name operators read where the extension's Work and sources are listed (`inproc.Titled` in process), is one line of at most 64 characters. The daemon lists every extension's title at `GET /v1/extensions`, and the web view labels sources with it. A first-party integration not yet on the contract takes its catalogue title. Without a title, the extension shows as its name;
- identifier-shaped names are `name` or `name-…`, and dotted names are `name.…`, with no duplicates;
- a non-re-derivable family states its loss, and a re-derivable one cannot;
- an ingress type declares families, and webhooks need an ingress type;
- work kinds need a route handler;
- operation kinds are `handler`, `wait` or `emit`, only emit may `commit`, and only a handler `serves` a capability (`extension.Operation.Serves`), named like an operation;
- an operation's `summary` (`extension.Operation.Summary`) is one line of at most 200 bytes, its `guidance` (its agent guidance, `extension.Operation.Guidance`) is at most 4096 bytes, and its `input` (the JSON Schema of its input's data) is an object;
- `targets` are wait target kinds (lower-case letters, digits, `-` or `_`), each declared once;
- `dependency_filter` needs a wait operation;
- `entry_gated` needs operations and an extension type;
- a toolset is named `<name>.…`, declared once, and grants at least one operation, each one the manifest declares or a builtin, once (`TestManifestToolsets`);
- toolbelt labels are `none`, `cli`, `mcp` or `sdk`;
- resource modes are distinct `shared`, `work` or `fresh`;
- task intervals are at least one second;
- the command and playbooks stay inside the extension directory;
- external stores name a declared connection type;
- an extension type's `settings` are distinct top-level field names, each a `string`, `boolean`, `number` or `strings` with a default of that type, and its `describe` names its `list`, `name` and `detail` fields;
- permissions are known and distinct;
- payload schema bases are dotted names under the extension's name without a version suffix, with positive, strictly increasing readable versions; `kv_version` is not negative;
- unknown fields are rejected. Additive manifest fields ship without a protocol version bump, so an unknown field is refused as `ErrNewerManifest` (the package needs a newer Open Conduit), not as an invalid manifest.

`TestManifestRejections` covers each rule, including a `storage` capability and a semantic executor label.

The same format and the same catalogue cover both workload shapes. Nothing in core distinguishes an "ingress extension" from an "executor extension". A manifest enables the methods of the surfaces it declares, and the host registers those surfaces through the same runtime interfaces it uses for in-process extensions (`TestExampleManifests`, `TestEveryMethodIsReachable`).

An extension type may declare what the operator's view of an entry shows (`GET /v1/config`), the way an in-process type's `Settings` and `Describe` do. `settings` lists the top-level config fields that are safe to show, with their value types and optional defaults: the host reads each from the entry's config and leaves out a value of another type; every other field is shown by name alone. `describe` lists what an entry sets up, one line per element of the array field `list`, named by its `name` field and detailed by its `detail` field. The host reads both from the config itself, so they need no process and work offline. Neither may name a field that can carry a credential.

```json
"extensions": [{"type": "chat", "ingress": true,
  "settings": [{"field": "team", "type": "string"}, {"field": "manage_webhook", "type": "boolean", "default": true}],
  "describe": {"list": "jobs", "name": "name", "detail": "schedule"}}]
```

These manifest fields carry what the in-process contract gives a first-party extension:

- **`entry_gated`** (`inproc.EntryGated`): the package's operations join a generation's toolbelt only when an enabled scope has one of its entries. Removing the last entry removes them, and a binding that still grants them keeps running, with its calls undeclared.
- **`routes[].binding`** (`inproc.ConfigType.Binding`): the handler checks the binding each of its routes binds. Config validation calls `validate` with kind `binding`, the handler as `type` and the binding as `binding`: its `executor`, `operations`, executor `config` and `grants`. Unlike every other kind, a refusal's message is shown: it is the reason in the `binding_refused` diagnostic at the route's `binding`, capped at 300 bytes, so it names the rule and never echoes a config value. A call that fails refuses the binding too.
- **`sandboxes`** (`config.Registry.Sandboxes` and `extension.SandboxProvider`): process sandbox provider types. A binding's `sandbox` config is checked with `validate` kind `sandbox` (the provider as `type`). The host binds the provider to each attempt as it does an in-process one (`extension.BindSandbox`): it merges the resource's needs and its own protected set into a launch's needs before `sandbox_provider.plan`, computes the plan's digest itself from what comes back and pins it, and asks `sandbox_provider.adopt` with the pinned plan when it recovers a segment. A refusal crosses as `sandbox_denied` or `sandbox_unavailable`, and anything else (the package down, an internal error) as `sandbox_unavailable`, so nothing runs. The provider's checks see each binding of theirs with its `sandbox` (`CheckBinding.sandbox`). The Go SDK serves `sdk.Extension.Sandboxes`.
- **`resources[].mode_default`** and **`resources[].sandbox`** (`config.Registry.ResourceModeDefault` and `ResourceSandbox`, which git fills in process): the provider answers config check's workspace checks for a binding with its resource. Config validation calls `validate` with kind `resource_mode` (`type` and the resource `config`), answered `{mode}`: the mode the config itself writes, empty for none. Kind `resource_sandbox` adds the `mode` the binding's executor requests (empty for the config's default) and is asked for a binding that declares a sandbox or container; its refusal's message is shown, capped at 300 bytes, as the reason in the `sandbox_workspace` diagnostic, and a call that fails refuses too. The Go SDK answers both from a provider that implements `extension.ResourceModeDefaulter` (`ModeDefault`) and `extension.ResourceSandboxChecker` (`SandboxWorkspace`).
- **`executors[].resource_mode`** (`config.ResourceModer`, which Claude Code implements in process): validate with kind `executor_mode`, the executor as `type` and the binding's executor `config`, answered `{mode}`: the mode that config makes it request, empty for the resource config's default. It feeds the sandbox check above and the mode warning (`config check` warns when the resource names a mode the executor's request overrides, [workspaces](../workspaces/#modes-from-the-real-github-needs)). A failed call names no mode. The Go SDK answers from an executor that implements `extension.ResourceModer`. The TypeScript SDK serves neither executors nor resource providers yet.
- **`toolsets`** (`inproc.Toolsetter`): named sets of the package's operations for its agents' roles, each operation with the version it joined in (`{"name": "notes.author", "operations": [{"name": "notes.read"}, {"name": "notes.write", "since": 2}]}`). Config grants one as `@notes.author` ([toolsets](../toolbelt-management/#toolsets)). The Go SDK declares one with `Extension.Toolset`.
- **`executors[].playbook`** (`extension.PlaybookNamer`, which Claude Code and Codex implement): validate with kind `executor_playbook`, the executor as `type` and the binding's executor `config`, answered `{playbook}`: the playbook that config starts turns on, empty for none. Config check compares what the playbook declares it uses with what the binding is granted.
- **A run's `host_env`** (`extension.ExecutionContext.HostEnv`) is the daemon's values of the variables the binding's own `pass_env` names, those set. The operator names them in the configuration and the host checks them there, refusing `OS_*`, `OPEN_CONDUIT_*` and any variable an `env:` secret reference names (`pass_env_reserved`, `pass_env_secret`). A package never chooses which of the daemon's variables it sees, and its own sanitized environment never needs widening.
- **`executors[].pool`** (`inproc.Executor.Pool`): the capacity pool the executor's runs count against. Empty is agent turns (`settings.concurrency`); `jobs` is jobs (`settings.job_concurrency`). Any other value is refused, by `Manifest.Validate` for a package and at registration for an in-process extension, so no executor runs unbounded.
- **Executor config warnings** (`extension.ConfigWarner`, which Claude Code implements in process): the answer to `validate` with kind `executor` may carry `{warnings}`, fixed sentences about a valid binding config that should change, such as a deprecated or ignored setting. `config check` shows each one, prefixed with the scope and binding, without failing. A binding a scope inherits unchanged warns once. At most eight warnings are shown, each on one line and capped at 300 characters. A failed call or an older package gives none. No manifest declaration is needed. The Go SDK answers from an executor that implements `ConfigWarner`, and the runner example warns about its ignored `retries`.
- **`setup.first_run`** (`extension.SetupInfo.FirstRun`): this setup, run with no flags on a host that prompts for nothing, stores nothing and runs nothing, builds the configuration a daemon runs on when it has no config file and no stored generation ([setup sessions](../setup-sessions/#a-fresh-install)).

No first-party extension or executor is registered in-process any more: each runs only as a package ([packaging](../extension-packaging/)). A third-party extension's config entries are validated through `validate`.

## Public executor controls

- `extension.StopExecutor` is public because an external executor must be able to supply physical-stop evidence. The shell and claude-code executors implement it.
- `extension.AttachExecutor`, `extension.AttachSpec` and `extension.ErrNothingToAttach` make attach a public optional executor method. Bootstrap does not branch on claude-code. The management wire code is `nothing_to_attach`, and `claudecode.ErrNoSession` wraps `ErrNothingToAttach`. The operator's shell supplies the attach environment.
- `extension.ContainerAttachExecutor` is attach for Work that ran in a container: `executor.container_attach` sends the Work, its binding's config and secrets and the Work's container home, and the package answers the needs and launch, which the host's container broker plans under the binding's current policy. The package never launches the attach container itself.
- `extension.LiveExecutor` and `extension.ErrNotLive` give an operator a running attempt's output so far: `executor.live` takes the execution and a `tail`, and answers each output's name, media type, size and last `tail` bytes, or `not_live`.
- A package's executor that doesn't declare one of these controls answers `unsupported` without a call, which the operator API reports as the executor not supporting it.
- `extension.ContractVersion` versions the in-process API. `protocol.Version` versions the wire.

## Runtime, trust model and schema evolution

Provisional, like the rest of the protocol. Code:

- [`protocol`](https://github.com/open-conduit/open-conduit/blob/main/protocol/wire.go): the snake_case wire types and method params ([messages](https://github.com/open-conduit/open-conduit/blob/main/protocol/messages.go)), the [error mapping](https://github.com/open-conduit/open-conduit/blob/main/protocol/errors.go) onto the public sentinels, and [schema evolution](https://github.com/open-conduit/open-conduit/blob/main/protocol/schema.go).
- [`protocol/jsonrpc`](https://github.com/open-conduit/open-conduit/blob/main/protocol/jsonrpc/conn.go): the transport, hardened from the prototype.
- [`protocol/sdk`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/sdk.go): a minimal extension-side runtime that serves the public Go contracts and gives the extension a [`Host`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/host.go) client. [Writing an extension with the SDK](../extension-packaging/#writing-an-extension-with-the-sdk) describes its full shape.
- [`internal/extproc`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/discover.go): the host runtime. It covers discovery, conflicts, the [supervised process](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/process.go), the [scoped host service](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/service.go) and the [adapters](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/adapters.go).
- Examples:
  - [`protocol/examples/notes`](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/notes/notes.go) is the example external extension. It covers ingress, a route, operations, an emission with committed records, a recurrence, tasks, three releases and a KV migration.
  - [`protocol/examples/runner`](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/runner/runner.go) is the example external executor: detached jobs with inject, interrupt, stop and forget.

### Loading

- **Install location.** `<config dir>/extensions/<name>/open-conduit-extension.json`, beside `config.json` (`extproc.Dir`). The directory must be named after the manifest's name. Install, update, rollback and removal are `open-conduit extension` commands ([packaging](../extension-packaging/#install-update-rollback-and-removal)).
- **Bundled packages.** Packages can also ship with the binary, in `extensions/` beside the real executable (links resolved, as the `os` toolbelt is found). They are discovered after the installed ones. The release archive bundles `setup`, `codex`, `posthog`, `human`, `observe`, `linear`, `jira`, `bitbucket`, `schedule`, `introspection`, `slack`, `github` and `shell` this way ([packaging](../extension-packaging/#bundled-with-the-release)); all but `setup` are served by `open-conduit extension-run NAME`. An installed package of the same name replaces the bundled one, even when the installed one is refused, so a release that needs a newer Open Conduit is reported rather than silently swapped for the bundled one. Removing the installed copy brings the bundled one back. `extension list` shows bundled packages, marked as such. At start the daemon launches every discovered package at once and waits for the slowest handshake, so adding bundled packages doesn't add their start times together (and the adoption of attempts a graceful restart released isn't held up by them); they join the daemon in discovery order.
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
  - `Registration()` is an `extension.Registration` for the toolbelt, with handler, wait and emit operations, `emit.commit`, `dependency.filter` and, for `provides.operation_gate` with the `operation.gate` permission, a gate over `operation.gate`, asked only about calls in the package's configured scopes (`{operation, owner, authority, input}`, answered with `{wait?}`: no wait lets the call through, and an error refuses it). A gate that times out, crashes or fails with anything but an operation error refuses with `gate_failed`, so the host fails closed. The SDK's `Extension.OperationGate` declares and serves one; the TypeScript SDK has no gates yet, as it has no wait plans;
  - `Executor(name)` implements every optional executor interface;
  - `Recurrence(kind)` is a `runtime.RecurrenceFunc`;
  - `WorkKind(kind)` is a `runtime.WorkKindPolicy`;
  - `Render`, `Task` and `Check`;
  - `BindingCheck(handler)`, a handler's binding check.

  Core registers these exactly as it registers in-process extensions.
- **In-transaction calls.** `Process.CallInTx` enforces the 250 ms deadline, does not wait for a restart, and refuses every extension-to-host callback with `reentrant_call` while one is outstanding. Two outcomes cannot fail a transaction:
  - The `bool` dependency filter counts a failure as "no match". The event is not lost; a later matching event still wakes the wait.
  - Work-kind trigger selection falls back to the runtime's documented first input.

  The other two in-transaction methods return errors and abort the transaction, which is retried.

### Executors across extension restarts

An extension crash is not an execution failure. `remoteHandle.Wait` notices that the process generation changed, either through a transport error or because the new process does not know the handle. It then calls `executor.recover` on the restarted process with the last metadata persisted through its run. Re-adoption therefore needs the run's lease-fenced `PersistRecovery`: it goes through the scheduler's fenced function, so an observer whose lease has been superseded is refused. A crash before anything was persisted, and `not_adoptable`, are returned to the scheduler's stop-and-retry policy.

Runs are addressed by unguessable host-assigned IDs. A closed run refuses persistence and resources, so a stale or restarted process cannot persist for an attempt the host no longer serves through it. When the host refused the persistence that made the extension's `Run` fail, `Run` returns the host's own error, so the conformance check that `PersistRecovery` failures propagate holds across the wire.

### Trust model

An extension process runs with the operator's user privileges. **Process separation is fault isolation, not a sandbox.** An extension can read and write anything the operator's account can, open network connections, and spawn processes. Installing an extension is therefore the same trust decision as installing any program the operator runs. [Certification](../extension-packaging/#certification) checks protocol conformance. It cannot make a malicious extension safe.

What the host does enforce is the protocol surface. Every extension-to-host call is bound to the identity the host started the process with, never to anything the extension sends:

- **KV.** Only the extension's namespaces (`name`, `name.*`). `kv.commit` batches are all-or-nothing with version or absence preconditions; a batch touching any foreign namespace applies nothing. The extension's KV layout version lives in the reserved `core.extensions` namespace, which no extension can address.
- **State reads.** Only scopes whose configuration uses the extension, and Work of its own declared kinds in any scope. A record outside them is `not_found`, so a read is never an existence oracle. Scope reads drop secret references.
- **`event.accept` and `kv.commit` Emissions.**
  - The scope must be configured. The family must be declared. The source must be the extension's (`name`, `name/*`). The payload must be its current schema version.
  - The host assigns causation, execution and the Event ID.
  - A derived Event's parent must be an accepted Event of the same scope.
  - Causal depth and per-minute limits are the runtime's (`AcceptDerivedInTx`), and records commit atomically with a newly added Event.
  - `event.accept` only derives. A root Event comes only from ingress or a `kv.commit` Emission with no parent, of one of the extension's own families in a configured scope: the same power `inproc.Host.AcceptInTx` gives an in-process extension (introspection's host-watch reports), so a bundled extension running as a package keeps it (#428). A root starts a new causal chain, so an extension that answers its own routed Events with roots can loop where a derived Event would hit the chain's bounds. The host does not detect that; it is the same trust an in-process extension has always had.
- **`timer.*` and `work.cancel`.** Only declared recurrences and the extension's own families and Work kinds in configured scopes. Cancellation keys are prefixed with the extension name.
- **`ingress.deliver`.** Only sources the last `configure` registered. The answer comes after durable acceptance.
- **`secret.resolve`.** Only configured scopes, through a host resolver that admits only the names bound to the extension's entries, or the secret names of a connection one of those entries or one of the scope's bindings names. A connection must be granted by the scope authorization and be of one of the extension's declared connection types (B05). A connection that is not granted, that no entry or binding in the scope names, or of another type is refused. In-process extensions get the same resolver (`inproc.Host.Secrets`).
- **`execution.persist_recovery`, `resource.*`, `sandbox.*` and `container.*`.** Only through a run the host opened for this process (described above), and `sandbox.*` and `container.*` only for a run whose binding has a sandbox or container: the binding's provider plans every launch the package asks for and the host's guard checks it, so a package can't widen its confinement. Launching through the plan is the executor's job, as it is in process: the host can't see a process the package starts on its own, so the sandbox or container confines the commands of an executor you trust to use it, and installing a package is trusting its code.
- **`telemetry.capture`, `probe.*` and `config.*`.** Only with the [permission](#permissions) that gates them. A captured event is for a configured scope or the machine; a probe runs only with the arguments it declares; the config host answers only redacted text and applies nothing.
- **Methods.** Only methods the manifest enables (`Manifest.Enabled`), which leaves out those whose permission it does not declare. A host-to-extension method sent to the host is `method_not_found`.

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
- There is no automatic downgrade. Once a release has migrated KV, the older release is refused. The operator restores a backup taken before the upgrade ([backup bundles](../backup-bundles/)) or reinstalls the newer release.
- An older release that does run against newer events, with an unchanged `kv_version`, answers them deterministically as `newer`.
- Retiring a version is a release decision, and events still in use should be re-observed first.

### Evidence

- **Same suites, both ways.** `integration/extension_suites_test.go` runs the shared ingress, executor, inject, toolbelt (persistent and reopen) and extension KV (`conformance.RunExtensionKV`) suites against the example code in-process and as a subprocess. `TestWeakenedExtensionKVIsRejected` shows that the KV suite catches an unscoped or non-atomic KV.
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

## Route traces

A route trace (`POST /v1/scopes/{id}/route/trace`, `open-conduit route --trace`) asks every route how it would decide a hypothetical Event, so `route.match` is called for Events that never happen, as often as an operator asks. A handler must therefore be free of side effects: it decides from its input alone and records, sends, counts and caches nothing. Routing may also call it again for the same Event after a revision conflict. The `reason` it returns explains the verdict in a trace and never changes the decision.

A handler declared with `"samples": true` in `provides.routes` also answers `route.samples` (`extension.RouteSampler`): given the scope, the route and the host's clock (`now`), it returns named sample Events (`id`, `title`, `event`) that `GET /v1/scopes/{id}/route/samples` and `open-conduit route --samples` offer for a trace. A sample whose data carries a time writes it as of `now`, not the process's own clock, so it traces the same under a test clock or skew. The host drops a sample whose payload the extension may not write (`Manifest.CheckWrite`). Like `route.match`, it must be free of side effects.

## Work display and wait descriptions

Operator views say what a Work is and what it waits for in the words of the source that knows, not by reading correlation keys or event types. Core stores both and never interprets the text.

**Work display.** A route handler may return `display` with a match (`extension.RouteDecision.Display`, wire `route.match` result `display`): `title`, a short `ref` (`#136`, `ENG-12`), `requester`, and up to 8 `links`, each a `label` and an http or https `url`. The router stores it on the Work in the same transaction that routes the Event, replacing what was there; a match without one keeps the current display, which the handler sees on the input's `work.display`, so it can add a link to what it already said. When a key attachment brings earlier input into the same routing pass, the displays apply in evaluation order: the Event's own first, then each earlier input's, each handler seeing the display it replaces, so a link one adds is not lost. `GET /v1/work` and `GET /v1/work/{id}` return it as `display`, and the client as `Work.Display`. Work whose source says nothing has none, and views show the label they always did.

**Work context.** A route handler may also return `Context` with a match that creates Work (`extension.RouteDecision.Context`). It becomes the new Work's context: what `read_context` returns, and what an executor reads per Work, such as the [Claude Code executor](../claude-code-executor/)'s `model` from its `models`. It applies only to Work the match creates and never changes an existing Work's context. The `setup` package uses it to store the model the operator chose when starting a session. `route.match` carries it as its `context` result.

**Work grants.** A route handler may also return `Grants` with a match that creates Work (`extension.RouteDecision.Grants`): the names of the bound binding's [grants](../config-management/#binding-grants) the new Work opts into. They are stored on the Work as `grants` and never change. `route.match` carries them as its `grants` result, and a scope's bindings carry their `grants` over the state reads, so a package can offer them (#243).

Input that reaches a waiting Work goes where its Wait sends it (it wakes the Wait or is queued as context) without a route policy, but the router still evaluates the routes for its display alone: a match's display replaces the Work's, under the same limits, and nothing else about the decision applies. A route that errs or matches nothing leaves the display as it is and never holds the input up, so a GitHub issue renamed while its agent waits is renamed in `GET /v1/work`. Open Work from before route displays, still without one, gets one once per store when the daemon starts (`Router.BackfillDisplays`): the router reads each such Work's last 50 Events on its keys and its first (a busy thread's first message is often the one that names it), asks the routes about them newest first until one gives a display on its own, then replays the Events after that one so each can add to it (a PR's link). No source service is called, and a route that gives nothing leaves the Work without.

`work.annotate` carries the display only to extensions configured for the Work's scope.

| Source | Title | Ref | Requester | Links |
| --- | --- | --- | --- | --- |
| GitHub issue (and held issue) | the issue title | `#N` | the issue author | `Issue #N`, then `PR #N` once a PR's merge, close, checks or comment reaches the Work |
| Linear, Jira | the issue title | the issue key | the author | `Linear KEY` or `Jira KEY` |
| Slack thread | the first line of the first message, without mention tokens | | the user ID | `Slack thread` |
| Schedule job | the job name | | | |

**Wait description.** A wait request may carry `description` (`extension.WaitRequest.Description`; wire `operation.wait` result `wait.description`; the builtin `wait` input `description`, and `os wait --description`). The toolbelt stores it on the Wait when it is declared, cut to its first line within the limit (`extension.Clip`), so an agent's or an extension's text never fails the wait; a timed Wait given none is described there as `until 25 Oct 2026 09:00 UTC` (`extension.DueLayout`, the format GitHub uses), which outlives its timer. The description never changes afterwards. `GET /v1/waits` returns it as `description`, with `due_at` while the Wait's timeout timer exists. `github.wait` and holds describe the conditions in words (`checks on PR #137`, `PR #12 to merge, or 25 Oct 2026 09:00 UTC`), and `human.ask` says `a person's answer` without repeating the question, which may be private.

**Wait progress.** The extension whose operation declared a Wait (a translated wait, `operation.wait`) owns it: the toolbelt records it on the Wait as `Owner`. While the Wait is active, its owner may set a short `progress` line beside the description ("3 of 7 checks finished") with `wait.progress` (`work_id`, `progress`; in process `inproc.Host.WaitProgress`, the SDK's `Host.WaitProgress`). It names the Work, not the Wait: a Work has at most one active Wait, and the extension knows its Work from the wait it planned. The host cuts the text to one line within 120 bytes (`extension.MaxWaitProgress`) and never reads it; `""` clears it. The method is enabled by a wait operation in the manifest. A Work whose active Wait another extension declared, or a builtin `wait` declared (no owner), is refused as `unauthorized`; a Work with no active Wait as `not_found`, so a woken or cancelled Wait keeps the progress it had. `GET /v1/waits` returns it as `progress`, and the client as `Wait.Progress`. GitHub supplies it for `pr:N:checks`: each wait plan records which head the Work waits on, and each `github.checks.pending` observation of that head sets `1 of 2 checks finished, 1 failed` (a required check that has not reported counts as unfinished). The result that wakes the Wait is not pending, so the progress stays as the last pending observation left it.

**Limits.** Every display field is one line of UTF-8 without control characters: title 200 bytes, ref 40, requester 100, link label 60 (required), URL 2048; a display needs a title or a ref. A wait description is cut to 200 bytes. A display that breaks a limit is never truncated into something its source did not say, and never stops routing: the router drops it, keeps the Work's current display, routes the Event as usual and records why in the Event's route record (`GET /events/{id}/route` field `display_refused`); the process adapter drops it before that and logs the extension, handler and reason. Producers check text with `extension.ValidateDisplay`, fit it with `extension.Clip` and check links with `extension.DisplayURL`. Certification's `routes.decisions` (criteria version 2) reads each route case's own answer and fails one whose display breaks a limit or links anywhere but http or https.


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

Runtime, trust model and schema evolution:

11. The install location `<config dir>/extensions/<name>/`, with the directory named after the manifest.
12. The trust model: process separation is fault isolation, not a sandbox; the host enforces only the protocol surface.
13. A subprocess mints a causal root only through ingress or a parentless `kv.commit` Emission of its own families (#428); `event.accept` requires a parent in a configured scope.
14. Payload schemas `<base>.v<N>` declared per base with readable versions. The host gates reads deterministically (no-match, false, generic render). Writes must be current. Envelopes are never rewritten, and older versions are upcast in the extension.
15. A payload change requires new observation and delivery identities (include the schema version). Otherwise core's canonical-identity conflict applies.
16. `kv_version` with host-recorded migration and no downgrade. There are no rolling upgrades.
17. Executor re-adoption after an extension crash happens inside the host adapter, through the run's fenced persistence.
18. Fallbacks for the in-transaction calls that cannot fail a transaction: the dependency filter counts a failure as false, and trigger selection falls back to the first input.
19. Reentrancy is refused for every callback while any in-transaction call to that extension is outstanding. This is coarse: a concurrent background callback in that 250 ms window is refused too.

Packaging, SDK and certification: decisions 21–32 in [extension packaging](../extension-packaging/#provisional-decisions-for-user-review).

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **07-01 (decision B08).** Extracted the protocol from the code that shipped through 05-04 and the 06-01/06-02 code, built the transport prototype and the method catalogue, and recorded decisions 1–10. The transport measurements above come from that prototype. 07-01 also changed two contracts:
  - the daemon's private `stopper` interface was promoted to the public `extension.StopExecutor`; shell and claude-code implemented it unchanged;
  - bootstrap's claude-code special case for attach (`Attach`, `ErrNoSession`, wire `no_session`) was generalised to `extension.AttachExecutor`, `AttachSpec` and `ErrNothingToAttach`. The management wire code `no_session` became `nothing_to_attach` (05-04's note), `claudecode.ErrNoSession` was made to wrap `ErrNothingToAttach`, and the unused attach environment that claude-code computed was removed.

  At 07-01 the wire types, the install location and the capture of extension stderr were left to 07-02, and install, update and removal to 07-03. 07-01 allowed 07-02 to move `recurrence.next`, `work_kind.trigger` and `dependency.filter` to evaluation before the transaction with in-transaction revalidation; 07-02 kept all four in-transaction methods as bounded calls. The list of in-process couplings decided which integrations 07-03 could round-trip without change.
- **07-02.** Implemented the wire types, `protocol/jsonrpc`, a minimal `protocol/sdk`, the host runtime in `internal/extproc` (loading, handshake, supervision, scoping, adapters), the trust model, payload and KV schema evolution, and the example extension and executor through the shared suites. It recorded decisions 11–20. Not done in 07-02:
  - **Daemon wiring.** `open-conduit serve` did not discover or start extensions. `extproc` provided every piece: `Discover` and `CheckConflicts` against the bootstrap registries; `Load` with `Deps` from runtime ingress, timers, control, artifacts and the secret resolver; registering `Remote`'s adapters next to the first-party ones; feeding config entries through `configure` and `validate`. Wiring it into bootstrap was deferred to 07-03, together with install, the `config validate` hook and the round trip of two first-party integrations, because it touched config validation and the daemon's composition, which 07-03 reshaped anyway.
  - **Surfaces not adapted.** Host-to-extension `webhook.handle`, `provider.*`, `telemetry.export`, `work.annotate`, `validate` and `setup.run`, and extension-to-host `provider.owner_status` and the `setup.*` callbacks, stayed catalogued but unadapted. The SDK answered the host-to-extension ones `method_not_found`, and the host answered the extension-to-host ones `unsupported`.
  - Decision 20 recorded that deferral.
- **07-03.** Delivered both deferred items: `serve` discovers, admits, starts and registers installed packages, and every catalogued surface is adapted. It also delivered the SDK's final shape, the package layout and install location, certification, and the suggested round trip of two materially different first-party integrations that avoid the in-process couplings: Slack (workspace-free ingress, receiver, tools, projector task, setup) and the shell executor (execution with resume, inject, interrupt, stop, artifacts). See [extension packaging](../extension-packaging/), decisions 21–32.
- **Issue #123.** Added the Work display on route decisions and the Wait description on wait requests, both stored by core and shown through the API, supplied by GitHub, the tracker kit, Slack, schedule and `human.ask`. Certification criteria version 2 checks display limits and link schemes.
- **Issue #134.** A waiting Work's display follows the routes' answer for input its Wait takes, and the daemon backfills a display for open Work from before #123 from its accepted history, once per store.
- **Issue #233.** An operation's `Authority` carries the connection the calling Execution's binding names (`connection` on the wire), and the host hands a connection a binding names to the extension that owns its type: its config in `configure` (`binding_connections`) and its secrets through `secret.resolve`, through the one host resolver in-process extensions and packages share. Before this an extension could act only as its own entries' connections, so making a binding write as a second account needed extension-specific config and bootstrap code.
- **Issue #239.** Carried the in-process-only host surfaces over the protocol behind manifest `permissions`: `telemetry.capture` and `telemetry.sink` (introspection's and PostHog's telemetry events) and `probe.list` and `probe.run` (setup probes). Extension types declare `settings` and `describe`, which the host reads from an entry's config, so a package's entries show typed values as bundled ones do. The host nudges its loop after a package's `event.accept` and `timer.arm`, as it does for in-process extensions. `TestInprocSurfacesAreCarried` maps every `inproc` surface to what carries it, with the remaining gaps owned by #241 and #243.
- **Issue #241.** Put the config host on the protocol behind the `config.read` permission (`config.base`, `config.edit`, `config.validate`, `config.trace`). The host keeps config text behind handles and answers only redacted copies, so an extension proposes edits against text it never holds. The setup extension moved onto the same handle-based contract and no longer keeps the base's text, with its references, in its KV.
- **Issue #330.** Carried config check's workspace checks over `validate` (kinds `resource_mode`, `resource_sandbox` and `executor_mode`, declared by `resources[].mode_default`, `resources[].sandbox` and `executors[].resource_mode`), so an installed provider and executor get the `sandbox_workspace` refusal and the mode warning as git and Claude Code do. Before this only `internal/bootstrap` could fill those hooks. The notes and runner examples answer them, and certification checks them (criteria version 3).
- **Issue #364.** Added `action.submit` (permission `action.submit`, `inproc.Host.Act`, the Go SDK's `Host.Act`): an extension invokes an action for a person on its own surface, as a caller of its own kind, so approvals can be answered from GitHub comments and, later, Slack. The TypeScript SDK doesn't offer it yet.
- **Issue #364 (Slack).** Added `action.list` (`inproc.Host.Actions`, the Go SDK's `Host.Actions`) under the `action.submit` permission, so an extension can show the actions offered on Work on its own surface.
- **Issue #357.** Carried the last in-process-only surfaces: `target.resolve` and manifest `targets` (`inproc.Targeter`, the `job:` waits schedule resolves), `wait.target` (`inproc.Host.WaitTarget`, for a wait operation, in the extension's own scopes), `scope.authorize` (`inproc.Host.Authorize`, asked only for one of its own scopes, answering `{allowed, reason}`), `event.render_owned` (`inproc.Host.Render`, for executors) and `segment.ended` with manifest `segment_end` (`inproc.SegmentEnder`; best effort and not awaited, so a slow package never holds an execution's settle). A package resolves targets only in its own scopes, and a resolved target's Event types must be its own families. `parityGaps` is empty.
- **Issue #428.** Transaction-bound writes became returned emissions: a `kv.commit` batch carries `emissions`, the root and derived Events a transaction accepts, which the host accepts in its own transaction with the batch's checks and writes (`conformance.RunExtensionEmissions`, with negative controls). `sdk.EmitInTx` returns an Event from an `sdk.Store` transaction, and `protocol/sdk/inprocsdk` maps `inproc.Host.AcceptInTx`, `AcceptDerivedInTx` and `EventScope` onto it, `Timers` onto `timer.arm` and `timer.cancel`, `CancelWork` onto `work.cancel`, and, with their permissions declared, `Config` onto `config.*` and `Probes` onto `probe.*`. So no `inproc.Host` service is nil for a package that declares what it needs. A manifest command of exactly `open-conduit extension-run <name>` (`protocol.HostRunCommand`) runs on the host's own executable, which serves every first-party package, so none builds an executable of its own. `state.waits` reads a Work's Waits in the extension's scopes (human's asks look up their asking Work's Wait), and `protocol/sdk/inprocsdk` carries the operation and start gates, panels, telemetry exporter and sink. Human left the in-process list with `state.sequence` (the latest sequence in one call, rather than paging the log) and manifest `entry_named`, under which a package goes by its first entry's name on each scope, as human's action provider and annotations always have. Since `work.act` names no provider, the bridge hands an action no provider offers now to the scope's only provider, so its own refusal (an answer to a closed question) comes back rather than `unknown_action`. Observe left it next; the bridge now declares a route handler that offers samples (`extension.RouteSampler`) with `samples`, so a route trace asks it through `route.samples`. Linear, Jira and Bitbucket followed: the trackers through the kit's own package entry point (`extensions/tracker/pkg`), with Bitbucket keying a reopened issue's generation from the tracker's Work rather than the kit's in-process registry, so neither needs the other's process. Schedule followed. `validate` answers what a valid entry or route sets up (`describe`), which the host keeps for the config view. A derived Event may name its parent as its cause, which the bridge clears because the host assigns causation. The ingress suite takes a push-only source, whose every family is one-shot, with no canonical observation. Introspection followed: a package reads and cancels Work of its own declared kinds in any scope, and `work_kinds[].run_once` carries a run-once kind. A package offers setup probes of its own: manifest `probes` declares them, which the host lists as `<name>.<probe>`, and `probe.serve` runs one, a failure answering its reason; the bridge serves `extension.SetupProbes`. GitHub followed: a check declaration's `offline` marks one that offline doctor runs, only for a package an enabled scope uses, through the package offline validation has started for that configuration (a package the configuration doesn't use is never started), and the host writes the agents' plugin from every extension's playbooks. Claude Code followed, the last executor in process: `executor.live` and `executor.container_attach` carry an operator's live view and attach in a container, a bridged executor declares `resource_mode` and `playbook` when it names them, and a check's configuration view carries the executor config of the extension's own bindings, so its doctor check reads its bindings itself.
