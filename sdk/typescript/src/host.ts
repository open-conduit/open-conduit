// The extension's client for the scoped extension-to-host methods, as
// protocol/sdk/host.go offers them. The host binds every call to the
// identity it started the process with, so nothing here can name another
// extension's namespaces or scopes.

import { Code, ProtocolError } from "./errors.ts";
import { MaxChunkBytes } from "./jsonrpc.ts";
import { type Infer, type Type, matches, t } from "./schema.ts";
import {
  AcceptedEvent,
  ArtifactCreateResult,
  ArtifactRef,
  type Event,
  EventAcceptResult,
  type KVCheck,
  type KVDelete,
  KVGetResult,
  KVListResult,
  SecretResolveResult,
  Work,
} from "./wire.ts";

/** Makes one extension-to-host call; the result is decoded by the caller. */
export type Caller = (
  method: string,
  params: unknown,
  signal?: AbortSignal,
) => Promise<unknown>;

/** A KV value: bytes, or a string stored as its UTF-8 bytes. */
export type Value = Uint8Array | string;

/** A KV entry and its version, which a later commit can require unchanged. */
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

const Empty = t.any();

export class Host {
  readonly #call: Caller;

  constructor(call: Caller) {
    this.#call = call;
  }

  /** Makes any extension-to-host call, decoding its result through result. */
  async call<S extends Type<unknown>>(
    method: string,
    params: unknown,
    result: S,
    signal?: AbortSignal,
  ): Promise<Infer<S>> {
    const out = await this.#call(method, params, signal);
    // A result that doesn't match is refused, as the Go SDK's decoding refuses it.
    if (!matches(result, out))
      throw new ProtocolError(Code.Invalid, `malformed ${method} result`);
    return out;
  }

  /** A KV value and its version, or undefined when the key is absent. */
  async get(
    namespace: string,
    key: string,
    signal?: AbortSignal,
  ): Promise<Entry | undefined> {
    const out = await this.call(
      "kv.get",
      { namespace, key },
      KVGetResult,
      signal,
    );
    return out.found ? entry(out.entry) : undefined;
  }

  /** The entries under prefix, in lexical key order. */
  async list(
    namespace: string,
    prefix = "",
    signal?: AbortSignal,
  ): Promise<Entry[]> {
    const out = await this.call(
      "kv.list",
      prefix === "" ? { namespace } : { namespace, prefix },
      KVListResult,
      signal,
    );
    return (out.entries ?? []).map(entry);
  }

  /** Applies a batch atomically; a failed check throws ProtocolError conflict. */
  async commit(b: Batch, signal?: AbortSignal): Promise<void> {
    const { puts, ...rest } = batchFields(b);
    await this.call(
      "kv.commit",
      puts === undefined ? rest : { ...rest, puts },
      Empty,
      signal,
    );
  }

  /**
   * Reads a key, applies fn and commits with a version check, retrying when
   * another writer got there first. fn returns undefined to delete the key.
   */
  async update(
    namespace: string,
    key: string,
    fn: (
      old: Uint8Array | undefined,
    ) => Value | undefined | Promise<Value | undefined>,
    signal?: AbortSignal,
  ): Promise<void> {
    for (let attempt = 0; attempt < 16; attempt++) {
      const old = await this.get(namespace, key, signal);
      const value = await fn(old?.value);
      const check: KVCheck =
        old === undefined
          ? { namespace, key, absent: true }
          : { namespace, key, version: old.version };
      const b: Batch =
        value === undefined
          ? { checks: [check], deletes: [{ namespace, key }] }
          : { checks: [check], puts: [{ namespace, key, value }] };
      try {
        return await this.commit(b, signal);
      } catch (err) {
        if (!(err instanceof ProtocolError && err.code === Code.Conflict))
          throw err;
      }
    }
    throw new ProtocolError(Code.Conflict, "update kept conflicting");
  }

