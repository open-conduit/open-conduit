// The wire messages this SDK uses, as protocol/messages.go, protocol/wire.go
// and protocol/manifest.go encode them. Each is a schema and a type of the
// same name: the SDK decodes what the host sends through the schema, so a
// malformed message is refused instead of reaching extension code as a lie.
//
// protocol/typescript_parity_test.go checks every field here against the Go
// type's JSON fields, so a renamed or removed Go field fails the Go tests.
//
// Times are RFC 3339 strings. Bytes ([]byte in Go: KV values, artifact
// chunks) are base64 strings; the Host client converts them. Go sends a nil
// slice or map as null, hence the t.nullable on the ones the SDK decodes.

import { type Infer, t } from "./schema.ts";

// A newer host may add fields; ignoring them is what keeps old extensions working.
const open = { open: true };

/** A versioned payload: data is any JSON value, schema names its shape. */
export const Payload = t.object(
  {
    schema: t.optional(t.string()),
    data: t.optional(t.any()),
  },
  open,
);
export type Payload = Infer<typeof Payload>;

export const Key = t.object(
  {
    source: t.string(),
    external_id: t.string(),
  },
  open,
);
export type Key = Infer<typeof Key>;

export const Event = t.object(
  {
    id: t.optional(t.string()),
    type: t.string(),
    source: t.optional(t.string()),
    delivery_id: t.optional(t.string()),
    occurred_at: t.optional(t.string()),
    received_at: t.optional(t.string()),
    correlation_key: t.optional(Key),
    causation_id: t.optional(t.string()),
    work_id: t.optional(t.string()),
    execution_id: t.optional(t.string()),
    data: t.optional(Payload),
    // Who caused the Event, as kind:id, when the host knows. The host sets
    // it; an Event an extension submits never carries one.
    actor: t.optional(t.string()),
    // The client the actor acted through, as kind:id, when a client
    // credential acted for the actor (a person signed in to it).
    via: t.optional(t.string()),
  },
  open,
);
export type Event = Infer<typeof Event>;

export const AcceptedEvent = t.object(
  {
    event: Event,
    sequence: t.integer(),
    routed: t.boolean(),
    routed_work_id: t.optional(t.string()),
    disposition: t.optional(t.string()),
    scope_id: t.optional(t.string()),
  },
  open,
);
export type AcceptedEvent = Infer<typeof AcceptedEvent>;

export const DisplayLink = t.object(
  {
    label: t.string(),
    url: t.string(),
  },
  open,
);
export type DisplayLink = Infer<typeof DisplayLink>;

export const Display = t.object(
  {
    title: t.optional(t.string()),
    ref: t.optional(t.string()),
    requester: t.optional(t.string()),
    links: t.optional(t.array(DisplayLink)),
  },
  open,
);
export type Display = Infer<typeof Display>;

export const Work = t.object(
  {
    id: t.string(),
    kind: t.string(),
    scope_id: t.string(),
    state: t.string(),
    correlation_keys: t.nullable(t.array(Key)),
    context: Payload,
    parent_id: t.optional(t.string()),
    display: t.optional(Display),
    // The executor of the Work's latest attempt, when recorded.
    latest_executor: t.optional(t.string()),
    revision: t.integer(),
    created_at: t.string(),
    updated_at: t.string(),
  },
  open,
);
export type Work = Infer<typeof Work>;

/** One Event for a Work that no attempt of it has read yet: queued, or accepted and not yet routed (unrouted). */
export const PendingInput = t.object(
  {
    event_id: t.string(),
    sequence: t.integer(),
    type: t.string(),
    binding: t.optional(t.string()),
    unrouted: t.optional(t.boolean()),
  },
  open,
);
export type PendingInput = Infer<typeof PendingInput>;

export const LeaseRef = t.object(
  {
    execution_id: t.string(),
    owner: t.string(),
    epoch: t.integer(),
  },
  open,
);
export type LeaseRef = Infer<typeof LeaseRef>;

