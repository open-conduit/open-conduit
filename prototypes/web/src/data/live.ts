// Read-only mapping from the management API (/v1) to the view models. It
// covers what the API exposes today; see INTEGRATION.md for what it can't
// derive yet (titles, check counts, asks).

import type { ExtAction, ExtPanel, Work, Turn, ConfigSnapshot, DaemonHealth, TranscriptItem, Disposition, WorkEvent, ScopeConfig, SourceSystem } from '../domain/types'
import { configFixture } from './fixtures/config'

const base = '/live/v1/'

async function get<T>(path: string): Promise<T> {
  const r = await fetch(base + path)
  const body = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(body?.error?.message ?? `GET /v1/${path} → ${r.status}`)
  return (body.data ?? body) as T
}

async function all<T>(path: string): Promise<T[]> {
  const out: T[] = []
  let cursor = ''
  for (let i = 0; i < 20; i++) {
    const page = await get<{ items: T[]; next_cursor?: string }>(path + (cursor ? (path.includes('?') ? '&' : '?') + 'cursor=' + encodeURIComponent(cursor) : ''))
    out.push(...page.items)
    if (!page.next_cursor) break
    cursor = page.next_cursor
  }
  return out
}

/* eslint-disable @typescript-eslint/no-explicit-any */
type Any = any

function sourceOf(kind: string): SourceSystem {
  if (kind.startsWith('github')) return 'github'
  if (kind.startsWith('slack')) return 'slack'
  if (kind.startsWith('schedule')) return 'schedule'
  if (kind.startsWith('introspection')) return 'introspection'
  return 'core'
}

function refOf(w: Any): string {
  const k = w.correlation_keys?.[0]?.external_id ?? ''
  const m = k.match(/(?:issue|pull)\/(\d+)/)
  if (m) return '#' + m[1]
  if (k.includes('thread')) return 'thread'
  return k.split('/').pop() || w.id.slice(5, 13)
}

function toTurn(r: Any): Turn {
  const d = r.outcome?.data?.data ?? {}
  return {
    id: r.id,
    attempt: r.attempt,
    state: r.state,
    binding: r.executor_binding,
    executor: r.executor ?? '',
    model: d.models?.[0],
    triggerEventId: r.trigger_id,
    generation: Number(r.config_generation ?? 0),
    createdAt: r.created_at,
    segments: [{ start: r.started_at ?? r.created_at, end: r.finished_at ?? (r.state === 'running' ? undefined : r.updated_at) }],
    outcome: r.outcome ? { kind: r.outcome.kind, summary: r.outcome.summary ?? '', status: d.status } : undefined,
    usage: r.usage ? { inputTokens: r.usage.input_tokens ?? 0, outputTokens: r.usage.output_tokens ?? 0, cachedTokens: r.usage.cached_tokens ?? 0, cost: r.usage.cost ?? 0 } : undefined,
    workspace: r.workspace ? { kind: r.workspace.kind ?? 'git', ref: r.workspace.ref, dir: r.workspace.dir } : undefined,
    inputs: r.inputs ?? 0,
    modelCalls: d.model_calls,
    toolCalls: d.tool_calls,
    deadline: r.deadline,
    error: r.state === 'failed' ? { code: 'failed', message: r.outcome?.summary ?? '' } : undefined,
    // artifacts let liveTranscript find the transcript
    ...({ artifacts: r.artifacts, transcriptArtifact: d.transcript } as object),
  }
}

/** The live API knows states and waits, not what a wait is for: derive coarsely. */
function disposition(w: Any, turns: Turn[], lastTrigger?: string): Disposition {
  const last = turns[turns.length - 1]
  const since = w.updated_at
  switch (w.state) {
    case 'running':
      return { group: 'running', kind: 'running', label: 'Running', detail: `Turn ${last?.attempt ?? 1}`, since }
    case 'ready':
      if (last?.state === 'failed') return { group: 'attention', kind: 'repair', label: 'Failed turn', detail: last.outcome?.summary ?? 'The latest turn failed', since }
      return { group: 'queued', kind: 'queued', label: 'Queued', detail: 'Ready for the next turn', since }
    case 'waiting': {
      const s = last?.outcome?.summary?.toLowerCase() ?? ''
      if (w.kind.startsWith('slack') || s.includes('thread')) return { group: 'waiting', kind: 'reply', label: 'Waiting for a reply', detail: last?.outcome?.summary ?? '', since }
      if (s.includes('check')) return { group: 'waiting', kind: 'ci', label: 'Waiting for CI', detail: last?.outcome?.summary ?? '', since }
      if (s.includes('review')) return { group: 'waiting', kind: 'review', label: 'Waiting for review', detail: last?.outcome?.summary ?? '', since }
      if (s.includes('merge')) return { group: 'waiting', kind: 'merge', label: 'Waiting for merge', detail: last?.outcome?.summary ?? '', since }
      return { group: 'waiting', kind: 'event', label: 'Waiting', detail: last?.outcome?.summary ?? lastTrigger ?? '', since }
    }
    case 'completed':
      return { group: 'closed', kind: 'completed', label: 'Completed', detail: last?.outcome?.summary ?? '', since }
    case 'failed':
      return { group: 'closed', kind: 'failed', label: 'Failed', detail: last?.outcome?.summary ?? '', since }
    default:
      return { group: 'closed', kind: 'cancelled', label: 'Cancelled', detail: '', since }
  }
}

