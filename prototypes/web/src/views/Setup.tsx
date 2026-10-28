import { useEffect, useRef, useState } from 'react'
import { useStore, adoptFreshSetup, recordConfigApply, toast } from '../data/store'
import { useSession, start, chooseGoal, decide, setChange, approveAll, userSays, apply, test, reset, STAGES, type Msg, type Change, type Check, type Decisions, type Finding } from '../data/setupAgent'
import { configFixture } from '../data/fixtures/config'
import { StateMark } from '../ui/marks'
import { Spinner } from '../ui/controls'
import { Code } from './Transcript'

export function SetupView() {
  const s = useSession()
  const cfg = useStore((x) => x.config)
  const status = useStore((x) => x.status)
  const existing = (cfg?.generation ?? 0) > 0
  const [tab, setTab] = useState<'changes' | 'env' | 'checks'>('changes')
  const [draft, setDraft] = useState('')
  const end = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (status === 'ready' && !s.msgs.length && !existing) void start()
  }, [status, existing, s.msgs.length])
  useEffect(() => {
    end.current?.scrollIntoView({ block: 'end', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
  }, [s.msgs.length, s.busy])
  useEffect(() => {
    if (s.stage === 'look') setTab('env')
    if (s.stage === 'review') setTab('changes')
    if (s.stage === 'validate' && s.checks.some((c) => c.status === 'fail')) setTab('changes')
  }, [s.stage, s.checks])

  // Hand the applied configuration to the rest of the app.
  useEffect(() => {
    if (s.generation > 0 && s.decisions && (cfg?.generation ?? 0) < s.generation) {
      const sids = s.decisions.repos.map((r) => r.split('/')[1])
      const base = existing ? cfg!.scopes : []
      const added = configFixture.scopes.filter((x) => sids.includes(x.id) && !base.some((b) => b.id === x.id))
      const made = (sids.filter((id) => !configFixture.scopes.some((x) => x.id === id)) as string[]).map((id) => ({ ...configFixture.scopes[0], id, name: s.decisions!.repos.find((r) => r.endsWith('/' + id))!, fixture: true }))
      adoptFreshSetup({ ...(cfg ?? configFixture), generation: s.generation, appliedAt: new Date().toISOString(), fileMatchesGeneration: true, connections: existing ? cfg!.connections : [{ id: 'github', type: 'github', secretNames: ['token'], status: 'ok', detail: `gh CLI login · acts as ${s.decisions.account}` }], scopes: [...base, ...added, ...made] })
      const r = recordConfigApply(s.generation, s.changes.filter((c) => c.state === 'applied').length)
      toast(`Generation ${s.generation} applied`, 'ok', r)
    }
  }, [s.generation])

  const counts = { proposed: 0, approved: 0, applied: 0, rejected: 0, superseded: 0 }
  s.changes.forEach((c) => counts[c.state]++)
  const stageIdx = STAGES.findIndex((x) => x.id === s.stage)

  if (existing && !s.msgs.length) return <Returning onStart={() => void start({ scopes: cfg!.scopes.length, names: cfg!.scopes.map((x) => x.id) })} />

  return (
    <div className="page setup">
      <header className="setup-head">
        <div>
          <h1>Setup</h1>
          <p className="page-sub">
            A Claude Code session running Open Conduit's setup playbook. It looks, explains and proposes; <b>you approve every change</b> before it's written.
          </p>
        </div>
        <ol className="stages" aria-label="Progress">
          {STAGES.map((x, i) => (
            <li key={x.id} className={i < stageIdx ? 'is-done' : i === stageIdx ? 'is-on' : ''} aria-current={i === stageIdx ? 'step' : undefined}>
              <span className="stage-n">{i < stageIdx ? '✓' : i + 1}</span>
              <span className="stage-l">{x.label}</span>
            </li>
          ))}
        </ol>
      </header>

      <div className="setup-grid">
        <section className="convo" aria-label="Conversation">
          <div className="convo-meta">
            <span className="sim-tag">Prototype</span> scripted agent · in production this is <code>claude-code</code> with <code>playbooks/setup</code> · started {new Date(s.startedAt).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })}
            {s.msgs.length > 0 && (
              <button className="link-btn" onClick={() => (reset(), existing ? undefined : void start())}>
                Start over
              </button>
            )}
          </div>
          <div className="convo-scroll" aria-live="polite">
            {s.msgs.map((m) => (
              <MsgView key={m.id} m={m} existing={existing} existingRepos={existing ? cfg!.scopes.filter((x) => x.source === 'github').map((x) => x.name) : []} onTab={setTab} />
            ))}
            {s.busy && (
              <div className="msg msg-agent is-typing">
                <span className="who">Assistant</span>
                <span className="typing" aria-label="The assistant is working">
                  <i />
                  <i />
                  <i />
                </span>
              </div>
            )}
            <div ref={end} />
          </div>
          <form
            className="composer"
            onSubmit={(e) => {
              e.preventDefault()
              if (!draft.trim()) return
              void userSays(draft.trim())
              setDraft('')
            }}
          >
            <input value={draft} onChange={(e) => setDraft(e.target.value)} placeholder={s.stage === 'review' ? 'Ask for a change, e.g. “use Sonnet for the agent”' : 'Ask the assistant anything about the setup'} aria-label="Message the assistant" />
            <button className="btn" disabled={!draft.trim()}>Send</button>
          </form>
        </section>

        <aside className="bench" aria-label="Setup workbench">
          <div className="tabs" role="tablist">
            <button role="tab" aria-selected={tab === 'changes'} className={tab === 'changes' ? 'is-on' : ''} onClick={() => setTab('changes')}>
              Change set <span className="tab-n">{s.changes.filter((c) => c.state !== 'superseded').length}</span>
            </button>
            <button role="tab" aria-selected={tab === 'env'} className={tab === 'env' ? 'is-on' : ''} onClick={() => setTab('env')}>
              This machine <span className="tab-n">{s.findings.length}</span>
            </button>
            <button role="tab" aria-selected={tab === 'checks'} className={tab === 'checks' ? 'is-on' : ''} onClick={() => setTab('checks')}>
              Checks {s.checks.some((c) => c.status === 'fail') && <span className="tab-n tab-fail">!</span>}
            </button>
          </div>
          {tab === 'changes' && (
            <div className="bench-body">
              <div className="ledger-states">
                <span>
                  <b>{counts.proposed}</b> proposed
                </span>
                <span>
                  <b>{counts.approved}</b> approved
                </span>
                <span>
                  <b>{counts.applied}</b> applied
                </span>
                {counts.proposed > 1 && (
                  <button className="btn btn-xs btn-primary" onClick={approveAll}>
                    Approve all {counts.proposed}
                  </button>
                )}
              </div>
              {s.changes.length === 0 && <p className="quiet">Nothing proposed yet. The assistant drafts changes after it has looked around and asked you a few questions.</p>}
              <ol className="changes">
                {s.changes.map((c) => (
                  <ChangeCard key={c.id} c={c} />
                ))}
              </ol>
              <p className="bench-note">
                Applied changes go through <code>config validate</code> and <code>config apply</code>: the file is written atomically and the last good one is kept. Secret values never appear here — only references.
              </p>
            </div>
          )}
          {tab === 'env' && <Env findings={s.findings} />}
          {tab === 'checks' && (
            <div className="bench-body">
              {s.checks.length === 0 ? <p className="quiet">Validation and doctor results appear here once there's something to check.</p> : <Checks checks={s.checks} />}
            </div>
          )}
        </aside>
      </div>
    </div>
  )
}

