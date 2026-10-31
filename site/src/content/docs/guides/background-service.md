---
title: Run it in the background
description: Run the daemon as a per-user launchd or systemd service.
---

`service install` runs the daemon as a per-user service: a launchd LaunchAgent on macOS, a systemd user unit on Linux. It starts at login, restarts if the daemon exits with an error, and runs in your session, so the OS keyring and your `gh` and Claude Code logins work as they do in a terminal.

```sh
open-conduit service install                         # takes serve's flags
open-conduit service status
open-conduit service stop                            # until the next start or login
open-conduit service start
open-conduit service uninstall
```

- `install` records the config path, the state directory, the `serve` flags you give it and a PATH built from your shell's PATH and wherever `claude`, `git`, `gh` and the tunnel binary are. It warns about any it can't find.
- Run `install` again after changing flags or installing a tool the daemon needs (a repository's toolchain, say): it replaces the unit and restarts the daemon.
- `open-conduit upgrade`, or the web view's Update page, installs a new release and restarts the service on it (see [upgrading](/open-conduit/start/install/#upgrading)).
- A `brew upgrade` restarts the daemon on the new version too. When the files the daemon runs from are removed, as `brew upgrade` does with the old version's directory, it stops with an error, and the service starts the new binary. Running turns carry on and the new daemon adopts them, but a shell step that calls `os` after the upgrade fails, since its PATH still names the removed directory. Upgrading while `open-conduit doctor` shows nothing running under `capacity` avoids that.
- Remove the service with `service uninstall` before a plain `brew uninstall`, or use `brew uninstall --zap open-conduit`, which removes it with the cask. If the cask is already gone, `launchctl bootout gui/$(id -u)/com.open-conduit.daemon` and deleting the plist clear it.
- Webhooks come from the config's `settings.webhooks`, as with `serve`.
- Output goes to `~/.local/state/open-conduit/logs/service.log` (under `--data-dir` if you give one). `open-conduit doctor` reports whether the service is installed and running.
- The unit lives at `~/Library/LaunchAgents/com.open-conduit.daemon.plist` or `~/.config/systemd/user/open-conduit.service`.
- Stop the service before `backup create` or `state check`, which need the daemon stopped.

:::note[Linux]
A systemd user unit runs only while you're logged in unless you run `loginctl enable-linger`. Without a desktop session the keyring may be locked, so prefer `file:` or `cmd:` secret references there.
:::
