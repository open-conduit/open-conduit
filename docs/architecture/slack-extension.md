# Slack ingress, conversations and marked replies

Ticket 04-01 delivers the first-party Slack extension in [`extensions/slack`](../../extensions/slack). It is the second workload (PRD workflow B) and is built only against the public contracts: [B03 ingress](extensions.md), [B04 routing](runtime-semantics.md) and [B02 storage/KV](domain-storage.md). It has no workspace, repository or tracker concept, and the runtime does not import it. [`internal/bootstrap`](../../internal/bootstrap/slack.go) registers it. Open Supervisor v1 has no Slack adapter, so there is no behavioral baseline: the PRD is the only reference.

All evidence so far uses fixtures and the [`slacktest`](../../extensions/slack/slacktest) fake (Web API, Socket Mode and Events API). No real Slack workspace was called. Real-Slack evidence is 04-03.

## The Slack app

[`manifest.go`](../../extensions/slack/manifest.go) is the single source of truth for the app the extension needs. `Manifest`, `ManifestJSON` and `CreateAppURL` produce it, and `TestManifestCoversWhatTheExtensionUses` drives every call the extension makes against the fake and checks that a manifest scope permits each one.

| Setting | Value |
| --- | --- |
| Socket Mode | On (the default receiver needs no public endpoint) |
| Bot user | Yes, always online |
| Bot token scopes | `channels:history`, `groups:history`, `channels:read`, `groups:read`, `chat:write`, `reactions:write`, `users:read`, `app_mentions:read` |
| Bot events | `message.channels`, `message.groups`, `app_mention` |
| App-level token | `connections:write` (Socket Mode only) |

The extension's own calls need only part of this:

- `conversations.history` and `conversations.replies` need `channels:history` (public channels) or `groups:history` (private channels).
- `chat.postMessage` needs `chat:write`.
- `auth.test` needs no scope.
- `apps.connections.open` uses the app-level token.

The setup channel picker will use `channels:read`/`groups:read`. `users:read`, `reactions:write` and `app_mentions:read` are granted for later use (names, acknowledgements); nothing in 04-01 depends on them. `app_mention` duplicates a `message` event, so push ignores it.

