// Mirrors ~/.config/open-conduit/config.json on the machine this prototype
// was built on, with secret references redacted (GET /v1/scopes does the
// same). The `nightly` scope is a fixture showing the schedule and
// introspection extensions with a shell executor.

import type { ConfigSnapshot, ScopeConfig, BindingConfig } from '../../domain/types'
import { ago } from './build'

const ghOps = ['read_context', 'emit', 'wait', 'declare_outcome', 'github.context', 'github.issue.view', 'github.issue.comment', 'github.issue.list', 'github.issue.create', 'github.pr.list', 'github.pr.view', 'github.pr.create', 'github.pr.comment', 'github.pr.reply', 'github.pr.review', 'github.checks', 'github.attachment.upload', 'github.attachment.get', 'github.wait']

function agent(checkout: string, copy?: string[]): BindingConfig {
  return {
    name: 'agent',
    executor: 'claude-code',
    connection: 'github',
    requires: { toolbelt: 'mcp', inject: true, resume: true, workspace: true },
    operations: ghOps.filter((o) => o !== 'emit'),
    config: { branch: 'agent/issue-{number}', model: 'opus', permission_mode: 'bypassPermissions' },
    resource: { provider: 'git', config: { checkout, mode: 'work', setup: 'go mod download', setup_when: ['go.sum'], ...(copy ? { copy } : {}) } },
  }
}
const hold: BindingConfig = {
  name: 'hold',
  executor: 'github-hold',
  connection: 'github',
  requires: { toolbelt: 'sdk' },
  operations: ['emit', 'github.hold.status', 'github.hold'],
  config: {},
}

function ghScope(id: string, checkout: string, copy?: string[]): ScopeConfig {
  return {
    id,
    name: `open-conduit/${id}`,
    enabled: true,
    source: 'github',
    description: 'Claims open issues, works each on its own branch with Claude Code, opens a PR and answers checks and reviews.',
    authorization: { connections: ['github'], operations: [...ghOps, 'github.hold.status', 'github.hold'] },
    extensions: [
      {
        name: id,
        type: 'github',
        connection: 'github',
        summary: `Repository open-conduit/${id === 'open-conduit' ? 'open-conduit' : id} · webhooks managed · reconcile every 5m`,
        config: { namespace: `github/github.com/open-conduit/${id}`, scope: id, repo: `open-conduit/${id}`, webhook_secret: '[redacted]', manage_webhook: true },
      },
    ],
    routes: [
      { name: 'held', handler: 'github-hold', binding: 'hold', activePolicy: 'queue', matches: 'Issues claimed with a hold (until a time, or until another issue or PR changes). The hold executor parks them; no agent runs.' },
      { name: 'issues', handler: 'github', binding: 'agent', activePolicy: 'inject', matches: 'Open issues without a skip label, plus every check, comment, review and merge on their pull requests.' },
    ],
    bindings: [agent(checkout, copy), hold],
    // As GET /v1/scopes/{id}/actions returns it on the local daemon.
    extActions: [{ provider: `github/${id}/${id}`, id: 'claim', label: `Claim an issue in open-conduit/${id} by hand`, prompt: 'issue number' }],
    panels: [],
  }
}

