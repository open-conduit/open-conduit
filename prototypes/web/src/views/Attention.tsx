import { useState } from 'react'
import type { Work } from '../domain/types'
import { useStore, type ActionId } from '../data/store'
import { workHref } from '../lib/router'
import { useNow, dur, since, clockTime } from '../lib/time'
import { StateMark } from '../ui/marks'
import { ActionDialog } from '../ui/actions'
import { ExtensionPanels, ActionControl } from '../ui/extensions'
import { Inline, Spinner } from '../ui/controls'

export function AttentionView() {
  const works = useStore((s) => s.works)
  const receipts = useStore((s) => s.receipts)
  const status = useStore((s) => s.status)
  const now = useNow()
  const [dialog, setDialog] = useState<{ w: Work; a: ActionId } | null>(null)

  // Any Work an extension is offering a person an action on, whatever the extension.
  const decisions = works.filter((w) => (w.extActions ?? []).length > 0 && w.disposition.group !== 'closed')
  const repairs = works.filter((w) => w.disposition.kind === 'repair')
  const held = works.filter((w) => w.hold)
  const watch = works.filter(
    (w) =>
      !w.hold &&
      w.disposition.group === 'waiting' &&
      (now - new Date(w.disposition.since).getTime() > 36 * 3600_000 || w.disposition.label.includes('failed')),
  )

  return (
    <div className="page attention">
      <header className="page-head">
        <div>
          <h1>Needs you</h1>
          <p className="page-sub">
            {status === 'loading'
              ? 'Reading work…'
              : decisions.length + repairs.length === 0
                ? 'Nothing is blocked on you. Agents are working or waiting on their own.'
                : `${decisions.length} waiting on a person, ${repairs.length} stopped and needing a repair. Everything else is moving on its own.`}
          </p>
        </div>
      </header>

      {status === 'loading' ? (
        <p className="loading-line">
          <Spinner /> Reading work…
        </p>
      ) : (
        <>
      <section className="att-section" aria-labelledby="att-dec">
        <h2 id="att-dec" className="section-rule">
          Waiting on a person <span>{decisions.length}</span>
        </h2>
        {decisions.length === 0 && <p className="quiet">No extension is offering an action on waiting Work.</p>}
        {decisions.map((w) => (
          <article key={w.id} className="att-item">
            <WorkLine w={w} now={now} />
            <ExtensionPanels panels={(w.panels ?? []).filter((p) => p.sections.some((x) => x.actions?.length))} target={{ kind: 'work', id: w.id }} prominent />
            {(w.extActions ?? [])
              .filter((a) => !(w.panels ?? []).some((p) => p.provider === a.provider && p.sections.some((x) => x.actions?.some((y) => y.id === a.id))))
              .map((a) => (
                <ActionControl key={a.provider + a.id} action={a} target={{ kind: 'work', id: w.id }} />
              ))}
          </article>
        ))}
      </section>

      <section className="att-section" aria-labelledby="att-rep">
        <h2 id="att-rep" className="section-rule">
          Stopped, needs a repair <span>{repairs.length}</span>
        </h2>
        {repairs.length === 0 && <p className="quiet">No failed retries or broken bindings.</p>}
        {repairs.map((w) => (
          <article key={w.id} className="att-item att-repair">
            <WorkLine w={w} now={now} />
            <div className="att-diag">
              <p>
                <b>{w.disposition.label}.</b> {w.disposition.detail}.
              </p>
              <div className="row-gap">
                {w.pinnedBinding ? (
                  <button className="btn btn-primary" onClick={() => setDialog({ w, a: 'rebind' })}>Rebind…</button>
                ) : (
                  <button className="btn btn-primary" onClick={() => setDialog({ w, a: 'retry' })}>Run again…</button>
                )}
                <a className="btn btn-quiet" href={workHref(w.id, [...w.turns].reverse().find((t) => t.state === 'failed')?.attempt)}>
                  Investigate
                </a>
              </div>
            </div>
          </article>
        ))}
      </section>

      <div className="att-cols">
        <section className="att-section" aria-labelledby="att-held">
          <h2 id="att-held" className="section-rule">
            Held by an operator <span>{held.length}</span>
          </h2>
          {held.length === 0 && <p className="quiet">Nothing is held.</p>}
          <ul className="att-list">
            {held.map((w) => (
              <li key={w.id}>
                <StateMark kind="held" />
                <div>
                  <a href={workHref(w.id)}>
                    <code>{w.ref}</code> {w.title}
                  </a>
                  <p>
                    “{w.hold!.reason}” · {w.hold!.by} · {since(w.hold!.at, now)}
                  </p>
                </div>
                <button className="btn btn-xs" onClick={() => setDialog({ w, a: 'release' })}>Release</button>
              </li>
            ))}
          </ul>
        </section>
        <section className="att-section" aria-labelledby="att-watch">
          <h2 id="att-watch" className="section-rule">
            Worth a look <span>{watch.length}</span>
          </h2>
          <p className="quiet small">Not blocked on you: long waits and scheduled jobs whose last run failed.</p>
          <ul className="att-list">
            {watch.map((w) => (
              <li key={w.id}>
                <StateMark kind={w.disposition.kind} />
                <div>
                  <a href={workHref(w.id)}>
                    <code>{w.ref}</code> {w.title}
                  </a>
                  <p>
                    {w.disposition.label} · {w.disposition.detail} · unchanged for {dur(now - new Date(w.disposition.since).getTime())}
                  </p>
                </div>
              </li>
            ))}
          </ul>
        </section>
      </div>

      <section className="att-section" aria-labelledby="att-log">
        <h2 id="att-log" className="section-rule">
          Interventions <span>{receipts.length} this session</span>
        </h2>
        {receipts.length === 0 && <p className="quiet">Answers, retries, holds and cancellations you make appear here with the exact API call.</p>}
        <ol className="log">
          {receipts.map((r) => {
            const w = works.find((x) => x.id === r.workId)
            return (
              <li key={r.id}>
                <time>{clockTime(r.at)}</time>
                <span className="log-what">
                  {r.summary}
                  {w && (
                    <>
                      {' '}
                      on <a href={workHref(w.id)}>{w.ref === 'thread' ? w.title : w.ref}</a>
                    </>
                  )}
                </span>
                <code className="log-call">
                  <span className="sim-tag">Simulated</span>
                  {r.method} {r.path.replace(/work%2F([0-9a-f]{8})[0-9a-f]{56}/g, 'work%2F$1…')}
                </code>
              </li>
            )
          })}
        </ol>
      </section>
        </>
      )}
      {dialog && <ActionDialog work={works.find((x) => x.id === dialog.w.id)!} action={dialog.a} onClose={() => setDialog(null)} />}
    </div>
  )
}

function WorkLine({ w, now }: { w: Work; now: number }) {
  return (
    <header className="att-head">
      <StateMark kind={w.disposition.kind} size={13} />
      <a href={workHref(w.id)} className="att-title">
        {w.ref !== 'thread' && <code>{w.ref}</code>} <Inline text={w.title} />
      </a>
      <span className="att-meta">
        {w.scopeId} · {w.requester ?? w.kind} · {since(w.disposition.since, now)}
      </span>
    </header>
  )
}
