---
title: Incidents and errors
description: Turn a PagerDuty incident or a Sentry issue into triage Work that gathers context, files the fix and reports back, and completes when the incident resolves.
---

When something breaks in production, an agent can do the first pass of triage: read the alert and the stack trace, find the commit Sentry suspects, file an issue for the fix, and tell the responders what it found on the incident. Two installed packages bring the alerts in:

- **`pagerduty`** opens Work for each triggered incident. The incident's later status changes reach that Work, which completes once the incident resolves.
- **`sentry`** opens Work for each new or regressed Sentry issue, or when one of your issue alert rules fires. Alerts on an issue that already has Work go to that Work, so a burst of alerts is triaged once.

Each package checks its service's webhook signature and refuses anything it can't verify. It also reconciles with the service's API on an interval, so it catches up on anything a webhook missed. The agent's operations only act on the incident or issue its Work was opened for, and acknowledging an incident is off unless you turn it on.

The flow below starts from PagerDuty, the usual pager. To triage Sentry issues that never page anyone, add a `sentry` route as well.

```text
incident triggered → triage Work → GitHub issue → note on the incident → wait → incident resolved → completed
                       ↑ reads the alert, the Sentry issue and the suspect commit
```

## Install the packages

Both packages are written only on the extension SDK, like any third-party package. They aren't bundled in the release yet, so build them from a checkout and install them:

```sh
make packages
open-conduit extension install dist/packages/pagerduty
open-conduit extension install dist/packages/sentry
```

`make certify` runs the conformance suite over both, against their fakes. An install takes effect at the next `serve`.

## Set up the services

**PagerDuty.**

1. Create a REST API key (Integrations → API Access Keys). A read-only key is enough unless the agent adds notes.
2. Add a webhook subscription (Integrations → Generic Webhooks (v3)) for `incident.triggered`, `incident.acknowledged` and `incident.resolved`, scoped to the services you want triaged. Its URL is your webhook address followed by `/pagerduty/<scope>/<entry>`, here `/pagerduty/prod/oncall`. Keep the signing secret it shows you.
3. Pick the PagerDuty user the agent writes as, and use their email as the connection's `from`.

**Sentry.**

1. Create an internal integration (Settings → Developer Settings → Custom Integrations) with Issue & Event set to Read & Write. Its token is the connection's `auth_token`.
2. Give it a webhook URL, your webhook address followed by `/sentry/<scope>/<entry>` (here `/sentry/prod/errors`), and turn on the `issue` webhook. To have alert rules open Work, turn on **Alert Rule Action** and add the integration as an action on the rules you want.
3. Keep its client secret: it signs the webhooks.

