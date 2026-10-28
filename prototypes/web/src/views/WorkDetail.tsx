import { useEffect, useMemo, useState } from 'react'
import type { Work, Turn, WorkEvent } from '../domain/types'
import { useStore, ensureTranscript, actionsFor, type ActionId } from '../data/store'
import { useRoute, setParams, go } from '../lib/router'
import { useNow, dur, since, clockTime, stamp, turnActive, workActive, workElapsed, money, kTok } from '../lib/time'
import { StateMark, Lifeline, Ratio } from '../ui/marks'
import { ActionMenu, ActionDialog } from '../ui/actions'
import { ExtensionPanels, ActionDialogFor } from '../ui/extensions'
import type { ExtAction } from '../domain/types'
import { sourceLabel } from '../lib/labels'
import { Copy, Spinner, Kbd, Inline } from '../ui/controls'
import { Transcript, Prose, type Filter } from './Transcript'

const findWork = (works: Work[], id: string) => works.find((w) => w.id.replace(/^work\//, '').startsWith(id))

export function WorkView({ id }: { id: string }) {
  const { params } = useRoute()
  const works = useStore((s) => s.works)
  const status = useStore((s) => s.status)
  const w = findWork(works, id)
  const now = useNow()
  const [dialog, setDialog] = useState<ActionId | null>(null)
  const [tab, setTab] = useState<'transcript' | 'events'>('transcript')
  const [extAction, setExtAction] = useState<ExtAction | null>(null)

  const defaultTurn = w ? (w.turns.find((t) => t.state === 'running') ?? w.turns.find((t) => t.state === 'parked') ?? w.turns[w.turns.length - 1]) : undefined
  const turnN = Number(params.get('turn')) || defaultTurn?.attempt
  const turn = w?.turns.find((t) => t.attempt === turnN) ?? defaultTurn

  useEffect(() => {
    if (w && turn) void ensureTranscript(w, turn.id)
  }, [w?.id, turn?.id])

  useEffect(() => {
    if (!w) return
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement
      if (el.closest('input, textarea, select, dialog, [role=menu]') || e.metaKey || e.ctrlKey) return
      const i = w.turns.findIndex((t) => t.id === turn?.id)
      if ((e.key === ']' || e.key === 'j') && i < w.turns.length - 1) setParams({ turn: String(w.turns[i + 1].attempt) })
      if ((e.key === '[' || e.key === 'k') && i > 0) setParams({ turn: String(w.turns[i - 1].attempt) })
      if (e.key === 'Escape' && !document.querySelector('dialog[open]')) go('#/runs')
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [w, turn])

  if (status === 'loading')
    return (
      <div className="page">
        <div className="loading-line">
          <Spinner /> Loading work…
        </div>
      </div>
    )
  if (!w)
    return (
      <div className="page">
        <div className="state-page">
          <h1>No work with ID {id}</h1>
          <p>It may belong to a different daemon, or retention may have removed it.</p>
          <a className="btn" href="#/runs">Back to Runs</a>
        </div>
      </div>
    )

  const acts = actionsFor(w)
  // Panels that offer an action lead the page; the rest sit with the details.
  const actionPanels = (w.panels ?? []).filter((p) => p.sections.some((x) => x.actions?.length))
  const primary = w.disposition.kind === 'repair' ? acts.find((a) => (a.id === 'retry' || a.id === 'rebind') && a.enabled) : w.hold ? acts.find((a) => a.id === 'release') : undefined

  return (
    <div className="page detail">
      <nav className="crumbs" aria-label="Breadcrumb">
        <a href="#/runs">Runs</a>
        <span aria-hidden>/</span>
        <a href={`#/runs?scope=${w.scopeId}`}>{w.scopeId}</a>
        <span aria-hidden>/</span>
        <span aria-current="page">{w.ref === 'thread' ? `${sourceLabel(w.source)} thread` : w.ref}</span>
      </nav>

      <header className="d-head">
        <div className="d-title">
          <h1>
            {w.ref !== 'thread' && <span className="d-ref">{w.ref}</span>}
            <Inline text={w.title} />
          </h1>
          <p className={'d-state k-' + w.disposition.kind}>
            <StateMark kind={w.disposition.kind} size={13} />
            <b>{w.disposition.label}</b>
            <span>{w.disposition.detail}</span>
            <span className="d-since">{w.disposition.group === 'closed' ? since(w.disposition.since, now) : 'for ' + dur(now - new Date(w.disposition.since).getTime())}</span>
          </p>
        </div>
        <div className="d-actions">
          {w.links.map((l) => (
            <a key={l.url} className="btn btn-quiet btn-sm" href={l.url} target="_blank" rel="noreferrer">
              {l.label} ↗
            </a>
          ))}
          {primary && (
            <button className="btn btn-primary btn-sm" onClick={() => setDialog(primary.id)}>
              {primary.label}
            </button>
          )}
          <ActionMenu work={w} onPick={setDialog} onExt={setExtAction} label={<span className="btn-sm-label">Actions ▾</span>} />
        </div>
      </header>

      {actionPanels.length > 0 && <ExtensionPanels panels={actionPanels} target={{ kind: 'work', id: w.id }} prominent />}
      {w.disposition.kind === 'repair' && <RepairNote w={w} onAct={setDialog} />}
      {w.hold && (
        <div className="callout tone-held">
          <strong>Held by {w.hold.by} {since(w.hold.at, now)}</strong> “{w.hold.reason}”. No turn starts or resumes until it's released; events keep queueing.
        </div>
      )}

      <Score w={w} now={now} selected={turn?.attempt} />

      <div className="d-body">
        <section className="d-main" aria-label="Turn">
          <div className="tabs" role="tablist">
            <button role="tab" aria-selected={tab === 'transcript'} className={tab === 'transcript' ? 'is-on' : ''} onClick={() => setTab('transcript')}>
              {turn ? `Turn ${turn.attempt}` : 'Turns'}
            </button>
            <button role="tab" aria-selected={tab === 'events'} className={tab === 'events' ? 'is-on' : ''} onClick={() => setTab('events')}>
              Events <span className="tab-n">{w.events.length}</span>
            </button>
          </div>
          {tab === 'events' ? <EventsTable w={w} /> : turn ? <TurnPanel w={w} t={turn} now={now} /> : <NoTurns w={w} />}
        </section>
        <Facts w={w} t={turn} now={now} />
      </div>
      {dialog && <ActionDialog work={w} action={dialog} onClose={() => setDialog(null)} />}
      <ActionDialogFor action={extAction} target={{ kind: 'work', id: w.id }} onClose={() => setExtAction(null)} />
    </div>
  )
}

function RepairNote({ w, onAct }: { w: Work; onAct: (a: ActionId) => void }) {
  const last = [...w.turns].reverse().find((t) => t.state === 'failed' || t.state === 'cancelled')
  return (
    <section className="repair" aria-label="Needs repair">
      <div className="repair-head">
        <StateMark kind="repair" size={13} />
        <b>{w.disposition.label}</b>
      </div>
      {w.pinnedBinding ? (
        <p>
          A configuration reload removed binding <code>{w.pinnedBinding}</code>, and this Work's queued input is pinned to it, so the scheduler can't start a turn. Rebinding moves the input to a binding that exists in the current generation.
        </p>
      ) : w.retry?.exhausted ? (
        <>
          <p>
            The last {w.retry.failureCount} attempts failed the same way, before the agent started: <b>workspace setup</b> couldn't download Go modules because <code>proxy.golang.org</code> didn't resolve. That points at this machine's network or DNS at the time, not at the work itself. Retry policy has stopped trying (<code>{w.retry.blockedReason}</code>).
          </p>
          <p className="repair-next">If the network is back, running it again is safe: the review comment that triggered turn 3 is still queued and will be presented to the new attempt.</p>
        </>
      ) : (
        <p>{last?.outcome?.summary ?? w.disposition.detail}</p>
      )}
      <div className="row-gap">
        {w.pinnedBinding ? (
          <button className="btn btn-primary" onClick={() => onAct('rebind')}>Rebind queued input…</button>
        ) : (
          <button className="btn btn-primary" onClick={() => onAct('retry')}>Run again…</button>
        )}
        {last && (
          <button className="btn btn-quiet" onClick={() => setParams({ turn: String(last.attempt) })}>
            Inspect turn {last.attempt}
          </button>
        )}
        <button className="btn btn-quiet" onClick={() => onAct('hold')}>Hold instead</button>
      </div>
    </section>
  )
}

/** The score: every turn and every wait between them, in order. */
function Score({ w, now, selected }: { w: Work; now: number; selected?: number }) {
  const active = workActive(w, now)
  const elapsed = workElapsed(w, now)
  return (
    <section className="score" aria-label="Lifecycle">
      <div className="score-top">
        <span className="score-k">Lifetime</span>
        <span className="score-v">
          <b>{dur(active, true)}</b> executing of <b>{dur(elapsed)}</b> since {clockTime(w.createdAt)}
        </span>
        <Ratio active={active} elapsed={elapsed} />
        <span className="score-v score-right">
          {w.turns.length} turn{w.turns.length === 1 ? '' : 's'} · {w.events.length} events
        </span>
      </div>
      <Lifeline work={w} now={now} height={20} />
      <ol className="score-turns">
        {w.turns.map((t, i) => {
          const prev = w.turns[i - 1]
          const gapFrom = prev ? prev.segments[prev.segments.length - 1].end : w.createdAt
          const gap = gapFrom ? new Date(t.segments[0].start).getTime() - new Date(gapFrom).getTime() : 0
          const trig = w.events.find((e) => e.id === t.triggerEventId)
          return (
            <li key={t.id} className="score-item">
              {gap > 90_000 && (
                <span className="score-gap" title="Time between turns">
                  {dur(gap)}
                  <i>{prev ? waitedFor(prev) : 'queued'}</i>
                </span>
              )}
              <button type="button" className={'score-turn s-' + t.state + (selected === t.attempt ? ' is-on' : '')} aria-current={selected === t.attempt ? 'step' : undefined} onClick={() => setParams({ turn: String(t.attempt) })}>
                <span className="st-row">
                  <StateMark kind={t.state} size={10} />
                  <b>Turn {t.attempt}</b>
                  <span className="st-dur">{dur(turnActive(t, now), true)}</span>
                </span>
                <span className="st-trig">{trig ? shortType(trig.type) : t.attempt === 1 ? 'start' : 'retry'}</span>
                {t.segments.length > 1 && <span className="st-seg">{t.segments.length} segments</span>}
              </button>
            </li>
          )
        })}
        {w.disposition.group !== 'closed' && w.disposition.group !== 'running' && (
          <li className="score-item score-now">
            <span className="score-gap is-open">
              {dur(now - new Date(w.disposition.since).getTime())}
              <i>{w.disposition.label.toLowerCase()}</i>
            </span>
            <span className={'score-next k-' + w.disposition.kind}>
              <StateMark kind={w.disposition.kind} size={10} /> now
            </span>
          </li>
        )}
      </ol>
    </section>
  )
}

function waitedFor(t: Turn): string {
  const s = t.outcome?.summary?.toLowerCase() ?? ''
  if (t.state === 'failed') return 'retry backoff'
  if (s.includes('waiting on checks') || s.includes('on checks')) return 'waiting on CI'
  if (s.includes('review')) return 'waiting on review'
  if (s.includes('merge')) return 'waiting on merge'
  if (s.includes('thread')) return 'waiting on a reply'
  return 'waiting'
}
/** An event type without its family prefix: github.checks.passed → checks passed. */
const shortType = (t: string) => t.split('.').slice(1).join(' ').replace(/_/g, ' ') || t

function TurnPanel({ w, t, now }: { w: Work; t: Turn; now: number }) {
  const tx = useStore((s) => s.transcripts[t.id])
  const [filters, setFilters] = useState<Set<Filter>>(new Set(['conversation', 'tools', 'effects', 'system']))
  const [query, setQuery] = useState('')
  const [expandAll, setExpandAll] = useState(0)
  const [jump, setJump] = useState(0)
  const trig = w.events.find((e) => e.id === t.triggerEventId)
  const items = Array.isArray(tx) ? tx : []
  const errors = items.filter((i) => i.kind === 'error' || ((i.kind === 'tool' || i.kind === 'effect') && !i.ok)).length
  const live = t.state === 'running' || t.state === 'pending'
  const counts = useMemo(() => {
    const c = { conversation: 0, tools: 0, effects: 0, system: 0 }
    for (const i of items) c[i.kind === 'tool' ? 'tools' : i.kind === 'effect' ? 'effects' : i.kind === 'system' ? 'system' : 'conversation']++
    return c
  }, [items])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.target as HTMLElement).closest('input, textarea, select, dialog')) return
      if (e.key === 'e' && errors) setJump((j) => j + 1)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [errors])
  const toggle = (f: Filter) =>
    setFilters((s) => {
      const n = new Set(s)
      if (n.has(f)) n.delete(f)
      else n.add(f)
      return n
    })

  return (
    <div className="turn">
      <div className="turn-head">
        <p className="turn-trigger">
          {trig ? (
            <>
              {t.attempt === 1 ? 'Started by' : t.state === 'failed' && t.attempt > 1 && w.turns[t.attempt - 2]?.state === 'failed' ? 'Retry of' : 'Woken by'} <code>{trig.type}</code> — {trig.summary}
              <span className="turn-when"> · {clockTime(t.segments[0].start)}</span>
            </>
          ) : (
            <>Started {clockTime(t.segments[0].start)}</>
          )}
        </p>
        {t.outcome && !live && (
          <div className={'turn-result o-' + t.outcome.kind}>
            <Prose text={t.outcome.summary} />
            <p className="t-meta">
              {t.outcome.kind === 'handoff' ? 'Turn ended; the next event continues the work' : t.outcome.kind === 'waiting' ? 'Parked; resumes in this session' : t.outcome.kind === 'completed' ? 'Completed' : 'Failed'}
              {t.outcome.status && <> · status {t.outcome.status}</>}
            </p>
          </div>
        )}
        <dl className="turn-stats">
          <div>
            <dt>Active</dt>
            <dd>
              {dur(turnActive(t, now), true)}
              {live && ' so far'}
              {t.segments.length > 1 && <small> in {t.segments.length} segments</small>}
            </dd>
          </div>
          <div>
            <dt>Executor</dt>
            <dd>
              {t.binding} → {t.executor}
            </dd>
          </div>
          {t.model && (
            <div>
              <dt>Model</dt>
              <dd>{t.model}</dd>
            </div>
          )}
          {t.usage && (
            <div>
              <dt>Cost</dt>
              <dd>
                {money(t.usage.cost)} <small>{kTok(t.usage.outputTokens)} out · {kTok(t.usage.cachedTokens)} cached</small>
              </dd>
            </div>
          )}
          {(t.modelCalls || t.toolCalls) && (
            <div>
              <dt>Calls</dt>
              <dd>
                {t.modelCalls ?? 0} model · {t.toolCalls ?? 0} tool
              </dd>
            </div>
          )}
          {live && t.deadline && (
            <div>
              <dt>Deadline</dt>
              <dd>{dur(new Date(t.deadline).getTime() - now)} left</dd>
            </div>
          )}
        </dl>
      </div>

      <div className="tx-tools" role="toolbar" aria-label="Transcript controls">
        <div className="tx-filters" role="group" aria-label="Show">
          {(['conversation', 'effects', 'tools', 'system'] as Filter[]).map((f) => (
            <label key={f} className={'chip' + (filters.has(f) ? ' is-on' : '')}>
              <input type="checkbox" checked={filters.has(f)} onChange={() => toggle(f)} />
              {{ conversation: 'Messages', effects: 'Toolbelt', tools: 'Tool calls', system: 'System' }[f]} <span>{counts[f]}</span>
            </label>
          ))}
        </div>
        <span className="toolbar-gap" />
        {errors > 0 && (
          <button className="btn btn-xs btn-fail" onClick={() => setJump((j) => j + 1)}>
            Jump to error <Kbd>e</Kbd>
          </button>
        )}
        <button className="btn btn-xs btn-quiet" onClick={() => setExpandAll((n) => (n % 2 === 1 ? n + 1 : n + 1))}>
          {expandAll % 2 === 1 ? 'Collapse all' : 'Expand all'}
        </button>
        <input className="tx-search" type="search" placeholder="Find in turn" value={query} onChange={(e) => setQuery(e.target.value)} aria-label="Find in this turn" />
      </div>

      {tx === 'loading' || tx === undefined ? (
        <div className="loading-line">
          <Spinner /> Reading the transcript artifact…
        </div>
      ) : (
        <Transcript items={items} live={live} filters={filters} query={query} expandAll={expandAll} jumpTo={jump} hideFinalOutcome={!!t.outcome && !live} />
      )}
    </div>
  )
}

