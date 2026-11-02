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
    revision: t.integer(),
    created_at: t.string(),
    updated_at: t.string(),
  },
  open,
);
export type Work = Infer<typeof Work>;

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
    public_url: t.optional(t.string()),
    dry_run: t.optional(t.boolean()),
    kv: KVState,
  },
  open,
);
export type ConfigureParams = Infer<typeof ConfigureParams>;

export const ConfigureResult = t.object(
  {
    kv_version: t.integer(),
  },
  open,
);
export type ConfigureResult = Infer<typeof ConfigureResult>;

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

export const StateParams = t.object(
  {
    id: t.optional(t.string()),
    scope_id: t.optional(t.string()),
    after: t.optional(t.integer()),
    limit: t.optional(t.integer()),
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
  },
  open,
);
export type ArtifactRef = Infer<typeof ArtifactRef>;

// The manifest (open-conduit-extension.json): the fields this SDK writes.

export const Family = t.object(
  {
    type: t.string(),
    // Whether a lost Event can be derived again, which tells the host how to recover.
    re_derivable: t.boolean(),
    loss: t.optional(t.string()),
  },
  open,
);
export type Family = Infer<typeof Family>;

export const ExtensionDecl = t.object(
  {
    type: t.string(),
  },
  open,
);
export type ExtensionDecl = Infer<typeof ExtensionDecl>;

export const OperationDecl = t.object(
  {
    name: t.string(),
    kind: t.enum(["handler", "wait", "emit"]),
    commit: t.optional(t.boolean()),
    guidance: t.optional(t.string()),
    input: t.optional(t.record(t.any())),
  },
  open,
);
export type OperationDecl = Infer<typeof OperationDecl>;

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
    operations: t.optional(t.array(OperationDecl)),
    tasks: t.optional(t.array(TaskDecl)),
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