/** The authority an operation call runs under: its scope, Work and grant. */
export const Authority = t.object(
  {
    scope_id: t.string(),
    work_id: t.string(),
    execution_id: t.string(),
    lease: LeaseRef,
    expires_at: t.string(),
    operations: t.optional(t.array(t.string())),
    event_types: t.optional(t.array(t.string())),
    trigger_id: t.optional(t.string()),
    // The connection the execution's binding names, whose secrets the operation may resolve.
    connection: t.optional(t.string()),
    // The kinds of child Work the execution may ask for, each with what to ask it for.
    work_kinds: t.optional(
      t.array(
        t.object(
          { kind: t.string(), description: t.optional(t.string()) },
          open,
        ),
      ),
    ),
  },
  open,
);
export type Authority = Infer<typeof Authority>;

// Lifecycle.

export const HostInfo = t.object(
  {
    name: t.string(),
    version: t.string(),
  },
  open,
);
export type HostInfo = Infer<typeof HostInfo>;

export const InitializeParams = t.object(
  {
    protocol_versions: t.nullable(t.array(t.integer())),
    host: HostInfo,
    state_dir: t.string(),
  },
  open,
);
export type InitializeParams = Infer<typeof InitializeParams>;

export const InitializeResult = t.object(
  {
    protocol_version: t.integer(),
    manifest: t.any(),
  },
  open,
);
export type InitializeResult = Infer<typeof InitializeResult>;

export const ConnectionEntry = t.object(
  {
    id: t.string(),
    type: t.string(),
    config: t.optional(t.any()),
    secrets: t.optional(t.array(t.string())),
  },
  open,
);
export type ConnectionEntry = Infer<typeof ConnectionEntry>;

/** One configured scope entry of one of the extension's types. */
export const ConfigEntry = t.object(
  {
    scope_id: t.string(),
    type: t.string(),
    config: t.optional(t.any()),
    secrets: t.optional(t.array(t.string())),
    name: t.optional(t.string()),
    connection: t.optional(ConnectionEntry),
    binding_connections: t.optional(t.array(ConnectionEntry)),
  },
  open,
);
export type ConfigEntry = Infer<typeof ConfigEntry>;

export const KVState = t.object(
  {
    stored: t.integer(),
    declared: t.integer(),
  },
  open,
);
export type KVState = Infer<typeof KVState>;

export const ConfigureParams = t.object(
  {
    generation: t.string(),
    entries: t.optional(t.array(ConfigEntry)),
    disabled: t.optional(t.array(ConfigEntry)),
    public_url: t.optional(t.string()),
    dry_run: t.optional(t.boolean()),
    kv: KVState,
  },
  open,
);
export type ConfigureParams = Infer<typeof ConfigureParams>;

export const ValidateParams = t.object(
  {
    kind: t.string(),
    type: t.string(),
    config: t.optional(t.any()),
    secret_names: t.optional(t.array(t.string())),
  },
  open,
);
export type ValidateParams = Infer<typeof ValidateParams>;

// Operations, tasks and checks.

export const OperationParams = t.object(
  {
    operation: t.string(),
    authority: Authority,
    input: Payload,
    request_id: t.optional(t.string()),
  },
  open,
);
export type OperationParams = Infer<typeof OperationParams>;

export const OperationResult = t.object(
  {
    output: Payload,
  },
  open,
);
export type OperationResult = Infer<typeof OperationResult>;

export const TaskRunParams = t.object(
  {
    task: t.string(),
  },
  open,
);
export type TaskRunParams = Infer<typeof TaskRunParams>;

export const CheckRunParams = t.object(
  {
    check: t.string(),
    deep: t.optional(t.boolean()),
    config: t.any(),
  },
  open,
);
export type CheckRunParams = Infer<typeof CheckRunParams>;

export const CheckRunResult = t.object(
  {
    ok: t.boolean(),
    detail: t.optional(t.string()),
    status: t.optional(t.string()),
    code: t.optional(t.string()),
    items: t.optional(t.array(t.string())),
  },
  open,
);
export type CheckRunResult = Infer<typeof CheckRunResult>;

// Extension-to-host calls.

