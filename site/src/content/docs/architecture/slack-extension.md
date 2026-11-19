---
title: "Slack ingress, conversations and marked replies"
---

The first-party Slack extension in [`extensions/slack`](https://github.com/open-conduit/open-conduit/tree/main/extensions/slack) turns Slack threads into support conversations: a person's message starts a `slack.conversation` Work, an agent replies in the thread with marked replies, and later messages in the thread reach the same Work. It is built only against the public contracts: [B03 ingress](../extensions/), [B04 routing](../runtime-semantics/) and [B02 storage/KV](../domain-storage/). It has no workspace, repository or tracker concept, and the runtime does not import it. It is written against the public [`extension/inproc`](https://github.com/open-conduit/open-conduit/blob/main/extension/inproc/inproc.go) contract (`extensions/slack/extension.go`) and runs out of process as a package bundled with the release: `extensions/slack/pkg` serves it unchanged through `protocol/sdk/inprocsdk` as `open-conduit extension-run slack`.

This page records two decisions: the manifest requests `metadata.message:read` for reply markers, and a support conversation waits while it is open and completes only on resolution. The tests use fixtures and the [`slacktest`](https://github.com/open-conduit/open-conduit/tree/main/extensions/slack/slacktest) fake (Web API, Socket Mode and Events API); one short run on real Slack is recorded under [History](#history).

## The Slack app

[`manifest.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/manifest.go) is the single source of truth for the app the extension needs. `Manifest`, `ManifestJSON` and `CreateAppURL` produce it, and `TestManifestCoversWhatTheExtensionUses` drives every call the extension makes against the fake and checks that a manifest scope permits each one.

| Setting | Value |
| --- | --- |
| Socket Mode | On (the default receiver needs no public endpoint) |
| Interactivity | On, over Socket Mode, so it needs no request URL. It carries the button presses of [prompts](#prompts-and-button-answers) |
| Bot user | Yes, always online |
| App Home | The messages tab on and writable, so people can send the app a [direct message](#direct-messages) |
| Bot token scopes | `channels:history`, `groups:history`, `channels:read`, `groups:read`, `chat:write`, `chat:write.customize`, `metadata.message:read`, `reactions:write`, `users:read`, `app_mentions:read`, `files:write`, `im:history`, `im:read` |
| Bot events | `message.channels`, `message.groups`, `message.im`, `app_mention` |
| App-level token | `connections:write` (Socket Mode only) |

The extension's own calls need only part of this:

- `conversations.history` and `conversations.replies` need `channels:history` (public channels), `groups:history` (private channels) or `im:history` (direct messages).
- `chat.postMessage` and `chat.postEphemeral` need `chat:write`. A message posted under a binding's own name and icon ([identities](#binding-identities)) also needs `chat:write.customize`.
- `files.getUploadURLExternal` and `files.completeUploadExternal` ([uploads](#uploads)) need `files:write`.
- `auth.test` needs no scope.
- `users.conversations` (the setup channel picker) needs `channels:read`/`groups:read`; listing the app's direct messages for reconcile needs `im:read`.
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
5. **Asks for a folder of instructions and knowledge.** Optional: Enter (or no interactive input) means none, and a path that isn't an existing directory is asked again. A relative path is taken from the working directory.
6. **Stores the tokens and returns the scope.** Both tokens go through the host's secret store (`<connection>-bot-token`, `<connection>-app-token`; only references reach the config). Setup returns the `slack` connection and a workspace-free scope:
   - an `agent` binding on `claude-code` (`requires` toolbelt mcp, inject, resume, `workspace: false`, no `resource`), with playbook `slack-thread`, args `{key}`, `dir` when a folder was given, and the operations `read_context`, `wait`, `declare_outcome`, `slack.context`, `slack.thread` and `slack.reply`;
   - the `conversations` route (handler `slack`, active policy `inject`);
   - the entry (`namespace` `slack/<team>`, the channel, `bot_user_id`, `receiver: socket`, `start` from `--start`).

With `--bot-token REF --app-token REF --team T… --channel C…`, setup prompts for nothing and verifies nothing (it never resolves references); it says to run `doctor --check-secrets` instead. `--dir FOLDER` gives the folder without the prompt; one that isn't an existing directory is a usage error. `--scope` (default `slack`) and `--connection` (default `slack`) name the result. `--api-url` points at another API base (the tests use the fake).

The `slack-cli` path is unverified against a real Slack CLI. The manifest URL path and the token checks have been run against real Slack.

**Doctor.** `slack-auth` (`--check-secrets`) runs the extension-owned `slack.CheckAuth`. It checks that the token's team is the entry's team and its user is the entry's bot user. It also compares the granted scopes with the manifest (`MissingScopes`). A missing `chat:write`, or both history scopes missing, fails with `scopes_missing`. Any other missing scope warns with `scopes_missing` and says what it costs (for `metadata.message:read`: markers found only by the text fallback; for `files:write`: `slack.upload` is refused). When the entry turns `uploads` on and the token lacks `files:write`, the missing scope fails instead, since every upload would be refused. When the entry sets `identities` and the token lacks `chat:write.customize`, or doesn't report its scopes, the check fails with `identity_scope_missing`, because those bindings' messages are refused. The extension maps its severity onto the check it serves.

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
| `channels` | 1–100 channel IDs (`C…` public, `G…`/`C…` private) to observe. May be empty when `direct_messages` is set. A DM (`D…`) is never listed here. |
| `direct_messages` | Turns on [direct messages](#direct-messages) with the app: `{"users": ["U…", …]}` (at most 1000) or `{"anyone": true}`. Exactly one is required, so turning DMs on always says whom they admit. Unset: DMs are not observed. |
| `receiver` | `socket` (default), `http` or `none` (reconcile only). |
| `signing_secret` | Names a selected scope secret. Required for `http`. |
| `start` | `all` (default): every human top-level message starts a conversation. `mention`: a human message mentioning `bot_user_id` in a thread no Work owns starts one. |
| `bot_user_id` | The app's bot user. Required for `mention`; also recognizes the app's own unmarked messages. |
| `start_window_hours` | Default 24. A message starts a conversation only if it was first received within this time of being written, so the first reconcile does not turn history into Work. |
| `claim_from` | The entry's start point: which threads no Work owns it may claim. `registration` (default): only threads begun at or after the time this daemon first registered the channel for the entry. `all`: threads of any age reconcile reads. An RFC 3339 time (`2026-11-13T00:00:00Z`): threads begun from then. See [start points](#start-points). |
| `history_hours` | Default 336 (14 days), 1–2160. The reconcile window. |
| `refresh_minutes` | Default 60. How long an unchanged thread's replies are taken from memory. |
| `projection` | `disabled`, `dry_run` and `format` (`markdown`, the default, or `mrkdwn`; see [message layout](#message-layout)). `serve --dry-run` forces a dry run. |
| `post` | Which bindings may post a new message (`slack.post`) and where: `[{"binding": "nightly", "channels": ["C0123"]}]`. Each channel must be one of `channels`, so the new thread's replies are observed. An entry's `follow_up` (`{"grants": ["…"]}`, or `{}` for none) lets a person's reply to that binding's post open a successor once the posting Work has finished, with only those binding grants. Without it such a reply starts nothing. Empty: no binding may post. |
| `progress` | `{"enabled": true, "stop": ["slack:U…", …]}` keeps a [progress message](#progress-messages) for each run in a conversation's thread. `stop` (at most 1000, each `slack:<user ID>`, the form a gate's responders take) lists who may press its **Stop** button; empty shows none, and `stop` without `enabled` is refused. Unset: no progress message, as before. |
| `feedback` | `{"enabled": true}` puts Slack's [feedback buttons](#feedback-buttons) below replies; `"bindings": ["…"]` narrows them to those bindings' replies, and `bindings` without `enabled` is refused. Unset: no buttons, as before. |
| `child_requests` | `true` shows the requests of a conversation's child Work, at any depth, in the conversation's thread: as tasks of its [progress message](#progress-messages), and as [prompts](#child-requests) when a Slack person may answer them. Unset: nothing of a child's is shown, as before. |
| `request_messages` | `{"enabled": true}` sends each Slack person a request names as a responder a [direct message](#request-messages) with the request and its buttons, for any Work in the scope, and changes it to how the request ended once it settles. Unset: nothing is sent, as before. |
| `identities` | The name and icon each binding's messages post under: `[{"binding": "engineer", "username": "Steve", "icon_emoji": ":wrench:"}]`, with `icon_url` (https) instead of `icon_emoji` if you like. Each entry sets a `username`, an icon or both. Needs `chat:write.customize`. A binding with no entry posts as the app. See [binding identities](#binding-identities). |
| `uploads` | Turns [uploads](#uploads) (`slack.upload`) on: `{"max_bytes": 5242880, "media_types": ["image/png", "image/*"]}`. `max_bytes` defaults to, and may not exceed, 10 MB: what one call carries to the extension, which runs out of process, once base64 has grown it by a third. `media_types` is required: each is exact, a type's every subtype (`image/*`) or anything (`*/*`). Needs `files:write`. Unset: every upload is refused. |

The entry needs a `connection`. A route uses handler `slack`. Bindings may grant the operations `slack.context`, `slack.thread`, `slack.reply`, `slack.post` and `slack.upload`.

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
| `slack.message.agent` | A message carrying this app's marker, or the app's own unmarked message (`bot_user_id`). A message posted under a binding's own name is the app's by its marker, never by its name | Yes |
| `slack.message.deleted` | A deleted parent Slack keeps as a tombstone | Yes |
| `slack.message.removed` | A push-only deletion notice for a message Slack no longer shows | **No** (declared loss) |
| `slack.thread.opened` | A message a Work posted with `slack.post`; its thread is attached to that Work. The projector accepts it when the post is made; sealed, so the builtin `emit` can't | **No** (declared loss) |
| `slack.feedback` | A press of the [feedback buttons](#feedback-buttons) below a reply, on the Work that posted it. The projector accepts it when the press arrives; sealed | **No** (declared loss) |

Push and reconcile share one normalizer. The version hashes the whole normalized envelope: type, key, occurrence time (the edit time for an edit) and payload. The delivery ID is `item@version`. Equal external state from either transport therefore has one identity, and the canonical-identity check deduplicates overlap. An edit is a new version of the same item.

The payload deliberately omits reply counts and latest-reply times, so a new reply never changes its parent's observation. Joins, topic changes and other non-content subtypes are ignored by push and reconcile alike. Mentions are parsed from the text. File IDs and names are kept, but file content is never read.

## Direct messages

With `direct_messages` set, a direct message with the app (an `im` conversation, ID `D…`) is a place where Work starts, alongside the watched channels.

- **One conversation is a top-level message and its thread, as in a channel.** The DM's ID is the thread key's channel (`channel/D…/thread/<ts>`), so identity, reply markers, `slack.reply`, `slack.thread`, `slack.context`, the durable wait and the `human.ask` buttons work in a DM exactly as in a channel thread. Each new top-level message in the DM is a new conversation, which keeps Work short; a reply in its thread is that Work's input. Answering in the thread is what keeps a follow-up with its conversation: a person's new top-level message starts another one.
- **`start` doesn't apply.** Nobody mentions the app in a DM, so every person's top-level message starts a conversation, within `start_window_hours`, whatever `start` says.
- **Who may start is checked at the host.** The route admits a DM only from a user `direct_messages.users` lists, or from anyone with `anyone`. The limit is on the entry, not on a route, so no route can take a DM another route's limit refused: every `slack` route applies it. A DM it refuses starts nothing, and the route trace records the reason (`user U… is not one direct_messages admits`). Slack's events don't say whether the sender is a guest, so "members only" isn't offered: list the users.
- **A route can select DMs.** A route config `{"direct_messages": true}` claims DMs (with `channels` as well, both). A route with `channels` and no `direct_messages` claims no DMs, and a route with neither claims everything the entry observes, so a DM can run on its own binding and playbook.
- **Push** takes `message.im` events for any `D…` conversation. **Reconcile** reads the watched channels, then the DMs that can matter: each DM with a person `direct_messages` admits (from `users.conversations`, `types=im`, listed once per generation), and each DM live Work owns a thread in, whoever it is with. It reads their history and threads as it reads a channel, followed by their owned threads older than the window. A DM with someone the limit refuses can't start Work, so it isn't read, and reconcile costs about one history call per admitted person's DM, not per person who ever wrote to the app. With `anyone`, that is every DM, so prefer `users` in a large workspace. A cursor names a DM by its ID rather than its position in the listing, so a DM opened mid-generation cannot shift it.
- **The listing is bounded** at 50 pages of 200. A longer one keeps what it read instead of failing the generation (Slack has no snapshot, and the runtime publishes a generation only once it is complete, so an error here would also stop recovery for the watched channels). The channels, the DMs listed and the DMs live Work owns are still reconciled. A DM past the bound is recovered only by push, and messages there that an earlier generation observed are reported absent, as for messages that leave the window.
- **Doctor** fails an entry that turns DMs on when the token lacks `im:history` or `im:read`, saying DMs would be neither read nor recovered. An entry without DMs doesn't need them. Replies in a DM use `chat.postMessage` (`chat:write`), so the manifest asks for no `im:write`: the app answers DMs people open, and never opens one.

## Push: Socket Mode (default) and the Events API

**Socket Mode.** [`socket.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/socket.go) uses `apps.connections.open` with the app token, then a minimal standard-library RFC 6455 client ([`websocket.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/websocket.go)). This adds no new module. The receiver:

- Hands each `events_api` envelope to the runtime's `Push`.
- **Acknowledges it only after durable acceptance.** An unaccepted envelope stays unacknowledged, Slack redelivers it (up to three retries) and reconcile recovers anything still missed.
- Answers pings, and reconnects on Slack's `disconnect` refresh, a dropped connection or idleness (3 minutes). Backoff runs from 1 s to 1 min. A rate-limited `apps.connections.open` waits out its `Retry-After`.
- Acknowledges and ignores other envelope types.

The TLS connection opened with the app token is the authentication boundary. The extension starts one receiver per registered entry and stops them when the daemon stops, and doctor's `slack` check reports whether it is connected, plus counts of acknowledged and unacknowledged envelopes.

**HTTP Events API** (`receiver: http`). The webhook listener mounts `POST /slack/<scope>/<entry>`, which is the app's Request URL. `VerifyDelivery` handles each request:

- It checks `X-Slack-Signature` (HMAC-SHA256 over `v0:<timestamp>:<body>`, in constant time; an empty secret never verifies) and rejects a timestamp more than 5 minutes from receipt (the replay guard). This happens before anything is parsed.
- It echoes `url_verification`.
- It answers 200 only after durable acceptance, 401 for a bad signature, 400 for a malformed body and 503 when the delivery was not accepted (Slack retries).
- It acknowledges `app_rate_limited` and other callbacks.

The listener never serves `/v1`. Socket Mode is the practical default.

`Push` makes no service calls and no coordination writes:

- A new message or `message_changed` is normalized as above.
- A `message_deleted` becomes the direct `slack.message.removed` Event (delivery `removed/<C>/<ts>`).
- Events from other teams, unwatched channels, or DMs when `direct_messages` is unset are ignored.

Retries of one callback share its `event_id`, and the message identity is its observed state, so redelivery deduplicates.

## Reconcile

Slack has no snapshot token. A generation reads a fixed window `[start − history_hours, start]`:

1. Every channel's top-level messages (`conversations.history`, newest first, with a cursor-stable pagination).
2. For each thread parent, the thread's replies up to `start` (`conversations.replies`, followed to its end, at most 50 pages of 200).
3. Then the threads that **live Work owns** whose parent is older than the window. The host supplies these from durable state (`IngressOptions.ActiveThreads`). A multi-day wait therefore still recovers missed replies after its parent leaves the window.

The cursor is `<channel>:h:<offset>:<slack cursor>` or `<channel>:a:<after ts>`, where `<channel>` is a watched channel's index or a DM's ID. A page re-reads its Slack page deterministically (it is cached in memory within the generation) and resumes at the offset. A call stops after 20 API calls or 10 seconds. `Complete` certifies that every listing was read to its end in this generation's window. Rate limits (HTTP 429 with `Retry-After`, which pauses that method for this client) and failed reads return errors, so no absence is inferred.

A thread whose reply count and latest reply are unchanged is taken from memory for `refresh_minutes`, so a quiet channel costs about one history call per page. Memory is lost on restart (one full read follows). The same safety nets as GitHub restart a generation: one older than six hours, or a page re-requested after a successful answer.

Messages that disappear become generic `core.ingress.absent`, which the route ignores.

While it reads a page, reconcile also finds the threads the entry will not claim (see [start points](#start-points)). Of the page's threads that began before the channel's start point and hold a message the start policy asks for, and of those holding a reply marked by a Work, it asks the host which ones no Work here holds the thread's key for (`IngressOptions.Unowned`, a read of durable state). It hands the unowned marked ones, with the Work IDs their markers name, to `IngressOptions.Foreign`, which durably records those naming Work unknown here before the page is returned, so the route already refuses them when it sees the page's Events. Each complete generation's counts per channel are what doctor reports.

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

- **Unowned threads.** A human, non-deleted message creates `slack.conversation` Work. The kind is namespaced because B02 requires every Work kind to be. It must also pass the start policy: it is a top-level message (`all`), or it mentions the bot (`mention`), and it was received within `start_window_hours` of being written. In a [direct message](#direct-messages) it is a top-level message from a person `direct_messages` admits. Its thread must also have begun at or after the channel's [start point](#start-points) and hold no reply marked by Work this daemon has no record of. A parent first seen in its edited form still starts the conversation, because its push may have been missed.
- **Existing conversations receive:** human messages (including edits) and replies marked by *another* Work.
- **Never delivered:**
  - The Work's own replies.
  - The app's unmarked messages.
  - Other bots' messages, tombstones, removals and absence.

  A reply therefore cannot start an event loop. Ingress Events are external roots, so provenance travels in the marker instead, and the core chain limits still bound every derived Event.

Completed Work keeps its thread key, and later messages are checkpointed against the terminal Work.

### Start points

A new daemon state, a new entry, or a channel newly added to an entry would otherwise claim every thread reconcile finds in `history_hours`, and `start_window_hours` admits a day of it: on a move to a new host, every recent thread with a mention got a turn hours later, and an agent that read an earlier daemon's reply as its own corrected it. Two rules stop that, and both apply only to a thread no Work here owns. Work that owns a thread takes its input as before, however old the thread is.

- **A thread is claimed only from its channel's start point.** The thread's first message, not the message at hand, must be at or after it, so a new mention in an old thread starts nothing either. With `claim_from` `registration` (the default) the start point is the time this daemon first registered the channel for the entry. The extension records it in its KV namespace `slack.claims` (`start/<namespace>/<channel>`) when it registers the entry, before the source ingests anything, and never moves it: a restart or a config change keeps it, a channel added later gets its own, and since KV is in the coordination database, a backup bundle carries it and a restore keeps it. Direct messages share one start point (`start/<namespace>/direct`), recorded when the entry first registers with `direct_messages` on, since no direct message is known before one arrives. Every channel's start point is recorded whatever `claim_from` says, so switching back to `registration` keeps the first one. A channel that Work here already holds a thread of when its start point is first recorded (an entry upgraded from before start points) gets the earliest such Work's creation time instead, so a mention missed during the upgrade is still claimed. `all` claims any age; a time claims from then.
- **A thread with a reply marked by Work this daemon has no record of is never claimed.** Such a reply shows that a daemon owned the thread before and its state is not here. A marker names the Work that wrote the reply, so the thread is recorded only when no Work here holds it and a marker names Work this daemon has no record of in any namespace. Another entry's reply in a shared channel names Work that exists here, so it records nothing. Reconcile records the thread (`foreign/<namespace>/<channel>/<thread ts>`) before the page with it is routed, so the thread's first message does not start Work, and the record stays.

The route reads both from the extension's in-memory copy of these records (`StartBook`), loaded at registration and updated as each record commits. It takes no transaction, and each record is written once before any Event it bears on is accepted, so a route evaluated again decides the same way. A channel with no recorded start point is not registered yet. That is the case when a route trace evaluates a config that has not been applied. The route answers for a thread begun after the channel registers and says so in its reason. No real Event reaches the route in that state: the extension records a channel's start point before it registers the source that ingests the channel, and an entry that can't record it (`slack_state_unavailable`) never registers its source, so ingestion fails closed. Each refusal is the route's reason in the route trace ("the thread began before channel C…'s start point …").

Doctor's `slack` check says from when each entry claims threads. For each channel, it also counts the threads with marked replies and no Work here (a warning, `state_not_moved`, with the remedy: restore a backup bundle), and the threads the last complete reconcile found begun before the start point that asked for a conversation and are not claimed. The extension logs each newly recorded marked thread.

A reconcile on a daemon that keeps its state still recovers a message missed in a short outage: its thread began after the start point, so the start window alone decides, as before.

## Posting from other Work

Work that did not start in Slack (a scheduled job, a webhook's Work) can post to a channel with `slack.post {channel, text}`, and the thread it opens becomes the Work's, so the agent that wrote the post answers the replies to it.

- **Authority at the host.** The operation reads the caller's binding from its stored Execution, never from the input, and refuses a channel the entry's `post` does not list for that binding (`channel_not_allowed`). A prompt therefore can't make an agent post wherever the bot is a member.
- **Once.** The post is a durable intent marked like a reply. With a request ID its intent is `post/<work>/<request id>`, so a retried call, in this turn or a retried one, posts it once; without one it is a digest of the input within the attempt. A lost answer is found again in the channel's history by its marker. A post not yet confirmed answers `pending`, as a reply does, and the projector finishes it; the agent needn't call again.
- **The thread becomes the Work's.** The transaction that records the post as made, whether the call itself or a later projector pass confirms it, also accepts `slack.thread.opened` `{namespace, team, channel, ts}` for the posting Work, derived from its attempt's trigger. Its delivery ID is the thread's, so it is accepted once. The `slack` route answers it with the thread's key and `deliver: none` ([routing](../routing-lifecycle/)): the Work owns the thread, and the attachment is in its history but is not input, so it doesn't give the job another turn. The scope needs a `slack` route for this, whatever binding it names. The family is sealed, so an executor can't claim a thread by emitting the Event itself with the builtin `emit`, and the route attaches only threads in a channel `post` lists.
- **Replies reach the agent that posted.** A person's message in a thread owned by Work that is not `slack.conversation`, in a thread that Work holds a key for, is that Work's input on the binding it already runs on (`deliver: current`), not the route's binding and its `slack-thread` playbook, and every `slack` route takes it whatever `channels` its config lists. `slack.reply`, `slack.thread`, `slack.context` and the `human.ask` prompts then act on the posted thread.
- **The thread says who posted it.** The intent records the binding the posting attempt ran on, from its stored Execution. `slack.thread.opened` carries it, and the route attaches `<thread key>/posted/<binding>` beside the thread's key when that binding may post in the channel. No message carries that key. It is what a reply after the Work finishes is checked against.
- **Questions after the Work has finished, when the operator opts in.** A person's new message in the thread once the posting Work has finished opens its successor (`RouteDecision.Successor`, [runtime semantics](../runtime-semantics/), rule 9) only when the `post` entry of the binding that posted it has `follow_up`. The check uses the configuration in force when the message is routed. Without `follow_up`, the message is `terminal`, as for any finished Work. Being allowed to post doesn't let people in the channel start the binding, and the check is the host's route, not the job's playbook, so it fails closed. The successor is Work of the same kind and context, whatever extension made the finished Work (a job's, a webhook delivery's): the host lets a route ask for a successor of the finished Work's own kind though the Slack extension doesn't declare it. When no successor opens, the reply's route record (`terminal_work`) lists why under its refusals, in the Slack route's words: no binding named in the thread's keys, a post config without `follow_up`, an edit. It has only the grants `follow_up` names, never the finished run's, so a question can't use what the operator gave the scheduled run (GitHub write, say). Its ID is `<work>/g2` (then `/g3` and on), and its generation key is `<thread key>/posted/<binding>/generation/2`. The thread's own key reaches it through the finished Work. The Slack tools and reconcile read a posted or generation key as the thread it names. The message is the successor's first input, delivered `current`: a successor that hasn't run takes its predecessor's binding, so the job's binding and playbook answer, not the route's. Once it finishes, the next question opens the generation after it. An edit of an earlier reply, another Work's reply and a deletion open nothing and stay `terminal`, and so does any message in a finished `slack.conversation`'s thread: a conversation ends when a person says it is resolved.
- **The post is the handoff.** The successor is a new session, so it learns what was asked about from the thread: `slack.thread` returns the post as the thread's first message, marked `agent on <work>`, followed by the replies. The thread holds what the person actually replied to, and it stays current when the post is edited. The finished Work's outcome summary is not handed on: it is a note about the job's turn, not about the post. A successor's turn starts from the binding's playbook like any other Work's, so the job's playbook should say what to do with a reply in its thread (read it, answer with `slack.reply`, finish). The playbook only helps: the grants `follow_up` names are what bound the successor. The post's own message is the app's, which is never input.

`TestSlackPostOpensAThreadTheJobOwns` covers it through the real toolbelt and router: a refused channel, a retried post written once, a refused forgery through the builtin `emit`, the attachment recorded as `attach` with no input, a person's reply queued on the job's binding without starting a conversation, and a post whose answer was lost, finished by the projector after the turn, whose thread still becomes the job's. With `follow_up` on the job's binding, the thread holds the key naming it. Once the job has finished, a question opens its successor on the job's binding with no grants, which reads the post with `slack.thread` and answers in the thread; a question while it is live is its input, and one after it has finished opens the third generation. `TestDaemonSlackReplyToAFinishedDeliveryOpensItsSuccessor` runs the same reply through the composed daemon, where the Slack extension is a separate process: a webhook delivery's Work posts its report and completes in one turn, in a scope whose `webhook` and `work` routes come before its `slack` route and whose post config sets `follow_up: {}`, and a person's reply opens the delivery's successor on the binding that posted, which answers in the thread.

## Thread execution and conversation lifecycle

The binding `init slack` writes reuses the Claude Code executor through the public contract, with no resource. `requires` has `workspace: false` and there is no Git provider. The session lives in the executor's private per-Work directory, or in the binding's `dir`. The binding uses playbook [`slack-thread`](https://github.com/open-conduit/open-conduit/blob/main/extensions/slack/playbooks/slack-thread.md) with args `{key}` (the thread key), and bootstrap writes that playbook into the Claude Code plugin beside the GitHub ones.

**Decision: the playbook covers the thread, not its purpose.** `slack-thread` says how to work a thread: read it, reply once with `slack.reply`, wait on it, ask a person, end the turn. It doesn't say what the thread is for (support, reviews, anything else) or what the agent may do about it. That comes from the binding's `dir`, its `CLAUDE.md` or the project's rules, because the operator binds behaviour to a scope ([the core](../core/)). A scope with a git resource gets a worktree under the same playbook. The playbook was called `support-conversation` until issue #193, and it is still shipped under that name as an alias for one release, so existing bindings keep working.

**Decision: wait while the conversation is open; complete only on resolution.** A turn replies once with `slack.reply`. It then calls the builtin `wait` on the thread: `{"match": {"types": ["slack.message.human"]}, "timeout_seconds": 604800}`. A type-only wait matches the Work's own thread key, and `slack.context` returns this input as `wait`. The attempt parks with the turn's status. A human message wakes it as an accepted Event (B04), and the same session resumes with the accumulated messages. When the seven-day timeout fires instead, the turn ends `done`: a handoff, so the Work stays `ready`, owns its thread, and the next message starts attempt N+1. Only a person's confirmation that the matter is resolved ends the turn `finished`, which completes the Work. Later messages in that thread are then audited as `terminal`, and a new top-level message starts a new conversation. The playbook says "when in doubt, wait". The cost of an open conversation is that its thread stays in reconcile's owned-thread set.

**Active messages.** The route's active policy is `inject`. A human message during a running turn is written to claude's stdin, echoed and proved consumed with the turn's result, including a waiting result. The parked wait is therefore not woken by what the turn already answered ([generic inject acknowledgement](../runtime-semantics/#generic-inject-acknowledgement)). A message that arrives after the turn stopped reading is not consumed: it wakes the wait or starts the next attempt. A binding on an executor without inject needs an explicit `unsupported_fallback` (`queue`). The shell executor supports inject through its inbox.

[`TestDaemonSlackSupportConversation`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_slack_support_test.go) drives this with a fake claude on the composed daemon over Socket Mode. The first turn replies and parks. A reply resumes the same session. A message mid-turn is injected once and consumed, with no wake and no extra segment. A restart mid-wait keeps the thread's Work, and the next reply resumes the same session. The run uses no workspace, `github` stays `not_configured`, and only fixtures are involved. The same sequence has also been run once on real Slack and real Claude (see [History](#history)). The multi-day wait is the fake-clock scenario under [Verification](#verification).

## Binding identities

One app can serve many agents: a front agent in a thread, the specialists it hands work to, scheduled jobs posting reports. The entry's `identities` give a binding's messages its own name and icon, so people see which agent wrote each one without the agent saying so in the text.

- **What carries it.** `slack.reply`, `slack.post`, outcome replies and the [prompts](#prompts-and-button-answers) of Work on the binding post with `chat.postMessage` `username` and `icon_emoji` or `icon_url`. A prompt takes the binding of the Work's latest attempt. A binding without an identity posts as the app, as before.
- **Authority at the host.** The binding is read from the writing attempt's stored Execution and recorded on the intent, never taken from the call's input, so a prompt can't make an agent post as a different agent. A re-driven intent keeps the binding it was recorded with.
- **Refused, not shown as the app.** Slack posts under the app's name when the token lacks `chat:write.customize`. So before posting with an identity the projector checks the token's granted scopes (`auth.test`, kept for ten minutes). If the scope is missing, or the scopes aren't reported, nothing is posted. A tool call (`slack.reply`, `slack.post`) ends its intent `failed` with `identity_scope_missing`, and the refusal names the scope and asks the agent to report it, so the agent decides what to do next. An outcome reply or a prompt has nobody waiting for a refusal, so it stays `pending` and each projector pass reports `reply:identity_scope_missing`. It is posted under the binding's name once the scope is granted and the cached scopes are read again. Doctor fails the entry for the same reason.
- **Still the app's own.** Slack shows such a message as a `bot_message` with the app's bot ID and the name, without the bot user. The marker identifies it as `slack.message.agent`, so it wakes no Work and reconcile doesn't take it for a person's message. When Slack withholds metadata, the marker search's fallback matches the app's bot ID (from `auth.test`) as well as its bot user, so a lost post is found again rather than repeated. The name is display only, and it never identifies the author.

`TestSlackBindingsPostUnderTheirOwnIdentity` and `TestSlackIdentityWithoutScopeIsRefused` cover this against the fake, which posts as Slack does with the scope and silently under the app's name without it.

## Marked replies

Replies are not re-derivable. Each one has a stable intent ID, a durable KV record and a discoverable marker:

- **Outcome replies.** An outcome declaring `slack.reply.v1` `{text, broadcast?}` becomes the intent `outcome/<execution id>`, re-derived from durable outcomes on every pass.
- **Tool replies.** `slack.reply` takes `{text, blocks?, broadcast?}` and creates the intent `call/<execution>/<request id>` (or a digest of the input). The record keeps the text and blocks, so the projector finishes a delivery whose caller is gone. The blocks are part of the intent's identity: a reused intent ID with other blocks is refused like other text.

The record moves through these states:

1. `pending`
2. `attempted`, recorded before `chat.postMessage`
3. `posted`, or `failed` when Slack answered `ok:false`. Nothing was posted, so it is never retried blindly, and the tool reports `slack_refused` with what Slack said about it (its `response_metadata` messages, such as which block field it refused, made printable and cut to five messages of 300 bytes).

A rate limit or a credential failure returns the record to `pending`. A lost answer (a network error or 5xx) leaves it `attempted`. After 30 seconds the projector reads the thread and completes the record when it finds the marker, instead of posting again. A reused intent ID with different content is refused (`intent_conflict`).

The marker is message metadata: `event_type` `open_conduit_reply`, with payload `{work, execution, intent}` where intent is `i<digest>`. When Slack returns no metadata for the app's messages, the search falls back to the app's bot user, or its bot ID, posting exactly this text, and normalization still recognizes the app's own messages (`bot_user_id`) as agent messages that are never input. That fallback could confuse two identical replies of different intents in one thread. Links are never unfurled.

The projector runs at most every five seconds in the background. `slack.context` returns the thread's identity, and `slack.thread` reads the thread (normalized and classified). Both act only on the Work's own thread.

## Uploads

`slack.upload` puts a file into the Work's own thread, such as a screenshot an agent took with a browser. Its input is `{content_base64, name?, title?}`; the agent passes `file_path` and the `os` client reads the file from its directory into `content_base64` and `name`, as for `github.attachment.upload`.

- **Limits at the host.** The entry's `uploads` sets the largest file and the media types accepted, and the host checks both before anything is sent to Slack: `too_large`, `media_type_not_allowed`, or `uploads_off` when the entry has no `uploads`. The media type is what the content sniffs as (Go's `http.DetectContentType`, the WHATWG algorithm), not the name, so a name can't pass one kind of file off as another. Sniffing tells a text file only as `text/plain`, so an entry that takes Markdown or JSON lists `text/plain` (or `text/*`).
- **Slack's external upload flow.** `files.getUploadURLExternal` reserves the file and returns its ID and a signed upload URL; the content is sent to that URL without a token; `files.completeUploadExternal` with `channel_id` and `thread_ts` shares it in the thread. A token without `files:write` is refused at the first call, with nothing reserved, as `upload_scope_missing`.
- **Written once, without a marker.** A file carries no message metadata, so the intent record (`call/<execution>/<request id>`, like a tool reply's) keeps the reserved file's ID instead. Only the share publishes anything, so only it can be ambiguous: the record is `attempted`, with the file's ID, before `files.completeUploadExternal` is called. A failure before that leaves the record `pending` and shares nothing (a reserved file nobody completes stays invisible), so a retry reserves another file. A lost answer leaves it `attempted`; after 30 seconds the call or the projector's next pass reads the thread and completes the record when a message there carries the file's ID. When none does, the same file is shared again, which needs only its ID, so the record never holds the content. A refusal to share ends the record `failed` (`slack_refused`), except that after a lost answer the thread is read once more first, in case the earlier share was made after all. The same request ID with another file is refused (`intent_conflict`).
- **Not input.** The file's message is the app's own, so normalization classes it as an agent message (with `bot_user_id`) or a bot message, never a person's.

`TestSlackUploadIsOnceAndWithinTheEntrysLimits` and `TestSlackUploadNeedsTheEntrysUploadsAndTheScope` cover this against the fake, which shares a file once and can lose or refuse the share's answer.

## Message layout

Models write standard markdown, so a message's text goes to Slack as a `markdown` block by default, and the agent is never taught Slack's `mrkdwn`. The text is also the message's `text`, which Slack shows in notifications and screen readers, and which the marker's text fallback and `slack.thread` read back. Markdown blocks share a limit of 12,000 characters a message. Longer text with nothing after it is posted as plain text, which Slack reads as `mrkdwn`; with blocks after it, it is refused (`text_too_long`). An entry with `projection.format` `mrkdwn` keeps the older behaviour: the text is posted as plain text, read as `mrkdwn`, and becomes a `section` block (at most 3,000 characters) only when blocks follow it. A prompt lays out its own blocks and isn't affected.

`slack.reply` and `slack.post` take an optional `blocks`: Block Kit blocks shown after the text, such as a `table`, a sortable `data_table`, a `data_visualization` chart or a `card`. The host keeps them as the agent sent them and posts them as sent (only compacted, with `<`, `>` and `&` escaped, which Slack reads as the same string), so a number in a table or chart reaches Slack exactly. The intent's digest of the blocks uses that same encoding, so a reply the projector finishes from its record is the same intent; it decodes only a copy to check them.

**Decision: the host admits display blocks only.** A button, select or input in an agent's message would send presses that nothing here handles, so the host refuses them (the one interactive block it does handle, the [feedback buttons](#feedback-buttons), it lays out itself) before anything is posted, and it fails closed. A block's type must be `markdown`, `section`, `header`, `divider`, `context`, `image`, `rich_text`, `table`, `data_table`, `data_visualization` or `card` (otherwise `block_not_allowed`). Nothing anywhere in it may be an interactive element (a `button`, `action_cell`, `feedback_buttons`, any select, picker or text input) or carry `action_id`, `actions` or `dispatch_action` (`block_interactive`), so a card's actions and a data table's button cells are refused while the rest of those blocks pass. The host also bounds what it keeps: at most 49 blocks after the text, 100,000 bytes in all. Everything else about a block (its fields, its sizes, how many charts a message may carry) is Slack's to judge. Its refusal comes back to the agent with Slack's reason, so the agent can correct the block. A probe in a real workspace (issue #674) showed `markdown`, `table`, `data_table`, `data_visualization`, `card` and `context_actions` render in a message for an app with only `chat:write`, `chat:write.customize` and `reactions:write`, and that `alert` is refused in a message. `TestSlackReplyToolSendsMarkdownAndBlocks` covers the layout, the refusals and Slack's reason against the fake.

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

## Prompts and button answers

While a conversation's Work waits, or is ready but hasn't started (held until a person approves its start, say), the projector lists the actions offered on it (`Host.Actions`) and posts each one that has choices, such as an approval request's **approve** and **refuse** or a question's options, once in the thread. The message shows the action's label, its detail (what the provider says is being decided: the question asked, or the call an approval is for, with its input) and a button per choice. Each prompt is a marked reply with the intent `prompt/<work>/<provider>/<action>/<digest of label, detail and choices>`, so the same open question is posted once however often the Work waits, a new question is posted afresh, and a lost post is recovered like any reply. An action that takes free text has no buttons and isn't prompted. The extension doesn't know what any action means.

A press arrives over Socket Mode as an `interactive` envelope, which is acknowledged at once. A `block_actions` press of one of the prompt's buttons, from the entry's team and a watched channel, goes to `Host.Act` as the presser, `slack:<user id>`, keyed by the user and the press's `action_ts`. The action's provider decides whether that user may act; for a `human` entry, its responders. The presser then gets a message only they see, in the thread: the provider's message when the answer was taken, or its refusal ("you are not one of the people this question is for", "this question has already been answered"). A press the host couldn't take for any other reason asks them to press again; it isn't retried.

Presses arrive only over Socket Mode. The HTTP Events API receiver doesn't take interactivity, and an app created before this needs **Interactivity** turned on in its settings.

Out of process, `Host.Actions`, `Host.Act` and `Host.Requests` are the host's `action.list`, `action.submit` and `request.list`, which the host serves only to a package whose manifest has the `action.submit` permission; the Slack package declares it. When the host refuses to list a Work's actions or requests, the thread shows nothing for it, so the projection pass reports the refusal as `actions:<code>` or `requests:<code>` (`undeclared` for a method the manifest doesn't enable), and doctor `slack` warns `replies_failing` with it until a pass lists them again. A host that doesn't serve listing at all (`unsupported`) offers nothing, which isn't reported.

### Child requests

A conversation hands tasks to child Work (`create_work`), and a child's gated call or question waits on a person the thread would otherwise not hear of, since the child isn't a conversation. With `child_requests` on, the projector also reads the requests reported on child Work (`Host.Requests`, which `human` answers with its asks): open or settled, with who may answer each (its responders) and how it ended. A child's request shows in the thread of its nearest ancestor that is a conversation of this entry, so a child of a child goes to the same thread, and a descendant that is a conversation itself keeps to its own thread.

While a child is waiting or ready, each of its open requests that names the action answering it, alone, and whose responders include a Slack person (`slack`, or `slack:<user id>`) is posted as a prompt, as above, with the buttons of the action that answers it. Its text names who asks (the child's binding identity's name, else its binding) and says who can answer: "Steve asks: Approve or refuse github.pr.create", with the call and its input. A request only operators or GitHub users can answer gets no prompt, since nobody in Slack could press it; it shows only in the progress message. A press answers the child's request: it goes to `Host.Act` on the child as `slack:<user id>`, and the provider's responders decide whether it is taken, as for the conversation's own. The conversation's own run isn't involved and doesn't wake. Its intent is `prompt/<child work>/…`, like the conversation's own.

**Decision: state in the thread, buttons only where they help.** The person who answers is often named in a gate and often not in the thread, and they may answer from the web view, the TUI or GitHub. So what the thread needs is the request's state; the buttons are posted only when someone who can press them may be there. Whether a request was granted, refused or timed out is the provider's to say, so the extension reads it from `Host.Requests` rather than from the provider's own Events. How a named responder outside the thread hears of a request is the job of [request messages](#request-messages), or of whatever the deployment builds on the `human` extension's request Events.

### Request messages

A request's named responder is often nowhere it shows: Work a schedule or a webhook started has no thread, an approver need not be in the conversation that asked, and a child's request shows only in its conversation's thread. With `request_messages` enabled, the projector reads the requests on every waiting or ready Work of the entry's scope (`Host.Requests`), conversation or not, and sends each Slack person an open request names (`slack:<member id>`) a direct message, once: the app posts it to their user ID, which Slack delivers in the app's DM with them (the App Home messages tab the manifest turns on; `chat:write` alone). A bare `slack` responder names nobody, so it is sent nothing; an operator or a GitHub user is not a Slack person. As for a child's prompt, a request is sent only while the action that answers it, alone, is offered on the Work, with that action's buttons. Its text names who asks (the binding identity's name, else the binding) and what, with the provider's detail and "Who can answer: …"; when the Work is a conversation, or a conversation of the entry handed it off at any depth, it links to that thread (`chat.getPermalink`).

A press in the DM is taken like a press in a thread: `Host.Act` on the asking Work as `slack:<user id>`, and the provider decides whether it is taken. The DM's channel is not one the entry observes, so `Interact` takes presses from a DM only while `request_messages` is on. Once the request is no longer open, however and wherever it ended (a press in the DM or the thread, the web view, a GitHub `/answer`, its deadline, the asker giving up), the next pass changes the message with `chat.update` to its first line and how it ended ("Answered by slack:U0123ABCD.", "Refused by …", expired, withdrawn) and drops its buttons. A person no longer among an open request's responders has theirs closed the same way. An escalation that names someone new sends them their own message.

Each message is one request's to one person, with a durable record under `dm/` in the projector's namespace (`pending`, `attempted`, `posted`, then `closed`, or `failed` when Slack refused the post). A post carries the reply marker, so one whose answer was lost is looked for in the person's DM (`users.conversations` and `conversations.history`, which need `im:read` and `im:history`) after the search delay, and posted again only if it isn't there. Without those scopes doctor warns, and such a post is sent again rather than found. A lost post whose request settles before the search runs is still looked for once the delay has passed: found, it is closed like any other; not there, its record is closed as never posted (left to be sent if the request is still open for the person); with the DM unreadable, its record is closed as it stands (`dm_unreadable`). A rate limit or a credential failure stops the pass, as for replies.

**Decision: the messages follow the provider's requests, not its Events.** The `human` extension also announces each change of a request as an Event, for a deployment's own channels. Slack reads `Host.Requests`, the contract every request provider answers, so it sends a request of any provider, and recovers from a restart by reading what is open now rather than by replaying history.

## Progress messages

With `progress` enabled, each run of a conversation (`slack.conversation`) gets one message in its thread that shows what the run is doing. A run is what one person's message starts or wakes: the latest person's message routed to the Work as input, not an edit or a deletion. It lasts until the next such message, which begins the next run.

Nothing runs alongside the run, and the agent calls nothing. Every projection pass (every five seconds) derives each live run's message from durable state alone: the Work, the child Work made since the run's message was accepted, the Work's Waits and the actions offered on it (`Host.Actions`). A run parked for hours, or a daemon restarted in between, shows the same message, because nothing about it lives in a process. Slack's streams close ten minutes after they start, which a hand-off can outlast, so the message is a `chat.postMessage` with a `plan` block that `chat.update` changes in place, as Slack advises for long tasks.

| What the run is doing | Plan title | Reaction on the person's message |
| --- | --- | --- |
| Queued, or held for an approval to start | Starting (Waiting while held) | `eyes` |
| Running | Working | `eyes` |
| Waiting for an approval or a question (an awaited action) | Waiting | `eyes` |
| Waiting for a child Work | Waiting for hand-offs | `eyes` |
| Waiting for anything else (a timer, another system) | Waiting | `eyes` |
| Waiting only for a person to write in the thread | Waiting for your reply | `white_check_mark` |
| Ended by the next person's message, or the Work completed | Done | `white_check_mark` |
| Failed | Failed | `x` |
| Cancelled | Stopped | `octagonal_sign` |

The plan has a task per child Work (its display title, `in_progress` until it completes, `error` if it failed or was cancelled), and a task per step the run waited on: an awaited action (an approval or a question, by its label) and a Wait that says what it is for (by its description and progress line). A step no longer offered or active is shown `complete`. With `child_requests` on, each child's task is followed by the requests it and its own children hold (see [child requests](#child-requests)), as "<who> asks: <label> (for <responders>)": `in_progress` while open, `complete` once answered or withdrawn, and `error` once refused or expired. A run with neither has one task, the plan's title. Slack's limit of 50 tasks keeps the latest. What a child or a gate means stays its owner's; the plan only shows their state. An approval's buttons are its [prompt](#prompts-and-button-answers), posted beside the progress message as before.

The message is posted once, as a marked reply with the intent `progress/<work>/<run event>` (so a lost post is found by its marker), under the binding's identity when the binding has one. It is then updated only when what it shows changes, at most once a pass, and each layout gets new block IDs, as Slack asks of an updated plan. A run that has already handed back by the first pass that sees it gets no message, so turning `progress` on doesn't post into every waiting conversation. Each run's record (the namespace's `progress/` keys) holds what it last showed and the reaction it last set, and is not read again once the run has ended and both are up to date.

While the run is live and `stop` lists anyone, the message carries a **Stop** button, with a confirmation. A press arrives like a prompt's: Slack says who pressed it, and the extension checks them against `stop`. Slack holds the authority here because the conversation is Slack's own Work kind; `Host.CancelWork` accepts only an extension's own kinds and checks the Work's revision. A press from anyone else, from another workspace or channel, or for Work that isn't this entry's conversation in that channel, cancels nothing. The cancellation is keyed by the presser and the press's `action_ts` and its reason (`stopped from Slack by slack:<user>`) goes in the Work's history. The presser is told the outcome in a message only they see.

The calls are `chat.update` (`chat:write`) and `reactions.add`/`reactions.remove` (`reactions:write`), both in the manifest already, so the setting needs no new scope and no Agents & AI Apps setting.

## Feedback buttons

With `feedback` enabled, a reply carries Slack's feedback buttons (a `context_actions` block with a `feedback_buttons` element, thumbs up and down) after the text and the agent's blocks: `slack.reply`'s replies and a conversation's outcome reply, from the bindings `feedback.bindings` names, or every binding when it names none. A `slack.reply` call with `"feedback": false` leaves them off that reply; it can't turn them on where the entry doesn't. Prompts, progress messages and `slack.post` posts have none. The host lays the block out, never the agent, so agent blocks still may not carry `feedback_buttons`. Whether a reply gets the buttons is kept in its intent record, so a retried or recovered post has the buttons its first attempt had; it isn't part of the intent's identity. A reply that can't take one more block (its text too long to lead blocks, or 49 blocks of its own) is posted without them rather than refused.

Each button's value carries the Work that posted the reply, the attempt and the reply's marker. A press arrives over Socket Mode like a prompt's, from the entry's team and a watched channel. The projector checks that the Work is this entry's and holds the reply's thread, then accepts `slack.feedback` on it: `{namespace, team, channel, thread, message, user, value, execution, intent, pressed_at}`, where `message` is the reply's ts and `value` is `positive` or `negative`. The delivery ID is the presser and the press's `action_ts`, so a redelivered press is accepted once. Nobody is told anything unless the press couldn't be recorded, when they're asked to press again.

**Decision: feedback is history, not input.** The `slack` route matches `slack.feedback` with `deliver: none` ([routing](../routing-lifecycle/)), so the Event is in the Work's history but is not its input: no wait matches it, no run wakes for it, and the conversation carries on as it was. On Work that has finished, the router files it as `terminal` on that Work: still in its history, and no successor is opened, since a match that only attaches never opens one. Each press is its own Event and nothing is overwritten, so a change of mind is a later Event and a person's latest press on a reply is what counts. What feedback means and what to do with it belongs to whoever reads the history. `TestSlackFeedbackIsRecordedWithoutWaking` and `TestSlackFeedbackFollowsTheEntryAndTheCall` cover it against the fake.

## Moving to a new host

Copy the state, not just the config. Stop the daemon, `backup create` a bundle, and on the new host `backup restore` it and point the daemon at the restored directory ([backups](../../guides/backups/)). The bundle carries every Work and its thread keys, its waits, and the entry's start points and marked-thread records, so waiting conversations continue in their threads and nothing is claimed twice.

If the new host starts with an empty state directory instead, the entry registers afresh and its start point is the new daemon's first start. No thread from before then is claimed, and the conversations the old daemon was holding open are not continued: a person's reply in one of those threads reaches no Work. Doctor's `slack` check counts the threads whose replies carry an earlier daemon's marker. To continue them, stop the new daemon and restore the old state instead. To have the new daemon claim recent history anyway, set `claim_from` to a time (or `all`); threads with marked replies are still not claimed.

## Limitations

- **Real Slack.** Real Slack has been exercised by one short run (see [History](#history)). Not covered on real Slack: a rate limit, `app_mention` duplication, the HTTP Events API, the `slack-cli` setup path, a reply missed by push and recovered by reconcile, a SIGKILL during an active turn, and a multi-day wait. The drills cover the missed reply and the SIGKILL, and the fake-clock scenario covers the multi-day wait in CI.
- **Posting.** `slack.post` has not been run on real Slack. A Work that posts several threads owns them all, but `slack.reply` and `slack.thread` act on the first thread key it holds. A successor answers only the thread whose question opened it. A message in another thread its predecessor posted, while the successor is live or after it has finished, is skipped and starts nothing, so it is never answered in the wrong thread. A thread posted before the binding was recorded (by Work from before this change) holds no posted key and opens no successor.
- **Identities.** Posting under a binding's own name has not been run on real Slack; the fake follows Slack's documented shape. A message posted under a custom name while Slack withholds metadata has no marker and no bot user, so normalization classifies it as `slack.message.bot`. That is never input either, but `slack.thread` shows it as a bot's message, not the agent's.
- **Registration lifetime.** As for GitHub, ingress registrations are startup-only. A changed entry reports `restart_required`.
- **Progress messages.** These have been exercised only against the `slacktest` fake; the `plan` block and its updates follow Slack's documented shape and the issue's probe. A plan block and an `actions` block in the same message is not documented by Slack. A turn's status summary isn't shown, and the final reply isn't streamed.
- **Feedback buttons.** These have been exercised only against the `slacktest` fake; the block follows Slack's documented shape, and issue #674's probe showed `context_actions` renders in a message. The fake doesn't model Slack's own pressed state on the buttons.
- **Volume.** The projector reads every Work in its scope each pass, and the router's per-event cost grows with a Work's queued input. Both are adequate for a support channel, with no high-volume claim.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- Ticket 04-01 delivered the Slack extension (ingress, Socket Mode and the Events API, reconcile, the route and marked replies) as the second workload. It added the separate one-shot deletion delivery to the shared ingress conformance.
- Ticket 04-02 delivered extension-owned setup (`init slack`), the manifest decision for `metadata.message:read`, the support binding and conversation lifecycle, and the generic inject acknowledgement. 04-01 and 04-02 used only fixtures and the `slacktest` fake.
- Ticket 04-03 added the fake-clock multi-day scenario (`slack_multiday_test.go`) and a short real-Slack run, below. A real multi-day wait was deferred to real-world operation by user decision.
- Issue #364 added prompts and button answers: a waiting Work's actions with choices are posted as buttons, and a press answers through `Host.Act` as `slack:<user>`. The manifest turned Interactivity on. This has been exercised only against the `slacktest` fake. Later in the same issue, ready Work that hasn't started was prompted too, so a held start can be approved from the thread.
- Issue #673 added progress messages: with `progress` enabled, a plan per run derived from durable state on each projection pass, with reactions on the person's message and a Stop button for the people `stop` lists, cancelling through `Host.CancelWork`.
- Issue #681 added `child_requests`: a child Work's requests show in the conversation's thread, as progress tasks with their state and responders, and as prompts when a Slack person may answer, read from the new `Host.Requests`. The issue first asked for the buttons alone, and was narrowed to show the state everywhere and the buttons only when a Slack responder could press them.
- Issue #759 found that none of the thread's buttons or child requests had shown since the extension moved out of process (#466): the package's manifest lacked the `action.submit` permission, so the host refused `action.list`, `action.submit` and `request.list`, and the projector dropped the refusal silently. The manifest now declares it, a refusal is reported by the pass and doctor, and `TestDaemonSlackShowsAChildsGatedCall` drives a child's gated call to the thread through the composed daemon.
- Issue #675 added feedback buttons: with `feedback` enabled, the host puts Slack's feedback buttons below replies, and a press is recorded on the Work that posted the reply as `slack.feedback`, attached without being input.

### Real Slack run (04-03)

A short run on 2026-10-25 used a real workspace app created with `init slack` from the manifest (Socket Mode, tokens in the keyring) and the support binding on Claude Code with `model: haiku`:

- `doctor --check-secrets`: `slack-auth verified`, with the granted scopes equal to the manifest's (no `scopes_missing`).
- **Metadata.** With `metadata.message:read`, `conversations.replies` with `include_all_metadata=true` returns the marker's full payload `{work, execution, intent}`. Without `include_all_metadata`, only the `event_type` comes back. The case without the scope was not tested (it needs an install without it), so the text fallback remains fixture-proven.
- **One real thread.** The top-level message created one `slack.conversation` Work over Socket Mode. The marked reply came back as `slack.message.agent` and was never input. The first thread reply woke the wait and resumed the same execution and Claude session. The daemon was SIGKILLed mid-wait and restarted, and the next reply woke the same Work, execution and session. A message posted 14 s into that turn was routed `inject`, echoed (`injected {accepted 1, consumed 1}`) and answered in the same reply. It neither woke the next wait nor started an attempt. Attempt 1 was the only execution, across three turns.
- **No workspace or Git.** `github` stayed `not_configured`, with 0 resource leases.
- **Not observed.** No 429 was seen, so real rate limits are unmeasured. No mention was posted, so `app_mention` duplication was not seen. `--via slack-cli` was not run.
- Issue #357 moved the extension off its bespoke bootstrap wiring (`internal/bootstrap/slack.go`) onto `extension/inproc`. An entry's webhook route moved from doctor `webhooks` to doctor `slack`, and its reconcile state to the host's `reconcile_lag` check, which now lists every source's last scan.
- Issue #428 replaced the hand-written package that mirrored the extension with `inprocsdk` serving the extension itself, so there is one implementation, and moved it out of process. The package now ships the `slack-thread` playbook, and its deep check is `slack-auth`.
