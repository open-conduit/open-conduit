// Application state and the simulated action model. Every mutation mirrors
// a real management API operation (method, path, body, Idempotency-Key) and
// records a receipt, but nothing is sent: in the prototype the transition is
// played out locally so the consequences can be seen.

import { useRef, useSyncExternalStore } from 'react'
import type { Work, ConfigSnapshot, DaemonHealth, ExtAction, TranscriptItem, TranscriptDraft, Receipt, Disposition, Turn, WorkEvent } from '../domain/types'
import { fixtureAdapter, liveAdapter, type ConduitAdapter, type Condition } from './adapter'
import { liveTail134, retryTail117, firstTurnTail } from './fixtures/scenarios'
import { simFor, type ActionResult } from './actionSim'

export interface Toast {
  id: string
  text: string
  receipt?: Receipt
  tone: 'ok' | 'info' | 'warn'
}

export interface State {
  status: 'loading' | 'ready' | 'error'
  error?: string
  source: 'fixture' | 'live'
  condition: Condition
  works: Work[]
  config?: ConfigSnapshot
  health?: DaemonHealth
  transcripts: Record<string, TranscriptItem[] | 'loading'>
  receipts: Receipt[]
  toasts: Toast[]
  theme: 'light' | 'dark'
  /** Work IDs where a simulated transition is playing out */
  animating: Record<string, boolean>
}

const params = new URLSearchParams(location.search)
const savedTheme = (() => {
  try {
    return localStorage.getItem('oc-theme') as State['theme'] | null
  } catch {
    return null
  }
})()

let state: State = {
  status: 'loading',
  source: params.get('source') === 'live' ? 'live' : 'fixture',
  condition: (params.get('condition') as Condition) ?? 'normal',
  works: [],
  transcripts: {},
  receipts: [],
  toasts: [],
  theme: savedTheme ?? (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'),
  animating: {},
}
const listeners = new Set<() => void>()
function set(patch: Partial<State> | ((s: State) => Partial<State>)) {
  state = { ...state, ...(typeof patch === 'function' ? patch(state) : patch) }
  listeners.forEach((l) => l())
}
export const getState = () => state
const subscribe = (l: () => void) => {
  listeners.add(l)
  return () => void listeners.delete(l)
}
/** Selectors may derive new arrays; the result is cached per state snapshot. */
export function useStore<T>(sel: (s: State) => T): T {
  const cache = useRef<{ s: State; v: T } | null>(null)
  return useSyncExternalStore(subscribe, () => {
    if (cache.current?.s !== state) {
      const v = sel(state)
      const prev = cache.current?.v
      cache.current = { s: state, v: shallowEqual(prev, v) ? (prev as T) : v }
    }
    return cache.current.v
  })
}
function shallowEqual(a: unknown, b: unknown) {
  if (Object.is(a, b)) return true
  if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false
  return a.every((x, i) => Object.is(x, b[i]))
}

let adapter: ConduitAdapter = state.source === 'live' ? liveAdapter() : fixtureAdapter(state.condition)

export async function load() {
  set({ status: 'loading', error: undefined, transcripts: {} })
  try {
    const snap = await adapter.load()
    set({ status: 'ready', works: snap.works, config: snap.config, health: snap.health })
    if (state.source === 'fixture' && state.condition === 'normal') startLiveTail()
  } catch (e) {
    set({ status: 'error', error: (e as Error).message })
  }
}

export function setSource(source: State['source'], condition: Condition = 'normal') {
  stopTimers()
  adapter = source === 'live' ? liveAdapter() : fixtureAdapter(condition)
  set({ source, condition, receipts: [], animating: {} })
  const u = new URL(location.href)
  if (source === 'live') u.searchParams.set('source', 'live')
  else u.searchParams.delete('source')
  if (condition !== 'normal') u.searchParams.set('condition', condition)
  else u.searchParams.delete('condition')
  history.replaceState(null, '', u)
  void load()
}

export function setTheme(theme: State['theme']) {
  try {
    localStorage.setItem('oc-theme', theme)
  } catch {
    /* per-viewer convenience only */
  }
  set({ theme })
}

export async function ensureTranscript(work: Work, turnId: string) {
  if (state.transcripts[turnId]) return
  set((s) => ({ transcripts: { ...s.transcripts, [turnId]: 'loading' } }))
  const items = await adapter.transcript(work, turnId).catch(() => [])
  set((s) => ({ transcripts: { ...s.transcripts, [turnId]: items } }))
}

// ---- helpers ----

let n = 0
const uid = (p: string) => `${p}-${Date.now().toString(36)}-${++n}`
const now = () => new Date().toISOString()

export function toast(text: string, tone: Toast['tone'] = 'ok', receipt?: Receipt) {
  const t: Toast = { id: uid('toast'), text, tone, receipt }
  set((s) => ({ toasts: [...s.toasts.slice(-3), t] }))
  setTimeout(() => dismissToast(t.id), receipt ? 9000 : 5000)
}
export function dismissToast(id: string) {
  set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }))
}

