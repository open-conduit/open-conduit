// The weather extension served in-process over a stream pair, with a Conn
// standing in for the host's side.
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import { Conn, OperationError, ProtocolError } from "@open-conduit/sdk";
import { build } from "./weather.ts";

const authority = { scope_id: "s1", work_id: "w1", execution_id: "e1", lease: { execution_id: "e1", owner: "o", epoch: 1 }, expires_at: "2026-11-01T13:00:00Z" };

async function serve() {
  const toExt = new PassThrough();
  const fromExt = new PassThrough();
  build().serve(toExt, fromExt);
  const kv = new Map<string, string>();
  const host = new Conn(fromExt, toExt, async (method, params) => {
    const p = params as { namespace: string; key: string; puts?: { namespace: string; key: string; value: string }[] };
    if (method === "kv.get") {
      const v = kv.get(`${p.namespace}/${p.key}`);
      return v === undefined ? { found: false, entry: { key: "", value: null, version: "" } } : { found: true, entry: { key: p.key, value: v, version: v } };
    }
    if (method === "kv.commit") {
      for (const put of p.puts ?? []) kv.set(`${put.namespace}/${put.key}`, put.value);
      return {};
    }
    throw new ProtocolError("method_not_found", method);
  });
  await host.call("initialize", { protocol_versions: [1], host: { name: "test", version: "0" }, state_dir: "" });
  await host.call("configure", { generation: "g1", kv: { stored: 0, declared: 0 }, entries: [{ scope_id: "s1", type: "weather", config: { forecasts: { london: { summary: "rain", high_c: 14 } } } }] });
  return { host, kv, end: () => toExt.end() };
}

const forecast = (data: unknown, scope = "s1") => ({ operation: "weather.forecast", authority: { ...authority, scope_id: scope }, input: { data } });

test("weather.forecast answers from the scope's forecasts and counts the lookup", async () => {
  const { host, kv, end } = await serve();
  assert.deepEqual(await host.call("operation.call", forecast({ city: "london", days: 2 })), { output: { schema: "weather.forecast.v1", data: { city: "london", days: 2, summary: "rain", high_c: 14 } } });
  assert.equal(Buffer.from(kv.get("weather.lookups/s1/london")!, "base64").toString(), "1");
  end();
});

test("weather.forecast refuses an unknown city and an unconfigured scope", async () => {
  const { host, end } = await serve();
  await assert.rejects(host.call("operation.call", forecast({ city: "atlantis" })), (e: unknown) => e instanceof OperationError && e.code === "unknown_city");
  await assert.rejects(host.call("operation.call", forecast({ city: "london" }, "s2")), (e: unknown) => e instanceof OperationError && e.code === "not_configured");
  end();
});

test("a weather entry without forecasts is invalid", async () => {
  const { host, end } = await serve();
  await assert.rejects(host.call("validate", { kind: "extension", type: "weather", config: {} }), (e: unknown) => e instanceof ProtocolError && e.code === "invalid");
  assert.deepEqual(await host.call("validate", { kind: "extension", type: "weather", config: { forecasts: {} } }), {});
  end();
});
