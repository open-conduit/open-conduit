---
title: "GitHub ingress, routing policy and projections"
description: "How the GitHub extension observes issues, pull requests, comments and checks, claims issues and pull requests as Work and projects status back to GitHub."
---

The first-party GitHub extension observes a repository's issues, pull requests, comments, reviews and checks, claims admitted issues (and, when asked, each open PR) as Work, and projects each Work's status back to GitHub as labels, marked comments and reactions. It lives in [`extensions/github`](https://github.com/open-conduit/open-conduit/tree/main/extensions/github) and is built against the public contracts: [B03 ingress](../extensions/), [B04 routing](../runtime-semantics/) and [B02 storage/KV](../domain-storage/). It imports only public packages, and the runtime does not import it. It is written on the public [`extension/inproc`](https://github.com/open-conduit/open-conduit/blob/main/extension/inproc/inproc.go) contract and runs as a package bundled with the release, served by `protocol/sdk/inprocsdk` (`extensions/github/pkg`, `open-conduit extension-run github`): its config types, entries and their ingress, webhook routes, projection, `manage_webhook` re-pointing, tools, actions, annotations, rendering and doctor checks all reach the host through it ([`host.go`](https://github.com/open-conduit/open-conduit/blob/main/extensions/github/host.go)). The tools ask the host's authorization (`Host.Authorize`) for cross-scope reads and filing, and its wait targets (`Host.WaitTarget`) for `job:` conditions, and are told when a container segment ends (`inproc.SegmentEnder`) to revoke its git tokens.

The agent-facing tools, waits and scheduled holds built on this extension are described in [engineering tools, waits and playbooks](../engineering-tools/); the webhook listener, tunnels, doctor checks and operator controls in [operator setup](../operator-setup/).

## Configuration

A `github` connection holds non-secret config: `api_url` (default `https://api.github.com`, or an Enterprise `/api/v3`), `auth` (`token` by default, or `app` with `app_id`) and an optional `bot_app_id`. Credentials come only from the connection's `secret_refs`:

- `token` for token auth. The gh CLI login is `cmd:gh auth token`, so no separate `gh` mode exists.
- `app_private_key` for app auth.
- `bot_private_key` for the bot App.

A private key is the App's PEM. `parseKey` also accepts it hex-encoded, which is how macOS Keychain's `security find-generic-password -w` returns a multi-line value (so a `keyring:` reference reads it that way there), and with its line breaks written as `\n`, as an environment variable often holds it. Setting up a bot App is a [guide](../../guides/github-bot/). A binding whose `connection` is a second `github` connection writes as it, so a reviewing agent writes as its own App ([a reviewer's own writer](../engineering-tools/#tool-operations)).

Secrets are resolved lazily before a call, cached for the credential's lifetime and resolved again after a 401. A rotated reference therefore takes effect without a restart. Credential failures return `ErrCredential` and never include the reference or its value.

An applied change to the connection a registered entry uses (`auth`, `app_id`, `bot_app_id`, `git` or its `secret_refs`) also takes effect without a restart: the extension's configure calls `Client.Reconfigure`, which swaps the client's reader, writer and git setting and drops its cached tokens, so the next write goes out as the new identity. A change of `api_url` can't be applied in place and reports `restart_required` for the entry.

A `github` extension entry (an ingress entry: `config.scope` must be the containing scope) has these fields:

| Field | Meaning |
| --- | --- |
| `namespace` | Stable source identity (for example `github/github.com/owner/repo`). Also the correlation-key source. |
| `repo` | The repository (`owner/name`). |
| `tracker` | Optional shared tracker `{repo, namespace}` whose issues this repository's PRs reference as `owner/repo#N`. |
| `track_issues` | Default true. False observes only PRs, comments, reviews and checks. |
| `labels` | `claimed`, `scheduled`, `help`, `review`, `done`, `skip[]`, `require[]`. |
| `closed_horizon_hours` | Default 1. How long after it closes an issue or PR stays observed by reconcile. |
| `branch_pattern` | Default `^agent/issue-(\d+)$`. `""` disables branch correlation. |
| `preview_pattern` | Default `deploy-preview`. |
| `projection` | `disabled`, `verify_seconds` (default 900) and `dry_run`. |
| `webhook_secret` | Names a selected scope secret. |
| `base_branch` | The branch PRs target (default: the repository's). |
| `no_checks_minutes` | Default 10; `-1` disables. How long a PR head with no checks waits before `github.checks.none`, and how long a head without a check the base branch requires is held before `github.checks.missing`. With `-1` such a head stays pending. |
| `track_pull_requests` | Default false. Also observes everything about each PR (its state, comments, reviews, checks and conflicts) under the PR's own key, for a route that [claims pull requests](#pull-request-work). |
| `acknowledge_issues` | Default false. Also observes the opening of each new issue under its own key, for a route that [acknowledges issues](#acknowledgement-work) from authors `admit` doesn't admit. Needs `track_issues`. |
| `admit` | Who may start Work. `authors[]`: the GitHub author associations admitted (`OWNER`, `MEMBER`, `COLLABORATOR`, `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `FIRST_TIMER`, `MANNEQUIN`, `NONE`, case-insensitive), or `"*"` for anyone; default `OWNER`, `MEMBER`, `COLLABORATOR`; an empty list is refused. `forks`: default false; true lets a `pull_requests` route claim PRs from forks. |

The entry also needs a `connection`. A route uses handler `github`, with optional config `{"claim": "issues"}` (the default), `{"claim": "pull_requests"}`, which claims [pull request Work](#pull-request-work), or `{"claim": "acknowledgements"}`, which claims [acknowledgement Work](#acknowledgement-work); `"drafts": true` lets a `pull_requests` route claim draft PRs as well, and `"authors": ["dependabot[bot]", "renovate[bot]"]` limits it to PRs those logins opened (compared without case), so a PR by anyone else falls through to the next route. A login the route names is admitted by name in place of `admit.authors` (a bot's association is `NONE`); `admit.forks` still holds.

## Client and transport

The client enforces these rules:

- Every API GET is conditional. A cached ETag makes an unchanged poll a 304 that does not count against the rate limit.
- A primary limit (`X-RateLimit-Remaining: 0` with its reset) or a secondary limit (`Retry-After`, or a "rate limit" message, which waits a minute) pauses that credential. The credential is identified by a digest only. Its calls then fail locally with `RateLimitError` until the limit lifts. A 403 with neither signal stays an ordinary error.
- Lists follow `Link: rel="next"`. An unfinished listing is an error, never a short success.
- App auth signs an RS256 JWT, looks up the installation and caches installation tokens until five minutes before they expire. A 401 invalidates the cached token and retries once.
- Writes (labels, comments, reactions) use the bot App when one is configured. Reads use the sign-in identity.
- `GitToken` mints a token for git that is never cached: an installation token limited to one repository with `contents: write`, plus each further permission it is asked for (what the execution's granted operations need, [`github.git.credential`](../engineering-tools/#tool-operations)) that the App's installation holds on the repository, from the bot App or else the sign-in App. It returns the token only once GitHub's git service accepts it for push: it asks for the receive-pack advertisement (`GET <web>/<repo>.git/info/refs?service=git-receive-pack`, the first request of `git push`, with the token as git sends it), and takes 401, 403 or 404 as the token still propagating, asking again after 0.25, 0.5, 1, 2, 4 and 8 seconds (`gitAcceptDelays`). A new installation token can be refused for push for a few seconds after GitHub mints it (`Permission to <repo>.git denied`), and git would take that as the push's answer. One still refused after the last is revoked and is `ErrGitTokenUnaccepted`; any other answer, or none, hands the token out as minted. Its caller revokes it with `RevokeToken` (`DELETE /installation/token`). On a token connection it is `ErrStaticToken` ([`github.git.credential`](../engineering-tools/#tool-operations)).
- `GitIdentity` is that same App's bot user as git's author and committer (`GET /app` with the App's JWT for its slug, then `GET /users/<slug>[bot]` for the user id), looked up once; `ErrStaticToken` on a token connection.

## Git as the bot

A connection's `git` is who git works as in the runs of its repositories' Work: `owner` (the default) leaves the operator's own credentials and identity, and `bot` works as the App. `bot` needs an App (`auth` `app` or a `bot_app_id`); the connection validator refuses it on a token connection without one.

The extension does this through the generic run environment hook (`extension.RunEnvironment`, protocol `run.environment`; see [extension protocol](../extension-protocol/)), with nothing in an executor or the core that knows about GitHub. `Tools.RunEnvironment` binds the run's Work as a toolbelt call would (its issue's repository, else the scope's only one). The connection that decides is the binding's own `github` connection (`RunEnvironmentRequest.Connection`, built by `Writers` as for a binding's writes) when it names one, else the repository's entry connection; a binding naming none, the entry's, or one of another type uses the entry's. When that connection has `git` `bot`, it returns this layer:

- `credential.helper` set empty, then to `!os git-credential`, through `GIT_CONFIG_*`. The empty entry clears the helpers git would otherwise ask first (the operator's keychain or `gh`), so pushes go only through the operations serving git credentials that the binding is granted: for GitHub, [`github.git.credential`](../engineering-tools/#tool-operations).
- `commit.gpgsign` and `tag.gpgsign` off, because the operator's key isn't the bot's.
- `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` set to `GitIdentity`.

Every other run of a Work the extension can bind (`git` `owner`, including one whose binding's own connection leaves `git` unset while the entry's sets `bot`) gets only a credential helper for the connection's web host, `credential.https://<host>.helper` set to `TokenHelper`: it answers git's `get` with the run's `GH_TOKEN`, else `GITHUB_TOKEN` (a binding's personal access token), and says nothing when it has neither, so git goes on to its next helper. Git asks the operator's own helpers first, so on the host it is a fallback; in a container, which has none, it is how a static token reaches the repository. The Claude Code executor used to set this helper itself; it now names no code host. A run the extension can't bind gets nothing. [`github.git.credential`](../engineering-tools/#tool-operations) mints from the same connection, taken from the call's `Authority.Connection`, so the token and the identity come from the same App. A failed identity lookup, or a binding connection `Writers` can't build (another `api_url`), fails the run before it starts (`run environment unavailable`). Doctor's `github-git-bot` check warns when a git workspace binding in such a scope isn't granted `github.git.credential`, so every push is refused, or pushes over SSH, which skips the helper. `init github` grants it to every binding it writes with a git workspace, through the `@github.workspace` [toolset](../toolbelt-management/#toolsets).

## Ingress and canonical identity

`Spec` declares one recoverable family per observed state. Only `github.delivery`, the raw webhook delivery (the trigger input for webhook jobs), is non-re-derivable, and it declares its loss limitation.

| Families | Item | Correlation |
| --- | --- | --- |
| `github.issue.open`, `github.issue.closed` | `issue/N` | `issue/N`, or `issue/N/generation/reopen-<event id>` after a reopening |
| `github.pull_request.open`, `.closed`, `.merged` | `pr/N` | The tracker issue the PR belongs to, else `pr/N` |
| `github.comment.human`, `.bot`, `.agent` (conversation, inline and review; `kind` in the payload) | `issue/N/comment/ID`, `pr/N/comment/ID`, `pr/N/review_comment/ID`, `pr/N/review/ID` | Their issue or PR's key |
| `github.checks.pending`, `.passed`, `.failed`, `.none`, `.missing` | `pr/N/checks` | The PR's key |
| `github.pull_request.conflicts` | `pr/N/conflicts` | The tracker issue the PR belongs to, else `pr/N` (a PR with no issue is observed only with `track_pull_requests`) |
| Issue opening copies (`acknowledge_issues`): `github.issue.open` | `issue_ack/N` | `issue/N/ack` |
| PR copies (`track_pull_requests`): every PR family above | `pull_request/N`, `pull_request/N/comment/ID`, `pull_request/N/review/ID`, `pull_request/N/review_comment/ID`, `pull_request/N/checks`, `pull_request/N/conflicts` | `pull_request/N` |

Push and reconcile share one normalizer. The observation version hashes the whole normalized envelope (type, key, occurrence time, payload). Equal external state from either transport therefore has the same version and delivery ID (`item@version`), and B02 deduplication keeps the first accepted envelope. Any difference is a new version, so the runtime's canonical-identity check can never wedge a source. Types name observed state, not webhook actions, because reconcile sees state. Author class is in the type, so a Wait can filter out agent and bot notes without a payload predicate.

Issue and PR payloads carry the author's `author_association` and `by_connection`, which admission reads, and `author_permission` when the association alone wouldn't admit a person (not a bot) to an open issue or PR but `COLLABORATOR` would; a PR's also carries `author_type` and `fork`, true when its head is in another repository or its head repository is gone (GitHub reports a deleted fork's head repository as null). Adding them changed every issue and PR observation's identity once, on upgrade: existing Work skips a repeated `github.issue.open`, and a PR's own Work hears its PR once as changed.

The issue payload deliberately omits `updated_at`, comment counts and the projected status labels. Comments and status projection therefore never create issue observations, and projection cannot feed back into itself. The claim label is included, because admission reads it. Lifecycle recurrences stay distinct because `closed_at` and the reopen generation change with each close and reopening. This is a deliberate application of B03's recurrence rule. Comments, reviews, PR heads and check runs carry their own occurrence identity (`updated_at`, head SHA, run IDs).

An inline comment is observed where it was made: its `commit_id` and `line` are GitHub's `original_commit_id` and `original_line`. GitHub moves `commit_id` and `line` to follow the PR's head, so a push would otherwise be a new version of an unchanged comment and hand it to the Work as news again. A reply in its thread is its own item and leaves the comment's version alone. Adding this changed the identity of every inline comment observed after a push once, on upgrade: Work still open then hears each such comment one last time. An edit that bumps `updated_at` is still a new version; routes skip it as not news.

A review payload carries no `commit_id`. GitHub moves a review's `commit_id` to the PR's new head when a force-push drops the commit it was submitted on, and a review has no `original_commit_id`, so a force-push would otherwise be a new version of an unchanged review and hand it to the Work as news again. Dropping it changed the identity of every review once, on upgrade: Work still open then hears each of its PR's reviews one last time.

**Reopen generations (B04).** An issue's generation is the latest `reopened` event at or before the observation time: the issue's current state, a comment's creation time, or the creation time of the PR that references it. This depends only on immutable history, so push and reconcile agree. An open issue whose `state_reason` is not `reopened` needs no history read. Old keys stay with the terminal owner. The first observation of a new generation creates successor Work, and its payload names the previous generation as `predecessor` (opaque lineage). A reopened issue always starts a new generation. If the previous generation's Work is still active, it receives the close as input and winds down. PR keys are not generation-qualified: a PR correlates with the issue generation current when it was created.

**PR attachment.** A PR, and every comment, review and check on it, is keyed to its tracker issue. The issue is found from the branch pattern first, then from a closing or reference keyword in the PR body. A shared tracker accepts only `owner/repo#N` naming it. Deriving the key deterministically means no route has to attach keys, and PR state changes reach the issue's Work directly. A reference to a missing issue falls back to `pr/N`.

**PR copies.** With `track_pull_requests`, every observation of a PR (its state, comments, inline comments, reviews, checks and conflicts: every item under `pr/N`) is emitted twice: once under the key above, and once more, with the same type and payload, under the PR's own key `pull_request/N`, with the item's `pr/` replaced by `pull_request/`. The copy is built from the original, so push and reconcile agree on it in the same way. An issue's Work owns the issue key and the PR's Work owns the PR's key, so each hears the PR's news without either taking a key from the other. Every PR family is copied alike, so the extension never decides which of a PR's events matter to whoever consumes them; nothing of an issue's is copied. Copies sort after every original, and a PR's copy before its comments' copies.

**Issue opening copies.** With `acknowledge_issues`, the first open generation of each issue is emitted a second time, as `github.issue.open` under the key `issue/N/ack` (item `issue_ack/N`). The copy carries only what the opening fixed (the repository, number, title, URL, creation time and the author with their association, permission and `by_connection`), so a label, body or assignee edit is the same version and nothing new; push and reconcile agree on it as on the original. A reopening isn't copied, so a reopened issue isn't acknowledged again. Two things are never copied: an issue opened more than 72 hours (`AckWindow`) before it was observed, so turning acknowledgements on, or a reconcile after a long outage, doesn't thank the authors of old issues; and an opening whose author's permission read failed, so a private org member isn't taken for an outsider. The next observation, once the read succeeds, copies it.

**Push.** `VerifyDelivery` checks `X-Hub-Signature-256` (HMAC-SHA256, constant time; an empty secret never verifies) before parsing. It wraps GitHub's event name and delivery GUID, which travel in headers, into the Delivery body (`application/vnd.open-conduit.github-delivery+json`). `WebhookHandler` maps outcomes to HTTP statuses: 202 only after the runtime durably accepted the delivery, 401 for a bad signature and 503 when the delivery was not accepted. The listener and tunnel that expose it are described in [operator setup](../operator-setup/#webhooks-tunnels-and-reconcile-only-operation). Without them the daemon runs reconcile-only, which loses only latency.

Push makes read-only service calls for generation history, PR details and aggregate checks, and no coordination writes. When check runs are refused (a fine-grained token cannot hold Checks), the aggregate uses the head's Actions workflow runs, else statuses only (see [engineering tools](../engineering-tools/#checks-timing)). A review without a body is never observed. GitHub wraps every inline comment and reply in one, the agent's own included, so observing it as a human comment would wake waiting agents in a loop. Check events re-read the aggregate state of each open PR whose head is the delivery's commit, exactly as reconcile evaluates it. A status event finds its PRs through `commits/{sha}/pulls`.

**Reconcile.** GitHub has no snapshot token. A generation lists the open issues, then the closed issues, then the same for PRs, and then the repository's conversation and inline comments updated within the horizon, so its cost follows the open items and recent activity rather than the repository's history:

- **Open items** are listed oldest first. One opened during the scan lands after the cursor, one closed during it is picked up by the closed listing, and a deletion can only skip one.
- **Closed items** are listed most recently updated first and observed only if they closed within the horizon. The listing stops at the first item last updated before the horizon (an item closes no later than its last update); GitHub filters the issue listing by `since` as well. The horizon runs from `closed_at`, so a comment on a long-closed item doesn't bring it back.
- **A close during the scan.** A closed item the open listing already observed in this generation is left to the next one, so no item has two versions in one generation (the runtime refuses that).
- **The horizon** is `closed_horizon_hours` before the generation starts, or, when the last completed generation began earlier than that, that generation's start, at most 168 hours back. So steady 15-minute scans look back an hour, and a stretch of failed scans loses no close made meanwhile. The generation ID (`gen-<start>-<horizon>`) carries the horizon, so every page reads the same window; a generation or cursor from an earlier version restarts.

Reviews and checks are read for each listed PR. Each open PR that belongs to an issue (every open PR, with `track_pull_requests`) is also read on its own, because only that read carries `mergeable` and GitHub sends no webhook when a PR picks up conflicts (the base branch moving under it). When GitHub says it cannot merge, the PR is `github.pull_request.conflicts`. The payload names the head commit and the base branch but not the base's commit, and the occurrence is the head's commit time, so one conflicting head is one observation however often the base moves after it; a new head that still conflicts is a new one. A `pull_request` delivery that already carries `mergeable: false` observes the same. A PR that is only behind its base is not observed.

Each `Reconcile` call is one bounded page. The cursor is `phase:page:offset` (phases `open_issues`, `closed_issues`, `open_pulls`, `closed_pulls`, `comments`, `review_comments`), and a call stops after 25 detail reads or 10 seconds. `Complete` certifies that every listing was read to its end in this generation. Rate limits and failed reads return errors, so no absence is inferred. A rate limit fails the page as `rate_limited` and an HTTP failure as `http_<status>`, each with GitHub's own message, so the host records the cause with the scan failure ([event ingress](../event-ingress/)); any other failure stays unauthored and its text goes to the extension's log.

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
  - Its author is admitted: the payload's `author_association` is one of `admit.authors`, its `author_permission` is `admin`, `maintain` or `write` and `admit.authors` names `COLLABORATOR`, or the payload's `by_connection` is true. GitHub reports a member whose org membership is private (its default) as `CONTRIBUTOR` to an App, so the normalizer reads such an author's repository permission (`GET /repos/{repo}/collaborators/{login}/permission`, which metadata read allows), for an open issue or PR, once per login per push or scan, each read spent from the scan page's budget; a read that fails leaves `author_permission` out, failing closed. That is how an agent's `github.issue.create` follow-ups are admitted: the normalizer sets it when a `Bot` author's login is the bot this connection's writes go out as (`Client.WriterBot`: the bot App, else the sign-in App, looked up once and cached per scan). Any other bot is refused, whatever its body says, because Dependabot release notes or a workflow posting as `github-actions[bot]` carry outside text; a token connection has no bot of its own, and its token's user normally passes `admit.authors`. An identity that can't be resolved leaves `by_connection` false, failing closed until a later observation resolves it. A missing association is refused. The labels are checked first, so a trace names a skip label before the author.
  - Scheduled issues are claimed by the [`github-hold` route](../engineering-tools/#scheduled-holds), never by `github`.
- **Existing Work** receives these as input: new human comments, reviews with a body, other Works' agent comments, closes, merges, completed checks (passed, failed, none and missing required checks) and merge conflicts.
- **Never delivered:** edits, unmarked bot comments, this Work's own notes, open-state updates, pending checks and absence. That holds while the Work waits: what doesn't wake a waiting Work reaches it as context at its next turn only when it is input this list would deliver.

Labels influence only this claim decision. Work lifecycle never reads them.

A route with `claim` `issues` never takes input for a PR's Work, so the two can share a scope in either order. Any `github` route takes the news of a PR that Work without an issue opened ([below](#prs-that-work-without-an-issue-opened)).

### Pull request Work

A route with `{"claim": "pull_requests"}` claims each open PR as its own `github.pull_request` Work, owning the PR's key `pull_request/N`. What that Work does is up to the route's binding and its playbook; a review bot is one use ([recipe](../../guides/github-bot/#a-review-bot-for-every-pr)). The route reads only PR copies, so it needs an entry with `track_pull_requests`; otherwise its trace reason says so.

- **Unowned PR copies** create Work for a `github.pull_request.open` copy of an open PR that isn't a draft (with `drafts`, a draft too), isn't from a fork (unless `admit.forks`) and, when the route lists `authors`, was opened by one of them, who is admitted by being named ([dependency upgrades](../../workflows/dependency-upgrades/) uses this for Dependabot and Renovate); otherwise its author is admitted by the same rule as an issue's. A draft is claimed when it is marked ready, because that is a new observation of the open PR. A PR that belongs to an issue another Work owns is claimed all the same: the keys differ.
- **A PR's Work** receives what an issue's Work hears of its PRs (the merge or close, completed checks, merge conflicts, new human comments and reviews with a body, and comments from other Works' agents), and new versions of the open PR (a push, or a change of title, body or draft state) as well. Its own comments, reviews and replies carry its Work in the marker and are never delivered, nor are edits, unmarked bot comments and pending checks.
- **Lifecycle.** Each pass is an Execution. Between passes the agent parks with `github.wait`, and needs no condition: what wakes an issue's agent (comments, check results, conflicts, the merge or close) and a push to its PR wake it, and another agent's comment wakes it through the same peer dependency an issue's agent has ([waits](../engineering-tools/#waits)). Its `ignore` drops news its role doesn't act on: a reviewer ignores `conflicts`, which are the author's to resolve, and re-reviews on the push that follows. Nothing is posted for the wait. The agent ends the Work when the PR merges or closes.

The agent reads which PR its Work is for from `github.context` (`pr`, `pr_url`, `repo`), and the PR operations default to its repository; it never sees the key. The projector leaves a PR's Work alone: it has no issue to label.

### PRs that Work without an issue opened

Work that owns no issue (a Slack thread, a job, child Work) can open a PR with `github.pr.create` ([code work without an issue](../engineering-tools/#code-work-without-an-issue)). The tools then accept `github.pull_request.owned` for that Work, and every `github` route, whatever it claims, handles it and the PR's news before its claim rules:

- **The attachment.** A `github.pull_request.owned` Event is matched only for the Work it names (`work_id`), only from the namespace its payload names and only when that entry is in the scope; an issue's, a PR's own and an acknowledgement's Work are skipped. The match attaches the PR's key `pr/N` with `deliver: none`, so the Event is in the Work's history without being input: the turn that opened the PR may still be running. The family is sealed, so no executor can emit it.
- **The news.** Work of a kind that isn't this extension's and that owns a `pr/N` key hears that PR's merge, close, conflicts, completed checks, new human comments and reviews with a body, and other agents' comments, as an issue's Work hears its PRs. Pushes and other open-state updates, edits and its own notes are not delivered. Matches use `deliver: current`, so the input is queued on the binding the Work runs on, not the route's: the agent that opened the PR answers, whichever route took the Event. The Work's display is left as it is.
- **What the scope needs.** A `github` entry for the repository (`track_issues: false` when the repository's issues aren't used) and at least one `github` route, so its Events are routed; the route's binding is not used for this news. A PR keyed to a tracker issue (from an `agent/issue-N` branch or a body that links the issue) belongs to that issue's Work instead, which is why `github.pr.create` refuses both for Work without an issue.

### Acknowledgement Work

A route with `{"claim": "acknowledgements"}` claims the opening copy of a new issue whose author `admit.authors` doesn't admit as `github.issue.ack` Work, owning `issue/N/ack` and never the issue's key `issue/N`. It is the inverse of an issues route's author rule; a recipe that thanks the author with a shell binding is in [acknowledge outside issues](../../workflows/acknowledge-issues/). The route reads only opening copies, so it needs an entry with `acknowledge_issues`; otherwise its trace reason says so.

- **Skipped, with a reason in the route trace:** an author `admit.authors` admits (by association, or by write permission with `COLLABORATOR`), so `"*"` acknowledges no one; any bot, this connection's own included; and any event that isn't an opening copy. Labels don't matter.
- **One per issue.** Once the Work exists it takes no input: a re-observation of the copy (redelivery, reconcile), and everything else about the issue, is skipped as already acknowledged. The completed Work keeps `issue/N/ack`, so it is never claimed again.
- **The issue stays free.** Issues and hold routes skip the copy, and the Work doesn't own `issue/N`, so the issue can be claimed by hand or by a widened `admit` afterwards. The projector reads only issue keys, so it leaves the issue's labels, reactions and notices alone for this Work.
- **Tools.** `github.issue.view` and `github.issue.comment` default to the issue, and `github.context` returns `issue`, `issue_url` and `acknowledgement: true`. Operations that do an issue's work (PRs, uploads, holds) are refused with `acknowledgement`. A comment carries the Work's marker, so run history links the issue's Event, the Work, its Execution and the comment.

### Which scope routes an entry's events

The runtime routes an Event in the scope of the entry that observed it, using that scope's routes and the routes it inherits from its ancestors. The handler also requires the entry to belong to that scope. So a route in another scope, including a child scope from an [included folder](../config-management/#included-folders), never sees a repository whose entry lives in the operator's main scope; inheritance runs only from parent to child. To claim such a repository's PRs from its own scope, give that scope its own `github` entry for the repository: its own `namespace`, `track_issues: false` and `track_pull_requests: true`, beside its `pull_requests` route. It polls the repository a second time, which costs little because every GET is conditional, and it needs its own webhook or runs reconcile-only. The simpler choice, when the binding may live in the repository's scope, is a second route and binding there.

## Projections and marked comments

`Projector.Converge` (run by the daemon at most every five seconds) reads durable state: Work, attempts and outcomes in the extension's scope whose keys are in its namespace. When several generations share an issue, the live one wins.

**Labels** are level-triggered:

- `claimed` is present while Work owns the issue. It comes off once the Work completes, whether or not the issue is closed: nothing hears the issue any more, so the label would promise an agent who isn't there. The completed Work still owns the issue's generation, so admission never claims it again; a reopening starts a new generation, which is claimed again. Failed and cancelled Work keep it.
- No status label is set while an attempt runs.
- After an attempt, the status comes from an outcome declaring `core.turn.v1` `{status}`. Otherwise completed Work projects `done`, failed Work projects `help` and cancelled Work projects none.

The projector re-reads the issue and changes only managed labels. It reapplies them when desired state changes, and at `verify_seconds` for nonterminal Work, which repairs drift.

When Work completes and its issue is still open (its PR didn't close it), the projector posts one notice on the issue, as the durable intent `finished/<work id>`: the agent has finished, a comment there won't reach one, and closing and reopening the issue brings one back. A human comment on that issue is otherwise routed `terminal` and dropped, like any input for terminal Work.

**Comments** are not re-derivable. An outcome's `core.turn.v1` `{comment}` becomes an intent with the stable ID `outcome/<execution id>` and a durable KV record. The record moves through these states:

1. `pending`
2. `attempted`, recorded before the POST
3. `posted`

The body carries `<!-- open-conduit:agent work=<id> intent=<digest> -->`. If a post's result is ambiguous (a lost response, or a crash after GitHub created the comment), the record stays `attempted`. After a 30-second search delay, the projector finds the marker in the issue's comments and completes the record instead of posting again. A changed body under a reused ID is refused (`intent_conflict`).

**Reactions:** a `+1` acknowledges each human comment the router delivered to Work. Reactions are idempotent at GitHub. The cursor never passes an unrouted comment. It retries rate-limit and credential failures, and skips permanent refusals.

**Answers in comments:** a human comment or review routed to Work whose first line is `/<action>` or `/<action> <input>` (`/answer approve`) asks for that action on the Work. The projector hands it to `Host.Act` as the comment's author, `github:<login>`, keyed by the comment, and the action's provider decides whether that login may act: a `human` gate's responders, say. A taken action is acknowledged with `+1` and a refused one with `confused`; a `/word` nothing on the Work offers (`no_action`) is an ordinary comment. Edited comments ask for nothing. A failure that isn't a refusal leaves the comment for the next pass, which reuses its key.

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
- Horizon ageing from the close, listings that stop at the horizon and read no closed history, a horizon that reaches back over failed scans, a close during a scan observed once, earlier generations restarting, stable pagination with conditional revalidation, and rate-limited pages that fail and then resume.
- Restart of rejected or stale generations.
- Webhook signatures and HTTP mapping, primary and secondary rate limits, ordinary 403s, token re-resolution after a 401, App token caching and refresh, bot-App writes, reactions, markers, and config validation.
- Git tokens minted for one repository with `contents: write` and the write permissions the granted operations need, capped at the installation's (the fake refuses a token asking for more, as GitHub does), reused, refused for other URLs, revoked, and refused on a token connection. The fake holds an installation token to what it was minted for, in its API and in its git smart-HTTP handler (`GitHandler`).
- Admission and input-selection rules, including `pull_requests` routes: claiming PR copies (drafts held until ready), leaving issues and their Work alone, and never delivering the Work's own comments.
- PR copies of every PR family, identical from push and reconcile, none of an issue's, and none without `track_pull_requests`; conflicts on a PR with no issue only when tracking.
- Issue opening copies, identical from push and reconcile, unchanged by an edit, and none for a reopening, an old issue, an unread permission or without `acknowledge_issues`; `acknowledgements` routes claiming only outsiders' copies, once, and leaving the issue's key free.

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

[`TestGitHubPullRequestWorkRunsAlongsideTheBuilder`](https://github.com/open-conduit/open-conduit/blob/main/integration/github_pull_work_test.go) runs a builder and a reviewer through the toolbelt and scheduler. The builder's issue Work opens a PR, and the PR's own Work is claimed for it on the reviewer's binding without touching the builder's key or input. The reviewer reads the issue its PR closes by number (and is refused `number_required` without one). Its review reaches the builder but doesn't wake the parked reviewer, a push wakes it, and the merge reaches both, which then complete. `TestGitHubReconcileClaimsMissedPullRequests` claims a PR opened while nothing was listening, and a draft only once it is ready.

[`TestGitHubAcknowledgesOutsideAuthorsWithoutClaiming`](https://github.com/open-conduit/open-conduit/blob/main/integration/github_ack_work_test.go) delivers an outsider's issue: one acknowledgement Work comments once (a retried call included) and completes, the issue gets no label, redelivery and reconcile start nothing more, and a claim by hand then claims the issue. A trusted author's issue is claimed, not acknowledged, and the trace says why.

[`TestDaemonGitHubReconcileToProjection`](https://github.com/open-conduit/open-conduit/blob/main/integration/daemon_github_test.go) drives the composed `serve` daemon. The registered ingress reconciles the fake, the `github` route claims the issue and a shell attempt declares a status and comment. Labels and the marked comment converge, and doctor's `github` check is `ok`. A restart posts nothing again.

## Limitations

- **Projection volume.** The projector reads every Work in the store on each pass and keeps those in its scope and namespace. That has no high-volume claim.
- **Unenforced setting.** `settings.admission_limit` (the claimed-per-project cap) is validated but not enforced, and `config check` warns where a config sets it. A scope's `limits.concurrency` does cap how many of its issues have a turn running at once.
- **Registration lifetime.** Ingress registrations are startup-only (B05). A changed or removed entry, an entry moved to another connection, or its connection moved to another `api_url` reports `restart_required` until the daemon restarts. Other connection changes apply in place (see Configuration).
- **Pull request Work.** A PR reopened after its Work completed isn't claimed again: the completed Work keeps the PR's key. While a PR's Work waits, input its route passes on but that doesn't wake it is kept as wait context, as an issue's agent's is, so the next pass sees it; what the route skips, such as pending checks, is not.
- **Raw deliveries.** `github.delivery` events are non-re-derivable by declaration (`DeliveryLoss`): a raw delivery missed while push is unavailable is lost, though the state it carried is recovered by reconcile. Webhook jobs rely on it; the trade-off is set out under recoverability of trigger families in [scheduled jobs](../schedule-extension/).

## Related parts

- **Webhook exposure.** [Operator setup](../operator-setup/) covers the separate listener mounting `WebhookHandler` per entry, tunnels (ngrok, cloudflared or an installed package's) and a static `public_url`, `manage_webhook` (`Client.EnsureWebhook`), `serve --dry-run`, the manual claim (`github.claim.requested`), `Client.Verify` for doctor's `github-auth`, and `Annotate` for operator views.
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
- Issue #357 moved the extension's entries, ingress, webhooks, projection, `manage_webhook`, tools, actions and annotations off `internal/bootstrap/github.go` onto `extension/inproc`. Each entry's webhook route and hook moved from doctor `webhooks` to doctor `github`, and its reconcile state to `reconcile_lag`. A change to a connection's secret references, rather than its config or secret names, now takes effect at the client's next credential refresh instead of at once.
- Issue #428 moved it out of process. It runs as a package bundled with the release (`extensions/github/pkg`, `open-conduit extension-run github`), with its `github-hold` executor in the jobs pool, its setup probes offered through `probe.serve` and its `work-issue` playbook written by the host. Offline doctor runs its offline checks through the package. Its checks are `github`, `github-capability`, `github-git-bot` and `github-auth`.
- Issue #357 also packaged the extension as `packages/github`, served unchanged by `protocol/sdk/inprocsdk` and certified against `githubtest`. The `github-hold` executor now adopts an attempt it started (`Recover` returns its handle, deciding or decided, for the last 256), and an attempt with no toolbelt endpoint fails saying so rather than refusing to launch; it runs the shared executor suite.
- Issue #614 found the `github-hold` executor's `Recover` returned its handle without re-acknowledging its recovery state, so the scheduler failed every adoption (`invalid_state`) and a held issue whose lease lapsed on a slow host stalled. Its deciding attempt also kept the old epoch's credential, which recovery fences. `Recover` now re-acknowledges, hands the attempt the new credential and decides again a decision refused under the old one. The shared executor case now requires the re-acknowledgement (it only permitted it), so another executor can't repeat the gap.
- Issue #554 had `GitToken` wait until GitHub's git service accepts a token it has just minted. Before, the first push of a segment could be refused with `403 Permission to <repo>.git denied` and the same token accepted on a retry seconds later.
- Issue #524 had `github.git.credential` mint its token with the write permissions the execution's granted operations need (Issues for issue comments and new issues, Pull requests for the PR operations) as well as `contents: write`, capped at the installation's, and doctor's `github-auth` warn `git_permission_missing` when the installation lacks one. Before, the token had `contents: write` alone, so a closing keyword in a commit pushed straight to the default branch never closed its issue, and an agent that then closed it through the API was refused.
