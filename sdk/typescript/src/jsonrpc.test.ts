import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import { Code, ProtocolError, TransportError } from "./errors.ts";
import { Conn } from "./jsonrpc.ts";

function peer(maxMessage?: number) {
  const input = new PassThrough();
  const output = new PassThrough();
  const reasons: string[] = [];
  const o =
    maxMessage === undefined
      ? { onProtocolError: (r: string) => void reasons.push(r) }
      : { maxMessage, onProtocolError: (r: string) => void reasons.push(r) };
  const conn = new Conn(input, output, async (method) => ({ method }), o);
  const lines: unknown[] = [];
  output.on("data", (b: Buffer) =>
    b
      .toString()
      .split("\n")
      .filter(Boolean)
      .forEach((l) => lines.push(JSON.parse(l))),
  );
  return { conn, input, lines, reasons };
}

test("a request split across chunks is served once it is whole", async () => {
  const { input, lines, conn } = peer();
  input.write('{"jsonrpc":"2.0","id":1,"me');
  input.write('thod":"ping"}\n');
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(lines, [
    { jsonrpc: "2.0", id: 1, result: { method: "ping" } },
  ]);
  conn.close();
});

test("a line over the bound closes the connection and fails pending calls", async () => {
  const { conn, input, reasons } = peer(64);
  const pending = conn.call("slow", {});
  input.write(`{"jsonrpc":"2.0","method":"x","params":"${"a".repeat(100)}"}\n`);
  await assert.rejects(pending, TransportError);
  assert.deepEqual(reasons, ["message exceeds the protocol bound"]);
});

test("invalid JSON and a truncated final line are protocol errors", async () => {
  const a = peer();
  a.input.write("not json\n");
  await a.conn.done;
  assert.deepEqual(a.reasons, ["malformed message"]);
  const b = peer();
  b.input.end('{"jsonrpc":"2.0","id":1,"result":');
  await b.conn.done;
  assert.deepEqual(b.reasons, ["truncated message"]);
});

test("over the inflight bound a request is refused as overloaded", async () => {
  const input = new PassThrough();
  const output = new PassThrough();
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  const conn = new Conn(input, output, async () => held, { maxInflight: 1 });
  const lines: { id: number; error?: { data: { code: string } } }[] = [];
  output.on("data", (b: Buffer) =>
    b
      .toString()
      .split("\n")
      .filter(Boolean)
      .forEach((l) => lines.push(JSON.parse(l))),
  );
  input.write(
    '{"jsonrpc":"2.0","id":1,"method":"a"}\n{"jsonrpc":"2.0","id":2,"method":"b"}\n',
  );
  await new Promise((r) => setImmediate(r));
  assert.equal(lines[0]?.id, 2);
  assert.equal(lines[0]?.error?.data.code, Code.Overloaded);
  release();
  conn.close();
});

test("an authored error crosses with its code", async () => {
  const input = new PassThrough();
  const output = new PassThrough();
  const conn = new Conn(input, output, async () => {
    throw new ProtocolError(Code.Conflict, "version moved");
  });
  const lines: unknown[] = [];
  output.on("data", (b: Buffer) =>
    b
      .toString()
      .split("\n")
      .filter(Boolean)
      .forEach((l) => lines.push(JSON.parse(l))),
  );
  input.write('{"jsonrpc":"2.0","id":"x","method":"kv.commit"}\n');
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(lines, [
    {
      jsonrpc: "2.0",
      id: "x",
      error: {
        code: -32000,
        message: "version moved",
        data: { code: "conflict" },
      },
    },
  ]);
  conn.close();
});
