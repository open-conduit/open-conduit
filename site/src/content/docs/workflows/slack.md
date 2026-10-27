---
title: Slack support
description: Answer Slack threads with a workspace-free agent over Socket Mode.
---

Open Conduit answers threads in a Slack channel with an agent that needs no repository or checkout. Each thread is one Work item, follow-ups are injected into a running turn, and replies are marked so the agent never answers itself. The connection uses Socket Mode, so no public URL is needed.

## Set it up

```sh
open-conduit init slack
open-conduit config check
```

`init slack` creates the Slack app from a manifest, verifies both tokens, stores them in the keyring and fills in the team, channel and bot user.

To answer only when the app is mentioned, set `"start": "mention"` with the app's `bot_user_id` in the extension's config.

## Example config

Each workflow is a scope in the config. The example below is a complete, minimal config that passes `open-conduit config check`; see [configuration and secrets](/open-conduit/guides/configuration/) for what each part means.

```json
{
  "version": 1,
  "settings": {},
  "connections": [
    {"id": "slack", "type": "slack", "config": {},
     "secret_refs": {"bot_token": "keyring:slack-bot-token", "app_token": "keyring:slack-app-token"}}
  ],
  "scopes": [
    {
      "id": "support",
      "name": "Slack support",
      "secret_refs": {},
      "authorization": {
        "connections": ["slack"],
        "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "support", "type": "slack", "connection": "slack", "secret_names": [],
         "config": {"namespace": "slack/T0123456789", "scope": "support", "team": "T0123456789",
                    "channels": ["C0123456789"], "start": "all", "receiver": "socket"}}
      ],
      "routes": [
        {"name": "conversations", "handler": "slack", "binding": "support", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "support",
          "executor": "claude-code",
          "connection": "slack",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": false},
          "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"],
          "secret_names": [],
          "config": {"playbook": "support-conversation", "args": "{key}"}
        }
      ]
    }
  ]
}
```

See [Slack ingress, conversations and marked replies](../../architecture/slack-extension/) for the details.
