---
title: Management HTTP API
description: "The daemon's local management API: its socket and credential, envelopes, idempotency, IDs in paths, lists and every endpoint."
---

This page is generated from the management server's route table (`management.APIRoutes`) by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.

The CLI, the terminal UI and the web view are clients of one local HTTP API that the daemon serves. A tool of your own can call it the same way. The [CLI reference](../cli/) covers the same operations as commands; the [architecture pages](../../architecture/config-management/) describe what each does.

## Reaching it

The API is served over a Unix socket that only its owner can open: `<state>/management.sock`, where `<state>` is `--data-dir`, else `$XDG_STATE_HOME/open-conduit`, else `~/.local/state/open-conduit`. `--socket` or `OPEN_CONDUIT_SOCKET` moves it. Every request carries an operator credential as `Authorization: Bearer <token>`. The owner's credential is the `token` field of `<state>/private/operator.token`, a private (0600) JSON file of `{"token_id", "token"}`. `open-conduit auth add NAME --role view|operate|configure|admin --out FILE` issues one for another operator; `--ttl 8h` (or `"ttl"` in `POST /v1/auth/operators`) makes it expire, after which it is `401 unauthenticated`. A trusted identity provider's token is accepted too: `PUT /v1/auth/issuers/{id}` (admin) trusts an issuer with its credential verifier `type` (such as `oidc`), that verifier's `config` and `groups`, a table of the issuer's groups to roles, and its tokens then act as `operator:<id>:<subject>` with the highest role their groups map to.

```sh
TOKEN=$(jq -r .token ~/.local/state/open-conduit/private/operator.token)
curl --unix-socket ~/.local/state/open-conduit/management.sock \
  -H "Authorization: Bearer $TOKEN" http://localhost/v1/health
```

The host name in the URL is not used. A dev server or proxy that forwards to the socket must add the header itself; never hand the token to a browser.

## Roles

Each operator has a role, checked where the request is served, before anything is read or written: `view` reads, `operate` also runs controls and sends Events, `configure` also applies the fragment of a submitted include, and `admin` also applies and exports the whole configuration, stores and removes secrets, resolves them on request, lists and issues credentials, and reads the audit ledger. A request the role doesn't allow is refused with `403 forbidden_role`. The table below gives each endpoint's least role. `GET /v1/doctor` needs `admin` when it asks for `check_secrets=true`, which resolves every secret reference the config makes (running each `cmd:` one and reading the keyring); without it, it reports the secrets check as `not_checked` and needs `view`.

A role can also be granted at a scope instead of daemon-wide: `--grants team-a=operate` on `auth add` (`"grants": [{"scope", "role"}]` in `POST /v1/auth/operators`), or `grants` rows of `{"group", "scope", "role"}` on a trusted issuer. A grant is `view`, `operate` or `configure` and covers its scope and every scope beneath it; `admin` is only ever daemon-wide. An operator may hold a daemon-wide role and grants together. The table's *Target* column says what each endpoint acts on, and so which scope a grant must cover. For a record named in the path (`scope`, `work`, `execution`, `event`, `wait`, `artifact`, or an `include`, whose scope is the parent the config file attaches it under) or the body's `scope_id`, the host resolves its scope before the handler runs and refuses one outside every grant with `403 forbidden_scope` (a record that doesn't exist is `404`). A `list` holds only what the operator's grants cover, whatever its filters ask for. A `daemon` endpoint needs its role daemon-wide, and `any` needs it anywhere. The `/v1/toolbelt` endpoints are not for operators: an execution reaches them with its own token, on the socket the host gives that execution, and an operator credential is refused there (`403 wrong_credential_class`). Everything an operator does is recorded as theirs (`operator:<name>`), from the credential, never from the request.

## Acting for someone

A client that serves many people, such as a company's own UI or the web view behind a sign-in proxy, can hold one credential and act for whoever is signed in to it. Signing them in stays the client's. Issue the credential with `--act-for KIND` on `auth add` (`"act_for"` in `POST /v1/auth/operators`) and name the person on each request in an `Open-Conduit-Subject: KIND:ID` header. The host checks the credential may act for that kind, records the person as the actor of every Event the request makes, and the credential as `via`, the client it came through. It authorises the request as both, and nothing a subject sends falls back to the client's own authority.

- **A person of the client's own kind** (`acme-ui:alice` from a credential with `act_for` `acme-ui`) holds no operator role or grants. They may invoke actions (`POST …/actions/{provider}/{action}`), which the provider authorises as it would any caller, so a `human` gate's `responders` can name them. Any other route is `403 subject_not_permitted`: the client reads and lists as itself, without the header. A kind an installed package acts for itself (`slack`, say) can't be taken (`409 act_for_taken`).
- **An operator**, from a credential trusted to assert operator identities (`act_for` `operator`), is named as `operator:NAME`, with the highest role and every grant of that operator's active credentials, or sends their own credential (one the host issued, or an identity provider's token) in `Open-Conduit-Subject-Token` in place of a name. They may do what both they and the client may: the role is the lower of the two, and a grant reaches only scopes both cover. The owner's own `operator:local` is never a subject, and an operator with no active credential, or a token that doesn't authenticate, is refused (`403 forbidden_subject`, `401 subject_unauthenticated`).

