// Declaring each surface once, and serving it.
//
// extension() starts an Extension whose manifest is derived from what is
// registered on it, as the Go SDK's sdk.New does: operation, check, task,
// entryType and family each register a handler and the declaration that
// enables it, so the two cannot disagree. main() answers the packaging
// subcommands (package-manifest, package-certification) and otherwise
// serves protocol version 1 on stdin and stdout until stdin closes.

import { type Readable, Writable } from "node:stream";
import { Code, OperationError, ProtocolError } from "./errors.ts";
import { Host } from "./host.ts";
import { Conn } from "./jsonrpc.ts";
import { type Infer, type Type, check, isRecord, matches } from "./schema.ts";
import {
  type Authority,
  type CertificationFile,
  CheckRunParams,
  type CheckRunResult,
  type ConfigEntry,
  ConfigureParams,
  type ConnectionEntry,
  type Family,
  InitializeParams,
  type Manifest,
  type OperationDecl,
  OperationParams,
  TaskRunParams,
  ValidateParams,
} from "./wire.ts";

/** The protocol versions this SDK speaks. */
export const ProtocolVersions = [1];

/** What a handler can reach besides its input. */
export interface Context {
  host: Host;
  /** Aborts when the host cancels the request. */
  signal: AbortSignal;
}

export interface CallContext extends Context {
  /** The scope, Work and grant the call runs under. */
  authority: Authority;
  /** The caller's idempotency key: a retried call carries the same one. */
  requestId: string;
}

export interface OperationSpec<S extends Type<unknown>, Out> {
  /** The input's schema (t.object({}) for none); the handler receives what it accepts. */
  input: S;
  /** When an agent should call the operation, in Markdown. */
  guidance?: string;
  /** The payload schema the output is sent as (default "<operation>.v1"). */
  output?: string;
  /** Returns the output's data, or throws OperationError to refuse. */
  handler: (input: Infer<S>, ctx: CallContext) => Out | Promise<Out>;
}

/** One scope's entry of an entry type, with its config decoded. */
export interface EntryOf<C> {
  scopeId: string;
  name: string;
  config: C;
  /** The secret names the entry may resolve with host.secrets. */
  secrets: string[];
  connection?: ConnectionEntry;
}

export interface EntryTypeSpec<S extends Type<unknown>> {
  /** The entry's config schema. A config it refuses is invalid before the daemon applies it. */
  config: S;
  /** Checks the schema can't express; throw to refuse the config. */
  validate?: (config: Infer<S>) => void | Promise<void>;
  /** Receives every entry of the type on each configuration generation. */
  configure?: (entries: EntryOf<Infer<S>>[], ctx: Context) => void | Promise<void>;
}

export interface CheckContext extends Context {
  deep: boolean;
  /** The read-only view of the configuration (protocol.CheckConfig). */
  config: unknown;
}

/** A check returns its detail line, and throws to fail with the error's message as detail. */
export type CheckFn = (ctx: CheckContext) => string | Promise<string>;

export type TaskFn = (ctx: Context) => void | Promise<void>;

export type MigrateFn = (from: number, to: number, ctx: Context) => void | Promise<void>;

export interface Options {
  /** The extension's own version (required). */
  version: string;
  summary?: string;
  /** The package's command (default ["<name>-extension"]). */
  command?: string[];
  /** Host services beyond the extension's own surfaces it uses. */
  permissions?: string[];
  /** The layout version of the extension's own KV namespaces. */
  kvVersion?: number;
}

type Run = (data: unknown, ctx: CallContext) => Promise<{ schema: string; data: unknown }>;

interface EntryType {
  validate: (config: unknown) => Promise<boolean>;
  configure: (entries: ConfigEntry[], ctx: Context) => Promise<void>;
}

/** Starts an extension named name whose manifest its registrations fill in. */
export function extension(name: string, o: Options): Extension {
  return new Extension(name, o);
}

export class Extension {
  readonly manifest: Manifest;
  readonly #operations = new Map<string, Run>();
  readonly #checks = new Map<string, CheckFn>();
  readonly #tasks = new Map<string, TaskFn>();
  readonly #entryTypes = new Map<string, EntryType>();
  #migrate: MigrateFn | undefined;
  #certification: CertificationFile | undefined;

  constructor(name: string, o: Options) {
    // In the Go manifest's field order, so the written file reads the same.
    this.manifest = {
      protocol: [...ProtocolVersions],
      name,
      version: o.version,
      ...(o.summary === undefined ? {} : { summary: o.summary }),
      command: o.command ?? [`${name}-extension`],
      ...(o.kvVersion ? { kv_version: o.kvVersion } : {}),
      ...(o.permissions?.length ? { permissions: [...o.permissions] } : {}),
      provides: {},
    };
  }

