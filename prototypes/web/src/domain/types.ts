// View models the presentation layer renders. Each mirrors a real Open Conduit
// record (see the comments) with the derivations an operator needs already
// made, so a live adapter can build the same shapes from /v1 responses.

/** domain.WorkState */
export type WorkState = 'ready' | 'running' | 'waiting' | 'completed' | 'failed' | 'cancelled'

/** domain.ExecutionState. A turn is one Execution (one attempt). */
export type TurnState = 'pending' | 'running' | 'parked' | 'completed' | 'failed' | 'waiting' | 'handoff' | 'cancelled'

/** domain.OutcomeKind */
export type OutcomeKind = 'completed' | 'failed' | 'waiting' | 'handoff'

export type SourceSystem = 'github' | 'slack' | 'schedule' | 'introspection' | 'core'

/**
 * What the Work is doing or waiting for, from the operator's point of view.
 * Derived from Work state, the active Wait's match, holds, asks and retry
 * state. `group` drives the state rail; `kind` the finer distinction.
 */
export type DispositionGroup = 'attention' | 'running' | 'queued' | 'waiting' | 'held' | 'closed'
export type DispositionKind =
  | 'decision' // an extension offers a person an action on waiting Work
  | 'repair' // retry exhausted, binding removed, operator-cancelled turn
  | 'running'
  | 'queued'
  | 'ci'
  | 'reply'
  | 'review'
  | 'merge'
  | 'time'
  | 'event'
  | 'held'
  | 'completed'
  | 'failed'
  | 'cancelled'

export interface Disposition {
  group: DispositionGroup
  kind: DispositionKind
  /** Short state word: "Waiting for CI" */
  label: string
  /** One plain sentence: "3 of 7 checks finished on PR #137" */
  detail: string
  /** When the current condition began (for "waiting 2h") */
  since: string
}

export interface ExtLink {
  kind: 'issue' | 'pr' | 'thread' | 'branch' | 'job' | 'doc'
  label: string
  url: string
}

export interface Usage {
  inputTokens: number
  outputTokens: number
  cachedTokens: number
  cost: number
}

/** A resumed parked Execution runs in segments; each is active time. */
export interface Segment {
  start: string
  end?: string
  /** Event that woke this segment (absent for the first) */
  wakeEventId?: string
}

export type TranscriptItem =
  | { id: string; kind: 'prompt'; at: string; title: string; text: string }
  | { id: string; kind: 'system'; at: string; text: string; detail?: string }
  | { id: string; kind: 'agent'; at: string; text: string }
  | { id: string; kind: 'human'; at: string; author: string; via: string; text: string; injected?: boolean }
  | {
      id: string
      kind: 'tool'
      at: string
      tool: string
      /** One-line summary of the input, e.g. the command */
      label: string
      input?: string
      output?: string
      ok: boolean
      ms: number
    }
  | {
      id: string
      kind: 'effect'
      at: string
      /** Toolbelt operation, e.g. github.pr.create */
      operation: string
      label: string
      input?: string
      output?: string
      ok: boolean
      ms: number
      link?: ExtLink
    }
  | { id: string; kind: 'orchestration'; at: string; text: string; eventId?: string; tone: 'park' | 'wake' | 'inject' | 'start' | 'cancel' }
  | { id: string; kind: 'outcome'; at: string; outcome: OutcomeKind; summary: string; status?: string }
  | { id: string; kind: 'error'; at: string; text: string; detail?: string }

export interface Turn {
  /** Execution ID: work/<id>/attempt/<n> */
  id: string
  attempt: number
  state: TurnState
  binding: string
  executor: string
  model?: string
  triggerEventId: string
  generation: number
  createdAt: string
  segments: Segment[]
  outcome?: { kind: OutcomeKind; summary: string; status?: string }
  usage?: Usage
  workspace?: { kind: string; ref: string; dir?: string }
  /** Inputs presented to the attempt (queued or injected Events) */
  inputs: number
  modelCalls?: number
  toolCalls?: number
  deadline?: string
  error?: { code: string; message: string }
}

/** A routing explanation: runtime RouteRecord / GET /v1/events/{id}/route */
export interface RouteDecision {
  route?: string
  binding?: string
  policy?: 'queue' | 'inject' | 'interrupt' | 'drop'
  /** How the Event affected Work */
  effect: 'start' | 'queue' | 'inject' | 'wake' | 'drop' | 'control' | 'unmatched' | 'terminal'
  generation: number
  reason: string
}

export interface WorkEvent {
  id: string
  sequence: number
  type: string
  source: string
  occurredAt: string
  /** Human-readable summary of the opaque payload, supplied by the source */
  summary: string
  actor?: string
  causationId?: string
  /** Attempt this Event triggered, woke or was injected into */
  attempt?: number
  route: RouteDecision
}

// ---- extension-contributed UI (extension/action.go, extension/panel.go) ----
// Extensions contribute actions and panels as data; views render them
// generically and never name an extension. Shapes follow the wire types in
// internal/management/actions.go and panels.go.

