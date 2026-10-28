import { useState } from 'react'
import type { ScopeConfig, BindingConfig } from '../domain/types'
import { useStore, reconcile } from '../data/store'
import { useRoute, setParams } from '../lib/router'
import { since, useNow } from '../lib/time'
import { traceSamples, type TraceSample } from '../data/fixtures/config'
import { StateMark } from '../ui/marks'
import { FlowMap } from './FlowMap'
import { sourceLabel } from '../lib/labels'
import { ExtensionPanels, ActionControl, providerName } from '../ui/extensions'


export function ConfigView({ scopeId }: { scopeId?: string }) {
  const cfg = useStore((s) => s.config)
  const works = useStore((s) => s.works)
  const status = useStore((s) => s.status)
  const now = useNow()
  if (status === 'loading' || !cfg) return <div className="page"><p className="loading-line">Reading configuration…</p></div>
  if (cfg.generation === 0)
    return (
      <div className="page">
        <div className="state-page">
          <h1>No configuration yet</h1>
          <p>The daemon is running without a configuration generation, so nothing is watched and nothing can route.</p>
          <a className="btn btn-primary" href="#/setup">Start the setup assistant</a>
        </div>
      </div>
    )
  const scope = cfg.scopes.find((s) => s.id === scopeId)
  const active = (id: string) => works.filter((w) => w.scopeId === id && w.disposition.group !== 'closed').length

  return (
    <div className="page config">
      <header className="page-head">
        <div>
          <h1>Configuration</h1>
          <p className="page-sub">
            Generation <b>{cfg.generation}</b>, applied {since(cfg.appliedAt, now)} from <code>{cfg.path}</code>.{' '}
            {cfg.fileMatchesGeneration ? 'The file on disk matches it.' : <span className="sub-signal">The file on disk differs from the active generation.</span>}
          </p>
        </div>
        <div className="head-note">
          <b>Read-only here.</b> Change configuration with the <a href="#/setup">setup assistant</a>, which proposes each change for your approval, or <code>open-conduit config set</code>.
        </div>
      </header>

      <div className="cfg-layout">
        <nav className="cfg-nav" aria-label="Scopes">
          <a href="#/config" className={!scope ? 'is-on' : ''} aria-current={!scope ? 'page' : undefined}>
            <span>Overview</span>
          </a>
          <div className="cfg-nav-k">Scopes</div>
          {cfg.scopes.map((s) => (
            <a key={s.id} href={'#/config/' + s.id} className={scope?.id === s.id ? 'is-on' : ''} aria-current={scope?.id === s.id ? 'page' : undefined}>
              <span>{s.name}</span>
              <small>
                {sourceLabel(s.source)} · {active(s.id)} open
              </small>
            </a>
          ))}
          <a href="#/setup" className="cfg-add">+ Add a scope with the assistant</a>
        </nav>
        <div className="cfg-main">{scope ? <ScopePage s={scope} /> : <Overview />}</div>
      </div>
    </div>
  )
}

