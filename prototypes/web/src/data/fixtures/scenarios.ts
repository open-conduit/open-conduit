// The prototype's world. Scopes, binding names, event families, toolbelt
// operations and outcome statuses are the real ones from the local config
// and daemon history; the work items are constructed so every state an
// operator meets is represented. Items marked `fixture` show states the
// local daemon has not produced.

import type { Work, Turn, TranscriptItem, TranscriptDraft, ExtLink, ExtAction, ExtPanel } from '../../domain/types'
import { ago, ahead, script, endOf, seg, ev, route, workId, hex64, GH, GHJ, SLACK, gh } from './build'

export const transcripts: Record<string, TranscriptItem[]> = {}
const REPO = 'open-conduit/open-conduit'
const REPOJ = 'open-conduit/open-conduit-jiminny'

const issue = (n: number, repo = REPO): ExtLink => ({ kind: 'issue', label: `Issue #${n}`, url: gh(repo, 'issues', n) })
const pr = (n: number, repo = REPO): ExtLink => ({ kind: 'pr', label: `PR #${n}`, url: gh(repo, 'pull', n) })
const branch = (n: number, repo = REPO): ExtLink => ({ kind: 'branch', label: `agent/issue-${n}`, url: `https://github.com/${repo}/tree/agent/issue-${n}` })
const thread = (ts: string): ExtLink => ({ kind: 'thread', label: 'Slack thread', url: `https://app.slack.com/client/T0C49A2P55Z/C0CC4DM066Q/thread/C0CC4DM066Q-${ts}` })

function turn(w: string, attempt: number, t: Partial<Turn> & Pick<Turn, 'state' | 'triggerEventId'>, items?: TranscriptItem[]): Turn {
  const id = `${w}/attempt/${attempt}`
  if (items) transcripts[id] = items
  const start = items?.[0]?.at ?? ago(0)
  return {
    id,
    attempt,
    binding: 'agent',
    executor: 'claude-code',
    model: 'claude-opus-5-5',
    generation: 3,
    createdAt: start,
    segments: [seg(start, t.state === 'running' ? undefined : items ? endOf(items) : start)],
    inputs: 1,
    ...t,
  }
}

const usage = (outK: number, cachedK: number, cost: number) => ({ inputTokens: 40 + Math.round(outK), outputTokens: Math.round(outK * 1000), cachedTokens: Math.round(cachedK * 1000), cost })

function ghWork(n: number, w: Partial<Work> & Pick<Work, 'title' | 'state' | 'summary' | 'disposition' | 'turns' | 'events'>, repo = REPO, ns = GH): Work {
  const scopeId = repo === REPO ? 'open-conduit' : 'open-conduit-jiminny'
  const created = w.events[0]?.occurredAt ?? ago(60)
  return {
    id: workId(`${repo}#${n}`),
    kind: 'github.issue',
    scopeId,
    revision: 4 + w.turns.length * 3,
    ref: `#${n}`,
    source: 'github',
    correlationKeys: [{ source: ns, externalId: `issue/${n}` }],
    createdAt: created,
    updatedAt: w.disposition.since,
    binding: 'agent',
    executor: 'claude-code',
    links: [issue(n, repo)],
    waitTypes: ['github.checks.passed', 'github.checks.failed', 'github.comment.human', 'github.pull_request.merged'],
    ...w,
  }
}

// Generic short transcript for items whose detail matters less.
function brief(minutesAgo: number, n: number, input: string, steps: [string, string][], effect: [string, string, ExtLink?] | null, outcome: ['completed' | 'handoff' | 'waiting', string, string]): TranscriptItem[] {
  return script(minutesAgo, [
    ['prompt', `Playbook work-issue · ${input}`, `You are continuing Work for issue #${n} in ${REPO}.\n\nInputs (1):\n- ${input}\n\nRead context with read_context, act through the toolbelt, and finish the turn with declare_outcome.`],
    ['system', 'Session started · claude-opus-5-5 · resumed from the previous turn'],
    ['effect', 'read_context', 'Read Work context', 0.4, undefined, '{"work":"github.issue","inputs":1}'],
    ...steps.map(([tool, label]) => ['tool', tool, label, 2 + label.length / 20, label, 'ok'] as ['tool', string, string, number, string, string]),
    ...(effect ? [['effect', effect[0], effect[1], 1.2, undefined, 'ok', effect[2]] as ['effect', string, string, number, string?, string?, ExtLink?]] : []),
    ['outcome', outcome[0], outcome[1], outcome[2]],
  ])
}

// ════════════════════════════════════════════════════════════════════════
// GitHub development: issue → PR → checks → review → merge → completed
// ════════════════════════════════════════════════════════════════════════

