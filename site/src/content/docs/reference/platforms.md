---
title: Platforms and files
description: Supported platforms, where state and secrets live, and how to override the defaults.
---

## Platforms

Open Conduit runs on macOS and Linux, on amd64 and arm64. Windows is not supported.

## Files

| What | Where |
| --- | --- |
| Config | `~/.config/open-conduit/config.json` (or `$XDG_CONFIG_HOME/open-conduit/config.json`) |
| State | `~/.local/state/open-conduit` (or `$XDG_STATE_HOME/open-conduit`) |
| Control socket | a private Unix socket in the state directory |
| Service log | `logs/service.log` in the state directory |
| Secrets | the macOS Keychain, the Linux Secret Service through `secret-tool`, or an owner-only `secrets.json` beside the config |

## Overrides

| Flag | Environment variable | Overrides |
| --- | --- | --- |
| `--config` | `OPEN_CONDUIT_CONFIG` | the config file |
| `--data-dir` | `OPEN_CONDUIT_DATA_DIR` | the state directory |
| `--socket` | `OPEN_CONDUIT_SOCKET` | the control socket |
