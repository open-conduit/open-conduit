import { useState, type ReactNode } from 'react'
import type { Work } from '../domain/types'
import { actionsFor, liveTurn, parkedTurn, hold, release, retry, wake, rebind, cancelTurn, cancelWork, getState, type ActionId } from '../data/store'
import { Dialog, Menu, MenuItem, MenuRule, Copy } from './controls'
import { since } from '../lib/time'
import type { ExtAction } from '../domain/types'
import { providerName } from './extensions'

const shortWork = (id: string) => 'work%2F' + id.replace(/^work\//, '').slice(0, 8) + '…'

function Endpoint({ path, w }: { path: string; w: Work }) {
  return (
    <p className="endpoint">
      <span className="sim-tag">Simulated</span>
      <code>POST /v1/{path.replace('{id}', shortWork(w.id))}</code>
      <span>expected_revision {w.revision} · Idempotency-Key</span>
    </p>
  )
}

function Consequences({ items }: { items: ReactNode[] }) {
  return (
    <ul className="consequences">
      {items.map((x, i) => (
        <li key={i}>
          <span>{x}</span>
        </li>
      ))}
    </ul>
  )
}

export function ActionDialog({ work: w, action, onClose }: { work: Work; action: ActionId | null; onClose: () => void }) {
  const [reason, setReason] = useState('')
  const live = liveTurn(w)
  const parked = parkedTurn(w)
  const scope = getState().config?.scopes.find((s) => s.id === w.scopeId)
  const bindings = scope?.bindings.filter((b) => b.executor === 'claude-code') ?? []
  const [binding, setBinding] = useState(bindings[0]?.name ?? 'agent')
  // The active Wait's match types (domain.Wait.Match.Types); free text when unknown.
  const waitTypes = w.waitTypes ?? []
  const [evType, setEvType] = useState(waitTypes[0] ?? '')
  const done = () => {
    setReason('')
    onClose()
  }
  const lastFail = [...w.turns].reverse().find((t) => t.state === 'failed')

  if (!action) return null
  const title = {
    hold: `Hold ${w.ref}`,
    release: `Release ${w.ref}`,
    retry: `Run ${w.ref} again`,
    wake: `Send a wake event to ${w.ref}`,
    rebind: `Rebind ${w.ref}`,
    'cancel-turn': `Cancel turn ${(live ?? parked)?.attempt} of ${w.ref}`,
    'cancel-work': `Cancel ${w.ref} for good`,
    attach: `Attach to ${w.ref}'s session`,
  }[action]

  return (
    <Dialog open onClose={done} title={title} tone={action === 'cancel-work' || action === 'cancel-turn' ? 'danger' : undefined}>
      <p className="dialog-lede">{w.title}</p>
      {action === 'hold' && (
        <>
          <Consequences
            items={[
              'No turn starts or resumes while the hold is in place.',
              'New events keep queueing and waits keep resolving, so the next turn sees everything that arrived meanwhile.',
              parked ? `Turn ${parked.attempt} stays parked; it cannot resume until you release.` : 'Nothing is running now, so nothing is interrupted.',
            ]}
          />
          <label className="field">
            <span>Reason · shown to anyone looking at this Work</span>
            <input autoFocus value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. Waiting on a product decision" />
          </label>
          <Endpoint path="work/{id}/hold" w={w} />
          <footer className="dialog-foot">
            <button className="btn" onClick={done}>Keep running</button>
            <button className="btn btn-primary" onClick={() => (hold(w.id, reason || 'Held from the web view'), done())}>Hold</button>
          </footer>
        </>
      )}
      {action === 'release' && (
        <>
          <Consequences items={[`Removes the hold placed ${since(w.hold!.at)} by ${w.hold!.by}: “${w.hold!.reason}”.`, 'Queued input then starts normally, when an agent slot is free.']} />
          <Endpoint path="work/{id}/release" w={w} />
          <footer className="dialog-foot">
            <button className="btn" onClick={done}>Keep holding</button>
            <button className="btn btn-primary" autoFocus onClick={() => (release(w.id), done())}>Release</button>
          </footer>
        </>
      )}
      {action === 'retry' && (
        <>
          {lastFail?.error && (
            <div className="callout tone-fail">
              <strong>Last failure · turn {lastFail.attempt}</strong>
              <code>{lastFail.error.message}</code>
            </div>
          )}
          <Consequences
            items={[
              'Accepts a core.manual.retry control event and lets retry policy queue a new attempt.',
              w.retry ? `The failure count (${w.retry.failureCount}) resets; the retry budget is available again.` : 'Retry state resets.',
              'Inputs that arrived since the failure are presented to the new attempt.',
              'If the cause is still there, it will fail the same way — check it is resolved first.',
            ]}
          />
          <Endpoint path="work/{id}/retry" w={w} />
          <footer className="dialog-foot">
            <button className="btn" onClick={done}>Not yet</button>
            <button className="btn btn-primary" autoFocus onClick={() => (retry(w.id), done())}>Run again</button>
          </footer>
        </>
      )}
      {action === 'wake' && (
        <>
          <Consequences items={['The event is accepted like any other declared event addressed to this Work.', 'If it matches the active Wait, the Work wakes and its next turn is queued. If not, it is recorded and nothing else happens — waking never forces state.']} />
          <label className="field">
            <span>Event type{waitTypes.length ? ' · from the active Wait' : ''}</span>
            {waitTypes.length ? (
              <select value={evType} onChange={(e) => setEvType(e.target.value)}>
                {waitTypes.map((t) => (
                  <option key={t}>{t}</option>
                ))}
              </select>
            ) : (
              <input value={evType} onChange={(e) => setEvType(e.target.value)} placeholder="a type the Wait matches" />
            )}
          </label>
          <Endpoint path="work/{id}/wake" w={w} />
          <footer className="dialog-foot">
            <button className="btn" onClick={done}>Cancel</button>
            <button className="btn btn-primary" disabled={!evType.trim()} onClick={() => (wake(w.id, evType.trim()), done())}>Send event</button>
          </footer>
        </>
      )}
      {action === 'rebind' && (
        <>
          <div className="callout tone-signal">
            <strong>Queued input is pinned to <code>{w.pinnedBinding}</code></strong>
            Generation {getState().config?.generation} no longer has that binding, so this Work cannot start.
          </div>
          <Consequences items={['Moves the queued input and its retry state to another enabled binding in the same scope.', 'Work never changes scope. The control event records both bindings and the generation.']} />
          <label className="field">
            <span>New binding in {w.scopeId}</span>
            <select value={binding} onChange={(e) => setBinding(e.target.value)}>
              {bindings.map((b) => (
                <option key={b.name} value={b.name}>
                  {b.name} · {b.executor} · {String(b.config.model ?? '')}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>Reason</span>
            <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="agent-sonnet was removed in generation 3" />
          </label>
          <Endpoint path="work/{id}/rebind" w={w} />
          <footer className="dialog-foot">
            <button className="btn" onClick={done}>Cancel</button>
            <button className="btn btn-primary" onClick={() => (rebind(w.id, binding, reason || 'binding removed by reload'), done())}>Rebind to {binding}</button>
          </footer>
        </>
      )}
      {action === 'cancel-turn' && (
        <>
          <Consequences
            items={[
              <>Fences attempt {(live ?? parked)!.attempt} and stops its process. Work done so far in the workspace is kept.</>,
              <>The Work stays open, but scheduling is held: <strong>an operator-cancelled turn is never retried automatically</strong>. Run it again when you are ready.</>,
              w.turns.length ? 'Its transcript and outputs remain inspectable.' : '',
            ].filter(Boolean)}
          />
          <Endpoint path={`executions/${shortWork(w.id)}%2Fattempt%2F${(live ?? parked)!.attempt}/cancel`} w={w} />
          <footer className="dialog-foot">
            <button className="btn" autoFocus onClick={done}>Let it run</button>
            <button className="btn btn-danger" onClick={() => (cancelTurn(w.id), done())}>Cancel turn</button>
          </footer>
        </>
      )}
      {action === 'cancel-work' && (
        <>
          <Consequences
            items={[
              <><strong>This ends the Work permanently.</strong> It cannot be reopened; a new Event would need new Work.</>,
              live ? `Turn ${live.attempt} is running and will be stopped.` : parked ? `Parked turn ${parked.attempt} is cancelled.` : 'No turn is running.',
              'Its waits, timers and queued input are removed. Later events for it are recorded as terminal and ignored.',
              w.extActions?.length ? `Actions extensions offer on it now (${w.extActions.map((x) => x.label).join('; ')}) stop being offered.` : '',
              'Nothing is closed in the source for you; the source updates its own view of the Work from the cancellation.',
            ].filter(Boolean)}
          />
          <label className="field">
            <span>Reason</span>
            <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. Superseded by #150" />
          </label>
          <Endpoint path="work/{id}/cancel" w={w} />
          <footer className="dialog-foot">
            <button className="btn" autoFocus onClick={done}>Keep the Work</button>
            <button className="btn btn-danger" onClick={() => (cancelWork(w.id, reason || 'Cancelled from the web view'), done())}>
              Cancel Work
            </button>
          </footer>
        </>
      )}
      {action === 'attach' && (
        <>
          <Consequences items={['Attaching needs a terminal: the agent\'s session is an interactive Claude Code process.', 'The command holds the Work, opens the session in its workspace, and releases the hold when you exit — so nothing resumes underneath you.']} />
          <div className="cmd">
            <code>open-conduit work attach {w.id}</code>
            <Copy text={`open-conduit work attach ${w.id}`} />
          </div>
          <footer className="dialog-foot">
            <button className="btn" onClick={done}>Done</button>
          </footer>
        </>
      )}
    </Dialog>
  )
}

/** The kebab menu on a row or the action bar on detail. */
export function ActionMenu({ work: w, onPick, onExt, label }: { work: Work; onPick: (a: ActionId) => void; onExt?: (a: ExtAction) => void; label?: ReactNode }) {
  const acts = actionsFor(w)
  const safe = acts.filter((a) => !a.destructive)
  const danger = acts.filter((a) => a.destructive)
  return (
    <Menu label={`Actions for ${w.ref}`} trigger={label ?? <span className="kebab" aria-hidden>⋯</span>}>
      {(close) => (
        <>
          {(w.extActions ?? []).map((a) => (
            <MenuItem key={a.provider + a.id} hint={`from ${providerName(a.provider)}`} onSelect={() => (close(), onExt?.(a))}>
              {a.label}
            </MenuItem>
          ))}
          {(w.extActions ?? []).length > 0 && <MenuRule />}
          {safe.map((a) => (
            <MenuItem key={a.id} disabled={!a.enabled} why={a.why} onSelect={() => (close(), onPick(a.id))}>
              {a.label}
            </MenuItem>
          ))}
          <MenuRule />
          {danger.map((a) => (
            <MenuItem key={a.id} destructive disabled={!a.enabled} why={a.why} onSelect={() => (close(), onPick(a.id))}>
              {a.label}
            </MenuItem>
          ))}
        </>
      )}
    </Menu>
  )
}
