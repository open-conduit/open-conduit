// Declaring each surface once, and serving it.
//
// extension() starts an Extension whose manifest is derived from what is
// registered on it, as the Go SDK's sdk.New does: operation, check, task,
// entryType and family each set the handler and add the declaration that
// enables it, so the two cannot disagree. main() answers the packaging
// subcommands (package-manifest, package-certification) and otherwise
// serves protocol version 1 on stdin and stdout until stdin closes.

import { type Readable, Writable } from "node:stream";
import { Code, OperationError, ProtocolError } from "./errors.ts";
import { Host } from "./host.ts";
import { Conn } from "./jsonrpc.ts";
import { type Infer, type Type, check as checkInput, t } from "./schema.ts";
import type {
  Authority,
  CertificationFile,
  CheckRunParams,
  CheckRunResult,
  ConfigureParams,
  Family,
  InitializeParams,
  Manifest,
  OperationParams,
  TaskRunParams,
  ValidateParams,
} from "./wire.ts";

/** The protocol versions this SDK speaks. */
export const ProtocolVersions = [1];

/** What an operation handler is called with besides its input. */
export interface CallContext {
  host: Host;
  authority: Authority;
  /** The caller's idempotency key: a retried call carries the same one. */
  requestId: string;
  /** Aborts when the host cancels the call. */
  signal: AbortSignal;
}

export interface OperationSpec<S extends Type<unknown>, Out> {
  /** The input's schema; the handler receives Infer<input>. Default: no fields. */
  input?: S;
  /** When and how an agent should use the operation, in Markdown. */
  guidance?: string;
  /** The payload schema the output is sent as (default "<operation>.v1"). */
  output?: string;
  /** Returns the output's data, or throws OperationError to refuse. */
  handler: (input: Infer<S>, ctx: CallContext) => Out | Promise<Out>;
}

export interface CheckContext {
  host: Host;
  deep: boolean;
  /** The read-only view of the configuration (protocol.CheckConfig). */
  config: unknown;
  signal: AbortSignal;
}

/** A check returns its detail line, and throws to fail with the error's message as detail. */
export type CheckFn = (ctx: CheckContext) => string | Promise<string>;

export type TaskFn = (ctx: { host: Host; signal: AbortSignal }) => void | Promise<void>;

export type ConfigureFn = (params: ConfigureParams, ctx: { host: Host; signal: AbortSignal }) => void | Promise<void>;

/** Throw to refuse a config object; the host reports only that it is invalid. */
export type ValidateFn = (params: ValidateParams) => void | Promise<void>;

export type MigrateFn = (from: number, to: number, ctx: { host: Host; signal: AbortSignal }) => void | Promise<void>;

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

interface Operation {
  input: Readonly<Record<string, unknown>>;
  output: string;
  handler: (input: unknown, ctx: CallContext) => unknown;
}

/** Starts an extension named name whose manifest its registrations fill in. */
export function extension(name: string, o: Options): Extension {
  return new Extension(name, o);
}

export class Extension {
  readonly manifest: Manifest;
  readonly #operations = new Map<string, Operation>();
  readonly #checks = new Map<string, CheckFn>();
  readonly #tasks = new Map<string, TaskFn>();
  #configure: ConfigureFn | undefined;
  #validate: ValidateFn | undefined;
  #migrate: MigrateFn | undefined;
  #certification: CertificationFile | undefined;

  constructor(name: string, o: Options) {
    // In the Go manifest's field order, so the file reads the same.
    const m: Manifest = { protocol: [...ProtocolVersions], name, version: o.version } as Manifest;
    if (o.summary !== undefined) m.summary = o.summary;
    m.command = o.command ?? [`${name}-extension`];
    if (o.kvVersion !== undefined && o.kvVersion > 0) m.kv_version = o.kvVersion;
    if (o.permissions?.length) m.permissions = [...o.permissions];
    m.provides = {};
    this.manifest = m;
  }

