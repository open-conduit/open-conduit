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

## Start a different playbook per channel

One Slack entry can watch several channels and start each channel's threads on its own binding, and so its own playbook. Give a `slack` route `channels` in its config: it claims only threads in those channels. Routes are tried in order and the first match wins, so list the narrow routes first and a catch-all route (no `channels`) last. A route only sees the channels the entry watches, so list each channel in the entry's `channels` too. `config check` rejects a route config whose `channels` are not distinct Slack channel IDs.

Keep both channels on one entry rather than adding a second entry or scope for the same app: Slack spreads an app's Socket Mode events across its connections, so a second entry would receive some of the other channel's messages and drop them until the next reconcile.

Open Conduit ships `support-conversation`; `review-pr` below stands for a playbook of your own. The binding chooses the playbook; the agent never does.

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
      "name": "Slack support and reviews",
      "secret_refs": {},
      "authorization": {
        "connections": ["slack"],
        "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"],
        "read_scopes": []
      },
      "extensions": [
        {"name": "support", "type": "slack", "connection": "slack", "secret_names": [],
         "config": {"namespace": "slack/T0123456789", "scope": "support", "team": "T0123456789",
                    "channels": ["C0123456789", "C0REVIEWS01"], "start": "all", "receiver": "socket"}}
      ],
      "routes": [
        {"name": "reviews", "handler": "slack", "binding": "review", "active_policy": "inject",
         "config": {"channels": ["C0REVIEWS01"]}},
        {"name": "conversations", "handler": "slack", "binding": "support", "active_policy": "inject", "config": {}}
      ],
      "bindings": [
        {
          "name": "review",
          "executor": "claude-code",
          "connection": "slack",
          "requires": {"toolbelt": "mcp", "inject": true, "resume": true, "workspace": false},
          "operations": ["read_context", "wait", "declare_outcome", "slack.context", "slack.thread", "slack.reply"],
          "secret_names": [],
          "config": {"playbook": "review-pr", "args": "{key}"}
        },
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

Threads in `C0REVIEWS01` start on `review`; threads in `C0123456789` fall through to `conversations` and start on `support`. A thread stays with the route that started it.

Other handlers narrow a route the same way. Linear and Jira routes already take `{"match": {"labels": ["review"]}}` (or status, team, project and the other issue attributes) in their config, so one tracker entry can start different playbooks by label. The GitHub route takes no config yet; it would gain the same `match` when there is a need.

See [Slack ingress, conversations and marked replies](../../architecture/slack-extension/) for the details.
