// The setup assistant's agent, behind the boundary a real implementation
// would fill: a Claude Code session running the `setup` playbook
// (extensions/github/playbooks/setup/setup.md) with its probes, config
// edits and validation surfaced as structured events instead of terminal
// text. The prototype drives the same event stream from a script; nothing
// here runs a command, reads a credential or writes a file.

import { useSyncExternalStore } from 'react'
import type { DistOmit } from '../domain/types'

export type ProbeStatus = 'running' | 'ok' | 'warn' | 'fail'
export type ChangeState = 'proposed' | 'approved' | 'applied' | 'rejected' | 'superseded'

export interface Change {
  id: string
  pointer: string
  title: string
  why: string
  json: string
  state: ChangeState
  /** Contains a secret reference: shown as a reference, never resolved here */
  secretRef?: boolean
  revisionOf?: string
}

export interface Finding {
  group: 'Daemon' | 'Executors' | 'GitHub logins' | 'Checkouts'
  label: string
  detail: string
  status: 'ok' | 'warn' | 'info'
}

export interface Check {
  name: string
  status: 'ok' | 'warn' | 'fail' | 'info' | 'running'
  detail: string
  pointer?: string
  remedy?: string
}

export type Msg =
  | { id: string; kind: 'agent'; text: string }
  | { id: string; kind: 'user'; text: string }
  | { id: string; kind: 'probe'; cmd: string; why: string; result?: string; status: ProbeStatus }
  | { id: string; kind: 'choose-goal'; done?: string }
  | { id: string; kind: 'decide'; done?: Decisions }
  | { id: string; kind: 'proposal'; changeIds: string[] }
  | { id: string; kind: 'checks'; title: string; checks: Check[] }
  | { id: string; kind: 'apply'; done?: boolean }
  | { id: string; kind: 'test'; done?: 'trace' | 'real' }
  | { id: string; kind: 'trace' }
  | { id: string; kind: 'summary' }

export interface Decisions {
  repos: string[]
  account: string
  concurrency: number
  label: boolean
}

export type Stage = 'orient' | 'look' | 'decide' | 'credentials' | 'review' | 'validate' | 'test' | 'done'
export const STAGES: { id: Stage; label: string }[] = [
  { id: 'orient', label: 'Orient' },
  { id: 'look', label: 'Look around' },
  { id: 'decide', label: 'Decide' },
  { id: 'credentials', label: 'Credentials' },
  { id: 'review', label: 'Review changes' },
  { id: 'validate', label: 'Validate & apply' },
  { id: 'test', label: 'Test' },
  { id: 'done', label: 'Done' },
]

export interface Session {
  stage: Stage
  busy: boolean
  msgs: Msg[]
  findings: Finding[]
  changes: Change[]
  checks: Check[]
  generation: number
  decisions?: Decisions
  model: 'opus' | 'sonnet'
  startedAt: string
  /** Building on an existing configuration */
  existing: boolean
}

const fresh = (): Session => ({ stage: 'orient', busy: false, msgs: [], findings: [], changes: [], checks: [], generation: 0, model: 'opus', startedAt: new Date().toISOString(), existing: false })
let s: Session = fresh()
const subs = new Set<() => void>()
const emit = () => subs.forEach((f) => f())
function patch(p: Partial<Session> | ((x: Session) => Partial<Session>)) {
  s = { ...s, ...(typeof p === 'function' ? p(s) : p) }
  emit()
}
export function useSession(): Session {
  return useSyncExternalStore(
    (l) => (subs.add(l), () => subs.delete(l)),
    () => s,
  )
}

let n = 0
const id = () => 'm' + ++n
const wait = (ms: number) => new Promise((r) => setTimeout(r, matchMedia('(prefers-reduced-motion: reduce)').matches ? ms / 3 : ms))
const push = (m: DistOmit<Msg, 'id'>) => {
  const mm = m as Msg
  const full = { ...mm, id: id() } as Msg
  patch((x) => ({ msgs: [...x.msgs, full] }))
  return full.id
}
const update = (mid: string, p: Partial<Msg>) => patch((x) => ({ msgs: x.msgs.map((m) => (m.id === mid ? ({ ...m, ...p } as Msg) : m)) }))
async function say(text: string, pause = 500) {
  patch({ busy: true })
  await wait(pause)
  push({ kind: 'agent', text })
  patch({ busy: false })
}
async function probe(cmd: string, why: string, result: string, status: ProbeStatus = 'ok', ms = 650, finding?: Finding | Finding[]) {
  const mid = push({ kind: 'probe', cmd, why, status: 'running' })
  await wait(ms)
  update(mid, { result, status })
  if (finding) patch((x) => ({ findings: [...x.findings, ...(Array.isArray(finding) ? finding : [finding])] }))
}

