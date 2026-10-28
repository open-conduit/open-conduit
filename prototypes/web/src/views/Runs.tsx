import { useEffect, useMemo, useRef, useState } from 'react'
import type { Work, DispositionGroup } from '../domain/types'
import { useStore, actionsFor, hold, release, type ActionId } from '../data/store'
import { useRoute, setParams, go, workHref } from '../lib/router'
import { useNow, dur, since, workActive, workElapsed } from '../lib/time'
import { StateMark, Lifeline, Ratio } from '../ui/marks'
import { ActionMenu, ActionDialog } from '../ui/actions'
import { Kbd, Dialog, Inline } from '../ui/controls'
import { sourceLabel } from '../lib/labels'

const GROUPS: { g: DispositionGroup; label: string; blurb: string }[] = [
  { g: 'attention', label: 'Needs you', blurb: 'decisions and repairs' },
  { g: 'running', label: 'Running', blurb: 'executing now' },
  { g: 'queued', label: 'Queued', blurb: 'ready, awaiting a slot' },
  { g: 'waiting', label: 'Waiting', blurb: 'on CI, people or time' },
  { g: 'held', label: 'Held', blurb: 'paused on purpose' },
  { g: 'closed', label: 'Closed', blurb: 'completed or cancelled' },
]
const ORDER: Record<DispositionGroup, number> = { attention: 0, running: 1, queued: 2, waiting: 3, held: 4, closed: 5 }
const WINDOWS: Record<string, number> = { '1h': 60, '24h': 1440, '7d': 10080 }

type Sort = 'attention' | 'updated' | 'waiting' | 'active' | 'created'
const SORTS: { v: Sort; label: string }[] = [
  { v: 'attention', label: 'What needs me first' },
  { v: 'updated', label: 'Recently changed' },
  { v: 'waiting', label: 'Longest in this state' },
  { v: 'active', label: 'Most execution time' },
  { v: 'created', label: 'Newest work' },
]

