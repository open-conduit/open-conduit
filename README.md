# Open Conduit

A Go rewrite of Open Supervisor around Work, Event, Execution, Timer/Wait and Scope.

The project is in the contracts stage. Primitive, storage, ingress, executor, toolbelt and observability contracts include volatile reference doubles and conformance tests; no production runtime is implemented yet. The delivery baseline is pinned to Open Supervisor revision `c2461e55a9d7ca32527bace9db5ecec8c5e60cb0`; the existing runtime remains independently installable as the fallback.

- [Core in one page](docs/architecture/core.md)
- [Package boundaries and dependency rules](docs/architecture/packages.md)
- [Primitive and transactional storage contracts](docs/architecture/domain-storage.md)
- [Ingress, executor, toolbelt and observability contracts](docs/architecture/extensions.md)
- [Parity inventory, workflows and acceptance scenarios](docs/baseline/parity.md)
- [Pinned source test index](docs/baseline/source-tests.md)

Ticket identifiers in these documents refer to the local delivery plan. The baseline records required outcomes and verification work; it does not claim that the rewrite passes those scenarios yet.

Run the current contract checks with `go test -race ./...` and `go vet ./...` (Go 1.24.2 or later). Command scaffolding and CI follow in the contracts gate ticket.
