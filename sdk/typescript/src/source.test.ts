// Sources, routes, Work kinds, and wait and emit operations, served over an
// in-memory stream pair with a Conn standing in for the host. No process,
// no network, no clock.

import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import { Code, OperationError, ProtocolError } from "./errors.ts";
import { extension, type Extension } from "./extension.ts";
import { text } from "./host.ts";
import { Conn, type Handler } from "./jsonrpc.ts";
import { type Infer, type Type, matches, t } from "./schema.ts";
import {
  ConfigureResult,
  EmitCommit,
  EmitPlan,
  IngressDeliverParams,
  PushResult,
  RouteDecision,
  RouteSamplesResult,
  Snapshot,
  WaitPlan,
  WebhookHandleResult,
} from "./wire.ts";

function decode<S extends Type<unknown>>(schema: S, value: unknown): Infer<S> {
  assert.ok(matches(schema, value), `malformed: ${JSON.stringify(value)}`);
  return value;
}

const authority = {
  scope_id: "s1",
  work_id: "w1",
  execution_id: "e1",
  lease: { execution_id: "e1", owner: "o", epoch: 1 },
  expires_at: "2026-11-01T13:00:00Z",
};

const at = "2026-11-01T12:00:00Z";

async function ready(ext: Extension, hostHandler?: Handler) {
  const toExt = new PassThrough();
  const fromExt = new PassThrough();
  ext.serve(toExt, fromExt);
  const host = new Conn(
    fromExt,
    toExt,
    hostHandler ??
      (async (m) => {
        throw new ProtocolError(Code.MethodNotFound, m);
      }),
  );
  await host.call("initialize", {
    protocol_versions: [1],
    host: { name: "test", version: "0" },
    state_dir: "/state",
  });
  return { host, end: () => toExt.end() };
}

const families: [
  { type: string; re_derivable: boolean },
  ...{ type: string; re_derivable: boolean }[],
] = [
  { type: "desk.ticket.opened", re_derivable: true },
  { type: "desk.ticket.resolved", re_derivable: true },
];

function sign(secret: string, body: string): string {
  return createHmac("sha256", secret).update(body).digest("hex");
}

/** A helpdesk: tickets are a source, each ticket is one Work, and an agent waits for one to resolve. */
function desk(): Extension {
  return extension("desk", { version: "0.1.0" })
    .ingressType("desk", {
      config: t.object({
        namespace: t.string(),
        webhook_secret: t.optional(t.string()),
      }),
      families,
      webhookMaxBytes: 1 << 16,
      sources: (entries) =>
        entries.map((e) => ({
          name: `desk/${e.scopeId}/${e.name || "entry"}`,
          namespace: e.config.namespace,
          scopeId: e.scopeId,
          intervalSeconds: 300,
          webhook: e.config.webhook_secret !== undefined,
          data: { secret: e.config.webhook_secret ?? "" },
        })),
      push: (source, delivery) => {
        const ticket = JSON.parse(text(delivery.body)) as {
          id: string;
          status: string;
          version: string;
        };
        return {
          observations: [
            {
              id: ticket.id,
              version: ticket.version,
              event: {
                type: `desk.ticket.${ticket.status}`,
                source: source.namespace,
                correlation_key: {
                  source: source.namespace,
                  external_id: ticket.id,
                },
              },
            },
          ],
        };
      },
      reconcile: (source, scan) => ({
        scan_id: scan.scan_id || "scan-1",
        ...(scan.cursor
          ? { complete: true }
          : { next_cursor: "page-2", complete: false }),
        observations: [],
        checkpoint: `${source.namespace}@${scan.cursor || "start"}`,
      }),
      webhook: async (source, req, ctx) => {
        const want = sign(source.data!.secret, text(req.body));
        if (req.headers.get("x-desk-signature") !== want)
          return { status: 401 };
        const { added } = await ctx.deliver({
          id: req.headers.get("x-desk-delivery") ?? "",
          contentType: "application/json",
          body: req.body,
        });
        return { status: 202, body: added[0] ? "accepted" : "duplicate" };
      },
    })
    .route("desk", {
      config: t.object({ statuses: t.optional(t.array(t.string())) }),
      match: ({ event, work, config }) => {
        const e = event.event;
        if (work !== undefined)
          return { match: true, reason: "a change to a ticket with Work" };
        if (e.type !== "desk.ticket.opened")
          return { match: false, reason: "only an opened ticket opens Work" };
        if (config.statuses && !config.statuses.includes("opened"))
          return { match: false, reason: "a status this route doesn't admit" };
        const id = e.correlation_key?.external_id ?? "";
        return {
          match: true,
          create_id: `${e.source}/${id}`,
          kind: "desk.ticket",
          display: { title: `Ticket ${id}`, ref: id },
          reason: "an opened ticket",
        };
      },
      samples: ({ scope, now }) => [
        {
          id: "opened",
          title: "A ticket is opened",
          event: {
            type: "desk.ticket.opened",
            source: "desk",
            occurred_at: now,
            correlation_key: { source: "desk", external_id: scope.id },
          },
        },
      ],
    })
    .workKind("desk.ticket", {
      standing: true,
      // The latest input triggers.
      trigger: (inputs) => inputs.length - 1,
    })
    .wait("desk.wait_resolved", {
      input: t.object({ ticket: t.string() }),
      summary: "Wait until a ticket is resolved.",
      handler: ({ ticket }) => ({
        wait: {
          keys: [{ source: "desk", external_id: ticket }],
          types: ["desk.ticket.resolved"],
          description: `ticket ${ticket} resolved`,
        },
        output: { waiting_for: ticket },
      }),
    })
    .emit("desk.note", {
      input: t.object({ ticket: t.string(), note: t.string() }),
      commit: true,
      handler: ({ ticket, note }) => {
        if (note === "") throw new OperationError("empty", "a note needs text");
        return {
          event: {
            type: "desk.ticket.noted",
            correlation_key: { source: "desk", external_id: ticket },
          },
          once: `${ticket}:${note}`,
          commit: (emitted) => ({
            records: [
              { namespace: "desk", key: `note/${ticket}`, value: note },
            ],
            output: { event: emitted.event.event.id },
          }),
        };
      },
    })
    .family({ type: "desk.ticket.noted", re_derivable: false, loss: "notes" });
}