function Overview() {
  const cfg = useStore((s) => s.config)!
  const works = useStore((s) => s.works)
  return (
    <>
      <section className="cfg-sec">
        <h2 className="section-rule">How work flows through this installation</h2>
        <p className="quiet">Each scope is an independent boundary: its sources produce events, its routes decide what each event does, and its bindings say which executor does the work. Read each row left to right.</p>
        <div className="matrix" role="table" aria-label="Scopes">
          <div className="mx-head" role="row">
            <span role="columnheader">Scope</span>
            <span role="columnheader">Events come from</span>
            <span role="columnheader">Routes, in order</span>
            <span role="columnheader">Work goes to</span>
            <span role="columnheader">Extensions offer</span>
          </div>
          {cfg.scopes.map((s) => (
            <a key={s.id} role="row" className="mx-row" href={'#/config/' + s.id}>
              <span role="cell" className="mx-scope">
                <b>{s.name}</b>
                <small>
                  {works.filter((w) => w.scopeId === s.id && w.disposition.group !== 'closed').length} open · {s.id}
                  {s.fixture && <span className="fx">fixture</span>}
                </small>
              </span>
              <span role="cell">
                {s.extensions
                  .filter((e) => e.ingress !== false)
                  .map((e) => (
                    <span key={e.name} className="mx-chip">
                      {sourceLabel(e.type)} <small>{e.name}</small>
                    </span>
                  ))}
              </span>
              <span role="cell" className="mx-routes">
                {s.routes.map((r, i) => (
                  <span key={r.name}>
                    <i>{i + 1}</i> {r.name} <small>{r.activePolicy}</small>
                  </span>
                ))}
              </span>
              <span role="cell">
                {[...new Set(s.routes.map((r) => r.binding))].map((b) => {
                  const bd = s.bindings.find((x) => x.name === b)
                  return (
                    <span key={b} className="mx-chip">
                      {b} <small>→ {bd?.executor}</small>
                    </span>
                  )
                })}
              </span>
              <span role="cell">
                <ScopeOffers s={s} />
              </span>
            </a>
          ))}
        </div>
      </section>

      <div className="cfg-cols">
        <section className="cfg-sec">
          <h2 className="section-rule">Connections</h2>
          <ul className="kv-list">
            {cfg.connections.map((c) => (
              <li key={c.id}>
                <StateMark kind={c.status === 'ok' ? 'completed' : 'failed'} />
                <b>{c.id}</b>
                <span>{c.detail}</span>
                <small>
                  secrets: {c.secretNames.join(', ')} · <span className="redacted">reference redacted</span>
                </small>
              </li>
            ))}
          </ul>
        </section>
        <section className="cfg-sec">
          <h2 className="section-rule">Executors on this machine</h2>
          <ul className="kv-list">
            {cfg.executors.map((x) => (
              <li key={x.name}>
                <StateMark kind={x.available ? 'completed' : 'failed'} />
                <b>{x.name}</b>
                <span>{x.detail}</span>
                <small>
                  {(['inject', 'resume', 'interrupt', 'workspace'] as const)
                    .filter((k) => x.capabilities[k])
                    .join(' · ') || 'run only'}
                </small>
              </li>
            ))}
          </ul>
        </section>
        <section className="cfg-sec">
          <h2 className="section-rule">Settings</h2>
          <dl className="kv">
            <dt>Agent turns at once</dt>
            <dd>{cfg.settings.concurrency} <code>settings.concurrency</code></dd>
            <dt>Jobs at once</dt>
            <dd>{cfg.settings.jobConcurrency} <code>settings.job_concurrency</code></dd>
            <dt>Webhooks</dt>
            <dd>
              <code>{cfg.settings.webhooks.addr}</code> via {cfg.settings.webhooks.tunnel}
            </dd>
          </dl>
        </section>
      </div>
    </>
  )
}

function ScopePage({ s }: { s: ScopeConfig }) {
  const { params } = useRoute()
  const samples = traceSamples.filter((t) => t.scopeId === s.id)
  const traceId = params.get('trace')
  const trace = samples.find((t) => t.id === traceId) ?? (traceId ? samples[0] : undefined)
  const [openBinding, setOpenBinding] = useState<string | null>(null)
  const cfg = useStore((x) => x.config)

  return (
    <>
      <header className="scope-head">
        <div>
          <h2>{s.name}</h2>
          <p>{s.description}</p>
        </div>
        <div className="row-gap">
          <button className="btn btn-sm" onClick={() => reconcile(s.id)} title="POST /v1/scopes/{id}/reconcile">
            Reconcile now
          </button>
          <a className="btn btn-sm btn-quiet" href={`#/runs?scope=${s.id}`}>
            View its runs
          </a>
        </div>
      </header>

      <section className="cfg-sec">
        <div className="cfg-h-row">
          <h3 className="cfg-h">How events flow</h3>
          {samples.length > 0 ? (
            <button className={'btn btn-sm' + (trace ? ' btn-primary' : '')} onClick={() => setParams({ trace: trace ? undefined : samples[0]?.id })} aria-pressed={!!trace}>
              {trace ? 'Stop tracing' : 'Trace an event'}
            </button>
          ) : (
            <span className="quiet small">No trace samples for this scope in the prototype</span>
          )}
        </div>
        {trace && (
          <div className="trace">
            <label className="field field-inline">
              <span>Suppose</span>
              <select value={trace.id} onChange={(e) => setParams({ trace: e.target.value })}>
                {samples.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.label}
                  </option>
                ))}
              </select>
            </label>
            <TraceFacts t={trace} />
          </div>
        )}
        <FlowMap
          s={s}
          executors={cfg?.executors ?? []}
          trace={trace}
          onBinding={(name) => {
            setOpenBinding(name)
            setTimeout(() => document.getElementById('binding-' + name)?.scrollIntoView({ block: 'center', behavior: 'smooth' }), 0)
          }}
        />
        {trace && (
          <div className="trace-result">
            <b>{trace.result.effect}</b>
            <p>{trace.result.detail}</p>
          </div>
        )}
      </section>

      <section className="cfg-sec">
        <h3 className="cfg-h">Binding details</h3>
        <div className="bindings">
          {s.bindings.map((b) => (
            <Binding key={b.name} b={b} partial={s.partial} open={openBinding === b.name} onToggle={() => setOpenBinding(openBinding === b.name ? null : b.name)} />
          ))}
        </div>
      </section>

      <section className="cfg-sec">
        <h3 className="cfg-h">What extensions offer here</h3>
        {(s.panels ?? []).length > 0 && <ExtensionPanels panels={s.panels!} target={{ kind: 'scope', id: s.id }} />}
        {(s.extActions ?? []).length > 0 ? (
          <div className="xactions">
            {s.extActions!.map((a) => (
              <ActionControl key={a.provider + a.id} action={a} target={{ kind: 'scope', id: s.id }} />
            ))}
          </div>
        ) : (
          (s.panels ?? []).length === 0 && <p className="quiet">No extension offers a scope-level action or panel here.</p>
        )}
        <p className="quiet small">Extensions may also offer actions and panels on individual Work while they apply, such as a question an agent is waiting on. They appear on that Work and in Needs you.</p>
        <dl className="kv kv-wide">
          <dt>Authorized connections</dt>
          <dd>{s.partial ? <span className="quiet">not exposed by /v1/config</span> : s.authorization.connections.join(', ') || '—'}</dd>
          <dt>Operation ceiling</dt>
          <dd className="ops">
            {s.partial && <span className="quiet">not exposed by /v1/config</span>}
            {s.authorization.operations.map((o) => (
              <code key={o}>{o}</code>
            ))}
          </dd>
        </dl>
      </section>
    </>
  )
}

