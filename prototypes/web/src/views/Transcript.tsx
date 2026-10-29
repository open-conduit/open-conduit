import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import type { TranscriptItem } from '../domain/types'
import { stamp, dur } from '../lib/time'
import { StateMark } from '../ui/marks'

type Tool = Extract<TranscriptItem, { kind: 'tool' }>
type Block = { kind: 'item'; item: TranscriptItem } | { kind: 'tools'; id: string; items: Tool[] } | { kind: 'systems'; id: string; items: Extract<TranscriptItem, { kind: 'system' }>[] }

export type Filter = 'conversation' | 'tools' | 'effects' | 'system'

/** Routine tool calls collapse into groups; everything consequential stays visible. */
function blocks(items: TranscriptItem[]): Block[] {
  const out: Block[] = []
  for (const it of items) {
    const last = out[out.length - 1]
    if (it.kind === 'tool') {
      if (last?.kind === 'tools') last.items.push(it)
      else out.push({ kind: 'tools', id: 'g-' + it.id, items: [it] })
    } else if (it.kind === 'system') {
      if (last?.kind === 'systems') last.items.push(it)
      else out.push({ kind: 'systems', id: 's-' + it.id, items: [it] })
    } else out.push({ kind: 'item', item: it })
  }
  return out
}

export function Transcript({ items, live, filters, query, expandAll, jumpTo, hideFinalOutcome }: { items: TranscriptItem[]; live: boolean; filters: Set<Filter>; query: string; expandAll: number; jumpTo: number; hideFinalOutcome?: boolean }) {
  const q = query.trim().toLowerCase()
  const visible = useMemo(
    () =>
      items.filter((it, i) => {
        if (hideFinalOutcome && it.kind === 'outcome' && i === items.length - 1) return false
        const f: Filter = it.kind === 'tool' ? 'tools' : it.kind === 'effect' ? 'effects' : it.kind === 'system' ? 'system' : 'conversation'
        if (!filters.has(f) && it.kind !== 'outcome' && it.kind !== 'error' && it.kind !== 'orchestration') return false
        if (!q) return true
        return JSON.stringify(it).toLowerCase().includes(q)
      }),
    [items, filters, q, hideFinalOutcome],
  )
  const bs = useMemo(() => blocks(visible), [visible])
  const end = useRef<HTMLDivElement>(null)
  const root = useRef<HTMLDivElement>(null)
  const [follow, setFollow] = useState(true)
  const count = items.length
  const seen = useRef(count)
  useEffect(() => {
    // Follow only output that arrives while watching, never on first render.
    if (count === seen.current) return
    seen.current = count
    if (live && follow) end.current?.scrollIntoView({ block: 'nearest', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
  }, [count, live, follow])
  useEffect(() => {
    if (!jumpTo) return
    const el = root.current?.querySelector<HTMLElement>('.is-error, .t-error')
    if (el) {
      el.scrollIntoView({ block: 'center' })
      el.querySelector<HTMLElement>('button, summary')?.focus()
    }
  }, [jumpTo])

  if (!items.length) return <p className="tx-empty">{live ? 'Waiting for the first output from the executor…' : 'This turn produced no transcript. Executors that are not agents report only their outcome.'}</p>
  return (
    <div className="transcript" ref={root}>
      {bs.map((b) =>
        b.kind === 'tools' ? (
          b.items.length >= 3 && !q ? (
            <ToolGroup key={b.id + expandAll} items={b.items} openDefault={expandAll > 0 && expandAll % 2 === 1} />
          ) : (
            b.items.map((t) => <ToolRow key={t.id + expandAll} t={t} openDefault={expandAll % 2 === 1 || (!!q && JSON.stringify(t).toLowerCase().includes(q))} />)
          )
        ) : b.kind === 'systems' ? (
          <div key={b.id} className="t-system">
            {b.items.map((s) => (
              <div key={s.id}>
                <time>{stamp(s.at)}</time>
                <span>{s.text}</span>
                {s.detail && <span className="t-sys-detail">{s.detail}</span>}
              </div>
            ))}
          </div>
        ) : (
          <Item key={b.item.id + expandAll} it={b.item} expand={expandAll % 2 === 1} />
        ),
      )}
      {live && (
        <div className="t-live" ref={end}>
          <span className="t-live-dot" aria-hidden />
          Running · new output appears here
          <label className="t-follow">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> follow
          </label>
        </div>
      )}
    </div>
  )
}

function Item({ it, expand }: { it: TranscriptItem; expand: boolean }) {
  switch (it.kind) {
    case 'prompt':
      return (
        <details className="c-act c-prompt" open={expand}>
          <summary>
            <span className="c-act-glyph" aria-hidden>↘</span>
            <span className="c-act-what">Input to the executor</span>
            <span className="c-act-label">{it.title}</span>
            <time>{stamp(it.at)}</time>
          </summary>
          <pre className="t-pre">{it.text}</pre>
        </details>
      )
    case 'agent':
      return (
        <div className="c-msg from-agent">
          <span className="c-avatar is-agent" aria-hidden />
          <div className="c-body">
            <div className="c-meta">
              <b>Agent</b>
              <time>{stamp(it.at)}</time>
            </div>
            <div className="c-bubble">
              <Prose text={it.text} />
            </div>
          </div>
        </div>
      )
    case 'human':
      return (
        <div className={'c-msg from-person' + (it.injected ? ' is-injected' : '')}>
          <div className="c-body">
            <div className="c-meta">
              <b>{it.author}</b>
              <span>{it.via}</span>
              {it.injected && <span className="c-inj">injected while running</span>}
              <time>{stamp(it.at)}</time>
            </div>
            <div className="c-bubble">
              <Prose text={it.text} />
            </div>
          </div>
          <span className="c-avatar" aria-hidden>
            {it.author.replace(/^operator:/, '').slice(0, 1).toUpperCase()}
          </span>
        </div>
      )
    case 'effect':
      return <Effect e={it} openDefault={expand} />
    case 'orchestration':
      return (
        <div className={'t-orch tone-' + it.tone} role="separator" aria-label={it.text}>
          <span className="t-orch-line" />
          <span className="t-orch-text">
            <OrchGlyph tone={it.tone} />
            {it.text}
            <time>{stamp(it.at)}</time>
          </span>
          <span className="t-orch-line" />
        </div>
      )
    case 'outcome':
      return (
        <div className={'t-outcome o-' + it.outcome}>
          <Prose text={it.summary} />
          <p className="t-meta">
            Outcome {it.outcome}
            {it.status && <> · status {it.status}</>}
          </p>
        </div>
      )
    case 'error':
      return (
        <div className="t-error" tabIndex={-1}>
          <div className="t-error-head">
            <StateMark kind="failed" /> {it.text}
          </div>
          {it.detail && <Code text={it.detail} lines={10} bad />}
        </div>
      )
    default:
      return null
  }
}

function OrchGlyph({ tone }: { tone: string }) {
  const k = tone === 'park' ? 'parked' : tone === 'wake' ? 'running' : tone === 'cancel' ? 'cancelled' : tone === 'inject' ? 'decision' : 'queued'
  return <StateMark kind={k as 'parked'} size={10} />
}

function ToolGroup({ items, openDefault }: { items: Tool[]; openDefault: boolean }) {
  const failed = items.filter((t) => !t.ok).length
  const [open, setOpen] = useState(openDefault || failed > 0)
  const byTool = Object.entries(items.reduce<Record<string, number>>((a, t) => ((a[t.tool] = (a[t.tool] ?? 0) + 1), a), {}))
  const ms = items.reduce((a, t) => a + t.ms, 0)
  return (
    <div className={'c-act c-group' + (failed ? ' is-error' : '')}>
      <button type="button" className="c-act-row" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        <span className="c-act-glyph" aria-hidden>{open ? '▾' : '▸'}</span>
        <span className="c-act-what">{items.length} tool calls</span>
        <span className="c-act-label">{byTool.map(([k, v]) => `${k} ×${v}`).join(' · ')}</span>
        {failed > 0 && <span className="t-fail-n">{failed} failed</span>}
        {ms >= 1000 && <span className="t-dur">{dur(ms, true)}</span>}
        <time>{stamp(items[0].at)}</time>
      </button>
      {open && (
        <div className="c-group-body">
          {items.map((t) => (
            <ToolRow key={t.id} t={t} openDefault={!t.ok} />
          ))}
        </div>
      )}
    </div>
  )
}

function ToolRow({ t, openDefault }: { t: Tool; openDefault: boolean }) {
  const [open, setOpen] = useState(openDefault)
  const has = !!(t.input || t.output)
  return (
    <div className={'c-act c-tool' + (t.ok ? '' : ' is-error')}>
      <button type="button" className="c-act-row" aria-expanded={has ? open : undefined} onClick={() => has && setOpen((o) => !o)} disabled={!has}>
        <span className="c-act-glyph" aria-hidden>{has ? (open ? '▾' : '▸') : '·'}</span>
        <span className="c-act-what mono">{t.tool}</span>
        <span className="c-act-label mono">{t.label !== t.tool ? t.label : ''}</span>
        {!t.ok && <span className="t-fail-n">failed</span>}
        {t.ms >= 1000 && <span className="t-dur">{dur(t.ms, true)}</span>}
        <time>{stamp(t.at)}</time>
      </button>
      {open && (
        <div className="c-io">
          {t.input && t.input !== t.label && <Code text={t.input} lines={12} label="Input" />}
          {t.output && <Code text={t.output} lines={14} label="Result" bad={!t.ok} />}
        </div>
      )}
    </div>
  )
}

// ---- toolbelt calls: what was said, not the JSON it was said in ----

const CORE = ['read_context', 'emit', 'wait', 'declare_outcome', 'create_work', 'call']

function parse(s?: string): unknown {
  if (!s) return undefined
  try {
    return JSON.parse(s)
  } catch {
    return undefined
  }
}

/** The longest prose string in a request: the message the call carried, whoever's operation it is. */
function proseIn(v: unknown): string | undefined {
  let best: string | undefined
  const walk = (x: unknown) => {
    if (typeof x === 'string') {
      if (x.length > 40 && /\s/.test(x) && !/^[{[]/.test(x.trim()) && (!best || x.length > best.length)) best = x
    } else if (Array.isArray(x)) x.forEach(walk)
    else if (x && typeof x === 'object') Object.values(x).forEach(walk)
  }
  walk(v)
  return best
}

/** Short scalar facts from a response, e.g. state posted · number 131. */
function factsIn(v: unknown): string[] {
  const out: string[] = []
  const walk = (x: unknown, key: string, depth: number) => {
    if (out.length >= 3 || depth > 3) return
    if (x && typeof x === 'object' && !Array.isArray(x)) for (const [k, y] of Object.entries(x)) walk(y, k, depth + 1)
    else if ((typeof x === 'string' && x.length <= 40 && key !== 'schema' && !/[{}"]/.test(x)) || typeof x === 'number' || typeof x === 'boolean') out.push(`${key} ${String(x)}`)
  }
  walk(v, '', 0)
  return out
}

function Effect({ e, openDefault }: { e: Extract<TranscriptItem, { kind: 'effect' }>; openDefault: boolean }) {
  const [open, setOpen] = useState(openDefault)
  const owner = CORE.includes(e.operation) ? '' : e.operation.includes('.') ? e.operation.split('.')[0] : 'extension'
  const said = proseIn(parse(e.input))
  const facts = factsIn(parse(e.output))
  const summary = facts.length ? facts.join(' · ') : e.output && e.output.length <= 60 && !e.output.includes('\n') ? e.output : ''
  const raw = open && (
    <div className="c-io">
      {e.input && <Code text={e.input} lines={12} label="Request" />}
      {e.output && <Code text={e.output} lines={12} label="Response" bad={!e.ok} />}
    </div>
  )
  // A call that carried a message reads as a message the agent sent.
  if (said)
    return (
      <div className="c-msg from-agent is-sent">
        <span className="c-avatar is-agent" aria-hidden />
        <div className="c-body">
          <div className="c-meta">
            <b>Agent</b>
            <span>
              sent via <code>{e.operation}</code>
            </span>
            {e.link && (
              <a href={e.link.url} target="_blank" rel="noreferrer">
                {e.link.label} ↗
              </a>
            )}
            <time>{stamp(e.at)}</time>
          </div>
          <div className="c-bubble">
            <Prose text={said} />
          </div>
          <button type="button" className="c-raw" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
            {summary ? `${summary} · ` : ''}
            {open ? 'hide request' : 'request and response'}
          </button>
          {raw}
        </div>
      </div>
    )
  return (
    <div className={'c-act c-call' + (owner ? ' is-ext' : '') + (e.ok ? '' : ' is-error')}>
      <button type="button" className="c-act-row" aria-expanded={open} onClick={() => setOpen((o) => !o)} disabled={!e.input && !e.output}>
        <span className="c-act-glyph" aria-hidden>{owner ? '◇' : '·'}</span>
        <span className="c-act-what mono">{e.operation}</span>
        <span className="c-act-label">
          {e.label !== e.operation ? e.label : ''}
          {summary && <span className="c-facts"> → {summary}</span>}
        </span>
        {e.link && (
          <a href={e.link.url} target="_blank" rel="noreferrer" className="t-link" onClick={(ev) => ev.stopPropagation()}>
            {e.link.label} ↗
          </a>
        )}
        <time>{stamp(e.at)}</time>
      </button>
      {raw}
    </div>
  )
}

export function Code({ text, lines = 14, label, bad }: { text: string; lines?: number; label?: string; bad?: boolean }) {
  const all = text.split('\n')
  const [full, setFull] = useState(false)
  const shown = full ? all : all.slice(0, lines)
  const diff = all.some((l) => /^[+-]\s|^@@/.test(l))
  return (
    <div className="code">
      {label && <div className="code-label">{label}</div>}
      <pre className="code-block">
        {shown.map((l, i) => (
          <span key={i} className={diff ? (l.startsWith('+') ? 'ln-add' : l.startsWith('-') ? 'ln-del' : l.startsWith('@@') ? 'ln-hunk' : '') : bad && /^(--- )?FAIL|^panic:|\berror:|no such host|exit status [1-9]/i.test(l) ? 'ln-bad' : ''}>
            {l + '\n'}
          </span>
        ))}
      </pre>
      {all.length > lines && (
        <button type="button" className="link-btn code-more" onClick={() => setFull((f) => !f)}>
          {full ? 'Show less' : `Show all ${all.length} lines`}
        </button>
      )}
    </div>
  )
}

/** Light formatting for messages: paragraphs, line breaks, `code`, **bold** and *bold* (chat markup). */
export function Prose({ text }: { text: string }): ReactNode {
  const inline = (line: string, k: string) =>
    line.split(/(`[^`]+`|\*\*[^*]+\*\*|\*[^*\s][^*]*\*)/).map((s, j) =>
      s.startsWith('`') && s.endsWith('`') && s.length > 2 ? (
        <code key={k + j}>{s.slice(1, -1)}</code>
      ) : s.startsWith('*') && s.endsWith('*') && s.length > 2 ? (
        <b key={k + j}>{s.replace(/^\*\*?|\*\*?$/g, '')}</b>
      ) : (
        s
      ),
    )
  return (
    <div className="prose">
      {text.split(/\n{2,}/).map((p, i) => (
        <p key={i}>
          {p.split('\n').map((line, j, all) => (
            <span key={j}>
              {inline(line, `${i}-${j}-`)}
              {j < all.length - 1 && <br />}
            </span>
          ))}
        </p>
      ))}
    </div>
  )
}