const entries = [
  {
    scope_id: "s1",
    type: "desk",
    name: "main",
    config: { namespace: "desk", webhook_secret: "hush" },
  },
  { scope_id: "s2", type: "desk", config: { namespace: "desk/eu" } },
];

async function configured(hostHandler?: Handler) {
  const c = await ready(desk(), hostHandler);
  const out = decode(
    ConfigureResult,
    await c.host.call("configure", {
      generation: "g1",
      entries,
      kv: { stored: 0, declared: 0 },
    }),
  );
  return { ...c, out };
}

test("an ingress type, route, Work kind and wait and emit operations declare themselves", () => {
  const p = desk().manifest.provides;
  assert.deepEqual(p.extensions, [{ type: "desk", ingress: true }]);
  assert.deepEqual(p.families, [
    ...families,
    { type: "desk.ticket.noted", re_derivable: false, loss: "notes" },
  ]);
  assert.equal(p.webhooks, true);
  assert.equal(p.webhook_max_bytes, 1 << 16);
  assert.deepEqual(p.routes, [
    { handler: "desk", config: true, samples: true },
  ]);
  assert.deepEqual(p.work_kinds, [
    { kind: "desk.ticket", standing: true, trigger: true },
  ]);
  assert.deepEqual(
    p.operations?.map((o) => [o.name, o.kind, o.commit]),
    [
      ["desk.wait_resolved", "wait", undefined],
      ["desk.note", "emit", true],
    ],
  );
});

test("configure registers each entry's source and the webhook paths of those that take one", async () => {
  const { out, end } = await configured();
  assert.deepEqual(out, {
    kv_version: 0,
    sources: [
      {
        name: "desk/s1/main",
        source: { namespace: "desk", scope_id: "s1" },
        interval_seconds: 300,
        families,
      },
      {
        name: "desk/s2/entry",
        source: { namespace: "desk/eu", scope_id: "s2" },
        interval_seconds: 300,
        families,
      },
    ],
    webhook_paths: ["/desk/s1/main"],
  });
  end();
});

test("a source outside the extension's namespace is refused at configure", async () => {
  const { host, end } = await ready(desk());
  await assert.rejects(
    host.call("configure", {
      generation: "g1",
      entries: [
        { scope_id: "s1", type: "desk", config: { namespace: "other" } },
      ],
      kv: { stored: 0, declared: 0 },
    }),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.Invalid,
  );
  end();
});

