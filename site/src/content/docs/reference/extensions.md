---
title: Extensions
description: Add integrations, executors and toolbelt operations as separate extension packages.
---

Integrations outside the built-in set run as separate processes that speak a JSON-RPC protocol over stdio. A package is a directory holding a manifest, an executable and a certification fixture.

A package can contribute integrations, executors, toolbelt operations, setups and doctor checks. Once installed, its types are known to `config check`. To write one, start with [Build your first extension](../../guides/first-extension/).

```sh
open-conduit extension new my-extension              # start a package repository whose make check passes first time
open-conduit extension check ./my-extension          # lint it without starting it; says what to change for each problem
open-conduit extension inspect ./my-extension        # check a package offline
open-conduit extension certify ./my-extension        # run the conformance suite against it
open-conduit extension install ./my-extension        # takes effect at the next serve
open-conduit extension install https://example.com/my-extension-darwin-arm64.tar.gz --sha256 <hex>
open-conduit extension list
open-conduit extension update ./my-extension         # the replaced release is kept; also takes an archive
open-conduit extension rollback my-extension
open-conduit extension remove my-extension           # refused while the config uses it
```

A package that needs a host service beyond its own surfaces, such as receiving every scope's telemetry events, running setup probes or proposing configuration, declares a [permission](../../architecture/extension-protocol/#permissions) for it. `inspect`, `install` and `list` show each permission and what it allows, and the daemon refuses a call the manifest did not declare.

:::caution
The protocol and packaging are provisional and may still change.
:::

When install, start or `check` refuses a package, the message carries a code; [troubleshooting](../../architecture/extension-packaging/#troubleshooting) says what to change for each. See [extension packaging](../../architecture/extension-packaging/) for writing one with the Go SDK, and [the extension protocol](../../architecture/extension-protocol/) for the wire format.
