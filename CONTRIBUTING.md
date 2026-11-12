# Contributing to Open Conduit

This guide covers setting up a checkout, the checks a change must pass, and the rules the code follows. [The architecture section of the docs site](https://open-conduit.github.io/open-conduit/architecture/) describes how each part is built and what it guarantees, and [the docs index](docs/README.md) lists the build and check commands. AI coding agents should also follow [AGENTS.md](AGENTS.md).

Open Conduit's core is released under the [FSL-1.1-ALv2](LICENSE), and the extension SDKs, public contracts and examples under the MIT License (each of those directories holds its own `LICENSE`). A contribution is accepted under the licence of the directory it changes; [Licensing](https://open-conduit.github.io/open-conduit/reference/licensing/) lists them. Ask before you start a large change.

To report a vulnerability, follow [SECURITY.md](SECURITY.md). Don't open a public issue for it.

## Setup

You need Go, Python, Make and Git, on macOS or Linux; [Toolchain](#toolchain) lists the versions and what else some targets need.

```sh
make config-deps   # once: installs the config schema checker into .venv-contracts
make check         # what CI runs
```

`make config-deps` is the only step that installs anything. Apart from Go fetching the modules `go.mod` and `tools/go.mod` pin into its module cache the first time, `make check` runs offline. It runs `check-static`, which is everything but the tests:

- `fmt-check`: `gofmt` on every Go file. Run `make fmt` to fix it.
- `vet`: `go vet`, including the fault-injection build tag.
- `lint`: [staticcheck](https://staticcheck.dev/) with the checks `staticcheck.conf` selects, including the fault-injection build tag. Fix a finding rather than disabling its check; when a check is wrong for this code base, turn it off in `staticcheck.conf` with a comment saying why.
- `boundaries`: the package boundary checker (below).
- `config-check`: the configuration schema and its example fixtures.
- `docs-check`: the README and docs examples (below), and that the generated reference pages are current.
- `build` and `smoke`: builds `bin/open-conduit` and `bin/os`, and runs their help and version commands.

and then `test`:

- `test-unit`: `go test -race` on every package but `integration/`.
- `test-integration`: the daemon integration tests in `integration/`, under `-race`. This is the slow part. `scripts/test-integration.sh` splits them into `INTEGRATION_SHARDS` shards (4 by default), sharing the tests out by expected duration from the list of slow tests at its top, and runs the shards at once; `INTEGRATION_RUN` runs only some of them (`make test-integration INTEGRATION_SHARDS=12 INTEGRATION_RUN='1 2 3'`). Each shard is a separate process, so a test there can't rely on state another test left behind. The performance budget tests (`TestInTransactionRPCBudget*`) are in no shard: they run alone after the shards, in the run that takes shard 1. When you add a test that takes more than a few seconds, add it to that list. A plain `go test -race ./...` runs the whole integration package in one process under go test's default ten-minute timeout, which a loaded machine can exceed; the timeout's panic then names whichever test happened to be running. Pass `-timeout 20m` (`make test` does), or run the integration tests through `make test-integration`.

CI runs the same checks on Linux with the current stable Go, spread over parallel jobs so a pull request waits only for the slowest: `make check-static`, `make test-unit`, `make test-integration` in four runners of three shards each, and `make ts-check` with `make certify`. The required `check` job passes when they all have. `make cross-build` and `make release-snapshot` run alongside. Each Go job restores a build cache of its own that the same job saved on its last run on `main` (`.github/actions/go`), so a pull request compiles only what it changed. The action also prunes the restored cache to what the run that saved it used, so a cache doesn't grow with every merge; the unit tests still run with `-count=1`, so no test result is reused. It doesn't test macOS or the Go 1.24.2 minimum, so check those locally when a change could affect them. A pull request that only changes docs (`site/`, `docs/**/*.md` and the Markdown at the repository root) skips it and runs `make docs-check` instead; the docs site only builds when `site/` changes. To check the minimum toolchain locally (`lint` says it is skipped there, since staticcheck needs Go 1.26 to build and its findings don't depend on the toolchain):

```sh
GOTOOLCHAIN=go1.24.2 make check
```

Other targets:

```sh
make cross-build          # darwin and linux, amd64 and arm64, into dist/ (build evidence only)
make drill                # the daemon kill and restart drills on their own
make ts-deps              # once: installs the TypeScript SDK's and example's npm dependencies (needs Node 22.18+)
make ts-check             # Prettier, typecheck and unit tests for the TypeScript SDK and its example (npm run format fixes formatting)
make certify              # builds the extension packages and certifies them, plus the negative controls (needs Node and npm)
make protocol-prototype   # the extension transport comparison; downloads its own modules on first run
make docs-check           # only the docs example checks
make docs-gen             # rewrite the generated reference pages (CLI, config, toolbelt operations)
make release-snapshot     # a local release build with GoReleaser (below); publishes nothing
```

`make protocol-prototype` lives in its own Go module under `prototypes/transport`, so its dependencies never enter `go.mod`. It needs the network the first time, which is why it isn't part of `make check`.

### Toolchain

| Tool | Version | Needed by |
| --- | --- | --- |
| Go | 1.24.2 or later (the `go` line in `go.mod`); `lint` needs 1.26 or later and is skipped, saying so, under an older one | everything |
| Make, Git | any recent | everything |
| Python | 3.10 or later, as `python3` (or set `PYTHON`) | `config-deps`, which creates the `.venv-contracts` virtualenv `config-check` runs in |
| staticcheck | pinned in `tools/go.mod`; nothing to install | `lint` |
| Node, with npm | 22.18 or later | `ts-deps`, `ts-check`, `ts-packages`, `certify` |
| GoReleaser | v2, on your PATH (or set `GORELEASER`) | `release-snapshot` |
| Docker | a running daemon (OrbStack or Docker Desktop on macOS) | `container-test`, `gateway-image` |

staticcheck is a `tool` in a module of its own, `tools/go.mod`, which `make lint` runs with `go tool -modfile=tools/go.mod staticcheck`. That keeps staticcheck's dependencies and its Go 1.26 minimum out of `go.mod`, and its version and checksums in the repository. To upgrade it, run `go get -tool honnef.co/go/tools/cmd/staticcheck@<version>` in `tools/`.

`make check` needs only Go, Make, Git and the `config-deps` virtualenv; it never needs Node. The targets above check for their tools first and stop with what to install (`scripts/require-tool.sh`): `config-check` without the virtualenv says to run `make config-deps`, and the Node targets say when `node` is missing or older than 22.18.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, for example `feat(slack): …` or `fix(runtime): …`. The history uses `feat`, `fix`, `docs`, `test` and `build`; `refactor` and `chore` are fine too. The scope is usually the package or area, such as `runtime`, `github`, `workspaces` or `readme`. The commitlint workflow checks every pull request's title and commits, because the release workflow reads them to pick the next version.

Keep each commit to one change, and run `make check` before you push. If a change alters behavior that an architecture document describes, update that document in the same change. If it changes a README example or command, `make docs-check` tells you when the docs and the code disagree. The site's command line, configuration and toolbelt operations references are generated from the CLI's help, the config schema with the adapter catalogue the packages' manifests describe, and the operation registry: change those sources, then run `make docs-gen`.

## Releases

Releases are cut by hand from the release workflow (Actions, release, Run workflow) on `main`. It runs `make check` and `make release-snapshot`, then semantic-release picks the next version from the Conventional Commits since the last tag and tags the commit, and GoReleaser publishes the GitHub release with the generated notes: `open-conduit` and `os` for macOS and Linux on amd64 and arm64, with `checksums.txt`. With no `feat`, `fix` or breaking commit since the last tag, nothing is released. The Homebrew cask in `open-conduit/homebrew-tap` is updated only when the `HOMEBREW_TAP_TOKEN` secret is set. Run the workflow with `dry_run` to see the next version and its notes. `main` only takes changes through pull requests, so a release commits nothing back to it: the GitHub releases are the changelog, and `CHANGELOG.md` stops at 1.0.0.

`make release-snapshot` runs the same GoReleaser build locally with `--snapshot` (CI runs it on every pull request that changes code). It needs `goreleaser` v2 on your PATH. It writes every archive, the checksums and the cask into `dist/release`, then smoke-tests this machine's archive and checks the version the binaries report.

## Package boundaries

The core knows nothing about GitHub, Slack, Git or Claude. Decision B01 in [package boundaries](https://open-conduit.github.io/open-conduit/architecture/packages/) sets out who may import what:

- `domain`, `extension`, `storage`, `observability` and `protocol` are the public contracts.
- `internal/runtime` uses only the public contracts. It never imports an extension, executor, SQLite, config or management code. Its tests may run it over SQLite through `internal/runtime/runtimetest`, which production code can't import.
- Extensions (`extensions/<name>`, executors included: an executor is an extension that declares one) use the public contracts, `client` and the protocol SDK they are served with, never the runtime, SQLite or each other. The one exception is the shared tracker kit, which Linear, Jira and Bitbucket may use.
- `internal/bootstrap` and `cmd/open-conduit` are the composition root that wires implementations together.

`scripts/check-boundaries` (`make boundaries`) enforces these rules over Go's resolved production and test dependency graph, so a test import can't bring in an adapter either. It also checks the core's source. The public contracts and `internal/runtime` import only the standard library (and not `database/sql`) unless the checker's `thirdParty` allowlist names a module. Their code, comments aside, names no service, executor or workflow (`github`, `slack`, `claude`, issue, review, pull request). No production file in `internal/bootstrap` imports an extension or executor: bootstrap composes only packages. Only an extension's own code, the SDKs that serve it (`protocol/sdk`) and `integration`'s tests import `extension/inproc`. A new top-level package is refused until it has a rule. Add the rule to `scripts/check-boundaries/main.go`, add a case to its tests, and record the ownership in `site/src/content/docs/architecture/packages.md`.

## Shared conformance suites and negative controls

`conformance` holds reusable suites for the public contracts. Every implementation of a contract runs them:

- `RunStorage` for coordination storage;
- `RunRuntime` for routing, waits, races and causal limits;
- `RunIngress`, `RunExecutor`, `RunInjectExecutor`, `RunToolbelt` and `RunObserver` for extensions, executors, the toolbelt and telemetry;
- `RunExtensionKV` for an extension's scoped storage.

`conformance/reference` has small in-memory doubles for testing the suites themselves. Production code and runtime tests must not use them. Runtime tests run against the runtime under test, and real compositions belong in `integration/`.

A passing suite only means something if it can fail. Each suite has negative controls: deliberately weakened implementations, such as storage that loses deduplication, a toolbelt that skips authority or an extension that drops a version check. The control tests pass only when the suite rejects them (`TestWeakened…` in `conformance` and `internal/storage/sqlite`, and `TestCertificationNegativeControls` for extension packages). If you add a case to a suite, add or extend a negative control that the case catches.

## Adding an extension

Every extension, first-party or not, runs as a package: its own process, speaking the [extension protocol](https://open-conduit.github.io/open-conduit/architecture/extension-protocol/), discovered from its manifest and composed by bootstrap through the same surfaces as any other. Bootstrap names no extension, so nothing registers one in process. There are two ways to write one, and both implement the same public contracts from `extension` (ingress, route handlers, toolbelt operations, executors, resource providers) and run the same shared suites.

**A Go library on `extension/inproc` (first-party).** Add a package under `extensions/<name>` that imports only the public contracts. Implement the surfaces it has from `extension/inproc` (config types, operations, routes, start, configure, toolbelt registration, actions, annotations, rendering, tasks, doctor checks, playbooks, telemetry). Describe its config types' fields with `Fields` on each `inproc.ConfigType` (and `inproc.Executor`), and its `open-conduit init` setup, if it has one, with `inproc.SetupFlower`. Then bundle it as a package: add `extensions/<name>/pkg` with a `Manifest` and a `Main` that serve the extension through `protocol/sdk/inprocsdk`, which declares the manifest from those surfaces (with what it can't read from them, such as the title, summary and permissions, in a base manifest) and serves each surface over the protocol. Its manifest's command is `protocol.HostRunCommand(name)`, so `open-conduit extension-run <name>` serves it and it builds no executable of its own: add it to `runnable` in `cmd/open-conduit/extension_run.go` (with `Bundled` set, so the release archive carries it) and to `RUNNABLE_PACKAGES` in the Makefile, and certify it (`make certify`). `extensions/human` and `extensions/posthog` are good models, and `extensions/schedule` shows ingress, webhooks, timers and derived Events. If the contract lacks a surface you need, add it to the protocol, `protocol/sdk` and the `internal/extproc` adapter, so an installed package can use it too, and to `extension/inproc` and `inprocsdk`; never wire the extension into bootstrap.

**A program on `protocol/sdk` (installed package).** Write a program on `protocol/sdk` that speaks the extension protocol over stdin and stdout, then package, install and certify it with `open-conduit extension`. [Extension packaging](https://open-conduit.github.io/open-conduit/architecture/extension-packaging/) is the guide, and `protocol/examples/notes` and `protocol/examples/runner` are complete examples. A setup sets `sdk.Extension.Setups` and the manifest's `setup`. `sdk/typescript` is the SDK for TypeScript, and `protocol/examples/weather` is an example written with it. `extensions/sentry` and `extensions/pagerduty` are sources written only on the SDK and served by `open-conduit extension-run`, each with a fake of its service for tests and certification.

Either way, run the shared suites against it in its own tests, and add an end-to-end case in `integration/` when it changes daemon behavior. Domain types stay in the extension: core gets no new primitive, and the runtime never learns the service's vocabulary. `make certify` runs the conformance runner over every package `open-conduit extension-run` serves. The protocol and packaging are provisional.

An extension runs with the operator's privileges either way. The host enforces the protocol surface, but it is not a sandbox (see [SECURITY.md](SECURITY.md#trust-model)).

## Tests

- **No real services.** Tests never call GitHub, Slack, Linear, Jira, Bitbucket, PostHog or Claude. Use the in-repo fakes, such as `extensions/github/githubtest` and `extensions/slack/slacktest`, which serve each service's API over `httptest`, and fake processes for CLIs.
- **No real time.** Use the explicitly advanced `conformance.Clock` (`conformance.NewClock()`), or inject a `Now func() time.Time` where a component takes one. Don't wait for timers with sleeps. Wait on a barrier or a condition with a deadline instead. In `integration/`, `eventually` (and `waitFor`, `waitWithin`, `drill.until` built on it) is the one place that polls a condition, and `drill.settle` waits for the daemon's loop barrier: full host loop passes that began after the call, published by the `faultinject` build. To show that something did not happen, settle and then check. `TestNoSleep` parses `integration/`, `conformance/` and `internal/runtime/` and fails on any `time.Sleep` beyond its allowlist, which may only shrink.
- **No shared state.** Use `t.TempDir()` for config, state and workspaces. Never read or write the operator's `~/.config/open-conduit` or `~/.local/state/open-conduit`. Stop every process a test starts, and check that none is left behind. Leave nothing in the temp directory either: something kept for the whole test binary (a binary built once) needs removing after the run (in `integration/`, make it with `keptDir`, which `TestMain` clears). `make test-unit` and `make test-integration` run under `scripts/check-temp-dir.sh`, which fails a run that leaves anything there.
- **Race detector.** `make check` runs everything with `-race`. A flaky test is a bug, so fix it rather than retrying.
- **Runtime tests.** `go test ./internal/runtime/...` runs the shared runtime suite (`conformance.RunRuntime`) and the router, scheduler, wait, timer, causal and ingress cases against the production runtime on SQLite in a temp dir with `conformance.Clock`, in seconds. `internal/runtime/runtimetest` has the fixtures. Put a case there when it only exercises runtime semantics.
- **Real daemon tests.** `integration/` starts the real daemon on SQLite, drives it through the CLI and client, and kills and restarts it (`make drill`). Add a case there when a change crosses the daemon boundary.

## Secrets in fixtures

Never commit a real credential, token, webhook secret or private URL: not in a fixture, a test, a config example, a log or a commit message.

- Configs and fixtures hold secret references (`env:NAME`, `keyring:name`, `file:name`), never values. The config validator rejects literals in secret slots.
- Fixture values must be obviously fake, like `test-token` or `xoxb-test`. Give webhook tests a synthetic signing secret and sign the body in the test.
- Tests that check redaction should assert that the fake value is absent from output, responses and storage.
- Keep `.env` files and local operator config out of the repository.
