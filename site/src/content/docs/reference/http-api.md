---
title: Management HTTP API
description: "The daemon's local management API: its socket and credential, envelopes, idempotency, IDs in paths, lists and every endpoint."
---

This page is generated from the management server's route table (`management.APIRoutes`) by `make docs-gen`; `make docs-check` fails when it falls behind. Edit the source, not this page.

The CLI, the terminal UI and the web view are clients of one local HTTP API that the daemon serves. A tool of your own can call it the same way. The [CLI reference](../cli/) covers the same operations as commands; the [architecture pages](../../architecture/config-management/) describe what each does.

## Reaching it

The API is served over a Unix socket that only its owner can open: `<state>/management.sock`, where `<state>` is `--data-dir`, else `$XDG_STATE_HOME/open-conduit`, else `~/.local/state/open-conduit`. `--socket` or `OPEN_CONDUIT_SOCKET` moves it. Every request carries an operator credential as `Authorization: Bearer <token>`. The owner's credential is the `token` field of `<state>/private/operator.token`, a private (0600) JSON file of `{"token_id", "token"}`. `open-conduit auth add NAME --role view|operate|admin --out FILE` issues one for another operator.

```sh
TOKEN=$(jq -r .token ~/.local/state/open-conduit/private/operator.token)
curl --unix-socket ~/.local/state/open-conduit/management.sock \
  -H "Authorization: Bearer $TOKEN" http://localhost/v1/health
```

The host name in the URL is not used. A dev server or proxy that forwards to the socket must add the header itself; never hand the token to a browser.

## Roles

Each operator has a role, checked where the request is served, before anything is read or written: `view` reads, `operate` also runs controls and sends Events, and `admin` also applies and exports configuration, stores and removes secrets, and issues credentials. A request the role doesn't allow is refused with `403 forbidden_role`. The table below gives each endpoint's least role. The `/v1/toolbelt` endpoints are not for operators: an execution reaches them with its own token, on the socket the host gives that execution, and an operator credential is refused there (`403 wrong_credential_class`). Everything an operator does is recorded as theirs (`operator:<name>`), from the credential, never from the request.

## Envelopes