export const KVGetParams = t.object(
  {
    namespace: t.string(),
    key: t.string(),
  },
  open,
);
export type KVGetParams = Infer<typeof KVGetParams>;

export const KVEntry = t.object(
  {
    key: t.string(),
    value: t.nullable(t.string()),
    version: t.string(),
  },
  open,
);
export type KVEntry = Infer<typeof KVEntry>;

export const KVGetResult = t.object(
  {
    found: t.boolean(),
    entry: KVEntry,
  },
  open,
);
export type KVGetResult = Infer<typeof KVGetResult>;

export const KVListParams = t.object(
  {
    namespace: t.string(),
    prefix: t.optional(t.string()),
  },
  open,
);
export type KVListParams = Infer<typeof KVListParams>;

export const KVListResult = t.object(
  {
    entries: t.nullable(t.array(KVEntry)),
  },
  open,
);
export type KVListResult = Infer<typeof KVListResult>;

/** A precondition: the key holds version, or is absent. */
export const KVCheck = t.object(
  {
    namespace: t.string(),
    key: t.string(),
    version: t.optional(t.string()),
    absent: t.optional(t.boolean()),
  },
  open,
);
export type KVCheck = Infer<typeof KVCheck>;

export const KVPut = t.object(
  {
    namespace: t.string(),
    key: t.string(),
    value: t.string(),
  },
  open,
);
export type KVPut = Infer<typeof KVPut>;

export const KVDelete = t.object(
  {
    namespace: t.string(),
    key: t.string(),
  },
  open,
);
export type KVDelete = Infer<typeof KVDelete>;

export const KVCommitParams = t.object(
  {
    checks: t.optional(t.array(KVCheck)),
    puts: t.optional(t.array(KVPut)),
    deletes: t.optional(t.array(KVDelete)),
  },
  open,
);
export type KVCommitParams = Infer<typeof KVCommitParams>;

/** A Work's position in a state.works order: the next page starts after it. */
export const WorkCursor = t.object(
  {
    at: t.string(),
    id: t.string(),
  },
  open,
);
export type WorkCursor = Infer<typeof WorkCursor>;

export const StateParams = t.object(
  {
    id: t.optional(t.string()),
    scope_id: t.optional(t.string()),
    after: t.optional(t.integer()),
    limit: t.optional(t.integer()),
    states: t.optional(t.array(t.string())),
    kind: t.optional(t.string()),
    parent_id: t.optional(t.string()),
    sources: t.optional(t.array(t.string())),
    executor: t.optional(t.string()),
    correlation: t.optional(Key),
    updated_since: t.optional(t.string()),
    order: t.optional(t.string()),
    cursor: t.optional(WorkCursor),
    source: t.optional(t.string()),
    delivery_id: t.optional(t.string()),
  },
  open,
);
export type StateParams = Infer<typeof StateParams>;

export const EventAcceptParams = t.object(
  {
    scope_id: t.string(),
    parent: t.string(),
    event: Event,
    checks: t.optional(t.array(KVCheck)),
    records: t.optional(t.array(KVPut)),
    deletes: t.optional(t.array(KVDelete)),
  },
  open,
);
export type EventAcceptParams = Infer<typeof EventAcceptParams>;

export const EventAcceptResult = t.object(
  {
    event: AcceptedEvent,
    // False when the host already had this Event: accepting is idempotent.
    added: t.boolean(),
  },
  open,
);
export type EventAcceptResult = Infer<typeof EventAcceptResult>;

export const SecretResolveParams = t.object(
  {
    scope_id: t.string(),
    connection: t.optional(t.string()),
    names: t.array(t.string()),
  },
  open,
);
export type SecretResolveParams = Infer<typeof SecretResolveParams>;

export const SecretResolveResult = t.object(
  {
    values: t.nullable(t.record(t.string())),
  },
  open,
);
export type SecretResolveResult = Infer<typeof SecretResolveResult>;

export const ArtifactCreateParams = t.object(
  {
    media_type: t.string(),
  },
  open,
);
export type ArtifactCreateParams = Infer<typeof ArtifactCreateParams>;