  /** Registers a request/response toolbelt operation and declares it. */
  operation<S extends Type<unknown>, Out>(name: string, spec: OperationSpec<S, Out>): this {
    if (this.#operations.has(name)) throw new Error(`sdk: operation ${name} is registered twice`);
    const input = spec.input;
    const output = spec.output ?? `${name}.v1`;
    this.#operations.set(name, async (data, ctx) => {
      // Refused before the handler runs, so it only ever sees what its type says.
      if (!matches(input, data)) throw new OperationError("bad_request", `${name}: ${check(input, data)}`);
      return { schema: output, data: (await spec.handler(data, ctx)) ?? null };
    });
    const decl: OperationDecl = { name, kind: "handler", ...(spec.guidance === undefined ? {} : { guidance: spec.guidance }), input: json(input) };
    (this.manifest.provides.operations ??= []).push(decl);
    return this;
  }

  /** Declares a scope entry type: an "extensions" entry in a scope's config. */
  entryType<S extends Type<unknown>>(type: string, spec: EntryTypeSpec<S>): this {
    const decode = (e: ConfigEntry): EntryOf<Infer<S>> => {
      // An absent config is an empty one, as Go reads it.
      const config = e.config ?? {};
      if (!matches(spec.config, config)) throw new ProtocolError(Code.Invalid, `${type} entry in scope ${e.scope_id}: ${check(spec.config, config)}`);
      return { scopeId: e.scope_id, name: e.name ?? "", config, secrets: e.secrets ?? [], ...(e.connection ? { connection: e.connection } : {}) };
    };
    this.#entryTypes.set(type, {
      validate: async (config) => {
        const c = config ?? {};
        if (!matches(spec.config, c)) return false;
        try {
          await spec.validate?.(c);
          return true;
        } catch {
          return false;
        }
      },
      configure: async (entries, ctx) => {
        // Decoded even with no hook, so a bad config fails configure rather than passing silently.
        const decoded = entries.filter((e) => e.type === type).map(decode);
        await spec.configure?.(decoded, ctx);
      },
    });
    (this.manifest.provides.extensions ??= []).push({ type });
    return this;
  }

  /** Registers a doctor check and declares it. */
  check(name: string, fn: CheckFn, o: { deep?: boolean } = {}): this {
    this.#checks.set(name, fn);
    (this.manifest.provides.checks ??= []).push(o.deep ? { name, deep: true } : { name });
    return this;
  }

  /** Registers a task the host runs every intervalSeconds (at least one) and declares it. */
  task(name: string, intervalSeconds: number, fn: TaskFn): this {
    this.#tasks.set(name, fn);
    (this.manifest.provides.tasks ??= []).push({ name, interval_seconds: Math.floor(intervalSeconds) });
    return this;
  }

  /** Declares event families the extension derives through host.accept. */
  family(...families: Family[]): this {
    (this.manifest.provides.families ??= []).push(...families);
    return this;
  }

  /** Upgrades the extension's KV namespaces to kvVersion. It must be idempotent: a crash mid-way retries it. */
  onMigrate(fn: MigrateFn): this {
    this.#migrate = fn;
    return this;
  }

  /** Sets the package's certification.json content (package-certification). */
  certification(file: CertificationFile): this {
    this.#certification = file;
    return this;
  }

  /** Serves the extension on input and output (tests pass a pair of in-memory streams). */
  serve(input: Readable, output: Writable): Server {
    return new Server(this, input, output);
  }

  /**
   * The entry point of a packaged extension: `package-manifest` prints the
   * manifest, `package-certification` the certification file, and anything
   * else serves on stdin and stdout until stdin closes, then exits.
   */
  async main(argv: string[] = process.argv.slice(2)): Promise<void> {
    if (argv.length === 1 && argv[0] === "package-manifest") return print(this.manifest);
    if (argv.length === 1 && argv[0] === "package-certification") return this.#certification === undefined ? undefined : print(this.#certification);
    // stdout carries the protocol, so a stray console.log would corrupt it: send those to stderr.
    const stdout = process.stdout;
    const protocolWrite = stdout.write.bind(stdout);
    stdout.write = process.stderr.write.bind(process.stderr);
    const out = new Writable({
      write(chunk, _encoding, done) {
        protocolWrite(chunk, done);
      },
    });
    await this.serve(process.stdin, out).done;
    // An extension never outlives its host, whatever timers it left running.
    process.exit(0);
  }

  /** @internal */
  _served(): { operations: Map<string, Run>; checks: Map<string, CheckFn>; tasks: Map<string, TaskFn>; entryTypes: Map<string, EntryType>; migrate: MigrateFn | undefined } {
    return { operations: this.#operations, checks: this.#checks, tasks: this.#tasks, entryTypes: this.#entryTypes, migrate: this.#migrate };
  }
}

function json(schema: Type<unknown>): Record<string, unknown> {
  // A round trip through JSON gives the schema exactly as the manifest file holds it.
  const plain: unknown = JSON.parse(JSON.stringify(schema));
  return isRecord(plain) ? plain : {};
}

