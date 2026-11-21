---
title: Build your first extension in TypeScript
description: In about 30 minutes, write a weather extension in TypeScript that agents can call, test it, install it and watch an agent use it.
---

This tutorial builds a small extension package from nothing: `weather`, which gives agents a forecast for a city by asking a weather service. On the way you write a toolbelt operation, test it, check the package and install it. It takes about 30 minutes, and it is [the Go tutorial](/open-conduit/guides/first-extension/) in TypeScript.

You need [Node](https://nodejs.org/) 22.18 or later, with npm, and Make. You also need Open Conduit installed, for `extension new`, and for the last step set up with a GitHub repository, as in the [quick start](/open-conduit/start/quick-start/).

## The words you'll meet

- **Operation:** something an agent can ask Open Conduit to do, such as `github.issue.comment`. Agents call operations through their toolbelt; your extension adds one, `weather.forecast`.
- **Event:** something that happened, recorded once: an issue opened, a comment posted. Events start and wake Work.
- **Work:** one job an agent does, such as working an issue, across as many turns as it takes.
- **Scope:** one part of your configuration, such as a GitHub repository, with its own agents, their operations and the extensions they use.
- **Entry:** an extension's settings in one scope. Your weather entry names the weather service that scope uses.
- **Binding:** an agent a scope can run, and the operations it may call.

## 1. Start the package

`extension new` writes a package repository whose checks pass on its first run:

```sh
open-conduit extension new weather --lang ts
cd weather
npm install
make check
```

`make check` runs Prettier, the typecheck and the tests, then builds the package into `dist/weather`. The repository holds one operation, `weather.hello`, which you are about to replace:

| File | Holds |
| --- | --- |
| `src/extension.ts` | the extension: its operations, and the fixture certification runs |
| `src/extension.test.ts` | unit tests, which call the operation in memory |
| `src/main.ts` | the command, which you won't need to change |
| `scripts/package.mjs` | lays out the installable package |
| `playbooks/weather.md` | what the package tells agents about itself |
| `Makefile` | `check`, `package`, `lint` and `certify` |

## 2. Write `weather.forecast`

An operation is a handler with a schema for its input. Agents read the schema, so a field's `description` is what an agent sees, and the handler receives input that has already been checked and typed.

This operation needs to know where the weather service is, and that differs from one setup to another. So the package also declares an entry type, `weather`: an operator adds a weather entry to a scope with the service's address, and Open Conduit sends every entry to the package in `configure`, at start and whenever the configuration changes. The handler looks up the service for the scope the calling agent works in.

Replace `src/extension.ts` with:

```ts title="src/extension.ts"
// weather-extension is an Open Conduit extension package. It serves one
// toolbelt operation, weather.forecast, that asks the weather service a
// scope's weather entry names for a city's forecast.

import { extension, OperationError, t } from "@open-conduit/sdk";

// The package's release, written into the manifest. `make package
// VERSION=...` sets it, as the release workflow does from the tag.
export const version = process.env.EXTENSION_VERSION ?? "0.1.0";

// The config of a scope's weather entry. A config that doesn't match is
// refused before the daemon applies it.
const EntryConfig = t.object({
  service: t.string({ description: "The weather service's base URL." }),
});

interface Forecast {
  city: string;
  summary: string;
  high_c: number;
  low_c: number;
}

// Tests pass their own fetch, so they never touch the network.
export function build(fetcher: typeof fetch = fetch) {
  // Rebuilt on every configuration, so a removed entry stops answering.
  let services = new Map<string, string>();

  return extension("weather", {
    version,
    summary: "weather: forecasts for agents.",
    playbooks: "playbooks",
  })
    .entryType("weather", {
      config: EntryConfig,
      configure: (entries) => {
        services = new Map(
          entries.map((e) => [e.scopeId, e.config.service.replace(/\/$/, "")]),
        );
      },
    })
    .operation("weather.forecast", {
      input: t.object({
        city: t.string({ description: "The city to forecast, by name." }),
      }),
      guidance:
        "Call `weather.forecast` with a `city` to get its forecast: a summary and the high and low in °C.",
      handler: async ({ city }, { authority }): Promise<Forecast> => {
        const name = city.trim();
        if (name === "")
          throw new OperationError(
            "bad_request",
            "weather.forecast needs a city",
          );
        const service = services.get(authority.scope_id);
        if (service === undefined)
          throw new OperationError(
            "not_configured",
            "this scope has no weather entry naming a service",
          );
        let response: Response;
        try {
          response = await fetcher(
            `${service}/forecast?city=${encodeURIComponent(name)}`,
          );
        } catch {
          throw new OperationError(
            "service_unavailable",
            "the weather service did not answer; try again later",
          );
        }
        if (response.status === 404)
          throw new OperationError(
            "unknown_city",
            `the weather service doesn't know ${name}`,
          );
        if (!response.ok)
          throw new OperationError(
            "service_unavailable",
            `the weather service answered ${response.status}`,
          );
        return (await response.json()) as Forecast;
      },
    })
    .certification({
      // Certification never contacts a real service, so these are the
      // refusals the package gives without one.
      fixture: {
        operations: [
          {
            operation: "weather.forecast",
            input: { data: { city: " " } },
            error: "bad_request",
          },
          {
            operation: "weather.forecast",
            input: { data: { city: "Leeds" } },
            error: "not_configured",
          },
        ],
      },
    });
}
```

What changed from the scaffold:

- **The operation.** The handler checks its input, finds the scope's service and calls `GET /forecast?city=NAME` on it. A refusal the agent can act on is an `OperationError` with a stable code and a short message; the agent sees both.
- **The entry type.** `entryType` declares `weather`, checks its config against the schema and keeps each scope's service.
- **The guidance.** `guidance` tells agents when to call the operation. It is all an agent knows about it, so say what it gives back.
- **The certification fixture.** `certification` lists cases `make certify` checks.

Update the playbook the same way. Replace `playbooks/weather.md` with:

```md title="playbooks/weather.md"
---
name: weather
description: How an agent gets a weather forecast through the weather package. Read it before planning anything that depends on the weather.
---

# weather

The weather package gives you forecasts.

- Call `weather.forecast` with a `city` to get its forecast: a summary and the high and low in °C.
- If it refuses with `unknown_city`, ask for a nearby larger city instead of guessing.
```

## 3. Test it

`testHost` from `@open-conduit/sdk/testing` runs your extension on an in-memory host, over the same protocol the daemon uses, so a test runs in milliseconds and needs nothing built. These tests pass a stand-in `fetch`, configure a weather entry and call the operation. Replace `src/extension.test.ts` with:

```ts title="src/extension.test.ts"
import assert from "node:assert/strict";
import { test, type TestContext } from "node:test";
import { OperationError } from "@open-conduit/sdk";
import { testHost } from "@open-conduit/sdk/testing";
import { build } from "./extension.ts";

const service: typeof fetch = async (input) => {
  const city = new URL(String(input)).searchParams.get("city");
  if (city !== "Leeds") return new Response("", { status: 404 });
  return Response.json({ city, summary: "rain", high_c: 12, low_c: 6 });
};

async function serve(t: TestContext) {
  const host = await testHost(build(service));
  t.after(() => host.close());
  await host.configure([
    {
      scope_id: "s1",
      type: "weather",
      config: { service: "http://weather.test/" },
    },
  ]);
  return host;
}

const refused = (code: string) => (e: unknown) =>
  e instanceof OperationError && e.code === code;

test("weather.forecast answers from the scope's service", async (t) => {
  const host = await serve(t);
  const out = await host.operation("weather.forecast", { city: " Leeds " });
  assert.deepEqual(out.output, {
    city: "Leeds",
    summary: "rain",
    high_c: 12,
    low_c: 6,
  });
});

test("weather.forecast refuses what it can't answer", async (t) => {
  const host = await serve(t);
  await assert.rejects(
    host.operation("weather.forecast", { city: " " }),
    refused("bad_request"),
  );
  await assert.rejects(
    host.operation("weather.forecast", { city: "Atlantis" }),
    refused("unknown_city"),
  );
  await assert.rejects(
    host.operation("weather.forecast", { city: "Leeds" }, { scope_id: "s2" }),
    refused("not_configured"),
  );
});
```

Run everything again:

```sh
make check
```

## 4. Check, install and use it

`make check` ended by building the package in `dist/weather`. Lint it, and run the certification suite:

```sh
make lint
make certify
```

`make lint` reads the manifest and the playbooks without starting the package, and says what to change for each problem it finds. `make certify` starts the package and checks it speaks the protocol correctly, using the fixture in `certification`.

### A weather service to try it with

To see a real agent use the operation, it needs a weather service to ask. Add a stand-in that gives every city the same forecast, as `fakeweather.mjs`:

```js title="fakeweather.mjs"
// A stand-in weather service for trying the package by hand: it answers
// GET /forecast?city=NAME for any city.
import { createServer } from "node:http";

createServer((request, response) => {
  const url = new URL(request.url, "http://127.0.0.1:8090");
  if (request.method !== "GET" || url.pathname !== "/forecast") {
    response.writeHead(404).end();
    return;
  }
  response.writeHead(200, { "content-type": "application/json" });
  response.end(
    JSON.stringify({
      city: url.searchParams.get("city"),
      summary: "sunny spells",
      high_c: 18,
      low_c: 9,
    }),
  );
}).listen(8090, "127.0.0.1", () => {
  console.log("fake weather service on http://127.0.0.1:8090");
});
```

Start it in a terminal of its own and leave it running:

```sh
node fakeweather.mjs
```

### Install the package

Install the package `make check` built and restart the daemon, which reads packages only when it starts:

```sh
open-conduit extension install dist/weather
open-conduit service stop
open-conduit service start
```

If you run the daemon in the foreground instead, stop `open-conduit serve` and start it again.

The package's executable uses the Node that built it, whatever the daemon's PATH finds. If that Node is later removed, it falls back to the first Node on the daemon's PATH.

### Give a scope the operation

Your scope needs a weather entry, and its agent needs to be allowed to call `weather.forecast`. The commands below use the scope `web` and its binding `agent`, as `open-conduit init github --repo acme/web` names them; use your own scope's ID. Each command checks the result before saving it:

```sh
open-conduit config set /scopes/web/extensions/- '{"name": "weather", "type": "weather", "secret_names": [], "config": {"service": "http://127.0.0.1:8090"}}'
open-conduit config set /scopes/web/authorization/operations/- '"weather.forecast"'
open-conduit config set /scopes/web/bindings/agent/operations/- '"weather.forecast"'
```

The first adds the entry. The second lets the scope use the operation at all, and the third lets its agent call it. Then hand the changed file to the running daemon:

```sh
open-conduit config apply "$(open-conduit config path)"
```

### Ask an agent

Open an issue in the repository: "What's the weather forecast for Leeds? Answer here, no PR needed." Once Open Conduit picks it up, as it does any issue in that scope (`open-conduit claim N` claims issue N by hand), the agent finds `weather.forecast` in its toolbelt with your guidance. It calls it and answers on the issue with what the stand-in service said: sunny spells, 18°C at most.

Watch it happen in `open-conduit tui`, or find the run and read the agent's transcript, where the call and its result appear:

```sh
open-conduit runs list --scope web
open-conduit runs log ID --transcript
```

## Where next

- **Publish it.** Push the repository, and CI (`.github/workflows/ci.yml`) runs `make check` and certifies the package against Open Conduit's `main` and its latest release. Push a tag (`v1.0.0`) to publish a release: `.github/workflows/release.yml` builds one portable archive, which records no path from your machine and runs on the Node on the daemon's PATH, signs it with Sigstore and uploads it with its bundle. The generated README shows the `settings.extension_trust` entry an operator adds to trust it.
- **Own a source.** An extension can poll a vendor's API, verify its webhooks, map its objects to Work and let an agent wait for "this ticket is resolved". [Writing an extension in TypeScript](/open-conduit/architecture/extension-packaging/#writing-an-extension-in-typescript) covers `ingressType`, `route`, `workKind`, `wait` and `emit`.
- **Test more.** [The test host](/open-conduit/architecture/extension-packaging/#unit-tests-in-typescript-open-conduitsdktesting) drives each of those surfaces in memory, with a clock that moves only when your test moves it.
- **The reference.** [Extension packaging, SDK and certification](/open-conduit/architecture/extension-packaging/) and [the extension protocol](/open-conduit/architecture/extension-protocol/) are the full references.
