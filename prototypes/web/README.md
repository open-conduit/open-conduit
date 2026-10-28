# Open Conduit — web prototype (reference)

This is the design reference for the web view, not shipped code. The product is `internal/web` (server-rendered Go); stage 10 ported this prototype into it, and the screenshots in `reference/` are what each page was compared against. Nothing here is built by `make check` or the release.

```sh
cd prototypes/web
npm install && npm run dev   # http://localhost:5317
```

Fixture mode needs nothing else. The live mode (`/?source=live`) reads a local daemon's management socket, read-only.

---

An independent design exploration of Open Conduit's web interface. It was built outside the repository and changes nothing in it.

```sh
npm install
npm run dev          # http://localhost:5317
```

The status bar's **Prototype** menu switches between conditions without touching code:

| Condition | URL | Shows |
| --- | --- | --- |
| Healthy daemon | `/#/runs` | The full fixture world, with live simulation |
| Slow daemon | `/?condition=slow` | Loading states |
| Daemon unreachable | `/?condition=unreachable` | Error state and recovery steps |
| Fresh installation | `/?condition=fresh` | First run → setup assistant |
| Local daemon (read-only) | `/?source=live` | Real Work, runs and transcripts from your daemon |

Keyboard: `⌘K` jump anywhere · `g r/a/c/s` pages · `/` search · `j k` move · `Enter` open · `x` select · `[ ]` turns · `e` first error · `?` all shortcuts.

## Design direction

**A ledger, not a dashboard.** The product is an operations log that people steer, so the visual language comes from ledgers and instrument panels: warm paper ground, ink rules, condensed numerals, monospace for anything machine-produced. No cards-in-a-grid, no gradients, and per the repo's UI rules no one-sided accent borders, no small uppercase labels above headings and no numbers in coloured circles. Hierarchy comes from type weight, spacing and even, neutral borders.

**One signal colour, used only for people.** Vermilion means "a person is needed": an open question, an exhausted retry, a broken binding. Everything else is muted, and every state is a *shape* as well as a colour (◆ needs you, ● running, ◐ waiting, ■ held, ✓ done, ✕ failed), so colour is never the only cue.

**Time is the primary dimension.** Each row carries a *lifeline*: the Work's whole lifetime on one line, solid where something was executing, dotted where it waited, ticks where events arrived. "Active vs elapsed" — 4m executing over 1d 2h — is visible at a glance, and a run waiting on CI looks different from one waiting on a person.

**Say what it's waiting for.** The "Now" column is a sentence in the runtime's terms ("Waiting for CI · 3 of 7 checks finished on PR #137"), not a status pill. The state rail across the top is both the summary and the main filter.

**The run is a score.** Run detail shows turns as a sequence with the waits between them ("21h 20m · waiting on review"), then one turn's transcript. Toolbelt calls that changed something outside Open Conduit (`github.pr.create`, `slack.reply`, `human.ask`) are visually distinct from routine tool calls, which collapse into groups. Orchestration events (parked, woken, injected, cancelled) are rules across the transcript.

**Every action says what it will do, and what it really calls.** Confirmations list consequences in runtime terms ("an operator-cancelled turn is never retried automatically") and show the exact management API call with its expected revision. Receipts appear in toasts and in the "Needs you" log, always marked *Simulated*.

**Extensions contribute UI as data.** Views never name an extension. Whatever a provider returns from `GET /v1/{work|scopes}/{id}/actions` and `/panels` (extension `ActionProvider` and `PanelProvider`) is rendered by one generic renderer (`src/ui/extensions.tsx`): sections of text, fields, a table, links and action buttons, with no input, a prompt, or choices. Invoking one is `POST …/actions/{provider}/{action}`. Work an extension is offering a person an action on goes to *Needs you*. The agent's question on the Northwind thread is the `human` extension's real panel and `answer` action; the claim box on GitHub scopes is `github`'s real `claim` action. A new extension appears without UI changes.

**Configuration is a flow chart of real behaviour.** Each scope is drawn as one grid: events enter from its sources and meet route 1; a match goes right to the binding and the executor and workspace it runs on; no match falls down to the next route, and past the last to "unmatched". Each route shares a grid row with what it sends work to, so they always line up. The tracer lights up a sample event's path. On narrow screens the same chart becomes a vertical flow with the match branch indented. It is a fixed layout rather than a node canvas, because routing is ordered and first-match.

**Setup is a conversation with a ledger beside it.** The agent's probes, questions and proposals sit in the conversation; the change set, findings on this machine and validation results sit in a workbench beside it. Changes move proposed → approved → applied, and secrets are only ever references.

## Journeys to try

1. **GitHub development** — Runs → *Closed* → `#128`. Five turns: issue opened → PR #131 → checks pass → review comment → checks → merged. Turn 1 shows grouped tool calls and the external effects.
2. **Slack support** — *Needs you* → the Northwind refund question (the `human` extension's panel). Pick an option, send. The answer waits for a free agent slot (both are busy until #134 finishes, about a minute after load), then turn 1 resumes as a second segment of the *same* session, replies and parks on the thread.
3. **Failure investigation** — Runs → *Needs you* → `#117`. Three attempts failed in workspace setup on a DNS error; press `e` to jump to it; *Run again* shows consequences and the call; attempt 6 recovers and waits on CI.
4. **New installation** — Prototype menu → *Fresh installation* → *Start the setup assistant*. Choose GitHub, decide, try typing "use Sonnet" at review, approve all; validation fails on a real class of error (an operation outside the scope ceiling); approve the fix, apply, trace. Configuration then shows the new scope.

Also: the live turn on `#134` keeps producing output, with a review comment injected mid-turn; bulk-select rows with `x` and hold them; *Configuration → open-conduit → Trace an event*.

## What's live and what's simulated

| | Fixture mode | Live mode (`?source=live`) |
| --- | --- | --- |
| Work, turns, states, usage | constructed world | `GET /v1/work`, `/v1/runs`, `/v1/events`, `/v1/scopes`, `/v1/health` |
| Transcripts | constructed | Claude Code stream-json artifacts via `/v1/artifacts/{id}/content` |
| Configuration | mirrors the local `config.json` (redacted) plus a fixture `nightly` scope | scope routes and bindings from `/v1/scopes`; descriptions from fixtures |
| Extension actions and panels | fixtures shaped exactly like the providers' output | `GET /v1/{work,scopes}/{id}/actions` and `/panels` |
| Actions (core controls and extension actions) | simulated locally with realistic transitions and receipts; extension effects are fixture stand-ins (`src/data/fixtures/effects.ts`) | **never sent**: same local simulation |
| Setup assistant | scripted agent | scripted agent |

The live bridge in `vite.config.ts` reads the operator token server-side and forwards **GET only**; anything else gets `405 read_only`. The token never reaches the browser.

## Code map

```
src/domain/types.ts       view models (mirror wire types, with operator derivations)
src/data/adapter.ts       adapter boundary: fixture and live
src/data/live.ts          /v1 → view models, stream-json transcript parser
src/data/store.ts         state, action eligibility, simulated transitions, slot scheduler
src/data/setupAgent.ts    the setup agent's event stream (scripted)
src/data/fixtures/        the world: scenarios, config, route-trace samples
src/views/                Runs, WorkDetail + Transcript, Attention, Config, Setup
src/ui/                   marks (state shapes, lifeline), controls, action dialogs,
                          extensions (generic panel and action renderer)
src/data/actionSim.ts     where fixtures stand in for what extension actions do
src/styles/               tokens (light + dark), base, views
```

See [INTEGRATION.md](INTEGRATION.md) for what production would need.