A credential without `act_for`, a kind other than its own, or an execution's token, naming a subject is `403 forbidden_subject`. Rotating or revoking the client's own credential takes no subject. A subject's `Idempotency-Key`s are its own through that client: two people's equal keys are two requests.

## Envelopes

A success is `{"data": …}`. A failure is `{"error": {"code", "message", "request_id", "details"?}}` with an HTTP status to match: `400 malformed` or `invalid_filter`, `401 unauthenticated`, `403 forbidden_role`, `forbidden_scope`, `forbidden_subject` or `subject_not_permitted`, `404 not_found`, `409 revision_conflict`, `state_conflict` or `idempotency_conflict`, `422 invalid_request` or `invalid_config` (with `details`: each diagnostic's `path`, `code` and `message`), and `503 unavailable` (retry). Every response has an `X-Request-Id` header, the same as the error's `request_id`. Messages never carry a backend's or provider's own error text.

## Requests that change something

A `POST` or `PUT` takes a JSON body (`Content-Type: application/json`, at most 1 MiB) and no query string. Unknown, duplicate and `null` fields are refused as `400 malformed`. A `DELETE` needs no body.

- **Idempotency.** An `Idempotency-Key` header (1 to 128 printable ASCII characters) makes a request safe to retry: the same key with the same body, within 30 days, returns the first response (with an `Idempotency-Expires-At` header) rather than doing it again, and the same key with another body is `409 idempotency_conflict`. Where the table says *required*, a request without one is `400 idempotency_key_required`. A named operator's keys are their own: two operators' equal keys are two requests.
- **Revisions.** A control on Work, an execution or a scope takes an optional `expected_revision` (a decimal string, from the record's `revision`). Given, the control acts only while the record is still at that revision (`409 revision_conflict` otherwise); omitted, it acts on the current one.
- **Input to Work.** `POST /v1/events` with `scope_id`, a namespaced `type`, a `delivery_id` and optional `data` (`{"schema", "data"}`), `correlation_key`, `work_id` or `causation_id` sends an operator Event. The scope's routes decide where it goes, and an Event with `work_id` that no route takes is that Work's next input all the same, so Work made with `POST /v1/work` (`scope_id`, `kind`, `binding`, optional `context`) takes follow-up input this way. Its `core.work.created` Event carries its `kind` and `context`, and a claude-code turn shows the context as the task the operator gave: its `task`, else its `body` or `text`, else its data. An agent's turn shows the Event's `data.data`: its `body` or `text` as plain text, otherwise its fields as compact JSON. A message sent while a turn runs reaches that turn when the binding injects. See [routing](../../architecture/routing-lifecycle/).
- **Submitted includes.** `GET /v1/config/includes/{name}` reads a submitted include the config file declares (`{"name", "parent"}` in its `includes`): its `parent`, its `fragment_text` and its `hash`. `POST /v1/config/includes/{name}/apply` with `fragment_text` (or `fragment` as JSON) and that `expected_hash` replaces the include's scopes and publishes a new generation, within the executors, extension types and secret names the include allows (`not_allowed` otherwise), or answers `409 fragment_conflict` when someone applied to the include first. Applies to two includes never conflict, and neither stales a `POST /v1/config/apply`. A diagnostic at `fragment/…` is in the fragment; one at `config/…` is in the rest of the configuration, which the fragment may still have caused (by clashing with a scope ID, say). See [submitted includes](../../architecture/config-management/#submitted-includes).
- **Secrets.** `PUT /v1/secrets/{name}` with `{"value": "…"}` stores or replaces a value in the daemon's secret store and answers with the `reference` a config reads it with in `secret_refs` (`keyring:NAME` or `file:NAME`). It goes in the platform keyring, or the owner-only secrets file beside the config when no keyring is usable (`fell_back`); `"store": "keyring"` or `"file"` picks one. `GET /v1/secrets` lists each name with its `store`, `reference`, whether it `resolves` now (else its `failure` code) and the `referenced_by` pointers in the active config that read it. It knows the secrets file's names, the names stored in the keyring through the API, and every keyring or file reference the active config makes; a keyring entry stored only by `open-conduit secret set` and not referenced is not listed. `DELETE /v1/secrets/{name}` removes the name from both stores, and is refused with `409 secret_in_use` (each referencing pointer in `details`) while the active config reads it. A value is never logged, kept for replay or returned, so these routes take no `Idempotency-Key`; each call is recorded by name in the daemon's log as an `operator.*` record ([observability](../../architecture/observability-history/)).

## Audit ledger

The daemon keeps a durable audit ledger of administrative calls: who made each, what it acted on and how it ended. Every call that changes something (`POST`, `PUT` and `DELETE`, except a route trace, config validate and an execution's toolbelt calls) is recorded with its outcome, `ok` or the error code it was refused with. So is every operator request refused for its role, scope or subject (`403`), reads included. A request refused for its credential (`401`) on any route, the toolbelt's included, is recorded as that refusal, up to 10 a minute for each client, execution socket and the owner's socket; a throttled request (`429`) is never recorded. A control or action is recorded with the Event it made, by that Event's type and ID. A config apply is recorded in the transaction that publishes its generation, and a credential issued, rotated or revoked in the transaction that changes it. A secret stored or a release installed is recorded once the call returns. Successful reads are not recorded.

`GET /v1/audit` (admin) reads it in sequence order: `after` a sequence (0 from the start) and `limit` (1 to 1000, 1000 by default) give `{"records": [{"sequence", "at", "actor"?, "via"?, "action", "target"?, "outcome", "event_id"?, "detail"?, "prev"?, "hash"}], "next_after"}`. Sequences are decimal strings and run 1, 2, 3 with no gaps. `action` is the route (`POST /v1/config/apply`) or the Event's type, `target` is what the call acted on (`work/ID`, `credential/TOKEN_ID`, `secrets/NAME`), and `detail` holds facts that are never secret, such as the generation an apply made, or the ID of a revoked credential someone tried. A record whose credential was refused has no `actor`. An exporter streams the ledger by asking again with `after` set to `next_after`, and resumes from the last sequence it holds after a restart. Each record's `prev` is the `hash` of the record before it, and its `hash` is a SHA-256 over every other field. A copy can then show a missing, reordered or rewritten record (`domain.VerifyAuditChain` checks a page).

## IDs in paths

IDs are opaque and often contain `/` (`manual/…`, `work/…`, `manual/…/attempt/1`). Percent-encode the whole ID as one path segment: `GET /v1/work/manual%2F3f2a…`. An unencoded `/` makes another path, usually `404 not_found`.

## Lists

A list returns `{"items": […], "next_cursor": …}` (the Work list adds `total`). `limit` is 1 to 200 (50 by default). Pass `next_cursor` back as `cursor`, with the same filters, for the next page, until it is empty. A cursor is opaque, belongs to its list and filters, and expires after 15 minutes (`410 cursor_expired`: start again). Each filter is given at most once; an unknown or repeated one is `400 invalid_filter`. `with` adds to each item what its own endpoint returns: `GET /v1/work?with=actions,panels,runs` (any of the three; `limit` at most 50 with it) and `GET /v1/events?with=route`.

To follow what runs use, read `GET /v1/events?type=core.execution.usage&after_sequence=N` and keep the last `sequence` you read. Unlike a `cursor`, that number never expires and survives a restart, and each record is read exactly once. Each record's `data` (schema `core.execution.usage.v1`) is what one landed result added: `work`, `execution`, `scope`, `binding`, `executor`, `segment`, `started_at`, `ended_at`, `container`, `amounts` by unit, and the executor's `attribution` (such as `model`). An attempt's records sum to its `usage`. A `cost` amount is the executor's own figure and may not be what anyone is billed.

## Endpoints

| Method | Path | Role | Target | Idempotency-Key | Filters |
| --- | --- | --- | --- | --- | --- |
| `GET` | `/v1/health` | view | any | – | – |
| `GET` | `/v1/capacity` | view | daemon | – | – |
| `GET` | `/v1/quiescence` | view | daemon | – | – |
| `GET` | `/v1/drain` | view | daemon | – | – |
| `GET` | `/v1/portability` | admin | daemon | – | – |
| `POST` | `/v1/drain` | operate | daemon | optional | – |
| `DELETE` | `/v1/drain` | operate | daemon | optional | – |
| `GET` | `/v1/schema` | view | any | – | – |
| `GET` | `/v1/scopes` | view | list | – | `parent_id`, `enabled` |
| `GET` | `/v1/scopes/{id}` | view | scope | – | – |
| `GET` | `/v1/work` | view | list | – | `scope_id`, `state`, `kind`, `parent_id`, `source`, `executor`, `updated_after`, `q`, `sort`, `with` |
| `GET` | `/v1/work/facets` | view | list | – | `scope_id`, `state`, `kind` |
| `GET` | `/v1/work/{id}` | view | work | – | – |
| `GET` | `/v1/work/{id}/actions` | view | work | – | – |
| `GET` | `/v1/scopes/{id}/actions` | view | scope | – | – |
| `GET` | `/v1/work/{id}/panels` | view | work | – | – |
| `GET` | `/v1/work/{id}/terminal` | operate | work | – | – |
| `GET` | `/v1/work/{id}/config-candidate` | view | daemon | – | – |
| `GET` | `/v1/pages` | view | daemon | – | – |
| `GET` | `/v1/extensions` | view | any | – | – |
| `GET` | `/v1/scopes/{id}/panels` | view | scope | – | – |
| `GET` | `/v1/scopes/{id}/route/samples` | view | scope | – | – |
| `GET` | `/v1/events` | view | list | – | `scope_id`, `work_id`, `type`, `after_sequence`, `with` |
| `GET` | `/v1/events/{id}` | view | event | – | – |
| `GET` | `/v1/events/{id}/route` | view | event | – | – |
| `GET` | `/v1/executions` | view | list | – | `scope_id`, `work_id`, `state` |
| `GET` | `/v1/executions/{id}` | view | execution | – | – |
| `GET` | `/v1/waits` | view | list | – | `scope_id`, `work_id`, `execution_id`, `state` |
| `GET` | `/v1/waits/{id}` | view | wait | – | – |
| `GET` | `/v1/runs` | view | list | – | `scope_id`, `work_id`, `state`, `executor_binding`, `executor`, `work_kind`, `outcome` |
| `GET` | `/v1/runs/{id}` | view | execution | – | – |
| `GET` | `/v1/runs/{id}/live` | view | execution | – | – |
| `GET` | `/v1/doctor` | view | daemon | – | – |
| `GET` | `/v1/config` | view | daemon | – | – |
| `GET` | `/v1/config/export` | admin | daemon | – | – |
| `GET` | `/v1/artifacts` | view | list | – | `work_id`, `execution_id`, `kind` |
| `GET` | `/v1/artifacts/{id}/content` | view | artifact | – | – |
| `POST` | `/v1/events` | operate | body scope_id | required | – |
| `POST` | `/v1/work` | operate | body scope_id | required | – |
| `POST` | `/v1/work/{id}/cancel` | operate | work | required | – |
| `POST` | `/v1/work/{id}/retry` | operate | work | required | – |
| `POST` | `/v1/work/{id}/wake` | operate | work | required | – |
| `POST` | `/v1/work/{id}/rebind` | operate | work | required | – |
| `POST` | `/v1/work/{id}/hold` | operate | work | required | – |
| `POST` | `/v1/work/{id}/interrupt` | operate | work | required | – |
| `POST` | `/v1/work/{id}/release` | operate | work | required | – |
| `POST` | `/v1/work/{id}/attach` | operate | work | required | – |
| `POST` | `/v1/work/{id}/container-attach` | operate | work | required | – |
| `POST` | `/v1/work/{id}/container-detach` | operate | work | required | – |
| `POST` | `/v1/work/{id}/actions/{provider}/{action}` | operate | work | required | – |
| `POST` | `/v1/scopes/{id}/reconcile` | operate | scope | required | – |
| `POST` | `/v1/scopes/{id}/route/trace` | view | scope | optional | – |
| `POST` | `/v1/scopes/{id}/actions/{provider}/{action}` | operate | scope | required | – |
| `POST` | `/v1/executions/{id}/cancel` | operate | execution | required | – |
| `POST` | `/v1/events/{id}/skip` | operate | event | required | – |
| `POST` | `/v1/events/{id}/reroute` | operate | event | required | – |
| `POST` | `/v1/config/validate` | view | daemon | optional | – |
| `POST` | `/v1/config/apply` | admin | daemon | required | – |
| `GET` | `/v1/config/includes/{id}` | view | include | – | – |
| `POST` | `/v1/config/includes/{id}/apply` | configure | include | required | – |
| `GET` | `/v1/audit` | admin | daemon | – | `after` |
| `GET` | `/v1/secrets` | admin | daemon | – | – |
| `PUT` | `/v1/secrets/{id}` | admin | daemon | – | – |
| `DELETE` | `/v1/secrets/{id}` | admin | daemon | – | – |
| `POST` | `/v1/releases/install` | admin | daemon | optional | – |
| `POST` | `/v1/auth/rotate` | operate | any | optional | – |
| `GET` | `/v1/auth/operators` | admin | daemon | – | – |
| `POST` | `/v1/auth/operators` | admin | daemon | optional | – |
| `POST` | `/v1/auth/revoke` | operate | any | required | – |
| `GET` | `/v1/auth/issuers` | view | daemon | – | – |
| `PUT` | `/v1/auth/issuers/{id}` | admin | daemon | optional | – |
| `DELETE` | `/v1/auth/issuers/{id}` | admin | daemon | optional | – |
| `POST` | `/v1/toolbelt/call` | execution | execution token | optional | – |
| `GET` | `/v1/toolbelt/guidance` | execution | execution token | – | – |
| `GET` | `/v1/toolbelt/serving` | execution | execution token | – | – |
| `POST` | `/v1/toolbelt/git` | execution | execution token | optional | – |
| `DELETE` | `/v1/toolbelt/git/{id}` | execution | execution token | optional | – |
| `GET` | `/v1/toolbelt/git/{id}/{id}` | execution | execution token | – | – |
| `POST` | `/v1/toolbelt/git/{id}/{id}` | execution | execution token | – | – |