function NoTurns({ w }: { w: Work }) {
  return (
    <div className="tx-empty">
      {w.hold ? 'No turn has started: the Work was held before its first turn.' : w.disposition.group === 'queued' ? `No turn has started yet. ${w.disposition.detail}.` : `No turns yet. ${w.disposition.detail}.`}
    </div>
  )
}

const EFFECT_WORD: Record<WorkEvent['route']['effect'], string> = { start: 'started work', queue: 'queued', inject: 'injected', wake: 'woke', drop: 'dropped', control: 'control', unmatched: 'unmatched', terminal: 'after end' }

function EventsTable({ w }: { w: Work }) {
  const [open, setOpen] = useState<string | null>(null)
  return (
    <div className="events">
      <p className="events-intro">Every event recorded for this Work, in acceptance order, with how routing handled it. Use this to answer “why did that happen?”</p>
      <ol className="ev-list">
        {w.events.map((e) => (
          <li key={e.id} className={'ev eff-' + e.route.effect}>
            <button type="button" className="ev-row" aria-expanded={open === e.id} onClick={() => setOpen(open === e.id ? null : e.id)}>
              <span className="ev-seq">{e.sequence}</span>
              <time>{clockTime(e.occurredAt)}</time>
              <code className="ev-type">{e.type}</code>
              <span className="ev-sum">{e.summary}</span>
              <span className="ev-eff">{EFFECT_WORD[e.route.effect]}</span>
              {e.attempt && <span className="ev-turn">turn {e.attempt}</span>}
            </button>
            {open === e.id && (
              <dl className="ev-detail">
                <dt>Routing</dt>
                <dd>
                  {e.route.route ? (
                    <>
                      route <code>{e.route.route}</code>
                      {e.route.binding && <> → binding <code>{e.route.binding}</code></>}
                      {e.route.policy && <> · active policy <code>{e.route.policy}</code></>} · generation {e.route.generation}
                    </>
                  ) : (
                    'control event (not routed as input)'
                  )}
                </dd>
                <dt>Why</dt>
                <dd>{e.route.reason}</dd>
                <dt>Source</dt>
                <dd>
                  <code>{e.source}</code>
                  {e.actor && <> · {e.actor}</>} · accepted {stamp(e.occurredAt)}
                </dd>
                <dt>Event ID</dt>
                <dd>
                  <code>{e.id}</code> <Copy text={e.id} />
                </dd>
                <dt>CLI</dt>
                <dd>
                  <code>open-conduit events chain {e.id.slice(0, 18)}…</code>
                </dd>
              </dl>
            )}
          </li>
        ))}
      </ol>
    </div>
  )
}