The extension relies on no scope outside this list. Reply markers use Slack **message metadata**. Posting metadata needs only `chat:write`. Reading it back uses `include_all_metadata` on history and replies. Whether Slack returns metadata to a token without `metadata.message:read` is not yet confirmed (04-03 checks it on the real app). The extension does not depend on it, as described under [marked replies](#marked-replies).

### Intended setup flow (04-02)

Setup belongs to the extension, not to core. 04-02 adds an optional, generic `extension.Setup` hook. Core `open-conduit init <type>` dispatches to the registered extensions that implement it and supplies only a setup host: prompts, opening a URL, the secret store (`secret set`), config merge and offline validation, and running verification. The existing GitHub init moves behind the same hook. Core CLI packages import no extension, and a boundary check enforces this.

Slack's `Setup` implementation will:

1. Have the host open `CreateAppURL(name)`, which is `https://api.slack.com/apps?new_app=1&manifest_json=<urlencoded manifest>`. An optional `slack-cli` path is offered only when the `slack` CLI is on PATH.
2. Ask the user to create the app-level token with `connections:write` (Slack has no API for this) and paste it with the bot token.
3. Store both through the host's secret store.
4. Verify them with `auth.test` and `apps.connections.open`.
5. List the channels the bot is in so the user can pick, then return the scope config for the host to merge and validate.

The extension will also own a doctor check that reports scopes missing compared with the manifest (`MissingScopes`). All of this is assigned to 04-02. Until then, write the config by hand as below.

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

For 04-03, the real values come from `OPEN_CONDUIT_SLACK_BOT_TOKEN`, `OPEN_CONDUIT_SLACK_APP_TOKEN`, `OPEN_CONDUIT_SLACK_SIGNING_SECRET` and `OPEN_CONDUIT_SLACK_TEST_CHANNEL`, referenced as `env:` secrets. 04-01 never read them.

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

**Socket Mode.** [`socket.go`](../../extensions/slack/socket.go) uses `apps.connections.open` with the app token, then a minimal standard-library RFC 6455 client ([`websocket.go`](../../extensions/slack/websocket.go)). This adds no new module. The receiver:

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

The listener never serves `/v1`. Because v1 holds the machine's only free ngrok endpoint, Socket Mode is the practical default.

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
- **Rate limits.** `conversations.history` and `conversations.replies` are Tier 3 for internal apps. Slack's 2025 limits for commercially distributed non-Marketplace apps (one request per minute, 15 messages) would make reconcile far slower, but a workspace-internal app is not subject to them. 04-03 confirms the real app's limits.
- **Retention.** History the workspace's retention or plan hides is unrecoverable.

## Route policy

Handler `slack` reads only the detached Event, Scope and Work. It finds its entry in the scope metadata by namespace.

- **Unowned threads.** A human, non-deleted message creates `slack.conversation` Work. This is the PRD's `conversation`, namespaced because B02 requires every Work kind to be. It must also pass the start policy: it is a top-level message (`all`), or it mentions the bot (`mention`), and it was received within `start_window_hours` of being written. A parent first seen in its edited form still starts the conversation, because its push may have been missed.
- **Existing conversations receive:** human messages (including edits) and replies marked by *another* Work.
- **Never delivered:**
  - The Work's own replies.
  - The app's unmarked messages.
  - Other bots' messages, tombstones, removals and absence.

  A reply therefore cannot start an event loop. Ingress Events are external roots, so provenance travels in the marker instead, and the core chain limits still bound every derived Event.

Completed Work keeps its thread key, and later messages are checkpointed against the terminal Work. A support binding should hand back or wait rather than complete while the conversation is open. That policy, the active-execution inject path and context accumulation are 04-02.

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

- The shared ingress conformance, using a separate one-shot deletion delivery (see the [extensions](extensions.md) 04-01 update).
- Push/reconcile identity for parents, replies, bots, edits and tombstones; removals; ignored subtypes, foreign teams and foreign channels.
- 451 messages in one thread paginated under a 2-call budget.
- Thread caching and refresh, owned threads older than the window, a rate-limited scan that resumes, stale and foreign generations, and token rotation and revocation.
- Signature, replay window, URL verification and HTTP statuses; start and input policy; config; rendering.
- Socket Mode acknowledges only accepted envelopes, answers pings, and reconnects after a refresh or a drop; a bad app token backs off.
- The manifest covers every method used.

**SQLite integration** ([`slack_test.go`](../../integration/slack_test.go)), through the real runtime ingress and router:

- 301 messages in one thread (151 under `-race`): half pushed (including a redelivery and a forged delivery) and half reconciled. Each message is one distinct Event, there is one Work, and its input is ordered.
- Reconcile-only recovery, with the start window holding.
- Identity across a restart mid-generation, an edit and a removal.
- An ambiguous reply recovered by its marker, with provenance kept and never fed back as input.
- A real SIGKILL between Slack creating a reply and local completion, recovered without a duplicate.
- The reply tool: a retried call writes once, a lost answer is finished by the projector, and a refusal is not retried.
- A rate-limited scan infers no absence.
- Recovery without metadata.

**Composed daemon** ([`daemon_slack_test.go`](../../integration/daemon_slack_test.go), in `make drill`), with a workspace-free scope:

- Reconcile starts the conversation and the shell stand-in replies through `os call slack.reply`. A human reply over Socket Mode is acknowledged and resumes the same Work. The agent's own replies pushed back start nothing, and doctor is `slack: ok` with `github: not_configured`. A restart reconnects and posts nothing.
- The HTTP receiver echoes URL verification, refuses forgeries and accepts a signed callback.

## Limitations and follow-on work

- **Real Slack (04-03).** No real Slack API, Socket Mode connection or Events API delivery has been exercised. Metadata visibility, real rate limits and `app_mention` duplication are checked on the real app.
- **Support execution and setup (04-02).** Executor binding, inject acknowledgement, the generic extension `Setup` hook with Slack's setup on it, and the extension-owned doctor scope comparison.
- **Registration lifetime.** As for GitHub, ingress registrations are startup-only. A changed entry reports `restart_required`.
- **Volume.** The projector reads every Work in its scope each pass, and the router's per-event cost grows with a Work's queued input. Both are adequate for a support channel, with no high-volume claim.