/** GET /v1/{work|scopes}/{id}/actions: one action available on the target now */
export interface ExtAction {
  provider: string
  id: string
  label: string
  /** When set, the action takes one line of input */
  prompt?: string
  /** With a prompt: the inputs accepted, offered as one button each */
  choices?: string[]
}

export interface ExtPanelSection {
  title?: string
  text?: string
  fields?: { name: string; value: string }[]
  table?: { columns: string[]; rows: string[][] }
  links?: { label: string; url: string }[]
  /** Buttons, resolved by the host to actions listed on the target now */
  actions?: ExtAction[]
}

/** GET /v1/{work|scopes}/{id}/panels */
export interface ExtPanel {
  provider: string
  id: string
  title: string
  sections: ExtPanelSection[]
}

export interface Hold {
  reason: string
  at: string
  by: string
}

/** runtime.RetryState */
export interface Retry {
  failureCount: number
  exhausted: boolean
  blocked: boolean
  blockedReason?: string
  eligibleAt?: string
}

export interface Work {
  id: string
  kind: string
  scopeId: string
  state: WorkState
  revision: number
  title: string
  /** Display reference: "#128", "thread", "nightly-backup" */
  ref: string
  source: SourceSystem
  correlationKeys: { source: string; externalId: string }[]
  createdAt: string
  updatedAt: string
  binding: string
  executor: string
  /** Who raised it, when the source says */
  requester?: string
  /** Latest outcome data status the source projects (building, review…) */
  status?: string
  summary: string
  disposition: Disposition
  turns: Turn[]
  events: WorkEvent[]
  links: ExtLink[]
  hold?: Hold
  /** Actions extensions offer on this Work now */
  extActions?: ExtAction[]
  /** Panels extensions show on this Work now */
  panels?: ExtPanel[]
  /** Event types the active Wait matches (domain.Wait.Match.Types) */
  waitTypes?: string[]
  retry?: Retry
  /** Binding the queued input is pinned to, when it differs from any enabled binding */
  pinnedBinding?: string
  /** Fixture-only marker: content constructed to show a state */
  fixture?: boolean
}

// ---- configuration (config.json, GET /v1/config, GET /v1/scopes) ----

export interface Connection {
  id: string
  type: string
  /** Names only. Reference text is redacted like the management API does. */
  secretNames: string[]
  status: 'ok' | 'warn' | 'fail'
  detail: string
}

export interface ExtensionEntry {
  name: string
  type: string
  /** Produces Events (GET /v1/config `ingress`); false for entries such as action providers */
  ingress?: boolean
  connection?: string
  summary: string
  config: Record<string, unknown>
}

export interface RouteConfig {
  name: string
  handler: string
  binding: string
  activePolicy: 'queue' | 'inject' | 'interrupt' | 'drop'
  /** What the handler matches, in words (from the extension's documentation) */
  matches: string
}

export interface BindingConfig {
  name: string
  executor: string
  connection?: string
  requires: { toolbelt?: string; inject?: boolean; resume?: boolean; interrupt?: boolean; workspace?: boolean }
  operations: string[]
  config: Record<string, unknown>
  resource?: { provider: string; config: Record<string, unknown> }
  timeoutSeconds?: number
}

export interface ScopeConfig {
  id: string
  name: string
  enabled: boolean
  source: SourceSystem
  description: string
  authorization: { connections: string[]; operations: string[] }
  extensions: ExtensionEntry[]
  routes: RouteConfig[]
  bindings: BindingConfig[]
  fixture?: boolean
  /** Live mode: the API exposed entry names and types but not their config or binding operations */
  partial?: boolean
  /** Scope-level extension actions and panels */
  extActions?: ExtAction[]
  panels?: ExtPanel[]
}

export interface Executor {
  name: string
  kind: 'agent' | 'command' | 'internal'
  detail: string
  capabilities: { inject: boolean; resume: boolean; interrupt: boolean; workspace: boolean }
  available: boolean
}

export interface Settings {
  concurrency: number
  jobConcurrency: number
  webhooks: { addr: string; tunnel: string }
}

export interface ConfigSnapshot {
  generation: number
  appliedAt: string
  fileMatchesGeneration: boolean
  path: string
  settings: Settings
  connections: Connection[]
  scopes: ScopeConfig[]
  executors: Executor[]
}

export interface DaemonHealth {
  state: 'ready' | 'starting' | 'unreachable'
  version: string
  generation: number
  agentSlots: { used: number; total: number }
  jobSlots: { used: number; total: number }
  tunnel: string
  lastReconcile: string
}

/** A simulated mutation's receipt, shown so the real call is never ambiguous */
export interface Receipt {
  id: string
  at: string
  method: 'POST'
  path: string
  body: Record<string, unknown>
  summary: string
  workId?: string
  simulated: true
}

/** Omit that distributes over a union, so each variant keeps its own fields. */
export type DistOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never
/** A transcript item before it is placed in time. */
export type TranscriptDraft = DistOmit<TranscriptItem, 'id' | 'at'>