test("a verified webhook is delivered, pushed back through the source and answered once accepted", async () => {
  const delivered: unknown[] = [];
  let pushed: unknown;
  // The fake host answers ingress.deliver as the real one does: by
  // pushing the delivery to the source it names.
  let host!: Conn;
  const c = await configured(async (method, params) => {
    assert.equal(method, "ingress.deliver");
    const p = decode(IngressDeliverParams, params);
    delivered.push(p);
    pushed = decode(
      PushResult,
      await host.call("ingress.push", {
        name: p.name,
        source: { namespace: "desk", scope_id: "s1" },
        delivery: p.delivery,
      }),
    );
    return { accepted: [], added: [true] };
  });
  host = c.host;
  const body = JSON.stringify({ id: "T-9", status: "opened", version: "3" });
  const res = decode(
    WebhookHandleResult,
    await host.call("webhook.handle", {
      path: "/desk/s1/main",
      method: "POST",
      header: {
        "X-Desk-Signature": [sign("hush", body)],
        "X-Desk-Delivery": ["d-1"],
      },
      body: Buffer.from(body).toString("base64"),
      received_at: at,
    }),
  );
  assert.equal(res.status, 202);
  assert.equal(Buffer.from(res.body ?? "", "base64").toString(), "accepted");
  assert.deepEqual(delivered, [
    {
      name: "desk/s1/main",
      delivery: {
        id: "d-1",
        received_at: at,
        content_type: "application/json",
        body: Buffer.from(body).toString("base64"),
      },
    },
  ]);
  assert.deepEqual(pushed, {
    observations: [
      {
        id: "T-9",
        version: "3",
        event: {
          type: "desk.ticket.opened",
          source: "desk",
          correlation_key: { source: "desk", external_id: "T-9" },
        },
      },
    ],
  });
  c.end();
});

test("a webhook that fails its signature is refused, and a path no source serves is 404", async () => {
  const { host, end } = await configured(async () => {
    assert.fail("nothing is delivered");
  });
  const refused = decode(
    WebhookHandleResult,
    await host.call("webhook.handle", {
      path: "/desk/s1/main",
      method: "POST",
      header: { "X-Desk-Signature": ["forged"] },
      body: Buffer.from("{}").toString("base64"),
      received_at: at,
    }),
  );
  assert.equal(refused.status, 401);
  // s2's entry has no secret, so its source takes no webhook.
  for (const path of ["/desk/s2/entry", "/desk/nope"]) {
    const res = await host.call("webhook.handle", {
      path,
      method: "POST",
      received_at: at,
    });
    assert.deepEqual(res, { status: 404 });
  }
  end();
});

test("reconcile reads a source page by page", async () => {
  const { host, end } = await configured();
  const scan = { source: { namespace: "desk/eu", scope_id: "s2" } };
  const first = decode(
    Snapshot,
    await host.call("ingress.reconcile", { name: "desk/s2/entry", scan }),
  );
  assert.deepEqual(first, {
    scan_id: "scan-1",
    next_cursor: "page-2",
    complete: false,
    observations: [],
    checkpoint: "desk/eu@start",
  });
  const last = decode(
    Snapshot,
    await host.call("ingress.reconcile", {
      name: "desk/s2/entry",
      scan: { ...scan, scan_id: "scan-1", cursor: "page-2" },
    }),
  );
  assert.equal(last.complete, true);
  await assert.rejects(
    host.call("ingress.reconcile", { name: "desk/gone", scan }),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.NotFound,
  );
  end();
});

const scope = {
  id: "s1",
  name: "support",
  config: {},
  revision: 1,
  created_at: at,
  updated_at: at,
};

function routeInput(type: string, config: unknown, work?: unknown) {
  return {
    handler: "desk",
    input: {
      event: {
        event: {
          id: "ev1",
          type,
          source: "desk",
          correlation_key: { source: "desk", external_id: "T-9" },
        },
        sequence: 7,
        routed: false,
      },
      scope,
      ...(work === undefined ? {} : { work }),
      route: {
        name: "tickets",
        handler: "desk",
        binding: "agent",
        config: { data: config },
      },
    },
  };
}

test("a route opens Work with a display, reaches the Work an Event already has, and decodes its config", async () => {
  const { host, end } = await ready(desk());
  assert.deepEqual(
    decode(
      RouteDecision,
      await host.call("route.match", routeInput("desk.ticket.opened", {})),
    ),
    {
      match: true,
      create_id: "desk/T-9",
      kind: "desk.ticket",
      display: { title: "Ticket T-9", ref: "T-9" },
      reason: "an opened ticket",
    },
  );
  const filtered = decode(
    RouteDecision,
    await host.call(
      "route.match",
      routeInput("desk.ticket.opened", { statuses: ["resolved"] }),
    ),
  );
  assert.equal(filtered.match, false);
  const work = {
    id: "w1",
    kind: "desk.ticket",
    scope_id: "s1",
    state: "running",
    correlation_keys: null,
    context: {},
    revision: 2,
    created_at: at,
    updated_at: at,
  };
  const reached = decode(
    RouteDecision,
    await host.call(
      "route.match",
      routeInput("desk.ticket.resolved", {}, work),
    ),
  );
  assert.equal(reached.match, true);
  // A config the schema refuses never reaches match.
  await assert.rejects(
    host.call("route.match", routeInput("desk.ticket.opened", { statuses: 3 })),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.Invalid,
  );
  end();
});

