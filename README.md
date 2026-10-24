# Open Conduit

A Go rewrite of Open Supervisor around Work, Event, Execution, Timer/Wait and Scope.

The project is implementing the core after completing the contracts gate. Primitive, ingress, executor, toolbelt and observability contracts have shared conformance suites. Production components include SQLite storage, local artifacts, verified backups, durable ingress, routing, Work lifecycle and causal limits. The daemon and executor scheduler remain in progress. The delivery baseline is pinned to Open Supervisor revision `c2461e55a9d7ca32527bace9db5ecec8c5e60cb0`; the existing runtime remains independently installable as the fallback.

- [Core in one page](docs/architecture/core.md)
- [Package boundaries and dependency rules](docs/architecture/packages.md)
- [Primitive and transactional storage contracts](docs/architecture/domain-storage.md)
- [Ingress, executor, toolbelt and observability contracts](docs/architecture/extensions.md)
- [Routing, waits and recovery semantics with executable scenarios](docs/architecture/runtime-semantics.md)
- [Configuration, secrets and local management contract](docs/architecture/config-management.md)
- [SQLite and local artifact guarantees](docs/architecture/sqlite-artifacts.md)
- [Verified backup bundles](docs/architecture/backup-bundles.md)
- [Durable event acceptance and reconciliation](docs/architecture/event-ingress.md)
- [Durable routing, Work lifecycle and causal limits](docs/architecture/routing-lifecycle.md)
- [Contracts gate, supported build targets and development tooling](docs/architecture/contracts-gate.md)
- [Parity inventory, workflows and acceptance scenarios](docs/baseline/parity.md)
- [Pinned source test index](docs/baseline/source-tests.md)

Ticket identifiers in these documents refer to the local delivery plan. The baseline records required outcomes and verification work; it does not claim that the rewrite passes those scenarios yet.

Use Go 1.24.2 or later, Python 3.10+ and Make. Run `make config-deps` once to install the isolated schema checker, then `make check` for formatting, vet, race tests, dependency boundaries, config fixtures and binary smoke checks. `make cross-build` checks Darwin/Linux amd64/arm64 compilation.

`make build` creates `bin/open-conduit` and `bin/os`. Both currently support help/version only; runtime/toolbelt commands return an explicit unavailable error. Hosted CI is configured for the same checks on Linux/macOS; the daemon is not implemented yet.

Configuration schema/example checks: see [the fixture instructions](docs/architecture/examples/config/README.md). These validate the design contract; local server, config reload and secret resolution follow in stage 02.
