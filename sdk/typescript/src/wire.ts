// The wire types this SDK uses, as protocol/messages.go, protocol/wire.go
// and protocol/manifest.go encode them. Field names are the Go types' JSON
// names; protocol/typescript_parity_test.go checks every field here against
// them, so a renamed or removed Go field fails the Go tests.
//
// Times are RFC 3339 strings. Bytes ([]byte in Go: KV values, artifact
// chunks) are base64 strings on the wire; the Host client converts them.

/** A versioned payload: data is any JSON value, schema names its shape. */
export interface Payload {
  schema?: string;
  data?: unknown;
}

export interface Key {
  source: string;
  external_id: string;
}

export interface Event {
  id?: string;
  type: string;
  source?: string;
  delivery_id?: string;
  occurred_at?: string;
  received_at?: string;
  correlation_key?: Key;
  causation_id?: string;
  work_id?: string;
  execution_id?: string;
  data?: Payload;
}

export interface AcceptedEvent {
  event: Event;
  sequence: number;
  routed: boolean;
  routed_work_id?: string;
  disposition?: string;
}

export interface DisplayLink {
  label: string;
  url: string;
}

export interface Display {
  title?: string;
  ref?: string;
  requester?: string;
  links?: DisplayLink[];
}

export interface Work {
  id: string;
  kind: string;
  scope_id: string;
  state: string;
  correlation_keys: Key[] | null;
  context: Payload;
  parent_id?: string;
  display?: Display;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface LeaseRef {
  execution_id: string;
  owner: string;
  epoch: number;
}

/** The authority an operation call runs under: its scope, Work and grant. */
export interface Authority {
  scope_id: string;
  work_id: string;
  execution_id: string;
  lease: LeaseRef;
  expires_at: string;
  operations?: string[];
  event_types?: string[];
  trigger_id?: string;
  /** The connection the execution's binding names, or empty. */
  connection?: string;
}

// Lifecycle.

export interface HostInfo {
  name: string;
  version: string;
}

export interface InitializeParams {
  protocol_versions: number[];
  host: HostInfo;
  state_dir: string;
}

export interface InitializeResult {
  protocol_version: number;
  manifest: Manifest;
}

export interface ConnectionEntry {
  id: string;
  type: string;
  config?: unknown;
  secrets?: string[];
}

/** One configured scope entry of the extension's own types. */
export interface ConfigEntry {
  scope_id: string;
  type: string;
  config?: unknown;
  secrets?: string[];
  name?: string;
  connection?: ConnectionEntry;
  binding_connections?: ConnectionEntry[];
}

export interface KVState {
  stored: number;
  declared: number;
}

export interface ConfigureParams {
  generation: string;
  entries?: ConfigEntry[];
  public_url?: string;
  dry_run?: boolean;
  kv: KVState;
}

export interface ConfigureResult {
  kv_version: number;
}

export interface ValidateParams {
  kind: string;
  type: string;
  config?: unknown;
  secret_names?: string[];
}

// Operations, tasks and checks.

export interface OperationParams {
  operation: string;
  authority: Authority;
  input: Payload;
  request_id?: string;
}

export interface OperationResult {
  output: Payload;
}

export interface TaskRunParams {
  task: string;
}

export interface CheckRunParams {
  check: string;
  deep?: boolean;
  config: unknown;
}

export interface CheckRunResult {
  ok: boolean;
  detail?: string;
  status?: string;
  code?: string;
  items?: string[];
}

// Extension-to-host calls.

export interface KVGetParams {
  namespace: string;
  key: string;
}

export interface KVEntryWire {
  key: string;
  /** base64 */
  value: string | null;
  version: string;
}

export interface KVGetResult {
  found: boolean;
  entry: KVEntryWire;
}

export interface KVListParams {
  namespace: string;
  prefix?: string;
}

export interface KVListResult {
  entries: KVEntryWire[] | null;
}

export interface KVCheck {
  namespace: string;
  key: string;
  version?: string;
  absent?: boolean;
}

export interface KVPutWire {
  namespace: string;
  key: string;
  /** base64 */
  value: string;
}

export interface KVDelete {
  namespace: string;
  key: string;
}

export interface KVCommitParams {
  checks?: KVCheck[];
  puts?: KVPutWire[];
  deletes?: KVDelete[];
}

export interface StateParams {
  id?: string;
  scope_id?: string;
  after?: number;
  limit?: number;
}

export interface EventAcceptParams {
  scope_id: string;
  parent: string;
  event: Event;
  checks?: KVCheck[];
  records?: KVPutWire[];
  deletes?: KVDelete[];
}

export interface EventAcceptResult {
  event: AcceptedEvent;
  added: boolean;
}

export interface SecretResolveParams {
  scope_id: string;
  connection?: string;
  names: string[];
}

export interface SecretResolveResult {
  values: Record<string, string> | null;
}

export interface ArtifactCreateParams {
  media_type: string;
}

export interface ArtifactCreateResult {
  writer: string;
}

export interface ArtifactWriteParams {
  writer: string;
  /** base64 */
  chunk: string;
}

export interface ArtifactTarget {
  writer: string;
}

export interface ArtifactRef {
  id: string;
  size: number;
  media_type?: string;
}

// The manifest (open-conduit-extension.json): the fields this SDK writes.

export interface Family {
  type: string;
  re_derivable: boolean;
  loss?: string;
}

export interface ExtensionDecl {
  type: string;
}

export interface OperationDecl {
  name: string;
  kind: "handler" | "wait" | "emit";
  commit?: boolean;
  guidance?: string;
  input?: Record<string, unknown>;
}

export interface TaskDecl {
  name: string;
  interval_seconds: number;
}

export interface CheckDecl {
  name: string;
  deep?: boolean;
}

export interface Provides {
  extensions?: ExtensionDecl[];
  families?: Family[];
  operations?: OperationDecl[];
  tasks?: TaskDecl[];
  checks?: CheckDecl[];
}

export interface Manifest {
  protocol: number[];
  name: string;
  version: string;
  summary?: string;
  command: string[];
  kv_version?: number;
  permissions?: string[];
  provides: Provides;
}

/** A certification fixture (protocol/certification.go), inline. */
export interface CertificationFile {
  fixture?: Fixture;
  command?: string[];
}

export interface Fixture {
  entries?: ConfigEntry[];
  secrets?: Record<string, string>;
  scopes?: string[];
  operations?: OperationCase[];
}

export interface OperationCase {
  operation: string;
  input: Payload;
  output?: Payload;
  error?: string;
}