export const ArtifactCreateResult = t.object(
  {
    writer: t.string(),
  },
  open,
);
export type ArtifactCreateResult = Infer<typeof ArtifactCreateResult>;

export const ArtifactWriteParams = t.object(
  {
    writer: t.string(),
    chunk: t.string(),
  },
  open,
);
export type ArtifactWriteParams = Infer<typeof ArtifactWriteParams>;

export const ArtifactTarget = t.object(
  {
    writer: t.string(),
  },
  open,
);
export type ArtifactTarget = Infer<typeof ArtifactTarget>;

export const ArtifactRef = t.object(
  {
    id: t.string(),
    size: t.integer(),
    media_type: t.optional(t.string()),
    // The content's SHA-256; return the ref artifact.commit gave unchanged.
    sha256: t.optional(t.string()),
  },
  open,
);
export type ArtifactRef = Infer<typeof ArtifactRef>;

// Ingress: sources, deliveries, push and reconcile (protocol.Source and on).

/** A source's namespace (its correlation keys' source) in a scope. */
export const Source = t.object(
  {
    namespace: t.string(),
    scope_id: t.string(),
  },
  open,
);
export type Source = Infer<typeof Source>;

/** One delivery of a push (a webhook's body); body is base64. */
export const Delivery = t.object(
  {
    id: t.string(),
    received_at: t.string(),
    content_type: t.optional(t.string()),
    body: t.optional(t.string()),
  },
  open,
);
export type Delivery = Infer<typeof Delivery>;

export const EventFamily = t.object(
  {
    type: t.string(),
    re_derivable: t.boolean(),
    loss: t.optional(t.string()),
  },
  open,
);
export type EventFamily = Infer<typeof EventFamily>;

/** What a source holds of one object now: its ID, version and the Event it maps to. */
export const Observation = t.object(
  {
    id: t.string(),
    version: t.string(),
    event: Event,
  },
  open,
);
export type Observation = Infer<typeof Observation>;

export const PushResult = t.object(
  {
    observations: t.optional(t.array(Observation)),
    // Only for families declared not re-derivable.
    events: t.optional(t.array(Event)),
  },
  open,
);
export type PushResult = Infer<typeof PushResult>;

export const Scan = t.object(
  {
    source: Source,
    scan_id: t.optional(t.string()),
    cursor: t.optional(t.string()),
    checkpoint: t.optional(t.string()),
  },
  open,
);
export type Scan = Infer<typeof Scan>;

/**
 * One page of a reconcile. complete certifies the scan covered the whole
 * source across its pages; false with no next_cursor is a partial scan,
 * never proof that anything is gone. not_before (an RFC 3339 time) says the
 * provider can't answer this page yet, a rate limit say: such a page has no
 * observations, is not complete and repeats the scan's cursor as next_cursor,
 * and the host asks for it again at that time without counting a failure.
 */
export const Snapshot = t.object(
  {
    scan_id: t.string(),
    observations: t.optional(t.array(Observation)),
    next_cursor: t.optional(t.string()),
    complete: t.boolean(),
    checkpoint: t.optional(t.string()),
    not_before: t.optional(t.string()),
  },
  open,
);
export type Snapshot = Infer<typeof Snapshot>;

/** A source an entry registers at configure. */
export const SourceDecl = t.object(
  {
    name: t.string(),
    source: Source,
    interval_seconds: t.integer(),
    families: t.array(EventFamily),
  },
  open,
);
export type SourceDecl = Infer<typeof SourceDecl>;

export const ConfigureResult = t.object(
  {
    sources: t.optional(t.array(SourceDecl)),
    // Each under /<name>/; a path covers the paths beneath it.
    webhook_paths: t.optional(t.array(t.string())),
    kv_version: t.integer(),
  },
  open,
);
export type ConfigureResult = Infer<typeof ConfigureResult>;

export const IngressPushParams = t.object(
  {
    name: t.string(),
    source: Source,
    delivery: Delivery,
  },
  open,
);
export type IngressPushParams = Infer<typeof IngressPushParams>;