function work128(): Work {
  const w = workId(`${REPO}#128`)
  const exploreFiles = [
    ['Bash', 'rg -n "includeScopes|\\.open-conduit/config.json" internal'],
    ['Read', 'internal/config/manager.go'],
    ['Read', 'internal/config/json.go'],
    ['Bash', 'rg -n "type Scope struct" -A 30 internal/config/config.go'],
    ['Read', 'internal/config/config.go:140-260'],
    ['Bash', 'git log --oneline -8 -- internal/config/manager.go'],
    ['Read', 'internal/web/pages.go'],
    ['Read', 'internal/web/templates/scopes.html'],
    ['Read', 'internal/web/templates/config.html'],
    ['Bash', 'rg -n "Origin|origin" internal/config/*.go'],
    ['Read', 'internal/config/manager_test.go:1-120'],
    ['Bash', 'go doc ./internal/config Manager'],
  ] as const
  const t1 = script(1719, [
    ['prompt', 'Playbook work-issue · issue #128 opened', `You are working issue #128 in ${REPO} on branch agent/issue-128, in a git worktree of ~/Code/open-conduit.\n\nInputs (1):\n- github.issue.open #128 "Show which folder an included scope comes from" by @octo-dev\n\nFollow the work-issue playbook: comment that you have started, do the work, run make check, open a pull request, then wait on it with github.wait and end the turn with declare_outcome.`],
    ['system', 'Session started · claude-opus-5-5 · 58 tools · cwd …/workspaces/work-1f0c2a9e', 'MCP server open-conduit connected (toolbelt: read_context, emit, wait, declare_outcome, call)'],
    ['effect', 'read_context', 'Read Work context', 0.3, '{}', '{\n  "work": { "kind": "github.issue", "scope": "open-conduit", "revision": "2" },\n  "inputs": [{ "type": "github.issue.open", "issue": 128 }]\n}'],
    ['effect', 'github.issue.view', 'View issue #128', 0.9, '{ "number": 128 }', `#128 Show which folder an included scope comes from\n\nSince ea5f397 a scope can come from a working folder's .open-conduit/config.json. The Scopes page doesn't say which folder, so when two checkouts define a scope with the same name it's impossible to tell them apart.\n\nShow the folder under the scope name.`, issue(128)],
    ['effect', 'github.issue.comment', 'Comment on #128', 0.7, '{ "body": "Working on this on branch `agent/issue-128`. A PR will follow here." }', 'comment 2318840921 created'],
    ['agent', 'The composer in internal/config/manager.go merges included scopes but drops where each came from. I\'ll record the origin folder on the composed scope, keep it out of the persisted generation, and render it on the Scopes and Config pages.'],
    ...exploreFiles.map(([tool, label]) => ['tool', tool, label, 0.4 + label.length / 40, label, 'ok'] as ['tool', string, string, number, string, string]),
    ['agent', 'Origin needs to survive composition but not validation hashing, otherwise the generation would change when a checkout moves. I\'ll add an unexported `origin` field set by the composer and a `Scope.Origin()` accessor.'],
    ['tool', 'Edit', 'internal/config/manager.go', 0.2, `@@ func (m *Manager) compose(base File, folders []string) (File, error) {\n \tfor _, dir := range folders {\n \t\tinc, err := readIncluded(dir)\n \t\tif err != nil {\n \t\t\treturn File{}, fmt.Errorf("%s: %w", dir, err)\n \t\t}\n-\t\tbase.Scopes = append(base.Scopes, inc.Scopes...)\n+\t\tfor _, s := range inc.Scopes {\n+\t\t\ts.origin = dir\n+\t\t\tbase.Scopes = append(base.Scopes, s)\n+\t\t}\n \t}`, 'applied 1 hunk'],
    ['tool', 'Edit', 'internal/config/config.go', 0.2, `+// Origin is the working folder an included scope was composed from, or ""\n+// for a scope written in the main file. It is not part of the generation.\n+func (s Scope) Origin() string { return s.origin }`, 'applied 1 hunk'],
    ['tool', 'Edit', 'internal/web/templates/scopes.html', 0.2, `+{{if .Origin}}<p class="origin">from {{.Origin}}</p>{{end}}`, 'applied 1 hunk'],
    ['tool', 'Edit', 'internal/config/manager_test.go', 0.2, '+func TestComposeRecordsOrigin(t *testing.T) { … }', 'applied 1 hunk'],
    ['tool', 'Bash', 'go test ./internal/config/... ./internal/web/...', 14.2, 'go test ./internal/config/... ./internal/web/...', 'ok  \tgithub.com/open-conduit/open-conduit/internal/config\t6.912s\nok  \tgithub.com/open-conduit/open-conduit/internal/web\t3.204s'],
    ['tool', 'Bash', 'make check', 96.4, 'make check', 'gofmt … ok\ngo vet ./... ok\nstaticcheck ./... ok\nconfig-check ok\ngo test ./... (412 packages) ok\ncontracts gate ok'],
    ['tool', 'Bash', 'git commit -am "feat(web): show which folder an included scope comes from" && git push -u origin agent/issue-128', 3.1, undefined, '[agent/issue-128 2b7fab5] feat(web): show which folder an included scope comes from\n 4 files changed, 61 insertions(+), 3 deletions(-)\nbranch \'agent/issue-128\' set up to track \'origin/agent/issue-128\'.'],
    ['effect', 'github.pr.create', 'Open PR #131', 2.2, '{\n  "title": "feat(web): show which folder an included scope comes from",\n  "head": "agent/issue-128",\n  "body": "## What changed\\n\\nIncluded scopes record the working folder they were composed from…\\n\\nCloses #128"\n}', 'PR #131 opened', pr(131)],
    ['effect', 'wait', 'Wait on PR #131', 0.3, '{ "keys": ["pull/131"], "types": ["github.checks.*", "github.review.*", "github.comment.human", "github.pull_request.merged"] }', 'wait/5c1e… registered'],
    ['outcome', 'handoff', 'Opened PR #131: included scopes record the folder they were composed from; the Scopes and Config pages show it under the scope name.', 'building'],
  ])
  const t2 = script(1680, [
    ['prompt', 'Playbook work-issue · checks passed on PR #131', 'Inputs (1):\n- github.checks.passed on PR #131 (7 of 7)'],
    ['system', 'Session resumed · claude-opus-5-5'],
    ['effect', 'github.checks', 'Checks on PR #131', 0.8, '{ "pr": 131 }', 'build (ubuntu, 1.24.2)   success  4m12s\nbuild (ubuntu, stable)   success  4m03s\nbuild (macos, 1.24.2)    success  6m40s\nbuild (macos, stable)    success  6m22s\ncontracts                success  1m55s\ndocs                     success  0m48s\ncommitlint               success  0m09s'],
    ['effect', 'github.issue.comment', 'Post report on #128', 0.9, '{ "body": "PR #131 is ready for review. Checks: 7 of 7 passed…" }', 'comment created'],
    ['outcome', 'handoff', 'Checks passed on PR #131 (7 of 7). Posted the report on the issue; waiting on review.', 'review'],
  ])
  const t3 = script(399, [
    ['prompt', 'Playbook work-issue · review comment on PR #131', 'Inputs (1):\n- github.comment.human on PR #131 by @octo-dev'],
    ['system', 'Session resumed · claude-opus-5-5'],
    ['human', 'octo-dev', 'GitHub review comment', 'Nice. Can `config show` print the folder as well? Right now you only see it in the browser.'],
    ['agent', '`config show` prints the redacted file; I\'ll annotate included scopes with a `"$origin"` key in the output only, never in the written file.'],
    ['tool', 'Read', 'internal/cli/config.go', 0.3, 'internal/cli/config.go', '…412 lines'],
    ['tool', 'Edit', 'internal/cli/config.go', 0.2, `+\t\tif o := s.Origin(); o != "" {\n+\t\t\tout["$origin"] = o\n+\t\t}`, 'applied 1 hunk'],
    ['tool', 'Bash', 'go test ./internal/cli/... -run ConfigShow', 8.8, undefined, 'ok  \tgithub.com/open-conduit/open-conduit/internal/cli\t8.402s'],
    ['tool', 'Bash', 'git commit -am "feat(cli): show an included scope\'s folder in config show" && git push', 2.4, undefined, '[agent/issue-128 3e1c9f2] feat(cli): show an included scope\'s folder in config show'],
    ['effect', 'github.pr.reply', 'Reply to review comment', 0.8, '{ "body": "Done in 3e1c9f2: `config show` adds \\"$origin\\" to included scopes. It is never written to the file." }', 'reply created', pr(131)],
    ['outcome', 'handoff', 'Added the source folder to `config show` as an `$origin` key (3e1c9f2) and replied on the review; waiting on checks.', 'building'],
  ])
  const t4 = script(371, [
    ['prompt', 'Playbook work-issue · checks passed on PR #131', 'Inputs (1):\n- github.checks.passed on PR #131 (7 of 7) at 3e1c9f2'],
    ['effect', 'github.checks', 'Checks on PR #131', 0.7, '{ "pr": 131 }', '7 of 7 passed at 3e1c9f2'],
    ['outcome', 'handoff', 'Checks passed again at 3e1c9f2; waiting on merge.', 'review'],
  ])
  const t5 = script(127, [
    ['prompt', 'Playbook work-issue · PR #131 merged', 'Inputs (1):\n- github.pull_request.merged #131 by @octo-dev'],
    ['effect', 'github.issue.view', 'View issue #128', 0.6, '{ "number": 128 }', 'state: closed (closed by PR #131)'],
    ['outcome', 'completed', 'PR #131 merged and issue #128 closed; work complete.', 'finished'],
  ])
  const evs = [
    ev(1720, 'github.issue.open', GH, 'Issue #128 opened by @octo-dev', route('start', 'route issues matched an open issue with no skip label; created Work', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 1 }),
    ev(1697, 'github.pull_request.open', GH, 'PR #131 opened by the agent', route('drop', 'agent-authored delivery; marked and ignored', { route: 'issues', policy: 'inject' }), { actor: 'open-conduit[bot]' }),
    ev(1696, 'github.checks.pending', GH, '7 checks queued on PR #131', route('queue', 'matched the wait but pending checks do not wake it', { route: 'issues', binding: 'agent', policy: 'inject' })),
    ev(1680, 'github.checks.passed', GH, 'Checks passed on PR #131 · 7 of 7', route('wake', 'woke wait/5c1e… (github.checks.*); started turn 2', { route: 'issues', binding: 'agent', policy: 'inject' }), { attempt: 2 }),
    ev(400, 'github.comment.human', GH, '@octo-dev commented on PR #131', route('wake', 'woke the wait; started turn 3', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 3 }),
    ev(390, 'github.checks.pending', GH, '7 checks queued on PR #131 at 3e1c9f2', route('queue', 'pending checks do not wake the wait', { route: 'issues', binding: 'agent' })),
    ev(372, 'github.checks.passed', GH, 'Checks passed on PR #131 at 3e1c9f2 · 7 of 7', route('wake', 'woke the wait; started turn 4', { route: 'issues', binding: 'agent', policy: 'inject' }), { attempt: 4 }),
    ev(128, 'github.pull_request.merged', GH, 'PR #131 merged by @octo-dev', route('wake', 'woke the wait; started turn 5', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 5 }),
    ev(126, 'github.issue.closed', GH, 'Issue #128 closed by PR #131', route('terminal', 'Work already completed; checkpointed as terminal input', { route: 'issues' })),
  ]
  const turns: Turn[] = [
    turn(w, 1, { state: 'handoff', triggerEventId: evs[0].id, outcome: { kind: 'handoff', summary: 'Opened PR #131: included scopes record the folder they were composed from; the Scopes and Config pages show it under the scope name.', status: 'building' }, usage: usage(22.4, 3140, 1.84), workspace: { kind: 'git', ref: 'agent/issue-128', dir: '…/workspaces/work-1f0c2a9e' }, modelCalls: 31, toolCalls: 24 }, t1),
    turn(w, 2, { state: 'handoff', triggerEventId: evs[3].id, outcome: { kind: 'handoff', summary: 'Checks passed on PR #131 (7 of 7). Posted the report on the issue; waiting on review.', status: 'review' }, usage: usage(1.9, 410, 0.21), workspace: { kind: 'git', ref: 'agent/issue-128' }, modelCalls: 4, toolCalls: 2 }, t2),
    turn(w, 3, { state: 'handoff', triggerEventId: evs[4].id, outcome: { kind: 'handoff', summary: 'Added the source folder to `config show` as an `$origin` key (3e1c9f2) and replied on the review; waiting on checks.', status: 'building' }, usage: usage(6.2, 1220, 0.66), workspace: { kind: 'git', ref: 'agent/issue-128' }, modelCalls: 11, toolCalls: 6 }, t3),
    turn(w, 4, { state: 'handoff', triggerEventId: evs[6].id, outcome: { kind: 'handoff', summary: 'Checks passed again at 3e1c9f2; waiting on merge.', status: 'review' }, usage: usage(0.7, 380, 0.09), modelCalls: 2, toolCalls: 1 }, t4),
    turn(w, 5, { state: 'completed', triggerEventId: evs[7].id, outcome: { kind: 'completed', summary: 'PR #131 merged and issue #128 closed; work complete.', status: 'finished' }, usage: usage(0.5, 360, 0.07), modelCalls: 2, toolCalls: 1 }, t5),
  ]
  return ghWork(128, {
    title: 'Show which folder an included scope comes from',
    requester: 'octo-dev',
    state: 'completed',
    status: 'finished',
    summary: 'PR #131 merged and issue #128 closed; work complete.',
    disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'PR #131 merged and the issue closed', since: endOf(t5) },
    turns,
    events: evs,
    links: [issue(128), pr(131), branch(128)],
  })
}

