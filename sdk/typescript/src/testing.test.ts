// The test host serving a source-owning extension: what it accepts, writes
// and refuses, as the daemon would. No process, network or real clock.

import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";
import { Code, OperationError, ProtocolError } from "./errors.ts";
import { extension, type Extension } from "./extension.ts";
import { text } from "./host.ts";
import { t } from "./schema.ts";
import { TestClock, testHost, testStart } from "./testing.ts";

const sign = (secret: string, body: string) =>
  createHmac("sha256", secret).update(body).digest("hex");

/** A helpdesk: each ticket is one Work, and an agent can wait for one to resolve. */
function desk(): Extension {
  return extension("desk", { version: "0.1.0" })
    .ingressType("desk", {
      config: t.object({ secret: t.string() }),
      families: [
        { type: "desk.ticket.opened", re_derivable: true },
        { type: "desk.ticket.resolved", re_derivable: true },
      ],
      sources: (entries) =>
        entries.map((e) => ({
          name: `desk/${e.scopeId}`,
          namespace: "desk",
          scopeId: e.scopeId,
          intervalSeconds: 60,
          data: { secret: e.config.secret },
        })),
      push: (source, d) => {
        const ticket = JSON.parse(text(d.body)) as {
          id: string;
          status: string;
        };
        return {
          observations: [
            {
              id: ticket.id,
              version: ticket.status,
              event: {
                type: `desk.ticket.${ticket.status}`,
                delivery_id: `${ticket.id}@${ticket.status}`,
                correlation_key: {
                  source: source.namespace,
                  external_id: ticket.id,
                },
              },
            },
          ],
        };
      },
      reconcile: (_source, scan) =>
        scan.cursor
          ? { scan_id: scan.scan_id ?? "", complete: true }
          : { scan_id: "s-1", next_cursor: "2", complete: false },
      webhook: async (source, req, ctx) => {
        if (
          req.headers.get("x-sig") !== sign(source.data!.secret, text(req.body))
        )
          return { status: 401 };
        const { added } = await ctx.deliver({
          id: req.headers.get("x-id") ?? "",
          body: req.body,
        });
        return { status: added.every(Boolean) ? 202 : 200 };
      },
    })
    .route("desk", {
      match: ({ event }) => ({
        match: event.event.type === "desk.ticket.opened",
        create_id: `desk/${event.event.correlation_key?.external_id}`,
        kind: "desk.ticket",
      }),
      samples: ({ now }) => [
        {
          id: "opened",
          title: "A ticket opens",
          event: { type: "desk.ticket.opened", occurred_at: now },
        },
      ],
    })
    .workKind("desk.ticket", { trigger: (inputs) => inputs.length - 1 })
    .wait("desk.wait_resolved", {
      input: t.object({ ticket: t.string() }),
      handler: ({ ticket }) => ({
        wait: {
          keys: [{ source: "desk", external_id: ticket }],
          types: ["desk.ticket.resolved"],
        },
        output: { ticket },
      }),
    })
    .emit("desk.note", {
      input: t.object({ ticket: t.string(), note: t.string() }),
      commit: true,
      handler: ({ ticket, note }) => ({
        event: { type: "desk.ticket.noted", delivery_id: `${ticket}/${note}` },
        commit: () => ({
          records: [{ namespace: "desk", key: `note/${ticket}`, value: note }],
          output: { noted: ticket },
        }),
      }),
    })
    .family({ type: "desk.ticket.noted", re_derivable: false, loss: "notes" })
    .operation("desk.token", {
      input: t.object({}),
      handler: async (_in, { host, authority }) => {
        const v = await host.secrets(authority.scope_id, ["token"]);
        return { token: v.token ?? "" };
      },
    })
    .operation("desk.live", {
      input: t.object({}),
      handler: async (_in, { host }) => ({
        ids: (await host.queryWorks({ states: ["running"] })).map((w) => w.id),
      }),
    })
    .operation("desk.claim", {
      input: t.object({ ticket: t.string() }),
      handler: async ({ ticket }, { host }) => {
        // Absent-only: a second claim conflicts.
        await host.commit({
          checks: [{ namespace: "desk", key: `claim/${ticket}`, absent: true }],
          puts: [{ namespace: "desk", key: `claim/${ticket}`, value: "mine" }],
        });
        return {};
      },
    });
}

async function configured(o: Parameters<typeof testHost>[1] = {}) {
  const host = await testHost(desk(), o);
  await host.configure([
    { scope_id: "s1", type: "desk", config: { secret: "hush" } },
  ]);
  return host;
}

test("configure registers the extension's sources and webhook paths", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  assert.deepEqual(
    host.sources().map((s) => s.name),
    ["desk/s1"],
  );
});