export const IngressReconcileParams = t.object(
  {
    name: t.string(),
    scan: Scan,
  },
  open,
);
export type IngressReconcileParams = Infer<typeof IngressReconcileParams>;

export const IngressDeliverParams = t.object(
  {
    name: t.string(),
    delivery: Delivery,
  },
  open,
);
export type IngressDeliverParams = Infer<typeof IngressDeliverParams>;

/** A delivery's accepted Events; added[i] is false for one already accepted (a retry). */
export const IngressDeliverResult = t.object(
  {
    accepted: t.nullable(t.array(AcceptedEvent)),
    added: t.nullable(t.array(t.boolean())),
  },
  open,
);
export type IngressDeliverResult = Infer<typeof IngressDeliverResult>;

/** One HTTP request on a webhook path the extension registered; body is base64. */
export const WebhookHandleParams = t.object(
  {
    path: t.string(),
    method: t.string(),
    query: t.optional(t.string()),
    header: t.optional(t.record(t.array(t.string()))),
    body: t.optional(t.string()),
    received_at: t.string(),
    forward_id: t.optional(t.string()),
  },
  open,
);
export type WebhookHandleParams = Infer<typeof WebhookHandleParams>;

export const WebhookHandleResult = t.object(
  {
    status: t.integer(),
    header: t.optional(t.record(t.array(t.string()))),
    body: t.optional(t.string()),
  },
  open,
);
export type WebhookHandleResult = Infer<typeof WebhookHandleResult>;

// Routing.

export const Route = t.object(
  {
    name: t.string(),
    handler: t.string(),
    binding: t.string(),
    active_policy: t.optional(t.string()),
    config: Payload,
  },
  open,
);
export type Route = Infer<typeof Route>;

/** A scope as an extension sees it: never its secret references. */
export const Scope = t.object(
  {
    id: t.string(),
    name: t.string(),
    parent_id: t.optional(t.string()),
    bindings: t.optional(t.array(t.any())),
    routes: t.optional(t.array(Route)),
    config: Payload,
    revision: t.integer(),
    created_at: t.string(),
    updated_at: t.string(),
  },
  open,
);
export type Scope = Infer<typeof Scope>;

export const RouteInput = t.object(
  {
    event: AcceptedEvent,
    scope: Scope,
    work: t.optional(Work),
    route: Route,
  },
  open,
);
export type RouteInput = Infer<typeof RouteInput>;

/**
 * A route's answer for one Event. On a match with no work, create_id and
 * kind open new Work; attach_keys add correlation keys to it; display
 * replaces what the Work shows. deliver is absent to queue the Event on
 * the route's binding, "none" to only attach, "current" to queue it on the
 * binding the Work runs on.
 */
export const RouteDecision = t.object(
  {
    match: t.boolean(),
    create_id: t.optional(t.string()),
    kind: t.optional(t.string()),
    attach_keys: t.optional(t.array(Key)),
    reason: t.optional(t.string()),
    display: t.optional(Display),
    context: t.optional(Payload),
    grants: t.optional(t.array(t.string())),
    successor: t.optional(t.boolean()),
    deliver: t.optional(t.string()),
  },
  open,
);
export type RouteDecision = Infer<typeof RouteDecision>;

/** A sample Event a route handler offers for a route trace (routes.decisions). */
export const RouteSample = t.object(
  {
    id: t.string(),
    title: t.string(),
    event: Event,
  },
  open,
);
export type RouteSample = Infer<typeof RouteSample>;

export const RouteMatchParams = t.object(
  {
    handler: t.string(),
    input: RouteInput,
  },
  open,
);
export type RouteMatchParams = Infer<typeof RouteMatchParams>;

export const RouteSamplesParams = t.object(
  {
    handler: t.string(),
    scope: Scope,
    route: Route,
    now: t.string(),
  },
  open,
);
export type RouteSamplesParams = Infer<typeof RouteSamplesParams>;

export const RouteSamplesResult = t.object(
  {
    samples: t.array(RouteSample),
  },
  open,
);
export type RouteSamplesResult = Infer<typeof RouteSamplesResult>;

