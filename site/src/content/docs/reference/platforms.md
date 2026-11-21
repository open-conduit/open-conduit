---
title: Platforms and files
description: Supported platforms, where state and secrets live, and how to override the defaults.
---

## Platforms

Open Conduit runs on macOS and Linux, on amd64 and arm64. Windows is not supported. The [container image](/open-conduit/guides/headless/) is Linux on amd64 and arm64.

## Files

| What | Where |
| --- | --- |
| Config | `~/.config/open-conduit/config.json` (or `$XDG_CONFIG_HOME/open-conduit/config.json`) |
| State | `~/.local/state/open-conduit` (or `$XDG_STATE_HOME/open-conduit`) |
| Control socket | a private Unix socket in the state directory |
| Service log | `logs/service.log` in the state directory |
| Install ID | `telemetry-id` in the state directory: the random ID [usage telemetry](/open-conduit/reference/telemetry/) reports under |
| Secrets | the macOS Keychain, the Linux Secret Service through `secret-tool`, or an owner-only `secrets.json` beside the config |

The state directory must be on a local or block disk, such as ext4, XFS, APFS, EBS, Persistent Disk or Azure Disk. The daemon and the offline state commands refuse NFS, EFS, Filestore, SMB, Azure Files, FUSE bucket mounts and similar filesystems, because SQLite's WAL and file locks can't protect a live database there. `state check` and `doctor` report the filesystem. To move a state directory off one, stop the daemon and copy the whole directory to a local disk with `cp -a`, then point `OPEN_CONDUIT_DATA_DIR` at the copy.

## Overrides

| Flag | Environment variable | Overrides |
| --- | --- | --- |
| `--config` | `OPEN_CONDUIT_CONFIG` | the config file |
| `--data-dir` | `OPEN_CONDUIT_DATA_DIR` | the state directory |
| `--socket` | `OPEN_CONDUIT_SOCKET` | the control socket |
| `serve --no-telemetry` | `OPEN_CONDUIT_NO_TELEMETRY` or `DO_NOT_TRACK` | turns off [usage telemetry](/open-conduit/reference/telemetry/) |