/** Scope-level actions and panels, as the extensions report them. */
function ScopeOffers({ s }: { s: ScopeConfig }) {
  const acts = s.extActions ?? []
  const panels = s.panels ?? []
  if (!acts.length && !panels.length) return <span className="quiet">—</span>
  return (
    <>
      {acts.map((a) => (
        <span key={a.provider + a.id} className="mx-chip" title={a.label}>
          {a.id} <small>{providerName(a.provider)}</small>
        </span>
      ))}
      {panels.map((p) => (
        <span key={p.provider + p.id} className="mx-chip">
          {p.title} <small>panel</small>
        </span>
      ))}
    </>
  )
}

function TraceFacts({ t }: { t: TraceSample }) {
  return (
    <div className="trace-facts">
      <code>{t.type}</code>
      <span className="quiet">from {t.source}</span>
      <ul>
        {t.facts.map((f) => (
          <li key={f}>{f}</li>
        ))}
      </ul>
    </div>
  )
}

function Binding({ b, open, onToggle, partial }: { b: BindingConfig; open: boolean; onToggle: () => void; partial?: boolean }) {
  const caps = (['inject', 'resume', 'interrupt', 'workspace'] as const).filter((k) => b.requires[k])
  return (
    <article className={'binding' + (open ? ' is-open' : '')} id={'binding-' + b.name}>
      <button type="button" className="binding-head" aria-expanded={open} onClick={onToggle}>
        <b>{b.name}</b>
        <span>→ {b.executor}</span>
        {b.config.model ? <small>model {String(b.config.model)}</small> : null}
        {b.resource && <small>worktree of {String(b.resource.config.checkout).replace('~', '~')}</small>}
        <span className="toolbar-gap" />
        <small>{b.operations.length ? `${b.operations.length} operations` : partial ? 'operations not exposed' : 'no operations'}</small>
      </button>
      {open && (
        <dl className="kv binding-body">
          <dt>Needs</dt>
          <dd>
            toolbelt <code>{b.requires.toolbelt}</code>
            {caps.map((c) => (
              <code key={c}>{c}</code>
            ))}
          </dd>
          {Object.keys(b.config).length > 0 && (
            <>
              <dt>Executor config</dt>
              <dd>
                {Object.entries(b.config).map(([k, v]) => (
                  <code key={k}>
                    {k}: {String(v)}
                  </code>
                ))}
              </dd>
            </>
          )}
          {b.resource && (
            <>
              <dt>Workspace</dt>
              <dd>
                {b.resource.provider} ·{' '}
                {Object.entries(b.resource.config).map(([k, v]) => (
                  <code key={k}>
                    {k}: {Array.isArray(v) ? v.join(', ') : String(v)}
                  </code>
                ))}
              </dd>
            </>
          )}
          {b.timeoutSeconds && (
            <>
              <dt>Deadline</dt>
              <dd>{b.timeoutSeconds / 60} minutes per attempt</dd>
            </>
          )}
          <dt>May call</dt>
          <dd className="ops">
            {b.operations.map((o) => (
              <code key={o}>{o}</code>
            ))}
          </dd>
        </dl>
      )}
    </article>
  )
}
