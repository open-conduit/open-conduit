Gets the `check` CI job from 10–12 minutes towards under 3, without skipping or weakening anything it runs.

## Where the time went

From the step timestamps of a recent `main` run, `check` ran everything one after another on one runner: vet about 60s, staticcheck about 40s, unit tests under `-race` about 3 min, integration (4 shards in one runner) about 5 min, build about 30s, certify about 1 min.

The Go build cache didn't help. `setup-go` gives every job the same cache key, so on `main` the first job to finish saves it, and that's usually the actionlint job: `main`'s Linux cache is 25 MB. Every PR was compiling the whole tree from cold, `-race` included.

## Changes

- **Parallel jobs.** `check` is now `static` (`make check-static`: fmt, vet, staticcheck, boundaries, config contract, docs-check, build, smoke), `unit` (`make test-unit`), `integration (1/4)` to `(4/4)` (12 shards, 3 per runner) and `certify` (`ts-check` and `certify`). A job named `check` stays as the one required check. It fails if any of those failed or were cancelled, and passes when they were skipped because no code changed.
- **One cache per job.** `.github/actions/go` sets up Go and restores a cache of the job's own (build, modules, staticcheck). The cache is saved on pushes to `main` only, so PRs restore `main`'s caches and don't evict them. `release-snapshot` reuses `cross-build`'s cache and `docs-check` reuses `static`'s. The unit tests run with `GOFLAGS=-count=1`, so a cached test result is never reused.
- **Bounded caches.** After restoring, the action deletes build and staticcheck entries that the run which saved the cache didn't use. It knows which by a marker the action writes into GOCACHE at job start, which is saved with the cache, and because Go refreshes an entry's mtime whenever it uses one over an hour old. A saved cache therefore holds at most about two runs' output, instead of growing with every merge towards the repository's 10 GB allowance. The module cache is kept whole.
- **Balanced shards.** `scripts/test-integration.sh` hands the tests out longest first, each to the shard with the least work, using a short list of slow tests (any other test counts as 1s). Before, it dealt them out in turn by name. `INTEGRATION_RUN` picks which shards a run takes. Every test still runs in exactly one shard. The performance budget tests from #433 are in no shard: they run alone after the shards, on the runner that takes shard 1.
- **Faster tests, same assertions.** `TestCertificationFirstPartyPackages` certifies its eight packages in parallel subtests: 75s down to 18s locally, at about 50% CPU, so it is mostly waiting. The package helpers build their binaries with one `go build` per set of flags.
- **Docs.** CONTRIBUTING, CLAUDE.md and `architecture/performance.md` describe the split.

## Timings

This PR's runs had cold caches, because nothing is saved until `main` runs the new workflow. At 3e91d6b the parts behind `check` took 2m17s (`unit`) to 3m55s (`integration (1/4)`, which also runs the budget tests). About 1.5 minutes of each was compiling from cold. Under 3 minutes needs the warm caches, so the first PRs after the merge are the real measure. `cross-build` (5m28s cold) is required but outside `check`; it gets its own cache too.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
