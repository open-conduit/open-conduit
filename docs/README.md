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

The architecture documents are published as [the Architecture section of the docs site](https://open-conduit.github.io/open-conduit/architecture/), and their source is in `site/src/content/docs/architecture/`. Each page separates current behaviour and guarantees from the design decisions (B01 to B08) and from a History section recording how that part was built. [Performance](https://open-conduit.github.io/open-conduit/architecture/performance/) gathers the performance evidence and what is still unmeasured.

This directory keeps only [the configuration schema](architecture/config.schema.json) and [its fixtures](architecture/examples/config/README.md), which `make config-check` and the config tests read.