function Facts({ w, t, now }: { w: Work; t?: Turn; now: number }) {
  const cfg = useStore((s) => s.config)
  const scope = cfg?.scopes.find((s) => s.id === w.scopeId)
  const firstRoute = w.events.find((e) => e.route.route)?.route
  const route = scope?.routes.find((r) => r.name === firstRoute?.route)
  const total = w.turns.reduce((a, x) => a + (x.usage?.cost ?? 0), 0)
  return (
    <aside className="facts" aria-label="Details">
      <section>
        <h3>Work</h3>
        <dl>
          <dt>ID</dt>
          <dd className="mono-cut">
            {w.id.slice(0, 18)}… <Copy text={w.id} />
          </dd>
          <dt>Kind</dt>
          <dd><code>{w.kind}</code></dd>
          <dt>Scope</dt>
          <dd><a href={'#/config/' + w.scopeId}>{scope?.name ?? w.scopeId}</a></dd>
          <dt>State</dt>
          <dd><code>{w.state}</code> · revision {w.revision}</dd>
          <dt>Owns</dt>
          <dd>
            {w.correlationKeys.map((k) => (
              <code key={k.externalId} className="block-code">{k.externalId}</code>
            ))}
          </dd>
          <dt>Created</dt>
          <dd>{clockTime(w.createdAt)}</dd>
        </dl>
      </section>
      <section>
        <h3>Routing</h3>
        <dl>
          <dt>Route</dt>
          <dd>
            <code>{firstRoute?.route ?? '—'}</code>
            {route && <> · handler <code>{route.handler}</code></>}
          </dd>
          <dt>While running</dt>
          <dd>{route ? <><code>{route.activePolicy}</code> — {policyWords(route.activePolicy)}</> : '—'}</dd>
          <dt>Binding</dt>
          <dd>
            <code>{w.binding}</code> → {w.executor}
          </dd>
          <dt>Generation</dt>
          <dd>{t?.generation ?? cfg?.generation}</dd>
        </dl>
        <a className="link-btn" href={`#/config/${w.scopeId}?trace=1`}>Trace how an event routes here →</a>
      </section>
      {t && (
        <section>
          <h3>Turn {t.attempt}</h3>
          <dl>
            <dt>Execution</dt>
            <dd className="mono-cut">…/attempt/{t.attempt} <Copy text={t.id} /></dd>
            <dt>State</dt>
            <dd><code>{t.state}</code></dd>
            {t.workspace && (
              <>
                <dt>Workspace</dt>
                <dd>
                  {t.workspace.kind} · <code>{t.workspace.ref}</code>
                </dd>
              </>
            )}
            <dt>Inputs</dt>
            <dd>{t.inputs} event{t.inputs === 1 ? '' : 's'}</dd>
            {t.error && (
              <>
                <dt>Error</dt>
                <dd className="fail-text"><code>{t.error.code}</code></dd>
              </>
            )}
          </dl>
          <div className="cmd cmd-sm">
            <code>open-conduit runs log {t.id.slice(0, 14)}… --transcript</code>
            <Copy text={`open-conduit runs log ${t.id} --transcript`} />
          </div>
        </section>
      )}
      <section>
        <h3>Totals</h3>
        <dl>
          <dt>Executing</dt>
          <dd>{dur(workActive(w, now), true)}</dd>
          <dt>Lifetime</dt>
          <dd>{dur(workElapsed(w, now))}</dd>
          <dt>Cost</dt>
          <dd>{total ? money(total) : '—'} <small>{w.turns.filter((x) => x.usage).length} turns reported usage</small></dd>
        </dl>
      </section>
      {(w.panels ?? []).filter((p) => !p.sections.some((x) => x.actions?.length)).length > 0 && (
        <section>
          <h3>From extensions</h3>
          <ExtensionPanels panels={(w.panels ?? []).filter((p) => !p.sections.some((x) => x.actions?.length))} target={{ kind: 'work', id: w.id }} />
        </section>
      )}
    </aside>
  )
}

export const policyWords = (p: string) =>
  ({ inject: 'new events are delivered into the live session', queue: 'new events wait for the next turn', interrupt: 'new events stop the turn and start another', drop: 'new events are ignored' })[p] ?? p