export async function loadLive() {
  const [health, works, runs, scopes, events, config] = await Promise.all([
    get<Any>('health'),
    all<Any>('work'),
    all<Any>('runs'),
    all<Any>('scopes').catch(() => []),
    all<Any>('events').catch(() => []),
    get<Any>('config').catch(() => null),
  ])
  const cfgScope = (id: string) => (config?.scopes ?? []).find((x: Any) => x.id === id)
  const byWork = new Map<string, Turn[]>()
  for (const r of runs) {
    const list = byWork.get(r.work_id) ?? []
    list.push(toTurn(r))
    byWork.set(r.work_id, list)
  }
  const evByWork = new Map<string, WorkEvent[]>()
  for (const e of events) {
    const id = e.routed_work_id || e.work_id
    if (!id) continue
    const list = evByWork.get(id) ?? []
    list.push({ id: e.id, sequence: Number(e.sequence), type: e.type, source: e.source, occurredAt: e.occurred_at, summary: e.type, route: { effect: e.routed ? 'start' : 'unmatched', generation: 0, reason: e.disposition ?? '' } })
    evByWork.set(id, list)
  }
  const out: Work[] = works.map((w: Any) => {
    const turns = (byWork.get(w.id) ?? []).sort((a, b) => a.attempt - b.attempt)
    const last = turns[turns.length - 1]
    return {
      id: w.id,
      kind: w.kind,
      scopeId: w.scope_id,
      state: w.state,
      revision: Number(w.revision),
      title: last?.outcome?.summary?.split(/[.;:]/)[0] ?? w.kind,
      ref: refOf(w),
      source: sourceOf(w.kind),
      correlationKeys: (w.correlation_keys ?? []).map((k: Any) => ({ source: k.source, externalId: k.external_id })),
      createdAt: w.created_at,
      updatedAt: w.updated_at,
      binding: last?.binding ?? '',
      executor: last?.executor ?? '',
      summary: last?.outcome?.summary ?? '',
      status: last?.outcome?.status,
      disposition: disposition(w, turns),
      turns,
      events: (evByWork.get(w.id) ?? []).sort((a, b) => a.sequence - b.sequence),
      links: [],
    }
  })
  const cfg: ConfigSnapshot = {
    ...configFixture,
    generation: Number(health.config_generation ?? 0),
    scopes: scopes.length
      ? scopes.map((s: Any): ScopeConfig => {
          const fx = configFixture.scopes.find((f) => f.id === s.id)
          return {
            id: s.id,
            name: s.name,
            enabled: s.enabled,
            source: fx?.source ?? 'core',
            description: fx?.description ?? '',
            authorization: { connections: [], operations: [] },
            // Entry names and types come from GET /v1/config; their config is not exposed.
            extensions: (cfgScope(s.id)?.extensions ?? []).map((e: Any) => ({ name: e.name, type: e.type, ingress: e.ingress, connection: e.connection, summary: '', config: {} })),
            partial: true,
            routes: s.routes.map((r: Any) => ({ name: r.name, handler: r.handler, binding: r.binding, activePolicy: r.active_policy, matches: fx?.routes.find((x) => x.name === r.name)?.matches ?? '' })),
            bindings: s.bindings.map((b: Any) => {
              const cb = (cfgScope(s.id)?.bindings ?? []).find((x: Any) => x.name === b.name)
              return { name: b.name, executor: b.executor, requires: b.requires, operations: [], config: {}, resource: cb?.resource ? { provider: String(cb.resource), config: {} } : undefined }
            }),
          }
        })
      : structuredClone(configFixture.scopes),
  }
  // Extension-contributed UI: whatever providers offer now, rendered generically.
  const offers = async (path: string) => {
    const [a, p] = await Promise.all([get<Any>(path + '/actions').catch(() => ({ actions: [] })), get<Any>(path + '/panels').catch(() => ({ panels: [] }))])
    return { extActions: (a.actions ?? []) as ExtAction[], panels: toPanels(p.panels ?? []) }
  }
  await Promise.all([
    ...cfg.scopes.map(async (sc) => Object.assign(sc, await offers('scopes/' + encodeURIComponent(sc.id)))),
    ...out
      .filter((w) => w.disposition.group !== 'closed')
      .map(async (w) => {
        Object.assign(w, await offers('work/' + encodeURIComponent(w.id)))
        const lead = w.panels?.find((p) => p.sections.some((x) => x.actions?.length))
        if (w.state === 'waiting' && w.extActions?.length)
          w.disposition = { group: 'attention', kind: 'decision', label: lead?.title ?? w.extActions[0].label, detail: lead?.sections.find((x) => x.text)?.text ?? w.extActions[0].label, since: w.disposition.since }
      }),
  ])
  const h: DaemonHealth = {
    state: health.state === 'ready' ? 'ready' : 'starting',
    version: health.version,
    generation: Number(health.config_generation ?? 0),
    agentSlots: { used: out.filter((w) => w.state === 'running').length, total: 2 },
    jobSlots: { used: 0, total: 1 },
    tunnel: 'cloudflared',
    lastReconcile: '',
  }
  return { works: out, config: cfg, health: h }
}