// ---- the script ----

let started = false
export async function start(existing?: { scopes: number; names: string[] }) {
  if (started || s.msgs.length) return
  started = true
  patch({ stage: 'orient' })
  if (existing) {
    patch({ existing: true })
    await say(
      `Welcome back. This installation already has ${existing.scopes} scopes (${existing.names.join(', ')}), and I'll build on them — I never remove a connection, scope, route or binding you didn't ask me to.\n\nWhat would you like to add?`,
      300,
    )
    push({ kind: 'choose-goal' })
    return
  }
  await say(
    'Hi — I\'ll set Open Conduit up with you. In plain terms:\n\n• **What it\'s for:** it watches places work turns up — GitHub issues, Slack threads, schedules — picks up the items you allow, and has an agent work each one, coming back whenever something new happens: a comment, CI finishing, a PR merging.\n• **How it works:** a background daemon on this machine. Each item gets its own git worktree and branch from your local clone; you watch and steer it here.\n• **What we\'ll do:** pick what it watches and which account it acts as, review every config change together, then prove it works.\n\nWhat would you like it to do first?',
    300,
  )
  push({ kind: 'choose-goal' })
}

export async function chooseGoal(mid: string, goal: string) {
  update(mid, { done: goal })
  push({ kind: 'user', text: goal })
  if (!goal.startsWith('Work GitHub')) {
    await say(goal.startsWith('Answer') ? 'Slack needs its own app and two tokens, which is a longer path. Let\'s start with GitHub, which this machine is already signed in to, and add Slack afterwards from this same screen.' : 'Let\'s look around first; what I find will suggest a starting point. GitHub issues is the quickest first win if you have repositories checked out.')
  }
  patch({ stage: 'look' })
  await say('I\'ll look around this machine before asking you anything else. These are all read-only.')
  await probe('open-conduit health', 'Is a daemon running, and with what configuration?', s.existing ? 'ready · v1.5.0 · generation 3 active · file matches' : 'ready · v1.5.0 · no configuration generation yet', 'ok', 600, { group: 'Daemon', label: 'Daemon running', detail: s.existing ? 'v1.5.0 · generation 3 · 4 scopes' : 'v1.5.0 · no generation yet · state in ~/.local/state/open-conduit', status: 'ok' })
  await probe('open-conduit adapters', 'Which integrations and executors this binary registers', 'github · slack · linear · jira · bitbucket · schedule · human · introspection · posthog · executors: claude-code, shell · workspace: git', 'ok', 500)
  await probe('command -v claude && claude --version', 'Can Claude Code be the executor?', '/opt/homebrew/bin/claude · 2.1.30 (Claude Code) · signed in', 'ok', 700, [
    { group: 'Executors', label: 'claude-code', detail: 'Claude Code 2.1.30 · signed in · inject, resume, workspace', status: 'ok' },
    { group: 'Executors', label: 'shell', detail: 'built in · for scheduled commands', status: 'ok' },
  ])
  await probe('command -v gh && file -b "$(command -v gh)"', 'Which gh will the daemon run? Wrappers and shims change the account.', '/opt/homebrew/bin/gh · Mach-O 64-bit executable arm64 (the real binary)', 'ok', 500)
  await probe("gh auth status --json hosts --jq '.hosts[][] | [.host, .login, .active, .tokenSource] | @tsv'", 'Which accounts gh can act as. This never prints a token.', 'github.com  octo-dev     true   keyring\ngithub.com  oc-agent-bot  false  keyring', 'ok', 800, [
    { group: 'GitHub logins', label: 'octo-dev', detail: 'active · keyring', status: 'ok' },
    { group: 'GitHub logins', label: 'oc-agent-bot', detail: 'keyring', status: 'ok' },
  ])
  await probe('for d in ~/Code/*/.git; do git -C "${d%/.git}" remote get-url origin; done', 'Checkouts with GitHub remotes', '~/Code/open-conduit          git@github.com:open-conduit/open-conduit.git\n~/Code/open-conduit-jiminny  git@github.com:open-conduit/open-conduit-jiminny.git\n~/Code/open-conduit-testing  git@github.com:open-conduit/open-conduit-testing.git\n~/Code/dotfiles              git@github.com:octo-dev/dotfiles.git', 'ok', 900, [
    { group: 'Checkouts', label: 'open-conduit/open-conduit', detail: '~/Code/open-conduit · Go · clean', status: 'ok' },
    { group: 'Checkouts', label: 'open-conduit/open-conduit-jiminny', detail: '~/Code/open-conduit-jiminny · Go · clean', status: 'ok' },
    { group: 'Checkouts', label: 'open-conduit/open-conduit-testing', detail: '~/Code/open-conduit-testing · docs only', status: 'info' },
    { group: 'Checkouts', label: 'octo-dev/dotfiles', detail: '~/Code/dotfiles · 3 uncommitted changes', status: 'warn' },
  ])
  patch({ stage: 'decide' })
  await say('Claude Code is installed and signed in, so it can do the work. `gh` knows two accounts, and I found four checkouts with GitHub remotes. A few decisions, then I\'ll draft the config.')
  push({ kind: 'decide' })
}