function patchWork(id: string, f: (w: Work) => Partial<Work>) {
  set((s) => ({ works: s.works.map((w) => (w.id === id ? { ...w, ...f(w), updatedAt: now() } : w)) }))
}
function patchTurn(workId: string, turnId: string, f: (t: Turn) => Partial<Turn>) {
  patchWork(workId, (w) => ({ turns: w.turns.map((t) => (t.id === turnId ? { ...t, ...f(t) } : t)) }))
}
function appendItem(turnId: string, item: TranscriptDraft) {
  const full = { ...item, id: uid('i'), at: now() } as TranscriptItem
  set((s) => {
    const cur = s.transcripts[turnId]
    return { transcripts: { ...s.transcripts, [turnId]: [...(Array.isArray(cur) ? cur : []), full] } }
  })
}
function addEvent(workId: string, e: Omit<WorkEvent, 'sequence' | 'id' | 'occurredAt'>) {
  const max = Math.max(4100, ...state.works.flatMap((w) => w.events.map((x) => x.sequence)))
  const full: WorkEvent = { ...e, id: uid('evt'), sequence: max + 1, occurredAt: now() }
  patchWork(workId, (w) => ({ events: [...w.events, full] }))
  return full
}
const work = (id: string) => state.works.find((w) => w.id === id)!
const enc = (id: string) => id.split('/').map(encodeURIComponent).join('%2F')

function receipt(path: string, body: Record<string, unknown>, summary: string, workId?: string): Receipt {
  const r: Receipt = { id: uid('rcpt'), at: now(), method: 'POST', path, body, summary, workId, simulated: true }
  set((s) => ({ receipts: [r, ...s.receipts] }))
  return r
}

let waitingForSlot: { workId: string; start: () => void }[] = []
const timers = new Map<ReturnType<typeof setTimeout>, string | undefined>()
function later(ms: number, f: () => void, workId?: string) {
  const t = setTimeout(() => {
    timers.delete(t)
    f()
  }, ms)
  timers.set(t, workId)
}
function stopTimers(workId?: string) {
  for (const [t, w] of timers) {
    if (workId === undefined || w === workId) {
      clearTimeout(t)
      timers.delete(t)
    }
  }
  if (workId === undefined) waitingForSlot = []
}

/** Plays items into a turn one by one, then calls done. */
function play(workId: string, turnId: string, items: TranscriptDraft[], every: number, done: () => void) {
  set((s) => ({ animating: { ...s.animating, [workId]: true } }))
  items.forEach((it, i) => later(every * (i + 1), () => appendItem(turnId, it), workId))
  later(every * (items.length + 1), () => {
    set((s) => ({ animating: { ...s.animating, [workId]: false } }))
    done()
  }, workId)
}

// ---- agent slots: like the scheduler, never exceed settings.concurrency ----

const agentRunning = () => state.works.filter((w) => w.turns.some((t) => t.state === 'running' && t.executor === 'claude-code')).length
const slotTotal = () => state.health?.agentSlots.total ?? 2
const slotsBusy = () => `${agentRunning()} of ${slotTotal()}`

/** Start now if a slot is free; otherwise wait in line. Operator actions go first. */
function requestSlot(workId: string, start: () => void, first = false) {
  if (agentRunning() < slotTotal()) return start()
  if (first) waitingForSlot.unshift({ workId, start })
  else waitingForSlot.push({ workId, start })
}
function releaseSlot() {
  later(1500, () => {
    while (waitingForSlot.length && agentRunning() < slotTotal()) waitingForSlot.shift()!.start()
  })
}

