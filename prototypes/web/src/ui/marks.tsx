import type { DispositionKind, Work, Turn } from '../domain/types'
import { turnActive } from '../lib/time'

/** State is always a shape and a colour, never colour alone. */
export function StateMark({ kind, size = 12, title }: { kind: DispositionKind | Turn['state']; size?: number; title?: string }) {
  const s = size
  const c = s / 2
  const common = { width: s, height: s, viewBox: `0 0 ${s} ${s}`, 'aria-hidden': title ? undefined : true, role: title ? 'img' : undefined, className: 'mark' } as const
  const t = title ? <title>{title}</title> : null
  switch (kind) {
    case 'decision':
      return (
        <svg {...common} className="mark mark-signal">
          {t}
          <path d={`M${c} 0.5 L${s - 0.5} ${c} L${c} ${s - 0.5} L0.5 ${c}Z`} fill="currentColor" />
        </svg>
      )
    case 'repair':
      return (
        <svg {...common} className="mark mark-signal">
          {t}
          <path d={`M${c} 1.4 L${s - 1.4} ${c} L${c} ${s - 1.4} L1.4 ${c}Z`} fill="none" stroke="currentColor" strokeWidth="1.8" />
          <circle cx={c} cy={c} r={1.4} fill="currentColor" />
        </svg>
      )
    case 'running':
      return (
        <svg {...common} className="mark mark-run">
          {t}
          <circle cx={c} cy={c} r={c - 0.5} fill="none" stroke="currentColor" strokeOpacity=".35" className="pulse" />
          <circle cx={c} cy={c} r={c * 0.55} fill="currentColor" />
        </svg>
      )
    case 'pending':
    case 'queued':
      return (
        <svg {...common} className="mark mark-muted">
          {t}
          <circle cx={c} cy={c} r={c - 1} fill="none" stroke="currentColor" strokeWidth="1.4" strokeDasharray="2 1.6" />
        </svg>
      )
    case 'ci':
    case 'reply':
    case 'review':
    case 'merge':
    case 'time':
    case 'event':
    case 'parked':
    case 'waiting':
    case 'handoff':
      return (
        <svg {...common} className="mark mark-wait">
          {t}
          <circle cx={c} cy={c} r={c - 0.9} fill="none" stroke="currentColor" strokeWidth="1.4" />
          <path d={`M${c} 0.9 A${c - 0.9} ${c - 0.9} 0 0 0 ${c} ${s - 0.9}Z`} fill="currentColor" />
        </svg>
      )
    case 'held':
      return (
        <svg {...common} className="mark mark-held">
          {t}
          <rect x="1.5" y="1.5" width={s - 3} height={s - 3} fill="currentColor" />
          <rect x={c - 2.2} y="3.5" width="1.4" height={s - 7} fill="var(--sheet)" />
          <rect x={c + 0.8} y="3.5" width="1.4" height={s - 7} fill="var(--sheet)" />
        </svg>
      )
    case 'completed':
      return (
        <svg {...common} className="mark mark-done">
          {t}
          <path d={`M1.5 ${c + 0.5} L${c - 1} ${s - 2.5} L${s - 1.2} 2.2`} fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="square" />
        </svg>
      )
    case 'failed':
      return (
        <svg {...common} className="mark mark-fail">
          {t}
          <path d={`M2 2 L${s - 2} ${s - 2} M${s - 2} 2 L2 ${s - 2}`} stroke="currentColor" strokeWidth="1.9" />
        </svg>
      )
    case 'cancelled':
      return (
        <svg {...common} className="mark mark-muted">
          {t}
          <circle cx={c} cy={c} r={c - 1} fill="none" stroke="currentColor" strokeWidth="1.4" />
          <path d={`M2.6 ${s - 2.6} L${s - 2.6} 2.6`} stroke="currentColor" strokeWidth="1.4" />
        </svg>
      )
  }
  return null
}

const turnColor = (t: Turn) => (t.state === 'running' || t.state === 'pending' ? 'var(--run)' : t.state === 'failed' ? 'var(--fail)' : t.state === 'cancelled' ? 'var(--ink-3)' : 'var(--ink)')

/**
 * The lifeline: a Work's whole lifetime on one line. Solid bars are active
 * execution (each resumed segment separately); the dotted line between is
 * time spent waiting, parked or queued. Ticks above are Events.
 */
export function Lifeline({ work, now, height = 22, showEvents = true }: { work: Work; now: number; height?: number; showEvents?: boolean }) {
  const W = 1000 // viewBox units; the svg stretches to its column
  const start = new Date(work.createdAt).getTime()
  const closed = work.disposition.group === 'closed'
  const end = Math.max(closed ? new Date(work.disposition.since).getTime() : now, start + 60_000)
  const x = (t: number) => ((Math.min(Math.max(t, start), end) - start) / (end - start)) * (W - 8) + 4
  const mid = height / 2 + 3
  const bars = work.turns.flatMap((t) =>
    t.segments.map((s, i) => {
      const a = x(new Date(s.start).getTime())
      const b = x(s.end ? new Date(s.end).getTime() : now)
      return <rect key={t.id + i} x={a} y={mid - 4} width={Math.max(b - a, 22)} height={8} fill={turnColor(t)} />
    }),
  )
  const endX = x(end)
  return (
    <span className="lifeline">
      <svg viewBox={`0 0 ${W} ${height + 6}`} preserveAspectRatio="none" width="100%" height={height + 6} aria-hidden>
        <line x1={4} x2={endX} y1={mid} y2={mid} stroke="var(--rule-2)" strokeWidth={1.5} strokeDasharray="3 5" vectorEffect="non-scaling-stroke" />
        {showEvents &&
          work.events.map((e) => {
            const ex = x(new Date(e.occurredAt).getTime())
            return <line key={e.id} x1={ex} x2={ex} y1={1} y2={mid - 7} stroke={e.actor && !e.actor.includes('bot') ? 'var(--ink-2)' : 'var(--rule-2)'} strokeWidth={1.25} vectorEffect="non-scaling-stroke" />
          })}
        {bars}
      </svg>
      {!closed && <StateMark kind={work.disposition.kind} size={9} />}
    </span>
  )
}

/** Proportion bar: active execution vs elapsed lifetime. */
export function Ratio({ active, elapsed }: { active: number; elapsed: number }) {
  const p = elapsed > 0 ? Math.min(1, active / elapsed) : 0
  return (
    <span className="ratio" aria-hidden>
      <span style={{ width: `${Math.max(p * 100, active > 0 ? 3 : 0)}%` }} />
    </span>
  )
}

export function turnActiveMs(t: Turn, now: number) {
  return turnActive(t, now)
}
