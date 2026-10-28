// A scope's routing as a flow chart. Events enter from the scope's sources
// and meet route 1. A match goes right: handler → binding → executor and
// workspace. No match falls down to the next route, and past the last one
// to "unmatched". Every node of a route shares one grid row with it, so a
// route always lines up with what it sends work to.

import type { CSSProperties } from 'react'
import type { ScopeConfig, BindingConfig, Executor } from '../domain/types'
import type { TraceSample } from '../data/fixtures/config'
import { sourceLabel } from '../lib/labels'
import { policyWords } from './WorkDetail'

type Verdict = 'match' | 'skip' | 'not-reached'

const at = (row: number, col: number): CSSProperties => ({ ['--r' as string]: row, ['--c' as string]: col })

export function FlowMap({ s, executors, trace, onBinding }: { s: ScopeConfig; executors: Executor[]; trace?: TraceSample; onBinding: (name: string) => void }) {
  const sources = s.extensions.filter((e) => e.ingress !== false)
  const verdict = (route: string): Verdict | undefined => trace?.steps.find((x) => x.route === route)?.verdict
  const unmatched = !!trace && trace.steps.every((x) => x.verdict !== 'match')
  // Rows: route i at 2i+1, its fall-through at 2i+2; "unmatched" after the last.
  const last = s.routes.length * 2 + 1

  return (
    <div className={'flow' + (trace ? ' is-tracing' : '')} role="list" aria-label="Routing, in order">
      <div className="flow-head" style={at(0, 1)}>Events come from</div>
      <div className="flow-head" style={at(0, 3)}>Routes, in order · first match wins</div>
      <div className="flow-head" style={at(0, 5)}>Binding</div>
      <div className="flow-head" style={at(0, 7)}>Runs on</div>

      <div className={'node node-source' + (trace ? ' is-lit' : '')} style={at(1, 1)}>
        {sources.length === 0 && <span className="quiet">No sources: only operator-posted events reach this scope.</span>}
        {sources.map((e) => (
          <div key={e.name} className="src">
            <b>{sourceLabel(e.type)}</b> <span className="mono">{e.name}</span>
            {e.connection && <small>via {e.connection}</small>}
            {e.summary && <p>{e.summary}</p>}
          </div>
        ))}
      </div>
      <Edge style={at(1, 2)} lit={!!trace} top="event" />

      {s.routes.map((r, i) => {
        const row = i * 2 + 1
        const v = verdict(r.name)
        const b = s.bindings.find((x) => x.name === r.binding)
        const x = executors.find((e) => e.name === b?.executor)
        const shared = s.routes.findIndex((o) => o.binding === r.binding) < i
        return (
          <div key={r.name} className={'flow-row' + (v ? ' v-' + v : '')} role="listitem" style={{ display: 'contents' }}>
            <div className="node node-route" style={at(row, 3)}>
              <div className="node-top">
                <span className="route-ord">{i + 1}</span>
                <b className="mono">{r.name}</b>
                <small>handler {r.handler}</small>
              </div>
              {r.matches && <p>{r.matches}</p>}
              {v && <p className={'verdict v-' + v}>{v === 'match' ? 'Matches' : v === 'skip' ? 'Skipped' : 'Not reached'} — {trace!.steps.find((t) => t.route === r.name)!.reason}</p>}
            </div>
            <Edge style={at(row, 4)} lit={v === 'match'} top="match" bottom={`if busy: ${r.activePolicy}`} title={policyWords(r.activePolicy)} />
            <button type="button" className="node node-binding" style={at(row, 5)} onClick={() => onBinding(r.binding)} aria-label={`Binding ${r.binding}, show details`}>
              <div className="node-top">
                <b className="mono">{r.binding}</b>
                {shared && <small>same as above</small>}
              </div>
              {b ? <BindingCaps b={b} /> : <p className="warn">Binding not found in this scope</p>}
            </button>
            <Edge style={at(row, 6)} lit={v === 'match'} />
            <div className="node node-exec" style={at(row, 7)}>
              <div className="node-top">
                <b>{b?.executor ?? '—'}</b>
                {x && <small>{x.kind}</small>}
              </div>
              {b && <Workspace b={b} />}
            </div>
            <Fall style={at(row + 1, 3)} lit={v === 'skip'} />
          </div>
        )
      })}

      <div className={'node node-none' + (unmatched ? ' is-lit' : '')} style={at(last, 3)}>
        <b>No route matched</b>
        <p>The event is still accepted and recorded, then checkpointed as unmatched. <code>open-conduit route</code> explains it.</p>
      </div>
    </div>
  )
}

function BindingCaps({ b }: { b: BindingConfig }) {
  const caps = (['inject', 'resume', 'interrupt', 'workspace'] as const).filter((k) => b.requires[k])
  return (
    <p className="caps">
      {b.requires.toolbelt && <span>{b.requires.toolbelt} toolbelt</span>}
      {caps.map((c) => (
        <span key={c}>{c}</span>
      ))}
      {b.operations.length > 0 && <span>{b.operations.length} operations</span>}
    </p>
  )
}

function Workspace({ b }: { b: BindingConfig }) {
  const model = b.config.model ? String(b.config.model) : ''
  const checkout = b.resource ? String(b.resource.config.checkout ?? '').replace(/^\/Users\/[^/]+/, '~') : ''
  return (
    <p className="ws">
      {model && <span>model {model}</span>}
      {b.resource ? <span>{checkout ? `worktree of ${checkout}` : `${b.resource.provider} workspace`}</span> : b.requires.workspace ? <span>workspace</span> : <span>no workspace</span>}
      {b.timeoutSeconds && <span>{b.timeoutSeconds / 60}m deadline</span>}
    </p>
  )
}

function Edge({ style, lit, top, bottom, title }: { style: CSSProperties; lit?: boolean; top?: string; bottom?: string; title?: string }) {
  return (
    <div className={'edge' + (lit ? ' is-lit' : '')} style={style} title={title} aria-hidden>
      {top && <span className="edge-label">{top}</span>}
      <span className="edge-line" />
      {bottom && <span className="edge-label edge-label-b">{bottom}</span>}
    </div>
  )
}

function Fall({ style, lit }: { style: CSSProperties; lit?: boolean }) {
  return (
    <div className={'fall' + (lit ? ' is-lit' : '')} style={style} aria-hidden>
      <span className="fall-line" />
      <span className="edge-label">no match → next</span>
    </div>
  )
}
