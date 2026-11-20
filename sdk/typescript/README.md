# @open-conduit/sdk

Build an [Open Conduit](https://github.com/open-conduit/open-conduit) extension in TypeScript: serve protocol version 1 on stdin and stdout, call the host (KV, `event.accept`, `ingress.deliver`, secrets, artifacts, state reads), declare each operation, source, route and Work kind once, and unit-test it on an in-memory host (`@open-conduit/sdk/testing`) so the manifest is derived from the code.

The guide is [Writing an extension in TypeScript](https://open-conduit.github.io/open-conduit/architecture/extension-packaging/#writing-an-extension-in-typescript). [`protocol/examples/weather`](../../protocol/examples/weather) is a complete package that `make certify` certifies.

```sh
npm ci
npm run typecheck
npm test
```

Node 22.18 or later. The tests run the TypeScript sources directly with Node's type stripping.
