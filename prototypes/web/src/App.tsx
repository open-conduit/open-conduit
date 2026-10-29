import { useEffect, useMemo, useRef, useState } from 'react'
import { useStore, load, setSource, setTheme } from './data/store'
import { useRoute, go, workHref } from './lib/router'
import { since, useNow } from './lib/time'
import { Toasts, Kbd, Menu, MenuItem, MenuRule, Dialog } from './ui/controls'
import { StateMark } from './ui/marks'
import { RunsView } from './views/Runs'
import { WorkView } from './views/WorkDetail'
import { AttentionView } from './views/Attention'
import { ConfigView } from './views/Config'
import { SetupView } from './views/Setup'
import type { Condition } from './data/adapter'
import type { DispositionKind } from './domain/types'

export function App() {
  const route = useRoute()
  const theme = useStore((s) => s.theme)
  const status = useStore((s) => s.status)
  const attention = useStore((s) => s.works.filter((w) => w.disposition.group === 'attention').length)
  const [palette, setPalette] = useState(false)
  const [help, setHelp] = useState(false)

  useEffect(() => {
    document.documentElement.dataset.theme = theme
  }, [theme])
  useEffect(() => void load(), [])

  // Global keys: g-chords for navigation, ⌘K for jump, ? for help.
  useEffect(() => {
    let chord = false
    let t: ReturnType<typeof setTimeout>
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement
      const typing = el.closest('input, textarea, select, [contenteditable]')
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPalette(true)
        return
      }
      if (typing || e.metaKey || e.ctrlKey || e.altKey || document.querySelector('dialog[open]')) return
      if (chord) {
        chord = false
        const dest = { r: '#/runs', a: '#/attention', c: '#/config', s: '#/setup' }[e.key]
        if (dest) {
          e.preventDefault()
          go(dest)
        }
        return
      }
      if (e.key === 'g') {
        chord = true
        clearTimeout(t)
        t = setTimeout(() => (chord = false), 900)
      } else if (e.key === '?') setHelp(true)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  useEffect(() => {
    const names = { runs: 'Runs', work: 'Work', attention: 'Needs you', config: 'Configuration', setup: 'Setup' }
    document.title = `${attention ? `(${attention}) ` : ''}${names[route.view]} · Open Conduit`
  }, [route.view, attention])

  const nav = [
    { view: 'runs', href: '#/runs', label: 'Runs', key: 'r' },
    { view: 'attention', href: '#/attention', label: 'Needs you', key: 'a', count: attention },
    { view: 'config', href: '#/config', label: 'Configuration', key: 'c' },
    { view: 'setup', href: '#/setup', label: 'Setup', key: 's' },
  ] as const

  return (
    <div className="app">
      <a href="#main" className="skip">Skip to content</a>
      <header className="bar">
        <a className="wordmark" href="#/runs" aria-label="Open Conduit — Runs">
          <Logo />
          <span>
            Open<b>Conduit</b>
          </span>
        </a>
        <nav className="nav" aria-label="Primary">
          {nav.map((n) => {
            const on = route.view === n.view || (n.view === 'runs' && route.view === 'work')
            return (
              <a key={n.view} href={n.href} className={'nav-item' + (on ? ' is-on' : '')} aria-current={on ? 'page' : undefined} title={`g then ${n.key}`}>
                {n.label}
                {'count' in n && n.count > 0 && <span className="nav-count" aria-label={`${n.count} items`}>{n.count}</span>}
              </a>
            )
          })}
        </nav>
        <div className="bar-tools">
          <button type="button" className="jump" onClick={() => setPalette(true)}>
            <span>Jump to work…</span>
            <Kbd>⌘K</Kbd>
          </button>
          <button type="button" className="btn btn-quiet btn-icon" onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')} aria-label={`Switch to ${theme === 'dark' ? 'light' : 'dark'} theme`} title="Theme">
            {theme === 'dark' ? '☀' : '☾'}
          </button>
        </div>
      </header>

      <main id="main" className="main" tabIndex={-1}>
        {status === 'error' ? <Unreachable /> : route.view === 'work' ? <WorkView id={route.id!} /> : route.view === 'attention' ? <AttentionView /> : route.view === 'config' ? <ConfigView scopeId={route.id} /> : route.view === 'setup' ? <SetupView /> : <RunsView />}
      </main>

      <StatusLine onHelp={() => setHelp(true)} />
      <Toasts />
      <Palette open={palette} onClose={() => setPalette(false)} />
      <Dialog open={help} onClose={() => setHelp(false)} title="Keyboard">
        <dl className="keys">
          <dt><Kbd>⌘K</Kbd></dt><dd>Jump to any work, scope or page</dd>
          <dt><Kbd>g</Kbd> <Kbd>r</Kbd> / <Kbd>a</Kbd> / <Kbd>c</Kbd> / <Kbd>s</Kbd></dt><dd>Runs, Needs you, Configuration, Setup</dd>
          <dt><Kbd>/</Kbd></dt><dd>Search runs</dd>
          <dt><Kbd>j</Kbd> <Kbd>k</Kbd></dt><dd>Move through runs, or through turns on a run</dd>
          <dt><Kbd>Enter</Kbd></dt><dd>Open the focused run</dd>
          <dt><Kbd>x</Kbd></dt><dd>Select the focused run for bulk actions</dd>
          <dt><Kbd>[</Kbd> <Kbd>]</Kbd></dt><dd>Previous / next turn</dd>
          <dt><Kbd>e</Kbd></dt><dd>On a run: jump to the first error</dd>
          <dt><Kbd>Esc</Kbd></dt><dd>Close dialogs and menus; back to Runs from a run</dd>
        </dl>
      </Dialog>
    </div>
  )
}

function Logo() {
  return (
    <svg width="22" height="22" viewBox="0 0 32 32" aria-hidden>
      <rect width="32" height="32" rx="5" fill="var(--ink)" />
      <path d="M7 11h11a4 4 0 0 1 0 8h-4" stroke="var(--signal)" strokeWidth="3" fill="none" strokeLinecap="round" />
      <circle cx="10" cy="19" r="2.5" fill="var(--paper)" />
    </svg>
  )
}

function Unreachable() {
  const error = useStore((s) => s.error)
  return (
    <div className="page">
      <div className="state-page">
        <StateMark kind="failed" size={18} />
        <h1>The daemon isn't answering</h1>
        <p>Open Conduit's web view reads everything from the local daemon's management socket, and it didn't respond.</p>
        <pre className="code-block">{error}</pre>
        <ol className="steps">
          <li>Check whether it runs: <code>open-conduit health</code></li>
          <li>If it's installed as a service: <code>open-conduit service status</code> and the end of its log</li>
          <li>Otherwise start it: <code>open-conduit serve</code></li>
        </ol>
        <div className="row-gap">
          <button className="btn btn-primary" onClick={() => void load()}>Try again</button>
          <button className="btn" onClick={() => setSource('fixture', 'normal')}>Back to the healthy prototype</button>
        </div>
      </div>
    </div>
  )
}

function StatusLine({ onHelp }: { onHelp: () => void }) {
  const h = useStore((s) => s.health)
  const cfg = useStore((s) => s.config)
  const source = useStore((s) => s.source)
  const condition = useStore((s) => s.condition)
  const receipts = useStore((s) => s.receipts.length)
  const running = useStore((s) => s.works.filter((w) => w.turns.some((t) => t.state === 'running' && t.executor === 'claude-code')).length)
  const status = useStore((s) => s.status)
  const now = useNow()
  const conds: { c: Condition; label: string; hint: string }[] = [
    { c: 'normal', label: 'Healthy daemon', hint: 'the full fixture world' },
    { c: 'slow', label: 'Slow daemon', hint: 'loading states' },
    { c: 'unreachable', label: 'Daemon unreachable', hint: 'error state' },
    { c: 'fresh', label: 'Fresh installation', hint: 'no configuration yet' },
  ]
  return (
    <footer className="status" aria-label="System status">
      <span className="st-item">
        <span className={'st-dot ' + (status === 'error' ? 'is-down' : status === 'loading' ? 'is-wait' : 'is-up')} aria-hidden />
        {status === 'error' ? 'daemon unreachable' : status === 'loading' ? 'connecting…' : `daemon ${h?.state} · v${h?.version}`}
      </span>
      {h && status === 'ready' && (
        <>
          <a className="st-item" href="#/config">generation {h.generation || '—'}</a>
          <span className="st-item" title="settings.concurrency">
            agent slots <b>{running}</b>/{h.agentSlots.total}
          </span>
          <span className="st-item st-wide" title="settings.job_concurrency">
            job slots {h.jobSlots.used}/{h.jobSlots.total}
          </span>
          <span className="st-item st-wide">webhooks · {h.tunnel}</span>
          {h.lastReconcile && <span className="st-item st-wide">reconciled {since(h.lastReconcile, now)}</span>}
        </>
      )}
      <span className="st-spacer" />
      {receipts > 0 && <span className="st-item st-wide">{receipts} simulated call{receipts > 1 ? 's' : ''}</span>}
      <Menu
        label="Prototype data"
        className="st-menu"
        align="end"
        trigger={
          <span className="st-proto">
            <span className="sim-tag">Prototype</span>
            {source === 'live' ? 'live · read-only' : condition === 'normal' ? 'fixture data' : conds.find((c) => c.c === condition)?.label.toLowerCase()}
          </span>
        }
      >
        {(close) => (
          <>
            {conds.map((c) => (
              <MenuItem key={c.c} hint={c.hint} onSelect={() => (close(), setSource('fixture', c.c))}>
                {source === 'fixture' && condition === c.c ? '● ' : ''}
                {c.label}
              </MenuItem>
            ))}
            <MenuRule />
            <MenuItem hint="GET only, through the dev server" onSelect={() => (close(), setSource('live'))}>
              {source === 'live' ? '● ' : ''}Local daemon (read-only)
            </MenuItem>
          </>
        )}
      </Menu>
      <button type="button" className="st-item st-help" onClick={onHelp} aria-label="Keyboard shortcuts">
        <Kbd>?</Kbd>
      </button>
      {cfg && !cfg.fileMatchesGeneration && cfg.generation > 0 && <span className="st-item st-warn">config file differs from the active generation</span>}
    </footer>
  )
}

function Palette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const works = useStore((s) => s.works)
  const scopes = useStore((s) => s.config?.scopes ?? [])
  const [q, setQ] = useState('')
  const [i, setI] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (open) {
      setQ('')
      setI(0)
      setTimeout(() => input.current?.focus(), 0)
    }
  }, [open])
  const items = useMemo(() => {
    const pages = [
      { key: 'p-runs', label: 'Runs', hint: 'page', href: '#/runs' },
      { key: 'p-att', label: 'Needs you', hint: 'page', href: '#/attention' },
      { key: 'p-cfg', label: 'Configuration', hint: 'page', href: '#/config' },
      { key: 'p-setup', label: 'Setup assistant', hint: 'page', href: '#/setup' },
      { key: 'p-trace', label: 'Trace how an event would route', hint: 'configuration', href: '#/config?trace=1' },
    ]
    const ws = works.map((w) => ({ key: w.id, label: `${w.ref === 'thread' ? '' : w.ref + ' '}${w.title}`, hint: `${w.scopeId} · ${w.disposition.label.toLowerCase()}`, href: workHref(w.id), mark: w.disposition.kind }))
    const ss = scopes.map((s) => ({ key: 's-' + s.id, label: s.name, hint: 'scope', href: '#/config/' + s.id }))
    const all: { key: string; label: string; hint: string; href: string; mark?: DispositionKind }[] = [...ws, ...ss, ...pages]
    const t = q.trim().toLowerCase()
    return (t ? all.filter((x) => (x.label + ' ' + x.hint).toLowerCase().includes(t)) : all).slice(0, 12)
  }, [q, works, scopes])
  const pick = (href: string) => {
    onClose()
    go(href)
  }
  return (
    <Dialog open={open} onClose={onClose} title="Jump to">
      <input
        ref={input}
        className="palette-input"
        value={q}
        onChange={(e) => (setQ(e.target.value), setI(0))}
        placeholder="Issue number, title, scope or page"
        aria-label="Search"
        aria-controls="palette-list"
        aria-activedescendant={items[i] ? 'pal-' + i : undefined}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown') (e.preventDefault(), setI((v) => Math.min(v + 1, items.length - 1)))
          if (e.key === 'ArrowUp') (e.preventDefault(), setI((v) => Math.max(v - 1, 0)))
          if (e.key === 'Enter' && items[i]) pick(items[i].href)
        }}
      />
      <ul className="palette-list" id="palette-list" role="listbox" aria-label="Results">
        {items.map((x, n) => (
          <li key={x.key} id={'pal-' + n} role="option" aria-selected={n === i} className={n === i ? 'is-on' : ''} onMouseEnter={() => setI(n)} onClick={() => pick(x.href)}>
            {x.mark ? <StateMark kind={x.mark} /> : <span className="pal-dot" />}
            <span className="pal-label">{x.label}</span>
            <span className="pal-hint">{x.hint}</span>
          </li>
        ))}
        {items.length === 0 && <li className="pal-empty">Nothing matches “{q}”</li>}
      </ul>
    </Dialog>
  )
}