// ════════════════════════════════════════════════════════════════════════
// Running now, with a review comment injected into the live session
// ════════════════════════════════════════════════════════════════════════

function work134(): Work {
  const w = workId(`${REPO}#134`)
  const t1 = brief(1499, 134, 'github.issue.open #134 "Explain why an event was dropped in `route`"', [
    ['Bash', 'rg -n "Disposition|reason" internal/runtime/routing.go'],
    ['Read', 'internal/cli/route.go'],
    ['Edit', 'internal/cli/route.go'],
    ['Edit', 'internal/runtime/routing.go'],
    ['Bash', 'go test ./internal/cli/... ./internal/runtime/...'],
    ['Bash', 'make check'],
  ], ['github.pr.create', 'Open PR #139', pr(139)], ['handoff', 'Opened PR #139: `route` prints the reason an Event was dropped (agent-authored, terminal Work, drop policy, hold) next to the route that dropped it.', 'building'])
  const t2 = script(8.6, [
    ['prompt', 'Playbook work-issue · checks failed on PR #139', 'Inputs (1):\n- github.checks.failed on PR #139 · build (ubuntu, stable) failed'],
    ['system', 'Session resumed · claude-opus-5-5 · cwd …/workspaces/work-77ab01c4'],
    ['effect', 'github.checks', 'Checks on PR #139', 0.9, '{ "pr": 139 }', 'build (ubuntu, 1.24.2)   success\nbuild (ubuntu, stable)   failure  TestRouteExplainDrop\nbuild (macos, 1.24.2)    success\nbuild (macos, stable)    success\ncontracts                success\ndocs                     success\ncommitlint               success'],
    ['tool', 'Bash', 'go test -run TestRouteExplainDrop -count=1 ./internal/cli/', 7.4, undefined, `--- FAIL: TestRouteExplainDrop (0.41s)\n    route_test.go:212: output mismatch\n        want: "dropped by route issues: agent-authored delivery"\n         got: "dropped by route issues: agent-authored delivery (marker 0x2d)"\nFAIL\nFAIL\tgithub.com/open-conduit/open-conduit/internal/cli\t0.612s`, true],
    ['agent', 'Go 1.26 changed `%v` for the marker type, so the reason now includes its raw value. The reason should never print the marker; I\'ll format it explicitly instead of relying on the default verb.'],
    ['tool', 'Read', 'internal/runtime/routing.go:410-470', 0.3, undefined, '…'],
    ['tool', 'Edit', 'internal/runtime/routing.go', 0.2, '-\treason := fmt.Sprintf("agent-authored delivery %v", m)\n+\treason := "agent-authored delivery"', 'applied 1 hunk'],
    ['gap', 40],
    ['orch', 'inject', 'Injected github.comment.human from @octo-dev into the running turn (route issues, policy inject)'],
    ['human', 'octo-dev', 'GitHub comment on PR #139', 'While you\'re in there: the drop-by-hold case should say which hold, not just "held".', true],
    ['agent', 'I\'ll include the hold\'s reason and when it was placed, e.g. "dropped: Work held since 14:02 (needs a decision on config format v2)".'],
    ['tool', 'Edit', 'internal/runtime/routing.go', 0.2, '+\tcase held:\n+\t\treason = fmt.Sprintf("Work held since %s (%s)", h.At.Format(time.Kitchen), h.Reason)', 'applied 1 hunk'],
    ['tool', 'Edit', 'internal/cli/route_test.go', 0.2, '+\t{name: "held", want: "dropped: Work held since 2:02PM (needs a decision)"},', 'applied 1 hunk'],
  ])
  const evs = [
    ev(1500, 'github.issue.open', GH, 'Issue #134 opened by @octo-dev', route('start', 'route issues matched; created Work', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 1 }),
    ev(1462, 'github.checks.pending', GH, '7 checks queued on PR #139', route('queue', 'pending checks do not wake the wait', { route: 'issues', binding: 'agent' })),
    ev(8.7, 'github.checks.failed', GH, 'Checks failed on PR #139 · build (ubuntu, stable)', route('wake', 'woke the wait; started turn 2', { route: 'issues', binding: 'agent', policy: 'inject' }), { attempt: 2 }),
    ev(7.3, 'github.comment.human', GH, '@octo-dev commented on PR #139', route('inject', 'turn 2 is running and route issues has policy inject; injected into the session', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 2 }),
  ]
  return ghWork(134, {
    title: 'Explain why an event was dropped in `open-conduit route`',
    requester: 'octo-dev',
    state: 'running',
    status: 'building',
    summary: 'Fixing TestRouteExplainDrop on Go stable; folding in the hold-reason review comment.',
    disposition: { group: 'running', kind: 'running', label: 'Running', detail: 'Turn 2 · fixing the failed ubuntu stable check; 1 comment injected', since: ago(8.6) },
    turns: [
      turn(w, 1, { state: 'handoff', triggerEventId: evs[0].id, outcome: { kind: 'handoff', summary: 'Opened PR #139: `route` prints the reason an Event was dropped next to the route that dropped it.', status: 'building' }, usage: usage(14.1, 2210, 1.22), workspace: { kind: 'git', ref: 'agent/issue-134' }, modelCalls: 19, toolCalls: 9 }, t1),
      turn(w, 2, { state: 'running', triggerEventId: evs[2].id, inputs: 2, workspace: { kind: 'git', ref: 'agent/issue-134', dir: '…/workspaces/work-77ab01c4' }, deadline: ahead(36.4), usage: usage(3.1, 640, 0.31), modelCalls: 7, toolCalls: 6 }, t2),
    ],
    events: evs,
    links: [issue(134), pr(139), branch(134)],
  })
}

/** Items the simulator appends to #134's running turn, seconds apart. */
export const liveTail134: TranscriptDraft[] = [
  { kind: 'tool', tool: 'Bash', label: 'go test -run TestRouteExplain -count=1 ./internal/cli/ ./internal/runtime/', input: 'go test -run TestRouteExplain -count=1 ./internal/cli/ ./internal/runtime/', output: 'ok  \tgithub.com/open-conduit/open-conduit/internal/cli\t0.588s\nok  \tgithub.com/open-conduit/open-conduit/internal/runtime\t2.913s', ok: true, ms: 9400 },
  { kind: 'tool', tool: 'Bash', label: 'make check', input: 'make check', output: 'gofmt ok · vet ok · staticcheck ok · go test ./... ok · contracts gate ok', ok: true, ms: 91200 },
  { kind: 'tool', tool: 'Bash', label: 'git commit -am "fix(route): format drop reasons explicitly and name the hold" && git push', output: '[agent/issue-134 9d04e7b] fix(route): format drop reasons explicitly and name the hold', ok: true, ms: 2600 },
  { kind: 'effect', operation: 'github.pr.reply', label: 'Reply to @octo-dev on PR #139', input: '{ "body": "Done in 9d04e7b: held drops now say since when and why." }', output: 'reply created', ok: true, ms: 800 },
  { kind: 'effect', operation: 'wait', label: 'Wait on PR #139', input: '{ "keys": ["pull/139"], "types": ["github.checks.*", "github.comment.human", "github.pull_request.merged"] }', output: 'wait registered', ok: true, ms: 300 },
  { kind: 'outcome', outcome: 'handoff', summary: 'Fixed TestRouteExplainDrop on Go stable by formatting drop reasons explicitly, and held drops now name the hold (9d04e7b); waiting on checks.', status: 'building' },
] satisfies TranscriptDraft[]

// ════════════════════════════════════════════════════════════════════════
// Failure investigation: workspace setup fails, retries exhaust
// ════════════════════════════════════════════════════════════════════════