function toPanels(ps: Any[]): ExtPanel[] {
  return ps.map((p) => ({ provider: p.provider, id: p.id, title: p.title, sections: (p.sections ?? []).map((x: Any) => ({ title: x.title, text: x.text, fields: x.fields, table: x.table, links: x.links, actions: x.actions })) }))
}

/** Parse a Claude Code stream-json transcript artifact into transcript items. */
export async function liveTranscript(_work: Work, turnId: string): Promise<TranscriptItem[]> {
  const run = await get<Any>('runs/' + encodeURIComponent(turnId))
  const id = run.outcome?.data?.data?.transcript ?? run.artifacts?.find((a: Any) => a.media_type?.includes('ndjson'))?.id
  if (!id) return []
  const r = await fetch(base + 'artifacts/' + encodeURIComponent(id) + '/content')
  if (!r.ok) return []
  const text = await r.text()
  const items: TranscriptItem[] = []
  const pending = new Map<string, Extract<TranscriptItem, { kind: 'tool' | 'effect' }>>()
  let n = 0
  const at = run.started_at ?? run.created_at
  for (const line of text.split('\n')) {
    if (!line.trim()) continue
    let o: Any
    try {
      o = JSON.parse(line)
    } catch {
      continue
    }
    const ts = o.timestamp ?? at
    if (o.type === 'system' && o.subtype === 'init') items.push({ id: 'l' + n++, kind: 'system', at: ts, text: `Session started · ${o.model ?? ''} · ${o.tools?.length ?? 0} tools`, detail: o.cwd })
    if (o.type === 'assistant') {
      for (const b of o.message?.content ?? []) {
        if (b.type === 'text' && b.text.trim()) items.push({ id: 'l' + n++, kind: 'agent', at: ts, text: b.text })
        if (b.type === 'tool_use') {
          const belt = b.name.startsWith('mcp__open-conduit__')
          const op = belt ? (b.input?.operation ?? b.name.replace('mcp__open-conduit__', '')) : b.name
          const label = belt ? op : b.input?.command ?? b.input?.file_path ?? b.input?.pattern ?? b.input?.description ?? b.name
          const item = belt
            ? { id: 'l' + n++, kind: 'effect' as const, at: ts, operation: op, label: String(label), input: JSON.stringify(b.input?.input ?? b.input, null, 2), ok: true, ms: 0 }
            : { id: 'l' + n++, kind: 'tool' as const, at: ts, tool: b.name, label: String(label).split('\n')[0].slice(0, 160), input: typeof b.input === 'object' ? JSON.stringify(b.input, null, 2) : String(b.input), ok: true, ms: 0 }
          pending.set(b.id, item)
          items.push(item)
        }
      }
    }
    if (o.type === 'user' && typeof o.message?.content === 'string' && !items.some((i) => i.kind === 'prompt')) {
      // The first plain user message is what the executor was given.
      const text = o.message.content as string
      items.push({ id: 'l' + n++, kind: 'prompt', at: ts, title: text.split('\n').find((l: string) => l.trim())?.slice(0, 100) ?? 'Prompt', text })
    }
    if (o.type === 'user' && Array.isArray(o.message?.content)) {
      for (const b of o.message?.content ?? []) {
        if (b.type !== 'tool_result') continue
        const it = pending.get(b.tool_use_id)
        if (!it) continue
        const content = Array.isArray(b.content) ? b.content.map((c: Any) => c.text ?? '').join('\n') : String(b.content ?? '')
        it.output = content
        it.ok = !b.is_error
      }
    }
    if (o.type === 'result' && run.outcome) items.push({ id: 'l' + n++, kind: 'outcome', at: ts, outcome: run.outcome.kind, summary: run.outcome.summary ?? '' })
  }
  return items
}
