// The protocol version 1 transport, as protocol/jsonrpc implements it:
// JSON-RPC 2.0 over a pair of byte streams, one JSON object per line. The
// connection is symmetric: either side sends requests, which is how an
// extension calls the host (KV, events, secrets) while serving a host
// request.
//
//   - over maxInflight concurrently served requests, a request is refused
//     with "overloaded" rather than queued;
//   - a line longer than maxMessage, invalid JSON or a truncated final line
//     closes the connection, failing every pending call with TransportError;
//   - authored errors cross as -32000 with data.code; anything else crosses
//     as "internal" with no detail;
//   - "$/cancel" aborts the peer's handler signal; a cancelled call rejects
//     at once with CancelledError and a late response is discarded.

import type { Readable, Writable } from "node:stream";
import { CancelledError, Code, ProtocolError, TransportError, decode, encode } from "./errors.ts";
import { type Infer, isRecord, matches, t } from "./schema.ts";

/** Bounds of protocol version 1 (protocol/protocol.go). */
export const MaxMessageBytes = 16 << 20;
export const MaxChunkBytes = 256 << 10;
export const MaxInflight = 64;
export const CancelMethod = "$/cancel";

const codeParse = -32700;
const codeInvalidRequest = -32600;
const codeMethodNotFound = -32601;
const codeInvalidParams = -32602;
const codeInternal = -32603;
const codeCancelled = -32800;
const codeApplication = -32000;

/** Serves one inbound request. Its signal aborts when the peer cancels. */
export type Handler = (method: string, params: unknown, signal: AbortSignal) => Promise<unknown>;

export interface ConnOptions {
  maxMessage?: number;
  maxInflight?: number;
  /** Receives why a malformed stream closed the connection. */
  onProtocolError?: (reason: string) => void;
}

const WireError = t.object({
  code: t.integer(),
  message: t.string(),
  data: t.optional(t.object({ code: t.string(), reset_at: t.optional(t.string()) }, { open: true })),
}, { open: true });
type WireError = Infer<typeof WireError>;

// Only the envelope is decoded here; params and results are decoded by
// whoever knows their schema.
const Message = t.object({
  jsonrpc: t.literal("2.0"),
  id: t.optional(t.nullable(t.any())),
  method: t.optional(t.string()),
  params: t.optional(t.any()),
  result: t.optional(t.any()),
  error: t.optional(t.nullable(WireError)),
}, { open: true });
type Message = Infer<typeof Message>;

type ID = number | string;

function isID(v: unknown): v is ID {
  return typeof v === "number" || typeof v === "string";
}

interface Pending {
  resolve: (v: unknown) => void;
  reject: (e: Error) => void;
}

/** One JSON-RPC peer. */
export class Conn {
  readonly #handler: Handler;
  readonly #out: Writable;
  readonly #maxMessage: number;
  readonly #maxInflight: number;
  readonly #onProtocolError: ((reason: string) => void) | undefined;
  readonly #pending = new Map<string, Pending>();
  readonly #cancels = new Map<string, AbortController>();
  #next = 0;
  #inflight = 0;
  #closed = false;
  #buffered: Buffer[] = [];
  #bufferedBytes = 0;
  readonly done: Promise<void>;
  #finish!: () => void;