export function RunsView() {
  const { params } = useRoute()
  const works = useStore((s) => s.works)
  const status = useStore((s) => s.status)
  const scopes = useStore((s) => s.config?.scopes ?? [])
  const configured = useStore((s) => (s.config?.generation ?? 0) > 0)
  const now = useNow()

  const g = (params.get('g') ?? 'open') as DispositionGroup | 'open' | 'all'
  const wk = params.get('w')
  const q = params.get('q') ?? ''
  const scope = params.get('scope') ?? ''
  const source = params.get('source') ?? ''
  const executor = params.get('executor') ?? ''
  const win = params.get('updated') ?? ''
  const sort = (params.get('sort') ?? 'attention') as Sort
  const groupBy = params.get('by') ?? 'state'

  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [cursor, setCursor] = useState(0)
  const [dialog, setDialog] = useState<{ work: Work; action: ActionId } | null>(null)
  const [bulk, setBulk] = useState<'hold' | 'release' | null>(null)
  const search = useRef<HTMLInputElement>(null)
  const list = useRef<HTMLDivElement>(null)

  // Facets apply before the state rail so its counts reflect them.
  const faceted = useMemo(() => {
    const t = q.trim().toLowerCase()
    return works.filter((w) => {
      if (scope && w.scopeId !== scope) return false
      if (source && w.source !== source) return false
      if (executor && w.executor !== executor) return false
      if (win && now - new Date(w.updatedAt).getTime() > WINDOWS[win] * 60_000) return false
      if (t) {
        const hay = [w.ref, w.title, w.scopeId, w.summary, w.disposition.label, w.disposition.detail, w.requester ?? '', w.kind, w.id, ...w.links.map((l) => l.label)].join(' ').toLowerCase()
        if (!t.split(/\s+/).every((p) => hay.includes(p))) return false
      }
      return true
    })
  }, [works, q, scope, source, executor, win, now])

  const counts = useMemo(() => {
    const c: Record<string, number> = {}
    for (const w of faceted) {
      c[w.disposition.group] = (c[w.disposition.group] ?? 0) + 1
      if (w.disposition.group === 'waiting') c['k:' + w.disposition.label] = (c['k:' + w.disposition.label] ?? 0) + 1
    }
    return c
  }, [faceted])

  const rows = useMemo(() => {
    let r = faceted.filter((w) => (g === 'all' ? true : g === 'open' ? w.disposition.group !== 'closed' : w.disposition.group === g))
    if (g === 'waiting' && wk) r = r.filter((w) => w.disposition.label === wk)
    const age = (w: Work) => new Date(w.disposition.since).getTime()
    const cmp: Record<Sort, (a: Work, b: Work) => number> = {
      attention: (a, b) => ORDER[a.disposition.group] - ORDER[b.disposition.group] || (a.disposition.group === 'closed' ? age(b) - age(a) : age(a) - age(b)),
      updated: (a, b) => b.updatedAt.localeCompare(a.updatedAt),
      waiting: (a, b) => age(a) - age(b),
      active: (a, b) => workActive(b, now) - workActive(a, now),
      created: (a, b) => b.createdAt.localeCompare(a.createdAt),
    }
    return [...r].sort(cmp[sort])
  }, [faceted, g, wk, sort, now])

  const sections = useMemo(() => {
    if (groupBy === 'none') return [{ key: 'all', label: '', rows }]
    if (groupBy === 'scope') {
      const ids = [...new Set(rows.map((w) => w.scopeId))]
      return ids.map((id) => ({ key: id, label: scopes.find((s) => s.id === id)?.name ?? id, rows: rows.filter((w) => w.scopeId === id) }))
    }
    return GROUPS.map((x) => ({ key: x.g, label: x.label, rows: rows.filter((w) => w.disposition.group === x.g) })).filter((s) => s.rows.length)
  }, [rows, groupBy, scopes])
  const flat = sections.flatMap((s) => s.rows)

  useEffect(() => setCursor((c) => Math.min(c, Math.max(flat.length - 1, 0))), [flat.length])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement
      if (el.closest('input, textarea, select, dialog, [role=menu]') || e.metaKey || e.ctrlKey || e.altKey) {
        if (e.key === 'Escape' && el === search.current) search.current?.blur()
        return
      }
      if (e.key === '/') {
        e.preventDefault()
        search.current?.focus()
      } else if (e.key === 'j' || e.key === 'k') {
        e.preventDefault()
        setCursor((c) => {
          const n = Math.max(0, Math.min(flat.length - 1, c + (e.key === 'j' ? 1 : -1)))
          list.current?.querySelector<HTMLElement>(`[data-row="${n}"] .row-link`)?.focus()
          return n
        })
      } else if (e.key === 'Enter' && flat[cursor] && !el.closest('a, button')) {
        go(workHref(flat[cursor].id))
      } else if (e.key === 'x' && flat[cursor]) {
        toggle(flat[cursor].id)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  })

  const toggle = (id: string) =>
    setSelected((s) => {
      const n = new Set(s)
      if (n.has(id)) n.delete(id)
      else n.add(id)
      return n
    })
  const sel = works.filter((w) => selected.has(w.id))
  const canHold = sel.filter((w) => actionsFor(w).find((a) => a.id === 'hold')?.enabled)
  const canRelease = sel.filter((w) => w.hold)

  const activeFilters = [
    scope && { k: 'scope', label: `Scope: ${scope}` },
    source && { k: 'source', label: `Source: ${sourceLabel(source)}` },
    executor && { k: 'executor', label: `Executor: ${executor}` },
    win && { k: 'updated', label: `Changed in the last ${win}` },
    q && { k: 'q', label: `“${q}”` },
  ].filter(Boolean) as { k: string; label: string }[]

  const total = works.length
  const running = counts.running ?? 0
  const att = counts.attention ?? 0

  if (status === 'ready' && !configured && total === 0) return <FreshInstall />

  return (
    <div className="page runs">
      <header className="page-head">
        <div>
          <h1>Runs</h1>
          <p className="page-sub">
            {status === 'loading' ? (
              'Reading work from the daemon…'
            ) : total === 0 ? (
              `No work yet. Watching ${scopes.length} scope${scopes.length === 1 ? '' : 's'}; new issues, threads and schedule firings appear here.`
            ) : (
              <>
                {total} pieces of work across {new Set(works.map((w) => w.scopeId)).size} scopes. {att > 0 && <a href="#/attention" className="sub-signal">{att} need you</a>}
                {att > 0 && running > 0 && ', '}
                {running > 0 && `${running} running now`}
                {(att > 0 || running > 0) && '.'}
              </>
            )}
          </p>
        </div>
      </header>

      <div className="rail" role="radiogroup" aria-label="Filter by state">
        <RailCell on={g === 'open'} n={faceted.filter((w) => w.disposition.group !== 'closed').length} label="Open" blurb="everything not closed" onClick={() => setParams({ g: undefined, w: undefined })} loading={status === 'loading'} />
        {GROUPS.map((x) => (
          <RailCell key={x.g} on={g === x.g} n={counts[x.g] ?? 0} label={x.label} blurb={x.blurb} signal={x.g === 'attention'} onClick={() => setParams({ g: x.g, w: undefined })} loading={status === 'loading'} />
        ))}
      </div>
      {g === 'waiting' && (
        <div className="subrail" role="radiogroup" aria-label="Waiting for">
          <span className="subrail-lead">Waiting</span>
          <button role="radio" aria-checked={!wk} className={!wk ? 'is-on' : ''} onClick={() => setParams({ w: undefined })}>
            Anything <span>{counts.waiting ?? 0}</span>
          </button>
          {Object.keys(counts)
            .filter((k) => k.startsWith('k:'))
            .map((k) => k.slice(2))
            .sort((a, b) => (counts['k:' + b] ?? 0) - (counts['k:' + a] ?? 0))
            .map((label) => (
              <button key={label} role="radio" aria-checked={wk === label} className={wk === label ? 'is-on' : ''} onClick={() => setParams({ w: label })}>
                {label.replace(/^Waiting (for|on) /, '')} <span>{counts['k:' + label]}</span>
              </button>
            ))}
        </div>
      )}

      <div className="toolbar">
        <label className="search">
          <span className="sr-only">Search runs</span>
          <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden>
            <circle cx="6" cy="6" r="4.5" fill="none" stroke="currentColor" strokeWidth="1.4" />
            <path d="M9.5 9.5 13 13" stroke="currentColor" strokeWidth="1.4" />
          </svg>
          <input ref={search} type="search" value={q} placeholder="Search titles, refs, summaries, people" onChange={(e) => setParams({ q: e.target.value })} />
          {!q && <Kbd>/</Kbd>}
        </label>
        <Facet label="Scope" value={scope} onChange={(v) => setParams({ scope: v })} options={scopes.map((s) => ({ v: s.id, label: s.name }))} />
        <Facet label="Source" value={source} onChange={(v) => setParams({ source: v })} options={[...new Set(works.map((w) => w.source))].map((s) => ({ v: s, label: sourceLabel(s) }))} />
        <Facet label="Executor" value={executor} onChange={(v) => setParams({ executor: v })} options={[...new Set(works.map((w) => w.executor))].map((s) => ({ v: s, label: s }))} />
        <Facet label="Changed" value={win} onChange={(v) => setParams({ updated: v })} options={[{ v: '1h', label: 'last hour' }, { v: '24h', label: 'last 24 hours' }, { v: '7d', label: 'last 7 days' }]} anyLabel="any time" />
        <span className="toolbar-gap" />
        <Facet label="Group" value={groupBy} onChange={(v) => setParams({ by: v === 'state' ? undefined : v })} options={[{ v: 'state', label: 'state' }, { v: 'scope', label: 'scope' }, { v: 'none', label: 'none' }]} noAny />
        <Facet label="Sort" value={sort} onChange={(v) => setParams({ sort: v === 'attention' ? undefined : v })} options={SORTS.map((s) => ({ v: s.v, label: s.label }))} noAny />
      </div>

      {activeFilters.length > 0 && (
        <div className="filters-on" aria-label="Active filters">
          {activeFilters.map((f) => (
            <button key={f.k} className="token" onClick={() => setParams({ [f.k]: undefined })} aria-label={`Remove filter ${f.label}`}>
              {f.label} <span aria-hidden>✕</span>
            </button>
          ))}
          <button className="link-btn" onClick={() => setParams({ scope: undefined, source: undefined, executor: undefined, updated: undefined, q: undefined })}>
            Clear all
          </button>
        </div>
      )}

      {sel.length > 0 && (
        <div className="bulkbar" role="region" aria-label="Bulk actions">
          <span>
            <b>{sel.length}</b> selected
          </span>
          <button className="btn" disabled={!canHold.length} onClick={() => setBulk('hold')}>
            Hold {canHold.length < sel.length && canHold.length > 0 ? `${canHold.length} of ${sel.length}` : ''}
          </button>
          <button className="btn" disabled={!canRelease.length} onClick={() => setBulk('release')}>
            Release {canRelease.length < sel.length && canRelease.length > 0 ? `${canRelease.length} of ${sel.length}` : ''}
          </button>
          <button className="link-btn" onClick={() => setSelected(new Set())}>
            Clear selection
          </button>
        </div>
      )}

      <div className="ledger" ref={list} role="table" aria-label="Work" aria-rowcount={flat.length}>
        <div className="ledger-head" role="row">
          <span role="columnheader" className="c-sel">
            <span className="sr-only">Select</span>
          </span>
          <span role="columnheader" className="c-work">Work</span>
          <span role="columnheader" className="c-now">Now</span>
          <span role="columnheader" className="c-life">Lifetime</span>
          <span role="columnheader" className="c-turns" title="Executions">Turns</span>
          <span role="columnheader" className="c-time" title="Active execution time against elapsed lifetime">Active / elapsed</span>
          <span role="columnheader" className="c-upd">Changed</span>
          <span role="columnheader" className="c-act">
            <span className="sr-only">Actions</span>
          </span>
        </div>
        {status === 'loading' && <Skeleton />}
        {status === 'ready' &&
          sections.map((s) => (
            <div key={s.key} role="rowgroup" className="ledger-group">
              {s.label && (
                <div className="group-rule" role="row">
                  <span role="cell">
                    {s.label} <span className="group-n">{s.rows.length}</span>
                  </span>
                </div>
              )}
              {s.rows.map((w) => {
                const idx = flat.indexOf(w)
                return <Row key={w.id} w={w} now={now} index={idx} focused={idx === cursor} selected={selected.has(w.id)} onToggle={() => toggle(w.id)} onAction={(a) => setDialog({ work: w, action: a })} onFocus={() => setCursor(idx)} />
              })}
            </div>
          ))}
        {status === 'ready' && flat.length === 0 && (
          <div className="empty" role="row">
            <div role="cell">
              {works.length === 0 ? (
                <>
                  <h2>No work yet</h2>
                  <p>Open Conduit is configured, but no source has produced work. New issues, threads and schedule firings appear here as they arrive.</p>
                  <a className="btn" href="#/config?trace=1">Trace how an event would route</a>
                </>
              ) : g === 'attention' && activeFilters.length === 0 ? (
                <>
                  <h2>Nothing needs you</h2>
                  <p>No open questions, failed retries or broken bindings. Agents are working or waiting on their own.</p>
                </>
              ) : (
                <>
                  <h2>No work matches</h2>
                  <p>
                    {faceted.length === 0 ? 'Your filters exclude everything.' : `${faceted.length} pieces of work match your filters, but none are ${g === 'open' ? 'open' : GROUPS.find((x) => x.g === g)?.label.toLowerCase()}.`}
                  </p>
                  <div className="row-gap">
                    {activeFilters.length > 0 && <button className="btn" onClick={() => setParams({ scope: undefined, source: undefined, executor: undefined, updated: undefined, q: undefined })}>Clear filters</button>}
                    {g !== 'all' && <button className="btn btn-quiet" onClick={() => setParams({ g: 'all', w: undefined })}>Show every state</button>}
                  </div>
                </>
              )}
            </div>
          </div>
        )}
      </div>
      <p className="legend" aria-label="Lifetime legend">
        <span><i className="lg-bar" /> executing</span>
        <span><i className="lg-gap" /> waiting, parked or queued</span>
        <span><i className="lg-tick" /> event</span>
        <span className="legend-keys"><Kbd>j</Kbd><Kbd>k</Kbd> move · <Kbd>↵</Kbd> open · <Kbd>x</Kbd> select</span>
      </p>

      {dialog && <ActionDialog work={works.find((w) => w.id === dialog.work.id)!} action={dialog.action} onClose={() => setDialog(null)} />}
      <Dialog open={!!bulk} onClose={() => setBulk(null)} title={bulk === 'hold' ? `Hold ${canHold.length} pieces of work` : `Release ${canRelease.length} pieces of work`}>
        <BulkBody kind={bulk} hold={canHold} release={canRelease} skipped={sel.length - (bulk === 'hold' ? canHold.length : canRelease.length)} onDone={() => (setBulk(null), setSelected(new Set()))} />
      </Dialog>
    </div>
  )
}

