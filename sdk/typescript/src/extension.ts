// Declaring each surface once, and serving it.
//
// extension() starts an Extension whose manifest is derived from what is
// registered on it, as the Go SDK's sdk.New does: operation, check, task,
// entryType and family each register a handler and the declaration that
// enables it, so the two cannot disagree. main() answers the packaging
// subcommands (package-manifest, package-certification) and otherwise
// serves protocol version 1 on stdin and stdout until stdin closes.

import { randomBytes } from "node:crypto";
import { type Readable, Writable } from "node:stream";
import { Code, ConfigError, OperationError, ProtocolError } from "./errors.ts";
import { type DeliveryInput, Host, type Put, base64, toBytes } from "./host.ts";
import { Conn } from "./jsonrpc.ts";
import {
  type Infer,
  type Type,
  check,
  diagnose,
  isRecord,
  matches,
} from "./schema.ts";
import {
  type AcceptedEvent,
  type Authority,
  type CertificationFile,
  CheckRunParams,
  type CheckRunResult,
  type ConfigEntry,
  ConfigureParams,
  type ConfigureResult,
  type ConnectionEntry,
  type Dependency,
  EmitCommitParams,
  type Emitted,
  type Event,
  type Family,
  IngressPushParams,
  IngressReconcileParams,
  InitializeParams,
  type Manifest,
  type OperationDecl,
  OperationParams,
  type PushResult,
  type Route,
  type RouteDecision,
  type RouteInput,
  RouteMatchParams,
  type RouteSample,
  RouteSamplesParams,
  type Scan,
  type Scope,
  type Snapshot,
  type SourceDecl,
  TaskRunParams,
  type TaskRunResult,
  ValidateParams,
  type WaitRequest,
  WebhookHandleParams,
  type WebhookHandleResult,
  WorkKindTriggerParams,
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
  /** What the operation does, in one line (at most 200 bytes). */
  summary?: string;
  /** When an agent should call the operation, in Markdown. */
  guidance?: string;
  /**
   * What a call does in a person's words, for whoever is asked to approve
   * it: a phrase that follows "wants to", with {field} for a top-level
   * input value (e.g. "deploy {service} to {env}"; one line, at most 200
   * bytes).
   */
  wants?: string;
  /** A capability the operation answers, routed by name (e.g. "git.credential"). */
  serves?: string;
  /** The payload schema the output is sent as (default "<operation>.v1"). */
  output?: string;
  /** Returns the output's data, or throws OperationError to refuse. */
  handler: (input: Infer<S>, ctx: CallContext) => Out | Promise<Out>;
}

/** The fields every kind of operation declares. */
interface OperationDoc<S extends Type<unknown>> {
  /** The input's schema (t.object({}) for none); the handler receives what it accepts. */
  input: S;
  /** What the operation does, in one line (at most 200 bytes). */
  summary?: string;
  /** When an agent should call the operation, in Markdown. */
  guidance?: string;
  /** What a call does in a person's words, as for OperationSpec.wants. */
  wants?: string;
  /** The payload schema the output is sent as (default "<operation>.v1"). */
  output?: string;
}

/** What a wait operation's handler returns: what the Work waits for, and what the caller is told. */
export interface WaitOut {
  /** The Events (and time) that end the wait; keys left out mean every key the Work owns. */
  wait: WaitRequest;
  /** Events on keys the Work doesn't own (another Work's, or one no Work owns yet) to alias into it. */
  dependencies?: Dependency[];
  /** The output's data, returned to the caller as the operation's output. */
  output?: unknown;
}

export interface WaitSpec<S extends Type<unknown>> extends OperationDoc<S> {
  /** Turns a call into a wait the host parks the Work on; throws OperationError to refuse. */
  handler: (input: Infer<S>, ctx: CallContext) => WaitOut | Promise<WaitOut>;
}

/** What an emission's commit writes and returns. It must be pure and deterministic: the host may run it again. */
export interface CommitOut {
  /** KV writes in the extension's own namespaces, committed with the Event. */
  records?: Put[];
  /** The output's data, returned to the caller. */
  output?: unknown;
}

/** What an emit operation's handler returns: the one Event to emit, of a family the extension declares. */
export interface EmitOut {
  event: Event;
  /** An accepted Event in the source's namespace the emission derives from, instead of the attempt's trigger. */
  cause?: string;
  /** The emission's identity across executions: at most one Event is accepted per scope and once. */
  once?: string;
  /** Runs in the accepting transaction once the Event is accepted (declare the operation with commit: true). */
  commit?: (emitted: Emitted) => CommitOut;
}