test("a route's config is checked at validate, field by field", async () => {
  const { host, end } = await ready(desk());
  await host.call("validate", { kind: "route", type: "desk", config: {} });
  await assert.rejects(
    host.call("validate", {
      kind: "route",
      type: "desk",
      config: { statuses: "opened" },
    }),
    (e: unknown) =>
      e instanceof ProtocolError &&
      e.code === Code.Invalid &&
      e.diagnostics.some((d) => d.path === "/statuses"),
  );
  end();
});

test("a route offers samples built from its scope, as of the host's clock", async () => {
  const { host, end } = await ready(desk());
  const out = decode(
    RouteSamplesResult,
    await host.call("route.samples", {
      handler: "desk",
      scope,
      route: { name: "tickets", handler: "desk", binding: "agent", config: {} },
      now: at,
    }),
  );
  assert.deepEqual(out.samples, [
    {
      id: "opened",
      title: "A ticket is opened",
      event: {
        type: "desk.ticket.opened",
        source: "desk",
        occurred_at: at,
        correlation_key: { source: "desk", external_id: "s1" },
      },
    },
  ]);
  end();
});

test("a Work kind's trigger picks the input that triggers the next execution", async () => {
  const { host, end } = await ready(desk());
  const inputs = [
    { type: "desk.ticket.opened" },
    { type: "desk.ticket.noted" },
  ];
  assert.deepEqual(
    await host.call("work_kind.trigger", { kind: "desk.ticket", inputs }),
    { index: 1 },
  );
  end();
});

test("a wait operation returns the plan the host parks the Work on", async () => {
  const { host, end } = await ready(desk());
  const plan = decode(
    WaitPlan,
    await host.call("operation.wait", {
      operation: "desk.wait_resolved",
      authority,
      input: { data: { ticket: "T-9" } },
    }),
  );
  assert.deepEqual(plan, {
    wait: {
      keys: [{ source: "desk", external_id: "T-9" }],
      types: ["desk.ticket.resolved"],
      description: "ticket T-9 resolved",
    },
    output: {
      schema: "desk.wait_resolved.v1",
      data: { waiting_for: "T-9" },
    },
  });
  // Called as the wrong kind, it is refused rather than run.
  await assert.rejects(
    host.call("operation.call", {
      operation: "desk.wait_resolved",
      authority,
      input: { data: { ticket: "T-9" } },
    }),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.Undeclared,
  );
  // Its input is checked as strictly as a handler's.
  await assert.rejects(
    host.call("operation.wait", {
      operation: "desk.wait_resolved",
      authority,
      input: { data: {} },
    }),
    (e: unknown) => e instanceof OperationError && e.code === "bad_request",
  );
  end();
});

test("an emit operation plans its Event, and its commit writes records once it is accepted", async () => {
  const { host, end } = await ready(desk());
  const plan = decode(
    EmitPlan,
    await host.call("operation.emit", {
      operation: "desk.note",
      authority,
      input: { data: { ticket: "T-9", note: "called back" } },
    }),
  );
  assert.deepEqual(plan.event, {
    type: "desk.ticket.noted",
    correlation_key: { source: "desk", external_id: "T-9" },
  });
  assert.equal(plan.once, "T-9:called back");
  // Random, so a restarted process never reuses an ID the host still holds.
  assert.match(plan.commit_id ?? "", /^[0-9a-f]{24}$/);
  const emitted = {
    event: {
      event: { id: "ev2", type: "desk.ticket.noted" },
      sequence: 8,
      routed: false,
    },
    added: true,
    root: "ev0",
    depth: 1,
  };
  const commit = { commit_id: plan.commit_id, emitted };
  const out = decode(EmitCommit, await host.call("emit.commit", commit));
  assert.deepEqual(out, {
    records: [
      {
        namespace: "desk",
        key: "note/T-9",
        value: Buffer.from("called back").toString("base64"),
      },
    ],
    output: { schema: "desk.note.v1", data: { event: "ev2" } },
  });
  // The host may ask again when its transaction runs again.
  assert.deepEqual(await host.call("emit.commit", commit), out);
  await assert.rejects(
    host.call("emit.commit", { commit_id: "unknown", emitted }),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.NotFound,
  );
  await assert.rejects(
    host.call("operation.emit", {
      operation: "desk.note",
      authority,
      input: { data: { ticket: "T-9", note: "" } },
    }),
    (e: unknown) => e instanceof OperationError && e.code === "empty",
  );
  end();
});
