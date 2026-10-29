// Stand-ins for what extension actions do in the fixture world. Each
// mirrors the real provider's Invoke: the Event it produces and, when that
// Event matches the Work's Wait, what the resumed attempt does.

import { registerActionSim } from '../actionSim'
import { HUMAN_PROVIDER, refundResumeTail } from './scenarios'

registerActionSim(HUMAN_PROVIDER, 'answer', (input) => ({
  message: 'Answer recorded.',
  event: { type: 'human.answer', summary: `operator:local answered: “${input}”` },
  resumes: { tail: refundResumeTail(input), then: { group: 'waiting', kind: 'reply', label: 'Waiting for a reply', detail: 'Replied in the thread; parked until the next message' } },
}))

// github's scope action (extensions/github/actions.go): claim an issue by hand.
registerActionSim('*', 'claim', (input) => ({
  message: `Claimed issue #${input.replace(/^#/, '')}. Its first turn starts on the next routing pass.`,
  event: { type: 'github.claim', summary: `claimed #${input} by hand` },
}))