function work117(): Work {
  const w = workId(`${REPO}#117`)
  const t1 = brief(2900, 117, 'github.issue.open #117 "perf: index pending input by correlation key"', [
    ['Read', 'internal/runtime/routing.go'],
    ['Bash', 'go test -bench BenchmarkPendingInputs ./internal/runtime/'],
    ['Edit', 'internal/runtime/routing.go'],
    ['Edit', 'storage/sqlite/schema.sql'],
    ['Bash', 'make check'],
  ], ['github.pr.create', 'Open PR #118', pr(118)], ['handoff', 'Opened PR #118: pending input is indexed by correlation key; BenchmarkPendingInputs at 10k events drops from 41ms to 0.6ms.', 'building'])
  const t2 = brief(1310, 117, 'github.checks.failed on PR #118 · build (macos, stable)', [
    ['Bash', 'go test -race -run TestPendingIndexConcurrent -count=20 ./internal/runtime/'],
    ['Edit', 'internal/runtime/routing.go'],
    ['Bash', 'make check'],
  ], ['github.pr.comment', 'Comment on PR #118', pr(118)], ['handoff', 'The macOS stable failure was a real race in the index rebuild; guarded it with the route lock (c41e2d0) and pushed; waiting on checks.', 'building'])

  const failing = (minutesAgo: number, n: number, of: number, last: boolean) =>
    script(minutesAgo, [
      ['system', `Attempt ${n + 2} claimed · lease epoch ${n + 2} · generation 3`],
      ['system', 'Workspace: reusing worktree agent/issue-117 at …/workspaces/work-8a1d9f03'],
      ['system', 'Workspace setup triggered: go.sum changed since the last setup (setup_when: go.sum)'],
      ['error', 'Workspace setup failed: `go mod download` exited 1', `$ go mod download\ngo: modernc.org/sqlite@v1.34.4: Get "https://proxy.golang.org/modernc.org/sqlite/@v/v1.34.4.mod": dial tcp: lookup proxy.golang.org: no such host\ngo: golang.org/x/sync@v0.10.0: Get "https://proxy.golang.org/golang.org/x/sync/@v/v0.10.0.mod": dial tcp: lookup proxy.golang.org: no such host\nexit status 1`],
      ['outcome', 'failed', 'Workspace setup failed before the executor started: go mod download could not resolve proxy.golang.org.'],
      ['orch', 'park', last ? `Retry budget exhausted (${n} of ${of} failures). Work stays ready and blocked until an operator retries it.` : `Retry scheduled with backoff · failure ${n} of ${of}`],
    ])
  const t3 = failing(232, 1, 3, false)
  const t4 = failing(230, 2, 3, false)
  const t5 = failing(225, 3, 3, true)
  const evs = [
    ev(2901, 'github.issue.open', GH, 'Issue #117 opened by @octo-dev', route('start', 'route issues matched; created Work', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 1 }),
    ev(1311, 'github.checks.failed', GH, 'Checks failed on PR #118 · build (macos, stable)', route('wake', 'woke the wait; started turn 2', { route: 'issues', binding: 'agent', policy: 'inject' }), { attempt: 2 }),
    ev(233, 'github.comment.human', GH, '@octo-dev commented on PR #118: "Rebase on main — #45 landed the unrouted-event index"', route('wake', 'woke the wait; started turn 3', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 3 }),
  ]
  const err = { code: 'workspace_setup_failed', message: 'go mod download: lookup proxy.golang.org: no such host' }
  const failTurn = (a: number, items: TranscriptItem[]) =>
    turn(w, a, { state: 'failed', triggerEventId: evs[2].id, outcome: { kind: 'failed', summary: 'Workspace setup failed before the executor started: go mod download could not resolve proxy.golang.org.' }, workspace: { kind: 'git', ref: 'agent/issue-117', dir: '…/workspaces/work-8a1d9f03' }, error: err, model: undefined }, items)
  return ghWork(117, {
    title: 'perf: index pending input by correlation key',
    requester: 'octo-dev',
    state: 'ready',
    status: 'building',
    summary: 'Workspace setup failed 3 times: go mod download could not resolve proxy.golang.org.',
    disposition: { group: 'attention', kind: 'repair', label: 'Retries exhausted', detail: 'Workspace setup failed 3 times · go mod download could not resolve proxy.golang.org', since: endOf(t5) },
    retry: { failureCount: 3, exhausted: true, blocked: true, blockedReason: 'retry budget exhausted: 3 consecutive failures' },
    turns: [
      turn(w, 1, { state: 'handoff', triggerEventId: evs[0].id, outcome: { kind: 'handoff', summary: 'Opened PR #118: pending input is indexed by correlation key; BenchmarkPendingInputs at 10k events drops from 41ms to 0.6ms.', status: 'building' }, usage: usage(18.2, 2900, 1.61), workspace: { kind: 'git', ref: 'agent/issue-117' }, modelCalls: 24, toolCalls: 12 }, t1),
      turn(w, 2, { state: 'handoff', triggerEventId: evs[1].id, outcome: { kind: 'handoff', summary: 'The macOS stable failure was a real race in the index rebuild; guarded it with the route lock (c41e2d0) and pushed; waiting on checks.', status: 'building' }, usage: usage(8.4, 1500, 0.83), workspace: { kind: 'git', ref: 'agent/issue-117' }, modelCalls: 12, toolCalls: 7 }, t2),
      failTurn(3, t3),
      failTurn(4, t4),
      failTurn(5, t5),
    ],
    events: evs,
    links: [issue(117), pr(118), branch(117)],
    fixture: true,
  })
}

/** What a simulated retry of #117 shows: setup succeeds now DNS is back. */
export const retryTail117: TranscriptDraft[] = [
  { kind: 'system', text: 'Attempt 6 claimed · lease epoch 6 · generation 3' },
  { kind: 'system', text: 'Workspace setup: go mod download … ok (11.2s)' },
  { kind: 'prompt', title: 'Playbook work-issue · review comment on PR #118', text: 'Inputs (1):\n- github.comment.human on PR #118 by @octo-dev: "Rebase on main — #45 landed the unrouted-event index"' },
  { kind: 'tool', tool: 'Bash', label: 'git fetch origin && git rebase origin/main', output: 'Successfully rebased and updated refs/heads/agent/issue-117.', ok: true, ms: 3800 },
  { kind: 'agent', text: '#45 already added an unrouted-event index, so most of this PR is now redundant. I\'ll drop the duplicate index and keep only the correlation-key lookup it doesn\'t cover.' },
  { kind: 'tool', tool: 'Bash', label: 'make check', output: 'all checks ok', ok: true, ms: 88000 },
  { kind: 'tool', tool: 'Bash', label: 'git push --force-with-lease', output: '+ c41e2d0...e0f19aa agent/issue-117 -> agent/issue-117 (forced update)', ok: true, ms: 2100 },
  { kind: 'effect', operation: 'github.pr.reply', label: 'Reply to @octo-dev on PR #118', output: 'reply created', ok: true, ms: 700 },
  { kind: 'outcome', outcome: 'handoff', summary: 'Rebased on main; dropped the index #45 already provides and kept the correlation-key lookup (e0f19aa); waiting on checks.', status: 'building' },
] satisfies TranscriptDraft[]

// ════════════════════════════════════════════════════════════════════════
// Slack support: investigate → ask a person → park → answer → reply → park
// ════════════════════════════════════════════════════════════════════════

function slackWork(seed: string, w: Partial<Work> & Pick<Work, 'title' | 'state' | 'summary' | 'disposition' | 'turns' | 'events'>, ts: string): Work {
  return {
    id: workId(seed),
    kind: 'slack.conversation',
    scopeId: 'support',
    revision: 3 + w.turns.length * 4,
    ref: 'thread',
    source: 'slack',
    correlationKeys: [{ source: SLACK, externalId: `channel/C0CC4DM066Q/thread/${ts}` }],
    createdAt: w.events[0].occurredAt,
    updatedAt: w.disposition.since,
    binding: 'support',
    executor: 'claude-code',
    links: [thread(ts)],
    waitTypes: ['slack.message.human'],
    ...w,
  }
}

export const refundAskContext = `Northwind Logistics is on the annual Team plan (14 seats), renewed 87 days ago.

The public billing FAQ says annual plans are non-refundable after 30 days. The internal Billing Policy (v4, §3.2) allows a pro-rata refund of unused full months when an annual plan is cancelled within 90 days of renewal, at a Support Lead's discretion.

§3.2 is marked internal. Sharing it commits us to a discretionary refund of roughly 9 months × 14 seats.`