export async function decide(mid: string, d: Decisions) {
  update(mid, { done: d })
  patch({ decisions: d })
  push({ kind: 'user', text: `Watch ${d.repos.join(', ')} · act as ${d.account} · ${d.concurrency} at once · ${d.label ? 'only issues labelled agent' : 'every open issue'}` })
  patch({ stage: 'credentials' })
  await say(
    `For the token I'll write a **reference**, not a value: \`cmd:/opt/homebrew/bin/gh auth token --user ${d.account}\`. The daemon runs that command itself when it needs the token, so nothing secret passes through this conversation, the browser or the config file. I'll check that the reference resolves without showing what it resolves to.`,
  )
  await probe(`GH_TOKEN="$(gh auth token --user ${d.account})" gh api user --jq .login`, 'Does the reference produce a working token for that account?', `${d.account}   ← token resolved and discarded; never displayed`, 'ok', 900)
  patch({ stage: 'review' })
  await say('Here is the change set. Each change is shown exactly as it would be written. Nothing is applied until you approve it — approve them one by one, or all at once.')
  const changes = draft(d, s.model)
  patch({ changes })
  push({ kind: 'proposal', changeIds: changes.map((c) => c.id) })
}

function draft(d: Decisions, model: 'opus' | 'sonnet'): Change[] {
  const out: Change[] = s.existing ? [] : [
    { id: 'c-settings', pointer: '/settings', title: `Run up to ${d.concurrency} agent turns at once`, why: 'Each turn is a Claude Code session; this bounds cost and load on this machine.', json: `+ "concurrency": ${d.concurrency}`, state: 'proposed' },
    { id: 'c-conn', pointer: '/connections/github', title: `GitHub connection acting as ${d.account}`, why: 'Comments, PRs and labels will come from this account.', json: `+ {\n+   "id": "github",\n+   "type": "github",\n+   "secret_refs": { "token": "cmd:/opt/homebrew/bin/gh auth token --user ${d.account}" }\n+ }`, state: 'proposed', secretRef: true },
  ]
  if (s.existing) out.push({ id: 'c-conn-note', pointer: '/connections/github', title: 'Reuse the existing GitHub connection', why: 'Already configured and passing doctor; no change is written.', json: '  (unchanged)', state: 'proposed' })
  for (const repo of d.repos) {
    const sid = repo.split('/')[1]
    out.push(
      { id: 'c-scope-' + sid, pointer: `/scopes/${sid}`, title: `Scope ${sid} for ${repo}`, why: 'One scope per repository keeps its routes, agent and permissions separate.', json: `+ {\n+   "id": "${sid}",\n+   "name": "${repo}",\n+   "authorization": {\n+     "connections": ["github"],\n+     "operations": ["read_context", "wait", "declare_outcome", "github.context",\n+                    "github.issue.view", "github.issue.comment", "github.pr.view",\n+                    "github.checks", "github.wait"]\n+   }\n+ }`, state: 'proposed' },
      { id: 'c-ext-' + sid, pointer: `/scopes/${sid}/extensions/${sid}`, title: `Watch ${repo}`, why: 'No public URL is configured, so it reconciles every 5 minutes instead of using webhooks. You can add a tunnel later for instant pickup.', json: `+ {\n+   "name": "${sid}", "type": "github", "connection": "github",\n+   "config": { "namespace": "github/github.com/${repo}", "scope": "${sid}", "repo": "${repo}"${d.label ? ',\n+               "labels": { "require": ["agent"] }' : ''} }\n+ }`, state: 'proposed' },
      { id: 'c-route-' + sid, pointer: `/scopes/${sid}/routes/issues`, title: d.label ? 'Claim open issues labelled agent' : 'Claim every open issue', why: 'Comments that arrive while the agent works are injected into its session (policy inject).', json: `+ { "name": "issues", "handler": "github", "binding": "agent", "active_policy": "inject" }`, state: 'proposed' },
      { id: 'c-bind-' + sid, pointer: `/scopes/${sid}/bindings/agent`, title: `Claude Code (${model}) in a worktree of ~/Code/${sid}`, why: 'Each issue gets its own branch agent/issue-N, so agents never touch your working copy.', json: bindingJson(sid, model), state: 'proposed' },
    )
  }
  return out
}