  /**
   * Derives one Event from parent (an accepted Event in a configured scope),
   * committing the batch's KV writes atomically with it.
   */
  async accept(a: Accept, signal?: AbortSignal): Promise<EventAcceptResult> {
    const { puts, ...rest } = batchFields(a);
    // event.accept calls its puts "records".
    const params = {
      scope_id: a.scopeId,
      parent: a.parent,
      event: a.event,
      ...rest,
    };
    return this.call(
      "event.accept",
      puts === undefined ? params : { ...params, records: puts },
      EventAcceptResult,
      signal,
    );
  }

  /** Accepted Events of the configured scopes after a sequence. */
  async events(
    after = 0,
    limit = 0,
    signal?: AbortSignal,
  ): Promise<AcceptedEvent[]> {
    const params = {
      ...(after > 0 ? { after } : {}),
      ...(limit > 0 ? { limit } : {}),
    };
    return (
      (await this.call(
        "state.events",
        params,
        t.nullable(t.array(AcceptedEvent)),
        signal,
      )) ?? []
    );
  }

  /** Work of the configured scopes (one scope when scopeId is set). */
  async works(scopeId = "", signal?: AbortSignal): Promise<Work[]> {
    return (
      (await this.call(
        "state.works",
        scopeId === "" ? {} : { scope_id: scopeId },
        t.nullable(t.array(Work)),
        signal,
      )) ?? []
    );
  }

  /** Resolves secret names bound to the extension's entry in a scope. */
  async secrets(
    scopeId: string,
    names: string[],
    signal?: AbortSignal,
  ): Promise<Record<string, string>> {
    const out = await this.call(
      "secret.resolve",
      { scope_id: scopeId, names },
      SecretResolveResult,
      signal,
    );
    return out.values ?? {};
  }

  /**
   * Resolves secret names of a connection an entry or binding in the scope
   * names, such as an operation's authority.connection.
   */
  async connectionSecrets(
    scopeId: string,
    connection: string,
    names: string[],
    signal?: AbortSignal,
  ): Promise<Record<string, string>> {
    const out = await this.call(
      "secret.resolve",
      { scope_id: scopeId, connection, names },
      SecretResolveResult,
      signal,
    );
    return out.values ?? {};
  }

  /** Writes an immutable artifact in chunks, each acknowledged before the next. */
  async artifact(
    mediaType: string,
    data: Value,
    signal?: AbortSignal,
  ): Promise<ArtifactRef> {
    const bytes = toBytes(data);
    const { writer } = await this.call(
      "artifact.create",
      { media_type: mediaType },
      ArtifactCreateResult,
      signal,
    );
    try {
      for (let at = 0; at < bytes.length; at += MaxChunkBytes) {
        await this.call(
          "artifact.write",
          { writer, chunk: base64(bytes.subarray(at, at + MaxChunkBytes)) },
          Empty,
          signal,
        );
      }
    } catch (err) {
      // Not under signal: a cancelled write must still release the writer.
      await this.call("artifact.abort", { writer }, Empty).catch(() => {});
      throw err;
    }
    return this.call("artifact.commit", { writer }, ArtifactRef, signal);
  }

  /** Sets the progress line of a Work's active Wait ("" clears it). */
  async waitProgress(
    workId: string,
    progress: string,
    signal?: AbortSignal,
  ): Promise<void> {
    await this.call(
      "wait.progress",
      { work_id: workId, progress },
      Empty,
      signal,
    );
  }
}

function entry(e: {
  key: string;
  value: string | null;
  version: string;
}): Entry {
  return {
    key: e.key,
    value:
      e.value === null
        ? new Uint8Array()
        : new Uint8Array(Buffer.from(e.value, "base64")),
    version: e.version,
  };
}

function batchFields(b: Batch): {
  checks?: KVCheck[];
  puts?: { namespace: string; key: string; value: string }[];
  deletes?: KVDelete[];
} {
  return {
    ...(b.checks?.length ? { checks: b.checks } : {}),
    ...(b.puts?.length
      ? {
          puts: b.puts.map((p) => ({
            namespace: p.namespace,
            key: p.key,
            value: base64(toBytes(p.value)),
          })),
        }
      : {}),
    ...(b.deletes?.length ? { deletes: b.deletes } : {}),
  };
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