// ---- background: the running turn keeps working ----

function startLiveTail() {
  const w = state.works.find((x) => x.ref === '#134' && x.scopeId === 'open-conduit')
  if (!w) return
  const t = w.turns[w.turns.length - 1]
  later(4000, () =>
    void ensureTranscript(w, t.id).then(() =>
      play(w.id, t.id, liveTail134, 7000, () => {
        const outcome = liveTail134[liveTail134.length - 1] as Extract<TranscriptItem, { kind: 'outcome' }>
        patchTurn(w.id, t.id, (tt) => ({ state: 'handoff', outcome: { kind: 'handoff', summary: outcome.summary, status: 'building' }, segments: tt.segments.map((s, i) => (i === tt.segments.length - 1 ? { ...s, end: now() } : s)) }))
        patchWork(w.id, (ww) => ({ state: 'waiting', revision: ww.revision + 2, summary: outcome.summary, disposition: { group: 'waiting', kind: 'ci', label: 'Waiting for CI', detail: '0 of 7 checks finished on PR #139 at 9d04e7b', since: now() } }))
        toast('#134 finished turn 2 and is waiting for CI on PR #139', 'info')
        releaseSlot()
      }),
    ),
  )
  for (const ref of ['#140', '#141']) {
    const q = state.works.find((x) => x.ref === ref && x.disposition.group === 'queued')
    if (q) waitingForSlot.push({ workId: q.id, start: () => startQueued(ref) })
  }
}

/** A queued Work takes the freed agent slot and starts its first turn. */
function startQueued(ref: string) {
  const w = state.works.find((x) => x.ref === ref && x.disposition.group === 'queued')
  if (!w) return
  const attempt = w.turns.length + 1
  const id = `${w.id}/attempt/${attempt}`
  const t: Turn = { id, attempt, state: 'running', binding: 'agent', executor: 'claude-code', model: 'claude-opus-5-5', triggerEventId: w.events[0]?.id ?? '', generation: 3, createdAt: now(), segments: [{ start: now() }], inputs: 1, workspace: { kind: 'git', ref: `agent/issue-${ref.slice(1)}` } }
  set((s) => ({ transcripts: { ...s.transcripts, [id]: [] } }))
  patchWork(w.id, (ww) => ({ state: 'running', revision: ww.revision + 1, turns: [...ww.turns, t], disposition: { group: 'running', kind: 'running', label: 'Running', detail: `Turn ${attempt} · took a freed agent slot`, since: now() } }))
  play(w.id, id, firstTurnTail(w, attempt), 5000, () => {
    patchTurn(w.id, id, (tt) => ({ state: 'handoff', outcome: { kind: 'handoff', summary: 'Opened a PR: config-check depends on config-deps; waiting on checks.', status: 'building' }, segments: tt.segments.map((s) => ({ ...s, end: s.end ?? now() })) }))
    patchWork(w.id, (ww) => ({ state: 'waiting', revision: ww.revision + 2, summary: 'Opened a PR; waiting on checks.', disposition: { group: 'waiting', kind: 'ci', label: 'Waiting for CI', detail: '0 of 7 checks finished on the new PR', since: now() } }))
    releaseSlot()
  })
}

// ---- eligibility (mirrors internal/runtime/operator.go & manual.go) ----

export type ActionId = 'retry' | 'hold' | 'release' | 'wake' | 'rebind' | 'cancel-turn' | 'cancel-work' | 'attach'

export interface ActionSpec {
  id: ActionId
  label: string
  enabled: boolean
  /** Why it is unavailable, in the runtime's terms */
  why?: string
  destructive?: boolean
}

const terminal = (w: Work) => w.state === 'completed' || w.state === 'failed' || w.state === 'cancelled'
export const liveTurn = (w: Work) => w.turns.find((t) => t.state === 'running' || t.state === 'pending')
export const parkedTurn = (w: Work) => w.turns.find((t) => t.state === 'parked')

