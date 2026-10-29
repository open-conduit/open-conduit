# Integration gaps

What the prototype assumes that the daemon (origin/main as of 2026-10-28) does not yet provide, roughly in order of importance.

## 1. A browser-hosted setup session

The setup assistant needs the `setup` playbook running in a Claude Code session whose output arrives as structured events rather than terminal text. `open-conduit init --with claude` runs the playbook interactively in a terminal today. Missing:

- An endpoint to start, message and stop a setup session, streaming typed events: `message`, `probe {cmd, why, result, status}`, `question {options}`, `proposal {pointer, before, after, why}`, `checks`.
- A config **change-set** API: propose JSON-pointer edits, keep them pending, apply only approved ones. Today `config set`/`config apply` write directly; `config validate` exists and fits.
- A policy that the agent can read the environment but not apply changes; only the operator's approval calls `config apply`.
- A **dry-run route trace** for a synthetic event (`open-conduit route` explains events already accepted). Both the setup test step and the Configuration tracer depend on it; the tracer's results are fixtures today.

`src/data/setupAgent.ts` exposes the event stream the UI consumes, so a real session can replace the script without UI changes.

## 2. Why a Work is waiting

`wireRun` gives `wait_id` and the Wait gives match keys and types, but not the reason in operator terms. The prototype's "Waiting for CI · 3 of 7 checks finished on PR #137" needs either:

- a source-supplied *wait description* (the GitHub extension knows it is waiting on checks for pull/137), or
- the extension's Panels API (`GET /v1/work/{id}/panels`) to carry it.

The live adapter falls back to guessing from the outcome summary.

## 3. Titles and external links

`wireWork.context` is opaque. Rows need a title, a requester and links (issue, PR, branch, Slack thread). The live adapter currently uses the outcome summary as the title. A small, source-declared `display` payload (title, url, actor) on Work would fix it.

## 4. Event summaries and routing explanations per Work

The Events tab shows each Event's route, binding, policy and reason. `GET /v1/events/{id}/route` provides that one Event at a time; a per-Work list (`GET /v1/work/{id}/events?with=route`) would avoid N requests. Event payload summaries ("@octo-dev commented on PR #131") also need source-supplied text.

## 5. Extension actions and panels in lists

Actions and panels are per target (`GET /v1/work/{id}/actions`, `/panels`), so live mode makes two requests per open Work to fill *Needs you*. A list form (`GET /v1/work?with=actions,panels`, or an index of Work with available actions) would make it one request. Panels are documents with no markup, so the web view renders them like the TUI does; nothing extension-specific is needed in the UI.

## 6. Live updates

The prototype simulates streaming. Production needs a change stream (SSE on `/v1/events` or `/v1/runs/{id}/live` already exists for output tail) so rows, slots and transcripts update without polling.

## 7. Web authentication and mutation safety

The live bridge reads the operator token on the dev server. A shipped web view needs its own session (the existing `internal/web` has a login link and cookie), and every mutation must send `expected_revision` and an `Idempotency-Key`, as the dialogs show.

## 8. Extension and binding detail in `/v1/config`

`GET /v1/config` returns each extension entry's `name` and `type`, but not its `config`, and bindings without `operations`. Live mode can therefore say a scope *has* a `human` entry, but not who answers (`responders`, `surfaces`) or which bindings may call `human.ask`; the UI says so rather than guessing. Exposing non-secret entry config and binding operations (secret references stay redacted) would close this.

## 9. Smaller items

- Execution segments: `wireRun` has `started_at`/`finished_at` only, so active time for resumed parked attempts can't be split into segments. Segment start/end (or total active ms) would make "active vs elapsed" exact.
- Agent slot usage (`settings.concurrency` in use) is not in `/v1/health`.
- Attach needs a terminal; the web shows the command rather than pretending to attach.
- Scope descriptions and route "matches" text are written by hand in fixtures; handlers could declare them.
