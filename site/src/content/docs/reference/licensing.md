---
title: Licensing
description: Which parts of Open Conduit are under which licence, and what you may build on it.
---

Open Conduit has two licences. The core is under the Functional Source License (FSL-1.1-ALv2). Everything you need to build on it is under the MIT License.

## What you can do

Building on Open Conduit is welcome, commercial work included. You may:

- run it for yourself or inside your company, on as many machines as you like;
- write extensions, packages and applications on it, and sell them under any licence you choose;
- offer professional services to people who run it, such as setting it up, writing their extensions or operating it for them;
- change it and share your changes, as long as you keep the licence and copyright notices.

The one thing the FSL doesn't allow is a competing use: offering Open Conduit, or a product or service substantially like it, to others commercially, for example as a hosted Open Conduit service. A commercial licence for that is available separately; ask in a [GitHub issue](https://github.com/open-conduit/open-conduit/issues).

Every release also becomes available under the Apache License 2.0 two years after it is published, with no restrictions on use.

## Which licence covers what

| Part | Directories | Licence |
| --- | --- | --- |
| Core: the daemon, orchestration engine, first-party extensions and web UI | everything not listed below | [FSL-1.1-ALv2](https://github.com/open-conduit/open-conduit/blob/main/LICENSE) |
| TypeScript extension SDK | `sdk/typescript` | MIT |
| Extension protocol, Go extension SDK and example extensions | `protocol` | MIT |
| Public contracts extensions build against | `domain`, `extension`, `storage`, `observability` | MIT |
| Management API client and the test harness for extension packages | `client`, `extensiontest` | MIT |

Each MIT directory holds its own `LICENSE` file, which applies to it and the directories under it instead of the FSL. An extension that imports only these packages, which is all the SDKs need, is built entirely on MIT-licensed code.

## Contributing

A contribution is accepted under the licence of the directory it changes. [CONTRIBUTING.md](https://github.com/open-conduit/open-conduit/blob/main/CONTRIBUTING.md) covers the rest.