const bindingJson = (sid: string, model: string) =>
  `+ {\n+   "name": "agent", "executor": "claude-code", "connection": "github",\n+   "requires": { "toolbelt": "mcp", "inject": true, "resume": true, "workspace": true },\n+   "operations": ["read_context", "wait", "declare_outcome", "github.context", "github.issue.view",\n+                  "github.issue.comment", "github.pr.view", "github.pr.create", "github.checks", "github.wait"],\n+   "config": { "branch": "agent/issue-{number}", "model": "${model}" },\n+   "resource": { "provider": "git", "config": { "checkout": "~/Code/${sid}", "mode": "work" } }\n+ }`

export function setChange(cid: string, state: ChangeState) {
  patch((x) => ({ changes: x.changes.map((c) => (c.id === cid ? { ...c, state } : c)) }))
  maybeAdvance()
}
export function approveAll() {
  patch((x) => ({ changes: x.changes.map((c) => (c.state === 'proposed' ? { ...c, state: 'approved' } : c)) }))
  maybeAdvance()
}

let validating = false
function maybeAdvance() {
  const open = s.changes.filter((c) => c.state === 'proposed')
  if (open.length || validating) return
  if (s.stage === 'review') void validate()
  else if (s.stage === 'validate' && s.changes.some((c) => c.id === 'c-fix' && c.state === 'approved') && !s.msgs.some((m) => m.kind === 'apply')) void revalidate()
}

export async function userSays(text: string) {
  push({ kind: 'user', text })
  const t = text.toLowerCase()
  if (t.includes('sonnet') && s.stage === 'review') {
    await say('Switching the agent to Sonnet: cheaper and faster per turn, a little less capable on large changes. I\'ve revised the binding — the old version is kept below, struck through, so you can see what changed.')
    patch((x) => ({
      model: 'sonnet',
      changes: x.changes.flatMap((c) =>
        c.id.startsWith('c-bind-') && c.state !== 'superseded'
          ? [{ ...c, state: 'superseded' as const }, { ...c, id: c.id + '-r', title: c.title.replace('opus', 'sonnet'), json: c.json.replace('"model": "opus"', '"model": "sonnet"'), state: 'proposed' as const, revisionOf: c.id }]
          : [c],
      ),
    }))
    push({ kind: 'proposal', changeIds: s.changes.filter((c) => c.revisionOf).map((c) => c.id) })
    return
  }
  if (t.includes('slack')) return say('Slack comes next, once GitHub is working: it needs a Slack app (I\'ll generate the manifest) and two tokens you store with `open-conduit secret set` in your own terminal.')
  if (t.includes('token') || t.includes('secret') || t.includes('password')) return say('I never ask for or display tokens. Credentials stay where they are (your gh login, the keyring or an environment variable) and the config holds only a reference to them.')
  if (t.includes('webhook') || t.includes('tunnel')) return say('Without a webhook, new issues are picked up on the next reconcile (every 5 minutes). For instant pickup add `settings.webhooks` with a tunnel such as cloudflared — we can do that after the first test.')
  return say('Noted. In this prototype I follow one setup path, so free-form requests beyond a few (try “use Sonnet”, or ask about tokens or webhooks) are acknowledged but not acted on.')
}

async function validate() {
  validating = true
  patch({ stage: 'validate' })
  await say('All changes approved. Validating them against the daemon\'s own registrations before anything is written.', 300)
  const mid = push({ kind: 'checks', title: 'open-conduit config validate', checks: [{ name: 'validate', status: 'running', detail: `Checking ${s.changes.length} changes against generation ${s.existing ? 3 : 0}…` }] })
  await wait(1300)
  const sid = s.decisions!.repos[0].split('/')[1]
  const checks: Check[] = [
    { name: 'schema', status: 'ok', detail: 'config.json matches schema version 1' },
    { name: 'connections', status: 'ok', detail: 'github: type registered' },
    { name: 'authorization', status: 'fail', detail: 'binding agent may call github.pr.create, but the scope\'s ceiling does not allow it', pointer: `/scopes/${sid}/bindings/agent/operations/7`, remedy: `add "github.pr.create" to /scopes/${sid}/authorization/operations` },
    { name: 'workspace', status: 'ok', detail: `~/Code/${sid} is a git checkout with a clean worktree list` },
  ]
  update(mid, { checks } as Partial<Msg>)
  patch({ checks })
  await say(`One problem, and it's mine: the binding may open pull requests (\`github.pr.create\`) but I left that operation out of the scope's ceiling, so the daemon would refuse the call mid-turn. Here's the fix.`)
  const fix: Change = { id: 'c-fix', pointer: `/scopes/${sid}/authorization/operations`, title: 'Allow the agent to open pull requests', why: 'The scope ceiling bounds what any binding in it may do; it has to include github.pr.create.', json: `  "operations": ["read_context", "wait", "declare_outcome", "github.context",\n                 "github.issue.view", "github.issue.comment", "github.pr.view",\n-                "github.checks", "github.wait"]\n+                "github.pr.create", "github.checks", "github.wait"]`, state: 'proposed' }
  patch((x) => ({ changes: [...x.changes, fix] }))
  push({ kind: 'proposal', changeIds: ['c-fix'] })
  validating = false
}

