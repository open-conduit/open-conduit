# Open Conduit documentation

These documents are for people working on Open Conduit itself. To install and run it, start at [the user docs](https://open-conduit.github.io/open-conduit/) (built from `site/`) or [the top-level README](../README.md). [CONTRIBUTING.md](../CONTRIBUTING.md) covers setup, commits, boundaries and tests, and [SECURITY.md](../SECURITY.md) covers reporting vulnerabilities and the trust model.

## Building and checking

Use Go 1.24.2 or later, Python 3.10+ and Make.

- `make build` writes `bin/open-conduit` (the daemon and operator client) and `bin/os` (the toolbelt CLI that shell executions call).
- `make config-deps` installs the isolated configuration schema checker into `.venv-contracts`. Run it once before `make check`.
- `make check` runs formatting, vet, race tests (including the daemon integration tests), dependency boundaries, the configuration contract, the build and binary smoke checks. The tests also validate every example config in the README and in this directory against the daemon's own registrations, and check that the commands and make targets the README, this index, CONTRIBUTING.md and SECURITY.md show exist (`make docs-check` runs only these).
- `make cross-build` compiles Darwin and Linux binaries for amd64 and arm64 into `dist/`. It is build evidence only, not platform verification.
- `make drill` reruns the daemon kill and restart drills on their own.
- `make packages` builds the first-party and example extension packages into `dist/packages`, and `make certify` runs the conformance runner over them.
- `make protocol-prototype` reruns the transport comparison behind the extension protocol in its own module. It downloads its dependencies on first use, so it is not part of `make check`.

Hosted CI runs the same checks on Linux and macOS.

The user docs site lives in `site/` and is built with [Starlight](https://starlight.astro.build/). Run `npm ci` then `npm run dev` there to preview it; the `docs` workflow builds it on every pull request and publishes it to GitHub Pages from `main`. Its pages are checked by `make docs-check` like the README.

## Architecture

The architecture documents record the design and the evidence behind it, in roughly the order it was built. Their notes and headings carry milestone numbers (such as `03-05`) from that history.

**Core**

- [The core in one page](architecture/core.md)
- [Package boundaries and dependency rules](architecture/packages.md)
- [Primitive and transactional storage contracts](architecture/domain-storage.md)
- [Ingress, executor, toolbelt and observability contracts](architecture/extensions.md)
- [Routing, waits and recovery semantics with executable scenarios](architecture/runtime-semantics.md)
- [Configuration, secrets and local management contract](architecture/config-management.md) and [the configuration schema](architecture/config.schema.json), with [schema fixtures](architecture/examples/config/README.md)
- [Contracts, supported build targets and development tooling](architecture/contracts-gate.md)

**Runtime**

- [SQLite and local artifact guarantees](architecture/sqlite-artifacts.md)
- [Verified backup bundles](architecture/backup-bundles.md)
- [Durable event acceptance and reconciliation](architecture/event-ingress.md)
- [Durable routing, Work lifecycle and causal limits](architecture/routing-lifecycle.md)
- [Execution scheduling, recovery and control](architecture/execution-scheduling.md)
- [Durable timers, waits and dependency aliases](architecture/timers-waits.md)
- [Toolbelt authority, configuration and local management](architecture/toolbelt-management.md)
- [Shell executor and CLI toolbelt](architecture/shell-executor.md)
- [Lifecycle telemetry, unified run history and doctor](architecture/observability-history.md)
- [Daemon composition and the kill and restart drills](architecture/daemon-core-gate.md)

**Workloads and integrations**

- [GitHub ingress, routing policy and projections](architecture/github-extension.md)
- [Optional workspaces and the Git provider](architecture/workspaces.md)
- [Claude Code executor and private continuity](architecture/claude-code-executor.md)
- [Engineering tools, waits and playbooks](architecture/engineering-tools.md)
- [Slack ingress, conversations and marked replies](architecture/slack-extension.md)
- [Scheduled jobs: definitions, catch-up, the one-pending-run rule and job runs](architecture/schedule-extension.md)
- [Introspection: reporting, sweep, triage and fixes](architecture/issue-reporting.md)
- [Operator setup, doctor and engineering controls](architecture/operator-setup.md)
- [Installed integrations: Linear, Jira, Bitbucket, PostHog and operator tools](architecture/installed-integrations.md)
- [Backup, restore and state recovery](architecture/recovery.md)

**Extensions** (provisional)

- [Extension subprocess protocol: transport, manifest, runtime, trust model and schema evolution](architecture/extension-protocol.md)
- [Extension packaging, SDK, install, update, removal and certification](architecture/extension-packaging.md)