test("a signed webhook is delivered through push and accepted once; a retry adds nothing", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  const body = JSON.stringify({ id: "T-1", status: "opened" });
  const req = { headers: { "X-Sig": sign("hush", body), "X-Id": "d1" }, body };
  assert.equal((await host.webhook("/desk/s1", req)).status, 202);
  assert.equal((await host.webhook("/desk/s1", req)).status, 200);
  const events = host.events();
  assert.equal(events.length, 1);
  assert.equal(events[0]?.event.type, "desk.ticket.opened");
  assert.equal(events[0]?.event.source, "desk");
  assert.equal(events[0]?.event.received_at, testStart);
  // A forged one is refused before anything is accepted, and an unregistered path never reaches the extension.
  assert.equal(
    (await host.webhook("/desk/s1", { ...req, headers: { "X-Sig": "no" } }))
      .status,
    401,
  );
  assert.equal((await host.webhook("/desk/s9", req)).status, 404);
  assert.equal(host.events().length, 1);
});

test("reconcileAll reads a source to its last page", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  const pages = await host.reconcileAll("desk/s1");
  assert.deepEqual(
    pages.map((p) => [p.next_cursor, p.complete]),
    [
      ["2", false],
      [undefined, true],
    ],
  );
});

test("the clock moves only when the test moves it", async (t) => {
  const clock = new TestClock();
  const host = await configured({ clock });
  t.after(() => host.close());
  clock.advance(90_000);
  const samples = await host.samples(
    "desk",
    {
      id: "s1",
      name: "s1",
      config: {},
      revision: 1,
      created_at: testStart,
      updated_at: testStart,
    },
    { name: "r", handler: "desk", binding: "b", config: {} },
  );
  assert.equal(samples[0]?.event.occurred_at, "2026-10-24T12:01:30.000Z");
  await host.push("desk/s1", {
    id: "d2",
    body: JSON.stringify({ id: "T-2", status: "opened" }),
  });
  assert.equal(host.events()[0]?.event.received_at, "2026-10-24T12:01:30.000Z");
});

test("routes, triggers and wait operations answer through the host", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  const decision = await host.route("desk", {
    event: {
      type: "desk.ticket.opened",
      correlation_key: { source: "desk", external_id: "T-3" },
    },
    scope: {
      id: "s1",
      name: "s1",
      config: {},
      revision: 1,
      created_at: testStart,
      updated_at: testStart,
    },
    route: { name: "r", handler: "desk", binding: "b", config: {} },
  });
  assert.equal(decision.create_id, "desk/T-3");
  assert.equal(
    await host.trigger("desk.ticket", [{ type: "a" }, { type: "b" }]),
    1,
  );
  const waited = await host.operation("desk.wait_resolved", { ticket: "T-3" });
  assert.deepEqual(waited.output, { ticket: "T-3" });
  assert.deepEqual(waited.wait?.wait.types, ["desk.ticket.resolved"]);
});

test("an emission is accepted for the calling execution and its commit writes KV", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  const out = await host.operation("desk.note", {
    ticket: "T-4",
    note: "rang",
  });
  assert.deepEqual(out.output, { noted: "T-4" });
  assert.equal(out.event?.event.source, "execution/sdktest-execution");
  assert.equal(out.event?.event.work_id, "sdktest-work");
  assert.equal(host.kvText("desk", "note/T-4"), "rang");
  // The same emission again is a duplicate: nothing new is accepted or written.
  const again = await host.operation("desk.note", {
    ticket: "T-4",
    note: "rang",
  });
  assert.equal(again.added, false);
  assert.equal(host.events().length, 1);
});

test("secrets come from options, scoped to configured scopes", async (t) => {
  const host = await configured({ secrets: { s1: { token: "t0k" } } });
  t.after(() => host.close());
  assert.deepEqual((await host.operation("desk.token")).output, {
    token: "t0k",
  });
  await assert.rejects(
    host.operation("desk.token", {}, { scope_id: "elsewhere" }),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.Unauthorized,
  );
});

test("state reads see seeded Work, filtered as the host filters them", async (t) => {
  const work = (id: string, state: string) => ({
    id,
    kind: "desk.ticket",
    scope_id: "s1",
    state,
    correlation_keys: null,
    context: {},
    revision: 1,
    created_at: testStart,
    updated_at: testStart,
  });
  const host = await configured({
    works: [
      work("w1", "running"),
      work("w2", "completed"),
      work("w3", "running"),
    ],
  });
  t.after(() => host.close());
  assert.deepEqual((await host.operation("desk.live")).output, {
    ids: ["w1", "w3"],
  });
});

test("a KV precondition that fails conflicts and writes nothing", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  await host.operation("desk.claim", { ticket: "T-5" });
  assert.equal(host.kvText("desk", "claim/T-5"), "mine");
  await assert.rejects(
    host.operation("desk.claim", { ticket: "T-5" }),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.Conflict,
  );
});

test("an operation's own refusal reaches the test as it reaches an agent", async (t) => {
  const host = await configured();
  t.after(() => host.close());
  await assert.rejects(
    host.operation("desk.note", { ticket: "T-6" }),
    (e: unknown) => e instanceof OperationError && e.code === "bad_request",
  );
  await assert.rejects(
    host.operation("desk.nope"),
    (e: unknown) => e instanceof ProtocolError && e.code === Code.Undeclared,
  );
});