async function revalidate() {
  validating = true
  const mid = push({ kind: 'checks', title: 'open-conduit config validate', checks: [{ name: 'validate', status: 'running', detail: 'Re-checking…' }] })
  await wait(1000)
  const checks: Check[] = [
    { name: 'schema', status: 'ok', detail: 'matches schema version 1' },
    { name: 'connections', status: 'ok', detail: 'github: type registered' },
    { name: 'authorization', status: 'ok', detail: 'every binding operation is within its scope ceiling' },
    { name: 'workspace', status: 'ok', detail: 'checkouts exist and are git repositories' },
  ]
  update(mid, { checks } as Partial<Msg>)
  patch({ checks })
  await say('Valid. Applying writes the file atomically (keeping the last good one) and activates it as **generation ' + (s.existing ? 4 : 1) + '**. From then on:\n\n• the daemon watches ' + s.decisions!.repos.join(' and ') + (s.decisions!.label ? ' for open issues labelled `agent`' : ' for every open issue') + ',\n• claims them and comments as **' + s.decisions!.account + '**,\n• and runs up to ' + s.decisions!.concurrency + ' Claude Code sessions at once on this machine.')
  push({ kind: 'apply' })
  validating = false
}

export async function apply(mid: string) {
  update(mid, { done: true })
  patch({ busy: true })
  await wait(900)
  patch((x) => ({ generation: x.existing ? 4 : 1, busy: false, changes: x.changes.map((c) => (c.state === 'approved' ? { ...c, state: 'applied' } : c)) }))
  push({ kind: 'user', text: `Apply generation ${s.generation}` })
  await say(`Applied — generation ${s.generation} is active. Running doctor with secret checks to confirm every connection, credential and binding.`, 300)
  const did = push({ kind: 'checks', title: 'open-conduit doctor --check-secrets', checks: [{ name: 'doctor', status: 'running', detail: 'Running checks…' }] })
  await wait(1500)
  const checks: Check[] = [
    { name: 'config', status: 'ok', detail: `generation ${s.generation} is active and matches the file` },
    { name: 'connections', status: 'ok', detail: 'github reachable as ' + s.decisions!.account },
    { name: 'secrets', status: 'ok', detail: '1 secret reference resolved; values were discarded' },
    { name: 'bindings', status: 'ok', detail: 'claude-code found on PATH (/opt/homebrew/bin/claude)' },
    { name: 'workspaces', status: 'ok', detail: s.decisions!.repos.length + ' checkout(s) usable for worktrees' },
    { name: 'webhooks', status: 'info', detail: 'none configured; sources reconcile every 5 minutes' },
  ]
  update(did, { checks } as Partial<Msg>)
  patch({ checks, stage: 'test' })
  await say('Everything passes. Last step: prove an issue would be picked up. Two ways:')
  push({ kind: 'test' })
}

export async function test(mid: string, how: 'trace' | 'real') {
  update(mid, { done: how })
  push({ kind: 'user', text: how === 'trace' ? 'Trace a synthetic issue' : 'Label a real issue' })
  if (how === 'real') {
    await say('In the prototype I can\'t touch GitHub, so I\'ll trace a synthetic one instead. For real: add the `agent` label to a small issue and it will be claimed within 5 minutes; it appears on Runs.')
  }
  push({ kind: 'trace' })
  await wait(900)
  patch({ stage: 'done' })
  await say('That\'s the whole loop. You can come back here any time to add Slack, schedules or more repositories — I\'ll start from what\'s already configured instead of from scratch.')
  push({ kind: 'summary' })
}

export function reset() {
  started = false
  s = fresh()
  validating = false
  emit()
}
