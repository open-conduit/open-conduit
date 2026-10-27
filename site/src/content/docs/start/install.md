---
title: Install
description: Install Open Conduit with Homebrew, a release binary, go install or from source.
---

Open Conduit ships two commands, kept side by side:

- `open-conduit` is the daemon and operator client.
- `os` is the small toolbelt that shell commands call from inside a run. The daemon looks for it beside its own executable.

You also need the CLIs of the services you use: [Claude Code](https://docs.anthropic.com/en/docs/claude-code), signed in, for agent bindings, and optionally the GitHub CLI (`gh`), whose login is the default GitHub credential.

## Homebrew

```sh
brew install open-conduit/tap/open-conduit
```

The cask puts only `open-conduit` on your PATH. `os` stays beside it in the cask's own directory, where the daemon finds it, so it can't clash with another command of that name. Uninstalling the cask also removes the [background service](/open-conduit/guides/background-service/).

## Release binaries

Archives for macOS and Linux, on amd64 and arm64, are on the [releases page](https://github.com/open-conduit/open-conduit/releases) as `open-conduit_<version>_<os>_<arch>.tar.gz`, with a `checksums.txt`. Each holds `open-conduit` and `os`; keep them in the same directory.

The install script downloads the latest release, verifies its checksum, puts both binaries in `~/.local/lib/open-conduit` and links `open-conduit` into `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/open-conduit/open-conduit/main/install.sh | sh
```

Set `OPEN_CONDUIT_VERSION=v0.1.0` (or another tag) to install a particular release, and `BIN_DIR` or `INSTALL_DIR` to change where the link and the binaries go.

## go install

You need Go 1.24.2 or later. Install both commands, which puts them side by side in `$(go env GOBIN)` (default `~/go/bin`):

```sh
go install github.com/open-conduit/open-conduit/cmd/open-conduit@latest
go install github.com/open-conduit/open-conduit/cmd/os@latest
```

`os` lands on your PATH there under that generic name. Install both into the same directory, or point the daemon elsewhere with `serve --toolbelt-dir DIR`.

## From source

You need Go 1.24.2 or later, Git and Make:

```sh
git clone https://github.com/open-conduit/open-conduit.git
cd open-conduit
make build                 # writes bin/open-conduit and bin/os
cp bin/open-conduit bin/os /usr/local/bin/   # or anywhere on your PATH, side by side
```

Next: run `open-conduit init --with claude` and let Claude Code set it up with you, as the [quick start](/open-conduit/start/quick-start/) describes.