export function actionsFor(w: Work): ActionSpec[] {
  const live = liveTurn(w)
  const parked = parkedTurn(w)
  const out: ActionSpec[] = []
  const retryable = w.state === 'ready' && !live && !parked && (w.retry?.exhausted || w.retry?.blocked || w.disposition.kind === 'repair')
  out.push({ id: 'retry', label: 'Run again', enabled: !!retryable && !w.pinnedBinding, why: terminal(w) ? 'Finished Work cannot be retried' : live ? 'A turn is running' : w.pinnedBinding ? 'Rebind first: the pinned binding no longer exists' : !retryable ? 'Only Work that failed or was cancelled by an operator can be retried' : undefined })
  if (w.pinnedBinding) out.push({ id: 'rebind', label: 'Rebind queued input', enabled: w.state === 'ready' && !live && !parked })
  if (w.hold) out.push({ id: 'release', label: 'Release hold', enabled: true })
  else out.push({ id: 'hold', label: 'Hold', enabled: !terminal(w) && !live, why: terminal(w) ? 'Finished Work cannot be held' : live ? 'Holds apply between turns; this one is running. Cancel the turn or wait.' : undefined })
  out.push({ id: 'wake', label: 'Send a wake event…', enabled: w.state === 'waiting' && !w.hold, why: w.state !== 'waiting' ? 'Only waiting Work has a wait to wake' : undefined })
  out.push({ id: 'attach', label: 'Attach to session', enabled: !terminal(w) && w.executor === 'claude-code' && w.turns.length > 0, why: w.executor !== 'claude-code' ? `${w.executor} has no session to attach to` : undefined })
  out.push({ id: 'cancel-turn', label: 'Cancel this turn', enabled: !!(live || parked), destructive: true, why: !(live || parked) ? 'No turn is running or parked' : undefined })
  out.push({ id: 'cancel-work', label: 'Cancel Work', enabled: !terminal(w), destructive: true, why: terminal(w) ? 'Already finished' : undefined })
  return out
}

// ---- actions ----

export type ActionTarget = { kind: 'work'; id: string } | { kind: 'scope'; id: string }

/**
 * Invoke an extension's action (POST …/actions/{provider}/{action}). The
 * provider decides what it means; core records the Event it produces and,
 * when that Event matches the Work's Wait, resumes the parked attempt.
 */
export function invokeExtAction(target: ActionTarget, action: ExtAction, input: string) {
  const w = target.kind === 'work' ? work(target.id) : undefined
  const path = `/v1/${target.kind === 'work' ? 'work' : 'scopes'}/${enc(target.id)}/actions/${encodeURIComponent(action.provider)}/${action.id}`
  const body: Record<string, unknown> = { ...(w ? { expected_revision: String(w.revision) } : {}), ...(input ? { input } : {}) }
  const sim = simFor(action.provider, action.id)
  const result: ActionResult = sim ? sim(input, w) : { message: 'Accepted.', event: { type: `${action.provider.split('/')[0]}.${action.id}`, summary: `${action.label}${input ? `: ${input}` : ''}` } }
  const r = receipt(path, body, `${action.label}${input ? ` · “${input}”` : ''}`, w?.id)
  if (!w) {
    toast(result.message, 'ok', r)
    return
  }
  const parked = parkedTurn(w)
  const resumes = result.resumes && parked
  const e = addEvent(w.id, {
    type: result.event.type,
    source: 'core.action',
    summary: result.event.summary,
    actor: 'operator:local',
    attempt: resumes ? parked.attempt : undefined,
    route: resumes ? { effect: 'wake', generation: state.config?.generation ?? 3, reason: `matched the active Wait; resumes attempt ${parked.attempt}` } : { effect: 'control', generation: state.config?.generation ?? 3, reason: 'action Event accepted under core.action' },
  })
  // The provider no longer offers what was just done.
  patchWork(w.id, (ww) => ({
    revision: ww.revision + 1,
    extActions: (ww.extActions ?? []).filter((x) => !(x.provider === action.provider && x.id === action.id)),
    panels: (ww.panels ?? []).filter((p) => p.provider !== action.provider),
  }))
  if (!resumes) {
    toast(result.message, 'ok', r)
    return
  }
  patchWork(w.id, () => ({ state: 'ready', disposition: { group: 'queued', kind: 'queued', label: 'Queued', detail: `Woken by ${result.event.type} · turn ${parked.attempt} resumes when an agent slot is free (${slotsBusy()} in use)`, since: now() } }))
  toast(agentRunning() < slotTotal() ? `${result.message} Turn ${parked.attempt} resumes.` : `${result.message} Turn ${parked.attempt} resumes when an agent slot frees (${slotsBusy()} in use).`, 'ok', r)
  requestSlot(w.id, () => resumeParked(w.id, parked.id, e.id, result.resumes!.tail, result.resumes!.then), true)
}