export interface EmitSpec<S extends Type<unknown>> extends OperationDoc<S> {
  /** The handler's plans may carry a commit that writes records with the Event. */
  commit?: boolean;
  /** Turns a call into the Event to emit; throws OperationError to refuse. */
  handler: (input: Infer<S>, ctx: CallContext) => EmitOut | Promise<EmitOut>;
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
  /**
   * Checks the schema can't express; throw to refuse the config. Throw a
   * ConfigError to say which field is wrong and why: the host reports
   * each of its diagnostics at the field's full path. Anything else
   * thrown is reported only as invalid.
   */
  validate?: (config: Infer<S>) => void | Promise<void>;
  /** Receives every entry of the type on each configuration generation. */
  configure?: (
    entries: EntryOf<Infer<S>>[],
    ctx: Context,
  ) => void | Promise<void>;
}

/** A source an ingress entry registers, with whatever the extension keeps for it. */
export interface SourceSpec<D = unknown> {
  /** Its registration name: the extension's name, or that and "/" and more (as "<name>/<scope>/<entry>"). */
  name: string;
  /** Its correlation keys' source: the extension's name, or that and "/" and more. */
  namespace: string;
  scopeId: string;
  /** How often the host reconciles it, in whole seconds (at least one). */
  intervalSeconds: number;
  /** Serve the type's webhook for it at /<name> (default: whenever the type has one). */
  webhook?: boolean;
  /** What the handlers need for it: a client, its settings. */
  data?: D;
}

/** A delivery the host pushes to a source, with its body as bytes. */
export interface PushDelivery {
  id: string;
  receivedAt: string;
  contentType?: string;
  body: Uint8Array;
}

/** One HTTP request on a source's webhook path. */
export interface WebhookRequest {
  path: string;
  method: string;
  query: string;
  headers: Headers;
  body: Uint8Array;
  /** When the delivery was first received, by the listener or a forwarder in front of it. */
  receivedAt: string;
  /** The forwarder's delivery ID, when one is configured ("" otherwise). */
  forwardId: string;
}

export interface WebhookResponse {
  /** Default 200. A 2xx tells the sender its delivery is durably accepted. */
  status?: number;
  headers?: Record<string, string | string[]>;
  body?: Uint8Array | string;
}

export interface WebhookContext extends Context {
  /**
   * Hands a verified delivery to this source: the host calls push with it
   * and answers once its Events are durably accepted. receivedAt defaults
   * to the request's.
   */
  deliver: (
    delivery: DeliveryInput,
  ) => Promise<{ accepted: AcceptedEvent[]; added: boolean[] }>;
}

export interface IngressTypeSpec<S extends Type<unknown>, D> {
  /** The entry's config schema, as for entryType. */
  config: S;
  /** Checks the schema can't express, as for entryType. */
  validate?: (config: Infer<S>) => void | Promise<void>;
  /** The families its sources observe (declared in the manifest, at least one). */
  families: [Family, ...Family[]];
  /** Each configuration generation: the sources the type's entries register. */
  sources: (
    entries: EntryOf<Infer<S>>[],
    ctx: Context,
  ) => SourceSpec<D>[] | Promise<SourceSpec<D>[]>;
  /** Maps a delivery to observations (and events of families that aren't re-derivable). */
  push?: (
    source: SourceSpec<D>,
    delivery: PushDelivery,
    ctx: Context,
  ) => PushResult | Promise<PushResult>;
  /**
   * Reads one page of what the source holds now. Without it every scan is
   * an empty partial page, which never says anything is gone.
   */
  reconcile?: (
    source: SourceSpec<D>,
    scan: Scan,
    ctx: Context,
  ) => Snapshot | Promise<Snapshot>;
  /**
   * Serves a request on a source's webhook path: verify it (the vendor's
   * own signature), then ctx.deliver it before answering.
   */
  webhook?: (
    source: SourceSpec<D>,
    request: WebhookRequest,
    ctx: WebhookContext,
  ) => WebhookResponse | Promise<WebhookResponse>;
  /** The largest webhook body the host reads, in bytes (default 1 MiB). */
  webhookMaxBytes?: number;
}

/** A route's call: the Event, its scope, the Work it already reaches, the route, and the route's config decoded. */
export interface RouteCall<C> extends RouteInput {
  config: C;
}

export interface SampleCall<C> {
  scope: Scope;
  route: Route;
  /** The host's clock, for samples whose data carries a time. */
  now: string;
  config: C;
}

export interface RouteSpec<S extends Type<unknown>> {
  /** The route config's schema; checked before the daemon applies a config. */
  config?: S;
  /** Decides what an Event does: open Work, reach the Work it has, or nothing. No side effects. */
  match: (
    call: RouteCall<Infer<S>>,
    ctx: Context,
  ) => RouteDecision | Promise<RouteDecision>;
  /**
   * Sample Events for a route trace, built from the route's and scope's
   * config so they trace as written; IDs are stable. No side effects.
   */
  samples?: (
    call: SampleCall<Infer<S>>,
    ctx: Context,
  ) => RouteSample[] | Promise<RouteSample[]>;
}