function workRefund(): Work {
  const w = workId('slack:refund-northwind')
  const t1 = script(26, [
    ['prompt', 'Playbook support-conversation · new thread in #support', 'You are answering a Slack thread in #support (Socket Mode).\n\nInputs (1):\n- slack.message.human from Priya Raman (Northwind Logistics)\n\nUse slack.thread for history and the knowledge checkout for answers. Ask a person with human.ask before sharing anything internal, and post a holding reply before you ask.'],
    ['system', 'Session started · claude-opus-5-5 · cwd …/workspaces/support-3b19e2 (knowledge checkout open-conduit-testing)'],
    ['human', 'Priya Raman', 'Slack · #support', 'Hi — we\'re on the annual Team plan (14 seats) and want to cancel. We renewed in August. Is any of that refundable, or are we locked in until next August?'],
    ['effect', 'read_context', 'Read Work context', 0.3, undefined, '{ "work": "slack.conversation", "inputs": 1 }'],
    ['effect', 'slack.thread', 'Read thread history', 0.6, '{ "thread": "1792964751.025089" }', '1 message · Priya Raman (Northwind Logistics, shared channel)'],
    ['tool', 'Bash', 'rg -il "refund" kb/', 0.4, 'rg -il "refund" kb/', 'kb/public/faq-billing.md\nkb/billing/refund-policy.md\nkb/billing/plans/annual-team.md'],
    ['tool', 'Read', 'kb/public/faq-billing.md', 0.2, undefined, '## Can I get a refund?\nMonthly plans can be cancelled any time. Annual plans are refundable within 30 days of purchase or renewal; after that they run to the end of the term.'],
    ['tool', 'Read', 'kb/billing/refund-policy.md', 0.2, undefined, '---\nvisibility: internal\nowner: billing-ops\nversion: 4\n---\n…\n### 3.2 Early cancellation of annual plans\nAn annual plan cancelled within 90 days of its start or renewal may receive a pro-rata refund of the unused full months, at a Support Lead\'s discretion. Record the decision in the account notes.'],
    ['tool', 'Read', 'kb/billing/plans/annual-team.md', 0.2, undefined, 'Team (annual): billed per seat, renewal on the anniversary of first purchase.'],
    ['agent', 'The public FAQ says no refund after 30 days, but internal policy §3.2 allows a discretionary pro-rata refund within 90 days of renewal — Northwind is on day 87. Quoting §3.2 would share an internal document and commit us to a discretionary refund, so I\'ll ask before answering.'],
    ['effect', 'slack.reply', 'Post a holding reply', 0.7, '{ "text": "Thanks Priya — I\'m checking the refund terms for your renewal date now and will come back to you in this thread shortly." }', 'posted (marked as agent reply)', thread('1792964751.025089')],
    ['effect', 'human.ask', 'Ask: share the internal refund terms?', 0.4, JSON.stringify({ question: 'May I tell Northwind about the pro-rata refund in internal Billing Policy §3.2?', options: ['Yes — offer the pro-rata refund, without quoting the policy', 'Yes — offer it and quote §3.2', 'No — give the public answer only'], deadline: '4h' }, null, 2), 'ask a7f3c2 open · responders operator:local'],
    ['orch', 'park', 'Parked on ask a7f3c2 · waiting for human.answered · lease released'],
    ['outcome', 'waiting', 'Asked whether to offer Northwind the internal pro-rata refund (§3.2, day 87 of 90); holding reply posted; parked on the answer.'],
  ])
  const evs = [
    ev(26.2, 'slack.message.human', SLACK, 'Priya Raman started a thread in #support', route('start', 'route conversations matched a new thread; created Work', { route: 'conversations', binding: 'support', policy: 'inject' }), { actor: 'Priya Raman', attempt: 1 }),
    ev(23.4, 'slack.message.agent', SLACK, 'Agent posted a holding reply', route('drop', 'marked agent reply; never answered', { route: 'conversations' })),
    ev(23.2, 'human.ask', 'core.action', 'Ask a7f3c2 opened for operator:local', route('control', 'ask record created; the Work waits on human.answered', {})),
  ]
  // What the human extension returns for this Work: its Actions() and
  // Panel() (extensions/human/actions.go, annotate.go), as the API serves them.
  const ui = humanAsk({
    question: 'May I tell Northwind about the pro-rata refund in internal Billing Policy §3.2?',
    options: ['Yes — offer the pro-rata refund, without quoting the policy', 'Yes — offer it and quote §3.2', 'No — give the public answer only'],
    context: refundAskContext,
    responders: ['operator:local'],
    deadline: ahead(216),
  })
  const tr = turn(w, 1, { state: 'parked', binding: 'support', triggerEventId: evs[0].id, outcome: { kind: 'waiting', summary: 'Asked whether to offer Northwind the internal pro-rata refund (§3.2, day 87 of 90); holding reply posted; parked on the answer.' }, usage: usage(2.6, 168, 0.23), workspace: { kind: 'git', ref: 'support-3b19e2' }, modelCalls: 8, toolCalls: 7 }, t1)
  return slackWork('slack:refund-northwind', {
    title: 'Refund for an annual Team plan cancelled in month 3',
    requester: 'Priya Raman · Northwind Logistics',
    state: 'waiting',
    summary: tr.outcome!.summary,
    disposition: { group: 'attention', kind: 'decision', label: ui.panels[0].title, detail: ui.panels[0].sections[0].text ?? '', since: ago(23.2) },
    ...ui,
    waitTypes: ['human.answered'],
    turns: [tr],
    events: evs,
  }, '1792964751.025089')
}

export const HUMAN_PROVIDER = 'human/support/decisions'

/** human's Actions() and Panel() for an open ask, field for field. */
function humanAsk(q: { question: string; options: string[]; context?: string; responders: string[]; deadline?: string }): { extActions: ExtAction[]; panels: ExtPanel[] } {
  const numbered = q.options.map((o, i) => `${i + 1}. ${o}`).join('; ')
  let prompt = q.options.length ? `an option's number or text (${numbered})` : 'your answer'
  if (prompt.length > 200) prompt = "an option's number or text"
  const answer: ExtAction = { provider: HUMAN_PROVIDER, id: 'answer', label: "Answer the agent's question", prompt, choices: q.options.length ? q.options : undefined }
  const panel: ExtPanel = {
    provider: HUMAN_PROVIDER,
    id: 'question',
    title: 'The agent asks',
    sections: [
      { text: q.question },
      ...(q.context ? [{ title: 'Context', text: q.context }] : []),
      ...(q.options.length ? [{ title: 'Options', table: { columns: ['#', 'Option'], rows: q.options.map((o, i) => [String(i + 1), o]) } }] : []),
      { fields: [{ name: 'for', value: q.responders.join(', ') }, { name: 'status', value: 'waiting for an answer' }, ...(q.deadline ? [{ name: 'answer by', value: q.deadline }] : [])], actions: [answer] },
    ],
  }
  return { extActions: [answer], panels: [panel] }
}

/** After the answer: the parked Execution resumes in a new segment. */
export function refundResumeTail(answer: string): TranscriptDraft[] {
  const quote = answer.includes('quote')
  const decline = answer.startsWith('No')
  return [
    { kind: 'orchestration', tone: 'wake', text: 'Woken by human.answered · resumed the parked session' },
    { kind: 'human', author: 'operator:local', via: 'Answer via human.answer', text: answer },
    { kind: 'agent', text: decline ? 'I\'ll give the public answer: no refund after 30 days, the plan runs to term.' : `I'll offer the pro-rata refund for the 9 unused full months${quote ? ' and quote §3.2' : ' without quoting the internal policy'}, and say a Support Lead will confirm.` },
    {
      kind: 'effect',
      operation: 'slack.reply',
      label: 'Reply to Priya',
      input: JSON.stringify({ text: decline ? 'Annual plans are refundable within 30 days of renewal; after that the plan runs to the end of the term in August.' : 'Good news: because you\'re within 90 days of renewal, we can refund the 9 unused full months on all 14 seats. A Support Lead will confirm the amount here today.' }),
      output: 'posted (marked as agent reply)',
      ok: true,
      ms: 700,
    },
    { kind: 'effect', operation: 'wait', label: 'Wait on the thread', input: '{ "keys": ["thread/1792964751.025089"], "types": ["slack.message.human"] }', output: 'wait registered', ok: true, ms: 200 },
    { kind: 'orchestration', tone: 'park', text: 'Parked on the thread · waiting for slack.message.human' },
    { kind: 'outcome', outcome: 'waiting', summary: decline ? 'Gave the public refund answer; waiting on the thread.' : 'Offered the pro-rata refund of 9 unused months on 14 seats; a Support Lead will confirm; waiting on the thread.' },
  ] satisfies TranscriptDraft[]
}

function workDpa(): Work {
  const w = workId('slack:dpa-halden')
  const seg1 = script(312, [
    ['prompt', 'Playbook support-conversation · new thread in #support', 'Inputs (1):\n- slack.message.human from Tomás Ferreira (Halden Health)'],
    ['human', 'Tomás Ferreira', 'Slack · #support', 'Our procurement needs a countersigned DPA before renewal. Can you sign ours (attached, based on the 2026 EU SCCs)?'],
    ['effect', 'slack.thread', 'Read thread history', 0.5, undefined, '1 message, 1 attachment (Halden-DPA-2026.pdf)'],
    ['tool', 'Bash', 'rg -il "dpa|data processing" kb/', 0.4, undefined, 'kb/legal/dpa-requests.md\nkb/public/security-and-privacy.md'],
    ['tool', 'Read', 'kb/legal/dpa-requests.md', 0.2, undefined, 'visibility: internal\nCustomer paper goes to legal@ (5–10 days). Our standard pre-signed DPA (v2026.3) can be shared immediately.'],
    ['effect', 'slack.reply', 'Post a holding reply', 0.6, undefined, 'posted'],
    ['effect', 'human.ask', 'Ask: pre-signed DPA or legal review?', 0.4, undefined, 'ask 51d0e8 open'],
    ['orch', 'park', 'Parked on ask 51d0e8 · waiting for human.answered'],
  ])
  const seg2 = script(190, [
    ['orch', 'wake', 'Woken by human.answered (ask 51d0e8) · resumed the parked session'],
    ['human', 'operator:local', 'Answer to ask 51d0e8', 'Share the standard pre-signed DPA, and offer legal review of theirs.'],
    ['effect', 'slack.reply', 'Reply to Tomás', 0.8, '{ "text": "Here\'s our pre-signed DPA (v2026.3, 2026 EU SCCs)… If procurement needs your own paper, legal can review it in 5–10 working days." }', 'posted', thread('1792948802.117320')],
    ['effect', 'wait', 'Wait on the thread', 0.2, undefined, 'wait registered'],
    ['orch', 'park', 'Parked on the thread · waiting for slack.message.human'],
    ['outcome', 'waiting', 'Shared the pre-signed DPA (v2026.3) and offered legal review of Halden\'s own; waiting on the thread.'],
  ])
  const evs = [
    ev(312.2, 'slack.message.human', SLACK, 'Tomás Ferreira started a thread in #support', route('start', 'route conversations matched; created Work', { route: 'conversations', binding: 'support', policy: 'inject' }), { actor: 'Tomás Ferreira', attempt: 1 }),
    ev(309, 'human.ask', 'core.action', 'Ask 51d0e8 opened for operator:local', route('control', 'ask record created', {})),
    ev(190.1, 'human.answer', 'core.action', 'operator:local answered ask 51d0e8', route('wake', 'alias human.answered woke the wait; resumed attempt 1', { route: 'conversations', binding: 'support' }), { actor: 'operator:local', attempt: 1 }),
  ]
  const items = [...seg1, ...seg2]
  const tr = turn(w, 1, { state: 'parked', binding: 'support', triggerEventId: evs[0].id, outcome: { kind: 'waiting', summary: 'Shared the pre-signed DPA (v2026.3) and offered legal review of Halden\'s own; waiting on the thread.' }, usage: usage(2.2, 190, 0.2), modelCalls: 9, toolCalls: 7 }, items)
  tr.segments = [seg(seg1[0].at, endOf(seg1)), seg(seg2[0].at, endOf(seg2), evs[2].id)]
  return slackWork('slack:dpa-halden', {
    title: 'Countersign our DPA before renewal',
    requester: 'Tomás Ferreira · Halden Health',
    state: 'waiting',
    summary: tr.outcome!.summary,
    disposition: { group: 'waiting', kind: 'reply', label: 'Waiting for a reply', detail: 'Shared the pre-signed DPA; waiting for Tomás in the thread', since: endOf(seg2) },
    turns: [tr],
    events: evs,
    fixture: true,
  }, '1792948802.117320')
}

