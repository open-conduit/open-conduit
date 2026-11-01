// The SDK served over an in-memory stream pair, with a Conn standing in for
// the host. No process, no network, no clock.

import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import { CancelledError, Code, OperationError, ProtocolError } from "./errors.ts";
import { extension, type Extension } from "./extension.ts";
import { text } from "./host.ts";
import { Conn, type Handler } from "./jsonrpc.ts";
import { type Infer, t } from "./schema.ts";

const authority = { scope_id: "s1", work_id: "w1", execution_id: "e1", lease: { execution_id: "e1", owner: "o", epoch: 1 }, expires_at: "2026-11-01T13:00:00Z" };

/** Serves ext and returns the host side's connection. */
function connect(ext: Extension, hostHandler: Handler = async (m) => { throw new ProtocolError(Code.MethodNotFound, m); }) {
  const toExt = new PassThrough();
  const fromExt = new PassThrough();
  const server = ext.serve(toExt, fromExt);
  const host = new Conn(fromExt, toExt, hostHandler);
  return { server, host, end: () => toExt.end() };
}

async function ready(ext: Extension, hostHandler?: Handler) {
  const c = connect(ext, hostHandler);
  await c.host.call("initialize", { protocol_versions: [1], host: { name: "test", version: "0" }, state_dir: "/state" });
  return c;
}

const forecastInput = t.object({
  city: t.string({ description: "The city, by name." }),
  days: t.optional(t.integer()),
  units: t.optional(t.enum(["metric", "imperial"])),
});

function weather(): Extension {
  return extension("weather", { version: "0.1.0", summary: "Forecasts." })
    .operation("weather.forecast", {
      input: forecastInput,
      guidance: "Call it before planning anything outdoors.",
      handler: ({ city, days }) => {
        if (city === "atlantis") throw new OperationError("unknown_city", `no forecast for ${city}`);
        return { city, days: days ?? 1 };
      },
    })
    .check("weather", () => "ok");
}

test("the manifest is derived from the registrations", () => {
  assert.deepEqual(weather().manifest, {
    protocol: [1],
    name: "weather",
    version: "0.1.0",
    summary: "Forecasts.",
    command: ["weather-extension"],
    provides: {
      operations: [
        {
          name: "weather.forecast",
          kind: "handler",
          guidance: "Call it before planning anything outdoors.",
          input: {
            type: "object",
            properties: {
              city: { type: "string", description: "The city, by name." },
              days: { type: "integer" },
              units: { type: "string", enum: ["metric", "imperial"] },
            },
            additionalProperties: false,
            required: ["city"],
          },
        },
      ],
      checks: [{ name: "weather" }],
    },
  });
});

test("the handler's input type comes from the schema", () => {
  type In = Infer<typeof forecastInput>;
  const ok: In = { city: "london", units: "metric" };
  // @ts-expect-error city is required
  const missing: In = { days: 2 };
  // @ts-expect-error units is one of the enum's strings
  const wrong: In = { city: "london", units: "kelvin" };
  assert.ok(ok && missing && wrong);
});

test("initialize negotiates a version and returns the manifest", async () => {
  const ext = weather();
  const { host, end } = connect(ext);
  const out = await host.call<{ protocol_version: number; manifest: unknown }>("initialize", { protocol_versions: [1, 2], host: { name: "t", version: "0" }, state_dir: "/s" });
  assert.equal(out.protocol_version, 1);
  assert.deepEqual(out.manifest, ext.manifest);
  end();
});

test("initialize refuses a host that speaks no version the extension does", async () => {
  const { host, end } = connect(weather());
  await assert.rejects(host.call("initialize", { protocol_versions: [99], host: { name: "t", version: "0" }, state_dir: "" }), (e: unknown) => e instanceof ProtocolError && e.code === Code.UnsupportedVersion);
  end();
});

test("a request before initialize is refused", async () => {
  const { host, end } = connect(weather());
  await assert.rejects(host.call("check.run", { check: "weather", config: {} }), (e: unknown) => e instanceof ProtocolError && e.code === Code.Invalid);
  end();
});

test("an unknown method is method_not_found and the connection stays usable", async () => {
  const { host, end } = await ready(weather());
  await assert.rejects(host.call("route.match", {}), (e: unknown) => e instanceof ProtocolError && e.code === Code.MethodNotFound);
  assert.deepEqual(await host.call("check.run", { check: "weather", config: {} }), { ok: true, detail: "ok" });
  end();
});

test("after shutdown requests are shutting_down", async () => {
  const { host, end } = await ready(weather());
  await host.call("shutdown", {});
  await assert.rejects(host.call("check.run", { check: "weather", config: {} }), (e: unknown) => e instanceof ProtocolError && e.code === Code.ShuttingDown);
  end();
});

test("the connection ends when the host closes stdin", async () => {
  const { server, end } = await ready(weather());
  end();
  await server.done;
});

test("an operation call decodes its input and sends its output", async () => {
  const { host, end } = await ready(weather());
  const out = await host.call("operation.call", { operation: "weather.forecast", authority, input: { schema: "weather.forecast.request.v1", data: { city: "london", days: 3 } } });
  assert.deepEqual(out, { output: { schema: "weather.forecast.v1", data: { city: "london", days: 3 } } });
  end();
});