  /** Registers a request/response toolbelt operation and declares it. */
  operation<S extends Type<unknown> = Type<Record<string, never>>, Out = unknown>(name: string, spec: OperationSpec<S, Out>): this {
    if (this.#operations.has(name)) throw new Error(`sdk: operation ${name} is registered twice`);
    const input = spec.input ?? t.object({});
    this.#operations.set(name, { input, output: spec.output ?? `${name}.v1`, handler: spec.handler as (input: unknown, ctx: CallContext) => unknown });
    const decl: NonNullable<Manifest["provides"]["operations"]>[number] = { name, kind: "handler" };
    if (spec.guidance !== undefined) decl.guidance = spec.guidance;
    decl.input = JSON.parse(JSON.stringify(input)) as Record<string, unknown>;
    (this.manifest.provides.operations ??= []).push(decl);
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

  /** Declares a scope entry type (an "extensions" entry in a scope's config). */
  entryType(type: string): this {
    (this.manifest.provides.extensions ??= []).push({ type });
    return this;
  }

  /** Declares event families the extension derives through host.accept. */
  family(...families: Family[]): this {
    (this.manifest.provides.families ??= []).push(...families);
    return this;
  }

  /** Applies a configuration generation: the entries of the extension's types. */
  onConfigure(fn: ConfigureFn): this {
    this.#configure = fn;
    return this;
  }

  /** Checks one config object of a declared type before it is applied. */
  onValidate(fn: ValidateFn): this {
    this.#validate = fn;
    return this;
  }

  /** Upgrades the extension's KV namespaces to kvVersion; it must be idempotent. */
  onMigrate(fn: MigrateFn): this {
    this.#migrate = fn;
    return this;
  }

  /** Sets the package's certification.json content (package-certification). */
  certification(file: CertificationFile): this {
    this.#certification = file;
    return this;
  }

  /** Serves the extension on input/output (in-process tests pass a pair of streams). */
  serve(input: Readable, output: Writable): Server {
    return new Server(this, input, output);
  }

  /**
   * The entry point of a packaged extension: `package-manifest` prints the
   * manifest, `package-certification` the certification file, and anything
   * else serves on stdin and stdout until stdin closes, then exits.
   * console.log and other writes to stdout go to stderr while it serves,
   * so a stray print cannot corrupt the protocol stream.
   */
  async main(argv: string[] = process.argv.slice(2)): Promise<void> {
    if (argv.length === 1 && argv[0] === "package-manifest") return print(this.manifest);
    if (argv.length === 1 && argv[0] === "package-certification") return this.#certification === undefined ? undefined : print(this.#certification);
    const write = process.stdout.write.bind(process.stdout) as (chunk: Uint8Array, cb: (err?: Error | null) => void) => boolean;
    process.stdout.write = process.stderr.write.bind(process.stderr) as typeof process.stdout.write;
    const out = new Writable({ write: (chunk: Uint8Array, _enc, cb) => void write(chunk, cb) });
    const server = this.serve(process.stdin, out);
    await server.done;
    // An extension never outlives its host, whatever timers it left running.
    process.exit(0);
  }

  /** @internal */
  _operation(name: string): Operation | undefined {
    return this.#operations.get(name);
  }
  /** @internal */
  _check(name: string): CheckFn | undefined {
    return this.#checks.get(name);
  }
  /** @internal */
  _task(name: string): TaskFn | undefined {
    return this.#tasks.get(name);
  }
  /** @internal */
  _hooks(): { configure?: ConfigureFn; validate?: ValidateFn; migrate?: MigrateFn } {
    const h: { configure?: ConfigureFn; validate?: ValidateFn; migrate?: MigrateFn } = {};
    if (this.#configure) h.configure = this.#configure;
    if (this.#validate) h.validate = this.#validate;
    if (this.#migrate) h.migrate = this.#migrate;
    return h;
  }
}

function print(v: unknown): Promise<void> {
  return new Promise((resolve) => process.stdout.write(JSON.stringify(v, null, 2) + "\n", () => resolve()));
}

function notFound(method: string): ProtocolError {
  return new ProtocolError(Code.MethodNotFound, `no such method ${method}`);
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
    this.#conn = new Conn(input, output, (method, params, signal) => this.#handle(method, params ?? {}, signal), {
      onProtocolError: (reason) => process.stderr.write(`open-conduit sdk: ${reason}\n`),
    });
    this.host = new Host((method, params, signal) => this.#conn.call(method, params, signal));
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
    if (method === "initialize") return this.#initialize(raw as InitializeParams);
    if (!this.#ready) throw new ProtocolError(Code.Invalid, "initialize first");
    if (this.#draining) throw new ProtocolError(Code.ShuttingDown, "shutting down");
    const hooks = this.#ext._hooks();
    switch (method) {
      case "configure":
        return this.#configureCall(raw as ConfigureParams, signal);
      case "shutdown":
        this.#draining = true;
        return {};
      case "validate":
        if (hooks.validate !== undefined) {
          try {
            await hooks.validate(raw as ValidateParams);
          } catch (err) {
            if (err instanceof ProtocolError) throw err;
            throw new ProtocolError(Code.Invalid, `invalid ${(raw as ValidateParams).kind} config`);
          }
        }
        return {};
      case "operation.call":
      case "operation.wait":
      case "operation.emit":
        return this.#operationCall(method, raw as OperationParams, signal);
      case "task.run": {
        const fn = this.#ext._task((raw as TaskRunParams).task);
        if (fn === undefined) throw notFound(method);
        await fn({ host: this.host, signal });
        return {};
      }
      case "check.run": {
        const p = raw as CheckRunParams;
        const fn = this.#ext._check(p.check);
        if (fn === undefined) throw notFound(method);
        try {
          const detail = await fn({ host: this.host, deep: p.deep === true, config: p.config, signal });
          return result(true, detail);
        } catch (err) {
          return result(false, err instanceof Error ? err.message : String(err));
        }
      }
    }
    throw notFound(method);
  }

  #initialize(p: InitializeParams): unknown {
    let best = 0;
    for (const v of p.protocol_versions ?? []) if (this.#ext.manifest.protocol.includes(v) && v > best) best = v;
    if (best === 0) throw new ProtocolError(Code.UnsupportedVersion, "no common protocol version");
    this.#ready = true;
    this.#stateDir = p.state_dir ?? "";
    return { protocol_version: best, manifest: this.#ext.manifest };
  }

  async #configureCall(p: ConfigureParams, signal: AbortSignal): Promise<unknown> {
    const hooks = this.#ext._hooks();
    const ctx = { host: this.host, signal };
    const stored = p.kv?.stored ?? 0;
    const declared = p.kv?.declared ?? 0;
    if (stored < declared) {
      if (hooks.migrate === undefined && stored !== 0) throw new ProtocolError(Code.Unsupported, "no KV migration");
      await hooks.migrate?.(stored, declared, ctx);
    }
    await hooks.configure?.(p, ctx);
    return { kv_version: this.#ext.manifest.kv_version ?? 0 };
  }

  async #operationCall(method: string, p: OperationParams, signal: AbortSignal): Promise<unknown> {
    const op = this.#ext._operation(p.operation);
    if (op === undefined) throw new ProtocolError(Code.Undeclared, "unknown operation");
    if (method !== "operation.call") throw new ProtocolError(Code.Undeclared, "operation kind mismatch");
    let data = p.input?.data;
    if (data === undefined || data === null) data = {};
    const why = checkInput(op.input, data);
    if (why !== undefined) throw new OperationError("bad_request", `${p.operation}: ${why}`);
    const out = await op.handler(data, { host: this.host, authority: p.authority, requestId: p.request_id ?? "", signal });
    return { output: { schema: op.output, data: out === undefined ? null : out } };
  }
}

function result(ok: boolean, detail: string): CheckRunResult {
  return detail === "" ? { ok } : { ok, detail };
}

