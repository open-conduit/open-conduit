---
title: Scheduled jobs
description: Run commands or agent turns on cron, at fixed times, every N minutes, on events, on a webhook or by hand.
---

Open Conduit runs commands on a cron schedule, at fixed times, every N minutes, on code-host events, on a signed webhook or by hand.

- Each job keeps one run going and at most one waiting.
- A slot missed while the daemon was down is caught up if it is less than six hours late.
- A job can run an agent turn instead of a command: point its route at a `claude-code` binding instead of a `shell` one.

## Example config

Each workflow is a scope in the config. The example below is a complete, minimal config that passes `open-conduit config check`; see [configuration and secrets](/open-conduit/guides/configuration/) for what each part means.

```json
{
  "version": 1,
  "settings": {},
  "connections": [],
  "scopes": [
    {
      "id": "ops",
      "name": "Scheduled jobs",
      "secret_refs": {},
      "authorization": {"connections": [], "operations": ["read_context", "declare_outcome"], "read_scopes": []},
      "extensions": [
        {"name": "jobs", "type": "schedule", "secret_names": [],
         "config": {"namespace": "schedule/ops", "scope": "ops", "jobs": [
           {"name": "nightly-report", "cron": "0 7 * * *", "time_zone": "Europe/London"},
           {"name": "refresh", "every_minutes": 60}
         ]}}
      ],
      "routes": [
        {"name": "report", "handler": "schedule", "binding": "report", "active_policy": "queue",
         "config": {"jobs": ["nightly-report"]}},
        {"name": "refresh", "handler": "schedule", "binding": "refresh", "active_policy": "queue",
         "config": {"jobs": ["refresh"]}}
      ],
      "bindings": [
        {"name": "report", "executor": "shell", "requires": {"toolbelt": "cli"},
         "operations": ["read_context", "declare_outcome"], "secret_names": [],
         "config": {"command": "./scripts/nightly-report.sh", "dir": "/path/to/repo"}},
        {"name": "refresh", "executor": "shell", "requires": {"toolbelt": "cli"},
         "operations": ["read_context"], "secret_names": [],
         "config": {"command": "make refresh", "dir": "/path/to/repo"}}
      ]
    }
  ]
}
```

## Run a job by hand

```sh
open-conduit events emit --scope-id ops --type schedule.run --delivery-id run-1 --key-source schedule/ops --key-external-id job/nightly-report
```

See [scheduled jobs](../../architecture/schedule-extension/) for every trigger.
