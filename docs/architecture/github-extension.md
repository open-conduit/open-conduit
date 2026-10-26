# GitHub ingress, routing policy and projections

Ticket 03-01 delivers the first-party GitHub extension in [`extensions/github`](../../extensions/github). It is built against the public contracts: [B03 ingress](extensions.md), [B04 routing](runtime-semantics.md) and [B02 storage/KV](domain-storage.md). It imports only public packages, and the runtime does not import it. [`internal/bootstrap`](../../internal/bootstrap/github.go) registers it with the daemon. The Open Supervisor adapter (`internal/adapters/github` at the pinned baseline) was the behavioral reference.

## Configuration

A `github` connection holds non-secret config: `api_url` (default `https://api.github.com`, or an Enterprise `/api/v3`), `auth` (`token` by default, or `app` with `app_id`) and an optional `bot_app_id`. Credentials come only from the connection's `secret_refs`:

- `token` for token auth. The gh CLI login is `cmd:gh auth token`, so no separate `gh` mode exists.
- `app_private_key` for app auth.
- `bot_private_key` for the bot App.

Secrets are resolved lazily before a call, cached for the credential's lifetime and resolved again after a 401. A rotated reference therefore takes effect without a restart. Credential failures return `ErrCredential` and never include the reference or its value.

A `github` extension entry (an ingress entry: `config.scope` must be the containing scope) has these fields:

| Field | Meaning |
| --- | --- |
| `namespace` | Stable source identity (for example `github/github.com/owner/repo`). Also the correlation-key source. |
| `repo` | The repository (`owner/name`). |
| `tracker` | Optional shared tracker `{repo, namespace}` whose issues this repository's PRs reference as `owner/repo#N`. |
| `track_issues` | Default true. False observes only PRs, comments, reviews and checks. |
| `labels` | `claimed`, `scheduled`, `help`, `review`, `done`, `skip[]`, `require[]`. The defaults are Open Supervisor's names, so either runtime sees the other's claim. |
| `closed_horizon_hours` | Default 168. How long closed items stay observed. |
| `branch_pattern` | Default `^agent/issue-(\d+)$`. `""` disables branch correlation. |
| `preview_pattern` | Default `deploy-preview`. |
| `projection` | `disabled`, `verify_seconds` (default 900) and `dry_run`. |
| `webhook_secret` | Names a selected scope secret. |
| `base_branch` | The branch PRs target (default: the repository's). Since 03-04. |
| `no_checks_minutes` | Default 10; `-1` disables. How long a PR head with no checks waits before `github.checks.none` (since 03-04), and how long a head without a check the base branch requires is held before `github.checks.missing` (since 08-01). With `-1` such a head stays pending. |

The entry also needs a `connection`. A route uses handler `github`.

## Client and transport

The client enforces these rules:

- Every API GET is conditional. A cached ETag makes an unchanged poll a 304 that does not count against the rate limit.
- A primary limit (`X-RateLimit-Remaining: 0` with its reset) or a secondary limit (`Retry-After`, or a "rate limit" message, which waits a minute) pauses that credential. The credential is identified by a digest only. Its calls then fail locally with `RateLimitError` until the limit lifts. A 403 with neither signal stays an ordinary error.
- Lists follow `Link: rel="next"`. An unfinished listing is an error, never a short success.
- App auth signs an RS256 JWT, looks up the installation and caches installation tokens until five minutes before they expire. A 401 invalidates the cached token and retries once.
- Writes (labels, comments, reactions) use the bot App when one is configured. Reads use the sign-in identity.

## Ingress and canonical identity

`Spec` declares one recoverable family per observed state. Only `github.delivery`, the raw webhook delivery (the input for future webhook jobs), is non-re-derivable, and it declares its loss limitation.

| Families | Item | Correlation |
| --- | --- | --- |
| `github.issue.open`, `github.issue.closed` | `issue/N` | `issue/N`, or `issue/N/generation/reopen-<event id>` after a reopening |
| `github.pull_request.open`, `.closed`, `.merged` | `pr/N` | The tracker issue the PR belongs to, else `pr/N` |
| `github.comment.human`, `.bot`, `.agent` (conversation, inline and review; `kind` in the payload) | `issue/N/comment/ID`, `pr/N/comment/ID`, `pr/N/review_comment/ID`, `pr/N/review/ID` | Their issue or PR's key |
| `github.checks.pending`, `.passed`, `.failed`, `.none`, `.missing` | `pr/N/checks` | The PR's key |

Push and reconcile share one normalizer. The observation version hashes the whole normalized envelope (type, key, occurrence time, payload). Equal external state from either transport therefore has the same version and delivery ID (`item@version`), and B02 deduplication keeps the first accepted envelope. Any difference is a new version, so the runtime's canonical-identity check can never wedge a source. Types name observed state, not webhook actions, because reconcile sees state. Author class is in the type, so a Wait can filter out agent and bot notes without a payload predicate.

The issue payload deliberately omits `updated_at`, comment counts and the projected status labels. Comments and status projection therefore never create issue observations, and projection cannot feed back into itself. The claim label is included, because admission reads it. Lifecycle recurrences stay distinct because `closed_at` and the reopen generation change with each close and reopening. This is a deliberate application of B03's recurrence rule. Comments, reviews, PR heads and check runs carry their own occurrence identity (`updated_at`, head SHA, run IDs).

**Reopen generations (B04).** An issue's generation is the latest `reopened` event at or before the observation time: the issue's current state, a comment's creation time, or the creation time of the PR that references it. This depends only on immutable history, so push and reconcile agree. An open issue whose `state_reason` is not `reopened` needs no history read. Old keys stay with the terminal owner. The first observation of a new generation creates successor Work, and its payload names the previous generation as `predecessor` (opaque lineage). A reopened issue always starts a new generation. If the previous generation's Work is still active, it receives the close as input and winds down. PR keys are not generation-qualified: a PR correlates with the issue generation current when it was created.

**PR attachment (v1 `recordForPR`).** A PR, and every comment, review and check on it, is keyed to its tracker issue. The issue is found from the branch pattern first, then from a closing or reference keyword in the PR body. A shared tracker accepts only `owner/repo#N` naming it. Deriving the key deterministically means no route has to attach keys, and PR state changes reach the issue's Work directly. A reference to a missing issue falls back to `pr/N`.

**Push.** `VerifyDelivery` checks `X-Hub-Signature-256` (HMAC-SHA256, constant time; an empty secret never verifies) before parsing. It wraps GitHub's event name and delivery GUID, which travel in headers, into the Delivery body (`application/vnd.open-conduit.github-delivery+json`). `WebhookHandler` maps outcomes to HTTP statuses: 202 only after the runtime durably accepted the delivery, 401 for a bad signature and 503 when the delivery was not accepted. The listener and tunnel that expose it are delivered by 03-05 ([operator setup](operator-setup.md#webhooks-tunnels-and-reconcile-only-operation)). Without them the daemon runs reconcile-only, which loses only latency.

Push makes read-only service calls for generation history, PR details and aggregate checks, and no coordination writes. When check runs are refused (a fine-grained token cannot hold Checks), the aggregate uses the head's Actions workflow runs, else statuses only (03-06, see [engineering tools](engineering-tools.md#checks-timing)). A review without a body is never observed. GitHub wraps every inline comment and reply in one, the agent's own included. Observed as a human comment, it woke waiting agents in a loop on the real sandbox; v1 likewise kept only reviews with a body. Check events re-read the aggregate state of each open PR whose head is the delivery's commit, exactly as reconcile evaluates it. A status event finds its PRs through `commits/{sha}/pulls`.

**Reconcile.** GitHub has no snapshot token. A generation lists every issue and every PR in stable creation order, so pagination cannot repeat an item; a deletion can only skip one. It observes open items and items closed within the horizon, then the repository's conversation and inline comments updated within the horizon. Reviews and checks are read for each listed PR.

Each `Reconcile` call is one bounded page. The cursor is `phase:page:offset`, and a call stops after 25 detail reads or 10 seconds. `Complete` certifies that every listing was read to its end in this generation. Rate limits and failed reads return errors, so no absence is inferred.

Items that age out of the horizon become generic `core.ingress.absent`, which the route ignores. A false absence caused by a mid-scan deletion is therefore harmless, and the next generation re-observes the item under its original delivery identity.

The runtime publishes a generation's changes in lexical item order. Items nest under their issue or PR, so a newly observed issue (which may create Work) precedes the comments that become its input.

Two safety nets restart a generation by returning an exhausted partial page: a generation older than six hours, or a page that keeps being re-requested after a successful answer (the runtime rejected it).

## Route policy

Handler `github` reads only the detached Event, Scope and Work. It finds its extension entry in the scope metadata by namespace.

- **Unowned issue observations** create `github.issue` Work only when the issue is admitted (Open Supervisor parity):
  - The issue is open.
  - It has no skip label.
  - It has a required label, if any are configured.
  - It does not carry the claimed label, unless it is a reopened generation. A claimed issue with no Work belongs to another owner, which preserves a single owner while Open Supervisor runs.
  - Scheduled issues are claimed by the `github-hold` route (03-04), never by `github`.
- **Existing Work** receives these as input: new human comments, reviews with a body, other Works' agent comments (including Open Supervisor-marked notes from other issues), closes, merges and completed checks (passed, failed, none and missing required checks).
- **Never delivered:** edits, unmarked bot comments, this Work's own notes, open-state updates, pending checks and absence.

Labels influence only this claim decision. Work lifecycle never reads them.

## Projections and marked comments

`Projector.Converge` (run by the daemon at most every five seconds) reads durable state: Work, attempts and outcomes in the extension's scope whose keys are in its namespace. When several generations share an issue, the live one wins.

**Labels** are level-triggered:

- `claimed` is present while Work owns the issue.
- No status label is set while an attempt runs (v1 parity).
- After an attempt, the status comes from an outcome declaring `github.status.v1` `{status}`. Otherwise completed Work projects `done`, failed Work projects `help` and cancelled Work projects none.

The projector re-reads the issue and changes only managed labels. It reapplies them when desired state changes, and at `verify_seconds` for nonterminal Work, which repairs drift.

**Comments** are not re-derivable. An outcome's `github.status.v1` `{comment}` becomes an intent with the stable ID `outcome/<execution id>` and a durable KV record. The record moves through these states:

1. `pending`
2. `attempted`, recorded before the POST
3. `posted`

The body carries `<!-- open-conduit:agent work=<id> intent=<digest> -->`. If a post's result is ambiguous (a lost response, or a crash after GitHub created the comment), the record stays `attempted`. After a 30-second search delay, the projector finds the marker in the issue's comments and completes the record instead of posting again. A changed body under a reused ID is refused (`intent_conflict`).

**Fallback-window marker (06-02).** `WithMarker` appends Open Supervisor's own-note marker (`<!-- open-supervisor:agent -->`, `LegacyMarker`) after the Open Conduit marker. A v1 that takes a scope back during the fallback window then reads Open Conduit's comments as agent notes rather than human input. `ParseAgent` reads the Open Conduit marker first ([migration and recovery](migration-recovery.md)).

**Reactions:** a `+1` acknowledges each human comment the router delivered to Work (the v1 "heard" signal). Reactions are idempotent at GitHub. The cursor never passes an unrouted comment. It retries rate-limit and credential failures, and skips permanent refusals.

## Verification

Run these commands:

```sh
go test -race ./extensions/...
go test -race -run 'TestGitHub|TestDaemonGitHub' ./integration
make check
GOTOOLCHAIN=go1.24.2 make check
make cross-build
```

The [shared ingress conformance](../../conformance/extensions.go) passes against the real adapter, using the [githubtest](../../extensions/github/githubtest) REST fake (test support only).

The unit fixtures cover:

- Push/reconcile identity for every family, including merges, closes, reopen generations, PR attachment and shared-tracker references.
- Horizon ageing, stable pagination with conditional revalidation, and rate-limited pages that fail and then resume.
- Restart of rejected or stale generations.
- Webhook signatures and HTTP mapping, primary and secondary rate limits, ordinary 403s, token re-resolution after a 401, App token caching and refresh, bot-App writes, reactions, markers including Open Supervisor's, and config validation.
- Admission and input-selection rules.

The [SQLite integration cases](../../integration/github_test.go) run the real runtime ingress and router, and cover:

- Reconcile-only recovery of missed webhooks (claim and skip rules, with the comment queued behind the issue).
- Webhook/reconcile overlap and redelivery deduplication. A close made while push was down is recovered, and a forged delivery is rejected.
- A rate-limited scan infers no absence, then recovers.
- Projection convergence, steady state and drift repair. Projection writes generate no input, and the status comes off while running.
- A lost comment response is recovered through its marker.
- A real SIGKILL after GitHub created the comment but before local completion. A restarted projector finds the marker and posts nothing twice.
- A reopened issue creates successor-generation Work while the terminal owner keeps its key.
- Acknowledgements of routed comments only.
- Restart in the middle of a generation.

[`TestDaemonGitHubReconcileToProjection`](../../integration/daemon_github_test.go) drives the composed `serve` daemon. The registered ingress reconciles the fake, the `github` route claims the issue and a shell attempt declares a status and comment. Labels and the marked comment converge, and doctor's `github` check is `ok`. A restart posts nothing again.

## Limitations and follow-on work

- **Not proven yet.** No real GitHub API was called; all evidence uses fixtures. Real issues, real rate limits and real webhooks are 03-06/03-07 gate evidence.
- **Webhook exposure (03-05).** Delivered in [operator setup](operator-setup.md): the separate listener mounting `WebhookHandler` per entry, ngrok/cloudflared/static tunnels, `manage_webhook` (`Client.EnsureWebhook`), `serve --dry-run`, the manual claim (`github.claim.requested`), `Client.Verify` for doctor's `github_auth`, and `Annotate` for operator views.
- **Holds, waits and tools (03-04).** Delivered in [engineering tools, waits and playbooks](engineering-tools.md):
  - the agent-facing operations, posting through `Projector.Deliver` durable intents (re-driven by `Converge`) and `CreateIssue`;
  - translated waits with dependency filters;
  - `github.agent.turn` from durable outcomes;
  - the `github-hold` route and executor, with the `scheduled` label managed while held or waiting;
  - `github.checks.none`.
- **Claude Code continuity.** Delivered by 03-03: `WorkComments` posts a Work's marked comment exactly once per intent (`Deliver` plus `IntentComment`) and reads its reactions. The [Claude Code executor](claude-code-executor.md)'s handoff feedback uses it through bootstrap.
- **Projection volume.** The projector reads every Work in its scope each pass. That is enough for the first switch, but it has no high-volume claim.
- **Unenforced setting.** `settings.admission_limit` (v1's claimed-per-project cap) is still not enforced.
- **Registration lifetime.** Ingress registrations are startup-only (B05). A changed or removed entry reports `restart_required` until the daemon restarts.
- **Raw deliveries.** `github.delivery` events are non-re-derivable by declaration. Which delivery families a webhook job may rely on is decided in 05-01.