function Returning({ onStart }: { onStart: () => void }) {
  const cfg = useStore((x) => x.config)!
  return (
    <div className="page setup">
      <header className="page-head">
        <div>
          <h1>Setup</h1>
          <p className="page-sub">Generation {cfg.generation} is active with {cfg.scopes.length} scopes. The assistant builds on what's there; it never removes anything you didn't ask it to.</p>
        </div>
      </header>
      <div className="returning">
        <section>
          <h2 className="section-rule">What's set up</h2>
          <ul className="kv-list">
            {cfg.scopes.map((x) => (
              <li key={x.id}>
                <StateMark kind="completed" />
                <b>{x.name}</b>
                <span>{x.description}</span>
              </li>
            ))}
          </ul>
        </section>
        <section>
          <h2 className="section-rule">Add or change</h2>
          <div className="add-grid">
            {[
              ['Watch another repository', 'A new GitHub scope with its own agent and worktrees'],
              ['Connect Slack', 'Answer threads in a channel; needs a Slack app'],
              ['Add a scheduled job', 'A command or agent on a calendar'],
              ['Ask people before acting', 'A human entry so agents can call human.ask'],
              ['Change how agents run', 'Model, concurrency, permissions, timeouts'],
              ['Diagnose a problem', 'Run doctor and walk through what fails'],
            ].map(([t, d]) => (
              <button key={t} className="add-card" onClick={onStart}>
                <b>{t}</b>
                <span>{d}</span>
              </button>
            ))}
          </div>
          <p className="quiet small">In the prototype each of these opens the same scripted session.</p>
        </section>
      </div>
    </div>
  )
}

