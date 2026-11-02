---
title: Share it with a team
description: Run one daemon on a server for a team, with backups, a credential and role for each person, and the web view behind your own sign-in.
---

One daemon can serve a team. It runs on a server under an account of its own, and each person reaches it under their own name. This guide puts together pieces the other guides cover one at a time:

- [the background service](../background-service/) keeps the daemon running
- [scheduled backups](../backups/) keep its state
- [named operators](../operating/#more-than-one-operator) give each person a credential and a role
- [remote runners](../remote-runners/) move the runs to other machines, if you want them there

## 1. An account on the server

This walks through a Linux server, `conduit.example.com`, with an account `conduit` that owns the daemon, its state and its logins. Everyone else reaches the daemon through that account in the narrow ways below, never with a shell.

```sh
sudo useradd --create-home --shell /bin/bash conduit
sudo loginctl enable-linger conduit    # its user services run without anyone logged in
sudo machinectl shell conduit@         # a login shell with its user bus, for systemctl --user
```

As `conduit`, [install Open Conduit](/open-conduit/start/install/), sign `gh` and `claude` in, and [set it up](/open-conduit/start/quick-start/) as you would on your own machine. There is no desktop session to unlock a keyring, so store secrets with `file:` or `cmd:` references (see [configuration](../configuration/)).

Then install the service with the web view on:

```sh
open-conduit service install --web --web-host conduit.example.com
```

The daemon's state, its management socket and its operator token stay owner-only under `~conduit/.local/state/open-conduit`. Whoever can log in as `conduit` holds the owner's token and is `operator:local`, so keep that to the people who administer the server.

## 2. Backups off the machine

The daemon backs itself up every day, without stopping, into `backups/` in its state directory and keeps the newest seven (see [scheduled backups](/open-conduit/architecture/recovery/#scheduled-backups)). That survives a bad release, not a lost disk. Copy each bundle elsewhere, with whatever the server already uses (a `restic` or `rsync` timer, a volume snapshot), or point the bundles at a mounted volume:

```sh
open-conduit service install --web --web-host conduit.example.com --backup-dir /mnt/backups/open-conduit
```

A bundle holds the database and the artifacts runs kept. It does not hold the config, secrets or the agents' logins: keep the config file in version control or in the same backup, and expect to sign `gh` and `claude` in again on a new server. [Restore](../backups/#restore) is the same as on one machine.

## 3. A credential and a role for each person

Issue each person a credential with the role they need (see the [roles table](../operating/#more-than-one-operator)):

```sh
open-conduit auth add alice --role operate --out alice.token
```

Give alice the file over a channel you trust; it is the only copy. Everything she does is recorded as `operator:alice`, and anything her role doesn't allow is refused with `forbidden_role`. To let her answer an agent's questions and approvals too, add `operator:alice` to the `human` entry's `responders`.

The management API is served only on a Unix socket, so alice reaches it over SSH. Her key goes in `~conduit/.ssh/authorized_keys` with a forced command that connects to the socket and does nothing else:

```
restrict,command="socat - UNIX-CONNECT:/home/conduit/.local/state/open-conduit/management.sock" ssh-ed25519 AAAA… alice
```

`restrict` turns off shells, port forwarding and agent forwarding, and the command ignores whatever alice asks to run, so the key opens a stream to the management socket and nothing else. What she may do there is decided by her credential and its role. The server needs `socat` installed.

On her own machine, alice relays a local socket to the server and points the CLI at it and at her credential:

```sh
socat UNIX-LISTEN:"$HOME/.open-conduit.sock",fork,umask=077 EXEC:'ssh -T conduit@conduit.example.com' &
export OPEN_CONDUIT_SOCKET="$HOME/.open-conduit.sock"
export OPEN_CONDUIT_TOKEN_FILE="$HOME/alice.token"    # mode 0600
open-conduit work list
open-conduit tui
```

Each connection the CLI makes opens one SSH session. `work attach` and `container attach` run on the daemon's machine and can't be used this way.

To take someone's access away, revoke their credential (`auth revoke`, with the ID from `auth operators`) and remove their key.

## 4. The web view behind your own sign-in

The web view only listens on loopback, so it is never reachable from another machine on its own. To reach it from a browser, put a reverse proxy on the same server that serves HTTPS, signs people in (with your identity provider, GitHub or Google through [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/), say, or your tailnet) and forwards to `127.0.0.1:7420`.

The proxy must pass the browser's `Host` header through unchanged, so that the view sees the name it was started with (`--web-host`). It refuses any other name, and refuses a change whose `Origin` doesn't match. Caddy does this by default:

```
conduit.example.com {
	header Strict-Transport-Security "max-age=31536000"
	# your sign-in goes here, e.g. forward_auth to oauth2-proxy
	reverse_proxy 127.0.0.1:7420
}
```

With nginx, add `proxy_set_header Host $host;` and the same `Strict-Transport-Security` header.

The view marks its cookies `Secure` when a request arrives by its bare name, as it does through the proxy, so a signed-in browser never sends its session over plain HTTP. The HSTS header makes the browser use HTTPS for the name from the start, so a typed `http://` address never leaves the machine either.

Once signed in to the proxy, a person opens the view's own sign-in URL once to start a session. `open-conduit web url --host conduit.example.com` prints its token; use it as `https://conduit.example.com/login?token=…`.

**Everyone who uses the web view acts as you.** The view holds the owner's token, so whatever anyone does there is recorded as `operator:local`, with the `admin` role. The proxy decides who gets in, but not what they may do once inside. Give the view only to people you would trust with the server. Everyone else should use the CLI with their own credential, as in step 3.

## 5. Runs on other machines

By default runs execute on the server, as `conduit`. To keep heavy builds or untrusted repositories off it, give a binding a [remote runner](../remote-runners/) or a [container](../sandboxes/#containers). The daemon stays on the server and still checks every toolbelt call.
