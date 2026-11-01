---
title: "Extension packaging, SDK and certification"
description: "How to write, package, install, upgrade, roll back, remove and certify a third-party extension package."
---

New to extensions? [Build your first extension](../../guides/first-extension/) is a 30-minute tutorial that writes, tests and installs one; this page is the reference behind it.

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

## Starting a package

`open-conduit extension new NAME [--dir DIR] [--module PATH] [--replace DIR]` writes a package repository in `DIR` (default `./NAME`) whose `make check` passes on its first run. It holds:

- `go.mod`, with module `PATH` (default `example.com/NAME`), requiring the release of Open Conduit that ran the command, and `tool` directives for `cmd/open-conduit` and `cmd/os` so the tests and `make certify` need nothing installed. A development build requires the commit it was built from instead. `--replace DIR` builds against an Open Conduit checkout with a `replace` directive, starting from the checkout's requirements and `go.sum` so the package builds from the module cache the checkout builds from. The command runs `go mod tidy` there, so `go.sum` is complete; with `--replace`, a tidy that can't fetch what it wants is reported and the checkout's files are kept.
- `main.go`, with one operation an agent can call, `NAME.hello`, in the declare-once form, a certification fixture with a case for it, and a playbooks directory in the manifest.
- `main_test.go`, a unit test on [`sdktest`](#unit-tests-sdktest), and `e2e_test.go`, which installs the package in a real daemon with [`extensiontest`](#daemon-tests-extensiontest) and has a scripted agent call the operation.
- A `Makefile`: `check` (formatting, vet, `test -race`, build and `lint`), `package` (the [package layout](#package-layout) in `dist/NAME`), `lint` (`extension check` on it) and `certify`. `certify` uses the Open Conduit `go.mod` requires; `OPEN_CONDUIT_REF=<branch or tag>` builds Open Conduit from source at that ref instead, and skips a ref older than the version `go.mod` requires.
- `.github/workflows/ci.yml`, which runs `make check` and certifies against Open Conduit's `main` and its latest release.
- `playbooks/NAME.md`, a stub playbook naming the operation, and a README saying what to change first.

It refuses a name the [manifest rules](../extension-protocol/#manifest) or Open Conduit's own registrations rule out, and a directory that isn't empty. `TestExtensionNew` runs the command against this checkout and runs the new repository's `make check`. `TestTutorialFirstExtension` does the same with every file [the tutorial](../../guides/first-extension/) shows saved over the scaffold's, then runs its install and `config set` steps on a GitHub scope's config, so the tutorial fails CI when it stops working.

## Writing an extension with the SDK

An extension is a program that speaks protocol version 1 on stdin and stdout. In Go, `protocol/sdk` does that for you. Start one with `sdk.New`, register each handler, and call `sdk.Serve`. Registering a handler also declares it, so the manifest is derived from the registrations and can't disagree with them:

```go
type ForecastRequest struct {
	City string `json:"city" doc:"The city to forecast, by name."`
	Days int    `json:"days,omitempty"`
}

func forecast(ctx context.Context, a extension.Authority, in ForecastRequest) (Forecast, error) { … }

func main() {
	ext := sdk.New("weather", sdk.Version("0.1.0")).
		Operation("weather.forecast", sdk.Handler(forecast), sdk.Guidance("Call `weather.forecast` before planning anything outdoors."))
	if sdk.PackageCommand(os.Args[1:], ext.Manifest, nil, os.Stdout) {
		return // package-manifest / package-certification
	}
	_ = sdk.Serve(ext)
}
```

`sdk.New(name, opts...)` speaks protocol version 1 and runs the command `<name>-extension` unless `sdk.Command` says otherwise. Set what can't be inferred with `sdk.Version`, `sdk.Summary`, `sdk.Permissions` and `sdk.KVVersion`, or on `ext.Manifest` directly. Each registration returns the extension, so they chain:

| Registration | Declares |
| --- | --- |
| `Operation(name, sdk.Handler(fn), opts…)`, or `sdk.Wait(fn)`, `sdk.Emit(fn)` | `operations[]` with its kind, `guidance` (`sdk.Guidance`), `commit` (`sdk.Commit`, emit only) and `input` |
| `Route(handler, h, opts…)` | `routes[]`, with `samples` when `h` is an `extension.RouteSampler`, `binding` (`sdk.RouteBinding`) and `config` (`sdk.RouteConfig`) |
| `WorkKind(kind, opts…)` | `work_kinds[]`, `standing` (`sdk.Standing`), `trigger` (`sdk.Trigger(fn)`) |
| `Task(name, every, fn)`, `Check(name, fn, opts…)` | `tasks[]` with its interval, `checks[]` (`sdk.Deep`) |
| `IngressType(type, lookup, families…)`, `Family(families…)` | `extensions[]` with `ingress`, and `families[]` |
| `Recurrence(kind, fn)`, `DependencyFilter(fn)` | `recurrences[]`, `dependency_filter` |

An operation's `fn` takes a typed input: `func(ctx, extension.Authority, In) (Out, error)` for `sdk.Handler`, returning an `extension.WaitPlan` or `extension.EmitPlan` for the other two. The SDK decodes the input's data into `In` strictly. A missing required field, a field `In` doesn't have, or a value of the wrong type is refused with `bad_request` and a message naming the field, before `fn` runs. `Out` is sent as the output's data, under the schema `<operation>.v1` unless `sdk.OutputSchema` names another. The JSON Schema of `In` (`sdk.SchemaOf`) becomes the operation's `input` in the manifest: its fields by their `json` names, required unless tagged `omitempty` or `omitzero` or a pointer, with a `doc` tag as the description. The host carries `input` but doesn't check calls against it yet. Use `domain.Payload` for `In` or `Out` to handle the raw payload yourself.

A registration that can't be served panics when the extension is built: `sdk.Commit` on an operation that isn't an emit, or an input that isn't a struct or a payload. The surfaces with no registration method (`Configure`, `Validate`, `Webhooks`, `Executors`, `Providers`, `Sink`, `Actions`, `Render` and the rest in the table below) are set as fields, with their declaration on `ext.Manifest.Provides`. A literal `sdk.Extension` with a hand-written `Manifest` works too, and is what older packages use:

```go
func main() {
	if sdk.PackageCommand(os.Args[1:], manifest(), certification(), os.Stdout) {
		return
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
| `Validate` | connection, extension, route (`config`), executor or resource types, and route bindings (`routes[].binding`, kind `binding`) | `validate` |
| `Ingress` | `extensions[].ingress` | `ingress.push`, `ingress.reconcile` |
| `Webhooks` | `webhooks` | `webhook.handle` |
| `Routes`, `Triggers` | `routes` (`samples` for a handler that is an `extension.RouteSampler`), `work_kinds` | `route.match`, `route.samples`, `work_kind.trigger` |
| `Recurrences` | `recurrences` | `recurrence.next` |
| `Operations`, `Filter` | `operations` (`entry_gated` to offer them only with an entry), `dependency_filter` | `operation.*`, `emit.commit`, `dependency.filter` |
| `Executors` | `executors` | `executor.*` |
| `Providers` | `resources` | `provider.*` |
| `Tasks`, `Checks`, `CheckRuns` (a check that reads the configuration view or reports its own status, code and items) | `tasks`, `checks` | `task.run`, `check.run` |
| `Exporter` | `exporters` | `telemetry.export` |
| `Sink` | permission `telemetry.sink` | `telemetry.sink` |
| `RunEnvironment` | `run_environment` | `run.environment` |
| `Annotate` | `annotate` | `work.annotate` |
| `Actions`, `Act` | `actions` | `work.actions`, `work.act` |
| `Render` | `render` | `event.render` |
| `Setups` | `setup` (`first_run` for the setup that builds the default configuration) | `setup.run` with the `setup.*` callbacks |
| `Upcasters` | `schemas` | applied before extension code sees an older payload |
| — | `extensions[].settings`, `extensions[].describe` | none: the host reads an entry's [safe settings and describe lines](../extension-protocol/#manifest) from its config |

Reaching the host:

- **`sdk.Host`** is passed to `Configure`, `Migrate` and tasks, and is available inside any handler through `sdk.HostFrom(ctx)`. It has KV (`Get`, `List`, `Commit`, `Update`), `Accept` (derived Events with records), `Deliver` (your own receiver's deliveries), state reads (`Works`, `Events`), `Secrets` and `ConnectionSecrets`, and `Artifact`. With the matching [permission](../extension-protocol/#permissions) it also has `Capture` (`telemetry.capture`), `Probes` and `Probe` (`probes`), and `ConfigBase`, `ConfigEdit`, `ConfigValidate` and `ConfigTrace` (`config.read`).
- **`sdk.Store`** is `storage.Store` over the host. It lets code written against a coordination transaction run unchanged. KV reads carry version tokens, and one `Transact`'s writes commit as a single `kv.commit` batch whose preconditions are every key it read. A concurrent change to one of those keys is `storage.ErrConflict`. State reads are the scoped `state.*` methods. Every core write returns `sdk.ErrReadOnly`. Conflict detection covers keys read, not prefix ranges, so it is weaker than the host's serializable transactions. The Slack projector and tools run on it.
- **`sdk.Artifacts`** is `storage.Artifacts` over `artifact.*`. It writes in acknowledged chunks; Open and Delete are host-only. The shell executor writes its logs through it.

Other rules:

- Write nothing but protocol messages to stdout; `sdk.Serve` moves `os.Stdout` to stderr for you.
- Log to stderr. The host keeps a bounded log at `<state>/logs/extensions/<name>.log`.
- Exit when stdin closes.
- Keep durable state in host KV, in recovery metadata, or in the private state directory (`Server.StateDir()`, `<state>/extensions/<name>`).

Any language works: the protocol is newline-delimited JSON-RPC 2.0, and the certification runner checks a non-Go extension the same way.

### Writing an extension in TypeScript

[`sdk/typescript`](https://github.com/open-conduit/open-conduit/tree/main/sdk/typescript) is the same SDK for TypeScript on Node 22.18 or later, with no runtime dependencies. It covers serving, the host client and declare-once. `extension(name, options)` starts one, each registration declares itself in the manifest, and `main()` answers `package-manifest` and `package-certification` or serves on stdin and stdout:

```ts
import { extension, OperationError, t } from "@open-conduit/sdk";

const ext = extension("weather", { version: "0.1.0" })
  .operation("weather.forecast", {
    input: t.object({
      city: t.string({ description: "The city to forecast, by name." }),
      days: t.optional(t.integer()),
    }),
    guidance: "Call `weather.forecast` before planning anything outdoors.",
    handler: async ({ city, days = 1 }, { host, authority }) => {
      if (city === "atlantis") throw new OperationError("unknown_city", `no forecast for ${city}`);
      return { city, days, summary: "rain" };
    },
  });

void ext.main();
```

Schemas are built with `t` (`string`, `enum`, `literal`, `integer`, `number`, `boolean`, `array`, `object`, `record`, `optional`, `nullable`, `any`). One value is both the JSON Schema the manifest declares and the type the code receives, so neither needs a type assertion. An operation's input is decoded as strictly as the Go SDK decodes it: a missing required field, a field the schema doesn't have, or a value of the wrong type is refused with `bad_request` before the handler runs. Use `t.object({})` for an operation with no input. The output is sent as `<operation>.v1` unless `output` names another schema. Throw `OperationError` to refuse with your own code; anything else thrown crosses as `internal`, with the message kept in the extension's log.

| Registration | Declares |
| --- | --- |
| `operation(name, { input, guidance, output, handler })` | `operations[]`, kind `handler`, with `guidance` and `input` |
| `entryType(type, { config, validate, configure })` | `extensions[]`: a scope entry type. A config that `config` refuses, or that `validate` throws on, is invalid before the daemon applies it, and `configure` receives each generation's entries with their config typed |
| `check(name, fn, { deep })` | `checks[]`; `fn` returns the detail line and throws to fail |
| `task(name, intervalSeconds, fn)` | `tasks[]` |
| `family(...families)` | `families[]`, for Events derived with `host.accept` |

The `Host` client (`ctx.host`) offers `get`, `list`, `commit`, `update`, `accept`, `events`, `works`, `secrets`, `connectionSecrets`, `artifact` and `waitProgress`, plus `call(method, params, resultSchema)` for any other method. KV values are bytes (`Uint8Array`), or strings stored as UTF-8. `onMigrate` upgrades the extension's KV namespaces to `kvVersion`. Everything the host sends is decoded through the schemas in `src/wire.ts`, which ignore fields a newer host adds, and a malformed message is refused rather than passed on.

`main()` moves stdout to stderr while it serves, so `console.log` can't corrupt the protocol stream, and it exits when stdin closes. The manifest's command must be an executable file inside the package, so bundle the extension to one file with a `#!/usr/bin/env node` line and make it executable. [`protocol/examples/weather`](https://github.com/open-conduit/open-conduit/tree/main/protocol/examples/weather) does this with esbuild in `scripts/package.mjs`. Its unit tests serve the extension over a pair of in-memory streams, with the SDK's `Conn` standing in for the host, and `make certify` certifies its package.

This first step doesn't include `wait` and `emit` operations, routes, ingress or executors, an in-process test host like `sdktest`, a TypeScript `extension new` scaffold, or an npm release. `src/wire.ts` is written by hand. `TestTypeScriptWireMatchesGo` fails when a field there is no longer a JSON field of the Go type it names, or when Go can send null for a field the SDK doesn't accept null for.

### Examples

- [`protocol/examples/notes`](https://github.com/open-conduit/open-conduit/tree/main/protocol/examples/notes): every extension surface. It has ingress with a webhook receiver, a route with a trigger, operations including a committed emission, a recurrence, tasks, a resource provider, a telemetry exporter, annotations, a setup flow, validation, three releases with schema evolution, and a KV migration. It declares every permission: a task records telemetry events and runs the host's probes, another builds, validates and traces a config candidate, it receives the host's telemetry events as a sink, and its entry type declares settings and describe lines.
- [`protocol/examples/notes` fixture profile](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/notes/extension.go): the declare-once form. `TestFixtureManifestIsDerived` checks that its derived manifest equals the hand-written one.
- [`protocol/examples/runner`](https://github.com/open-conduit/open-conduit/tree/main/protocol/examples/runner): an executor with detached jobs, inject, interrupt, stop and forget.
- [`protocol/examples/weather`](https://github.com/open-conduit/open-conduit/tree/main/protocol/examples/weather): an extension in TypeScript, with one operation answered from its scope's config and a lookup count in host KV.
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

### Checking a package

`open-conduit extension check DIR [--json] [--allow NAME,…]` lints a package without starting it or a daemon. It reports every problem it finds rather than the first, and each comes with what to change and a link to [Troubleshooting](#troubleshooting). The exit status is 1 when any finding is an error; warnings don't stop install or start. It checks:

- **the manifest:** the strict rules, each broken rule on its own line, and a command inside the package that exists and is executable;
- **compatibility with this binary:** a protocol version in common, no field from a newer Open Conduit, a command built for this OS and architecture, and no name Open Conduit registers itself;
- **the playbooks:** the declared directory exists and holds playbooks, and every `` `ns.op` `` or `"ns.op"` a playbook names, in a namespace the package or core owns, is an operation the package declares or core registers, an event family the package declares, or a setup probe the host runs. Names in other namespaces, such as config paths and file names, are left alone. A package's own setup probes run inside its process, so the check can't see them; name them with `--allow`;
- **`certification.json`:** it parses, and an inline fixture has an ingress case when an extension type takes ingress, an executor case naming a declared executor when there is one, and route and operation cases only for declared handlers and operations. A fixture printed by a command is left to `certify`.

Run it in the package's CI before `certify`. The setup package's own `make check` runs it, with `--allow setup.fs.list,setup.tools` for its probes.

Packaged Go binaries also answer `package-manifest` and `package-certification` (`sdk.PackageCommand`), so the manifest has one source of truth in code. `make packages` builds the first-party packages and the examples into `dist/packages/<name>/` this way.

### Bundled with the release

The release archive carries packages as well as binaries, under `extensions/<name>/` beside `open-conduit` and `os`. The daemon discovers them there as [bundled packages](../extension-protocol/#loading), and a package of the same name installed in `<config dir>/extensions` replaces the bundled one. Today the archive bundles one: `setup`, the [setup assistant](../setup-sessions/#where-it-lives-and-how-it-ships), which a fresh install needs for its first-run configuration.

- **Source.** A bundled package lives in its own repository and module, built on `protocol/sdk` like any third party's, and is pinned in this repository's `go.mod` with a `tool` directive. Nothing imports it.
- **Build.** `scripts/bundle-packages.sh OUT [GOOS_GOARCH…]` builds it from the module cache for each platform and writes its manifest, certification and playbooks beside the binary. GoReleaser runs it before building and adds each platform's directory to that platform's archive. `make packages` runs it for this host into `dist/packages/setup`, and `make certify` certifies it with the others. `make release-snapshot` checks that `extension list` shows it as bundled.
- **Install and upgrade.** install.sh and `open-conduit upgrade` replace `extensions/` beside the binary with the release's as a whole (upgrade stages it, then swaps it in after the binaries, so a failed swap leaves the old one). Packages installed in the config directory are never touched. The Homebrew cask keeps the whole archive in its directory, so the daemon finds `extensions/` beside the linked binary.
- **Updating the pin.** Bump the version in `go.mod` (`go get github.com/open-conduit/extension-setup@VERSION`). The package's own CI certifies it against the latest release and `main` before it is tagged.

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
- Install, update and rollback print the [permissions](../extension-protocol/#permissions) the installed release declares, each with what it allows, so the operator sees what the package may use beyond its own surfaces.
- **`open-conduit extension list`** shows installed packages, their protocol versions, what they provide, their permissions, the release kept for rollback, and any discovery or conflict problems. `inspect` shows a directory's permissions before install.

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
   - doctor checks, each under its own name beside the first-party ones (deep checks under `--check-secrets`), with the `extensions` check reporting the package's own health;
   - Work annotations, prompt rendering for the package's own families, and telemetry export.

It then sends `configure` again on every config apply and every tunnel URL change. Ingress registrations are immutable for one daemon instance, as for first-party ingress, so a renamed source needs a restart. Nothing in core branches on a package: every decision reads the manifest.

Secrets reach a package only through `secret.resolve`. It returns the secret names selected by the package's own entries in a configured scope, or, with `connection` set, the secret names of a connection one of those entries names (B05). `config check` offline knows the installed packages' types, and each package checks its own opaque config there too: it is started for `validate` alone, without a coordination store, on a throwaway state directory, and stopped afterwards ([doctor checks](../extension-protocol/#doctor-checks)). `open-conduit init TYPE` offers installed packages' setups by starting the package for the duration of the flow, without a coordination store.

An installed package's `work_kind.trigger`, `recurrence.next` and `dependency.filter` run through the [bounded in-transaction calls](../extension-protocol/#transactions-and-in-transaction-calls), with the same fallbacks.

A package's playbooks join the Claude Code plugin the daemon writes at start, beside the first-party ones. Each `<name>.md` file in the manifest's `playbooks` directory is the playbook (a skill) of that name; other files are ignored. `{{bin}}` in the text becomes how the agent runs Open Conduit (`open-conduit` in a turn). A package playbook never replaces a first-party one of the same name: it is logged and left out. `init --with claude` starts on the first-run setup's playbook, the one named after its setup type, from the package.

## Diagnostics

Every refusal and failure gets a stable code, a summary, a remedy and a link to [Troubleshooting](#troubleshooting) (`extproc.Diagnose`). They appear in the daemon log, the `extensions` doctor check and the CLI. They never include config values or secrets.

Unsupported capabilities are refused at config validation, exactly as for in-process executors: a binding requiring `inject` against an executor that does not declare it is `unsupported_capability` at the binding's `requires` path. An undeclared surface is never called, and callbacks from one are refused with `undeclared`.

## Troubleshooting

What each code means and what to change. `extension check` gives the codes marked *check* before install; the others come from install, start, `config check` or doctor.

| Code | What is wrong | What to change |
| --- | --- | --- |
| `invalid_package` (check) | A manifest rule is broken, the command is missing or not executable, or the playbooks directory is missing | Run `extension check DIR`: it names each rule and the change for it |
| `incompatible_protocol` (check) | The package and this host share no protocol version | List a version this host speaks in `protocol` and build against an SDK release for it, or upgrade Open Conduit |
| `needs_newer_host` (check) | The manifest has a field this Open Conduit doesn't know | Correct it if it's a typo; otherwise upgrade Open Conduit to the release that added it, or install a release of the package built for this one |
| `wrong_platform` (check) | The command is built for another OS or architecture | Build it for this platform, or install the release archive built for it |
| `registration_conflict` (check) | The package claims a name Open Conduit or another package registers | Rename what the package declares; first-party names (`slack`, `shell`, `github`, …) can't be installed alongside |
| `unknown_operation` (check) | A playbook names an operation nobody declares | Declare it under `provides.operations`, correct the name, or `--allow` it if it is one of the package's own setup probes |
| `no_playbooks` (check, warning) | The playbooks directory has no `<name>.md` | Add one, or drop `playbooks` from the manifest |
| `no_certification` (check, warning) | No `certification.json` | Add one so `certify` can check each surface ([fixtures](#fixtures)) |
| `invalid_certification` (check) | `certification.json` doesn't parse or has neither a fixture nor a command | Fix it against `protocol.CertificationFile` |
| `fixture_missing_case` (check) | The fixture lacks a case `certify` requires, or a case names something undeclared | Add an ingress or executor case, or name a declared handler or operation |
| `invalid_archive` | The release archive was refused | Check the URL or path and its `--sha256`; an archive is a `.tar.gz` holding one package directory of regular files |
| `install_state` | Installing over an installed package, or changing one that isn't installed | `extension list` shows what is installed; use `update` to replace |
| `manifest_mismatch` | The process announced a different manifest | Binary and manifest are from different releases; reinstall |
| `kv_downgrade` | Older KV layout than recorded | Roll forward, or restore a backup |
| `start_failed` | The command could not start | Check that it exists and is executable for this platform |
| `handshake_failed` | Exited or silent during `initialize`/`configure` | Read `<state>/logs/extensions/<name>.log`; only protocol on stdout, logs on stderr |
| `configure_refused` | Refused the config, or registered names outside its namespace | `config check` and the log |
| `binding_refused` | A route handler refuses the binding its route binds | Change the binding's executor, operations, grants or config to what the message asks, or route to another handler |
| `extension_restarted` (warn) | The process crashed and was restarted | The log |
| `extension_check_failed` (warn) | A declared doctor check failed | The check's detail |

## Testing a package

There are two harnesses. Use `sdktest` for a handler's logic: it runs in milliseconds and needs nothing built. Use `extensiontest` to see the package working in the daemon: configured from a real config, routed, run by an agent and shown to the operator.

### Unit tests: `sdktest`

[`protocol/sdk/sdktest`](https://github.com/open-conduit/open-conduit/blob/main/protocol/sdk/sdktest/sdktest.go) serves your `sdk.Extension` on an in-memory host behind a real `jsonrpc` pipe, so every call crosses the wire as it would in the daemon. `sdktest.New(t, ext, opts...)` initializes it with a temporary state directory.

- **Seeding (options):** `WithScopes`, `WithWorks`, `WithExecutions`, `WithEvents(scope, events...)` (accepted as causal roots), `WithKV`, `WithKVVersion` (the stored layout, so `Configure` runs `Migrate`), `WithSecrets` and `WithConnectionSecrets`, `WithAuthority` (what `Operation` calls carry) and `WithClock`. Pass `conformance.NewClock()` and advance it; the default clock is fixed at `sdktest.Start`.
- **Calling in:** `sdktest.Call[Out](ctx, h, name, in)` calls a declared operation with `in` encoded as its input and decodes the output into `Out`, so a test never builds a payload by hand. `Operation(ctx, name, input)` calls a declared operation by its kind and returns an `OperationResult`: a handler's output, a wait's plan, or an emission's accepted Event with its commit's output (the records are written to KV). `Configure`, `Route`, `RunTask`, `Check` and `CheckRun` cover the other common surfaces, and `Call` reaches any other method (`webhook.handle`, `event.render` …).
- **Inspecting:** `KV(namespace)`, `Events()` (seeded, derived, delivered and emitted), `Commits()` (each applied `kv.commit` batch), `Artifacts()`, `Captured()` and `KVVersion()`.

The host answers the scoped methods the way the daemon does. KV values carry the same version tokens, a stale check is `storage.ErrConflict`, and another extension's namespace is unauthorized. `event.accept` needs a parent Event in the same configured scope, deduplicates by source and delivery ID, and writes its records only when the Event is new. `ingress.deliver` runs the delivery through the source's adapter (`ingress.push`) from the last `Configure`. State reads and secrets see only configured scopes. A method the manifest does not enable is refused as undeclared. It does not enforce causal depth and rate limits, and it does not serve timers, Work control, probes or `config.*` (they answer unsupported). [`protocol/examples/notes`](https://github.com/open-conduit/open-conduit/blob/main/protocol/examples/notes/notes_test.go) is tested this way.

```go
h := sdktest.New(t, acme.Build(), sdktest.WithKV("acme.index", "a", []byte(`{"text":"hi"}`)))
got, err := sdktest.Call[acme.Note](ctx, h, "acme.lookup", acme.LookupRequest{ID: "a"})
```

### Daemon tests: `extensiontest`

To see the package working in the real daemon, its repository's tests use [`extensiontest`](https://github.com/open-conduit/open-conduit/blob/main/extensiontest/daemon.go), which needs only this module as a dependency. Pin the two commands it builds with `tool` directives, so `go mod tidy` keeps what they need and the build works from the module cache:

```
tool (
	github.com/open-conduit/open-conduit/cmd/open-conduit
	github.com/open-conduit/open-conduit/cmd/os
)
```

A test asks the question a package's author has: when an agent calls my operation, does it work? It names the package, scripts the agent, starts a Work and checks what came back:

```go
func TestForecast(t *testing.T) {
	service := httptest.NewServer(fakeWeatherService)
	defer service.Close()
	d := extensiontest.Daemon(t, extensiontest.Package("."),
		extensiontest.Secret("api_key", "test-key"),
		extensiontest.Entry("weather", map[string]any{"service": service.URL}))
	agent := d.Agent().
		Call("weather.forecast", map[string]any{"city": "Leeds"}).
		Call("weather.report", map[string]any{"summary": "rain"}).
		Ends("finished")

	work := d.Start("weather.check", nil)
	if r := agent.Result(1, "weather.forecast"); r.IsError || !strings.Contains(r.Text, "rain in Leeds") {
		t.Fatalf("forecast: %+v", r)
	}
	d.WaitWork(work.ID, "completed")
	reported := d.Events("weather.reported")
	// ...
}
```

- **`extensiontest.Package(src)`** is the package under test: a built package directory (one with an `open-conduit-extension.json`), or its Go main package, which the harness builds as `make packages` would, asking the binary for its manifest and certification and copying the manifest's `playbooks` from `src` or the module's root. A directory with exactly one main package under `cmd/` builds that one, so `Package(".")` works from a package repository's root.
- **The generated scope.** With a package and no `Options.Config`, the daemon runs one scope, `test` (`extensiontest.ScopeID`), made from the manifests:
  - an entry for each extension type, named after the type, with config `{}` unless **`extensiontest.Entry(type, config)`** gives one;
  - a binding running the scripted agent, granted every operation the manifests declare plus `read_context`, `wait` and `declare_outcome`;
  - a route for each route handler they declare, and a `manual` route last.

  Before the daemon starts, the generated config goes through `config check` with the packages installed. When it is refused, the test fails with each refusal, what to change (an `Entry`, or the whole config) and the generated config. A package directory that can't be built or whose manifest is refused fails the same way.
- **`extensiontest.Secret(name, value)`** adds a secret to the scope's `secret_refs` and selects it in every generated entry, so the package's `secret.resolve` returns `value` as it would the operator's. Use it to point the package at an `httptest` fake of its service.
- **`d.Agent()`** is the scope's scripted agent. `Call(operation, input)` adds a toolbelt call to the turn being scripted, `Ends(state)` ends it with that structured status (`finished`, `review`, `help`, ...), and `Turn()` starts the next. The agent plays one turn each time the daemon starts or resumes it, for any Work, so script it before starting the Work. `Played(n)` waits for turn `n` and returns the message it was started or resumed with and each call's result; `Result(n, operation)` is one call's result, its `Text` and whether it was a refusal (`IsError`).
- **Work and Events.** `d.Start(kind, input)` creates Work of a kind in the scope, run by the agent, with `input` as its context. `d.Submit(event)` submits an operator Event, as `open-conduit events emit` does. `d.WaitWork(id, state)` and `d.Events(type)` wait, up to `extensiontest.Deadline`, for the Work to reach a state and for at least one Event of a type. None of them sleeps for a fixed time.
- **The daemon.** `extensiontest.Daemon(t, options...)` builds `open-conduit` and `os` once per test binary, at the version `go.mod` pins (with `-race` when the test runs under it). It starts `serve` with its state in a temporary directory and a short poll interval, and returns once the management API answers. `Client()` is the operator's `*client.Client`: assert what an operator sees with `Pages`, `Actions` and `Panels`, and what was recorded with `Events` and `Work`. `Until(what, timeout, fn)` polls a condition; `Restart()` kills the daemon and starts it again on the same state. `Close`, which the test's cleanup runs, kills it and fails the test on a race report in its log or on a process it started that is still running; the log is printed when the test failed.

`TestExtensiontestPackage` tests a small package in `integration/testdata/weather` this way.

### A config of your own

When a test needs more than the generated scope (connections, several scopes, a binding with grants, the execution token itself), pass the whole config as `extensiontest.Options{Config: ..., Packages: []string{dir}}`. `Options.Packages` are installed under `extensions/<name>` beside it. With your own config there is no generated agent: write one with **`extensiontest.FakeClaude(dir, turns...)`** and name its `Path` as a `claude-code` binding's `binary`. Each launch plays the next `Turn`: its toolbelt `Calls` (`Operation`, `Input`, `RequestID`) through the real `os mcp` server, then a wait for its `Barrier` file if it has one, then the end of the turn with its `State`. `Turn(n)` returns what turn `n` saw once its calls are made, including its toolbelt endpoint and token while the turn holds at its barrier. Each Work needs a fake of its own. The fake is the test binary itself, run again with an environment variable set, so it costs no extra build. The setup assistant's daemon tests (`TestDaemonSetupSession` and `TestDaemonSetupChangeSet`, in [extension-setup](https://github.com/open-conduit/extension-setup)) are written this way, because they test the binding and the agent's token as well as the package.

### Running a package while you build it: `open-conduit dev`

Tests show a package works. To watch it work, with a Work you start, the agent calling the operation and the page the package declares, run it against a throwaway daemon:

```sh
open-conduit dev ./weather --secret api_key=env:WEATHER_KEY --entry 'weather={"service":"http://127.0.0.1:8080"}'
```

`open-conduit dev [DIR]` (default `.`):

- builds the package in `DIR` as [`extensiontest.Package`](#daemon-tests-extensiontest) does and installs it;
- writes a config of the [generated scope](#daemon-tests-extensiontest) (one entry per extension type, a `claude-code` binding granted the package's operations plus `read_context`, `wait` and `declare_outcome`, its routes and a `manual` route), and refuses to start, with each refusal and the generated config, when `config check` would;
- starts `serve` on that config with its state in a new directory under `/tmp`, never the operator's config or state, with the web view on a free local port (`--web-addr` picks one);
- prints the web view's sign-in URL, a link to each page the package declares, the `work create` command that starts a Work in the scope, and where the state, config and daemon log are;
- checks `DIR` for changes every half second, leaving out hidden directories, `dist`, `bin` and `node_modules`. On a change it rebuilds the package and restarts the daemon on the same state, so earlier Work and Events stay. A build or config that fails is reported and the running daemon is left as it was;
- on Ctrl-C (or SIGTERM), stops the daemon, kills anything it started that is still running a few seconds later (an agent's wrapper outlives its daemon by design), and removes the directory.

Flags:

- **`--secret NAME=VALUE`** adds a secret to the scope and selects it in every generated entry, as `extensiontest.Secret` does. `NAME=env:VAR` resolves it from `VAR` in `dev`'s environment instead, so the value stays off the command line. Repeat it for each secret.
- **`--entry TYPE=JSON`** is the config of the generated entry for `TYPE`, as `extensiontest.Entry` gives it; `{}` when not given.
- **`--agent PATH`** is the Claude Code CLI the binding runs; by default, `claude` on `PATH`. The agent runs with your own Claude Code sign-in (`CLAUDE_CONFIG_DIR` when set). It has no built-in tools, only the toolbelt.

`dev` and `extensiontest` share the build and the scope's generation ([`extensiontest/devscope`](https://github.com/open-conduit/open-conduit/blob/main/extensiontest/devscope/devscope.go)), so a package runs under `dev` exactly as its tests run it. `TestDev` drives `dev` against `protocol/examples/notes` with a scripted agent: a Work calls `notes.lookup`, the notes page loads, a change to the source restarts the daemon, and an interrupt leaves no process or directory behind.

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
- `make certify` certifies the TypeScript example `weather`; `make ts-check` typechecks and unit-tests the TypeScript SDK and the example.
- `TestTypeScriptWireMatchesGo`: every field of the TypeScript SDK's wire messages is a JSON field of the Go type it encodes, and every field Go can send as null is nullable there.
- `TestExternalSlackRoundTrip`: the Slack package round trip. Socket envelope through `ingress.deliver`, acknowledged after acceptance; remote route; `slack.reply` via `sdk.Store` and the projector against the fake; projection task; check; render.
- `TestExternalShellAdoptionAndArtifacts`: the shell package's process group survives an extension crash and is re-adopted with the same recovery metadata; its log reaches the host artifact store.
- `TestExternalSurfacesRoundTrip`: `validate`, `webhook.handle`, `provider.*` with `provider.owner_status`, `telemetry.export`, `run.environment`, `work.annotate`, and `setup.run` with callbacks.
- `TestSDKStoreOverProtocol`.
- `TestDaemonExtensionPackages` (in `make drill`): CLI install, conflict refusal, offline config check, a workflow end to end in the real daemon, the `validate` hook, configure on apply, no orphans after a daemon kill, restart, and removal refused while in use.
- Unit tests in `internal/extproc` (install, update, rollback, removal, diagnostics, and `extension check` in `check_test.go`).
- `TestExtensionNew`: `extension new` writes a repository whose own `make check` passes against this checkout, and refuses a directory that isn't empty.
- `TestExtensionCheckCommand`: `extension check` through the binary, text and `--json`.
- The first-party in-process suites still run unchanged: Slack and shell keep their in-process registration, and workflows A–D pass (`TestDaemonAllWorkflows`).

## Provisional decisions for user review

21. Packages are directories under `<config dir>/extensions/<name>/`, read only at daemon start. Install, update, rollback and removal are offline CLI commands that take effect at the next serve; there is no hot load.
22. Install stages and renames atomically, keeps exactly one previous release for rollback, refuses a lower `kv_version`, and refuses first-party names. A first-party package (Slack, shell) therefore cannot replace its in-process registration; the packages exist to prove and certify the boundary.
23. Removal keeps durable state (KV, recorded layout, state directory, log) and is refused while the config uses the package, unless forced.
24. Compatibility is the protocol version (no host-version range), which changes only for a breaking change, with a policy of keeping the previous protocol version for at least one release after one. Additive surfaces (new methods and manifest fields) ship without a bump; a package that uses one is refused by an older host with `needs_newer_host`.
25. The SDK's final shape: `sdk.Extension` fields per surface, `sdk.Host`, `sdk.Store` (precondition-on-read optimistic commits; prefix ranges not protected) and `sdk.Artifacts`, plus `sdk.PackageCommand` for packaging. `sdk.New` and its registrations (`Operation` with typed `sdk.Handler`, `sdk.Wait` and `sdk.Emit`, `Route`, `WorkKind`, `Task`, `Check`, `IngressType` …) fill the same fields and derive the manifest from them; an operation's input struct gives its manifest `input` schema.
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