export const WorkKindTriggerParams = t.object(
  {
    kind: t.string(),
    inputs: t.nullable(t.array(Event)),
  },
  open,
);
export type WorkKindTriggerParams = Infer<typeof WorkKindTriggerParams>;

export const WorkKindTriggerResult = t.object(
  {
    index: t.integer(),
  },
  open,
);
export type WorkKindTriggerResult = Infer<typeof WorkKindTriggerResult>;

// Wait and emit operations.

/**
 * What a wait suspends the Work until: an Event of one of types (any, when
 * empty) on one of keys (every key the Work owns, when empty), or timeout.
 */
export const WaitRequest = t.object(
  {
    keys: t.optional(t.array(Key)),
    types: t.optional(t.array(t.string())),
    timeout: t.optional(t.string()),
    after_sequence: t.optional(t.integer()),
    description: t.optional(t.string()),
  },
  open,
);
export type WaitRequest = Infer<typeof WaitRequest>;

/**
 * A subscription to Events on another key (another Work's, or one no Work
 * owns yet), aliased into the waiting Work as alias_type Events.
 */
export const Dependency = t.object(
  {
    id: t.string(),
    source_scope_id: t.optional(t.string()),
    source_key: Key,
    types: t.optional(t.array(t.string())),
    alias_type: t.string(),
    filter: Payload,
    label: t.optional(t.string()),
    from_now: t.optional(t.boolean()),
  },
  open,
);
export type Dependency = Infer<typeof Dependency>;

export const WaitPlan = t.object(
  {
    wait: WaitRequest,
    dependencies: t.optional(t.array(Dependency)),
    output: Payload,
  },
  open,
);
export type WaitPlan = Infer<typeof WaitPlan>;

export const EmitPlan = t.object(
  {
    event: Event,
    cause: t.optional(t.string()),
    once: t.optional(t.string()),
    commit_id: t.optional(t.string()),
  },
  open,
);
export type EmitPlan = Infer<typeof EmitPlan>;

/** The Event an emission accepted, as its commit receives it. */
export const Emitted = t.object(
  {
    event: AcceptedEvent,
    added: t.boolean(),
    root: t.string(),
    depth: t.integer(),
  },
  open,
);
export type Emitted = Infer<typeof Emitted>;

/** A KV write an emission's commit makes; value is base64. */
export const EmitRecord = t.object(
  {
    namespace: t.string(),
    key: t.string(),
    value: t.string(),
  },
  open,
);
export type EmitRecord = Infer<typeof EmitRecord>;

export const EmitCommit = t.object(
  {
    records: t.optional(t.array(EmitRecord)),
    output: Payload,
  },
  open,
);
export type EmitCommit = Infer<typeof EmitCommit>;

export const EmitCommitParams = t.object(
  {
    commit_id: t.string(),
    emitted: Emitted,
  },
  open,
);
export type EmitCommitParams = Infer<typeof EmitCommitParams>;

// The manifest (open-conduit-extension.json): the fields this SDK writes.

export const Family = t.object(
  {
    type: t.string(),
    // Whether a lost Event can be derived again, which tells the host how to recover.
    re_derivable: t.boolean(),
    loss: t.optional(t.string()),
    // Only the extension's own emit operations may emit it; the builtin emit refuses it.
    sealed: t.optional(t.boolean()),
  },
  open,
);
export type Family = Infer<typeof Family>;

/** A payload schema the extension writes and reads: the versions it reads, the highest being the one it writes. */
export const SchemaDecl = t.object(
  {
    name: t.string(),
    versions: t.array(t.integer()),
  },
  open,
);
export type SchemaDecl = Infer<typeof SchemaDecl>;

export const ExtensionDecl = t.object(
  {
    type: t.string(),
    // The entry registers ingress sources at configure.
    ingress: t.optional(t.boolean()),
  },
  open,
);
export type ExtensionDecl = Infer<typeof ExtensionDecl>;