function workSso(): Work {
  const w = workId('slack:sso-loop')
  const t1 = script(214, [
    ['prompt', 'Playbook support-conversation · new thread in #support', 'Inputs (1):\n- slack.message.human from Marco Bellini (Fenwick & Ash)'],
    ['human', 'Marco Bellini', 'Slack · #support', 'Since we rotated our Okta signing cert this morning, everyone gets bounced back to the login page in a loop.'],
    ['tool', 'Read', 'kb/sso/troubleshooting.md', 0.2, undefined, '…A login loop right after certificate rotation usually means the new certificate was not uploaded on our side…'],
    ['effect', 'slack.reply', 'Ask for the SAML response and cert fingerprint', 0.8, '{ "text": "That loop usually means we still hold the old signing certificate. Could you send the new certificate\'s SHA-256 fingerprint and a HAR of one failed login?" }', 'posted', thread('1792960011.004410')],
    ['effect', 'wait', 'Wait on the thread', 0.2, undefined, 'wait registered'],
    ['orch', 'park', 'Parked on the thread · waiting for slack.message.human'],
    ['outcome', 'waiting', 'Likely stale IdP certificate on our side; asked Marco for the new fingerprint and a HAR; waiting on the thread.'],
  ])
  const evs = [ev(214.2, 'slack.message.human', SLACK, 'Marco Bellini started a thread in #support', route('start', 'route conversations matched; created Work', { route: 'conversations', binding: 'support', policy: 'inject' }), { actor: 'Marco Bellini', attempt: 1 })]
  return slackWork('slack:sso-loop', {
    title: 'Login loop after rotating the Okta signing certificate',
    requester: 'Marco Bellini · Fenwick & Ash',
    state: 'waiting',
    summary: 'Asked Marco for the new certificate fingerprint and a HAR of a failed login.',
    disposition: { group: 'waiting', kind: 'reply', label: 'Waiting for a reply', detail: 'Asked Marco for the certificate fingerprint and a HAR', since: endOf(t1) },
    turns: [turn(w, 1, { state: 'parked', binding: 'support', triggerEventId: evs[0].id, outcome: { kind: 'waiting', summary: 'Likely stale IdP certificate on our side; asked Marco for the new fingerprint and a HAR; waiting on the thread.' }, usage: usage(1.4, 120, 0.13), modelCalls: 5, toolCalls: 4 }, t1)],
    events: evs,
    fixture: true,
  }, '1792960011.004410')
}

function workWebhook410(): Work {
  const w = workId('slack:webhook-410')
  const t1 = script(2.4, [
    ['prompt', 'Playbook support-conversation · new thread in #support', 'Inputs (1):\n- slack.message.human from Aisha Okafor (Brightline)'],
    ['human', 'Aisha Okafor', 'Slack · #support', 'Our webhook endpoint has been getting 410 Gone from your delivery retries since about 09:00. Did something change?'],
    ['effect', 'slack.thread', 'Read thread history', 0.5, undefined, '1 message'],
    ['tool', 'Bash', 'rg -n "410" kb/webhooks/', 0.4, undefined, 'kb/webhooks/delivery.md:88: A 410 from your endpoint disables the subscription…'],
    ['tool', 'Read', 'kb/webhooks/delivery.md', 0.2, undefined, '…'],
  ])
  const evs = [ev(2.5, 'slack.message.human', SLACK, 'Aisha Okafor started a thread in #support', route('start', 'route conversations matched; created Work', { route: 'conversations', binding: 'support', policy: 'inject' }), { actor: 'Aisha Okafor', attempt: 1 })]
  return slackWork('slack:webhook-410', {
    title: 'Webhook deliveries returning 410 since this morning',
    requester: 'Aisha Okafor · Brightline',
    state: 'running',
    summary: 'Reading the webhook delivery docs on 410 handling.',
    disposition: { group: 'running', kind: 'running', label: 'Running', detail: 'Turn 1 · reading the webhook delivery docs on 410 handling', since: ago(2.4) },
    turns: [turn(w, 1, { state: 'running', binding: 'support', triggerEventId: evs[0].id, deadline: ahead(57.6), usage: usage(0.6, 40, 0.05), modelCalls: 3, toolCalls: 3 }, t1)],
    events: evs,
    fixture: true,
  }, '1792965102.331908')
}

function workAuditExport(): Work {
  const w = workId('slack:audit-export')
  const t1 = script(1460, [
    ['prompt', 'Playbook support-conversation · new thread in #support', 'Inputs (1):\n- slack.message.human from Jonas Weber (Kestrel)'],
    ['human', 'Jonas Weber', 'Slack · #support', 'How do I export the audit log for the last 90 days?'],
    ['tool', 'Read', 'kb/public/audit-log.md', 0.2, undefined, 'Settings → Security → Audit log → Export (CSV, up to 365 days)'],
    ['effect', 'slack.reply', 'Answer Jonas', 0.7, undefined, 'posted'],
    ['orch', 'park', 'Parked on the thread'],
  ])
  const t2 = script(1402, [
    ['orch', 'wake', 'Woken by slack.message.human · resumed'],
    ['human', 'Jonas Weber', 'Slack · #support', 'Found it, thanks!'],
    ['outcome', 'completed', 'Pointed Jonas to Settings → Security → Audit log → Export; he confirmed it worked.'],
  ])
  const evs = [
    ev(1460.2, 'slack.message.human', SLACK, 'Jonas Weber started a thread', route('start', 'created Work', { route: 'conversations', binding: 'support', policy: 'inject' }), { actor: 'Jonas Weber', attempt: 1 }),
    ev(1402.1, 'slack.message.human', SLACK, 'Jonas Weber replied: "Found it, thanks!"', route('wake', 'woke the wait; resumed attempt 1', { route: 'conversations', binding: 'support' }), { actor: 'Jonas Weber', attempt: 1 }),
  ]
  const tr = turn(w, 1, { state: 'completed', binding: 'support', triggerEventId: evs[0].id, outcome: { kind: 'completed', summary: 'Pointed Jonas to Settings → Security → Audit log → Export; he confirmed it worked.' }, usage: usage(0.9, 80, 0.08), modelCalls: 4, toolCalls: 2 }, [...t1, ...t2])
  tr.segments = [seg(t1[0].at, endOf(t1)), seg(t2[0].at, endOf(t2), evs[1].id)]
  return slackWork('slack:audit-export', {
    title: 'Exporting 90 days of audit log',
    requester: 'Jonas Weber · Kestrel',
    state: 'completed',
    summary: tr.outcome!.summary,
    disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'Answered; Jonas confirmed it worked', since: endOf(t2) },
    turns: [tr],
    events: evs,
    fixture: true,
  }, '1792878870.550120')
}

// ════════════════════════════════════════════════════════════════════════
// The rest of the board: waiting on CI, review, merge, time, other work;
// queued for capacity; held; rebind needed; completed; cancelled
// ════════════════════════════════════════════════════════════════════════

