# Contributing to Open Conduit

This guide covers setting up a checkout, the checks a change must pass, and the rules the code follows. [The docs index](docs/README.md) lists the architecture documents behind each part. AI coding agents should also follow [AGENTS.md](AGENTS.md).

The repository has no license yet. Take licensing questions to the maintainer, for example in a GitHub issue, and ask before you start a large change.

To report a vulnerability, follow [SECURITY.md](SECURITY.md). Don't open a public issue for it.

## Setup

You need Go 1.24.2 or later (the minimum in `go.mod`), Python 3.10 or later, Make and Git, on macOS or Linux.

```sh
make config-deps   # once: installs the config schema checker into .venv-contracts
make check         # what CI runs
```

`make config-deps` is the only step that installs anything. `make check` runs offline. It runs, in order:

- `fmt-check`: `gofmt` on every Go file. Run `make fmt` to fix it.
- `vet`: `go vet`, including the fault-injection build tag.
- `test`: `go test -race ./...`, including the daemon integration tests in `integration/`. This is the slow part.
- `boundaries`: the package boundary checker (below).
- `config-check`: the configuration schema and its example fixtures.
- `docs-check`: the README and docs examples (below).
- `build` and `smoke`: builds `bin/open-conduit` and `bin/os`, and runs their help and version commands.

CI runs `make check` on Linux and macOS with Go 1.24.2 and the current stable Go. To check the minimum toolchain locally:

```sh
GOTOOLCHAIN=go1.24.2 make check
```

Other targets:

```sh
make cross-build          # darwin and linux, amd64 and arm64, into dist/ (build evidence only)
make drill                # the daemon kill and restart drills on their own
make certify              # builds the extension packages and certifies them, plus the negative controls
make protocol-prototype   # the extension transport comparison; downloads its own modules on first run
make docs-check           # only the docs example checks
```

`make protocol-prototype` lives in its own Go module under `prototypes/transport`, so its dependencies never enter `go.mod`. It needs the network the first time, which is why it isn't part of `make check`.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, for example `feat(slack): …` or `fix(runtime): …`. The history uses `feat`, `fix`, `docs`, `test` and `build`; `refactor` and `chore` are fine too. The scope is usually the package or area, such as `runtime`, `github`, `workspaces` or `readme`. Nothing enforces this yet, but please follow it.

Keep each commit to one change, and run `make check` before you push. If a change alters behavior that an architecture document describes, update that document in the same change. If it changes a README example or command, `make docs-check` tells you when the docs and the code disagree.

## Package boundaries

The core knows nothing about GitHub, Slack, Git or Claude. Decision B01 in [package boundaries](docs/architecture/packages.md) sets out who may import what:

- `domain`, `extension`, `storage`, `observability` and `protocol` are the public contracts.
- `internal/runtime` uses only the public contracts. It never imports an extension, executor, SQLite, config or management code.
- Extensions (`extensions/<name>`) and executors (`executors/<name>`) use the public contracts and `client`, never the runtime, SQLite or each other. The one exception is the shared tracker kit, which Linear, Jira and Bitbucket may use.
- `internal/bootstrap` and `cmd/open-conduit` are the composition root that wires implementations together.

`scripts/check-boundaries` (`make boundaries`) enforces these rules over Go's resolved production and test dependency graph, so a test import can't bring in an adapter either. A new top-level package is refused until it has a rule. Add the rule to `scripts/check-boundaries/main.go`, add a case to its tests, and record the ownership in `docs/architecture/packages.md`.

## Shared conformance suites and negative controls

`conformance` holds reusable suites for the public contracts. Every implementation of a contract runs them:

- `RunStorage` for coordination storage;
- `RunRuntime` for routing, waits, races and causal limits;
- `RunIngress`, `RunExecutor`, `RunInjectExecutor`, `RunToolbelt` and `RunObserver` for extensions, executors, the toolbelt and telemetry;
- `RunExtensionKV` for an extension's scoped storage.

`conformance/reference` has small in-memory doubles for testing the suites themselves. Production code and runtime tests must not use them. Runtime tests run against the runtime under test, and real compositions belong in `integration/`.

A passing suite only means something if it can fail. Each suite has negative controls: deliberately weakened implementations, such as storage that loses deduplication, a toolbelt that skips authority or an extension that drops a version check. The control tests pass only when the suite rejects them (`TestWeakened…` in `conformance` and `internal/storage/sqlite`, and `TestCertificationNegativeControls` for extension packages). If you add a case to a suite, add or extend a negative control that the case catches.

## Adding an extension

There are two ways to add an extension. Both implement the same public contracts from `extension` (ingress, route handlers, toolbelt operations, executors, resource providers) and run the same shared suites.

**In-process (first-party).** Add a package under `extensions/<name>` (or `executors/<name>`) that imports only the public contracts. Register it in `internal/bootstrap`: its config types and validators, handlers, operations, executors and doctor checks. `internal/bootstrap/schedule.go` and `slack.go` are good models. Run the shared suites against it in its own tests, and add an end-to-end case in `integration/` when it changes daemon behavior. Domain types stay in the extension: core gets no new primitive, and the runtime never learns the service's vocabulary.

**Subprocess (installed package).** Write a program on `protocol/sdk` that speaks the extension protocol over stdin and stdout, then package, install and certify it with `open-conduit extension`. [Extension packaging](docs/architecture/extension-packaging.md) is the guide, and `protocol/examples/notes` and `protocol/examples/runner` are complete examples. `packages/slack` and `packages/shell` wrap first-party code the same way. `make certify` runs the conformance runner over all four. The protocol and packaging are provisional.

An extension runs with the operator's privileges either way. The host enforces the protocol surface, but it is not a sandbox (see [SECURITY.md](SECURITY.md#trust-model)).

## Tests

- **No real services.** Tests never call GitHub, Slack, Linear, Jira, Bitbucket, PostHog or Claude. Use the in-repo fakes, such as `extensions/github/githubtest` and `extensions/slack/slacktest`, which serve each service's API over `httptest`, and fake processes for CLIs.
- **No real time.** Use the explicitly advanced `conformance.Clock` (`conformance.NewClock()`), or inject a `Now func() time.Time` where a component takes one. Don't wait for timers with sleeps. Wait on a barrier or a condition with a deadline instead.
- **No shared state.** Use `t.TempDir()` for config, state and workspaces. Never read or write the operator's `~/.config/open-conduit` or `~/.local/state/open-conduit`, or Open Supervisor's files. Stop every process a test starts, and check that none is left behind.
- **Race detector.** `make check` runs everything with `-race`. A flaky test is a bug, so fix it rather than retrying.
- **Real daemon tests.** `integration/` starts the real daemon on SQLite, drives it through the CLI and client, and kills and restarts it (`make drill`). Add a case there when a change crosses the daemon boundary.

## Secrets in fixtures

Never commit a real credential, token, webhook secret or private URL: not in a fixture, a test, a config example, a log or a commit message.

- Configs and fixtures hold secret references (`env:NAME`, `keyring:name`, `file:name`), never values. The config validator rejects literals in secret slots.
- Fixture values must be obviously fake, like `test-token` or `xoxb-test`. Give webhook tests a synthetic signing secret and sign the body in the test.
- Tests that check redaction should assert that the fake value is absent from output, responses and storage.
- Keep `.env` files and local operator config out of the repository.
