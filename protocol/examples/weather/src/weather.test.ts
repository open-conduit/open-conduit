// Unit tests: the extension served in-process over a pair of streams, with
// a Conn playing the host. No daemon, network or clock, so they run in
// milliseconds; `make certify` covers the packaged binary against a real host.

import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import {
  Conn,
  KVCommitParams,
  KVGetParams,
  matches,
  OperationError,
  ProtocolError,
} from "@open-conduit/sdk";
import { build } from "./weather.ts";

const authority = {
  scope_id: "s1",
  work_id: "w1",
  execution_id: "e1",
  lease: { execution_id: "e1", owner: "o", epoch: 1 },
  expires_at: "2026-11-01T13:00:00Z",
};

async function serve() {
  const toExtension = new PassThrough();
  const fromExtension = new PassThrough();
  build().serve(toExtension, fromExtension);

  // Just enough of the host's KV for host.update: values stay base64, as on the wire.
  const kv = new Map<string, string>();
  const host = new Conn(fromExtension, toExtension, async (method, params) => {
    if (method === "kv.get" && matches(KVGetParams, params)) {
      const value = kv.get(`${params.namespace}/${params.key}`);
      return value === undefined
        ? { found: false, entry: { key: "", value: null, version: "" } }
        : { found: true, entry: { key: params.key, value, version: value } };
    }
    if (method === "kv.commit" && matches(KVCommitParams, params)) {
      for (const put of params.puts ?? [])
        kv.set(`${put.namespace}/${put.key}`, put.value);
      return {};
    }
    throw new ProtocolError("method_not_found", method);
  });

  // The host's own start-up: initialize, then configure with one scope's entry.
  await host.call("initialize", {
    protocol_versions: [1],
    host: { name: "test", version: "0" },
    state_dir: "",
  });
  await host.call("configure", {
    generation: "g1",
    kv: { stored: 0, declared: 0 },
    entries: [
      {
        scope_id: "s1",
        type: "weather",
        config: { forecasts: { london: { summary: "rain", high_c: 14 } } },
      },
    ],
  });
  return { host, kv, end: () => toExtension.end() };
}

const forecast = (data: unknown, scope = "s1") => ({
  operation: "weather.forecast",
  authority: { ...authority, scope_id: scope },
  input: { data },
});

test("weather.forecast answers from the scope's forecasts and counts the lookup", async () => {
  const { host, kv, end } = await serve();
  assert.deepEqual(
    await host.call("operation.call", forecast({ city: "london", days: 2 })),
    {
      output: {
        schema: "weather.forecast.v1",
        data: { city: "london", days: 2, summary: "rain", high_c: 14 },
      },
    },
  );
  assert.equal(
    Buffer.from(kv.get("weather.lookups/s1/london") ?? "", "base64").toString(),
    "1",
  );
  end();
});

test("weather.forecast refuses an unknown city and a scope with no entry", async () => {
  const { host, end } = await serve();
  await assert.rejects(
    host.call("operation.call", forecast({ city: "atlantis" })),
    (e: unknown) => e instanceof OperationError && e.code === "unknown_city",
  );
  await assert.rejects(
    host.call("operation.call", forecast({ city: "london" }, "s2")),
    (e: unknown) => e instanceof OperationError && e.code === "not_configured",
  );
  end();
});

test("a weather entry is invalid unless its forecasts match the schema", async () => {
  const { host, end } = await serve();
  for (const config of [{}, { forecasts: { london: { summary: "rain" } } }]) {
    await assert.rejects(
      host.call("validate", { kind: "extension", type: "weather", config }),
      (e: unknown) => e instanceof ProtocolError && e.code === "invalid",
      JSON.stringify(config),
    );
  }
  assert.deepEqual(
    await host.call("validate", {
      kind: "extension",
      type: "weather",
      config: { forecasts: {} },
    }),
    {},
  );
  end();
});