export const configFixture: ConfigSnapshot = {
  generation: 3,
  appliedAt: ago(118),
  fileMatchesGeneration: true,
  path: '~/.config/open-conduit/config.json',
  settings: { concurrency: 2, jobConcurrency: 1, webhooks: { addr: '127.0.0.1:60614', tunnel: 'cloudflared' } },
  connections: [
    { id: 'github', type: 'github', secretNames: ['token'], status: 'ok', detail: 'gh CLI login · acts as octo-dev' },
    { id: 'slack', type: 'slack', secretNames: ['bot_token', 'app_token'], status: 'ok', detail: 'Socket Mode · team T0C49A2P55Z' },
  ],
  executors: [
    { name: 'claude-code', kind: 'agent', detail: 'Claude Code CLI · signed in · MCP toolbelt', capabilities: { inject: true, resume: true, interrupt: true, workspace: true }, available: true },
    { name: 'shell', kind: 'command', detail: 'Runs a command per attempt · SDK toolbelt', capabilities: { inject: false, resume: false, interrupt: true, workspace: true }, available: true },
    { name: 'github-hold', kind: 'internal', detail: 'Built into the GitHub extension · parks held issues', capabilities: { inject: false, resume: false, interrupt: false, workspace: false }, available: true },
  ],
  scopes: [
    ghScope('open-conduit', '~/Code/open-conduit', ['.env']),
    ghScope('open-conduit-jiminny', '~/Code/open-conduit-jiminny'),
    {
      id: 'support',
      name: 'Slack testing',
      enabled: true,
      source: 'slack',
      description: 'Answers threads in #support with a workspace-free agent over Socket Mode; asks a person before sharing anything internal.',
      authorization: { connections: ['slack'], operations: ['read_context', 'wait', 'declare_outcome', 'slack.context', 'slack.thread', 'slack.reply', 'human.ask'] },
      extensions: [
        { name: 'support', type: 'slack', connection: 'slack', summary: 'Team T0C49A2P55Z · channel C0CC4DM066Q · starts on @mention · Socket Mode', config: { namespace: 'slack/T0C49A2P55Z', scope: 'support', team: 'T0C49A2P55Z', channels: ['C0CC4DM066Q'], start: 'mention', bot_user_id: 'U0CC0GZUFFC', receiver: 'socket' } },
        { name: 'decisions', type: 'human', ingress: false, summary: 'Questions agents ask with human.ask · answered by operator:local in the TUI or web', config: { responders: ['operator:local'], surfaces: ['tui'] } },
      ],
      routes: [{ name: 'conversations', handler: 'slack', binding: 'support', activePolicy: 'inject', matches: 'Threads in the configured channels that mention the app, and every later human message in them. Agent replies are marked and dropped.' }],
      bindings: [
        {
          name: 'support',
          executor: 'claude-code',
          connection: 'slack',
          requires: { toolbelt: 'mcp', inject: true, resume: true, workspace: true },
          operations: ['read_context', 'wait', 'declare_outcome', 'slack.context', 'slack.thread', 'slack.reply', 'human.ask'],
          config: { playbook: 'support-conversation', args: '{key}', model: 'opus', permission_mode: 'bypassPermissions' },
          resource: { provider: 'git', config: { checkout: '~/Code/open-conduit-testing', mode: 'work' } },
        },
      ],
    },
    {
      id: 'nightly',
      name: 'Nightly jobs',
      enabled: true,
      source: 'schedule',
      fixture: true,
      description: 'Standing Work that fires on calendar schedules: a state backup, a dependency audit and the daily triage of agent problem reports.',
      authorization: { connections: ['github'], operations: ['read_context', 'declare_outcome', 'introspection.reports', 'github.issue.create'] },
      extensions: [
        { name: 'nightly', type: 'schedule', summary: '3 schedules · local time', config: { jobs: [{ name: 'state-backup', at: '03:00' }, { name: 'dependency-audit', at: '06:00' }, { name: 'daily-triage', at: '07:00' }] } },
        { name: 'reports', type: 'introspection', connection: 'github', summary: 'Problem reports and corrections from every scope', config: { read_scopes: ['open-conduit', 'open-conduit-jiminny'] } },
      ],
      routes: [{ name: 'jobs', handler: 'schedule', binding: 'job', activePolicy: 'queue', matches: 'Each schedule firing. daily-triage selects the triage binding; the others use job.' }],
      bindings: [
        { name: 'job', executor: 'shell', requires: { toolbelt: 'sdk', workspace: false }, operations: ['read_context', 'declare_outcome'], config: { argv_from: 'schedule' }, timeoutSeconds: 600 },
        { name: 'triage', executor: 'claude-code', connection: 'github', requires: { toolbelt: 'mcp', resume: false, workspace: false }, operations: ['read_context', 'declare_outcome', 'introspection.reports', 'github.issue.create'], config: { playbook: 'daily-triage', model: 'opus' } },
      ],
    },
  ],
}

/** Sample Events for the route tracer: what would happen to each, and why. */
export interface TraceSample {
  id: string
  scopeId: string
  label: string
  type: string
  source: string
  facts: string[]
  steps: { route: string; verdict: 'skip' | 'match' | 'not-reached'; reason: string }[]
  result: { effect: string; detail: string }
}

