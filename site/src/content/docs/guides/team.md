---
title: Share it with a team
description: Run one daemon on a server for a team, with backups, a credential and role for each person, and the web view behind your own sign-in.
---

One daemon can serve a team. It runs on a server under an account of its own, and each person reaches it under their own name. This guide puts together pieces the other guides cover one at a time:

- [the background service](../background-service/) keeps the daemon running
- [scheduled backups](../backups/) keep its state
- [named operators](../operating/#more-than-one-operator) give each person a credential and a role
- [remote runners](../remote-runners/) move the runs to other machines, if you want them there

To run it on Kubernetes, ECS or Nomad instead of a login account, start from the image and [run it on a cluster](../headless/).

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

The daemon backs itself up every day, without stopping, into `backups/` in its state directory and keeps the newest seven (see [scheduled backups](/open-conduit/architecture/recovery/#scheduled-backups)). That survives a bad release, not a lost disk. Ship them to a [backup destination](/open-conduit/guides/backups/#ship-bundles-off-this-machine) such as an S3 bucket, copy each bundle elsewhere with whatever the server already uses (a `restic` or `rsync` timer, a volume snapshot), or point the bundles at a mounted volume:

```sh
open-conduit service install --web --web-host conduit.example.com --backup-dir /mnt/backups/open-conduit
```

A bundle holds the database and the artifacts runs kept, and the database holds every config generation, so a restored daemon runs the config it had and `open-conduit config export` writes the file back. It does not hold secret values, installed packages or the agents' logins: keep the secrets and the list of packages with the server's own setup, and expect to sign `gh` and `claude` in again on a new server. [Restore](../backups/#restore) is the same as on one machine.

## 3. A credential and a role for each person

Issue each person a credential with the role they need (see the [roles table](../operating/#more-than-one-operator)):

```sh
open-conduit auth add alice --role operate --out alice.token
```

Give alice the file over a channel you trust; it is the only copy. Add `--ttl 720h` (or any duration) to make it expire, so access lapses unless you issue another. Everything she does is recorded as `operator:alice`, and anything her role doesn't allow is refused with `forbidden_role`. To let her answer an agent's questions and approvals too, add `operator:alice` to the `human` entry's `responders`.

### A role in one team's scopes only

When several teams share the daemon, give each person a role where they work rather than everywhere. A grant is a role at a scope, and it covers that scope and every scope beneath it:

```sh
open-conduit auth add bea --grants support=operate,billing=view --out bea.token
```

bea can run controls on Work in `support` and its child scopes, and read `billing` and its children. Everything else is closed to her. Lists show her only the Work, Events and runs in those scopes, whatever filter she gives, and a read or control aimed at anything else is refused with `forbidden_scope`. A grant is `view` or `operate`. `admin` is only ever daemon-wide, because config, secrets and operators belong to the whole daemon. For the same reason, `config`, `doctor` and the extensions' pages need a daemon-wide role. `--role view --grants support=operate` combines the two: she can read everything and operate only in `support`. To change someone's grants, revoke the credential and issue another.

### Reach the daemon over TLS

To give alice, or a service of yours, an HTTPS endpoint (behind your load balancer, say), turn on the management listener with a certificate and key:

```sh
open-conduit serve --management-addr 0.0.0.0:8443 \
  --management-tls-cert /etc/open-conduit/tls.crt --management-tls-key /etc/open-conduit/tls.key \
  --management-client-ca /etc/open-conduit/clients-ca.pem
```

`--management-client-ca` is optional: with it, every connection must also present a client certificate that CA signed, which is worth having on any address outside a private network. A renewed certificate is picked up without a restart. If a proxy on the same machine terminates TLS, serve plaintext to it on loopback instead: `--management-addr 127.0.0.1:8443 --management-trusted-proxy`. Anything else that would serve plaintext, or a listener missing part of its setup, stops `serve` from starting. Give the paths as absolute paths when you install the service.

alice then points the CLI at it, with her credential, the CA her copy should trust, and her client certificate if you asked for one:

```sh
export OPEN_CONDUIT_ENDPOINT=https://conduit.example.com:8443
export OPEN_CONDUIT_TOKEN_FILE="$HOME/alice.token"    # mode 0600
export OPEN_CONDUIT_CA_FILE="$HOME/conduit-ca.pem"   # trust only this CA; leave unset for a public certificate
export OPEN_CONDUIT_CLIENT_CERT="$HOME/alice.crt" OPEN_CONDUIT_CLIENT_KEY="$HOME/alice.key"
open-conduit work list
```

Every call still needs her credential, and her role and grants decide what it may do, exactly as on the socket. Once a client has failed to authenticate 10 times in a minute, its further failures are refused for the rest of that minute and logged; a good credential from the same address is still served. `work attach` and `container attach` run on the daemon's machine, so over the network they say what to run there instead.

### Or relay the socket over SSH

Without the listener, the management API is served only on a Unix socket, so alice reaches it over SSH. Her key goes in `~conduit/.ssh/authorized_keys` with a forced command that connects to the socket and does nothing else:

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

Each connection the CLI makes opens one SSH session. Over this relay, add `--stream` to `work attach` or `container attach`: the daemon runs the session beside the run and relays its terminal to you. Over an `https://` endpoint they do this by themselves. It needs the `operate` role.

To take someone's access away, revoke their credentials (`auth revoke`, with each ID from `auth operators`) and remove their key. A credential with a TTL also lapses on its own at its expiry.

### Or let your identity provider decide

A company that runs joiners, movers and leavers through an identity provider (Okta, Entra ID, Google, Keycloak, anything that speaks OpenID Connect) can let it be the source of who may reach the daemon. Open Conduit doesn't run a sign-in flow or keep passwords: people get a token from the identity provider the way they already do, and the daemon checks it on every call. Trust the issuer once, with the groups that may do something:

```sh
open-conduit auth issuer set corp --type oidc \
  --config '{"issuer": "https://login.example.com/", "audience": "open-conduit"}' \
  --groups '{"platform-admins": "admin", "engineering": "operate", "everyone": "view"}'
```

The `oidc` package, bundled with the release, checks each token's signature against the keys the issuer publishes, and that it was issued by that issuer, for that audience, and hasn't expired. `open-conduit adapters oidc` lists its other settings: `jwks_url`, inline `keys`, which claims carry the person (`subject_claim`, default `sub`) and their groups (`groups_claim`, default `groups`), and the clock `leeway`.

Someone in `engineering` then calls with their token in place of a credential file. `OPEN_CONDUIT_TOKEN_FILE` may point at a private (0600) file holding just the token. Everything they do is recorded as `operator:corp:<subject>`, and their role is the highest one their groups map to. Someone whose groups map to none gets in but may do nothing. Leaving the company, or leaving the group, takes effect when their token expires, and removing a group from the table takes effect on the next call. The daemon checks the token on its first use and then until it expires, without asking the identity provider again, and it keeps working through a short outage of the identity provider. Your own credential never depends on the issuer, so you can still get in when the identity provider is down.

To give a group a role in some scopes only, add `--grants` rows to the issuer. Members of `support-oncall` then operate in `support` and its child scopes and nowhere else, as in the section above:

```sh
open-conduit auth issuer set corp --type oidc --config '…'   --groups '{"platform-admins": "admin"}'   --grants '[{"group": "support-oncall", "scope": "support", "role": "operate"}]'
```

Only an admin can trust an issuer or change its groups (`auth issuers` lists them, `auth issuer remove corp` stops trusting it), and each change is recorded in the daemon's log. The table lives in the daemon's own store, not in the config file, so applying a config can't grant anyone a role. To let someone answer an agent's questions, add `operator:corp:<subject>` to the `human` entry's `responders`.

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

**By default, everyone who uses the web view acts as you.** The view holds the owner's token, so whatever anyone does there is recorded as `operator:local`, with the `admin` role. The proxy decides who gets in, but not what they may do once inside. Give such a view only to people you would trust with the server.

### Or let each person act as themselves

When your proxy signs people in with an identity provider the daemon trusts (see [step 3](#or-let-your-identity-provider-decide)), it can pass each person's own token to the view, and the view then acts for that person on every call. Everything they do is recorded as them (`operator:corp:<subject>`), through the view, and they may do only what their groups allow, and nothing the view's own credential doesn't. The view holds a credential of its own that may act for operators, not yours:

```sh
open-conduit auth add web-console --role admin --act-for operator --out ~/.config/open-conduit/web-console.token
open-conduit service install --web --web-host conduit.example.com \
  --web-subject-header X-Forwarded-Access-Token \
  --web-token-file ~/.config/open-conduit/web-console.token
```

With oauth2-proxy, `--pass-access-token` sends the token in `X-Forwarded-Access-Token`; if your provider's access token isn't a JWT the `oidc` package can check, use `--pass-authorization-header` and `--web-subject-header Authorization` to send the ID token instead. Once the header is set, the view refuses any request that comes without it, rather than acting as itself, and the daemon refuses a token it can't verify. It reads nothing it caches for one person on behalf of another, and each browser's form token and list of recent changes are its own. The view's role (`admin` above) is the most anyone can do through it: someone in `platform-admins` through a view issued `--role operate` can't apply config.

People still open the view's sign-in URL once, as above. A named operator can be acted for too: a frontend of your own, with a credential issued `--act-for operator`, sends `Open-Conduit-Subject: operator:alice` and acts with alice's role and grants. A frontend can also act for people of its own kind, who have no role: issued `--act-for acme-ui`, it sends `Open-Conduit-Subject: acme-ui:alice`, and alice may answer and approve (actions) but nothing else, so a `human` entry's `responders` can name `acme-ui:alice`. The [HTTP API reference](../../reference/http-api/#acting-for-someone) has the detail.

## 5. Runs on other machines

By default runs execute on the server, as `conduit`. To keep heavy builds or untrusted repositories off it, give a binding a [remote runner](../remote-runners/) or a [container](../sandboxes/#containers). The daemon stays on the server and still checks every toolbelt call.
