// Small builders that keep fixture authoring readable. Times are minutes
// before the moment the prototype loaded, so the data always looks current.

import type { TranscriptItem, Segment, Turn, WorkEvent, RouteDecision, ExtLink } from '../../domain/types'

export const LOADED_AT = Date.now()

export function ago(minutes: number): string {
  return new Date(LOADED_AT - minutes * 60_000).toISOString()
}
export function ahead(minutes: number): string {
  return new Date(LOADED_AT + minutes * 60_000).toISOString()
}

/** Deterministic 64-hex identity, the shape the daemon uses for Work IDs. */
export function hex64(seed: string): string {
  let out = ''
  let h = 2166136261
  for (let round = 0; out.length < 64; round++) {
    for (let i = 0; i < seed.length; i++) {
      h ^= seed.charCodeAt(i) + round
      h = Math.imul(h, 16777619) >>> 0
    }
    out += h.toString(16).padStart(8, '0')
  }
  return out.slice(0, 64)
}

export function workId(seed: string) {
  return 'work/' + hex64(seed)
}

let itemSeq = 0
const nid = (p: string) => `${p}-${++itemSeq}`

/**
 * A transcript is written as a sequence of steps with durations (seconds).
 * `script(startMinutesAgo)` lays them out in time and returns the items.
 */
type Step =
  | ['prompt', string, string]
  | ['system', string, string?]
  | ['agent', string]
  | ['human', string, string, string, boolean?]
  | ['tool', string, string, number, string?, string?, boolean?]
  | ['effect', string, string, number, string?, string?, ExtLink?, boolean?]
  | ['orch', 'park' | 'wake' | 'inject' | 'start' | 'cancel', string, string?]
  | ['outcome', 'completed' | 'failed' | 'waiting' | 'handoff', string, string?]
  | ['error', string, string?]
  | ['gap', number]

export function script(startMinutesAgo: number, steps: Step[]): TranscriptItem[] {
  let t = LOADED_AT - startMinutesAgo * 60_000
  const at = () => new Date(t).toISOString()
  const items: TranscriptItem[] = []
  for (const s of steps) {
    switch (s[0]) {
      case 'gap':
        t += s[1] * 1000
        break
      case 'prompt':
        items.push({ id: nid('p'), kind: 'prompt', at: at(), title: s[1], text: s[2] })
        t += 1500
        break
      case 'system':
        items.push({ id: nid('s'), kind: 'system', at: at(), text: s[1], detail: s[2] })
        t += 800
        break
      case 'agent':
        items.push({ id: nid('a'), kind: 'agent', at: at(), text: s[1] })
        t += 4000 + s[1].length * 25
        break
      case 'human':
        items.push({ id: nid('h'), kind: 'human', at: at(), author: s[1], via: s[2], text: s[3], injected: s[4] })
        t += 1000
        break
      case 'tool': {
        const [, tool, label, secs, input, output, failed] = s
        items.push({ id: nid('t'), kind: 'tool', at: at(), tool, label, input, output, ok: !failed, ms: Math.round(secs * 1000) })
        t += secs * 1000 + 2500
        break
      }
      case 'effect': {
        const [, operation, label, secs, input, output, link, failed] = s
        items.push({ id: nid('e'), kind: 'effect', at: at(), operation, label, input, output, link, ok: !failed, ms: Math.round(secs * 1000) })
        t += secs * 1000 + 2000
        break
      }
      case 'orch':
        items.push({ id: nid('o'), kind: 'orchestration', at: at(), tone: s[1], text: s[2], eventId: s[3] })
        t += 300
        break
      case 'outcome':
        items.push({ id: nid('r'), kind: 'outcome', at: at(), outcome: s[1], summary: s[2], status: s[3] })
        break
      case 'error':
        items.push({ id: nid('x'), kind: 'error', at: at(), text: s[1], detail: s[2] })
        t += 500
        break
    }
  }
  return items
}

export function endOf(items: TranscriptItem[]): string {
  return items.length ? items[items.length - 1].at : new Date(LOADED_AT).toISOString()
}

export function seg(start: string, end?: string, wakeEventId?: string): Segment {
  return { start, end, wakeEventId }
}

export function route(effect: RouteDecision['effect'], reason: string, extra: Partial<RouteDecision> = {}): RouteDecision {
  return { effect, reason, generation: 3, ...extra }
}

let evSeq = 0
export function ev(
  minutesAgo: number,
  type: string,
  source: string,
  summary: string,
  r: RouteDecision,
  extra: Partial<WorkEvent> = {},
): WorkEvent {
  return {
    id: extra.id ?? `${source.split('/')[0]}-${hex64(type + summary + minutesAgo).slice(0, 40)}`,
    sequence: ++evSeq, // re-sequenced by time once every fixture is built
    type,
    source,
    occurredAt: ago(minutesAgo),
    summary,
    route: r,
    ...extra,
  }
}

export function sumUsage(turns: Turn[]) {
  const u = { inputTokens: 0, outputTokens: 0, cachedTokens: 0, cost: 0 }
  for (const t of turns) {
    if (!t.usage) continue
    u.inputTokens += t.usage.inputTokens
    u.outputTokens += t.usage.outputTokens
    u.cachedTokens += t.usage.cachedTokens
    u.cost += t.usage.cost
  }
  return u
}

export const GH = 'github/github.com/open-conduit/open-conduit'
export const GHJ = 'github/github.com/open-conduit/open-conduit-jiminny'
export const SLACK = 'slack/T0C49A2P55Z'
export const gh = (repo: string, kind: 'issues' | 'pull', n: number) => `https://github.com/${repo}/${kind}/${n}`