function simpleGh(n: number, title: string, opts: {
  minutesAgo: number
  disposition: Work['disposition']
  state: Work['state']
  summary: string
  status?: string
  turns?: number
  outcomeKind?: 'handoff' | 'completed'
  prNum?: number
  repo?: string
  extra?: Partial<Work>
}): Work {
  const repo = opts.repo ?? REPO
  const ns = repo === REPO ? GH : GHJ
  const w = workId(`${repo}#${n}`)
  const count = opts.turns ?? 1
  const turns: Turn[] = []
  const evs = [ev(opts.minutesAgo + 1, 'github.issue.open', ns, `Issue #${n} opened by @octo-dev`, route('start', 'route issues matched; created Work', { route: 'issues', binding: 'agent', policy: 'inject' }), { actor: 'octo-dev', attempt: 1 })]
  const span = Math.max(opts.minutesAgo - (opts.disposition.group === 'closed' ? 0 : 30), 10)
  for (let i = 1; i <= count; i++) {
    const start = opts.minutesAgo - ((i - 1) * span) / count
    const last = i === count
    const kind = last ? opts.outcomeKind ?? 'handoff' : 'handoff'
    const summary = last ? opts.summary : i === 1 && opts.prNum ? `Opened PR #${opts.prNum}.` : 'Addressed the new input and pushed; waiting on checks.'
    if (i > 1) evs.push(ev(start + 0.2, i === count && kind === 'completed' ? 'github.pull_request.merged' : 'github.checks.passed', ns, i === count && kind === 'completed' ? `PR #${opts.prNum} merged` : `Checks passed on PR #${opts.prNum}`, route('wake', `woke the wait; started turn ${i}`, { route: 'issues', binding: 'agent', policy: 'inject' }), { attempt: i }))
    const items = brief(start, n, i === 1 ? `github.issue.open #${n} "${title}"` : evs[evs.length - 1].summary, i === 1 ? [['Bash', 'rg -n "' + title.split(' ')[1] + '" internal'], ['Edit', 'internal/…'], ['Bash', 'make check']] : [['Bash', 'go test ./...']], i === 1 && opts.prNum ? ['github.pr.create', `Open PR #${opts.prNum}`, pr(opts.prNum, repo)] : null, [kind, summary, last ? opts.status ?? 'building' : 'building'])
    turns.push(turn(w, i, { state: kind === 'completed' ? 'completed' : 'handoff', triggerEventId: evs[evs.length - 1].id, outcome: { kind, summary, status: last ? opts.status : 'building' }, usage: usage(4 + n % 7, 700 + (n % 5) * 200, 0.35 + (n % 9) / 10), workspace: { kind: 'git', ref: `agent/issue-${n}` }, modelCalls: 6 + (n % 8), toolCalls: 4 + (n % 6) }, items))
  }
  return ghWork(n, {
    title,
    requester: 'octo-dev',
    state: opts.state,
    status: opts.status,
    summary: opts.summary,
    disposition: opts.disposition,
    turns,
    events: evs,
    links: opts.prNum ? [issue(n, repo), pr(opts.prNum, repo), branch(n, repo)] : [issue(n, repo)],
    ...opts.extra,
  }, repo, ns)
}

function scheduleWork(name: string, title: string, executor: 'shell' | 'claude-code', runs: { minutesAgo: number; secs: number; ok: boolean; summary: string; detail?: string }[], next: number, kind = 'schedule.job'): Work {
  const w = workId('schedule:' + name)
  const evs = runs.map((r, i) => ev(r.minutesAgo + 0.01, 'schedule.fire', 'schedule/nightly', `${name} fired (${i === runs.length - 1 ? 'latest' : 'earlier'})`, route('start', `route jobs matched schedule ${name}; queued a run of the standing Work`, { route: 'jobs', binding: executor === 'shell' ? 'job' : 'triage', policy: 'queue' }), { attempt: i + 1 }))
  const turns = runs.map((r, i) =>
    turn(
      w,
      i + 1,
      {
        state: r.ok ? 'completed' : 'failed',
        binding: executor === 'shell' ? 'job' : 'triage',
        executor,
        model: executor === 'shell' ? undefined : 'claude-opus-5-5',
        triggerEventId: evs[i].id,
        outcome: { kind: r.ok ? 'completed' : 'failed', summary: r.summary },
        error: r.ok ? undefined : { code: 'exit_status', message: r.summary },
        usage: executor === 'shell' ? undefined : usage(3.2, 420, 0.38),
      },
      executor === 'shell'
        ? script(r.minutesAgo, [
            ['system', `Run ${i + 1} · shell executor · timeout 10m`],
            ['tool', 'Shell', r.detail ?? name, r.secs, r.detail, r.ok ? 'exit status 0' : r.summary, !r.ok],
            ...(r.ok ? [] : [['error', r.summary, r.detail] as ['error', string, string?]]),
            ['outcome', r.ok ? 'completed' : 'failed', r.summary],
          ])
        : script(r.minutesAgo, [
            ['prompt', 'Playbook daily-triage', 'Read the problem reports and corrections agents filed in the last day, group them, and file an issue for each actionable cluster.'],
            ['effect', 'introspection.reports', 'Read 6 problem reports', 0.8, undefined, '6 reports · 3 env, 2 tool, 1 docs'],
            ['agent', 'Two clusters are actionable: agents keep hitting a missing .venv-contracts in fresh worktrees (3 reports), and workspace setup has no retry for transient network failures (2 reports).'],
            ['effect', 'github.issue.create', 'File #140', 1.1, undefined, 'issue #140 created', issue(140)],
            ['effect', 'github.issue.create', 'File #141', 1.0, undefined, 'issue #141 created', issue(141)],
            ['outcome', 'completed', r.summary],
          ]),
    ),
  )
  const last = runs[runs.length - 1]
  return {
    id: w,
    kind,
    scopeId: 'nightly',
    state: 'waiting',
    revision: 2 + runs.length * 3,
    title,
    ref: name,
    source: kind === 'introspection.triage' ? 'introspection' : 'schedule',
    correlationKeys: [{ source: 'schedule/nightly', externalId: `job/${name}` }],
    createdAt: ago(runs[0].minutesAgo + 2),
    updatedAt: ago(last.minutesAgo - last.secs / 60),
    binding: executor === 'shell' ? 'job' : 'triage',
    executor,
    summary: last.summary,
    disposition: {
      group: 'waiting',
      kind: 'time',
      label: last.ok ? 'Scheduled' : 'Scheduled · last run failed',
      detail: `Next run ${new Date(Date.now() + next * 60_000).toLocaleString('en-GB', { weekday: 'short', hour: '2-digit', minute: '2-digit' })}`,
      since: ago(last.minutesAgo - last.secs / 60),
    },
    turns,
    events: evs,
    links: [{ kind: 'job', label: `schedule ${name}`, url: '#/config/nightly' }],
    waitTypes: ['schedule.fire'],
    fixture: true,
  }
}

function hourOfDayAgo(h: number) {
  // minutes ago of the most recent hh:00 local time
  const now = new Date()
  const t = new Date(now)
  t.setHours(h, 0, 0, 0)
  if (t > now) t.setDate(t.getDate() - 1)
  return (now.getTime() - t.getTime()) / 60_000
}
function untilHour(h: number) {
  return 24 * 60 - hourOfDayAgo(h)
}