function BulkBody({ kind, hold: holdable, release: releasable, skipped, onDone }: { kind: 'hold' | 'release' | null; hold: Work[]; release: Work[]; skipped: number; onDone: () => void }) {
  const [reason, setReason] = useState('')
  if (!kind) return null
  const list = kind === 'hold' ? holdable : releasable
  return (
    <>
      <ul className="bulk-list">
        {list.map((w) => (
          <li key={w.id}>
            <StateMark kind={w.disposition.kind} /> <code>{w.ref}</code> {w.title}
          </li>
        ))}
      </ul>
      {skipped > 0 && <p className="note">{skipped} selected {skipped === 1 ? 'item is' : 'items are'} skipped: {kind === 'hold' ? 'running or finished work cannot be held.' : 'not held.'}</p>}
      {kind === 'hold' && (
        <>
          <p className="note">No turn starts or resumes while held. Events keep queueing, so nothing is lost.</p>
          <label className="field">
            <span>Reason</span>
            <input autoFocus value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. Pausing during the release freeze" />
          </label>
        </>
      )}
      <p className="endpoint">
        <span className="sim-tag">Simulated</span>
        <code>{list.length} × POST /v1/work/…/{kind}</code>
        <span>one request per Work, each with its own expected revision</span>
      </p>
      <footer className="dialog-foot">
        <button className="btn" onClick={onDone}>Cancel</button>
        <button
          className="btn btn-primary"
          onClick={() => {
            list.forEach((w) => (kind === 'hold' ? hold(w.id, reason || 'Bulk hold from the web view') : release(w.id)))
            onDone()
          }}
        >
          {kind === 'hold' ? 'Hold' : 'Release'} {list.length}
        </button>
      </footer>
    </>
  )
}

