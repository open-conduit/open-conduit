---
title: "Contracts gate and development checks"
---

Decision B06 · ticket 01-06 · reviewed 2026-10-24. This gate reviews the committed B01–B05 contracts at `d592e45` plus the 01-06 tooling/scaffold candidate following that revision. The local ticket records the resulting commit after review; this document identifies the reviewed base and candidate scope without a self-referential commit hash.

## Reproducible entrypoint

Use Go **1.24.2 or later** (the module's compatibility minimum), Python **3.10 or later** and Make. The config fixture checker uses pinned `jsonschema==4.23.0` in an isolated environment; it is not a Go/runtime dependency. CI selects Python 3.12. Dependency installation is explicit:

```sh
make config-deps
make check
make cross-build
```

`make check` checks formatting without modifying files, vets, race-tests all packages, checks the resolved production/test import graph, validates the configuration schema/examples, builds both binaries and smoke-checks help/version and unsupported commands. It does not install dependencies. `make fmt` formats Go sources; `make build` writes ignored `bin/open-conduit` and `bin/os`. Set `CONFIG_PYTHON=/path/to/venv/bin/python` to reuse an existing checker environment; `make config-deps` bootstraps the ignored `.venv-contracts/` otherwise. Build metadata uses `VERSION` and `COMMIT`, with local defaults `dev` and the current short revision.

The binaries are honest scaffolds: help/version work; runtime/operator/toolbelt commands return exit code 2 and open no configuration, database, socket or service. They provide no install, daemon, execution or ownership-transfer behavior. `os` keeps its PRD protocol identity. Stage 02 will replace the scaffold with authenticated local client calls.

The [CI workflow](https://github.com/open-conduit/open-conduit/blob/main/.github/workflows/ci.yml) runs `make check` on Ubuntu and macOS with the exact Go compatibility minimum and the stable Go selector; stable Ubuntu also cross-compiles both commands for Darwin/Linux amd64/arm64 with `CGO_ENABLED=0`. No hosted CI run is claimed by this local gate. Cross-builds prove compilation only; actual platform/runtime behavior must be verified as implementations arrive.

This plan targets Darwin/Linux architectures and deliberately defers Windows because B05 specifies Unix sockets and ownership/mode enforcement. Windows support requires a local transport/auth/permissions decision and equivalent tests first. The compatibility minimum is not a claim that an old toolchain remains security-supported; deployment builds must use a maintained toolchain, reviewed again at distribution (06-03). Go 1.24.2 remains useful as an explicit API/language compatibility check.

## Boundary enforcement

[The checker](https://github.com/open-conduit/open-conduit/blob/main/scripts/check-boundaries/main.go) decodes `go list -deps -test -json ./...` and traverses dependency closures, including external dependencies and Go's remapped internal/external test variants. It applies [B01](../packages/) to project dependencies and reports the complete forbidden import chain. Dependency test suites do not become a consumer's production dependencies. Tests cannot smuggle an adapter through a helper or a rewritten package variant.

Public contracts and volatile conformance/reference packages remain independent of runtime, SQLite, ingress adapters and executors. Runtime production code uses only its own packages and public domain/extension/storage/observability contracts. Runtime tests may use the **public `conformance` harness**, but not `conformance/reference`, SQLite or adapters: those tests must exercise the runtime under test. Backend/process/workload integration composition belongs in `integration/`, outside runtime. This narrowly scoped test exception is needed to run the shared suites against real implementations without weakening the core boundary. Other implementations can use the public harness for their shared contract tests.

`cmd/open-conduit` and `internal/bootstrap` assemble implementations; `cmd/os` uses public contracts/client plus the current shared CLI scaffold. CLI/TUI implementations depend on client/public contracts. Extension/executor closures may use public contracts/client and their own implementation family, never another adapter or server internals. New package dependencies require an explicit B01 ownership update; Go's `internal` visibility is insufficient.

Synthetic negative graph controls cover direct/transitive adapter leaks, runtime leakage into reference fixtures, domain contamination, executor-to-SQLite imports, unknown package dependencies, an external-helper bridge, runtime test-only adapter/SQLite/reference leakage and Go ImportMap rewriting. A positive control permits the runtime public harness and keeps dependency tests outside its closure. These checks run as ordinary Go tests in the gate.

## Review and evidence

The PRD primitive test still yields exactly Work, Event, Execution, Timer/Wait and Scope. The [one-page explanation](../core/) remains domain-free. B02 owns immutable Event identity, correlation ownership, transactional fencing, timers/KV and separate streaming artifacts. B03 owns in-process registrations, both ingress paths, executor-private continuity, scoped toolbelt authority and lifecycle/redaction hooks. B04 supplies deterministic routing, event-only wake, dependent aliases, retry/adoption and bounded causation. B05 resolves config inheritance/reload, secret isolation and local authenticated transport. Their public surfaces return no production implementation types.

Reference storage/executor/toolbelt/ingress/observer and runtime scenario suites run without a database, service, daemon or Git registration. No `internal/runtime`, SQLite backend, local management server, production executor or service integration exists at this gate. The only new `internal` implementation is inert CLI help/version formatting. The contract history precedes runtime work: B02 `7e0e65f`, B03 `f05020b`, B04 `7771e72`, B05 `d592e45`.

The shared runtime suite contains 19 cases covering ordered correlation routing, explicit policies, event/wait arbitration, cancellation/retry/adoption races and root-pinned causal bounds. Storage negative controls fail the shared cases for lost deduplication, non-atomic route coordination and non-atomic timer rollback. Extension negative controls fail canonical identity/loss declarations, unauthorized toolbelt calls and invalid telemetry. Runtime negative controls fail wait-registration/wake and causal-bound cases. The enclosing negative-control tests **pass only when the weakened implementations are rejected**; a green run is not evidence that broken implementations conform.

Local gate commands and their scope:

```sh
make check CONFIG_PYTHON=/tmp/open-conduit-b05-schema/bin/python
make cross-build
GOTOOLCHAIN=go1.24.2 make check CONFIG_PYTHON=/tmp/open-conduit-b05-schema/bin/python
go test -race -count=1 -v ./conformance -run 'TestWeakened'
git diff --check
```

All five commands passed locally on macOS arm64 with Go 1.27.3, including a separate Go 1.24.2 minimum-toolchain `make check`. Both commands cross-built successfully for all four OS/architecture pairs. The verbose negative-control run confirmed all three weakened-storage cases, both weakened-runtime cases and four weakened extension/authority/telemetry controls were rejected. The local ticket records review and resulting commit. Hosted CI, SQLite durability, real process kill/restart, socket/auth/secret resolution and real provider behavior are **not** demonstrated by this gate. They remain stage-02 and workload gates, not defects hidden by a reference fixture.

## Deferred decisions and stage-02 handoff

| Remaining decision/proof | Owner | Contract constraint |
| --- | --- | --- |
| SQLite/artifact durability and process restart | 02-01 through 02-09 | Implement shared storage/runtime suites; weaker artifact guarantees stay explicit. |
| Local auth/config reload/secret provider enforcement | 02-06/02-07 | B05's transport and authority decisions are settled; add implementation/crash/race tests. |
| OTel instrumentation/export coverage | 02-08 | Required B03 lifecycle correlation and redaction remain mandatory. |
| Optional workspace fresh/shared/copy/setup/cleanup | 03-02 | Generic resource descriptor only; no mandatory repository in Scope. |
| Inject consumption/completion acknowledgement | 04-02 (resolved: [generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement-04-02)) | Inject success means accepted, never proof of consumption; `Result.ConsumedInputs` is the proof on every outcome; `ErrNotAccepting` is definitive; preserve pending input. |
| One pending job rerun/replacement and possible coalescing | 05-01 | No speculative coalesce policy before workload evidence; preserve ordinary queue ordering. |
| Ownership transfer and active-state handling | 03-07, 06-02 | No dual scope owner; independent state. |
| Subprocess transport/manifest/schema evolution | 07-01 through 07-03 (07-01 provisional: [extension protocol](../extension-protocol/), JSON-RPC 2.0 over stdio) | No speculative third-party protocol before shipped workloads; schema evolution and packaging remain 07-02/07-03. |

No unresolved blocking contract defect remains after local checks and review. Stage 02 may start only once this ticket's final checks, evidence and commit are complete. Runtime implementation must pass the same public suites; a later primitive defect first updates contracts and negative controls, then reruns previous workload gates.