export const traceSamples: TraceSample[] = [
  {
    id: 'new-issue',
    scopeId: 'open-conduit',
    label: 'A new issue is opened',
    type: 'github.issue.open',
    source: 'github/github.com/open-conduit/open-conduit',
    facts: ['issue #150, no labels', 'no Work owns issue/150'],
    steps: [
      { route: 'held', verdict: 'skip', reason: 'github-hold matches only issues claimed with a hold; #150 has none' },
      { route: 'issues', verdict: 'match', reason: 'github matches an open issue without a skip label' },
    ],
    result: { effect: 'Start new Work', detail: 'Creates github.issue Work owning issue/150 and queues the Event for binding agent (claude-code, worktree on agent/issue-150).' },
  },
  {
    id: 'comment-running',
    scopeId: 'open-conduit',
    label: 'A comment arrives while the agent is working',
    type: 'github.comment.human',
    source: 'github/github.com/open-conduit/open-conduit',
    facts: ['comment on PR #139 by @octo-dev', 'Work #134 owns pull/139', 'turn 2 of #134 is running'],
    steps: [
      { route: 'held', verdict: 'skip', reason: '#134 has no hold' },
      { route: 'issues', verdict: 'match', reason: 'comments on a claimed issue\'s PR belong to its Work' },
    ],
    result: { effect: 'Inject into the running turn', detail: 'Active policy inject: claude-code declares inject, so the comment is delivered into the live session and kept as input until consumption is confirmed.' },
  },
  {
    id: 'checks-waiting',
    scopeId: 'open-conduit',
    label: 'Checks finish on a waiting pull request',
    type: 'github.checks.passed',
    source: 'github/github.com/open-conduit/open-conduit',
    facts: ['PR #137, 7 of 7 passed', 'Work #136 is waiting on pull/137 for github.checks.*'],
    steps: [
      { route: 'held', verdict: 'skip', reason: '#136 has no hold' },
      { route: 'issues', verdict: 'match', reason: 'belongs to Work #136' },
    ],
    result: { effect: 'Wake waiting Work', detail: 'The Event matches the active Wait (keys pull/137, types github.checks.*), so Work #136 becomes ready and its next turn starts when an agent slot frees.' },
  },
  {
    id: 'held-issue',
    scopeId: 'open-conduit',
    label: 'A held issue\'s blocker merges',
    type: 'github.pull_request.merged',
    source: 'github/github.com/open-conduit/open-conduit',
    facts: ['PR #137 merged', 'Work #142 is held until PR #137 merges'],
    steps: [
      { route: 'held', verdict: 'match', reason: 'github-hold matches: #142 is held on pull/137' },
      { route: 'issues', verdict: 'not-reached', reason: 'first match wins' },
    ],
    result: { effect: 'Release the hold', detail: 'The hold executor ends its wait; #142 is reclaimed by route issues on its next Event and starts its first agent turn.' },
  },
  {
    id: 'agent-reply',
    scopeId: 'support',
    label: 'The agent\'s own Slack reply comes back',
    type: 'slack.message.agent',
    source: 'slack/T0C49A2P55Z',
    facts: ['message posted by bot U0CC0GZUFFC', 'carries the agent reply marker'],
    steps: [{ route: 'conversations', verdict: 'match', reason: 'slack handler recognises the marker' }],
    result: { effect: 'Drop', detail: 'Marked agent replies are recorded and checkpointed but never answered, so the agent cannot reply to itself.' },
  },
  {
    id: 'unmentioned',
    scopeId: 'support',
    label: 'A message that doesn\'t mention the app',
    type: 'slack.message.human',
    source: 'slack/T0C49A2P55Z',
    facts: ['new top-level message in C0CC4DM066Q', 'no @mention of U0CC0GZUFFC', 'start: mention'],
    steps: [{ route: 'conversations', verdict: 'skip', reason: 'start is "mention" and the message does not mention the app' }],
    result: { effect: 'Unmatched', detail: 'No route matched. The Event is accepted and checkpointed as unmatched; open-conduit route explains it.' },
  },
]