/** A Wait was woken: a resume-capable executor continues the parked attempt. */
function resumeParked(workId: string, turnId: string, wakeId: string, tail: TranscriptDraft[], then: Omit<Disposition, 'since'>) {
  const w = work(workId)
  const t0 = w.turns.find((t) => t.id === turnId)!
  patchWork(workId, () => ({ state: 'running', disposition: { group: 'running', kind: 'running', label: 'Running', detail: `Turn ${t0.attempt} resumed`, since: now() } }))
  patchTurn(workId, turnId, (t) => ({ state: 'running', segments: [...t.segments, { start: now(), wakeEventId: wakeId }] }))
  void ensureTranscript(w, turnId).then(() => {
    play(workId, turnId, tail, 2600, () => {
      const outcome = [...tail].reverse().find((x) => x.kind === 'outcome') as Extract<TranscriptItem, { kind: 'outcome' }> | undefined
      patchTurn(workId, turnId, (t) => ({ state: 'parked', outcome: outcome ? { kind: outcome.outcome, summary: outcome.summary } : t.outcome, segments: t.segments.map((s, i) => (i === t.segments.length - 1 ? { ...s, end: now() } : s)) }))
      patchWork(workId, (ww) => ({ state: 'waiting', revision: ww.revision + 2, summary: outcome?.summary ?? ww.summary, disposition: { ...then, since: now() } }))
      releaseSlot()
      toast(`${w.ref === 'thread' ? w.title : w.ref}: ${then.label.toLowerCase()}`, 'info')
    })
  })
}

export function retry(workId: string) {
  const w = work(workId)
  const r = receipt(`/v1/work/${enc(workId)}/retry`, { expected_revision: String(w.revision) }, 'Accepted core.manual.retry', workId)
  addEvent(workId, { type: 'core.manual.retry', source: 'core.manual', summary: 'operator:local retried the Work', actor: 'operator:local', route: { effect: 'control', generation: 3, reason: 'retry policy re-queued the Work; failure count reset' } })
  const attempt = w.turns.length + 1
  const id = `${w.id}/attempt/${attempt}`
  const t: Turn = { id, attempt, state: 'pending', binding: w.binding, executor: w.executor, model: 'claude-opus-5-5', triggerEventId: w.events[w.events.length - 1]?.id ?? '', generation: 3, createdAt: now(), segments: [{ start: now() }], inputs: 1, workspace: w.turns[w.turns.length - 1]?.workspace }
  set((s) => ({ transcripts: { ...s.transcripts, [id]: [] } }))
  patchWork(workId, (ww) => ({ retry: undefined, revision: ww.revision + 1, turns: [...ww.turns, t], disposition: { group: 'queued', kind: 'queued', label: 'Queued', detail: `Retry accepted · attempt ${attempt} pending`, since: now() } }))
  toast(agentRunning() < slotTotal() ? `Retry accepted. Attempt ${attempt} is starting.` : `Retry accepted. Attempt ${attempt} starts when an agent slot frees (${slotsBusy()} in use).`, 'ok', r)
  if (agentRunning() >= slotTotal()) patchWork(workId, (ww) => ({ disposition: { ...ww.disposition, detail: `Retry accepted · attempt ${attempt} waits for an agent slot (${slotsBusy()} in use)` } }))
  requestSlot(workId, () => later(1200, () => {
    patchTurn(workId, id, () => ({ state: 'running' }))
    patchWork(workId, () => ({ state: 'running', disposition: { group: 'running', kind: 'running', label: 'Running', detail: `Turn ${attempt} · workspace setup succeeded`, since: now() } }))
    const tail = w.ref === '#117' ? retryTail117 : retryTail117.slice(0, 2).concat(retryTail117.slice(-1))
    play(workId, id, tail, 2400, () => {
      const outcome = tail[tail.length - 1] as Extract<TranscriptItem, { kind: 'outcome' }>
      patchTurn(workId, id, (tt) => ({ state: 'handoff', outcome: { kind: 'handoff', summary: outcome.summary, status: 'building' }, usage: { inputTokens: 61, outputTokens: 9200, cachedTokens: 1_480_000, cost: 0.91 }, segments: tt.segments.map((s) => ({ ...s, end: s.end ?? now() })) }))
      patchWork(workId, (ww) => ({ state: 'waiting', revision: ww.revision + 2, summary: outcome.summary, disposition: { group: 'waiting', kind: 'ci', label: 'Waiting for CI', detail: `0 of 7 checks finished on PR #118`, since: now() } }))
      toast(`${w.ref} recovered: turn ${attempt} pushed and is waiting for CI`, 'info')
      releaseSlot()
    })
  }), true)
}

