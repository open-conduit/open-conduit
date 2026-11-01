// The extension's client for the scoped extension-to-host methods, as
// protocol/sdk/host.go offers them. The host binds every call to the
// identity it started the process with: nothing here can name another
// extension's namespaces or scopes.

import { Code, ProtocolError } from "./errors.ts";
import { MaxChunkBytes } from "./jsonrpc.ts";
import type {
  AcceptedEvent,
  ArtifactCreateResult,
  ArtifactRef,
  Event,
  EventAcceptResult,
  KVCheck,
  KVDelete,
  KVEntryWire,
  KVGetResult,
  KVListResult,
  SecretResolveResult,
  Work,
} from "./wire.ts";

/** Makes one extension-to-host call. */
export type Caller = <T>(method: string, params: unknown, signal?: AbortSignal) => Promise<T>;

/** A KV value: bytes, or a string stored as its UTF-8 bytes. */
export type Value = Uint8Array | string;

/** A KV entry and its version (a digest of the value). */
export interface Entry {
  key: string;
  value: Uint8Array;
  version: string;
}

export interface Put {
  namespace: string;
  key: string;
  value: Value;
}

/** A precondition batch, applied atomically: a failed check changes nothing. */
export interface Batch {
  checks?: KVCheck[];
  puts?: Put[];
  deletes?: KVDelete[];
}

/** One Event derived from parent, committed atomically with the batch. */
export interface Accept extends Batch {
  scopeId: string;
  parent: string;
  event: Event;
}

export class Host {
  readonly #call: Caller;

  constructor(call: Caller) {
    this.#call = call;
  }

  /** Makes any extension-to-host call. */
  call<T = unknown>(method: string, params: unknown = {}, signal?: AbortSignal): Promise<T> {
    return this.#call<T>(method, params, signal);
  }

  /** A KV value and its version, or undefined when the key is absent. */
  async get(namespace: string, key: string, signal?: AbortSignal): Promise<Entry | undefined> {
    const out = await this.#call<KVGetResult>("kv.get", { namespace, key }, signal);
    return out.found ? entry(out.entry) : undefined;
  }

  /** The entries under prefix, in lexical key order. */
  async list(namespace: string, prefix = "", signal?: AbortSignal): Promise<Entry[]> {
    const params: Record<string, string> = { namespace };
    if (prefix !== "") params.prefix = prefix;
    const out = await this.#call<KVListResult>("kv.list", params, signal);
    return (out.entries ?? []).map(entry);
  }

  /** Applies a batch atomically; a failed check throws ProtocolError conflict. */
  async commit(b: Batch, signal?: AbortSignal): Promise<void> {
    await this.#call("kv.commit", batchFields(b), signal);
  }

  /**
   * Reads a key, applies fn and commits with a version check, retrying on
   * conflict. fn returns undefined to delete the key.
   */
  async update(namespace: string, key: string, fn: (old: Uint8Array | undefined) => Value | undefined | Promise<Value | undefined>, signal?: AbortSignal): Promise<void> {
    for (let attempt = 0; attempt < 16; attempt++) {
      const old = await this.get(namespace, key, signal);
      const value = await fn(old?.value);
      const check: KVCheck = old === undefined ? { namespace, key, absent: true } : { namespace, key, version: old.version };
      const b: Batch = value === undefined ? { checks: [check], deletes: [{ namespace, key }] } : { checks: [check], puts: [{ namespace, key, value }] };
      try {
        return await this.commit(b, signal);
      } catch (err) {
        if (!(err instanceof ProtocolError && err.code === Code.Conflict)) throw err;
      }
    }
    throw new ProtocolError(Code.Conflict, "update kept conflicting");
  }

  /**
   * Derives one Event from parent (an accepted Event in a configured scope),
   * committing the batch's KV writes atomically with it.
   */
  async accept(a: Accept, signal?: AbortSignal): Promise<{ event: AcceptedEvent; added: boolean }> {
    const { puts, ...rest } = batchFields(a);
    // event.accept names its puts "records".
    const params: Record<string, unknown> = { scope_id: a.scopeId, parent: a.parent, event: a.event, ...rest };
    if (puts !== undefined) params.records = puts;
    return this.#call<EventAcceptResult>("event.accept", params, signal);
  }

  /** Accepted Events of the configured scopes after a sequence. */
  async events(after = 0, limit = 0, signal?: AbortSignal): Promise<AcceptedEvent[]> {
    const params: Record<string, number> = {};
    if (after > 0) params.after = after;
    if (limit > 0) params.limit = limit;
    return (await this.#call<AcceptedEvent[] | null>("state.events", params, signal)) ?? [];
  }

  /** Work of the configured scopes (one scope when scopeId is set). */
  async works(scopeId = "", signal?: AbortSignal): Promise<Work[]> {
    return (await this.#call<Work[] | null>("state.works", scopeId === "" ? {} : { scope_id: scopeId }, signal)) ?? [];
  }

  /** Resolves secret names bound to the extension's entry in a scope. */
  async secrets(scopeId: string, names: string[], signal?: AbortSignal): Promise<Record<string, string>> {
    const out = await this.#call<SecretResolveResult>("secret.resolve", { scope_id: scopeId, names }, signal);
    return out.values ?? {};
  }

  /**
   * Resolves secret names of a connection an entry or binding in the scope
   * names (an operation's authority.connection).
   */
  async connectionSecrets(scopeId: string, connection: string, names: string[], signal?: AbortSignal): Promise<Record<string, string>> {
    const out = await this.#call<SecretResolveResult>("secret.resolve", { scope_id: scopeId, connection, names }, signal);
    return out.values ?? {};
  }

  /** Writes an immutable artifact in acknowledged chunks. */
  async artifact(mediaType: string, data: Value, signal?: AbortSignal): Promise<ArtifactRef> {
    const bytes = toBytes(data);
    const { writer } = await this.#call<ArtifactCreateResult>("artifact.create", { media_type: mediaType }, signal);
    try {
      for (let at = 0; at < bytes.length; at += MaxChunkBytes) {
        await this.#call("artifact.write", { writer, chunk: base64(bytes.subarray(at, at + MaxChunkBytes)) }, signal);
      }
    } catch (err) {
      await this.#call("artifact.abort", { writer }).catch(() => {});
      throw err;
    }
    return this.#call<ArtifactRef>("artifact.commit", { writer }, signal);
  }

  /** Sets the progress line of a Work's active Wait ("" clears it). */
  async waitProgress(workId: string, progress: string, signal?: AbortSignal): Promise<void> {
    await this.#call("wait.progress", { work_id: workId, progress }, signal);
  }
}

function entry(e: KVEntryWire): Entry {
  return { key: e.key, value: e.value === null ? new Uint8Array() : new Uint8Array(Buffer.from(e.value, "base64")), version: e.version };
}

function batchFields(b: Batch): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (b.checks?.length) out.checks = b.checks;
  if (b.puts?.length) out.puts = b.puts.map((p) => ({ namespace: p.namespace, key: p.key, value: base64(toBytes(p.value)) }));
  if (b.deletes?.length) out.deletes = b.deletes;
  return out;
}

function toBytes(v: Value): Uint8Array {
  return typeof v === "string" ? new TextEncoder().encode(v) : v;
}

function base64(b: Uint8Array): string {
  return Buffer.from(b.buffer, b.byteOffset, b.byteLength).toString("base64");
}

/** Decodes a KV value as UTF-8 text. */
export function text(v: Uint8Array): string {
  return new TextDecoder().decode(v);
}