function print(v: unknown): Promise<void> {
  return new Promise((resolve) => process.stdout.write(JSON.stringify(v, null, 2) + "\n", () => resolve()));
}

function notFound(method: string): ProtocolError {
  return new ProtocolError(Code.MethodNotFound, `no such method ${method}`);
}

/** Decodes a request's params, refusing malformed ones as jsonrpc.Params does in Go. */
function params<S extends Type<unknown>>(schema: S, raw: unknown): Infer<S> {
  const p = raw ?? {};
  if (!matches(schema, p)) throw new ProtocolError(Code.Invalid, "malformed params");
  return p;
}

/** One served connection. */
export class Server {
  readonly host: Host;
  readonly #ext: Extension;
  readonly #conn: Conn;
  #ready = false;
  #draining = false;
  #stateDir = "";

  constructor(ext: Extension, input: Readable, output: Writable) {
    this.#ext = ext;
    this.#conn = new Conn(input, output, (method, raw, signal) => this.#handle(method, raw, signal), {
      onProtocolError: (reason) => process.stderr.write(`open-conduit sdk: ${reason}\n`),
    });
    this.host = new Host((method, p, signal) => this.#conn.call(method, p, signal));
  }

  /** Resolves when the connection ends. */
  get done(): Promise<void> {
    return this.#conn.done;
  }

  /** The private state directory the host gave at initialize. */
  get stateDir(): string {
    return this.#stateDir;
  }

  /** Ends the connection. */
  close(): void {
    this.#conn.close();
  }

  async #handle(method: string, raw: unknown, signal: AbortSignal): Promise<unknown> {
    if (method === "initialize") return this.#initialize(params(InitializeParams, raw));
    // The host must know the manifest before anything else is served.
    if (!this.#ready) throw new ProtocolError(Code.Invalid, "initialize first");
    if (this.#draining) throw new ProtocolError(Code.ShuttingDown, "shutting down");
    const served = this.#ext._served();
    switch (method) {
      case "configure":
        return this.#configure(params(ConfigureParams, raw), signal);
      case "shutdown":
        this.#draining = true;
        return {};
      case "validate": {
        const p = params(ValidateParams, raw);
        const entryType = p.kind === "extension" ? served.entryTypes.get(p.type) : undefined;
        // The host shows the operator only that the config is invalid, never why.
        if (entryType !== undefined && !(await entryType.validate(p.config))) throw new ProtocolError(Code.Invalid, `invalid ${p.kind} config`);
        return {};
      }
      case "operation.call":
      case "operation.wait":
      case "operation.emit": {
        const p = params(OperationParams, raw);
        const run = served.operations.get(p.operation);
        if (run === undefined) throw new ProtocolError(Code.Undeclared, "unknown operation");
        // Every operation this SDK declares is a handler, so wait and emit are the wrong kind.
        if (method !== "operation.call") throw new ProtocolError(Code.Undeclared, "operation kind mismatch");
        const output = await run(p.input.data ?? {}, { host: this.host, authority: p.authority, requestId: p.request_id ?? "", signal });
        return { output };
      }
      case "task.run": {
        const fn = served.tasks.get(params(TaskRunParams, raw).task);
        if (fn === undefined) throw notFound(method);
        await fn({ host: this.host, signal });
        return {};
      }
      case "check.run": {
        const p = params(CheckRunParams, raw);
        const fn = served.checks.get(p.check);
        if (fn === undefined) throw notFound(method);
        try {
          return result(true, await fn({ host: this.host, deep: p.deep === true, config: p.config, signal }));
        } catch (err) {
          return result(false, err instanceof Error ? err.message : String(err));
        }
      }
    }
    throw notFound(method);
  }

  #initialize(p: InitializeParams): unknown {
    const offered = p.protocol_versions ?? [];
    const common = offered.filter((v) => this.#ext.manifest.protocol.includes(v));
    if (common.length === 0) throw new ProtocolError(Code.UnsupportedVersion, "no common protocol version");
    this.#ready = true;
    this.#stateDir = p.state_dir;
    return { protocol_version: Math.max(...common), manifest: this.#ext.manifest };
  }

  async #configure(p: ConfigureParams, signal: AbortSignal): Promise<unknown> {
    const { entryTypes, migrate } = this.#ext._served();
    const ctx = { host: this.host, signal };
    if (p.kv.stored < p.kv.declared) {
      // From layout 0 there is nothing stored to migrate, so no migration is needed.
      if (migrate === undefined && p.kv.stored !== 0) throw new ProtocolError(Code.Unsupported, "no KV migration");
      await migrate?.(p.kv.stored, p.kv.declared, ctx);
    }
    for (const entryType of entryTypes.values()) await entryType.configure(p.entries ?? [], ctx);
    return { kv_version: this.#ext.manifest.kv_version ?? 0 };
  }
}

function result(ok: boolean, detail: string): CheckRunResult {
  return detail === "" ? { ok } : { ok, detail };
}