test("a malformed input is refused with bad_request before the handler runs", async () => {
  const { host, end } = await ready(weather());
  const cases: [unknown, string][] = [
    [{}, 'weather.forecast: missing field "city"'],
    [{ city: "london", wind: true }, 'weather.forecast: unknown field "wind"'],
    [{ city: 7 }, 'weather.forecast: field "city" has the wrong type (got a JSON number)'],
    [{ city: "london", days: 1.5 }, 'weather.forecast: field "days" has the wrong type (got a JSON number)'],
    [{ city: "london", units: "kelvin" }, 'weather.forecast: field "units" must be one of "metric", "imperial"'],
    [[1], "weather.forecast: the input must be a JSON object"],
  ];
  for (const [data, message] of cases) {
    await assert.rejects(host.call("operation.call", { operation: "weather.forecast", authority, input: { data } }), (e: unknown) => e instanceof OperationError && e.code === "bad_request" && e.message === message, JSON.stringify(data));
  }
  end();
});

test("an OperationError reaches the caller verbatim and anything else is internal", async () => {
  const ext = weather().operation("weather.broken", { handler: () => { throw new Error("secret detail"); } });
  const { host, end } = await ready(ext);
  await assert.rejects(host.call("operation.call", { operation: "weather.forecast", authority, input: { data: { city: "atlantis" } } }), (e: unknown) => e instanceof OperationError && e.code === "unknown_city" && e.message === "no forecast for atlantis");
  await assert.rejects(host.call("operation.call", { operation: "weather.broken", authority, input: {} }), (e: unknown) => e instanceof ProtocolError && e.code === Code.Internal && !e.message.includes("secret"));
  end();
});

test("an undeclared operation is refused", async () => {
  const { host, end } = await ready(weather());
  await assert.rejects(host.call("operation.call", { operation: "weather.nope", authority, input: {} }), (e: unknown) => e instanceof ProtocolError && e.code === Code.Undeclared);
  await assert.rejects(host.call("operation.wait", { operation: "weather.forecast", authority, input: {} }), (e: unknown) => e instanceof ProtocolError && e.code === Code.Undeclared);
  end();
});

test("a handler calls the host while it serves", async () => {
  const kv = new Map<string, { value: string; version: string }>();
  const calls: string[] = [];
  const hostHandler: Handler = async (method, params) => {
    calls.push(method);
    const p = params as Record<string, unknown>;
    switch (method) {
      case "kv.get": {
        const e = kv.get(`${p.namespace}/${p.key}`);
        return e === undefined ? { found: false, entry: { key: "", value: null, version: "" } } : { found: true, entry: { key: p.key, ...e } };
      }
      case "kv.commit": {
        for (const c of (p.checks ?? []) as { namespace: string; key: string; version?: string; absent?: boolean }[]) {
          const e = kv.get(`${c.namespace}/${c.key}`);
          if (c.absent ? e !== undefined : e?.version !== c.version) throw new ProtocolError(Code.Conflict, "conflict");
        }
        for (const put of (p.puts ?? []) as { namespace: string; key: string; value: string }[]) kv.set(`${put.namespace}/${put.key}`, { value: put.value, version: `v${kv.size + 1}` });
        return {};
      }
      case "secret.resolve":
        return { values: { token: "t0ken" } };
    }
    throw new ProtocolError(Code.MethodNotFound, method);
  };
  const ext = extension("counter", { version: "1.0.0" }).operation("counter.bump", {
    input: t.object({ name: t.string() }),
    handler: async ({ name }, { host, authority }) => {
      await host.update("counter.counts", name, (old) => String(Number(old === undefined ? "0" : text(old)) + 1));
      const entry = await host.get("counter.counts", name);
      const secrets = await host.secrets(authority.scope_id, ["token"]);
      return { count: Number(text(entry!.value)), token: secrets.token };
    },
  });
  const { host, end } = await ready(ext, hostHandler);
  await host.call("operation.call", { operation: "counter.bump", authority, input: { data: { name: "a" } } });
  const out = await host.call("operation.call", { operation: "counter.bump", authority, input: { data: { name: "a" } } });
  assert.deepEqual(out, { output: { schema: "counter.bump.v1", data: { count: 2, token: "t0ken" } } });
  assert.equal(Buffer.from(kv.get("counter.counts/a")!.value, "base64").toString(), "2");
  assert.deepEqual(calls.slice(0, 2), ["kv.get", "kv.commit"]);
  end();
});

test("configure runs the hook and reports the KV layout", async () => {
  const seen: string[] = [];
  const ext = extension("weather", { version: "0.1.0", kvVersion: 2 })
    .entryType("weather")
    .onMigrate((from, to) => void seen.push(`migrate ${from}->${to}`))
    .onConfigure((p) => void seen.push(`configure ${p.entries?.length}`));
  const { host, end } = await ready(ext);
  const out = await host.call("configure", { generation: "g1", entries: [{ scope_id: "s1", type: "weather", config: {} }], kv: { stored: 0, declared: 2 } });
  assert.deepEqual(out, { kv_version: 2 });
  assert.deepEqual(seen, ["migrate 0->2", "configure 1"]);
  end();
});

test("a failing check answers ok false with its message as detail", async () => {
  const ext = extension("weather", { version: "0.1.0" }).check("weather", () => { throw new Error("service unreachable"); });
  const { host, end } = await ready(ext);
  assert.deepEqual(await host.call("check.run", { check: "weather", config: {} }), { ok: false, detail: "service unreachable" });
  end();
});

test("the host cancelling a call aborts the handler's signal", async () => {
  let aborted!: () => void;
  const wasAborted = new Promise<void>((r) => (aborted = r));
  const ext = extension("slow", { version: "1.0.0" }).operation("slow.wait", {
    handler: (_in, { signal }) => new Promise((_resolve, reject) => signal.addEventListener("abort", () => { aborted(); reject(new CancelledError()); })),
  });
  const { host, end } = await ready(ext);
  const abort = new AbortController();
  const call = host.call("operation.call", { operation: "slow.wait", authority, input: {} }, abort.signal);
  abort.abort();
  await assert.rejects(call, CancelledError);
  await wasAborted;
  end();
});