export interface WorkKindSpec {
  /** Its Work stays open across triggers. */
  standing?: boolean;
  /** Its Work runs on a clock: a dry run starts none of it. */
  timed?: boolean;
  /** Its Work does all its work in one attempt: a handoff completes it. */
  runOnce?: boolean;
  /** Picks, from a Work's pending inputs, the index of the one that triggers its next execution. */
  trigger?: (inputs: Event[]) => number;
}

export interface CheckContext extends Context {
  deep: boolean;
  /** The read-only view of the configuration (protocol.CheckConfig). */
  config: unknown;
}

/** A check returns its detail line, and throws to fail with the error's message as detail. */
export type CheckFn = (ctx: CheckContext) => string | Promise<string>;

/**
 * A task may return when it next has something to do and what wakes it
 * sooner (at most an hour off); returning nothing keeps its interval.
 */
export type TaskFn = (
  ctx: Context,
) => void | TaskRunResult | Promise<void | TaskRunResult>;

export type MigrateFn = (
  from: number,
  to: number,
  ctx: Context,
) => void | Promise<void>;

export interface Options {
  /** The extension's own version (required). */
  version: string;
  /** The display name operators read (default: the name). */
  title?: string;
  summary?: string;
  /** The package's command (default ["<name>-extension"]). */
  command?: string[];
  /** Host services beyond the extension's own surfaces it uses. */
  permissions?: string[];
  /** The layout version of the extension's own KV namespaces. */
  kvVersion?: number;
  /** A directory of playbooks, relative to the package's manifest, the host offers agents. */
  playbooks?: string;
}

type Kind = "handler" | "wait" | "emit";

interface Op {
  kind: Kind;
  run: (data: unknown, ctx: CallContext) => Promise<unknown>;
}

/** A registered source, with the handlers of its type. */
interface Registered {
  spec: SourceSpec<unknown>;
  type: IngressHandlers;
}

interface IngressHandlers {
  push?: (
    s: SourceSpec<unknown>,
    d: PushDelivery,
    ctx: Context,
  ) => PushResult | Promise<PushResult>;
  reconcile?: (
    s: SourceSpec<unknown>,
    scan: Scan,
    ctx: Context,
  ) => Snapshot | Promise<Snapshot>;
  webhook?: (
    s: SourceSpec<unknown>,
    r: WebhookRequest,
    ctx: WebhookContext,
  ) => WebhookResponse | Promise<WebhookResponse>;
}

interface EntryType {
  /** Throws ConfigError (or a bare refusal) for a config it refuses. */
  validate: (config: unknown) => Promise<void>;
  /** An ingress type returns the sources its entries register. */
  configure: (entries: ConfigEntry[], ctx: Context) => Promise<Registered[]>;
}

interface RouteHandler {
  validate: (config: unknown) => Promise<void>;
  match: (input: RouteInput, ctx: Context) => Promise<RouteDecision>;
  samples: (p: RouteSamplesParams, ctx: Context) => Promise<RouteSample[]>;
}

/** Decodes config through schema, reporting what it refuses at each field. */
async function validateConfig(
  schema: Type<unknown>,
  config: unknown,
  extra?: (config: never) => void | Promise<void>,
): Promise<void> {
  const c = config ?? {};
  if (!matches(schema, c)) {
    const diagnostics = diagnose(schema, c);
    if (diagnostics.length > 0) throw new ConfigError(diagnostics);
    throw new ProtocolError(Code.Invalid, "invalid extension config");
  }
  try {
    await extra?.(c as never);
  } catch (err) {
    if (err instanceof ConfigError) throw err;
    throw new ProtocolError(Code.Invalid, "invalid extension config");
  }
}

/** Starts an extension named name whose manifest its registrations fill in. */
export function extension(name: string, o: Options): Extension {
  return new Extension(name, o);
}

export class Extension {
  readonly manifest: Manifest;
  readonly #operations = new Map<string, Op>();
  readonly #checks = new Map<string, CheckFn>();
  readonly #tasks = new Map<string, TaskFn>();
  readonly #entryTypes = new Map<string, EntryType>();
  readonly #routes = new Map<string, RouteHandler>();
  readonly #triggers = new Map<string, (inputs: Event[]) => number>();
  #migrate: MigrateFn | undefined;
  #certification: CertificationFile | undefined;

  constructor(name: string, o: Options) {
    // In the Go manifest's field order, so the written file reads the same.
    this.manifest = {
      protocol: [...ProtocolVersions],
      name,
      version: o.version,
      ...(o.title === undefined ? {} : { title: o.title }),
      ...(o.summary === undefined ? {} : { summary: o.summary }),
      command: o.command ?? [`${name}-extension`],
      ...(o.kvVersion ? { kv_version: o.kvVersion } : {}),
      ...(o.permissions?.length ? { permissions: [...o.permissions] } : {}),
      provides: o.playbooks ? { playbooks: o.playbooks } : {},
    };
  }

