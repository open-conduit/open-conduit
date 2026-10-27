---
title: "Slack ingress, conversations and marked replies"
---

The first-party Slack extension in [`extensions/slack`](https://github.com/open-conduit/open-conduit/tree/main/extensions/slack) turns Slack threads into support conversations: a person's message starts a `slack.conversation` Work, an agent replies in the thread with marked replies, and later messages in the thread reach the same Work. It is built only against the public contracts: [B03 ingress](../extensions/), [B04 routing](../runtime-semantics/) and [B02 storage/KV](../domain-storage/). It has no workspace, repository or tracker concept, and the runtime does not import it. [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/slack.go) registers it.

This page records two decisions: the manifest requests `metadata.message:read` for reply markers, and a support conversation waits while it is open and completes only on resolution. The tests use fixtures and the [`slacktest`](https://github.com/open-conduit/open-conduit/tree/main/extensions/slack/slacktest) fake (Web API, Socket Mode and Events API); one short run on real Slack is recorded under [History](#history).

## The Slack app

[`manifest.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/manifest.go) is the single source of truth for the app the extension needs. `Manifest`, `ManifestJSON` and `CreateAppURL` produce it, and `TestManifestCoversWhatTheExtensionUses` drives every call the extension makes against the fake and checks that a manifest scope permits each one.

| Setting | Value |
| --- | --- |
| Socket Mode | On (the default receiver needs no public endpoint) |
| Bot user | Yes, always online |
| Bot token scopes | `channels:history`, `groups:history`, `channels:read`, `groups:read`, `chat:write`, `metadata.message:read`, `reactions:write`, `users:read`, `app_mentions:read` |
| Bot events | `message.channels`, `message.groups`, `app_mention` |
| App-level token | `connections:write` (Socket Mode only) |

The extension's own calls need only part of this:

- `conversations.history` and `conversations.replies` need `channels:history` (public channels) or `groups:history` (private channels).
- `chat.postMessage` needs `chat:write`.
- `auth.test` needs no scope.
- `users.conversations` (the setup channel picker) needs `channels:read`/`groups:read`.
- `apps.connections.open` uses the app-level token.

`users:read`, `reactions:write` and `app_mentions:read` are granted for future use (names, acknowledgements); nothing uses them yet. `app_mention` duplicates a `message` event, so push ignores it.

The extension relies on no scope outside this list. Reply markers use Slack **message metadata**. Posting metadata needs only `chat:write`. Reading it back uses `include_all_metadata` on history and replies.

**Decision: the manifest requests `metadata.message:read`.** Slack documents this scope as the one that lets an app read message metadata in its channels. Without it, marker recovery may fall back to matching the bot user and the exact text, which can confuse two identical replies in one thread. Setup creates the app from the manifest, so asking costs the operator nothing. It is read-only, covers only channels the app is in, and adds no write or user-data access. The text fallback stays for tokens installed without it (an older manifest, or a workspace admin who refused the scope). Doctor reports the missing scope as a warning naming that cost. On a real app with the scope, `conversations.replies` with `include_all_metadata=true` returns the marker's full payload `{work, execution, intent}`; without `include_all_metadata`, only the `event_type` comes back. Whether metadata comes back without the scope is untested, so the text fallback is proven only against fixtures.

### Setup: `init slack`

Setup belongs to the extension. [`setup.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/setup.go) implements the generic optional [`extension.Setup`](https://github.com/open-conduit/open-conduit/blob/main/extension/setup.go) hook (see [operator setup](../operator-setup/#extension-owned-setup)). Core hosts it and supplies the prompts, the browser opener, the secret store, the merge and offline validation. `open-conduit init slack`:

1. **Creates the app from the manifest.** By default the host opens `CreateAppURL(name)` (`--name`, default "Open Conduit"; `--no-open` prints it instead). With `--via slack-cli`, and only when `slack` is on PATH, setup writes a minimal local-manifest project under `<state>/slack-app/<scope>` instead: `manifest.json`, a `get-manifest` hook that prints it, and `manifest.source: local`. It then runs `slack app install --environment deployed` attached to the terminal. When the CLI is on PATH but not chosen, setup mentions it.
2. **Explains the tokens.** It says where to find the bot token (OAuth & Permissions, `xoxb-`). It explains how to create the app-level token, which Slack has no API for: Basic Information → App-Level Tokens with `connections:write`, giving `xapp-`. It also says to invite the app to the channel.
3. **Prompts for both tokens and verifies them.** The prompts are secret and checked by prefix. The tokens are verified before anything is stored: `auth.test` gives the team, the bot user and the granted scopes (`X-OAuth-Scopes`), and missing manifest scopes are warned about. `apps.connections.open` checks that the app token can open Socket Mode. A refusal stops setup with nothing stored.
4. **Picks the channel.** It lists the channels the bot is in (`users.conversations`). If there are none, it asks the user to invite the app and paste the channel ID. A given `--channel` is checked for membership.
5. **Stores the tokens and returns the scope.** Both tokens go through the host's secret store (`<connection>-bot-token`, `<connection>-app-token`; only references reach the config). Setup returns the `slack` connection and a workspace-free support scope:
   - a `support` binding on `claude-code` (`requires` toolbelt mcp, inject, resume, `workspace: false`, no `resource`), with playbook `support-conversation`, args `{key}` and the operations `read_context`, `wait`, `declare_outcome`, `slack.context`, `slack.thread` and `slack.reply`;
   - the `conversations` route (handler `slack`, active policy `inject`);
   - the entry (`namespace` `slack/<team>`, the channel, `bot_user_id`, `receiver: socket`, `start` from `--start`).

With `--bot-token REF --app-token REF --team T… --channel C…`, setup prompts for nothing and verifies nothing (it never resolves references); it says to run `doctor --check-secrets` instead. `--scope` (default `support`) and `--connection` (default `slack`) name the result. `--api-url` points at another API base (the tests use the fake).

The `slack-cli` path is unverified against a real Slack CLI. The manifest URL path and the token checks have been run against real Slack.

**Doctor.** `slack_auth` (`--check-secrets`) runs the extension-owned `slack.CheckAuth`. It checks that the token's team is the entry's team and its user is the entry's bot user. It also compares the granted scopes with the manifest (`MissingScopes`). A missing `chat:write`, or both history scopes missing, fails with `scopes_missing`. Any other missing scope warns with `scopes_missing` and says what it costs (for `metadata.message:read`: markers found only by the text fallback). Bootstrap only maps the severity onto the check.

## Configuration

A `slack` connection holds `api_url` (default `https://slack.com/api`). Credentials come only from its `secret_refs`, in the existing reference forms (`env:`, `file:`, `keyring:`, `cmd:`):

- `bot_token` (`xoxb-…`) for every Web API call.
- `app_token` (`xapp-…`, `connections:write`) for Socket Mode.

Tokens are resolved lazily. They are resolved again once after `invalid_auth` or `token_expired`, so a rotated reference applies without a restart. A credential failure is `ErrCredential` and never carries the reference or the value.

A `slack` extension entry is an ingress entry: `config.scope` must be the containing scope. It represents one app installation in one workspace, observing a set of channels.

| Field | Meaning |
| --- | --- |
| `namespace` | Stable source identity (for example `slack/T0123`). Also the correlation-key source. |
| `team` | The workspace's team ID. Push from another team is ignored. |
| `channels` | 1–100 channel IDs (`C…` public, `G…`/`C…` private) to observe. |
| `receiver` | `socket` (default), `http` or `none` (reconcile only). |
| `signing_secret` | Names a selected scope secret. Required for `http`. |
| `start` | `all` (default): every human top-level message starts a conversation. `mention`: a human message mentioning `bot_user_id` in a thread no Work owns starts one. |
| `bot_user_id` | The app's bot user. Required for `mention`; also recognizes the app's own unmarked messages. |
| `start_window_hours` | Default 24. A message starts a conversation only if it was first received within this time of being written, so the first reconcile does not turn history into Work. |
| `history_hours` | Default 336 (14 days), 1–2160. The reconcile window. |
| `refresh_minutes` | Default 60. How long an unchanged thread's replies are taken from memory. |
| `projection` | `disabled`, `dry_run`. `serve --dry-run` forces a dry run. |

The entry needs a `connection`. A route uses handler `slack`. Bindings may grant the operations `slack.context`, `slack.thread` and `slack.reply`.

`init slack` stores the tokens through the secret store (the keyring by default) and writes `keyring:` references. `env:` references work the same way.

```json
{"id": "slack", "type": "slack", "config": {},
 "secret_refs": {"bot_token": "env:OPEN_CONDUIT_SLACK_BOT_TOKEN", "app_token": "env:OPEN_CONDUIT_SLACK_APP_TOKEN"}}
```

## Identity and event families

One entry is one namespace. A thread's correlation key is `channel/<C>/thread/<thread ts>`. It is the Work's identity, so every later message of the thread reaches the same Work through its owned key, across any number of messages and restarts. Each message is the item `channel/<C>/thread/<T>/message/<ts>`. A top-level message is its own thread (`T == ts`), so every top-level message can start one. Slack ts values have a fixed width, so the lexical item order the runtime publishes puts a thread's parent before its replies.

| Family | Meaning | Recoverable |
| --- | --- | --- |
| `slack.message.human` | A person's message (top level or reply, including edits) | Yes |
| `slack.message.bot` | Another integration's message | Yes |
| `slack.message.agent` | A message carrying this app's marker, or the app's own unmarked message (`bot_user_id`) | Yes |
| `slack.message.deleted` | A deleted parent Slack keeps as a tombstone | Yes |
| `slack.message.removed` | A push-only deletion notice for a message Slack no longer shows | **No** (declared loss) |

Push and reconcile share one normalizer. The version hashes the whole normalized envelope: type, key, occurrence time (the edit time for an edit) and payload. The delivery ID is `item@version`. Equal external state from either transport therefore has one identity, and the canonical-identity check deduplicates overlap. An edit is a new version of the same item.

The payload deliberately omits reply counts and latest-reply times, so a new reply never changes its parent's observation. Joins, topic changes and other non-content subtypes are ignored by push and reconcile alike. Mentions are parsed from the text. File IDs and names are kept, but file content is never read.

## Push: Socket Mode (default) and the Events API

**Socket Mode.** [`socket.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/socket.go) uses `apps.connections.open` with the app token, then a minimal standard-library RFC 6455 client ([`websocket.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/websocket.go)). This adds no new module. The receiver:

- Hands each `events_api` envelope to the runtime's `Push`.
- **Acknowledges it only after durable acceptance.** An unaccepted envelope stays unacknowledged, Slack redelivers it (up to three retries) and reconcile recovers anything still missed.
- Answers pings, and reconnects on Slack's `disconnect` refresh, a dropped connection or idleness (3 minutes). Backoff runs from 1 s to 1 min. A rate-limited `apps.connections.open` waits out its `Retry-After`.
- Acknowledges and ignores other envelope types.

The TLS connection opened with the app token is the authentication boundary. Bootstrap starts one receiver per registered entry, and doctor's `slack` check reports whether it is connected, plus counts of acknowledged and unacknowledged envelopes.

**HTTP Events API** (`receiver: http`). The webhook listener mounts `POST /slack/<scope>/<entry>`, which is the app's Request URL. `VerifyDelivery` handles each request:

- It checks `X-Slack-Signature` (HMAC-SHA256 over `v0:<timestamp>:<body>`, in constant time; an empty secret never verifies) and rejects a timestamp more than 5 minutes from receipt (the replay guard). This happens before anything is parsed.
- It echoes `url_verification`.
- It answers 200 only after durable acceptance, 401 for a bad signature, 400 for a malformed body and 503 when the delivery was not accepted (Slack retries).
- It acknowledges `app_rate_limited` and other callbacks.

The listener never serves `/v1`. Socket Mode is the practical default.

`Push` makes no service calls and no coordination writes:

- A new message or `message_changed` is normalized as above.
- A `message_deleted` becomes the direct `slack.message.removed` Event (delivery `removed/<C>/<ts>`).
- Events from other teams or unwatched channels are ignored.

Retries of one callback share its `event_id`, and the message identity is its observed state, so redelivery deduplicates.

## Reconcile

Slack has no snapshot token. A generation reads a fixed window `[start − history_hours, start]`:

1. Every channel's top-level messages (`conversations.history`, newest first, with a cursor-stable pagination).
2. For each thread parent, the thread's replies up to `start` (`conversations.replies`, followed to its end, at most 50 pages of 200).
3. Then the threads that **live Work owns** whose parent is older than the window. The host supplies these from durable state (`IngressOptions.ActiveThreads`). A multi-day wait therefore still recovers missed replies after its parent leaves the window.

The cursor is `<channel>:h:<offset>:<slack cursor>` or `<channel>:a:<after ts>`. A page re-reads its Slack page deterministically (it is cached in memory within the generation) and resumes at the offset. A call stops after 20 API calls or 10 seconds. `Complete` certifies that every listing was read to its end in this generation's window. Rate limits (HTTP 429 with `Retry-After`, which pauses that method for this client) and failed reads return errors, so no absence is inferred.

A thread whose reply count and latest reply are unchanged is taken from memory for `refresh_minutes`, so a quiet channel costs about one history call per page. Memory is lost on restart (one full read follows). The same safety nets as GitHub restart a generation: one older than six hours, or a page re-requested after a successful answer.

Messages that disappear become generic `core.ingress.absent`, which the route ignores.

**Declared service limits and what cannot be re-derived.**

- **Push-only deletion detail.** A message deleted without a tombstone is announced only by push (`slack.message.removed`, declared non-re-derivable). Reconcile reports it as absence within the window, without the deletion time.
- **Reply edits.** An edit to a reply leaves the thread summary unchanged, so reconcile sees it within `refresh_minutes`, not at once. Push sees it immediately.
- **Old threads.** Threads older than `history_hours` and owned by no live Work are not reconciled. A reply there arrives only by push.
- **What is never observed.** Non-content subtypes, reactions and file contents.
- **Ordering.** Slack keeps an unacknowledged Socket Mode envelope for three retries. After that, only reconcile recovers the message, at the reconcile interval. Reconcile is not a total order: per-Work order is acceptance order, so pushed messages can precede older reconciled ones.
- **Rate limits.** `conversations.history` and `conversations.replies` are Tier 3 for internal apps. Slack's 2025 limits for commercially distributed non-Marketplace apps (one request per minute, 15 messages) would make reconcile far slower, but a workspace-internal app is not subject to them. The real limits are unmeasured: the one short real-Slack run hit no 429.
- **Retention.** History the workspace's retention or plan hides is unrecoverable.

## Route policy

Handler `slack` reads only the detached Event, Scope and Work, and its own route config. It finds its entry in the scope metadata by namespace. A route's config may list `channels`: the route then claims only threads in those channels, so one entry can serve several routes, each with its own binding and playbook ([example](../../workflows/slack/#start-a-different-playbook-per-channel)). Config validation rejects anything but distinct Slack channel IDs.

- **Unowned threads.** A human, non-deleted message creates `slack.conversation` Work. The kind is namespaced because B02 requires every Work kind to be. It must also pass the start policy: it is a top-level message (`all`), or it mentions the bot (`mention`), and it was received within `start_window_hours` of being written. A parent first seen in its edited form still starts the conversation, because its push may have been missed.
- **Existing conversations receive:** human messages (including edits) and replies marked by *another* Work.
- **Never delivered:**
  - The Work's own replies.
  - The app's unmarked messages.
  - Other bots' messages, tombstones, removals and absence.

  A reply therefore cannot start an event loop. Ingress Events are external roots, so provenance travels in the marker instead, and the core chain limits still bound every derived Event.

Completed Work keeps its thread key, and later messages are checkpointed against the terminal Work.

## Support execution and conversation lifecycle

The support binding reuses the Claude Code executor through the public contract, with its own investigation tools and no resource. `requires` has `workspace: false` and there is no Git provider. The session lives in the executor's private per-Work directory. The binding uses playbook [`support-conversation`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/playbooks/support-conversation.md) with args `{key}` (the thread key), and bootstrap writes that playbook into the Claude Code plugin beside the GitHub ones. `init slack` writes exactly this binding.

**Decision: wait while the conversation is open; complete only on resolution.** A turn replies once with `slack.reply`. It then calls the builtin `wait` on the thread: `{"match": {"types": ["slack.message.human"]}, "timeout_seconds": 604800}`. A type-only wait matches the Work's own thread key, and `slack.context` returns this input as `wait`. The attempt parks with the turn's status. A human message wakes it as an accepted Event (B04), and the same session resumes with the accumulated messages. When the seven-day timeout fires instead, the turn ends `done`: a handoff, so the Work stays `ready`, owns its thread, and the next message starts attempt N+1. Only a person's confirmation that the matter is resolved ends the turn `finished`, which completes the Work. Later messages in that thread are then audited as `terminal`, and a new top-level message starts a new conversation. The playbook says "when in doubt, wait". The cost of an open conversation is that its thread stays in reconcile's owned-thread set.

**Active messages.** The route's active policy is `inject`. A human message during a running turn is written to claude's stdin, echoed and proved consumed with the turn's result, including a waiting result. The parked wait is therefore not woken by what the turn already answered ([generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement)). A message that arrives after the turn stopped reading is not consumed: it wakes the wait or starts the next attempt. A binding on an executor without inject needs an explicit `unsupported_fallback` (`queue`). The shell executor supports inject through its inbox.

[`TestDaemonSlackSupportConversation`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_slack_support_test.go) drives this with a fake claude on the composed daemon over Socket Mode. The first turn replies and parks. A reply resumes the same session. A message mid-turn is injected once and consumed, with no wake and no extra segment. A restart mid-wait keeps the thread's Work, and the next reply resumes the same session. The run uses no workspace, `github` stays `not_configured`, and only fixtures are involved. The same sequence has also been run once on real Slack and real Claude (see [History](#history)). The multi-day wait is the fake-clock scenario under [Verification](#verification).

## Marked replies

Replies are not re-derivable. Each one has a stable intent ID, a durable KV record and a discoverable marker:

- **Outcome replies.** An outcome declaring `slack.reply.v1` `{text, broadcast?}` becomes the intent `outcome/<execution id>`, re-derived from durable outcomes on every pass.
- **Tool replies.** `slack.reply` takes `{text, broadcast?}` and creates the intent `call/<execution>/<request id>` (or a digest of the input). The record keeps the text, so the projector finishes a delivery whose caller is gone.

The record moves through these states:

1. `pending`
2. `attempted`, recorded before `chat.postMessage`
3. `posted`, or `failed` when Slack answered `ok:false`. Nothing was posted, so it is never retried blindly, and the tool reports `slack_refused`.

A rate limit or a credential failure returns the record to `pending`. A lost answer (a network error or 5xx) leaves it `attempted`. After 30 seconds the projector reads the thread and completes the record when it finds the marker, instead of posting again. A reused intent ID with different content is refused (`intent_conflict`).

The marker is message metadata: `event_type` `open_conduit_reply`, with payload `{work, execution, intent}` where intent is `i<digest>`. When Slack returns no metadata for the app's messages, the search falls back to the app's bot user posting exactly this text, and normalization still recognizes the app's own messages (`bot_user_id`) as agent messages that are never input. That fallback could confuse two identical replies of different intents in one thread. Links are never unfurled.

The projector runs at most every five seconds in the background. `slack.context` returns the thread's identity, and `slack.thread` reads the thread (normalized and classified). Both act only on the Work's own thread.

## Verification

Run these commands:

```sh
go test -race ./extensions/slack/...
go test -race -run 'TestSlack|TestDaemonSlack' ./integration
make check
GOTOOLCHAIN=go1.24.2 make check
make cross-build
make drill
```

**Unit tests:**

- The shared ingress conformance, using a separate one-shot deletion delivery (see [extensions](../extensions/)).
- Push/reconcile identity for parents, replies, bots, edits and tombstones; removals; ignored subtypes, foreign teams and foreign channels.
- 451 messages in one thread paginated under a 2-call budget.
- Thread caching and refresh, owned threads older than the window, a rate-limited scan that resumes, stale and foreign generations, and token rotation and revocation.
- Signature, replay window, URL verification and HTTP statuses; start and input policy; config; rendering.
- Socket Mode acknowledges only accepted envelopes, answers pings, and reconnects after a refresh or a drop; a bad app token backs off.
- The manifest covers every method used.

**SQLite integration** ([`slack_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/slack_test.go)), through the real runtime ingress and router:

- 301 messages in one thread (151 under `-race`): half pushed (including a redelivery and a forged delivery) and half reconciled. Each message is one distinct Event, there is one Work, and its input is ordered.
- Reconcile-only recovery, with the start window holding.
- Identity across a restart mid-generation, an edit and a removal.
- An ambiguous reply recovered by its marker, with provenance kept and never fed back as input.
- The multi-day scenario ([`slack_multiday_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/slack_multiday_test.go)), with a fake clock, for a resume-capable and a non-resumable executor. A turn parks on the playbook's seven-day wait. Three days later, after a restart, a reply missed by push, whose parent has left `history_hours`, is recovered by the owned-thread scan and wakes the same Work: the same execution resumes, or attempt N+1 starts with only the new message. The next wait times out after seven quiet days (`core.wait.timeout` continues the conversation), the turn hands back, and a message on day 12 still reaches the same Work.
- A real SIGKILL between Slack creating a reply and local completion, recovered without a duplicate.
- The reply tool: a retried call writes once, a lost answer is finished by the projector, and a refusal is not retried.
- A rate-limited scan infers no absence.
- Recovery without metadata.

**Composed daemon** ([`daemon_slack_test.go`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_slack_test.go), in `make drill`), with a workspace-free scope:

- Reconcile starts the conversation and the shell stand-in replies through `os call slack.reply`. A human reply over Socket Mode is acknowledged and resumes the same Work. The agent's own replies pushed back start nothing, and doctor is `slack: ok` with `github: not_configured`. A restart reconnects and posts nothing.
- The HTTP receiver echoes URL verification, refuses forgeries and accepts a signed callback.

## Limitations

- **Real Slack.** Real Slack has been exercised by one short run (see [History](#history)). Not covered on real Slack: a rate limit, `app_mention` duplication, the HTTP Events API, the `slack-cli` setup path, a reply missed by push and recovered by reconcile, a SIGKILL during an active turn, and a multi-day wait. The drills cover the missed reply and the SIGKILL, and the fake-clock scenario covers the multi-day wait in CI.
- **Registration lifetime.** As for GitHub, ingress registrations are startup-only. A changed entry reports `restart_required`.
- **Volume.** The projector reads every Work in its scope each pass, and the router's per-event cost grows with a Work's queued input. Both are adequate for a support channel, with no high-volume claim.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 04-01 delivered the Slack extension (ingress, Socket Mode and the Events API, reconcile, the route and marked replies) as the second workload. It added the separate one-shot deletion delivery to the shared ingress conformance.
- Ticket 04-02 delivered extension-owned setup (`init slack`), the manifest decision for `metadata.message:read`, the support binding and conversation lifecycle, and the generic inject acknowledgement. 04-01 and 04-02 used only fixtures and the `slacktest` fake.
- Ticket 04-03 added the fake-clock multi-day scenario (`slack_multiday_test.go`) and a short real-Slack run, below. A real multi-day wait was deferred to real-world operation by user decision.

### Real Slack run (04-03)

A short run on 2026-10-25 used a real workspace app created with `init slack` from the manifest (Socket Mode, tokens in the keyring) and the support binding on Claude Code with `model: haiku`:

- `doctor --check-secrets`: `slack_auth verified`, with the granted scopes equal to the manifest's (no `scopes_missing`).
- **Metadata.** With `metadata.message:read`, `conversations.replies` with `include_all_metadata=true` returns the marker's full payload `{work, execution, intent}`. Without `include_all_metadata`, only the `event_type` comes back. The case without the scope was not tested (it needs an install without it), so the text fallback remains fixture-proven.
- **One real thread.** The top-level message created one `slack.conversation` Work over Socket Mode. The marked reply came back as `slack.message.agent` and was never input. The first thread reply woke the wait and resumed the same execution and Claude session. The daemon was SIGKILLed mid-wait and restarted, and the next reply woke the same Work, execution and session. A message posted 14 s into that turn was routed `inject`, echoed (`injected {accepted 1, consumed 1}`) and answered in the same reply. It neither woke the next wait nor started an attempt. Attempt 1 was the only execution, across three turns.
- **No workspace or Git.** `github` stayed `not_configured`, with 0 resource leases.
- **Not observed.** No 429 was seen, so real rate limits are unmeasured. No mention was posted, so `app_mention` duplication was not seen. `--via slack-cli` was not run.
