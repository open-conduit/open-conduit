// Renders what extensions contribute: panels and actions, as data. Nothing
// here names an extension; a new one shows up without changes to this file.
// Panels follow extension/panel.go: sections in order of title, text,
// fields, table, links and action buttons.

import { useState } from 'react'
import type { ExtAction, ExtPanel } from '../domain/types'
import { invokeExtAction, type ActionTarget } from '../data/store'
import { Dialog } from './controls'

/** The provider's display name: its entry name, the last path segment. */
export const providerName = (p: string) => p.split('/').pop() || p

export function ExtensionPanels({ panels, target, prominent }: { panels: ExtPanel[]; target: ActionTarget; prominent?: boolean }) {
  return (
    <>
      {panels.map((p) => (
        <ExtensionPanel key={p.provider + p.id} panel={p} target={target} prominent={prominent} />
      ))}
    </>
  )
}

export function ExtensionPanel({ panel, target, prominent }: { panel: ExtPanel; target: ActionTarget; prominent?: boolean }) {
  const hasActions = panel.sections.some((s) => s.actions?.length)
  return (
    <section className={'xpanel' + (prominent && hasActions ? ' is-prominent' : '')} aria-label={panel.title}>
      <h2 className="xpanel-title">{panel.title}</h2>
      {panel.sections.map((s, i) => (
        <div key={i} className="xpanel-section">
          {s.title && <h3>{s.title}</h3>}
          {s.text && (
            <div className={i === 0 && !s.title ? 'xpanel-lead' : 'xpanel-text'}>
              {s.text.split('\n\n').map((para, j) => (
                <p key={j}>{para}</p>
              ))}
            </div>
          )}
          {s.fields && s.fields.length > 0 && (
            <dl className="xpanel-fields">
              {s.fields.map((f) => (
                <div key={f.name}>
                  <dt>{f.name}</dt>
                  <dd>{f.value}</dd>
                </div>
              ))}
            </dl>
          )}
          {s.table && (
            <table className="xpanel-table">
              <thead>
                <tr>
                  {s.table.columns.map((c) => (
                    <th key={c}>{c}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {s.table.rows.map((r, j) => (
                  <tr key={j}>
                    {r.map((c, k) => (
                      <td key={k}>{c}</td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {s.links && s.links.length > 0 && (
            <p className="xpanel-links">
              {s.links.map((l) => (
                <a key={l.url} href={l.url} target="_blank" rel="noreferrer">
                  {l.label} ↗
                </a>
              ))}
            </p>
          )}
          {s.actions?.map((a) => (
            <ActionControl key={a.provider + a.id} action={a} target={target} />
          ))}
        </div>
      ))}
      <p className="xpanel-from">From {providerName(panel.provider)}</p>
    </section>
  )
}

/**
 * One action, inline. Without a prompt it is a button; with a prompt, a text
 * box; with choices, one button per choice. A choice is selected first and
 * sent second, since an action's Event cannot be taken back.
 */
export function ActionControl({ action: a, target, onDone }: { action: ExtAction; target: ActionTarget; onDone?: () => void }) {
  const [choice, setChoice] = useState<string | null>(null)
  const [text, setText] = useState('')
  const send = (input: string) => {
    invokeExtAction(target, a, input)
    onDone?.()
  }
  if (!a.prompt)
    return (
      <div className="xaction">
        <button className="btn btn-primary" onClick={() => send('')}>
          {a.label}
        </button>
      </div>
    )
  if (a.choices?.length)
    return (
      <div className="xaction">
        <p className="xaction-label">{a.label}</p>
        <div className="xchoices" role="radiogroup" aria-label={a.label}>
          {a.choices.map((c) => (
            <button key={c} type="button" role="radio" aria-checked={choice === c} className={'xchoice' + (choice === c ? ' is-on' : '')} onClick={() => setChoice(c)}>
              {c}
            </button>
          ))}
        </div>
        <div className="xaction-foot">
          <button className="btn btn-signal" disabled={!choice} onClick={() => choice && send(choice)}>
            Send
          </button>
          <span className="xaction-note">
            Invokes <code>{a.id}</code> on {providerName(a.provider)}
          </span>
        </div>
      </div>
    )
  return (
    <form
      className="xaction"
      onSubmit={(e) => {
        e.preventDefault()
        if (text.trim()) send(text.trim())
      }}
    >
      <label className="field">
        <span>
          {a.label} — {a.prompt}
        </span>
        <span className="xaction-row">
          <input value={text} onChange={(e) => setText(e.target.value)} />
          <button className="btn btn-primary" disabled={!text.trim()}>
            Send
          </button>
        </span>
      </label>
    </form>
  )
}

/** An extension action opened from a menu. */
export function ActionDialogFor({ action, target, onClose }: { action: ExtAction | null; target: ActionTarget; onClose: () => void }) {
  if (!action) return null
  return (
    <Dialog open onClose={onClose} title={action.label}>
      <p className="dialog-lede">Offered by {providerName(action.provider)}. The extension checks the input and decides what happens; Open Conduit records the Event it produces.</p>
      <ActionControl action={action} target={target} onDone={onClose} />
      <p className="endpoint">
        <span className="sim-tag">Simulated</span>
        <code>
          POST /v1/{target.kind === 'work' ? 'work' : 'scopes'}/…/actions/{encodeURIComponent(action.provider)}/{action.id}
        </code>
      </p>
    </Dialog>
  )
}