function RailCell({ on, n, label, blurb, onClick, signal, loading }: { on: boolean; n: number; label: string; blurb: string; onClick: () => void; signal?: boolean; loading?: boolean }) {
  return (
    <button type="button" role="radio" aria-checked={on} className={'rail-cell' + (on ? ' is-on' : '') + (signal && n > 0 ? ' is-signal' : '')} onClick={onClick}>
      <span className="rail-n">{loading ? '–' : n}</span>
      <span className="rail-label">{label}</span>
      <span className="rail-blurb">{blurb}</span>
    </button>
  )
}

function Facet({ label, value, options, onChange, anyLabel = 'all', noAny }: { label: string; value: string; options: { v: string; label: string }[]; onChange: (v: string) => void; anyLabel?: string; noAny?: boolean }) {
  return (
    <label className={'facet' + (value && !noAny ? ' is-set' : '')}>
      <span className="facet-label">{label}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        {!noAny && <option value="">{anyLabel}</option>}
        {options.map((o) => (
          <option key={o.v} value={o.v}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  )
}

function Row({ w, now, index, focused, selected, onToggle, onAction, onFocus }: { w: Work; now: number; index: number; focused: boolean; selected: boolean; onToggle: () => void; onAction: (a: ActionId) => void; onFocus: () => void }) {
  const active = workActive(w, now)
  const elapsed = workElapsed(w, now)
  const live = w.turns.some((t) => t.state === 'running')
  const animating = useStore((s) => !!s.animating[w.id])
  const g = w.disposition.group
  return (
    <div role="row" data-row={index} className={'ledger-row g-' + g + (focused ? ' is-cursor' : '') + (selected ? ' is-selected' : '')} onFocus={onFocus} aria-selected={selected}>
      <span role="cell" className="c-sel">
        <input type="checkbox" checked={selected} onChange={onToggle} aria-label={`Select ${w.ref} ${w.title}`} />
      </span>
      <span role="cell" className="c-work">
        <a className="row-link" href={workHref(w.id)}>
          <span className="w-ref">{w.ref === 'thread' ? sourceLabel(w.source) : w.ref}</span>
          <span className="w-title"><Inline text={w.title} /></span>
        </a>
        <span className="w-meta">
          {w.scopeId}
          {w.requester && <> · {w.requester}</>}
          {w.fixture && <span className="fx" title="Constructed to show a state the local daemon hasn't produced">fixture</span>}
        </span>
      </span>
      <span role="cell" className="c-now">
        <span className={'now-label k-' + w.disposition.kind}>
          <StateMark kind={w.disposition.kind} />
          {w.disposition.label}
          <span className="now-since">{g === 'closed' ? since(w.disposition.since, now) : 'for ' + dur(now - new Date(w.disposition.since).getTime())}</span>
        </span>
        <span className="now-detail">{w.disposition.detail}</span>
      </span>
      <span role="cell" className="c-life">
        <Lifeline work={w} now={now} />
      </span>
      <span role="cell" className="c-turns">
        {w.turns.length || '—'}
        {live && <span className={'live-tag' + (animating ? ' is-ticking' : '')}>live</span>}
      </span>
      <span role="cell" className="c-time">
        <span className="t-active">{active ? dur(active, true) : '—'}</span>
        <span className="t-of">of {dur(elapsed)}</span>
        <Ratio active={active} elapsed={elapsed} />
      </span>
      <span role="cell" className="c-upd">{since(w.updatedAt, now)}</span>
      <span role="cell" className="c-act">
        {w.disposition.kind === 'decision' ? (
          <a className="btn btn-signal btn-xs" href={workHref(w.id)}>
            Decide
          </a>
        ) : w.disposition.kind === 'repair' && actionsFor(w).find((a) => a.id === 'retry' && a.enabled) ? (
          <button className="btn btn-xs" onClick={() => onAction('retry')}>
            Run again
          </button>
        ) : w.pinnedBinding ? (
          <button className="btn btn-xs" onClick={() => onAction('rebind')}>
            Rebind
          </button>
        ) : null}
        <ActionMenu work={w} onPick={onAction} />
      </span>
    </div>
  )
}

function Skeleton() {
  return (
    <div className="skeleton" aria-busy="true" aria-label="Loading work">
      {Array.from({ length: 9 }, (_, i) => (
        <div key={i} className="skel-row" style={{ animationDelay: i * 60 + 'ms' }}>
          <span style={{ width: 90 + ((i * 37) % 160) }} />
          <span style={{ width: 120 + ((i * 53) % 90) }} />
          <span style={{ width: '18%' }} />
        </div>
      ))}
    </div>
  )
}

function FreshInstall() {
  return (
    <div className="page">
      <div className="fresh">
        <div className="fresh-copy">
          <h1>Let's connect your first source of work</h1>
          <p className="fresh-state">The daemon is running, but nothing is configured yet.</p>
          <p>
            Open Conduit watches the places work turns up — GitHub issues, Slack threads, schedules — and gives each item to an agent or command, coming back to it whenever something new happens. The setup assistant looks at this machine, proposes a configuration and applies it only when you approve.
          </p>
          <div className="row-gap">
            <a className="btn btn-primary btn-lg" href="#/setup">
              Start the setup assistant
            </a>
            <a className="btn btn-quiet" href="https://open-conduit.github.io/open-conduit/start/quick-start/" target="_blank" rel="noreferrer">
              Set up by hand instead
            </a>
          </div>
        </div>
        <ol className="fresh-steps">
          <li><b>Look around</b> Find agents, CLI logins and checkouts on this machine.</li>
          <li><b>Decide together</b> Which repositories or channels, which account, how agents run.</li>
          <li><b>Review the change set</b> Every config change is shown before it's applied.</li>
          <li><b>Prove it works</b> Validate, run doctor, and trace a test event.</li>
        </ol>
      </div>
    </div>
  )
}