  /** Registers a request/response toolbelt operation and declares it. */
  operation<S extends Type<unknown>, Out>(
    name: string,
    spec: OperationSpec<S, Out>,
  ): this {
    const output = spec.output ?? `${name}.v1`;
    return this.#operation(
      name,
      "handler",
      spec,
      async (input, ctx) => ({
        schema: output,
        data: (await spec.handler(input as Infer<S>, ctx)) ?? null,
      }),
      spec.serves === undefined ? {} : { serves: spec.serves },
    );
  }

  /**
   * Registers an operation that suspends the calling Work until an Event
   * (or a time) comes, and declares it with kind wait: "wait until this
   * ticket is resolved".
   */
  wait<S extends Type<unknown>>(name: string, spec: WaitSpec<S>): this {
    const output = spec.output ?? `${name}.v1`;
    return this.#operation(name, "wait", spec, async (input, ctx) => {
      const plan = await spec.handler(input as Infer<S>, ctx);
      return {
        wait: plan.wait,
        ...(plan.dependencies?.length
          ? { dependencies: plan.dependencies }
          : {}),
        output: { schema: output, data: plan.output ?? null },
      };
    });
  }

  /**
   * Registers an operation that emits one Event of a family the extension
   * declares, and declares it with kind emit (and commit, when spec.commit).
   */
  emit<S extends Type<unknown>>(name: string, spec: EmitSpec<S>): this {
    const output = spec.output ?? `${name}.v1`;
    return this.#operation(
      name,
      "emit",
      spec,
      async (input, ctx) => {
        const plan = await spec.handler(input as Infer<S>, ctx);
        if (plan.commit !== undefined && !spec.commit)
          throw new Error(
            `sdk: operation ${name} returned a commit but is not declared with commit: true`,
          );
        return {
          plan: {
            event: plan.event,
            ...(plan.cause ? { cause: plan.cause } : {}),
            ...(plan.once ? { once: plan.once } : {}),
          },
          commit:
            plan.commit === undefined
              ? undefined
              : (emitted: Emitted) => {
                  const c = plan.commit!(emitted);
                  return {
                    ...(c.records?.length
                      ? {
                          records: c.records.map((r) => ({
                            namespace: r.namespace,
                            key: r.key,
                            value: base64(toBytes(r.value)),
                          })),
                        }
                      : {}),
                    output: { schema: output, data: c.output ?? null },
                  };
                },
        };
      },
      spec.commit ? { commit: true } : {},
    );
  }

  #operation<S extends Type<unknown>>(
    name: string,
    kind: Kind,
    spec: OperationDoc<S>,
    run: (input: unknown, ctx: CallContext) => Promise<unknown>,
    extra: Partial<OperationDecl> = {},
  ): this {
    if (this.#operations.has(name))
      throw new Error(`sdk: operation ${name} is registered twice`);
    const input = spec.input;
    this.#operations.set(name, {
      kind,
      run: async (data, ctx) => {
        // Refused before the handler runs, so it only ever sees what its type says.
        if (!matches(input, data))
          throw new OperationError(
            "bad_request",
            `${name}: ${check(input, data)}`,
          );
        return run(data, ctx);
      },
    });
    // In the Go declaration's field order, so the written manifest reads the same.
    const decl: OperationDecl = {
      name,
      kind,
      ...(extra.commit ? { commit: true } : {}),
      ...(spec.summary === undefined ? {} : { summary: spec.summary }),
      ...(spec.guidance === undefined ? {} : { guidance: spec.guidance }),
      input: json(input),
      ...(spec.wants === undefined ? {} : { wants: spec.wants }),
      ...(extra.serves === undefined ? {} : { serves: extra.serves }),
    };
    (this.manifest.provides.operations ??= []).push(decl);
    return this;
  }

  /** Declares a scope entry type: an "extensions" entry in a scope's config. */
  entryType<S extends Type<unknown>>(
    type: string,
    spec: EntryTypeSpec<S>,
  ): this {
    return this.#entryType(
      type,
      spec.config,
      spec.validate,
      async (entries, ctx) => {
        await spec.configure?.(entries, ctx);
        return [];
      },
    );
  }

  /**
   * Declares a scope entry type whose entries own event sources, with the
   * families they observe. Each configuration generation, sources names
   * the sources the entries register; the host then reconciles each every
   * intervalSeconds through reconcile, pushes deliveries to it through
   * push, and sends requests on its webhook path (/<source name>) to
   * webhook.
   */
  ingressType<S extends Type<unknown>, D>(
    type: string,
    spec: IngressTypeSpec<S, D>,
  ): this {
    const name = this.manifest.name;
    const families = spec.families.map((f) => ({
      type: f.type,
      re_derivable: f.re_derivable,
      ...(f.loss ? { loss: f.loss } : {}),
    }));
    const handlers = spec as unknown as IngressHandlers;
    this.#entryType(
      type,
      spec.config,
      spec.validate,
      async (entries, ctx) => {
        const sources = await spec.sources(entries, ctx);
        return sources.map((s) => {
          if (!inNamespace(name, s.name))
            throw new ProtocolError(
              Code.Invalid,
              `source ${s.name} must be named ${name} or begin with ${name}/`,
            );
          if (!inNamespace(name, s.namespace))
            throw new ProtocolError(
              Code.Invalid,
              `source ${s.name}: namespace ${s.namespace} must be ${name} or begin with ${name}/`,
            );
          if (!(s.intervalSeconds >= 1))
            throw new ProtocolError(
              Code.Invalid,
              `source ${s.name}: intervalSeconds must be at least one`,
            );
          const webhook = spec.webhook !== undefined && s.webhook !== false;
          if (webhook && !s.name.startsWith(`${name}/`))
            throw new ProtocolError(
              Code.Invalid,
              `source ${s.name}: a source with a webhook must be named ${name}/<more>, so its path is under /${name}/`,
            );
          return {
            spec: { ...s, webhook } as SourceSpec<unknown>,
            type: handlers,
            decl: {
              name: s.name,
              source: { namespace: s.namespace, scope_id: s.scopeId },
              interval_seconds: Math.floor(s.intervalSeconds),
              families,
            },
          };
        });
      },
      true,
    );
    const provides = this.manifest.provides;
    // A family two ingress types share is declared once.
    for (const f of spec.families)
      if (!provides.families?.some((x) => x.type === f.type))
        (provides.families ??= []).push(f);
    if (spec.webhook !== undefined) {
      provides.webhooks = true;
      if (spec.webhookMaxBytes !== undefined)
        provides.webhook_max_bytes = Math.max(
          provides.webhook_max_bytes ?? 0,
          Math.floor(spec.webhookMaxBytes),
        );
    }
    return this;
  }

  #entryType<S extends Type<unknown>>(
    type: string,
    config: S,
    validate: ((config: Infer<S>) => void | Promise<void>) | undefined,
    configure: (
      entries: EntryOf<Infer<S>>[],
      ctx: Context,
    ) => Promise<(Registered & { decl?: SourceDecl })[]>,
    ingress = false,
  ): this {
    if (this.#entryTypes.has(type))
      throw new Error(`sdk: entry type ${type} is registered twice`);
    const decode = (e: ConfigEntry): EntryOf<Infer<S>> => {
      // An absent config is an empty one, as Go reads it.
      const c = e.config ?? {};
      if (!matches(config, c))
        throw new ProtocolError(
          Code.Invalid,
          `${type} entry in scope ${e.scope_id}: ${check(config, c)}`,
        );
      return {
        scopeId: e.scope_id,
        name: e.name ?? "",
        config: c,
        secrets: e.secrets ?? [],
        ...(e.connection ? { connection: e.connection } : {}),
      };
    };
    this.#entryTypes.set(type, {
      validate: (c) => validateConfig(config, c, validate as never),
      // Decoded even with no hook, so a bad config fails configure rather than passing silently.
      configure: (entries, ctx) =>
        configure(entries.filter((e) => e.type === type).map(decode), ctx),
    });
    (this.manifest.provides.extensions ??= []).push(
      ingress ? { type, ingress: true } : { type },
    );
    return this;
  }

  /**
   * Registers a route handler and declares it: what an Event does (open
   * Work, reach the Work it already has, or nothing). A spec with samples
   * is declared as offering them, so route traces and the routes.decisions
   * certification apply; one with config has its routes' config checked.
   */
  route<S extends Type<unknown> = Type<Record<string, unknown>>>(
    handler: string,
    spec: RouteSpec<S>,
  ): this {
    if (this.#routes.has(handler))
      throw new Error(`sdk: route handler ${handler} is registered twice`);
    const schema = spec.config;
    const decode = (p: { data?: unknown }): Infer<S> => {
      const c = p.data ?? {};
      if (schema !== undefined && !matches(schema, c))
        throw new ProtocolError(
          Code.Invalid,
          `route config: ${check(schema, c)}`,
        );
      return c as Infer<S>;
    };
    this.#routes.set(handler, {
      validate: async (c) => {
        if (schema !== undefined) await validateConfig(schema, c);
      },
      match: async (input, ctx) =>
        spec.match({ ...input, config: decode(input.route.config) }, ctx),
      samples: async (p, ctx) =>
        spec.samples === undefined
          ? []
          : spec.samples(
              {
                scope: p.scope,
                route: p.route,
                now: p.now,
                config: decode(p.route.config),
              },
              ctx,
            ),
    });
    (this.manifest.provides.routes ??= []).push({
      handler,
      ...(schema === undefined ? {} : { config: true }),
      ...(spec.samples === undefined ? {} : { samples: true }),
    });
    return this;
  }

  /** Declares a kind of Work the extension's routes create, with its lifecycle. */
  workKind(kind: string, spec: WorkKindSpec = {}): this {
    if (spec.trigger !== undefined) this.#triggers.set(kind, spec.trigger);
    (this.manifest.provides.work_kinds ??= []).push({
      kind,
      ...(spec.standing ? { standing: true } : {}),
      ...(spec.trigger ? { trigger: true } : {}),
      ...(spec.timed ? { timed: true } : {}),
      ...(spec.runOnce ? { run_once: true } : {}),
    });
    return this;
  }

  /** Registers a doctor check and declares it. */
  check(name: string, fn: CheckFn, o: { deep?: boolean } = {}): this {
    this.#checks.set(name, fn);
    (this.manifest.provides.checks ??= []).push(
      o.deep ? { name, deep: true } : { name },
    );
    return this;
  }

  /** Registers a task the host runs every intervalSeconds (at least one) and declares it. */
  task(name: string, intervalSeconds: number, fn: TaskFn): this {
    this.#tasks.set(name, fn);
    (this.manifest.provides.tasks ??= []).push({
      name,
      interval_seconds: Math.floor(intervalSeconds),
    });
    return this;
  }

  /** Declares event families the extension derives through host.accept or emits (an ingress type's come with it). */
  family(...families: Family[]): this {
    (this.manifest.provides.families ??= []).push(...families);
    return this;
  }

  /**
   * Declares a payload schema the extension writes and reads, named
   * <extension>.<thing> and written as <name>.v<N>: the versions it can
   * read, ascending, the highest being the one it writes. The host refuses
   * an Event (ingress, event.accept or emitted) whose data names any other.
   */
  schema(name: string, ...versions: number[]): this {
    if (
      versions.length === 0 ||
      versions.some(
        (v, i) =>
          !Number.isInteger(v) || v < 1 || (i > 0 && v <= versions[i - 1]!),
      )
    )
      throw new Error(
        `sdk: schema ${name} needs positive, strictly increasing versions`,
      );
    (this.manifest.provides.schemas ??= []).push({
      name,
      versions: [...versions],
    });
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
    if (argv.length === 1 && argv[0] === "package-manifest")
      return print(this.manifest);
    if (argv.length === 1 && argv[0] === "package-certification")
      return this.#certification === undefined
        ? undefined
        : print(this.#certification);
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
  _served(): {
    operations: Map<string, Op>;
    checks: Map<string, CheckFn>;
    tasks: Map<string, TaskFn>;
    entryTypes: Map<string, EntryType>;
    routes: Map<string, RouteHandler>;
    triggers: Map<string, (inputs: Event[]) => number>;
    migrate: MigrateFn | undefined;
  } {
    return {
      operations: this.#operations,
      checks: this.#checks,
      tasks: this.#tasks,
      entryTypes: this.#entryTypes,
      routes: this.#routes,
      triggers: this.#triggers,
      migrate: this.#migrate,
    };
  }
}

/** protocol.SourceNamespace: the name itself, or it and "/" and more. */
function inNamespace(name: string, ns: string): boolean {
  return (
    ns === name || (ns.startsWith(`${name}/`) && ns.length > name.length + 1)
  );
}

function json(schema: Type<unknown>): Record<string, unknown> {
  // A round trip through JSON gives the schema exactly as the manifest file holds it.
  const plain: unknown = JSON.parse(JSON.stringify(schema));
  return isRecord(plain) ? plain : {};
}

function print(v: unknown): Promise<void> {
  return new Promise((resolve) =>
    process.stdout.write(JSON.stringify(v, null, 2) + "\n", () => resolve()),
  );
}

function notFound(method: string): ProtocolError {
  return new ProtocolError(Code.MethodNotFound, `no such method ${method}`);
}

/** Decodes a request's params, refusing malformed ones as jsonrpc.Params does in Go. */
function params<S extends Type<unknown>>(schema: S, raw: unknown): Infer<S> {
  const p = raw ?? {};
  if (!matches(schema, p))
    throw new ProtocolError(Code.Invalid, "malformed params");
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
  /** The sources the last configure registered, by name. */
  #sources = new Map<string, Registered>();
  /** Emissions' commits, by commit ID, until the host has had its chance to call them. */
  readonly #commits = new Map<string, (emitted: Emitted) => unknown>();

  constructor(ext: Extension, input: Readable, output: Writable) {
    this.#ext = ext;
    this.#conn = new Conn(
      input,
      output,
      (method, raw, signal) => this.#handle(method, raw, signal),
      {
        onProtocolError: (reason) =>
          process.stderr.write(`open-conduit sdk: ${reason}\n`),
      },
    );
    this.host = new Host((method, p, signal) =>
      this.#conn.call(method, p, signal),
    );
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

  async #handle(
    method: string,
    raw: unknown,
    signal: AbortSignal,
  ): Promise<unknown> {
    if (method === "initialize")
      return this.#initialize(params(InitializeParams, raw));
    // The host must know the manifest before anything else is served.
    if (!this.#ready) throw new ProtocolError(Code.Invalid, "initialize first");
    if (this.#draining)
      throw new ProtocolError(Code.ShuttingDown, "shutting down");
    const served = this.#ext._served();
    switch (method) {
      case "configure":
        return this.#configure(params(ConfigureParams, raw), signal);
      case "shutdown":
        this.#draining = true;
        return {};
      case "validate": {
        const p = params(ValidateParams, raw);
        // The host shows the operator a ConfigError's diagnostics; of any
        // other refusal, only that the config is invalid.
        if (p.kind === "extension")
          await served.entryTypes.get(p.type)?.validate(p.config);
        if (p.kind === "route")
          await served.routes.get(p.type)?.validate(p.config);
        return {};
      }
      case "operation.call":
      case "operation.wait":
      case "operation.emit": {
        const p = params(OperationParams, raw);
        const op = served.operations.get(p.operation);
        if (op === undefined)
          throw new ProtocolError(Code.Undeclared, "unknown operation");
        if (method !== `operation.${op.kind === "handler" ? "call" : op.kind}`)
          throw new ProtocolError(Code.Undeclared, "operation kind mismatch");
        const out = await op.run(p.input.data ?? {}, {
          host: this.host,
          authority: p.authority,
          requestId: p.request_id ?? "",
          signal,
        });
        if (op.kind === "handler") return { output: out };
        if (op.kind === "wait") return out;
        const { plan, commit } = out as {
          plan: Record<string, unknown>;
          commit?: (emitted: Emitted) => unknown;
        };
        if (commit === undefined) return plan;
        // Random, as Go's newID: a restarted process must never answer an ID the host holds from before with another emission's commit.
        const id = randomBytes(12).toString("hex");
        this.#commits.set(id, commit);
        // Dropped oldest first, whether the host called them or never did
        // (its transaction failed before acceptance).
        for (const old of this.#commits.keys()) {
          if (this.#commits.size <= 1024) break;
          this.#commits.delete(old);
        }
        return { ...plan, commit_id: id };
      }
      case "emit.commit": {
        const p = params(EmitCommitParams, raw);
        // It stays callable: the host may ask again when its transaction
        // runs again, possibly over a different acceptance.
        const commit = this.#commits.get(p.commit_id);
        if (commit === undefined)
          throw new ProtocolError(Code.NotFound, "unknown commit");
        return commit(p.emitted);
      }
      case "ingress.push": {
        const p = params(IngressPushParams, raw);
        const s = this.#source(p.name);
        if (s.type.push === undefined) throw notFound(method);
        const d = p.delivery;
        return s.type.push(
          s.spec,
          {
            id: d.id,
            receivedAt: d.received_at,
            ...(d.content_type ? { contentType: d.content_type } : {}),
            body: bytes(d.body),
          },
          { host: this.host, signal },
        );
      }
      case "ingress.reconcile": {
        const p = params(IngressReconcileParams, raw);
        const s = this.#source(p.name);
        if (s.type.reconcile === undefined)
          return { scan_id: p.scan.scan_id || "none", complete: false };
        return s.type.reconcile(s.spec, p.scan, { host: this.host, signal });
      }
      case "webhook.handle":
        return this.#webhook(params(WebhookHandleParams, raw), signal);
      case "route.match": {
        const p = params(RouteMatchParams, raw);
        const route = served.routes.get(p.handler);
        if (route === undefined) throw notFound(method);
        return route.match(p.input, { host: this.host, signal });
      }
      case "route.samples": {
        const p = params(RouteSamplesParams, raw);
        const route = served.routes.get(p.handler);
        if (route === undefined) throw notFound(method);
        return { samples: await route.samples(p, { host: this.host, signal }) };
      }
      case "work_kind.trigger": {
        const p = params(WorkKindTriggerParams, raw);
        const trigger = served.triggers.get(p.kind);
        if (trigger === undefined) throw notFound(method);
        return { index: trigger(p.inputs ?? []) };
      }
      case "task.run": {
        const fn = served.tasks.get(params(TaskRunParams, raw).task);
        if (fn === undefined) throw notFound(method);
        return (await fn({ host: this.host, signal })) ?? {};
      }
      case "check.run": {
        const p = params(CheckRunParams, raw);
        const fn = served.checks.get(p.check);
        if (fn === undefined) throw notFound(method);
        try {
          return result(
            true,
            await fn({
              host: this.host,
              deep: p.deep === true,
              config: p.config,
              signal,
            }),
          );
        } catch (err) {
          return result(
            false,
            err instanceof Error ? err.message : String(err),
          );
        }
      }
    }
    throw notFound(method);
  }

  #initialize(p: InitializeParams): unknown {
    const offered = p.protocol_versions ?? [];
    const common = offered.filter((v) =>
      this.#ext.manifest.protocol.includes(v),
    );
    if (common.length === 0)
      throw new ProtocolError(
        Code.UnsupportedVersion,
        "no common protocol version",
      );
    this.#ready = true;
    this.#stateDir = p.state_dir;
    return {
      protocol_version: Math.max(...common),
      manifest: this.#ext.manifest,
    };
  }

  async #configure(p: ConfigureParams, signal: AbortSignal): Promise<unknown> {
    const { entryTypes, migrate } = this.#ext._served();
    const ctx = { host: this.host, signal };
    if (p.kv.stored < p.kv.declared) {
      // From layout 0 there is nothing stored to migrate, so no migration is needed.
      if (migrate === undefined && p.kv.stored !== 0)
        throw new ProtocolError(Code.Unsupported, "no KV migration");
      await migrate?.(p.kv.stored, p.kv.declared, ctx);
    }
    const sources = new Map<string, Registered>();
    const decls: SourceDecl[] = [];
    const paths: string[] = [];
    for (const entryType of entryTypes.values())
      for (const r of await entryType.configure(p.entries ?? [], ctx)) {
        const { decl, ...registered } = r as Registered & { decl: SourceDecl };
        if (sources.has(decl.name))
          throw new ProtocolError(
            Code.Invalid,
            `source ${decl.name} is registered twice`,
          );
        sources.set(decl.name, registered);
        decls.push(decl);
        if (registered.spec.webhook) paths.push(`/${decl.name}`);
      }
    // Replaced only once the whole generation is read, so a refused one keeps the last.
    this.#sources = sources;
    const out: ConfigureResult = {
      kv_version: this.#ext.manifest.kv_version ?? 0,
    };
    if (decls.length > 0) out.sources = decls;
    if (paths.length > 0) out.webhook_paths = paths;
    return out;
  }

  #source(name: string): Registered {
    const s = this.#sources.get(name);
    if (s === undefined)
      throw new ProtocolError(Code.NotFound, "unknown source");
    return s;
  }

  async #webhook(
    p: WebhookHandleParams,
    signal: AbortSignal,
  ): Promise<WebhookHandleResult> {
    // The source whose path is the request's, or the deepest one above it.
    let found: Registered | undefined;
    for (const [name, s] of this.#sources) {
      const path = `/${name}`;
      if (
        s.spec.webhook &&
        s.type.webhook !== undefined &&
        (p.path === path || p.path.startsWith(`${path}/`)) &&
        (found === undefined || name.length > found.spec.name.length)
      )
        found = s;
    }
    if (found === undefined) return { status: 404 };
    const source = found;
    const headers = new Headers();
    for (const [k, vs] of Object.entries(p.header ?? {}))
      for (const v of vs) headers.append(k, v);
    const request: WebhookRequest = {
      path: p.path,
      method: p.method,
      query: p.query ?? "",
      headers,
      body: bytes(p.body),
      receivedAt: p.received_at,
      forwardId: p.forward_id ?? "",
    };
    try {
      const r = await source.type.webhook!(source.spec, request, {
        host: this.host,
        signal,
        deliver: (d) =>
          this.host.deliver(
            source.spec.name,
            { ...d, receivedAt: d.receivedAt ?? p.received_at },
            signal,
          ),
      });
      const out: WebhookHandleResult = { status: r.status ?? 200 };
      if (r.headers !== undefined)
        out.header = Object.fromEntries(
          Object.entries(r.headers).map(([k, v]) => [
            k,
            Array.isArray(v) ? v : [v],
          ]),
        );
      if (r.body !== undefined) out.body = base64(toBytes(r.body));
      return out;
    } catch (err) {
      // The sender retries a 5xx; nothing is accepted until deliver answers.
      process.stderr.write(
        `open-conduit sdk: webhook ${p.path}: ${err instanceof Error ? err.message : String(err)}\n`,
      );
      return { status: 500 };
    }
  }
}

function bytes(b64: string | undefined): Uint8Array {
  return b64 === undefined
    ? new Uint8Array()
    : new Uint8Array(Buffer.from(b64, "base64"));
}

function result(ok: boolean, detail: string): CheckRunResult {
  return detail === "" ? { ok } : { ok, detail };
}
