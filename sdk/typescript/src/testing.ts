// An in-memory host for unit-testing an extension, as protocol/sdk/sdktest
// is for Go. testHost serves an Extension over a pair of in-memory streams,
// as the daemon would, initializes it, and answers the extension-to-host
// methods from memory: KV with versions and conflicts, event.accept and
// ingress.deliver, the state.* reads of seeded scopes, Work and Events,
// secrets from options, artifacts and wait.progress. A test then calls into
// the extension (configure, operation, route, samples, reconcile, webhook,
// trigger, task, check, validate, and call for any other method) and
// inspects what it wrote (kv, events, artifacts, progress).
//
// It is not the host: causal depth and rate limits, timers, Work control and
// config.* are not served (they answer unsupported). Nothing here reads a
// real clock, the network or the operator's config: the clock is a
// TestClock the test moves, fixed at testStart until it does.

import { createHash } from "node:crypto";
import { PassThrough } from "node:stream";
import { Code, ProtocolError } from "./errors.ts";
import type { Extension } from "./extension.ts";
import { base64, toBytes, type Value } from "./host.ts";
import { Conn } from "./jsonrpc.ts";
import { type Infer, type Type, matches, t } from "./schema.ts";
import {
  type AcceptedEvent,
  type ArtifactRef,
  type Authority,
  type CheckRunResult,
  type ConfigEntry,
  ConfigureResult,
  EmitCommit,
  EmitPlan,
  type Event,
  EventAcceptParams,
  IngressDeliverParams,
  KVCommitParams,
  KVGetParams,
  KVListParams,
  OperationResult,
  type PendingInput,
  PushResult,
  type Route,
  RouteDecision,
  type RouteInput,
  RouteSample,
  type Scan,
  type Scope,
  SecretResolveParams,
  Snapshot,
  type SourceDecl,
  StateParams,
  WaitPlan,
  WebhookHandleResult,
  type Work,
} from "./wire.ts";

/** The test clock's start (sdktest.Start, conformance.NewClock's start). */
export const testStart = "2026-10-24T12:00:00.000Z";

/** The host's clock. It moves only when the test moves it. */
export class TestClock {
  #ms: number;

  constructor(start: string | Date = testStart) {
    this.#ms = new Date(start).getTime();
  }