Webhooks need a public address: see [webhooks](../../guides/configuration/#webhooks). Without one, leave out `webhook_secret` and the entries only reconcile, on `interval_seconds` (300 by default).

## Example config

These are the parts to add to a config that already has the [GitHub workflow](../github/) for `acme/web`. The connections go in the top-level `connections`:

```json
[
  {"id": "pagerduty", "type": "pagerduty", "config": {"from": "oncall-agent@acme.example"},
   "secret_refs": {"api_token": "keyring:pagerduty-api-key"}},
  {"id": "sentry", "type": "sentry", "config": {},
   "secret_refs": {"auth_token": "keyring:sentry-token"}}
]
```

Then a scope for production incidents:

```json
{
  "id": "prod",
  "name": "Production incidents",
  "secret_refs": {
    "pagerduty-webhook": "keyring:pagerduty-webhook-secret",
    "sentry-client-secret": "keyring:sentry-client-secret"
  },
  "authorization": {
    "connections": ["pagerduty", "sentry"],
    "operations": ["read_context", "wait", "declare_outcome",
                   "pagerduty.incident.view", "pagerduty.incident.note",
                   "sentry.issue.view", "sentry.issue.comment",
                   "github.issue.list", "github.issue.create"],
    "read_scopes": [],
    "file_scopes": ["web"]
  },
  "extensions": [
    {"name": "oncall", "type": "pagerduty", "connection": "pagerduty", "secret_names": ["pagerduty-webhook"],
     "config": {"scope": "prod", "services": ["PWEB123"], "webhook_secret": "pagerduty-webhook"}},
    {"name": "errors", "type": "sentry", "connection": "sentry", "secret_names": ["sentry-client-secret"],
     "config": {"scope": "prod", "org": "acme", "projects": ["web"], "webhook_secret": "sentry-client-secret"}}
  ],
  "routes": [
    {"name": "incidents", "handler": "pagerduty", "binding": "triage", "active_policy": "queue",
     "config": {"urgencies": ["high"]}}
  ],
  "bindings": [
    {"name": "triage", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
     "operations": ["read_context", "wait", "declare_outcome",
                    "pagerduty.incident.view", "pagerduty.incident.note",
                    "sentry.issue.view", "sentry.issue.comment",
                    "github.issue.list", "github.issue.create"],
     "secret_names": [], "config": {"playbook": "-", "structured": false, "dir": "/path/to/triage"}}
  ]
}
```

- The `pagerduty` route opens Work for each high-urgency incident on the listed services. Leave out `urgencies` for all of them.
- With the `sentry` entry in the scope, the triage agent can read the Sentry issue an alert names. To open Work for Sentry issues too, add a route with `"handler": "sentry"`. Its `triggers` choose what opens Work: `opened`, `regressed` and `alert` (all three by default), and `levels` limits it to issue levels such as `["error", "fatal"]`.
- `file_scopes` lets the agent file the fix as an issue in the `web` scope, where the GitHub agent picks it up and opens the pull request. Filing is all the triage agent does in the repository.

## The entries' config

| Field | Default | Meaning |
| --- | --- | --- |
| both: `scope` | (required) | The scope the entry is in. |
| `pagerduty`: `services` | all | Only incidents on these service IDs. |
| `pagerduty`: `allow_acknowledge` | `false` | Lets agents acknowledge their incident with `pagerduty.incident.acknowledge`. |
| `pagerduty`: `namespace` | `pagerduty` | The source's namespace; give each entry its own if one scope has several. |
| `sentry`: `org` | (required) | The organization slug. |
| `sentry`: `projects` | all | Only issues in these project slugs. |
| `sentry`: `namespace` | `sentry/<org>` | The source's namespace. |
| both: `webhook_secret` | none | The secret name holding the webhook signing secret (reconcile only without it). |
| both: `interval_seconds` | `300` | How often to reconcile, at least 60. |

The connections take `api_url` for an EU PagerDuty account (`https://api.eu.pagerduty.com`) or a self-hosted Sentry.

## The agent's instructions

A starting `CLAUDE.md` for `/path/to/triage`:

```md
You triage one PagerDuty incident.

1. Read it with `pagerduty.incident.view`. If an alert's details name a Sentry issue, read it with `sentry.issue.view` and that `issue`: the stack trace, the release and the suspect commits.
2. Look for an open issue about this with `github.issue.list` (`project`: "web"). If there isn't one, open one with `github.issue.create` (`project`: "web"): what fails, where in the code, the suspect commit with its link, and a link to the incident.
3. Add a note to the incident with `pagerduty.incident.note`: what you found, the suspect commit and the issue's link. Keep it short; responders read it on their phones.
4. Call `wait` with `{"match": {"types": ["pagerduty.incident.resolved", "core.ingress.absent"]}, "timeout_seconds": 604800}` and end your turn.
5. When you're woken because the incident resolved, call `declare_outcome` with `completed`. If you're woken by anything else, deal with it and wait again.
```

Each operation's own guidance reaches the agent through the toolbelt, so the instructions only have to say what to do with what it finds.

## Operations

| Operation | Does |
| --- | --- |
| `pagerduty.incident.view` | The incident, its alerts (with what the monitoring tool sent, such as a Sentry link) and its notes, and whether the agent may acknowledge it. |
| `pagerduty.incident.note` | Adds a note to the Work's incident, as the connection's `from` user. |
| `pagerduty.incident.acknowledge` | Acknowledges the Work's incident. Refused (`not_permitted`) unless the entry sets `allow_acknowledge`. |
| `sentry.issue.view` | An issue's state and counts, its latest event's stack trace, release and environment, and Sentry's suspect commits. Without `issue`, the Work's own issue. |
| `sentry.issue.comment` | Comments on the Work's Sentry issue. |
| `sentry.issue.resolve` | Resolves the Work's Sentry issue. |

The writes only reach the incident or issue the Work was opened for. Another Work can read a Sentry issue by ID, but can't change one.

## What to expect

- **Acknowledging is your call.** An acknowledged incident stops escalating, so an agent that acknowledges and then gets stuck leaves nobody paged. Set `allow_acknowledge` only if that's what you want.
- **A resolved incident completes its Work.** The agent waits for the resolution. If the webhook is missed, the next reconcile finds the incident gone and sends `core.ingress.absent`, which wakes the agent too.
- **One Work per issue or incident.** Alerts and status changes on the same issue or incident go to the same Work. Once that Work has completed, a later regression of the same Sentry issue starts nothing. A new PagerDuty incident always starts new Work.
- **Missed alerts aren't recovered.** Reconcile recovers issue and incident states, but not an alert rule firing. An issue that opened while the webhook was down still opens Work through `opened`.
- **Sentry lists recent issues.** Reconcile reads what Sentry's issue list returns, which by default leaves out issues not seen for a while. An open issue that drops off is reported as absent, and a route doesn't open Work for that.
- **Metric alerts and other resources are ignored.** Only issue webhooks and issue alert rules come in.

[Extension packaging](../../architecture/extension-packaging/) describes how these packages are built, as a model for others such as Datadog.