export function buildWorld(): Work[] {
  const backupAt = hourOfDayAgo(3)
  const auditAt = hourOfDayAgo(6)
  const triageAt = hourOfDayAgo(7)
  const all: Work[] = [
    work134(),
    workWebhook410(),
    workRefund(),
    work117(),
    simpleGh(109, 'Add `--json` to `events chain`', {
      minutesAgo: 610,
      state: 'ready',
      summary: 'Queued input is pinned to binding agent-sonnet, which generation 3 removed.',
      disposition: { group: 'attention', kind: 'repair', label: 'Needs rebinding', detail: 'Queued input is pinned to agent-sonnet, which generation 3 removed', since: ago(118) },
      turns: 1,
      prNum: 112,
      extra: { pinnedBinding: 'agent-sonnet', binding: 'agent-sonnet', fixture: true },
    }),
    simpleGh(136, 'TUI: show a held Work\'s reason in the list', {
      minutesAgo: 96,
      state: 'waiting',
      summary: 'Opened PR #137; waiting on checks.',
      disposition: { group: 'waiting', kind: 'ci', label: 'Waiting for CI', detail: '3 of 7 checks finished on PR #137 · macOS legs running', since: ago(61) },
      prNum: 137,
    }),
    simpleGh(121, 'Show live activity in `runs log --follow`', {
      minutesAgo: 1890,
      turns: 3,
      state: 'waiting',
      status: 'review',
      summary: 'Checks passed on PR #125 and it is approved; waiting on merge.',
      disposition: { group: 'waiting', kind: 'merge', label: 'Waiting for merge', detail: 'PR #125 approved by @octo-dev · checks passed', since: ago(205) },
      prNum: 125,
    }),
    simpleGh(138, 'Document the webhook tunnel fallback', {
      minutesAgo: 340,
      turns: 2,
      state: 'waiting',
      status: 'review',
      summary: 'Checks passed on PR #143; waiting on review.',
      disposition: { group: 'waiting', kind: 'review', label: 'Waiting for review', detail: 'PR #143 · review requested from @octo-dev', since: ago(270) },
      prNum: 143,
    }),
    simpleGh(135, 'Bump the Go floor once 1.26 is the stable leg', {
      minutesAgo: 4300,
      state: 'waiting',
      summary: 'Parked until issue #130 (CI matrix rework) closes.',
      disposition: { group: 'waiting', kind: 'event', label: 'Waiting on other work', detail: 'Wakes when issue #130 closes', since: ago(4280) },
      extra: { fixture: true },
    }),
    simpleGh(144, 'Re-run the flaky-test census', {
      minutesAgo: 1200,
      state: 'waiting',
      summary: 'Census filed on #144; parked until Monday 09:00 for the next run.',
      disposition: { group: 'waiting', kind: 'time', label: 'Waiting until a time', detail: 'Wakes Mon 09:00 (timer)', since: ago(1180) },
      extra: { fixture: true },
    }),
    simpleGh(140, 'Fresh worktrees miss .venv-contracts for `make check`', {
      minutesAgo: triageAt - 4,
      turns: 0,
      state: 'ready',
      summary: 'Filed by daily-triage from 3 problem reports; waiting for an agent slot.',
      disposition: { group: 'queued', kind: 'queued', label: 'Queued', detail: 'Waiting for an agent slot · 2 of 2 in use', since: ago(triageAt - 4) },
      extra: { requester: 'daily-triage', fixture: true },
    }),
    simpleGh(141, 'Retry workspace setup on transient network failures', {
      minutesAgo: triageAt - 5,
      turns: 0,
      state: 'ready',
      summary: 'Filed by daily-triage from 2 problem reports; waiting for an agent slot.',
      disposition: { group: 'queued', kind: 'queued', label: 'Queued', detail: 'Waiting for an agent slot · 2 of 2 in use', since: ago(triageAt - 5) },
      extra: { requester: 'daily-triage', fixture: true },
    }),
    simpleGh(98, 'Config format v2: nested scopes', {
      minutesAgo: 3100,
      turns: 0,
      state: 'ready',
      summary: 'Claimed and held by the operator before any turn started.',
      disposition: { group: 'held', kind: 'held', label: 'Held', detail: '“Needs a decision on config format v2 first” · operator:local', since: ago(2960) },
      extra: { hold: { reason: 'Needs a decision on config format v2 first', at: ago(2960), by: 'operator:local' } },
    }),
    simpleGh(142, 'Drop the deprecated admission_limit setting', {
      minutesAgo: 80,
      turns: 0,
      state: 'waiting',
      summary: 'Held by route held until PR #137 merges.',
      disposition: { group: 'held', kind: 'held', label: 'Held until PR #137 merges', detail: 'github.hold · route held · binding hold', since: ago(79) },
      extra: { kind: 'github.issue.held', binding: 'hold', executor: 'github-hold', fixture: true },
    }),
    work128(),
    simpleGh(31, 'Make `init --with claude` the quick start', { minutesAgo: 2700, turns: 3, state: 'completed', outcomeKind: 'completed', status: 'finished', prNum: 32, summary: 'PR #32 merged and issue #31 closed: `init --with claude` is the quick start in the README and the docs site.', disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'PR #32 merged', since: ago(1800) } }),
    simpleGh(25, 'Add a Starlight user docs site published to GitHub Pages', { minutesAgo: 5200, turns: 4, state: 'completed', outcomeKind: 'completed', status: 'finished', prNum: 26, summary: 'PR #26 merged and issue #25 closed; the docs site deploys from main.', disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'PR #26 merged', since: ago(3900) } }),
    simpleGh(22, 'Jira: drop the claimed label once completed Work\'s issue closes', { minutesAgo: 6100, turns: 2, state: 'completed', outcomeKind: 'completed', status: 'finished', prNum: 23, summary: 'PR #23 merged and issue #22 closed: Jira issues drop the claimed label once completed Work\'s issue closes.', disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'PR #23 merged', since: ago(5400) } }),
    simpleGh(13, 'Limit scheduled jobs and agent turns separately', { minutesAgo: 7400, turns: 3, state: 'completed', outcomeKind: 'completed', status: 'finished', prNum: 21, summary: 'PR #21 merged and issue #13 closed; separate agent and job concurrency limits are on main.', disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'PR #21 merged', since: ago(6500) } }),
    simpleGh(99, 'Try the Bubble Tea v2 upgrade', {
      minutesAgo: 4000,
      turns: 1,
      state: 'cancelled',
      prNum: 100,
      summary: 'Cancelled by the operator: superseded by #106.',
      disposition: { group: 'closed', kind: 'cancelled', label: 'Cancelled', detail: 'Superseded by #106 · operator:local', since: ago(3700) },
    }),
    simpleGh(7, 'Port the scheduler fairness fix', {
      repo: REPOJ,
      minutesAgo: 520,
      turns: 2,
      state: 'waiting',
      status: 'review',
      prNum: 8,
      summary: 'Checks passed on PR #8; waiting on review.',
      disposition: { group: 'waiting', kind: 'review', label: 'Waiting for review', detail: 'PR #8 · no reviewer assigned yet', since: ago(430) },
    }),
    simpleGh(9, 'Vendor the Jira field mapping', {
      repo: REPOJ,
      minutesAgo: 75,
      state: 'waiting',
      prNum: 10,
      summary: 'Opened PR #10; waiting on checks.',
      disposition: { group: 'waiting', kind: 'ci', label: 'Waiting for CI', detail: '0 of 4 checks started on PR #10 · queued on runners', since: ago(52) },
    }),
    simpleGh(4, 'Pin the jiminny Go toolchain', { repo: REPOJ, minutesAgo: 3300, turns: 2, state: 'completed', outcomeKind: 'completed', status: 'finished', prNum: 5, summary: 'PR #5 merged and issue #4 closed.', disposition: { group: 'closed', kind: 'completed', label: 'Completed', detail: 'PR #5 merged', since: ago(2900) } }),
    workDpa(),
    workSso(),
    workAuditExport(),
    scheduleWork('state-backup', 'Back up the daemon state', 'shell', [
      { minutesAgo: backupAt + 2880, secs: 38, ok: true, summary: 'Backup bundle written · 41.2 MB', detail: 'open-conduit backup create --to ~/Backups/open-conduit' },
      { minutesAgo: backupAt + 1440, secs: 40, ok: true, summary: 'Backup bundle written · 41.9 MB', detail: 'open-conduit backup create --to ~/Backups/open-conduit' },
      { minutesAgo: backupAt, secs: 41, ok: true, summary: 'Backup bundle written · 42.6 MB', detail: 'open-conduit backup create --to ~/Backups/open-conduit' },
    ], untilHour(3)),
    scheduleWork('dependency-audit', 'Audit Go dependencies for vulnerabilities', 'shell', [
      { minutesAgo: auditAt + 1440, secs: 52, ok: true, summary: 'No vulnerabilities found', detail: 'govulncheck ./...' },
      { minutesAgo: auditAt, secs: 49, ok: false, summary: 'exit status 3: GO-2026-4127 in golang.org/x/net@v0.31.0 (fixed in v0.33.0)', detail: 'govulncheck ./...' },
    ], untilHour(6)),
    scheduleWork('daily-triage', 'Triage agent problem reports', 'claude-code', [
      { minutesAgo: triageAt + 1440, secs: 140, ok: true, summary: 'Filed #133 from 2 problem reports; folded 4 corrections into CLAUDE.md.' },
      { minutesAgo: triageAt, secs: 164, ok: true, summary: 'Filed #140 and #141 from 6 problem reports.' },
    ], untilHour(7), 'introspection.triage'),
  ]
  // Sequences follow acceptance time across the whole store.
  const evs = all.flatMap((w) => w.events).sort((a, b) => a.occurredAt.localeCompare(b.occurredAt))
  evs.forEach((e, i) => (e.sequence = 4100 + i))
  return all
}

export const shortId = (id: string) => id.replace(/^work\//, '').slice(0, 8)
export { hex64 }

/** What a queued issue's first turn does once it gets a slot. */
export function firstTurnTail(w: Work, attempt: number): TranscriptDraft[] {
  const n = w.ref.slice(1)
  return [
    { kind: 'system', text: `Attempt ${attempt} claimed · lease epoch 1 · generation 3` },
    { kind: 'system', text: `Workspace: new worktree agent/issue-${n} · setup go mod download … ok` },
    { kind: 'prompt', title: `Playbook work-issue · issue ${w.ref} opened`, text: `Inputs (1):\n- github.issue.open ${w.ref} "${w.title}" filed by daily-triage` },
    { kind: 'effect', operation: 'github.issue.view', label: `View issue ${w.ref}`, output: w.title, ok: true, ms: 800 },
    { kind: 'effect', operation: 'github.issue.comment', label: `Comment on ${w.ref}`, input: `{ "body": "Working on this on branch \`agent/issue-${n}\`." }`, output: 'comment created', ok: true, ms: 700 },
    { kind: 'agent', text: 'make check bootstraps .venv-contracts only through `make config-deps`, which fresh worktrees never run. I\'ll make the config-check target depend on it.' },
    { kind: 'tool', tool: 'Edit', label: 'Makefile', input: '-config-check:\n+config-check: config-deps', output: 'applied 1 hunk', ok: true, ms: 200 },
    { kind: 'tool', tool: 'Bash', label: 'make check', output: 'all checks ok', ok: true, ms: 84000 },
    { kind: 'effect', operation: 'github.pr.create', label: 'Open a pull request', output: 'PR opened', ok: true, ms: 2100 },
    { kind: 'outcome', outcome: 'handoff', summary: 'Opened a PR: config-check depends on config-deps, so fresh worktrees bootstrap .venv-contracts; waiting on checks.', status: 'building' },
  ]
}