  /** The clock's time, as an RFC 3339 string. */
  now(): string {
    return new Date(this.#ms).toISOString();
  }

  /** Moves the clock on by ms milliseconds. */
  advance(ms: number): void {
    this.#ms += ms;
  }

  /** Sets the clock to a time. */
  set(at: string | Date): void {
    this.#ms = new Date(at).getTime();
  }
}

export interface TestHostOptions {
  clock?: TestClock;
  /** Scopes configured for the extension (configure's entries add theirs). */
  scopes?: string[];
  /** Seeded Work; its scope is configured. */
  works?: Work[];
  /** Seeded accepted Events, in order, each in a scope. */
  events?: { scopeId: string; event: Event }[];
  /** Input routed to a Work that no attempt has read yet, by Work ID. */
  pending?: Record<string, PendingInput[]>;
  /** Secret values an entry may resolve, by scope. */
  secrets?: Record<string, Record<string, string>>;
  /** Secret values of a connection in a scope. */
  connectionSecrets?: {
    scopeId: string;
    connection: string;
    values: Record<string, string>;
  }[];
  /** Seeded KV values. */
  kv?: { namespace: string; key: string; value: Value }[];
  /** The KV layout the host has recorded (default 0). */
  kvVersion?: number;
  /** The private state directory given at initialize (default "/sdktest/state"). */
  stateDir?: string;
  /** The authority operation calls carry; unset fields are defaulted. */
  authority?: Partial<Authority>;
}

/** What an operation returned: a handler's output, a wait's plan, an emission's Event and commit output. */
export interface OperationOut {
  /** The output's data (a handler's, a wait plan's, or an emission's commit's). */
  output: unknown;
  wait?: WaitPlan;
  event?: AcceptedEvent;
  /** False when an emission's once matched one already accepted. */
  added?: boolean;
}

export interface WebhookCall {
  method?: string;
  query?: string;
  headers?: Record<string, string | string[]>;
  body?: Value;
  /** The host's receipt time (default now). */
  receivedAt?: string;
  forwardId?: string;
}

export interface WebhookOut {
  status: number;
  headers: Record<string, string[]>;
  body: Uint8Array;
}

/** One committed artifact. */
export interface TestArtifact {
  ref: ArtifactRef;
  mediaType: string;
  data: Uint8Array;
}

/** A KV value's version: a digest of its content, as sdktest.Version. */
export function kvVersion(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex").slice(0, 32);
}

function digest(s: string): string {
  return createHash("sha256").update(s).digest("hex").slice(0, 32);
}

function unauthorized(message: string): ProtocolError {
  return new ProtocolError(Code.Unauthorized, message);
}

function invalid(message: string): ProtocolError {
  return new ProtocolError(Code.Invalid, message);
}

/** Decodes a request's params, refusing malformed ones as the host does. */
function params<S extends Type<unknown>>(schema: S, raw: unknown): Infer<S> {
  const p = raw ?? {};
  if (!matches(schema, p)) throw invalid("malformed params");
  return p;
}

/** Decodes an extension's answer, failing the test's call when it is malformed. */
function result<S extends Type<unknown>>(
  schema: S,
  method: string,
  raw: unknown,
): Infer<S> {
  if (!matches(schema, raw))
    throw new ProtocolError(
      Code.Invalid,
      `malformed ${method} result: ${JSON.stringify(raw)}`,
    );
  return raw;
}

function bytes(b64: string | null | undefined): Uint8Array {
  return b64 ? new Uint8Array(Buffer.from(b64, "base64")) : new Uint8Array();
}

/**
 * Serves ext on an in-memory host and initializes it. Close it when the
 * test ends (in an after hook), so the served connection ends.
 */
export async function testHost(
  ext: Extension,
  o: TestHostOptions = {},
): Promise<TestHost> {
  const h = new TestHost(ext, o);
  await h._initialize();
  return h;
}

export class TestHost {
  readonly clock: TestClock;
  readonly stateDir: string;
  readonly #ext: Extension;
  readonly #conn: Conn;
  readonly #toExt: PassThrough;
  readonly #o: TestHostOptions;
  readonly #scopes = new Set<string>();
  readonly #kv = new Map<string, Map<string, Uint8Array>>();
  readonly #events: { scopeId: string; accepted: AcceptedEvent }[] = [];
  readonly #causal = new Map<
    string,
    { scopeId: string; root: string; depth: number }
  >();
  readonly #works: Work[];
  readonly #sources = new Map<string, SourceDecl>();
  readonly #writers = new Map<
    string,
    { mediaType: string; chunks: Uint8Array[] }
  >();
  readonly #artifacts: TestArtifact[] = [];
  readonly #progress = new Map<string, string>();
  /** The webhook paths the last configure registered. */
  #paths: string[] = [];
  #kvVersion: number;
  #calls = 0;

  constructor(ext: Extension, o: TestHostOptions) {
    this.#ext = ext;
    this.#o = o;
    this.clock = o.clock ?? new TestClock();
    this.stateDir = o.stateDir ?? "/sdktest/state";
    this.#kvVersion = o.kvVersion ?? 0;
    this.#works = [...(o.works ?? [])];
    for (const s of o.scopes ?? []) this.#scopes.add(s);
    for (const w of this.#works) this.#scopes.add(w.scope_id);
    for (const s of Object.keys(o.secrets ?? {})) this.#scopes.add(s);
    for (const k of o.kv ?? []) this.#put(k.namespace, k.key, toBytes(k.value));
    for (const e of o.events ?? []) {
      this.#scopes.add(e.scopeId);
      this.#accept(e.scopeId, e.event, "");
    }
    this.#toExt = new PassThrough();
    const fromExt = new PassThrough();
    ext.serve(this.#toExt, fromExt);
    this.#conn = new Conn(fromExt, this.#toExt, (method, raw, signal) =>
      this.#handle(method, raw, signal),
    );
  }

  /** @internal */
  async _initialize(): Promise<void> {
    await this.#conn.call("initialize", {
      protocol_versions: [1],
      host: { name: "sdktest", version: "0" },
      state_dir: this.stateDir,
    });
  }

  /** Ends the served connection. */
  close(): void {
    this.#toExt.end();
  }

  /** The host clock's time. */
  now(): string {
    return this.clock.now();
  }

  // Calls into the extension.

  /** Makes any host-to-extension call, for a surface without a helper. */
  call(method: string, p: unknown): Promise<unknown> {
    return this.#conn.call(method, p);
  }

