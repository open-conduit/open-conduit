---
title: Introspection
description: Let agents report what breaks for them, and have a daily triage land the fixes.
---

Agents report what breaks for them: a command that doesn't exist, a credential that's missing, an instruction that's out of date. A daily triage reads those reports and the agents' notes, groups them by cause, and opens fix Work that lands a change.

- Add `introspection.report-issue` to a binding's operations to let its agent report problems.
- An optional **sweep** folds the corrections agents have logged into your rule files once a day.
- An optional **host watch** reports load, memory and disk pressure.

## Example config

Each workflow is a scope in the config. The example below is a complete, minimal config that passes `open-conduit config check`; see [configuration and secrets](/open-conduit/guides/configuration/) for what each part means.

```json
{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [
    {
      "id": "eng",
      "name": "Engineering",
      "secret_refs": {},
      "authorization": {
        "connections": [],
        "operations": ["read_context", "declare_outcome", "introspection.evidence", "introspection.reports",
                       "introspection.fix", "introspection.report.resolve", "introspection.notes.resolve"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "introspection", "type": "introspection", "secret_names": [],
         "config": {"triage": {"cron": "51 8 * * *", "time_zone": "UTC"}}}
      ],
      "routes": [
        {"name": "triage", "handler": "introspection", "binding": "triage", "active_policy": "queue",
         "config": {"work": "triage"}},
        {"name": "fixes", "handler": "introspection", "binding": "fix", "active_policy": "queue",
         "config": {"work": "fix"}}
      ],
      "bindings": [
        {"name": "triage", "executor": "claude-code", "requires": {"toolbelt": "mcp"},
         "operations": ["read_context", "declare_outcome", "introspection.evidence", "introspection.reports",
                        "introspection.fix", "introspection.report.resolve", "introspection.notes.resolve"],
         "secret_names": [], "config": {"playbook": "triage-reports"}},
        {"name": "fix", "executor": "claude-code", "requires": {"toolbelt": "mcp", "workspace": true},
         "operations": ["read_context", "declare_outcome", "introspection.reports", "introspection.report.resolve"],
         "secret_names": [], "config": {"playbook": "fix-report"},
         "resource": {"provider": "git", "config": {"checkout": "/path/to/repo", "mode": "work"}}}
      ]
    }
  ]
}
```

In practice the introspection entry sits in the same scope as your GitHub agent. See [introspection](https://github.com/open-conduit/open-conduit/blob/main/docs/architecture/issue-reporting.md) for the sweep, the host watch and by-hand runs.