  constructor(input: Readable, output: Writable, handler: Handler, o: ConnOptions = {}) {
    this.#handler = handler;
    this.#out = output;
    this.#maxMessage = o.maxMessage ?? MaxMessageBytes;
    this.#maxInflight = o.maxInflight ?? MaxInflight;
    this.#onProtocolError = o.onProtocolError;
    this.done = new Promise((resolve) => {
      this.#finish = resolve;
    });
    input.on("data", (chunk: Buffer | string) => this.#read(typeof chunk === "string" ? Buffer.from(chunk) : chunk));
    input.on("end", () => {
      // A partial final line (a response cut off by a crash) is a protocol
      // error, never a result.
      if (this.#bufferedBytes > 0) this.#protocolError("truncated message");
      else this.close();
    });
    input.on("error", () => this.close());
    output.on("error", () => this.close());
  }

  /** Reports whether the connection has ended. */
  get closed(): boolean {
    return this.#closed;
  }

  /** Ends the connection; pending calls reject with TransportError. */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    for (const c of this.#cancels.values()) c.abort();
    for (const p of this.#pending.values()) p.reject(new TransportError());
    this.#pending.clear();
    this.#finish();
  }

  #protocolError(reason: string): void {
    this.#onProtocolError?.(reason);
    this.close();
  }

  #read(chunk: Buffer): void {
    let start = 0;
    for (;;) {
      const nl = chunk.indexOf(0x0a, start);
      if (nl < 0) break;
      const part = chunk.subarray(start, nl);
      if (this.#bufferedBytes + part.length >= this.#maxMessage) return this.#protocolError("message exceeds the protocol bound");
      const line = this.#bufferedBytes === 0 ? part : Buffer.concat([...this.#buffered, part]);
      this.#buffered = [];
      this.#bufferedBytes = 0;
      start = nl + 1;
      if (line.length > 0) this.#message(line);
      if (this.#closed) return;
    }
    const rest = chunk.subarray(start);
    if (rest.length === 0) return;
    if (this.#bufferedBytes + rest.length >= this.#maxMessage) return this.#protocolError("message exceeds the protocol bound");
    this.#buffered.push(Buffer.from(rest));
    this.#bufferedBytes += rest.length;
  }

  #message(line: Buffer): void {
    let m: unknown;
    try {
      m = JSON.parse(line.toString("utf8"));
    } catch {
      return this.#protocolError("malformed message");
    }
    if (!matches(Message, m)) return this.#protocolError("malformed message");
    const { id, method } = m;
    if (method === CancelMethod) {
      if (isRecord(m.params)) this.#cancels.get(JSON.stringify(m.params.id))?.abort();
    } else if (method !== undefined && method !== "" && isID(id)) {
      // Refuse rather than queue: the reader also carries the responses
      // in-flight handlers wait for, so blocking it would deadlock them.
      if (this.#inflight >= this.#maxInflight) {
        this.#write({ jsonrpc: "2.0", id, error: { code: codeApplication, message: "too many requests in flight", data: { code: Code.Overloaded } } });
        return;
      }
      void this.#serve(id, method, m.params);
    } else if (method !== undefined && method !== "") {
      // Other notifications are served without a reply.
      void this.#handler(method, m.params, new AbortController().signal).catch(() => {});
    } else if (isID(id)) {
      const key = JSON.stringify(id);
      const p = this.#pending.get(key);
      this.#pending.delete(key);
      if (p === undefined) return;
      if (m.error !== undefined && m.error !== null) p.reject(decodeError(m.error));
      else p.resolve(m.result);
    } else {
      this.#protocolError("message is neither request nor response");
    }
  }

  async #serve(id: ID, method: string, params: unknown): Promise<void> {
    const key = JSON.stringify(id);
    const abort = new AbortController();
    this.#cancels.set(key, abort);
    this.#inflight++;
    let reply: Message;
    try {
      const result = await this.#handler(method, params, abort.signal);
      reply = abort.signal.aborted ? { jsonrpc: "2.0", id, error: encodeError(new CancelledError(), true) } : { jsonrpc: "2.0", id, result: result === undefined ? {} : result };
    } catch (err) {
      reply = { jsonrpc: "2.0", id, error: encodeError(err, abort.signal.aborted) };
    } finally {
      this.#inflight--;
      this.#cancels.delete(key);
    }
    if (this.#closed) return;
    if (!this.#write(reply) && reply.error === undefined) {
      // An oversized result is refused rather than closing the stream.
      this.#write({ jsonrpc: "2.0", id, error: encodeError(new ProtocolError(Code.Invalid, "message exceeds the protocol bound"), false) });
    }
  }

  /** Writes one message; false when it is over the bound or unencodable. */
  #write(m: Message): boolean {
    if (this.#closed) return false;
    let data: Buffer;
    try {
      data = Buffer.from(JSON.stringify(m) + "\n", "utf8");
    } catch {
      return false;
    }
    if (data.length - 1 >= this.#maxMessage) return false;
    this.#out.write(data);
    return true;
  }

  /**
   * Sends a request and resolves with its result, undecoded. Aborting
   * signal sends $/cancel and rejects at once with CancelledError.
   */
  call(method: string, params: unknown = {}, signal?: AbortSignal): Promise<unknown> {
    if (this.#closed) return Promise.reject(new TransportError());
    if (signal?.aborted) return Promise.reject(new CancelledError());
    const id = ++this.#next;
    const key = JSON.stringify(id);
    return new Promise<unknown>((resolve, reject) => {
      const onAbort = () => {
        if (!this.#pending.delete(key)) return;
        this.#write({ jsonrpc: "2.0", method: CancelMethod, params: { id } });
        reject(new CancelledError());
      };
      this.#pending.set(key, {
        resolve: (v) => {
          signal?.removeEventListener("abort", onAbort);
          resolve(v);
        },
        reject: (e) => {
          signal?.removeEventListener("abort", onAbort);
          reject(e);
        },
      });
      signal?.addEventListener("abort", onAbort, { once: true });
      if (!this.#write({ jsonrpc: "2.0", id, method, params })) {
        this.#pending.delete(key);
        signal?.removeEventListener("abort", onAbort);
        reject(new ProtocolError(Code.Invalid, "message exceeds the protocol bound"));
      }
    });
  }

  /** Sends a notification (no response). */
  notify(method: string, params: unknown = {}): void {
    this.#write({ jsonrpc: "2.0", method, params });
  }
}

function encodeError(err: unknown, cancelled: boolean): WireError {
  if (cancelled) return { code: codeCancelled, message: "request cancelled", data: { code: Code.Cancelled } };
  const e = encode(err);
  if (e.code === Code.Cancelled) return { code: codeCancelled, message: e.message, data: { code: e.code } };
  const code = e.code === Code.MethodNotFound ? codeMethodNotFound : e.code === Code.Internal ? codeInternal : codeApplication;
  const data: { code: string; reset_at?: string } = { code: e.code };
  if (e.resetAt !== undefined) data.reset_at = e.resetAt;
  return { code, message: e.message, data };
}

function decodeError(e: WireError): Error {
  if (e.code === codeCancelled) return new CancelledError();
  if (e.data?.code) return decode(e.data.code, e.message, e.data.reset_at);
  if (e.code === codeMethodNotFound) return new ProtocolError(Code.MethodNotFound, e.message);
  if (e.code === codeInvalidParams || e.code === codeInvalidRequest || e.code === codeParse) return new ProtocolError(Code.Invalid, e.message);
  return new ProtocolError(Code.Internal, "internal error");
}