function Rich({ text }: { text: string }) {
  return (
    <>
      {text.split('\n').map((line, i) =>
        line.trim() === '' ? (
          <br key={i} />
        ) : (
          <p key={i} className={line.startsWith('•') ? 'bullet' : ''}>
            {line
              .replace(/^•\s*/, '')
              .split(/(\*\*[^*]+\*\*|`[^`]+`)/)
              .map((p, j) => (p.startsWith('**') ? <b key={j}>{p.slice(2, -2)}</b> : p.startsWith('`') ? <code key={j}>{p.slice(1, -1)}</code> : p))}
          </p>
        ),
      )}
    </>
  )
}

function MsgView({ m, existing, existingRepos, onTab }: { m: Msg; existing: boolean; existingRepos: string[]; onTab: (t: 'changes' | 'env' | 'checks') => void }) {
  const s = useSession()
  switch (m.kind) {
    case 'agent':
      return (
        <div className="msg msg-agent">
          <span className="who">Assistant</span>
          <div className="msg-text">
            <Rich text={m.text} />
          </div>
        </div>
      )
    case 'user':
      return (
        <div className="msg msg-user">
          <span className="who">You</span>
          <div className="msg-text">{m.text}</div>
        </div>
      )
    case 'probe':
      return (
        <div className={'probe p-' + m.status}>
          <div className="probe-head">
            {m.status === 'running' ? <Spinner label="Running" /> : <StateMark kind={m.status === 'ok' ? 'completed' : m.status === 'warn' ? 'waiting' : 'failed'} size={10} />}
            <code>{m.cmd}</code>
          </div>
          <div className="probe-why">{m.why}</div>
          {m.result && <pre className="probe-out">{m.result}</pre>}
        </div>
      )
    case 'choose-goal':
      return (
        <div className="choices" role="group" aria-label="Choose a goal">
          {(existing ? ['Work GitHub issues in another repository', 'Answer questions in Slack', 'Run scheduled jobs', 'Not sure yet — look around first'] : ['Work GitHub issues with an agent', 'Answer questions in Slack', 'Run scheduled jobs', 'Not sure yet — look around first']).map((g) => (
            <button key={g} className={'choice' + (m.done === g ? ' is-on' : '')} disabled={!!m.done} onClick={() => void chooseGoal(m.id, g.replace('in another repository', 'with an agent'))}>
              {g}
            </button>
          ))}
        </div>
      )
    case 'decide':
      return <DecideForm m={m} existingRepos={existingRepos} />
    case 'proposal':
      return (
        <div className="proposal">
          <div className="proposal-head">
            <b>{m.changeIds.length === 1 ? '1 change' : `${m.changeIds.length} changes`} proposed</b>
            <button className="link-btn" onClick={() => onTab('changes')}>
              Open in change set →
            </button>
          </div>
          <ul>
            {m.changeIds.map((cid) => {
              const c = s.changes.find((x) => x.id === cid)
              if (!c) return null
              return (
                <li key={cid} className={'cs-' + c.state}>
                  <ChangeMark state={c.state} />
                  <span>{c.title}</span>
                  <code>{c.pointer}</code>
                </li>
              )
            })}
          </ul>
        </div>
      )
    case 'checks':
      return (
        <div className="checks-msg">
          <div className="probe-head">
            <code>{m.title}</code>
          </div>
          <Checks checks={m.checks} />
        </div>
      )
    case 'apply':
      return (
        <div className="apply-card">
          <p>
            <b>Apply generation {s.existing ? 4 : 1}?</b> {s.changes.filter((c) => c.state === 'approved').length} approved changes. The daemon starts acting on GitHub as soon as it's active.
          </p>
          <button className="btn btn-primary" disabled={m.done} onClick={() => void apply(m.id)}>
            {m.done ? 'Applied' : `Apply generation ${s.existing ? 4 : 1}`}
          </button>
        </div>
      )
    case 'test':
      return (
        <div className="choices choices-wide" role="group" aria-label="How to test">
          <button className={'choice' + (m.done === 'trace' ? ' is-on' : '')} disabled={!!m.done} onClick={() => void test(m.id, 'trace')}>
            <b>Trace a synthetic issue</b>
            <span>Run a made-up github.issue.open through the new routes. No side effects.</span>
          </button>
          <button className={'choice' + (m.done === 'real' ? ' is-on' : '')} disabled={!!m.done} onClick={() => void test(m.id, 'real')}>
            <b>Label a real issue</b>
            <span>It will be claimed, commented on and worked by an agent. Real effects on GitHub.</span>
          </button>
        </div>
      )
    case 'trace': {
      const repo = s.decisions?.repos[0] ?? 'open-conduit/open-conduit'
      return (
        <div className="trace-msg">
          <div className="probe-head">
            <code>trace · synthetic github.issue.open {repo}#9999</code> <span className="sim-tag">needs a dry-run API</span>
          </div>
          <ol className="trace-steps">
            <li><StateMark kind="completed" size={10} /> accepted from <code>github/github.com/{repo}</code></li>
            <li><StateMark kind="completed" size={10} /> scope <code>{repo.split('/')[1]}</code> · route 1 <code>issues</code> matches{s.decisions?.label ? ' (label agent present)' : ''}</li>
            <li><StateMark kind="completed" size={10} /> would create Work owning <code>issue/9999</code> and queue it for binding <code>agent</code> → claude-code ({s.model})</li>
            <li><StateMark kind="completed" size={10} /> workspace: worktree of <code>~/Code/{repo.split('/')[1]}</code> on <code>agent/issue-9999</code></li>
          </ol>
          <p className="quiet small">Dry run: nothing was accepted or created.</p>
        </div>
      )
    }
    case 'summary':
      return (
        <div className="summary-card">
          <h3>You're set up</h3>
          <ul>
            <li>Watching <b>{s.decisions?.repos.join(', ')}</b>{s.decisions?.label ? ' — issues labelled agent' : ''}</li>
            <li>Agent: Claude Code ({s.model}), acting as <b>{s.decisions?.account}</b>, up to {s.decisions?.concurrency} at once</li>
            <li>Generation {s.generation} active · doctor passes · new issues picked up within 5 minutes</li>
          </ul>
          <div className="row-gap">
            <a className="btn btn-primary" href="#/runs">Go to Runs</a>
            <a className="btn" href={'#/config/' + (s.decisions?.repos[0].split('/')[1] ?? '')}>See the configuration</a>
          </div>
          <p className="quiet small">Next, when you're ready: a webhook tunnel for instant pickup, Slack support, or a human entry so agents can ask before acting.</p>
        </div>
      )
  }
}

function DecideForm({ m, existingRepos }: { m: Extract<Msg, { kind: 'decide' }>; existingRepos: string[] }) {
  const repos = ['open-conduit/open-conduit', 'open-conduit/open-conduit-jiminny', 'open-conduit/open-conduit-testing', 'octo-dev/dotfiles']
  const [d, setD] = useState<Decisions>({ repos: existingRepos.length ? ['open-conduit/open-conduit-testing'] : ['open-conduit/open-conduit'], account: 'oc-agent-bot', concurrency: 2, label: true })
  const done = m.done
  const toggle = (r: string) => setD((x) => ({ ...x, repos: x.repos.includes(r) ? x.repos.filter((y) => y !== r) : [...x.repos, r] }))
  return (
    <form
      className={'decide' + (done ? ' is-done' : '')}
      onSubmit={(e) => {
        e.preventDefault()
        void decide(m.id, d)
      }}
    >
      <fieldset disabled={!!done}>
        <legend>Which repositories should it watch?</legend>
        {repos.map((r) => {
          const already = existingRepos.includes(r)
          return (
            <label key={r} className="check-row">
              <input type="checkbox" checked={already || d.repos.includes(r)} disabled={already} onChange={() => toggle(r)} />
              <span>
                <b>{r}</b>
                <small>{already ? 'already watched' : r.includes('dotfiles') ? 'has uncommitted changes — agents work in worktrees, but think twice' : r.includes('testing') ? 'docs only' : 'Go · clean'}</small>
              </span>
            </label>
          )
        })}
      </fieldset>
      <fieldset disabled={!!done}>
        <legend>Which account should the agent act as on GitHub?</legend>
        {[
          ['oc-agent-bot', 'Recommended: comments and PRs are clearly from an agent'],
          ['octo-dev', 'Your own account: everything the agent does looks like you'],
        ].map(([a, why]) => (
          <label key={a} className="check-row">
            <input type="radio" name="acct" checked={d.account === a} onChange={() => setD((x) => ({ ...x, account: a }))} />
            <span>
              <b>{a}</b>
              <small>{why}</small>
            </span>
          </label>
        ))}
      </fieldset>
      <fieldset disabled={!!done} className="decide-row">
        <legend>How should it start?</legend>
        <label className="field field-inline">
          <span>Agents at once</span>
          <select value={d.concurrency} onChange={(e) => setD((x) => ({ ...x, concurrency: Number(e.target.value) }))}>
            {[1, 2, 3, 4].map((x) => (
              <option key={x}>{x}</option>
            ))}
          </select>
        </label>
        <label className="check-row">
          <input type="checkbox" checked={d.label} onChange={(e) => setD((x) => ({ ...x, label: e.target.checked }))} />
          <span>
            <b>Only claim issues labelled agent</b>
            <small>Recommended for a first run — you choose what it works on</small>
          </span>
        </label>
      </fieldset>
      {!done && (
        <button className="btn btn-primary" disabled={!d.repos.length}>
          Draft the configuration
        </button>
      )}
    </form>
  )
}

function ChangeMark({ state }: { state: Change['state'] }) {
  return <StateMark kind={state === 'applied' ? 'completed' : state === 'approved' ? 'running' : state === 'rejected' || state === 'superseded' ? 'cancelled' : 'queued'} size={10} />
}

function ChangeCard({ c }: { c: Change }) {
  const [open, setOpen] = useState(c.state === 'proposed')
  useEffect(() => setOpen(c.state === 'proposed'), [c.state])
  return (
    <li className={'change cs-' + c.state}>
      <div className="change-head">
        <ChangeMark state={c.state} />
        <button type="button" className="change-title" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
          {c.title}
        </button>
        <span className="change-state">{c.revisionOf ? 'revised · ' : ''}{c.state}</span>
      </div>
      <code className="change-ptr">{c.pointer}</code>
      {open && (
        <>
          <p className="change-why">{c.why}</p>
          <Code text={c.json} lines={16} />
          {c.secretRef && <p className="secret-note">Secret by reference. The daemon runs this command when it needs the token; its output is never stored, logged or shown.</p>}
        </>
      )}
      {c.state === 'proposed' && (
        <div className="change-acts">
          <button className="btn btn-xs btn-primary" onClick={() => setChange(c.id, 'approved')}>Approve</button>
          <button className="btn btn-xs btn-quiet" onClick={() => setChange(c.id, 'rejected')}>Reject</button>
        </div>
      )}
      {c.state === 'approved' && (
        <div className="change-acts">
          <button className="btn btn-xs btn-quiet" onClick={() => setChange(c.id, 'proposed')}>Undo approval</button>
        </div>
      )}
    </li>
  )
}

function Checks({ checks }: { checks: Check[] }) {
  return (
    <ul className="checks">
      {checks.map((c) => (
        <li key={c.name} className={'ck-' + c.status}>
          {c.status === 'running' ? <Spinner /> : <StateMark kind={c.status === 'ok' ? 'completed' : c.status === 'fail' ? 'failed' : c.status === 'warn' ? 'waiting' : 'queued'} size={10} />}
          <b>{c.name}</b>
          <span>{c.detail}</span>
          {c.pointer && <code className="ck-ptr">{c.pointer}</code>}
          {c.remedy && <span className="ck-remedy">Remedy: {c.remedy}</span>}
        </li>
      ))}
    </ul>
  )
}

function Env({ findings }: { findings: Finding[] }) {
  const groups = [...new Set(findings.map((f) => f.group))]
  return (
    <div className="bench-body">
      {findings.length === 0 && <p className="quiet">What the assistant finds on this machine — executors, CLI logins, checkouts — collects here as it looks.</p>}
      {groups.map((g) => (
        <section key={g} className="env-group">
          <h3>{g}</h3>
          <ul className="kv-list">
            {findings
              .filter((f) => f.group === g)
              .map((f) => (
                <li key={f.label}>
                  <StateMark kind={f.status === 'ok' ? 'completed' : f.status === 'warn' ? 'waiting' : 'queued'} />
                  <b>{f.label}</b>
                  <span>{f.detail}</span>
                </li>
              ))}
          </ul>
        </section>
      ))}
    </div>
  )
}
