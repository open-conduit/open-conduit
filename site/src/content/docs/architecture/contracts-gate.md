---
title: "Development checks and the contracts gate"
description: "Decision B06: the reproducible checks every change passes, how they enforce the package boundaries and contracts, and the record of the stage 01 contracts gate."
---

This page records decision B06: the reproducible development checks every change must pass, and how they enforce the [B01](../packages/) package boundaries and the B02–B05 contracts. The checks are Make targets in the [Makefile](https://github.com/open-conduit/open-conduit/blob/main/Makefile), the [boundary checker](https://github.com/open-conduit/open-conduit/blob/main/scripts/check-boundaries/main.go) and the [CI workflow](https://github.com/open-conduit/open-conduit/blob/main/.github/workflows/ci.yml). The review that closed the stage 01 contracts gate is kept under [History](#history).

## Reproducible entrypoint

Use Go **1.24.2 or later** (the module's compatibility minimum), Python **3.10 or later** and Make. The config fixture checker uses pinned `jsonschema==4.23.0` in an isolated environment; it is not a Go/runtime dependency. CI selects Python 3.12. Dependency installation is explicit:

```sh
make config-deps
make check
make cross-build
```

`make check` checks formatting without modifying files, vets (including the `faultinject` build of the composition root), race-tests all packages (including the daemon drills in `integration/`), checks the resolved production/test import graph, validates the configuration schema/examples, checks the documentation examples (`make docs-check`), builds both binaries and smoke-checks help/version and that unknown or incomplete commands exit with code 2. It does not install dependencies or use the network. `make fmt` formats Go sources; `make build` writes ignored `bin/open-conduit` and `bin/os`. Set `CONFIG_PYTHON=/path/to/venv/bin/python` to reuse an existing checker environment; `make config-deps` bootstraps the ignored `.venv-contracts/` otherwise. Build metadata uses `VERSION` and `COMMIT`, with local defaults `dev` and the current short revision.

`make docs-check` checks the user-facing documents, including every page of the docs site: complete example configs (`json` code blocks containing `"version"`) pass offline validation, every `open-conduit` command in an `sh` code block exists with the flags it uses, and every `make` target shown is in the Makefile.

Other targets run evidence that `make check` does not show on its own:

- `make drill` runs the daemon kill/restart drills verbosely (they are also part of `make check`).
- `make certify` builds the extension packages (`make packages`), runs `open-conduit extension certify` over each and then the certification negative controls.
- `make protocol-prototype` runs the transport comparison in the separate `prototypes/transport` module. It downloads modules on first run, so it is not part of `make check`.
- `make release-snapshot` builds every platform's release archive, the checksums and the Homebrew cask with GoReleaser, without publishing, and smoke-tests this host's archive.

## Continuous integration

The [CI workflow](https://github.com/open-conduit/open-conduit/blob/main/.github/workflows/ci.yml) runs `make config-deps`, `make check`, `make drill` and `make certify` on Ubuntu and macOS, each with the exact Go compatibility minimum (1.24.2) and the stable Go selector. Stable Ubuntu also runs `make cross-build`, which compiles both commands for Darwin/Linux amd64/arm64 with `CGO_ENABLED=0`. Cross-builds prove compilation only; platform/runtime behaviour is verified by the tests on each CI runner.

On a pull request, CI first classifies the changed files. The required jobs always report, but skip their steps when nothing they check has changed. A docs-only change runs `make docs-check` instead of the full checks. A change with code also runs `make release-snapshot`, and a change to a workflow is linted with `actionlint`. Pushes to `main` check everything.

## Supported platforms and toolchains

Open Conduit targets Darwin and Linux on amd64 and arm64, and deliberately does not support Windows, because B05 specifies Unix sockets and ownership/mode enforcement. Windows support requires a local transport/auth/permissions decision and equivalent tests first.

The compatibility minimum is not a claim that an old toolchain remains security-supported; deployment builds must use a maintained toolchain. The [release workflow](https://github.com/open-conduit/open-conduit/blob/main/.github/workflows/release.yml) and the release snapshot in CI build with the current stable Go for that reason. Go 1.24.2 remains useful as an explicit API/language compatibility check.

## Boundary enforcement

[The checker](https://github.com/open-conduit/open-conduit/blob/main/scripts/check-boundaries/main.go) decodes `go list -deps -test -json ./...` and traverses dependency closures, including external dependencies and Go's remapped internal/external test variants. It applies [B01](../packages/) to project dependencies and reports the complete forbidden import chain. Dependency test suites do not become a consumer's production dependencies. Tests cannot smuggle an adapter through a helper or a rewritten package variant.

Public contracts and volatile conformance/reference packages remain independent of runtime, SQLite, ingress adapters and executors. Runtime production code uses only its own packages and public domain/extension/storage/observability contracts. Runtime tests may use the **public `conformance` harness**, but not `conformance/reference`, SQLite or adapters: those tests must exercise the runtime under test. Backend/process/workload integration composition belongs in `integration/`, outside runtime. This narrowly scoped test exception is needed to run the shared suites against real implementations without weakening the core boundary. Other implementations can use the public harness for their shared contract tests.

`cmd/open-conduit` and `internal/bootstrap` assemble implementations; `cmd/os` uses public contracts/client plus the shared `internal/cli`. CLI/TUI implementations depend on client/public contracts. Extension/executor closures may use public contracts/client and their own implementation family, never another adapter or server internals. New package dependencies require an explicit B01 ownership update; Go's `internal` visibility is insufficient.

Synthetic negative graph controls cover direct/transitive adapter leaks, runtime leakage into reference fixtures, domain contamination, executor-to-SQLite imports, unknown package dependencies, an external-helper bridge, runtime test-only adapter/SQLite/reference leakage and Go ImportMap rewriting, alongside cases for each later package family. A positive control permits the runtime public harness and keeps dependency tests outside its closure. These checks run as ordinary Go tests in `make check`.

## Contract negative controls

The shared suites prove a contract only if a broken implementation fails them. Storage negative controls fail the shared cases for lost deduplication, non-atomic route coordination and non-atomic timer rollback. Extension negative controls fail canonical identity/loss declarations, unauthorized toolbelt calls and invalid telemetry. Runtime negative controls fail wait-registration/wake and causal-bound cases. The extension KV suite and the certification runner have their own weakened implementations. The enclosing negative-control tests **pass only when the weakened implementations are rejected**; a green run is not evidence that broken implementations conform. To see each control's verdict:

```sh
go test -race -count=1 -v ./conformance -run 'TestWeakened'
```

A primitive defect found later first updates the contracts and negative controls, then reruns the previous workload gates.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

### The stage 01 contracts gate (01-06)

Ticket 01-06 reviewed the committed B01–B05 contracts at `d592e45` plus the 01-06 tooling/scaffold candidate following that revision, on 2026-10-24. The local ticket recorded the resulting commit after review; the document identified the reviewed base and candidate scope without a self-referential commit hash.

At that gate the binaries were honest scaffolds: help/version worked; runtime/operator/toolbelt commands returned exit code 2 and opened no configuration, database, socket or service. They provided no install, daemon, execution or ownership-transfer behaviour. Stage 02 replaced the scaffold with authenticated local client calls. The CI workflow then ran `make check` on Ubuntu and macOS with the Go minimum and stable, plus the stable Ubuntu cross-build; no hosted CI run was claimed by the local gate. Toolchain support was to be reviewed again at distribution (06-03).

**Review.** The primitive test in the planning document still yielded exactly Work, Event, Execution, Timer/Wait and Scope. The [one-page explanation](../core/) remained domain-free. B02 owned immutable Event identity, correlation ownership, transactional fencing, timers/KV and separate streaming artifacts. B03 owned in-process registrations, both ingress paths, executor-private continuity, scoped toolbelt authority and lifecycle/redaction hooks. B04 supplied deterministic routing, event-only wake, dependent aliases, retry/adoption and bounded causation. B05 resolved config inheritance/reload, secret isolation and local authenticated transport. Their public surfaces returned no production implementation types.

Reference storage/executor/toolbelt/ingress/observer and runtime scenario suites ran without a database, service, daemon or Git registration. No `internal/runtime`, SQLite backend, local management server, production executor or service integration existed at this gate. The only new `internal` implementation was inert CLI help/version formatting. The contract history preceded runtime work: B02 `7e0e65f`, B03 `f05020b`, B04 `7771e72`, B05 `d592e45`. The shared runtime suite contained 19 cases covering ordered correlation routing, explicit policies, event/wait arbitration, cancellation/retry/adoption races and root-pinned causal bounds.

**Evidence.** Local gate commands and their scope:

```text
make check CONFIG_PYTHON=/tmp/open-conduit-b05-schema/bin/python
make cross-build
GOTOOLCHAIN=go1.24.2 make check CONFIG_PYTHON=/tmp/open-conduit-b05-schema/bin/python
go test -race -count=1 -v ./conformance -run 'TestWeakened'
git diff --check
```

All five commands passed locally on macOS arm64 with Go 1.27.3, including a separate Go 1.24.2 minimum-toolchain `make check`. Both commands cross-built successfully for all four OS/architecture pairs. The verbose negative-control run confirmed all three weakened-storage cases, both weakened-runtime cases and four weakened extension/authority/telemetry controls were rejected. Hosted CI, SQLite durability, real process kill/restart, socket/auth/secret resolution and real provider behaviour were **not** demonstrated by this gate. They were left to stage-02 and workload gates, not defects hidden by a reference fixture. No unresolved blocking contract defect remained after local checks and review, and stage 02 could start once the ticket's final checks, evidence and commit were complete.

**Deferred decisions and stage-02 handoff.** The gate handed these decisions and proofs to later tickets:

| Remaining decision/proof | Owner | Contract constraint |
| --- | --- | --- |
| SQLite/artifact durability and process restart | 02-01 through 02-09 | Implement shared storage/runtime suites; weaker artifact guarantees stay explicit. |
| Local auth/config reload/secret provider enforcement | 02-06/02-07 | B05's transport and authority decisions are settled; add implementation/crash/race tests. |
| OTel instrumentation/export coverage | 02-08 | Required B03 lifecycle correlation and redaction remain mandatory. |
| Optional workspace fresh/shared/copy/setup/cleanup | 03-02 | Generic resource descriptor only; no mandatory repository in Scope. |
| Inject consumption/completion acknowledgement | 04-02 (resolved: [generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement)) | Inject success means accepted, never proof of consumption; `Result.ConsumedInputs` is the proof on every outcome; `ErrNotAccepting` is definitive; preserve pending input. |
| One pending job rerun/replacement and possible coalescing | 05-01 (resolved: [pending-job coalescing](../runtime-semantics/#pending-job-coalescing-and-standing-work)) | No speculative coalesce policy before workload evidence; preserve ordinary queue ordering. |
| Ownership transfer and active-state handling | 03-07, 06-02 | No dual scope owner; independent state. |
| Subprocess transport/manifest/schema evolution | 07-01 through 07-03 (07-01 provisional: [extension protocol](../extension-protocol/), JSON-RPC 2.0 over stdio) | No speculative third-party protocol before shipped workloads; schema evolution and packaging followed in 07-02/07-03. |

### Later changes to the checks

- The daemon drills (`make drill`) were added as stage 02 gate evidence; they also run as part of `make check`.
- `make protocol-prototype` (07-01), `make packages` and `make certify` (07-03) were added for the extension protocol work.
- `make release-snapshot` (09-03) added release packaging without publishing.
- `make docs-check` added the documentation example checks, and CI gained change detection so docs-only pull requests skip builds and tests.