export const OperationDecl = t.object(
  {
    name: t.string(),
    kind: t.enum(["handler", "wait", "emit"]),
    commit: t.optional(t.boolean()),
    summary: t.optional(t.string()),
    guidance: t.optional(t.string()),
    input: t.optional(t.record(t.any())),
    wants: t.optional(t.string()),
    serves: t.optional(t.string()),
  },
  open,
);
export type OperationDecl = Infer<typeof OperationDecl>;

export const RouteDecl = t.object(
  {
    handler: t.string(),
    // Its routes carry config, checked through validate with kind route.
    config: t.optional(t.boolean()),
    // It offers sample Events for a route trace through route.samples.
    samples: t.optional(t.boolean()),
  },
  open,
);
export type RouteDecl = Infer<typeof RouteDecl>;

export const WorkKindDecl = t.object(
  {
    kind: t.string(),
    standing: t.optional(t.boolean()),
    trigger: t.optional(t.boolean()),
    timed: t.optional(t.boolean()),
    run_once: t.optional(t.boolean()),
  },
  open,
);
export type WorkKindDecl = Infer<typeof WorkKindDecl>;

export const ToolsetOperation = t.object(
  {
    name: t.string(),
    // The toolset version the operation joined in (1 when left out).
    since: t.optional(t.integer()),
  },
  open,
);
export type ToolsetOperation = Infer<typeof ToolsetOperation>;

// A toolset config grants as "@<name>": the operations (the extension's own
// or builtins) an agent playing one of its roles is given.
export const ToolsetDecl = t.object(
  {
    name: t.string(),
    summary: t.optional(t.string()),
    operations: t.array(ToolsetOperation),
  },
  open,
);
export type ToolsetDecl = Infer<typeof ToolsetDecl>;

export const TaskDecl = t.object(
  {
    name: t.string(),
    interval_seconds: t.integer(),
  },
  open,
);
export type TaskDecl = Infer<typeof TaskDecl>;

export const CheckDecl = t.object(
  {
    name: t.string(),
    // Deep checks run only on doctor --deep, so they may be slow or call out.
    deep: t.optional(t.boolean()),
  },
  open,
);
export type CheckDecl = Infer<typeof CheckDecl>;

export const Provides = t.object(
  {
    extensions: t.optional(t.array(ExtensionDecl)),
    families: t.optional(t.array(Family)),
    schemas: t.optional(t.array(SchemaDecl)),
    routes: t.optional(t.array(RouteDecl)),
    work_kinds: t.optional(t.array(WorkKindDecl)),
    operations: t.optional(t.array(OperationDecl)),
    toolsets: t.optional(t.array(ToolsetDecl)),
    tasks: t.optional(t.array(TaskDecl)),
    webhooks: t.optional(t.boolean()),
    webhook_max_bytes: t.optional(t.integer()),
    checks: t.optional(t.array(CheckDecl)),
  },
  open,
);
export type Provides = Infer<typeof Provides>;

export const Manifest = t.object(
  {
    protocol: t.array(t.integer()),
    name: t.string(),
    version: t.string(),
    title: t.optional(t.string()),
    summary: t.optional(t.string()),
    command: t.array(t.string()),
    kv_version: t.optional(t.integer()),
    permissions: t.optional(t.array(t.string())),
    provides: Provides,
  },
  open,
);
export type Manifest = Infer<typeof Manifest>;

export const OperationCase = t.object(
  {
    operation: t.string(),
    input: Payload,
    output: t.optional(Payload),
    // The refusal code expected instead of an output.
    error: t.optional(t.string()),
  },
  open,
);
export type OperationCase = Infer<typeof OperationCase>;

export const Fixture = t.object(
  {
    entries: t.optional(t.array(ConfigEntry)),
    secrets: t.optional(t.record(t.string())),
    scopes: t.optional(t.array(t.string())),
    operations: t.optional(t.array(OperationCase)),
  },
  open,
);
export type Fixture = Infer<typeof Fixture>;

/** A package's certification.json: the fixture `extension certify` runs. */
export const CertificationFile = t.object(
  {
    fixture: t.optional(Fixture),
    command: t.optional(t.array(t.string())),
  },
  open,
);
export type CertificationFile = Infer<typeof CertificationFile>;
