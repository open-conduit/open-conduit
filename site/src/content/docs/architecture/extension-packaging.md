---
title: "Extension packaging, SDK and certification"
description: "How to write, package, install, upgrade, roll back, remove and certify a third-party extension package."
---

This guide covers writing, packaging, installing, upgrading, rolling back, removing and certifying a third-party extension. Installed packages run as separate processes that speak the [subprocess protocol](../extension-protocol/) (decision B08) and are supervised under its [runtime, trust model and schema evolution](../extension-protocol/#runtime-trust-model-and-schema-evolution). **Packaging, the SDK and certification are provisional**, like the protocol: they may still change, and decisions 21–32 at the end are listed for review.

Code:

- [`protocol/sdk`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/sdk.go): the extension-side SDK;
- [`protocol/certification.go`](https://github.com/open-conduit/open-conduit/blob/main/protocol/certification.go): the certification fixture format;
- [`conformance.Criteria`](https://github.com/open-conduit/open-conduit/blob/main/conformance/certification.go): the criteria;
- [`internal/certify`](https://github.com/open-conduit/open-conduit/blob/main/internal/certify/certify.go): the runner;
- [`internal/extproc/install.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/install.go): install, update, rollback and removal;
- [`internal/extproc/archive.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/archive.go): release archives and the platform check;
- [`internal/extproc/diagnose.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/extproc/diagnose.go): diagnostics;
- [`internal/bootstrap/extensions.go`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/extensions.go): daemon wiring.

## Writing an extension with the SDK

An extension is a program that speaks protocol version 1 on stdin and stdout. In Go, `protocol/sdk` does that for you: fill an `sdk.Extension` and call `sdk.Serve`.

```go
func main() {
	if sdk.PackageCommand(os.Args[1:], manifest(), certification(), os.Stdout) {
		return // package-manifest / package-certification
	}
	_ = sdk.Serve(&sdk.Extension{
		Manifest:  manifest(),
		Configure: configure,                 // entries → ingress sources, webhook paths
		Validate:  validate,                  // config objects of your declared types
		Ingress:   func(name string) (extension.Ingress, bool) { … },
		Routes:    map[string]extension.RouteHandler{"acme": route{}},
		Operations: []extension.Operation{…}, // toolbelt operations
		Executors: map[string]extension.Executor{"acme": executor{}},
		Tasks:     map[string]func(context.Context, *sdk.Host) error{"acme-sync": sync},
	})
}
```

The SDK serves the public Go contracts unchanged, so code written for the in-process interfaces (`extension.Ingress`, `extension.Executor` and its optional controls, `extension.RouteHandler`, `extension.Operation`, `extension.ResourceProvider`, `observability.Exporter`, `extension.Setup`) runs in a subprocess as it is. The first-party Slack extension and shell executor run this way with no change to their packages ([`packages/slack`](https://github.com/open-conduit/open-conduit/blob/main/packages/slack/slack.go), [`packages/shell`](https://github.com/open-conduit/open-conduit/blob/main/packages/shell/shell.go)).

| `sdk.Extension` field | Manifest declaration | Wire methods |
| --- | --- | --- |
| `Configure`, `Migrate` | always | `configure` (KV migration first when `kv.stored < kv.declared`) |
| `Validate` | connection, extension, route (`config`), executor or resource types | `validate` |
| `Ingress` | `extensions[].ingress` | `ingress.push`, `ingress.reconcile` |
| `Webhooks` | `webhooks` | `webhook.handle` |
| `Routes`, `Triggers` | `routes`, `work_kinds` | `route.match`, `work_kind.trigger` |
| `Recurrences` | `recurrences` | `recurrence.next` |
| `Operations`, `Filter` | `operations`, `dependency_filter` | `operation.*`, `emit.commit`, `dependency.filter` |
| `Executors` | `executors` | `executor.*` |
| `Providers` | `resources` | `provider.*` |
| `Tasks`, `Checks` | `tasks`, `checks` | `task.run`, `check.run` |
| `Exporter` | `exporters` | `telemetry.export` |
| `Annotate` | `annotate` | `work.annotate` |
| `Actions`, `Act` | `actions` | `work.actions`, `work.act` |
| `Render` | `render` | `event.render` |
| `Setups` | `setup` | `setup.run` with the `setup.*` callbacks |
| `Upcasters` | `schemas` | applied before extension code sees an older payload |

Reaching the host:

- **`sdk.Host`** is passed to `Configure`, `Migrate` and tasks, and is available inside any handler through `sdk.HostFrom(ctx)`. It has KV (`Get`, `List`, `Commit`, `Update`), `Accept` (derived Events with records), `Deliver` (your own receiver's deliveries), state reads (`Works`, `Events`), `Secrets` and `ConnectionSecrets`, and `Artifact`.
- **`sdk.Store`** is `storage.Store` over the host. It lets code written against a coordination transaction run unchanged. KV reads carry version tokens, and one `Transact`'s writes commit as a single `kv.commit` batch whose preconditions are every key it read. A concurrent change to one of those keys is `storage.ErrConflict`. State reads are the scoped `state.*` methods. Every core write returns `sdk.ErrReadOnly`. Conflict detection covers keys read, not prefix ranges, so it is weaker than the host's serializable transactions. The Slack projector and tools run on it.
- **`sdk.Artifacts`** is `storage.Artifacts` over `artifact.*`. It writes in acknowledged chunks; Open and Delete are host-only. The shell executor writes its logs through it.

Other rules:

- Write nothing but protocol messages to stdout; `sdk.Serve` moves `os.Stdout` to stderr for you.
- Log to stderr. The host keeps a bounded log at `<state>/logs/extensions/<name>.log`.
- Exit when stdin closes.
- Keep durable state in host KV, in recovery metadata, or in the private state directory (`Server.StateDir()`, `<state>/extensions/<name>`).

Any language works: the protocol is newline-delimited JSON-RPC 2.0, and the certification runner checks a non-Go extension the same way.

### Examples

- [`protocol/examples/notes`](https://github.com/open-conduit/open-conduit/tree/main/protocol/examples/notes): every extension surface. It has ingress with a webhook receiver, a route with a trigger, operations including a committed emission, a recurrence, tasks, a resource provider, a telemetry exporter, annotations, a setup flow, validation, three releases with schema evolution, and a KV migration.
- [`protocol/examples/runner`](https://github.com/open-conduit/open-conduit/tree/main/protocol/examples/runner): an executor with detached jobs, inject, interrupt, stop and forget.
- [`packages/slack`](https://github.com/open-conduit/open-conduit/tree/main/packages/slack) and [`packages/shell`](https://github.com/open-conduit/open-conduit/tree/main/packages/shell): real first-party workloads packaged.

## Package layout

A package is one directory, named after the manifest:

```
acme/
  open-conduit-extension.json   manifest (required); "name": "acme"
  acme-extension                the manifest's command[0], relative and executable
  playbooks/                    the manifest's playbooks directory, if declared
  certification.json            the conformance runner's fixture (recommended)
  …                             anything else the command needs (the shell package ships its toolbelt `os` binary)
```

`open-conduit extension inspect DIR` checks a directory offline. It checks the [strict manifest rules](../extension-protocol/#manifest), a relative command inside the package that exists and is executable, and the declared playbooks directory. A package is self-contained: install copies it, resolving symlinks.

Packaged Go binaries also answer `package-manifest` and `package-certification` (`sdk.PackageCommand`), so the manifest has one source of truth in code. `make packages` builds the first-party packages and the examples into `dist/packages/<name>/` this way.

## Compatibility ranges

| What | Range | Checked |
| --- | --- | --- |
| Protocol | The manifest's `protocol` lists every version the package speaks. This host speaks `protocol.Versions` (`[1]`). They are compatible when the lists intersect; the highest common version is used. | at install (`incompatible_protocol`) and at every start (`initialize` refusal) |
| Host build | No host-version field. The protocol version changes only for a breaking change, and the host then keeps speaking the previous one for at least one release. An additive surface (a new method or manifest field) arrives in a release without a version bump, so a package that uses one needs that release of Open Conduit or later. An older host does not know the field and refuses the package as needing a newer Open Conduit. | at install and at every start (`needs_newer_host`) |
| Own release | `version` is informational: shown in `list`, doctor and reports. | — |
| KV layout | `kv_version` never goes down. A newer release migrates from the stored layout at configure. | at update (refused) and at start (`kv_downgrade`) |
| Payload schemas | `schemas[].versions` lists what the release reads; the last is what it writes ([schema evolution](../extension-protocol/#event-payload-schema-evolution)). | every call (deterministic read gate) |
| Executor capabilities | Bindings `requires` mechanical labels; a package declares exactly what it implements. | at config validation (as for in-process executors) |

## Install, update, rollback and removal

Installed packages live in `<config dir>/extensions/<name>/`, beside `config.json`. The daemon reads them only at start, so **every change takes effect at the next `open-conduit serve`**. Directories starting with `.` belong to the installer: `.staging` and `.previous/<name>`.

- **`open-conduit extension install DIR`** checks the layout and protocol compatibility. It refuses, atomically, any name that conflicts with a first-party registration or another installed package. It then copies into a staging directory and renames into place, so a failed install changes nothing. A package named after a first-party extension (`slack`, `shell`, `github`, …) is refused while that extension is registered in-process (`registration_conflict`).
- **`open-conduit extension install ARCHIVE [--sha256 HEX]`** installs a release archive instead of a directory. `ARCHIVE` is a path or an `http(s)` URL to a `.tar.gz` holding one package directory (`acme/open-conduit-extension.json`, `acme/acme-extension`, …). The archive is checked against `--sha256` when given, unpacked into the installer's staging directory and then installed exactly as a directory is. It is refused (`invalid_archive`) when it comes over plain `http` without `--sha256`, when an entry's path escapes the package directory, when it holds a link or special file, or when it holds more than one top-level entry (macOS `._*` metadata is skipped). Publish one archive per OS and architecture.
- Any install or update refuses a package whose command is an executable built for another OS or architecture (`wrong_platform`), naming both platforms. A command that is a script is not checked.
- **`open-conduit extension update DIR`** (or `ARCHIVE [--sha256 HEX]`) does the same for an installed name. A lower `kv_version` is refused. The replaced release is kept as `.previous/<name>`.
- **`open-conduit extension rollback NAME`** swaps the kept release back. If the newer release already migrated the extension's KV layout, the daemon refuses the older one at start with `kv_downgrade`, because there is no automatic downgrade. The remedy is to restore a backup taken before the update ([backup bundles](../backup-bundles/)).
- **`open-conduit extension remove NAME`** is refused while the config at `--config` still uses the package's types (it validates the config without the package). `--force` overrides. Removal deletes the package and its kept release, never its durable state. Its KV namespaces and recorded layout stay in the coordination store, and `<state>/extensions/<name>` and its log stay on disk, so reinstalling the same or a newer release resumes from them. To purge, delete those directories after removal.
- **`open-conduit extension list`** shows installed packages, their protocol versions, what they provide, the release kept for rollback, and any discovery or conflict problems.

### Upgrade procedure

1. `open-conduit extension certify NEW_DIR` (below).
2. `open-conduit backup create DEST` with the daemon stopped, if the new release raises `kv_version`.
3. `open-conduit extension update NEW_DIR`.
4. Restart the daemon. At start the old process is gone; the new one migrates its KV at `configure`, and payloads it cannot read are skipped deterministically. There are no rolling upgrades.
5. Check `open-conduit doctor` (the `extensions` check). On trouble, run `open-conduit extension rollback NAME` and restart. If the new release migrated KV, the rollback is refused at start and you restore the backup instead.

### Removal procedure

1. Remove the config entries, bindings and routes that use the package; `config check` confirms.
2. Apply the config to the running daemon. Work bound to the package's executor should be finished or cancelled first; detached executor jobs keep running until they end.
3. `open-conduit extension remove NAME`, then restart the daemon.

## Daemon wiring

At start, `serve`:

1. discovers the packages and refuses a conflicting set atomically;
2. starts each behind its scoped service, before the configuration opens, so the packages' validators check it. The first `configure` carries an empty generation; the real one follows once the configuration and ingress exist;
3. registers, next to the first-party registrations:
   - config types with `validate`-backed validators;
   - route handlers, Work kinds and recurrences;
   - executors (their binding config validated by the package);
   - resource providers, reconciled with the daemon's owner status;
   - toolbelt operations;
   - the ingress sources each `configure` returns;
   - webhook paths on the webhook listener;
   - declared tasks at their intervals;
   - doctor checks: the `extensions` check, with deep checks under `--check-secrets`;
   - Work annotations, prompt rendering for the package's own families, and telemetry export.

It then sends `configure` again on every config apply and every tunnel URL change. Ingress registrations are immutable for one daemon instance, as for first-party ingress, so a renamed source needs a restart. Nothing in core branches on a package: every decision reads the manifest.

Secrets reach a package only through `secret.resolve`. It returns the secret names selected by the package's own entries in a configured scope, or, with `connection` set, the secret names of a connection one of those entries names (B05). `config check` offline knows the installed packages' types. Their opaque config is checked by the package only when a daemon validates or applies it; an offline check never starts a process. `open-conduit init TYPE` offers installed packages' setups by starting the package for the duration of the flow, without a coordination store.

An installed package's `work_kind.trigger`, `recurrence.next` and `dependency.filter` run through the [bounded in-transaction calls](../extension-protocol/#transactions-and-in-transaction-calls), with the same fallbacks.

Playbooks of installed packages are not copied into the Claude Code plugin directory; playbooks remain a first-party convention.

## Diagnostics

Every refusal and failure gets a stable code, a summary and a remedy (`extproc.Diagnose`). They appear in the daemon log, the `extensions` doctor check and the CLI. They never include config values or secrets.

| Code | Meaning | Remedy |
| --- | --- | --- |
| `invalid_package` | Layout or manifest rule broken | `extension inspect DIR` names each rule |
| `incompatible_protocol` | No common protocol version | Install a release built for this host's protocol, or upgrade Open Conduit |
| `needs_newer_host` | The manifest uses a field this host does not know: the package was built for a newer Open Conduit | Upgrade Open Conduit to the release the package was built against or later, or install a release of the package built for this one |
| `manifest_mismatch` | The process announced a different manifest | Binary and manifest are from different releases; reinstall |
| `kv_downgrade` | Older KV layout than recorded | Roll forward, or restore a backup |
| `registration_conflict` | A name owned by a first-party or another package | Remove the conflicting package |
| `start_failed` | The command could not start | Check that it exists and is executable for this platform |
| `handshake_failed` | Exited or silent during `initialize`/`configure` | Read `<state>/logs/extensions/<name>.log`; only protocol on stdout |
| `configure_refused` | Refused the config, or registered names outside its namespace | `config check` and the log |
| `extension_restarted` (warn) | The process crashed and was restarted | The log |
| `extension_check_failed` (warn) | A declared check failed | The check's detail |

Unsupported capabilities are refused at config validation, exactly as for in-process executors: a binding requiring `inject` against an executor that does not declare it is `unsupported_capability` at the binding's `requires` path. An undeclared surface is never called, and callbacks from one are refused with `undeclared`.

## Certification

`open-conduit extension certify DIR [--json] [--report FILE]` is the distributable conformance runner. It ships in the `open-conduit` binary. It starts the package exactly as the daemon does: `internal/extproc`, with a real SQLite coordination store and artifact store in a temporary directory, and the package's own fixture. It then runs the criteria and prints or writes a report. The exit status is 0 only when the package is certified. `make certify` certifies the first-party packages and the examples, and runs the negative controls.

### Criteria (version 2)

| ID | Applies | Requirement |
| --- | --- | --- |
| `manifest.valid` | always | Strict manifest and package layout, directory named after the manifest |
| `handshake.manifest` | always | `initialize` returns the file's manifest at a common version; `configure` with the fixture succeeds and registers only own names |
| `handshake.version_refusal` | always | An offer of only unsupported versions is refused with `unsupported_protocol_version` |
| `lifecycle.before_initialize` | always | A request before `initialize` is refused |
| `lifecycle.unknown_method` | always | `method_not_found`, and the connection stays usable |
| `lifecycle.shutdown` | always | `shutting_down` after `shutdown` |
| `lifecycle.stdin_eof` | always | Exits within 5 s of stdin closing |
| `lifecycle.restart` | always | Killed, it is restarted and completes the handshake again |
| `transport.clean_stdout` | always | Only protocol messages on stdout |
| `scope.host_surface` | always | No host call refused as unauthorized during the run |
| `storage.kv_migration` | `kv_version > 0` | Migrates from layout 0, and again from 0 (idempotent) |
| `ingress.conformance` | ingress type | `conformance.CheckIngress` on the fixture's case |
| `routes.decisions` | routes and cases | Each case decided deterministically as expected, with any Work display within its limits and http(s) links |
| `operations.cases` | operations | Each case's output or refusal code; an undeclared operation is refused |
| `executor.conformance` | executor | `conformance.CheckExecutor` |
| `executor.inject` | inject capability | `conformance.CheckInjectExecutor` |
| `executor.controls` | stop or forget | Served for an execution the package does not hold |
| `surfaces.served` | checks or tasks | Every check answers; every task is served |

A package is certified when every applicable criterion passes. A declared ingress type or executor without a fixture case fails. Missing route and operation cases are noted.

### Fixtures

`certification.json` holds either an inline `fixture` or a `command`. The command prints the fixture as one JSON line and keeps serving (for example a fake service) until stdin closes. `{tmp}` and `{state}` in the fixture are replaced with the run's temporary and state directories. The format is `protocol.Fixture`:

- `entries`, sent in `configure`, with their scopes, and `secrets` for the names bound to them;
- one `ingress` case: source, delivery, canonical observation, one-shot family and delivery. A source whose families are all re-derivable (reconcile recovers everything) leaves the one-shot family empty;
- `routes`, `operations` (handler operations), one `executor` case and one `inject` case with its ready and finish files.

`receive_time_varies` declares a package that stamps reconcile observations with its own clock (Slack does). The runner then compares observations without `received_at` and says so in the result.

### Reports

A report states:

- the runner and criteria version;
- the package name, version and digest (SHA-256 of the manifest and binary);
- the package's and host's protocol versions and the negotiated one;
- the host build, Go version, OS and architecture;
- every criterion's result;
- the **limits**: what certification does not establish. It is not a sandbox or a safety review. Only the fixture is exercised, against fakes. One host, platform and protocol version are tested. The surfaces not covered by version 2 are webhooks, setup, providers, telemetry, annotations, rendering, emit and wait operations, the in-transaction callbacks, and the size and overload limits; the host's own suites cover those adapters.

### Negative controls

[`protocol/examples/weakened`](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/weakened/weakened.go) packages the examples with one defect each. `TestCertificationNegativeControls` proves that each fails the criterion it names, and is never certified:

- version-blind negotiation;
- manifest drift;
- stdout noise;
- no exit on EOF;
- unstable push identities;
- non-deterministic routes;
- wrong operation refusals;
- a non-idempotent migration;
- a foreign KV write;
- acknowledgement before recovery metadata;
- replacement on recover;
- inject accepted for unknown executions.

## Evidence

- `TestCertificationFirstPartyPackages`: Slack, shell, notes and runner are certified, and their reports state versions and limits.
- `TestCertificationNegativeControls` and `TestCertificationInvalidPackage`.
- `TestExternalSlackRoundTrip`: the Slack package round trip. Socket envelope through `ingress.deliver`, acknowledged after acceptance; remote route; `slack.reply` via `sdk.Store` and the projector against the fake; projection task; check; render.
- `TestExternalShellAdoptionAndArtifacts`: the shell package's process group survives an extension crash and is re-adopted with the same recovery metadata; its log reaches the host artifact store.
- `TestExternalSurfacesRoundTrip`: `validate`, `webhook.handle`, `provider.*` with `provider.owner_status`, `telemetry.export`, `work.annotate`, and `setup.run` with callbacks.
- `TestSDKStoreOverProtocol`.
- `TestDaemonExtensionPackages` (in `make drill`): CLI install, conflict refusal, offline config check, a workflow end to end in the real daemon, the `validate` hook, configure on apply, no orphans after a daemon kill, restart, and removal refused while in use.
- Unit tests in `internal/extproc` (install, update, rollback, removal, diagnostics).
- The first-party in-process suites still run unchanged: Slack and shell keep their in-process registration, and workflows A–D pass (`TestDaemonAllWorkflows`).

## Provisional decisions for user review

21. Packages are directories under `<config dir>/extensions/<name>/`, read only at daemon start. Install, update, rollback and removal are offline CLI commands that take effect at the next serve; there is no hot load.
22. Install stages and renames atomically, keeps exactly one previous release for rollback, refuses a lower `kv_version`, and refuses first-party names. A first-party package (Slack, shell) therefore cannot replace its in-process registration; the packages exist to prove and certify the boundary.
23. Removal keeps durable state (KV, recorded layout, state directory, log) and is refused while the config uses the package, unless forced.
24. Compatibility is the protocol version (no host-version range), which changes only for a breaking change, with a policy of keeping the previous protocol version for at least one release after one. Additive surfaces (new methods and manifest fields) ship without a bump; a package that uses one is refused by an older host with `needs_newer_host`.
25. The SDK's final shape: `sdk.Extension` fields per surface, `sdk.Host`, `sdk.Store` (precondition-on-read optimistic commits; prefix ranges not protected) and `sdk.Artifacts`, plus `sdk.PackageCommand` for packaging.
26. `secret.resolve` takes a `connection` field and `configure` entries carry `name` and `connection`, so a package reaches its connections' secrets without bootstrap glue (B05 scoping kept).
27. Packages start before the configuration opens. A package that fails to start leaves its types unregistered, so a config using them fails validation and the daemon refuses to start with the diagnostic.
28. Certification criteria version 2 (above; version 2 added the Work display limits to `routes.decisions`), run against the package's own fixture by `open-conduit extension certify`. The fixture format lives in `protocol`. A declared ingress type or executor requires a fixture case.
29. `receive_time_varies` relaxes only `received_at` equality for packages that stamp reconcile observations with their own clock.
30. The certification runner is production code in the `open-conduit` binary, so composition may depend on the public `conformance` checks (never `conformance/reference`); the boundary rule says so.
31. The Slack package requires source namespaces `slack` or `slack/…` (the subprocess namespacing rule), which the in-process extension does not.
32. Playbooks of installed packages are not wired into agent executors (left as a first-party convention).

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **07-03.** Built on the protocol (07-01) and its runtime (07-02). It wired installed packages into the daemon, adapted the surfaces 07-02 had left catalogued (`webhook.handle`, `provider.*` with `provider.owner_status`, `telemetry.export`, `work.annotate`, `validate`, `setup.run` with the `setup.*` callbacks), settled the SDK's final shape, added the package layout, the `open-conduit extension` commands, diagnostics and certification, and packaged the Slack extension and the shell executor as the round trip of two first-party integrations. It added the `connection` field to `secret.resolve` and the `name` and `connection` fields to `configure` entries, and narrowed the package boundary rule so composition may depend on the public `conformance` checks. It recorded decisions 21–32 and the evidence listed above.
- 07-03 listed two items as "not wired, and recorded as follow-ons": playbooks of installed packages were not copied into the Claude Code plugin directory, and installed packages' `work_kind.trigger`, `recurrence.next` and `dependency.filter` ran through 07-02's bounded in-transaction calls with the same fallbacks.
