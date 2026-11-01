---
title: "GitHub ingress, routing policy and projections"
description: "How the GitHub extension observes issues, pull requests, comments and checks, claims issues and pull requests as Work and projects status back to GitHub."
---

The first-party GitHub extension observes a repository's issues, pull requests, comments, reviews and checks, claims admitted issues (and, when asked, each open PR) as Work, and projects each Work's status back to GitHub as labels, marked comments and reactions. It lives in [`extensions/github`](https://github.com/open-conduit/open-conduit/tree/main/extensions/github) and is built against the public contracts: [B03 ingress](../extensions/), [B04 routing](../runtime-semantics/) and [B02 storage/KV](../domain-storage/). It imports only public packages, and the runtime does not import it. [`internal/bootstrap`](https://github.com/open-conduit/open-conduit/blob/main/internal/bootstrap/github.go) registers it with the daemon.

The agent-facing tools, waits and scheduled holds built on this extension are described in [engineering tools, waits and playbooks](../engineering-tools/); the webhook listener, tunnels, doctor checks and operator controls in [operator setup](../operator-setup/).

## Configuration

A `github` connection holds non-secret config: `api_url` (default `https://api.github.com`, or an Enterprise `/api/v3`), `auth` (`token` by default, or `app` with `app_id`) and an optional `bot_app_id`. Credentials come only from the connection's `secret_refs`:

- `token` for token auth. The gh CLI login is `cmd:gh auth token`, so no separate `gh` mode exists.
- `app_private_key` for app auth.
- `bot_private_key` for the bot App.

A private key is the App's PEM. `parseKey` also accepts it hex-encoded, which is how macOS Keychain's `security find-generic-password -w` returns a multi-line value (so a `keyring:` reference reads it that way there), and with its line breaks written as `\n`, as an environment variable often holds it. Setting up a bot App is a [guide](../../guides/github-bot/). A binding whose `connection` is a second `github` connection writes as it, so a reviewing agent writes as its own App ([a reviewer's own writer](../engineering-tools/#tool-operations)).

Secrets are resolved lazily before a call, cached for the credential's lifetime and resolved again after a 401. A rotated reference therefore takes effect without a restart. Credential failures return `ErrCredential` and never include the reference or its value.

An applied change to the connection a registered entry uses (`auth`, `app_id`, `bot_app_id`, `git` or its `secret_refs`) also takes effect without a restart: `registerGitHub` calls `Client.Reconfigure`, which swaps the client's reader, writer and git setting and drops its cached tokens, so the next write goes out as the new identity. A change of `api_url` can't be applied in place and reports `restart_required` for the entry.

A `github` extension entry (an ingress entry: `config.scope` must be the containing scope) has these fields:

| Field | Meaning |
| --- | --- |
| `namespace` | Stable source identity (for example `github/github.com/owner/repo`). Also the correlation-key source. |
| `repo` | The repository (`owner/name`). |
| `tracker` | Optional shared tracker `{repo, namespace}` whose issues this repository's PRs reference as `owner/repo#N`. |
| `track_issues` | Default true. False observes only PRs, comments, reviews and checks. |
| `labels` | `claimed`, `scheduled`, `help`, `review`, `done`, `skip[]`, `require[]`. |
| `closed_horizon_hours` | Default 168. How long closed items stay observed. |
| `branch_pattern` | Default `^agent/issue-(\d+)$`. `""` disables branch correlation. |
| `preview_pattern` | Default `deploy-preview`. |
| `projection` | `disabled`, `verify_seconds` (default 900) and `dry_run`. |
| `webhook_secret` | Names a selected scope secret. |
| `base_branch` | The branch PRs target (default: the repository's). |
| `no_checks_minutes` | Default 10; `-1` disables. How long a PR head with no checks waits before `github.checks.none`, and how long a head without a check the base branch requires is held before `github.checks.missing`. With `-1` such a head stays pending. |
| `track_pull_requests` | Default false. Also observes everything about each PR (its state, comments, reviews, checks and conflicts) under the PR's own key, for a route that [claims pull requests](#pull-request-work). |

The entry also needs a `connection`. A route uses handler `github`, with optional config `{"claim": "issues"}` (the default) or `{"claim": "pull_requests"}`, which claims [pull request Work](#pull-request-work); `"drafts": true` lets a `pull_requests` route claim draft PRs as well.

## Client and transport

The client enforces these rules:

- Every API GET is conditional. A cached ETag makes an unchanged poll a 304 that does not count against the rate limit.
- A primary limit (`X-RateLimit-Remaining: 0` with its reset) or a secondary limit (`Retry-After`, or a "rate limit" message, which waits a minute) pauses that credential. The credential is identified by a digest only. Its calls then fail locally with `RateLimitError` until the limit lifts. A 403 with neither signal stays an ordinary error.
- Lists follow `Link: rel="next"`. An unfinished listing is an error, never a short success.
- App auth signs an RS256 JWT, looks up the installation and caches installation tokens until five minutes before they expire. A 401 invalidates the cached token and retries once.
- Writes (labels, comments, reactions) use the bot App when one is configured. Reads use the sign-in identity.
- `GitToken` mints a token for git that is never cached: an installation token limited to one repository with `contents: write`, from the bot App or else the sign-in App. Its caller revokes it with `RevokeToken` (`DELETE /installation/token`). On a token connection it is `ErrStaticToken` ([`github.git.credential`](../engineering-tools/#tool-operations)).
- `GitIdentity` is that same App's bot user as git's author and committer (`GET /app` with the App's JWT for its slug, then `GET /users/<slug>[bot]` for the user id), looked up once; `ErrStaticToken` on a token connection.

## Git as the bot

A connection's `git` is who git works as in the runs of its repositories' Work: `owner` (the default) leaves the operator's own credentials and identity, and `bot` works as the App. `bot` needs an App (`auth` `app` or a `bot_app_id`); the connection validator refuses it on a token connection without one.

The extension does this through the generic run environment hook (`extension.RunEnvironment`, protocol `run.environment`; see [extension protocol](../extension-protocol/)), with nothing in an executor or the core that knows about GitHub. `Tools.RunEnvironment` binds the run's Work as a toolbelt call would (its issue's repository, else the scope's only one). The connection that decides is the binding's own `github` connection (`RunEnvironmentRequest.Connection`, built by `Writers` as for a binding's writes) when it names one, else the repository's entry connection; a binding naming none, the entry's, or one of another type uses the entry's. When that connection has `git` `bot`, it returns this layer:

- `credential.helper` set empty, then to `!os git-credential`, through `GIT_CONFIG_*`. The empty entry clears the helpers git would otherwise ask first (the operator's keychain or `gh`), so pushes go only through [`github.git.credential`](../engineering-tools/#tool-operations), which the binding must be granted.
- `commit.gpgsign` and `tag.gpgsign` off, because the operator's key isn't the bot's.
- `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` set to `GitIdentity`.

Every other run of a Work the extension can bind (`git` `owner`, including one whose binding's own connection leaves `git` unset while the entry's sets `bot`) gets only a credential helper for the connection's web host, `credential.https://<host>.helper` set to `TokenHelper`: it answers git's `get` with the run's `GH_TOKEN`, else `GITHUB_TOKEN` (a binding's personal access token), and says nothing when it has neither, so git goes on to its next helper. Git asks the operator's own helpers first, so on the host it is a fallback; in a container, which has none, it is how a static token reaches the repository. The Claude Code executor used to set this helper itself; it now names no code host. A run the extension can't bind gets nothing. [`github.git.credential`](../engineering-tools/#tool-operations) mints from the same connection, taken from the call's `Authority.Connection`, so the token and the identity come from the same App. A failed identity lookup, or a binding connection `Writers` can't build (another `api_url`), fails the run before it starts (`run environment unavailable`). Doctor's `git_bot` check warns when a git workspace binding in such a scope isn't granted `github.git.credential`, so every push is refused, or pushes over SSH, which skips the helper. `init github` and `migrate` grant the operation to every binding they write with a git workspace (`WorkspaceOperations`).

## Ingress and canonical identity

`Spec` declares one recoverable family per observed state. Only `github.delivery`, the raw webhook delivery (the trigger input for webhook jobs), is non-re-derivable, and it declares its loss limitation.

| Families | Item | Correlation |
| --- | --- | --- |
| `github.issue.open`, `github.issue.closed` | `issue/N` | `issue/N`, or `issue/N/generation/reopen-<event id>` after a reopening |
| `github.pull_request.open`, `.closed`, `.merged` | `pr/N` | The tracker issue the PR belongs to, else `pr/N` |
| `github.comment.human`, `.bot`, `.agent` (conversation, inline and review; `kind` in the payload) | `issue/N/comment/ID`, `pr/N/comment/ID`, `pr/N/review_comment/ID`, `pr/N/review/ID` | Their issue or PR's key |
| `github.checks.pending`, `.passed`, `.failed`, `.none`, `.missing` | `pr/N/checks` | The PR's key |
| `github.pull_request.conflicts` | `pr/N/conflicts` | The tracker issue the PR belongs to, else `pr/N` (a PR with no issue is observed only with `track_pull_requests`) |
| PR copies (`track_pull_requests`): every PR family above | `pull_request/N`, `pull_request/N/comment/ID`, `pull_request/N/review/ID`, `pull_request/N/review_comment/ID`, `pull_request/N/checks`, `pull_request/N/conflicts` | `pull_request/N` |

Push and reconcile share one normalizer. The observation version hashes the whole normalized envelope (type, key, occurrence time, payload). Equal external state from either transport therefore has the same version and delivery ID (`item@version`), and B02 deduplication keeps the first accepted envelope. Any difference is a new version, so the runtime's canonical-identity check can never wedge a source. Types name observed state, not webhook actions, because reconcile sees state. Author class is in the type, so a Wait can filter out agent and bot notes without a payload predicate.

The issue payload deliberately omits `updated_at`, comment counts and the projected status labels. Comments and status projection therefore never create issue observations, and projection cannot feed back into itself. The claim label is included, because admission reads it. Lifecycle recurrences stay distinct because `closed_at` and the reopen generation change with each close and reopening. This is a deliberate application of B03's recurrence rule. Comments, reviews, PR heads and check runs carry their own occurrence identity (`updated_at`, head SHA, run IDs).

**Reopen generations (B04).** An issue's generation is the latest `reopened` event at or before the observation time: the issue's current state, a comment's creation time, or the creation time of the PR that references it. This depends only on immutable history, so push and reconcile agree. An open issue whose `state_reason` is not `reopened` needs no history read. Old keys stay with the terminal owner. The first observation of a new generation creates successor Work, and its payload names the previous generation as `predecessor` (opaque lineage). A reopened issue always starts a new generation. If the previous generation's Work is still active, it receives the close as input and winds down. PR keys are not generation-qualified: a PR correlates with the issue generation current when it was created.

**PR attachment.** A PR, and every comment, review and check on it, is keyed to its tracker issue. The issue is found from the branch pattern first, then from a closing or reference keyword in the PR body. A shared tracker accepts only `owner/repo#N` naming it. Deriving the key deterministically means no route has to attach keys, and PR state changes reach the issue's Work directly. A reference to a missing issue falls back to `pr/N`.

**PR copies.** With `track_pull_requests`, every observation of a PR (its state, comments, inline comments, reviews, checks and conflicts: every item under `pr/N`) is emitted twice: once under the key above, and once more, with the same type and payload, under the PR's own key `pull_request/N`, with the item's `pr/` replaced by `pull_request/`. The copy is built from the original, so push and reconcile agree on it in the same way. An issue's Work owns the issue key and the PR's Work owns the PR's key, so each hears the PR's news without either taking a key from the other. Every PR family is copied alike, so the extension never decides which of a PR's events matter to whoever consumes them; nothing of an issue's is copied. Copies sort after every original, and a PR's copy before its comments' copies.

**Push.** `VerifyDelivery` checks `X-Hub-Signature-256` (HMAC-SHA256, constant time; an empty secret never verifies) before parsing. It wraps GitHub's event name and delivery GUID, which travel in headers, into the Delivery body (`application/vnd.open-conduit.github-delivery+json`). `WebhookHandler` maps outcomes to HTTP statuses: 202 only after the runtime durably accepted the delivery, 401 for a bad signature and 503 when the delivery was not accepted. The listener and tunnel that expose it are described in [operator setup](../operator-setup/#webhooks-tunnels-and-reconcile-only-operation). Without them the daemon runs reconcile-only, which loses only latency.

Push makes read-only service calls for generation history, PR details and aggregate checks, and no coordination writes. When check runs are refused (a fine-grained token cannot hold Checks), the aggregate uses the head's Actions workflow runs, else statuses only (see [engineering tools](../engineering-tools/#checks-timing)). A review without a body is never observed. GitHub wraps every inline comment and reply in one, the agent's own included, so observing it as a human comment would wake waiting agents in a loop. Check events re-read the aggregate state of each open PR whose head is the delivery's commit, exactly as reconcile evaluates it. A status event finds its PRs through `commits/{sha}/pulls`.

**Reconcile.** GitHub has no snapshot token. A generation lists every issue and every PR in stable creation order, so pagination cannot repeat an item; a deletion can only skip one. It observes open items and items closed within the horizon, then the repository's conversation and inline comments updated within the horizon. Reviews and checks are read for each listed PR. Each open PR that belongs to an issue (every open PR, with `track_pull_requests`) is also read on its own, because only that read carries `mergeable` and GitHub sends no webhook when a PR picks up conflicts (the base branch moving under it). When GitHub says it cannot merge, the PR is `github.pull_request.conflicts`. The payload names the head commit and the base branch but not the base's commit, and the occurrence is the head's commit time, so one conflicting head is one observation however often the base moves after it; a new head that still conflicts is a new one. A `pull_request` delivery that already carries `mergeable: false` observes the same. A PR that is only behind its base is not observed.

Each `Reconcile` call is one bounded page. The cursor is `phase:page:offset`, and a call stops after 25 detail reads or 10 seconds. `Complete` certifies that every listing was read to its end in this generation. Rate limits and failed reads return errors, so no absence is inferred.

Items that age out of the horizon become generic `core.ingress.absent`, which the route ignores. A false absence caused by a mid-scan deletion is therefore harmless, and the next generation re-observes the item under its original delivery identity.

The runtime publishes a generation's changes in lexical item order. Items nest under their issue or PR, so a newly observed issue (which may create Work) precedes the comments that become its input.

Two safety nets restart a generation by returning an exhausted partial page: a generation older than six hours, or a page that keeps being re-requested after a successful answer (the runtime rejected it).

## Route policy

Handler `github` reads only the detached Event, Scope and Work. It finds its extension entry in the scope metadata by namespace.

- **Unowned issue observations** create `github.issue` Work only when the issue is admitted:
  - The issue is open.
  - It has no skip label.
  - It has a required label, if any are configured.
  - It does not carry the claimed label, unless it is a reopened generation. A claimed issue with no Work belongs to another owner, which preserves a single owner.
  - Scheduled issues are claimed by the [`github-hold` route](../engineering-tools/#scheduled-holds), never by `github`.
- **Existing Work** receives these as input: new human comments, reviews with a body, other Works' agent comments, closes, merges, completed checks (passed, failed, none and missing required checks) and merge conflicts.
- **Never delivered:** edits, unmarked bot comments, this Work's own notes, open-state updates, pending checks and absence.

Labels influence only this claim decision. Work lifecycle never reads them.

A route with `claim` `issues` never takes input for a PR's Work, so the two can share a scope in either order.

### Pull request Work

A route with `{"claim": "pull_requests"}` claims each open PR as its own `github.pull_request` Work, owning the PR's key `pull_request/N`. What that Work does is up to the route's binding and its playbook; a review bot is one use ([recipe](../../guides/github-bot/#a-review-bot-for-every-pr)). The route reads only PR copies, so it needs an entry with `track_pull_requests`; otherwise its trace reason says so.

- **Unowned PR copies** create Work for a `github.pull_request.open` copy of an open PR that isn't a draft (with `drafts`, a draft too). A draft is claimed when it is marked ready, because that is a new observation of the open PR. A PR that belongs to an issue another Work owns is claimed all the same: the keys differ.
- **A PR's Work** receives what an issue's Work hears of its PRs (the merge or close, completed checks, merge conflicts, new human comments and reviews with a body, and comments from other Works' agents), and new versions of the open PR (a push, or a change of title, body or draft state) as well. Its own comments, reviews and replies carry its Work in the marker and are never delivered, nor are edits, unmarked bot comments and pending checks.
- **Lifecycle.** Each pass is an Execution. Between passes the agent parks with `github.wait`, and needs no condition: what wakes an issue's agent (comments, check results, conflicts, the merge or close) and a push to its PR wake it, and another agent's comment wakes it through the same peer dependency an issue's agent has ([waits](../engineering-tools/#waits)). Nothing is posted for the wait. The agent ends the Work when the PR merges or closes.

The agent reads which PR its Work is for from `github.context` (`pr`, `pr_url`, `repo`), and the PR operations default to its repository; it never sees the key. The projector leaves a PR's Work alone: it has no issue to label.

### Which scope routes an entry's events

The runtime routes an Event in the scope of the entry that observed it, using that scope's routes and the routes it inherits from its ancestors. The handler also requires the entry to belong to that scope. So a route in another scope, including a child scope from an [included folder](../config-management/#included-folders), never sees a repository whose entry lives in the operator's main scope; inheritance runs only from parent to child. To claim such a repository's PRs from its own scope, give that scope its own `github` entry for the repository: its own `namespace`, `track_issues: false` and `track_pull_requests: true`, beside its `pull_requests` route. It polls the repository a second time, which costs little because every GET is conditional, and it needs its own webhook or runs reconcile-only. The simpler choice, when the binding may live in the repository's scope, is a second route and binding there.

## Projections and marked comments

`Projector.Converge` (run by the daemon at most every five seconds) reads durable state: Work, attempts and outcomes in the extension's scope whose keys are in its namespace. When several generations share an issue, the live one wins.

**Labels** are level-triggered:

- `claimed` is present while Work owns the issue. It comes off once the Work completes, whether or not the issue is closed: nothing hears the issue any more, so the label would promise an agent who isn't there. The completed Work still owns the issue's generation, so admission never claims it again; a reopening starts a new generation, which is claimed again. Failed and cancelled Work keep it.
- No status label is set while an attempt runs.
- After an attempt, the status comes from an outcome declaring `core.turn.v1` `{status}` (or, for one release, the `github.status.v1` stored outcomes keep). Otherwise completed Work projects `done`, failed Work projects `help` and cancelled Work projects none.

The projector re-reads the issue and changes only managed labels. It reapplies them when desired state changes, and at `verify_seconds` for nonterminal Work, which repairs drift.

When Work completes and its issue is still open (its PR didn't close it), the projector posts one notice on the issue, as the durable intent `finished/<work id>`: the agent has finished, a comment there won't reach one, and closing and reopening the issue brings one back. A human comment on that issue is otherwise routed `terminal` and dropped, like any input for terminal Work.

**Comments** are not re-derivable. An outcome's `core.turn.v1` `{comment}` becomes an intent with the stable ID `outcome/<execution id>` and a durable KV record. The record moves through these states:

1. `pending`
2. `attempted`, recorded before the POST
3. `posted`

The body carries `<!-- open-conduit:agent work=<id> intent=<digest> -->`. If a post's result is ambiguous (a lost response, or a crash after GitHub created the comment), the record stays `attempted`. After a 30-second search delay, the projector finds the marker in the issue's comments and completes the record instead of posting again. A changed body under a reused ID is refused (`intent_conflict`).

**Reactions:** a `+1` acknowledges each human comment the router delivered to Work. Reactions are idempotent at GitHub. The cursor never passes an unrouted comment. It retries rate-limit and credential failures, and skips permanent refusals.

## Setup probes

For [setup sessions](../setup-sessions/) the extension offers two probes (`extension.SetupProbes`). `github.accounts` runs `gh auth status --json hosts` and returns each account's host, login, active flag, token source and state, never the token. `github.repos` runs `gh repo list OWNER` for an owner that must be a GitHub login, and returns repository names.

## Verification

Run these commands:

```sh
go test -race ./extensions/...
go test -race -run 'TestGitHub|TestDaemonGitHub' ./integration
make check
GOTOOLCHAIN=go1.24.2 make check
make cross-build
```

The [shared ingress conformance](https://github.com/open-conduit/open-conduit/blob/main/conformance/extensions.go) passes against the real adapter, using the [githubtest](https://github.com/open-conduit/open-conduit/tree/main/extensions/github/githubtest) REST fake (test support only).

The unit fixtures cover:

- Push/reconcile identity for every family, including merges, closes, reopen generations, PR attachment and shared-tracker references.
- Horizon ageing, stable pagination with conditional revalidation, and rate-limited pages that fail and then resume.
- Restart of rejected or stale generations.
- Webhook signatures and HTTP mapping, primary and secondary rate limits, ordinary 403s, token re-resolution after a 401, App token caching and refresh, bot-App writes, reactions, markers, and config validation.
- Git tokens minted for one repository with `contents: write`, reused, refused for other URLs, revoked, and refused on a token connection. The fake holds an installation token to what it was minted for, in its API and in its git smart-HTTP handler (`GitHandler`).
- Admission and input-selection rules, including `pull_requests` routes: claiming PR copies (drafts held until ready), leaving issues and their Work alone, and never delivering the Work's own comments.
- PR copies of every PR family, identical from push and reconcile, none of an issue's, and none without `track_pull_requests`; conflicts on a PR with no issue only when tracking.

The [SQLite integration cases](https://github.com/open-conduit/open-conduit/blob/main/integration/github_test.go) run the real runtime ingress and router, and cover:

- Reconcile-only recovery of missed webhooks (claim and skip rules, with the comment queued behind the issue).
- Webhook/reconcile overlap and redelivery deduplication. A close made while push was down is recovered, and a forged delivery is rejected.
- A rate-limited scan infers no absence, then recovers.
- Projection convergence, steady state and drift repair. Projection writes generate no input, and the status comes off while running.
- A lost comment response is recovered through its marker.
- A real SIGKILL after GitHub created the comment but before local completion. A restarted projector finds the marker and posts nothing twice.
- A reopened issue creates successor-generation Work while the terminal owner keeps its key.
- Acknowledgements of routed comments only.
- Restart in the middle of a generation.

[`TestGitHubPullRequestWorkRunsAlongsideTheBuilder`](https://github.com/open-conduit/open-conduit/blob/main/integration/github_pull_work_test.go) runs a builder and a reviewer through the toolbelt and scheduler. The builder's issue Work opens a PR, and the PR's own Work is claimed for it on the reviewer's binding without touching the builder's key or input. The reviewer's review reaches the builder but doesn't wake the parked reviewer, a push wakes it, and the merge reaches both, which then complete. `TestGitHubReconcileClaimsMissedPullRequests` claims a PR opened while nothing was listening, and a draft only once it is ready.

[`TestDaemonGitHubReconcileToProjection`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_github_test.go) drives the composed `serve` daemon. The registered ingress reconciles the fake, the `github` route claims the issue and a shell attempt declares a status and comment. Labels and the marked comment converge, and doctor's `github` check is `ok`. A restart posts nothing again.

## Limitations

- **Projection volume.** The projector reads every Work in the store on each pass and keeps those in its scope and namespace. That has no high-volume claim.
- **Unenforced setting.** `settings.admission_limit` (the claimed-per-project cap) is validated but not enforced, and `config check` warns where a config sets it. A scope's `limits.concurrency` does cap how many of its issues have a turn running at once.
- **Registration lifetime.** Ingress registrations are startup-only (B05). A changed or removed entry, an entry moved to another connection, or its connection moved to another `api_url` reports `restart_required` until the daemon restarts. Other connection changes apply in place (see Configuration).
- **Pull request Work.** A PR reopened after its Work completed isn't claimed again: the completed Work keeps the PR's key. While a PR's Work waits, its own comments are kept as wait context, as an issue's agent's are, so the next pass sees them, though they never wake it.
- **Raw deliveries.** `github.delivery` events are non-re-derivable by declaration (`DeliveryLoss`): a raw delivery missed while push is unavailable is lost, though the state it carried is recovered by reconcile. Webhook jobs rely on it; the trade-off is set out under recoverability of trigger families in [scheduled jobs](../schedule-extension/).

## Related parts

- **Webhook exposure.** [Operator setup](../operator-setup/) covers the separate listener mounting `WebhookHandler` per entry, ngrok/cloudflared/static tunnels, `manage_webhook` (`Client.EnsureWebhook`), `serve --dry-run`, the manual claim (`github.claim.requested`), `Client.Verify` for doctor's `github_auth`, and `Annotate` for operator views.
- **Holds, waits and tools.** [Engineering tools, waits and playbooks](../engineering-tools/) covers:
  - the agent-facing operations, posting through `Projector.Deliver` durable intents (re-driven by `Converge`) and `CreateIssue`;
  - translated waits with dependency filters;
  - `issue:N:<state>` waits on the host's `core.execution.turn` Event;
  - the `github-hold` route and executor, with the `scheduled` label managed while held or waiting;
  - `github.checks.none` and `github.checks.missing`.

## History

:::note
These notes record how this part was built and the evidence gathered at each stage. They describe the code at the time; the sections above describe current behaviour.
:::

- **03-01** delivered the extension: configuration, client and transport, ingress and canonical identity, route policy, projections and marked comments, with the fixture and integration evidence listed under Verification. No real GitHub API was called; all evidence used fixtures, and real issues, real rate limits and real webhooks were left to the 03-06/03-07 gate.
- **03-03** added `WorkComments` for the Claude Code executor's handoff feedback. It was removed with the handoff experiment (#73).
- **03-04** added `base_branch`, `no_checks_minutes` and `github.checks.none`, the agent-facing tools, translated waits, `github.agent.turn` and the `github-hold` route.
- **03-05** delivered the webhook listener, tunnels, managed hooks, dry run, the manual claim, `Client.Verify` and `Annotate`. Until then the daemon ran reconcile-only.
- **03-06** added the Actions workflow-run fallback for check runs after a fine-grained token failed every reconcile on the real sandbox once a PR existed.
- On the real sandbox, the review GitHub wraps around inline comments, observed as a human comment, woke waiting agents in a loop. That is why a review without a body is never observed.
- At 03-01, which delivery families a webhook job may rely on was left to 05-01. The [scheduled jobs](../schedule-extension/) page records the outcome: jobs rely on `github.delivery` and keep its declared loss.
- **08-01** extended `no_checks_minutes` to the required-checks hold and added `github.checks.missing`.
- At delivery the projection volume was described as enough for the first deployment, and `settings.admission_limit` was an open item.