const prior: Record<string, Disposition> = {}

export function hold(workId: string, reason: string) {
  const w = work(workId)
  const r = receipt(`/v1/work/${enc(workId)}/hold`, { expected_revision: String(w.revision), reason }, 'Accepted core.manual.hold', workId)
  prior[workId] = w.disposition
  addEvent(workId, { type: 'core.manual.hold', source: 'core.manual', summary: `Held by operator:local: “${reason}”`, actor: 'operator:local', route: { effect: 'control', generation: 3, reason: 'scheduling hold placed; input keeps queueing' } })
  patchWork(workId, (ww) => ({ hold: { reason, at: now(), by: 'operator:local' }, revision: ww.revision + 1, disposition: { group: 'held', kind: 'held', label: 'Held', detail: `“${reason}” · operator:local`, since: now() } }))
  toast(`${w.ref} held. No turn will start or resume until you release it.`, 'ok', r)
}

export function release(workId: string) {
  const w = work(workId)
  const r = receipt(`/v1/work/${enc(workId)}/release`, { expected_revision: String(w.revision) }, 'Accepted core.manual.release', workId)
  addEvent(workId, { type: 'core.manual.release', source: 'core.manual', summary: 'Released by operator:local', actor: 'operator:local', route: { effect: 'control', generation: 3, reason: 'hold removed; queued input starts normally' } })
  const back = prior[workId] ?? (w.turns.length === 0 ? { group: 'queued', kind: 'queued', label: 'Queued', detail: `Waiting for an agent slot · ${slotsBusy()} in use`, since: now() } : w.disposition)
  patchWork(workId, (ww) => ({ hold: undefined, revision: ww.revision + 1, disposition: { ...back, since: now() } as Disposition }))
  toast(`${w.ref} released.`, 'ok', r)
}

export function wake(workId: string, type: string) {
  const w = work(workId)
  const r = receipt(`/v1/work/${enc(workId)}/wake`, { expected_revision: String(w.revision), type, data: {} }, `Accepted ${type}`, workId)
  addEvent(workId, { type, source: 'core.manual', summary: `Wake event ${type} from operator:local`, actor: 'operator:local', route: { effect: 'wake', generation: 3, reason: 'matched the active Wait; Work is ready' } })
  patchWork(workId, (ww) => ({ state: 'ready', revision: ww.revision + 1, disposition: { group: 'queued', kind: 'queued', label: 'Queued', detail: `Woken by ${type} · waiting for an agent slot`, since: now() } }))
  toast(`The event woke ${w.ref}'s wait. Its next turn starts when a slot frees.`, 'ok', r)
}

