@AGENTS.md

# Principles

Work is the fundamental abstraction. Everything else (an LLM, GitHub, Slack, a human, a shell command, even an execution) is a participant in the lifecycle of Work. [Core principles](site/src/content/docs/architecture/principles.md) is the full statement; read it before designing anything that touches the core. In five sentences:

1. Work is durable; executions are attempts.
2. Events describe reality; the runtime manages consequences.
3. The core owns orchestration semantics; extensions own domain semantics.
4. Any suitably capable participant can execute, observe, initiate or influence Work, subject to explicit authority.
5. Complex behaviour emerges through composition, not through special cases in the core.

The litmus test: if a new capability needs the core to understand a particular domain, integration or executor, first ask whether the abstraction is wrong. Either a reusable primitive is missing, or an extension should translate the behaviour into the existing primitives (Work, Event, Execution, Timer/Wait, Scope). A feature doesn't enter the core merely because it is widely useful.

# Working on Open Conduit

- **Know which side of the boundary you're on.** `domain`, `extension`, `storage`, `observability` and `protocol` are the public contracts; `internal/runtime` uses only those; `extensions/<name>` and `executors/<name>` use the contracts and `client`, never the runtime, SQLite or each other; `internal/bootstrap` and `cmd/open-conduit` wire it all together. `make boundaries` enforces this, and CONTRIBUTING.md's "Package boundaries" has the detail.
- **No privileged extensions.** Anything a bundled extension does must be possible for an independently installed one (`protocol/sdk`, `sdk/typescript`) without changing the core. If a first-party extension needs a new hook, add it to the public contracts, not a side door.
- **Don't assume the executor is an LLM.** Executors declare capabilities (run, resume, inject, interrupt) and the runtime binds them. A shell script or a human must fit the same model.
- **Put authority at the host.** Operations are checked where the host serves them; never rely on instructions in a prompt or playbook to keep an executor in bounds, and fail closed.
- **Let the platform wait.** Waiting, timers, retries and recovery belong to the runtime. Don't keep a process alive or poll to wait for something; express it as a Wait or Timer.
- **Leave a trail.** New behaviour should show up in run history and telemetry, linked from the triggering Event through Work and Execution to its outcome, so it can be inspected and corrected.
- **Prove contracts with the shared suites.** A new implementation runs the `conformance` suites; a new suite case comes with a negative control that it catches.
- **Docs move with the code.** The architecture pages in `site/src/content/docs/architecture/` describe current behaviour; update the relevant one in the same commit as the change.

# Process

[CONTRIBUTING.md](CONTRIBUTING.md) is the full guide. In short:

1. Branch from `main`. `main` only takes changes through pull requests.
2. Make one change per commit. If it alters behaviour an architecture document describes, update that document in the same commit.
3. `make check` is what CI runs (`fmt-check`, `vet`, `test -race`, `boundaries`, `config-check`, `docs-check`, `build`, `smoke`). Run `make fmt` to fix formatting. A docs-only change needs only `make docs-check`.
4. Push and open a pull request. CI runs `make check`, `make certify`, `make cross-build` and commitlint.

Tests never call real services, never wait on real time and never touch the operator's config or state; see "Tests" in CONTRIBUTING.md.

# UI

- No eyebrows or sidebrows: never give an item a coloured border on one side only (a left or top accent stripe on a card, panel, row or message). Use even, neutral borders and let layout and type carry the structure.
- No other stock AI-UI tells either: gradient text or backgrounds, glassy blur, emoji as icons, numbers in coloured circles, small uppercase tracked labels above headings, a pill on everything.
- Look at your own screenshot before sharing it. Things that relate (a route and its binding, a row's cells) must line up; when the layout depends on the data, compute the rows and put them in one grid rather than independent stacks.

# Commit messages and PR titles (commitlint)

The commitlint workflow lints **the PR title and every commit on the branch** with `@commitlint/config-conventional` (`.commitlintrc.json`). semantic-release reads them to pick the next version, so a bad message fails CI and a wrong type ships the wrong release.

The header is `type(scope): summary`:

- `type` is lower case and one of `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`, `chore`, `style` or `revert`. Nothing else passes: not `feature`, `bugfix`, `update`, `doc` or `tests`.
  - `feat` cuts a minor release and `fix` or `perf` a patch. Use them only for changes users see; tests, docs, CI and tooling take their own type.
- `scope` is optional, lower case, and names the package or area: `runtime`, `github`, `daemon`, `ci`, `readme`.
- `summary` starts lower case (not `Add …` or `Fix …`), uses the imperative, and has no full stop at the end.
- The whole header is at most 100 characters, including the scope and the ` (#123)` GitHub appends on a squash merge. Aim for under 90.
- A breaking change takes `!` before the colon (`feat(protocol)!: …`) or a `BREAKING CHANGE:` footer.
- Leave a blank line between the header and the body, and between the body and footers such as `Co-Authored-By:`. Body and footer lines have no length limit.

Good: `fix(github): drop the claimed label once completed work's issue is closed`

Bad, each fails: `Fix github label bug`, `fix(GitHub): Drop the label.`, `feature(github): drop the label`.

Don't write the `(#123)` PR number into a message yourself; GitHub adds it on merge.

On a squash merge, a PR with one commit lands as that commit's message and a PR with more lands as its title, so both must pass. Before pushing, lint the branch and the title you mean to use the way CI does (it needs Node):

```sh
npx -y -p @commitlint/cli@21 -p @commitlint/config-conventional@21 -- commitlint --from origin/main --to HEAD
printf '%s\n' 'fix(github): the PR title' | npx -y -p @commitlint/cli@21 -p @commitlint/config-conventional@21 -- commitlint
```

To fix a bad message, `git commit --amend` (last commit) or rebase and reword, then force-push the branch. Edit a bad PR title on the PR; commitlint reruns on edit.