A success is `{"data": …}`. A failure is `{"error": {"code", "message", "request_id", "details"?}}` with an HTTP status to match: `400 malformed` or `invalid_filter`, `401 unauthenticated`, `403 forbidden_role`, `404 not_found`, `409 revision_conflict`, `state_conflict` or `idempotency_conflict`, `422 invalid_request` or `invalid_config` (with `details`: each diagnostic's `path`, `code` and `message`), and `503 unavailable` (retry). Every response has an `X-Request-Id` header, the same as the error's `request_id`. Messages never carry a backend's or provider's own error text.

## Requests that change something

A `POST` or `PUT` takes a JSON body (`Content-Type: application/json`, at most 1 MiB) and no query string. Unknown, duplicate and `null` fields are refused as `400 malformed`. A `DELETE` needs no body.

- **Idempotency.** An `Idempotency-Key` header (1 to 128 printable ASCII characters) makes a request safe to retry: the same key with the same body, within 30 days, returns the first response (with an `Idempotency-Expires-At` header) rather than doing it again, and the same key with another body is `409 idempotency_conflict`. Where the table says *required*, a request without one is `400 idempotency_key_required`. A named operator's keys are their own: two operators' equal keys are two requests.
- **Revisions.** A control on Work, an execution or a scope takes an optional `expected_revision` (a decimal string, from the record's `revision`). Given, the control acts only while the record is still at that revision (`409 revision_conflict` otherwise); omitted, it acts on the current one.
- **Input to Work.** `POST /v1/events` with `scope_id`, a namespaced `type`, a `delivery_id` and optional `data` (`{"schema", "data"}`), `correlation_key`, `work_id` or `causation_id` sends an operator Event. The scope's routes decide where it goes, and an Event with `work_id` that no route takes is that Work's next input all the same, so Work made with `POST /v1/work` (`scope_id`, `kind`, `binding`, optional `context`) takes follow-up input this way. See [routing](../../architecture/routing-lifecycle/).
- **Secrets.** `PUT /v1/secrets/{name}` with `{"value": "…"}` stores or replaces a value in the daemon's secret store and answers with the `reference` a config reads it with in `secret_refs` (`keyring:NAME` or `file:NAME`). It goes in the platform keyring, or the owner-only secrets file beside the config when no keyring is usable (`fell_back`); `"store": "keyring"` or `"file"` picks one. `GET /v1/secrets` lists each name with its `store`, `reference`, whether it `resolves` now (else its `failure` code) and the `referenced_by` pointers in the active config that read it. It knows the secrets file's names, the names stored in the keyring through the API, and every keyring or file reference the active config makes; a keyring entry stored only by `open-conduit secret set` and not referenced is not listed. `DELETE /v1/secrets/{name}` removes the name from both stores, and is refused with `409 secret_in_use` (each referencing pointer in `details`) while the active config reads it. A value is never logged, kept for replay or returned, so these routes take no `Idempotency-Key`; each call is recorded by name in the daemon's log as an `operator.*` record ([observability](../../architecture/observability-history/)).

## IDs in paths

IDs are opaque and often contain `/` (`manual/…`, `work/…`, `manual/…/attempt/1`). Percent-encode the whole ID as one path segment: `GET /v1/work/manual%2F3f2a…`. An unencoded `/` makes another path, usually `404 not_found`.

## Lists

A list returns `{"items": […], "next_cursor": …}` (the Work list adds `total`). `limit` is 1 to 200 (50 by default). Pass `next_cursor` back as `cursor`, with the same filters, for the next page, until it is empty. A cursor is opaque, belongs to its list and filters, and expires after 15 minutes (`410 cursor_expired`: start again). Each filter is given at most once; an unknown or repeated one is `400 invalid_filter`. `with` adds to each item what its own endpoint returns: `GET /v1/work?with=actions,panels,runs` (any of the three; `limit` at most 50 with it) and `GET /v1/events?with=route`.

## Endpoints

| Method | Path | Role | Idempotency-Key | Filters |
| --- | --- | --- | --- | --- |
| `GET` | `/v1/health` | view | – | – |
| `GET` | `/v1/capacity` | view | – | – |
| `GET` | `/v1/schema` | view | – | – |
| `GET` | `/v1/scopes` | view | – | `parent_id`, `enabled` |
| `GET` | `/v1/scopes/{id}` | view | – | – |
| `GET` | `/v1/work` | view | – | `scope_id`, `state`, `kind`, `parent_id`, `source`, `executor`, `updated_after`, `q`, `sort`, `with` |
| `GET` | `/v1/work/facets` | view | – | `scope_id`, `state`, `kind` |
| `GET` | `/v1/work/{id}` | view | – | – |
| `GET` | `/v1/work/{id}/actions` | view | – | – |
| `GET` | `/v1/scopes/{id}/actions` | view | – | – |
| `GET` | `/v1/work/{id}/panels` | view | – | – |
| `GET` | `/v1/work/{id}/config-candidate` | view | – | – |
| `GET` | `/v1/pages` | view | – | – |
| `GET` | `/v1/extensions` | view | – | – |
| `GET` | `/v1/scopes/{id}/panels` | view | – | – |
| `GET` | `/v1/scopes/{id}/route/samples` | view | – | – |
| `GET` | `/v1/events` | view | – | `scope_id`, `work_id`, `type`, `after_sequence`, `with` |
| `GET` | `/v1/events/{id}` | view | – | – |
| `GET` | `/v1/events/{id}/route` | view | – | – |
| `GET` | `/v1/executions` | view | – | `scope_id`, `work_id`, `state` |
| `GET` | `/v1/executions/{id}` | view | – | – |
| `GET` | `/v1/waits` | view | – | `scope_id`, `work_id`, `execution_id`, `state` |
| `GET` | `/v1/waits/{id}` | view | – | – |
| `GET` | `/v1/runs` | view | – | `scope_id`, `work_id`, `state`, `executor_binding`, `executor`, `work_kind`, `outcome` |
| `GET` | `/v1/runs/{id}` | view | – | – |
| `GET` | `/v1/runs/{id}/live` | view | – | – |
| `GET` | `/v1/doctor` | view | – | – |
| `GET` | `/v1/config` | view | – | – |
| `GET` | `/v1/config/export` | admin | – | – |
| `GET` | `/v1/artifacts` | view | – | `work_id`, `execution_id`, `kind` |
| `GET` | `/v1/artifacts/{id}/content` | view | – | – |
| `POST` | `/v1/events` | operate | required | – |
| `POST` | `/v1/work` | operate | required | – |
| `POST` | `/v1/work/{id}/cancel` | operate | required | – |
| `POST` | `/v1/work/{id}/retry` | operate | required | – |
| `POST` | `/v1/work/{id}/wake` | operate | required | – |
| `POST` | `/v1/work/{id}/rebind` | operate | required | – |
| `POST` | `/v1/work/{id}/hold` | operate | required | – |
| `POST` | `/v1/work/{id}/interrupt` | operate | required | – |
| `POST` | `/v1/work/{id}/release` | operate | required | – |
| `POST` | `/v1/work/{id}/attach` | operate | required | – |
| `POST` | `/v1/work/{id}/container-attach` | operate | required | – |
| `POST` | `/v1/work/{id}/container-detach` | operate | required | – |
| `POST` | `/v1/work/{id}/actions/{provider}/{action}` | operate | required | – |
| `POST` | `/v1/scopes/{id}/reconcile` | operate | required | – |
| `POST` | `/v1/scopes/{id}/route/trace` | view | optional | – |
| `POST` | `/v1/scopes/{id}/actions/{provider}/{action}` | operate | required | – |
| `POST` | `/v1/executions/{id}/cancel` | operate | required | – |
| `POST` | `/v1/events/{id}/skip` | operate | required | – |
| `POST` | `/v1/events/{id}/reroute` | operate | required | – |
| `POST` | `/v1/config/validate` | view | optional | – |
| `POST` | `/v1/config/apply` | admin | required | – |
| `GET` | `/v1/secrets` | admin | – | – |
| `PUT` | `/v1/secrets/{id}` | admin | – | – |
| `DELETE` | `/v1/secrets/{id}` | admin | – | – |
| `POST` | `/v1/auth/rotate` | operate | optional | – |
| `GET` | `/v1/auth/operators` | view | – | – |
| `POST` | `/v1/auth/operators` | admin | optional | – |
| `POST` | `/v1/auth/revoke` | operate | required | – |
| `POST` | `/v1/toolbelt/call` | execution | optional | – |
| `GET` | `/v1/toolbelt/guidance` | execution | – | – |
| `GET` | `/v1/toolbelt/serving` | execution | – | – |