  /**
   * Applies a configuration generation. Each entry's scope becomes
   * configured, and the sources the result registers can be delivered to,
   * reconciled and sent webhooks.
   */
  async configure(
    entries: ConfigEntry[],
    o: { disabled?: ConfigEntry[]; publicUrl?: string } = {},
  ): Promise<ConfigureResult> {
    const out = result(
      ConfigureResult,
      "configure",
      await this.#conn.call("configure", {
        generation: `sdktest-${++this.#calls}`,
        entries,
        ...(o.disabled ? { disabled: o.disabled } : {}),
        ...(o.publicUrl ? { public_url: o.publicUrl } : {}),
        kv: {
          stored: this.#kvVersion,
          declared: this.#ext.manifest.kv_version ?? 0,
        },
      }),
    );
    for (const e of entries) this.#scopes.add(e.scope_id);
    this.#sources.clear();
    for (const s of out.sources ?? []) this.#sources.set(s.name, s);
    this.#paths = out.webhook_paths ?? [];
    this.#kvVersion = out.kv_version;
    return out;
  }

  /** Checks a config object as config check does: throws the extension's refusal. */
  async validate(kind: string, type: string, config: unknown): Promise<void> {
    await this.#conn.call("validate", { kind, type, config });
  }

  /**
   * Calls a declared operation with input as its data, by the kind the
   * manifest declares. Unset authority fields default: the first
   * configured scope, Work "sdktest-work", execution "sdktest-execution",
   * an expiry an hour from now, every declared operation and family. An
   * emission is accepted in the authority's scope, and its commit's
   * records are written to KV, as the toolbelt would.
   */
  async operation(
    name: string,
    input: unknown = {},
    authority: Partial<Authority> = {},
  ): Promise<OperationOut> {
    const decl = this.#ext.manifest.provides.operations?.find(
      (d) => d.name === name,
    );
    if (decl === undefined)
      throw new ProtocolError(
        Code.Undeclared,
        `operation ${name} is not declared`,
      );
    const a = this.#authority(authority);
    const p = {
      operation: name,
      authority: a,
      input: { data: input },
      request_id: `sdktest-${++this.#calls}`,
    };
    if (decl.kind === "wait") {
      const plan = result(
        WaitPlan,
        "operation.wait",
        await this.#conn.call("operation.wait", p),
      );
      return { output: plan.output.data, wait: plan };
    }
    if (decl.kind === "emit")
      return this.#emit(
        a,
        result(
          EmitPlan,
          "operation.emit",
          await this.#conn.call("operation.emit", p),
        ),
      );
    const out = result(
      OperationResult,
      "operation.call",
      await this.#conn.call("operation.call", p),
    );
    return { output: out.output.data };
  }

  /** Asks a route handler to match one input; route.config is given as its data. */
  async route(
    handler: string,
    input: Omit<RouteInput, "event"> & {
      event: Event | AcceptedEvent;
    },
  ): Promise<RouteDecision> {
    const event =
      "event" in input.event
        ? input.event
        : { event: input.event, sequence: 1, routed: false };
    return result(
      RouteDecision,
      "route.match",
      await this.#conn.call("route.match", {
        handler,
        input: { ...input, event },
      }),
    );
  }

  /** The sample Events a route handler offers for a route of a scope, as of the clock's now. */
  async samples(
    handler: string,
    scope: Scope,
    route: Route,
  ): Promise<RouteSample[]> {
    const out = result(
      t.object({ samples: t.array(RouteSample) }, { open: true }),
      "route.samples",
      await this.#conn.call("route.samples", {
        handler,
        scope,
        route,
        now: this.now(),
      }),
    );
    return out.samples;
  }

  /** Reads one page of a registered source. */
  async reconcile(
    source: string,
    scan: Omit<Scan, "source"> = {},
  ): Promise<Snapshot> {
    const decl = this.#source(source);
    return result(
      Snapshot,
      "ingress.reconcile",
      await this.#conn.call("ingress.reconcile", {
        name: source,
        scan: { ...scan, source: decl.source },
      }),
    );
  }

  /** Reads every page of a registered source, from its first. */
  async reconcileAll(source: string): Promise<Snapshot[]> {
    const pages: Snapshot[] = [];
    let scan: Omit<Scan, "source"> = {};
    for (;;) {
      const page = await this.reconcile(source, scan);
      pages.push(page);
      if (!page.next_cursor) return pages;
      if (pages.length > 10_000)
        throw new Error(`sdktest: ${source} never stops paging`);
      scan = { scan_id: page.scan_id, cursor: page.next_cursor };
    }
  }

  /** Pushes a delivery to a registered source and accepts what it observed, as the host's ingress does. */
  async push(
    source: string,
    delivery: {
      id: string;
      body: Value;
      contentType?: string;
      receivedAt?: string;
    },
  ): Promise<{ accepted: AcceptedEvent[]; added: boolean[] }> {
    return this.#deliver({
      name: source,
      delivery: {
        id: delivery.id,
        received_at: delivery.receivedAt ?? this.now(),
        ...(delivery.contentType ? { content_type: delivery.contentType } : {}),
        body: base64(toBytes(delivery.body)),
      },
    });
  }

  /**
   * Sends one HTTP request on a webhook path, as the host's listener
   * does. A path no registered source took answers 404 here, as the
   * listener answers it, without reaching the extension.
   */
  async webhook(path: string, req: WebhookCall = {}): Promise<WebhookOut> {
    if (!this.#paths.some((p) => path === p || path.startsWith(`${p}/`)))
      return { status: 404, headers: {}, body: new Uint8Array() };
    const header: Record<string, string[]> = {};
    for (const [k, v] of Object.entries(req.headers ?? {}))
      header[k] = Array.isArray(v) ? v : [v];
    const out = result(
      WebhookHandleResult,
      "webhook.handle",
      await this.#conn.call("webhook.handle", {
        path,
        method: req.method ?? "POST",
        ...(req.query ? { query: req.query } : {}),
        ...(Object.keys(header).length ? { header } : {}),
        ...(req.body === undefined ? {} : { body: base64(toBytes(req.body)) }),
        received_at: req.receivedAt ?? this.now(),
        ...(req.forwardId ? { forward_id: req.forwardId } : {}),
      }),
    );
    return {
      status: out.status,
      headers: out.header ?? {},
      body: bytes(out.body),
    };
  }

  /** Asks a Work kind's trigger which of inputs triggers the next execution. */
  async trigger(kind: string, inputs: Event[]): Promise<number> {
    const out = result(
      t.object({ index: t.integer() }, { open: true }),
      "work_kind.trigger",
      await this.#conn.call("work_kind.trigger", { kind, inputs }),
    );
    return out.index;
  }

  /** Runs a background task once. */
  async task(name: string): Promise<void> {
    await this.#conn.call("task.run", { task: name });
  }

  /** Runs a doctor check, with an empty configuration view unless config is given. */
  async check(
    name: string,
    o: { deep?: boolean; config?: unknown } = {},
  ): Promise<CheckRunResult> {
    return (await this.#conn.call("check.run", {
      check: name,
      ...(o.deep ? { deep: true } : {}),
      config: o.config ?? {},
    })) as CheckRunResult;
  }

  // What the extension wrote.

  /** A KV value, or undefined. */
  kv(namespace: string, key: string): Uint8Array | undefined {
    return this.#kv.get(namespace)?.get(key);
  }

  /** A KV value as UTF-8 text, or undefined. */
  kvText(namespace: string, key: string): string | undefined {
    const v = this.kv(namespace, key);
    return v === undefined ? undefined : new TextDecoder().decode(v);
  }

  /** Every accepted Event, in order, seeded ones included. */
  events(): AcceptedEvent[] {
    return this.#events.map((e) => e.accepted);
  }

  /** The sources the last configure registered. */
  sources(): SourceDecl[] {
    return [...this.#sources.values()];
  }

  /** Committed artifacts, in order. */
  artifacts(): TestArtifact[] {
    return [...this.#artifacts];
  }

  /** The progress line wait.progress last set for a Work ("" for none). */
  progress(workId: string): string {
    return this.#progress.get(workId) ?? "";
  }

  // Serving the extension's calls.

  #authority(a: Partial<Authority>): Authority {
    const scope = a.scope_id ?? [...this.#scopes][0] ?? "";
    const execution = a.execution_id ?? "sdktest-execution";
    return {
      scope_id: scope,
      work_id: a.work_id ?? "sdktest-work",
      execution_id: execution,
      lease: a.lease ?? { execution_id: execution, owner: "sdktest", epoch: 1 },
      expires_at:
        a.expires_at ??
        new Date(new Date(this.now()).getTime() + 3_600_000).toISOString(),
      operations:
        a.operations ??
        (this.#ext.manifest.provides.operations ?? []).map((d) => d.name),
      event_types:
        a.event_types ??
        (this.#ext.manifest.provides.families ?? []).map((f) => f.type),
      ...(a.trigger_id ? { trigger_id: a.trigger_id } : {}),
      ...(a.connection ? { connection: a.connection } : {}),
      ...(a.work_kinds ? { work_kinds: a.work_kinds } : {}),
    };
  }

  async #emit(a: Authority, plan: EmitPlan): Promise<OperationOut> {
    const e = plan.event;
    if (!(a.event_types ?? []).includes(e.type) || !this.#ownFamily(e.type))
      throw new ProtocolError(Code.Undeclared, "family not declared");
    if (
      e.id ||
      e.source ||
      e.causation_id ||
      e.work_id ||
      e.execution_id ||
      e.received_at
    )
      throw unauthorized(
        "the host assigns an emission's identity, source and causation",
      );
    if (!e.delivery_id) throw invalid("an emission needs a delivery ID");
    this.#checkWrite(e.data);
    const event: Event = {
      ...e,
      id: `emit/${digest(`${a.execution_id}\x00${e.delivery_id}`)}`,
      source: `execution/${a.execution_id}`,
      work_id: a.work_id,
      execution_id: a.execution_id,
    };
    const { accepted, added } = this.#accept(
      a.scope_id,
      event,
      plan.cause || a.trigger_id || "",
    );
    const info = this.#causal.get(accepted.event.id ?? "")!;
    const out: OperationOut = { output: undefined, event: accepted, added };
    if (!plan.commit_id) return out;
    const commit = result(
      EmitCommit,
      "emit.commit",
      await this.#conn.call("emit.commit", {
        commit_id: plan.commit_id,
        emitted: { event: accepted, added, root: info.root, depth: info.depth },
      }),
    );
    for (const r of commit.records ?? [])
      this.#ownNamespace(r.namespace, r.key);
    if (added)
      for (const r of commit.records ?? [])
        this.#put(r.namespace, r.key, bytes(r.value));
    out.output = commit.output.data;
    return out;
  }

  async #handle(
    method: string,
    raw: unknown,
    _signal: AbortSignal,
  ): Promise<unknown> {
    switch (method) {
      case "kv.get": {
        const p = params(KVGetParams, raw);
        this.#ownNamespace(p.namespace, p.key);
        const v = this.kv(p.namespace, p.key);
        return v === undefined
          ? { found: false, entry: { key: "", value: null, version: "" } }
          : {
              found: true,
              entry: { key: p.key, value: base64(v), version: kvVersion(v) },
            };
      }
      case "kv.list": {
        const p = params(KVListParams, raw);
        this.#ownNamespace(p.namespace, "-");
        const entries = [...(this.#kv.get(p.namespace) ?? new Map()).entries()]
          .filter(([k]) => k.startsWith(p.prefix ?? ""))
          .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
          .map(([key, v]) => ({
            key,
            value: base64(v),
            version: kvVersion(v),
          }));
        return { entries };
      }
      case "kv.commit": {
        const p = params(KVCommitParams, raw);
        this.#checkBatch(p.checks ?? [], p.puts ?? [], p.deletes ?? []);
        if ((p.puts?.length ?? 0) + (p.deletes?.length ?? 0) === 0)
          throw invalid("empty batch");
        this.#checks(p.checks ?? []);
        this.#apply(p.puts ?? [], p.deletes ?? []);
        return {};
      }
      case "event.accept":
        return this.#eventAccept(params(EventAcceptParams, raw));
      case "ingress.deliver":
        return this.#deliver(params(IngressDeliverParams, raw));
      case "secret.resolve": {
        const p = params(SecretResolveParams, raw);
        if (!this.#scopes.has(p.scope_id))
          throw unauthorized("scope is not configured for this extension");
        let values: Record<string, string> | undefined;
        if (p.connection) {
          values = this.#o.connectionSecrets?.find(
            (c) => c.scopeId === p.scope_id && c.connection === p.connection,
          )?.values;
          if (values === undefined)
            throw unauthorized(
              "connection is not bound to the extension in the scope",
            );
        } else values = this.#o.secrets?.[p.scope_id] ?? {};
        return {
          values: Object.fromEntries(
            p.names.filter((n) => n in values).map((n) => [n, values[n]]),
          ),
        };
      }
      case "state.events":
      case "state.work_events": {
        const p = params(StateParams, raw);
        const limit =
          p.limit && p.limit > 0 && p.limit <= 1000 ? p.limit : 1000;
        if (method === "state.work_events" && !p.id) return [];
        return this.#events
          .filter(
            (e) =>
              e.accepted.sequence > (p.after ?? 0) &&
              this.#scopes.has(e.scopeId) &&
              (method === "state.events" ||
                e.accepted.event.work_id === p.id ||
                e.accepted.routed_work_id === p.id),
          )
          .slice(0, limit)
          .map((e) => ({ ...e.accepted, scope_id: e.scopeId }));
      }
      case "state.works":
        return this.#queryWorks(params(StateParams, raw));
      case "state.pending": {
        const p = params(StateParams, raw);
        const w = this.#works.find(
          (x) => x.id === p.id && this.#scopes.has(x.scope_id),
        );
        if (w === undefined)
          throw new ProtocolError(Code.NotFound, "not found");
        return this.#o.pending?.[w.id] ?? [];
      }
      case "wait.progress": {
        const p = params(
          t.object(
            { work_id: t.string(), progress: t.string() },
            { open: true },
          ),
          raw,
        );
        this.#progress.set(p.work_id, p.progress);
        return {};
      }
      case "artifact.create": {
        const p = params(
          t.object({ media_type: t.string() }, { open: true }),
          raw,
        );
        const id = `writer-${++this.#calls}`;
        this.#writers.set(id, { mediaType: p.media_type, chunks: [] });
        return { writer: id };
      }
      case "artifact.write":
      case "artifact.commit":
      case "artifact.abort": {
        const p = params(
          t.object(
            { writer: t.string(), chunk: t.optional(t.string()) },
            { open: true },
          ),
          raw,
        );
        const w = this.#writers.get(p.writer);
        if (w === undefined)
          throw new ProtocolError(Code.NotFound, "unknown writer");
        if (method === "artifact.write") {
          w.chunks.push(bytes(p.chunk));
          return {};
        }
        this.#writers.delete(p.writer);
        if (method === "artifact.abort") return {};
        const data = new Uint8Array(Buffer.concat(w.chunks));
        const sha256 = createHash("sha256").update(data).digest("hex");
        const ref = {
          id: `artifact-${this.#artifacts.length + 1}`,
          size: data.length,
          media_type: w.mediaType,
          sha256,
        };
        this.#artifacts.push({ ref, mediaType: w.mediaType, data });
        return ref;
      }
    }
    throw new ProtocolError(
      Code.Unsupported,
      `${method} is not served by the test host`,
    );
  }

  /** state.works as the host answers it (StateParams.Query, storage.SelectWorks). */
  #queryWorks(p: StateParams): Work[] {
    if (p.scope_id && !this.#scopes.has(p.scope_id))
      throw unauthorized("scope is not configured for this extension");
    const order = p.order ?? "";
    if (
      !["", "created", "-created", "updated", "-updated"].includes(order) ||
      (p.limit ?? 0) < 0 ||
      (p.states ?? []).some((s) => !workStates.includes(s)) ||
      (p.updated_since !== undefined &&
        Number.isNaN(Date.parse(p.updated_since))) ||
      (p.cursor !== undefined && Number.isNaN(Date.parse(p.cursor.at)))
    )
      throw invalid("malformed works query");
    const limit = Math.min(p.limit ?? 0, MaxWorksPage);
    const byUpdated = order.endsWith("updated");
    const desc = order.startsWith("-");
    // A Work's position: its time as an instant, then its ID.
    const key = (at: string, id: string) => ({ at: Date.parse(at), id });
    const keyOf = (w: Work) =>
      key(byUpdated ? w.updated_at : w.created_at, w.id);
    const cmp = (
      a: { at: number; id: string },
      b: { at: number; id: string },
    ) => (a.at !== b.at ? a.at - b.at : a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
    const since =
      p.updated_since === undefined ? undefined : Date.parse(p.updated_since);
    const cursor =
      p.cursor === undefined ? undefined : key(p.cursor.at, p.cursor.id);
    const selected = this.#works
      .filter(
        (w) =>
          this.#scopes.has(w.scope_id) &&
          (!p.states?.length || p.states.includes(w.state)) &&
          (!p.scope_id || w.scope_id === p.scope_id) &&
          (!p.kind || w.kind === p.kind) &&
          (!p.parent_id || w.parent_id === p.parent_id) &&
          (!p.sources?.length || p.sources.includes(workSource(w))) &&
          (!p.executor || w.latest_executor === p.executor) &&
          (since === undefined || Date.parse(w.updated_at) >= since) &&
          (!p.correlation ||
            (w.correlation_keys ?? []).some(
              (k) =>
                k.source === p.correlation!.source &&
                k.external_id === p.correlation!.external_id,
            )),
      )
      .sort((a, b) =>
        desc ? cmp(keyOf(b), keyOf(a)) : cmp(keyOf(a), keyOf(b)),
      )
      .filter(
        (w) =>
          cursor === undefined ||
          (desc ? cmp(keyOf(w), cursor) < 0 : cmp(keyOf(w), cursor) > 0),
      );
    return limit > 0 ? selected.slice(0, limit) : selected;
  }

  /**
   * The host's write rule (Manifest.CheckWrite): a payload an extension
   * writes is absent, or names the current version of one of its own
   * declared schemas.
   */
  #checkWrite(data: { schema?: string; data?: unknown } | undefined): void {
    const schema = data?.schema ?? "";
    const value = data?.data;
    if (schema === "" && (value === undefined || value === null)) return;
    const name = this.#ext.manifest.name;
    if (!inNamespace(name, schema, "."))
      throw new ProtocolError(
        Code.Schema,
        `schema "${schema}" is outside namespace "${name}"`,
      );
    const m = /^(.+)\.v([1-9][0-9]{0,8})$/.exec(schema);
    const decl =
      m && this.#ext.manifest.provides.schemas?.find((d) => d.name === m[1]);
    const current = decl ? decl.versions[decl.versions.length - 1] : undefined;
    if (!m || current === undefined || Number(m[2]) !== current)
      throw new ProtocolError(
        Code.Schema,
        `schema "${schema}" is not the current version of a schema the extension declares`,
      );
  }

  async #deliver(
    p: IngressDeliverParams,
  ): Promise<{ accepted: AcceptedEvent[]; added: boolean[] }> {
    const src = this.#sources.get(p.name);
    if (src === undefined)
      throw unauthorized("not a source this extension registered");
    const pushed = result(
      PushResult,
      "ingress.push",
      await this.#conn.call("ingress.push", {
        name: p.name,
        source: src.source,
        delivery: {
          ...p.delivery,
          received_at: p.delivery.received_at || this.now(),
        },
      }),
    );
    const out = { accepted: [] as AcceptedEvent[], added: [] as boolean[] };
    const events = [
      ...(pushed.events ?? []),
      ...(pushed.observations ?? []).map((o) => o.event),
    ];
    // Checked before any is accepted, so a refused delivery accepts nothing.
    for (const e of events) this.#checkWrite(e.data);
    for (const e of events) {
      const r = this.#accept(
        src.source.scope_id,
        { ...e, source: e.source || src.source.namespace },
        "",
      );
      out.accepted.push(r.accepted);
      out.added.push(r.added);
    }
    return out;
  }

  #eventAccept(p: EventAcceptParams): unknown {
    if (!p.parent)
      throw unauthorized(
        "a subprocess never mints a causal root outside ingress",
      );
    const e = p.event;
    const name = this.#ext.manifest.name;
    if (!this.#ownFamily(e.type))
      throw new ProtocolError(Code.Undeclared, "family not declared");
    if (!e.source || !inNamespace(name, e.source, "/") || !e.delivery_id)
      throw unauthorized("source is outside the extension's namespace");
    if (e.causation_id || e.execution_id)
      throw unauthorized("causation and execution are assigned by the host");
    this.#checkWrite(e.data);
    this.#checkBatch(p.checks ?? [], p.records ?? [], p.deletes ?? []);
    if (!this.#scopes.has(p.scope_id))
      throw unauthorized("scope is not configured for this extension");
    const parent = this.#causal.get(p.parent);
    if (parent === undefined)
      throw unauthorized("parent is not an accepted event");
    if (parent.scopeId !== p.scope_id)
      throw unauthorized("parent is outside the scope");
    this.#checks(p.checks ?? []);
    const r = this.#accept(
      p.scope_id,
      { ...e, id: `ext/${digest(`${e.source}\x00${e.delivery_id}`)}` },
      p.parent,
    );
    if (r.added) this.#apply(p.records ?? [], p.deletes ?? []);
    return { event: r.accepted, added: r.added };
  }

  /** Records e in scope, deduplicated by ID or delivery, as a root or derived from parent. */
  #accept(
    scopeId: string,
    e: Event,
    parent: string,
  ): { accepted: AcceptedEvent; added: boolean } {
    const id =
      e.id || `ext/${digest(`${e.source ?? ""}\x00${e.delivery_id ?? ""}`)}`;
    for (const x of this.#events) {
      const a = x.accepted.event;
      if (
        a.id === id ||
        (e.delivery_id &&
          a.source === e.source &&
          a.delivery_id === e.delivery_id)
      )
        return { accepted: x.accepted, added: false };
    }
    const received = e.received_at || this.now();
    const event: Event = {
      ...e,
      id,
      received_at: received,
      occurred_at: e.occurred_at || received,
      ...(parent ? { causation_id: parent } : {}),
    };
    const p = parent ? this.#causal.get(parent) : undefined;
    this.#causal.set(id, {
      scopeId,
      root: p?.root ?? id,
      depth: p === undefined ? 0 : p.depth + 1,
    });
    const accepted: AcceptedEvent = {
      event,
      sequence: this.#events.length + 1,
      routed: false,
    };
    this.#events.push({ scopeId, accepted });
    return { accepted, added: true };
  }

  #source(name: string): SourceDecl {
    const s = this.#sources.get(name);
    if (s === undefined)
      throw new Error(
        `sdktest: no source ${name}; configure registered ${[...this.#sources.keys()].join(", ") || "none"}`,
      );
    return s;
  }

  #ownFamily(type: string): boolean {
    return (this.#ext.manifest.provides.families ?? []).some(
      (f) => f.type === type,
    );
  }

  #ownNamespace(ns: string, key: string): void {
    if (!key || !inNamespace(this.#ext.manifest.name, ns, "."))
      throw unauthorized(
        `namespace ${ns} is not owned by ${this.#ext.manifest.name}`,
      );
  }

  #checkBatch(
    checks: {
      namespace: string;
      key: string;
      version?: string;
      absent?: boolean;
    }[],
    puts: { namespace: string; key: string; value: string | null }[],
    deletes: { namespace: string; key: string }[],
  ): void {
    for (const c of checks) {
      this.#ownNamespace(c.namespace, c.key);
      if (Boolean(c.absent) === Boolean(c.version))
        throw invalid("a check names a version or absence");
    }
    for (const p of puts) {
      this.#ownNamespace(p.namespace, p.key);
      if (p.value === null) throw invalid("a put needs a value");
    }
    for (const d of deletes) this.#ownNamespace(d.namespace, d.key);
  }

  #checks(
    checks: {
      namespace: string;
      key: string;
      version?: string;
      absent?: boolean;
    }[],
  ): void {
    for (const c of checks) {
      const v = this.kv(c.namespace, c.key);
      if (v === undefined ? !c.absent : c.absent || kvVersion(v) !== c.version)
        throw new ProtocolError(Code.Conflict, "precondition failed");
    }
  }

  #apply(
    puts: { namespace: string; key: string; value: string | null }[],
    deletes: { namespace: string; key: string }[],
  ): void {
    for (const p of puts) this.#put(p.namespace, p.key, bytes(p.value));
    for (const d of deletes) this.#kv.get(d.namespace)?.delete(d.key);
  }

  #put(ns: string, key: string, value: Uint8Array): void {
    let m = this.#kv.get(ns);
    if (m === undefined) this.#kv.set(ns, (m = new Map()));
    m.set(key, new Uint8Array(value));
  }
}

/** The Work states a query may name (domain.WorkState). */
const workStates = [
  "ready",
  "running",
  "waiting",
  "completed",
  "failed",
  "cancelled",
];

/** The most Work one state.works page holds (protocol.MaxWorksPage). */
const MaxWorksPage = 1000;

/** storage.WorkSource: the source of a Work's subject key, or its kind. */
function workSource(w: Work): string {
  return (
    (w.correlation_keys ?? []).find((k) => k.source !== "core.work")?.source ??
    w.kind
  );
}

/** protocol.Namespace (sep ".") and protocol.SourceNamespace (sep "/"). */
function inNamespace(name: string, ns: string, sep: string): boolean {
  return (
    ns === name || (ns.startsWith(name + sep) && ns.length > name.length + 1)
  );
}
