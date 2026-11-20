// Unit tests: the extension served in-process on the SDK's test host, which
// answers KV, secrets and state reads from memory. No daemon, network or
// clock, so they run in milliseconds; `make certify` covers the packaged
// binary against a real host.

import assert from "node:assert/strict";
import { test } from "node:test";
import { OperationError, ProtocolError } from "@open-conduit/sdk";
import { testHost } from "@open-conduit/sdk/testing";
import { build } from "./weather.ts";

async function serve() {
  const host = await testHost(build());
  await host.configure([
    {
      scope_id: "s1",
      type: "weather",
      config: { forecasts: { london: { summary: "rain", high_c: 14 } } },
    },
  ]);
  return host;
}

test("weather.forecast answers from the scope's forecasts and counts the lookup", async (t) => {
  const host = await serve();
  t.after(() => host.close());
  const out = await host.operation("weather.forecast", {
    city: "london",
    days: 2,
  });
  assert.deepEqual(out.output, {
    city: "london",
    days: 2,
    summary: "rain",
    high_c: 14,
  });
  assert.equal(host.kvText("weather.lookups", "s1/london"), "1");
  await host.operation("weather.forecast", { city: "london" });
  assert.equal(host.kvText("weather.lookups", "s1/london"), "2");
});

test("weather.forecast refuses an unknown city and a scope with no entry", async (t) => {
  const host = await serve();
  t.after(() => host.close());
  await assert.rejects(
    host.operation("weather.forecast", { city: "atlantis" }),
    (e: unknown) => e instanceof OperationError && e.code === "unknown_city",
  );
  await assert.rejects(
    host.operation("weather.forecast", { city: "london" }, { scope_id: "s2" }),
    (e: unknown) => e instanceof OperationError && e.code === "not_configured",
  );
});

test("a weather entry is invalid unless its forecasts match the schema", async (t) => {
  const host = await serve();
  t.after(() => host.close());
  for (const config of [{}, { forecasts: { london: { summary: "rain" } } }]) {
    await assert.rejects(
      host.validate("extension", "weather", config),
      (e: unknown) => e instanceof ProtocolError && e.code === "invalid",
      JSON.stringify(config),
    );
  }
  await host.validate("extension", "weather", { forecasts: {} });
});