export function rebind(workId: string, binding: string, reason: string) {
  const w = work(workId)
  const r = receipt(`/v1/work/${enc(workId)}/rebind`, { expected_revision: String(w.revision), binding, reason }, `Accepted core.manual.rebind → ${binding}`, workId)
  addEvent(workId, { type: 'core.manual.rebind', source: 'core.manual', summary: `Rebound from ${w.pinnedBinding} to ${binding}`, actor: 'operator:local', route: { effect: 'control', generation: 3, reason: `queued input now targets ${binding} in generation 3` } })
  patchWork(workId, (ww) => ({ pinnedBinding: undefined, binding, revision: ww.revision + 1, disposition: { group: 'queued', kind: 'queued', label: 'Queued', detail: `Rebound to ${binding} · waiting for an agent slot`, since: now() } }))
  toast(`${w.ref} rebound to ${binding}.`, 'ok', r)
}

export function cancelTurn(workId: string) {
  const w = work(workId)
  const t = liveTurn(w) ?? parkedTurn(w)
  if (!t) return
  const r = receipt(`/v1/executions/${enc(t.id)}/cancel`, { expected_revision: String(w.revision) }, `Cancelled attempt ${t.attempt}`, workId)
  stopTimersFor(workId)
  appendItem(t.id, { kind: 'orchestration', tone: 'cancel', text: 'Cancelled by operator:local · lease fenced · process stop dispatched' } as TranscriptDraft)
  patchTurn(workId, t.id, (tt) => ({ state: 'cancelled', outcome: { kind: 'failed', summary: 'Cancelled by the operator.' }, segments: tt.segments.map((s) => ({ ...s, end: s.end ?? now() })) }))
  patchWork(workId, (ww) => ({ state: 'ready', revision: ww.revision + 1, disposition: { group: 'attention', kind: 'repair', label: 'Turn cancelled', detail: 'Scheduling is held until you run it again', since: now() } }))
  if (t.state === 'running') releaseSlot()
  waitingForSlot = waitingForSlot.filter((x) => x.workId !== workId)
  toast(`Turn ${t.attempt} of ${w.ref} cancelled. It will not retry on its own.`, 'warn', r)
}

export function cancelWork(workId: string, reason: string) {
  const w = work(workId)
  const t = liveTurn(w) ?? parkedTurn(w)
  const r = receipt(`/v1/work/${enc(workId)}/cancel`, { expected_revision: String(w.revision), reason }, 'Accepted core.manual.cancel', workId)
  stopTimersFor(workId)
  if (t) {
    appendItem(t.id, { kind: 'orchestration', tone: 'cancel', text: 'Work cancelled by operator:local · attempt fenced' } as TranscriptDraft)
    patchTurn(workId, t.id, (tt) => ({ state: 'cancelled', segments: tt.segments.map((s) => ({ ...s, end: s.end ?? now() })) }))
    if (t.state === 'running') releaseSlot()
  }
  waitingForSlot = waitingForSlot.filter((x) => x.workId !== workId)
  addEvent(workId, { type: 'core.manual.cancel', source: 'core.manual', summary: `Cancelled by operator:local: “${reason}”`, actor: 'operator:local', route: { effect: 'control', generation: 3, reason: 'Work terminal; waits, timers and queued input removed' } })
  patchWork(workId, (ww) => ({ state: 'cancelled', hold: undefined, extActions: [], panels: [], revision: ww.revision + 1, disposition: { group: 'closed', kind: 'cancelled', label: 'Cancelled', detail: `“${reason}” · operator:local`, since: now() } }))
  toast(`${w.ref} cancelled.`, 'warn', r)
}

function stopTimersFor(workId: string) {
  if (state.animating[workId]) {
    stopTimers(workId)
    set((s) => ({ animating: { ...s.animating, [workId]: false } }))
  }
}

export function reconcile(scopeId: string) {
  const r = receipt(`/v1/scopes/${encodeURIComponent(scopeId)}/reconcile`, { expected_revision: '7' }, 'Accepted core.manual.reconcile', undefined)
  toast(`Reconcile requested for ${scopeId}. Its sources re-read on the next pass.`, 'ok', r)
}

export function recordConfigApply(generation: number, changes: number) {
  const r = receipt('/v1/config/apply', { expected_generation: String(generation - 1), file: '~/.config/open-conduit/config.json' }, `Applied generation ${generation} (${changes} changes)`)
  return r
}

export function adoptFreshSetup(config: ConfigSnapshot) {
  set((s) => ({ config, health: s.health ? { ...s.health, generation: config.generation, tunnel: 'none' } : s.health }))
}
