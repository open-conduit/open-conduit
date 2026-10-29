// What an extension action does is the extension's business. In the
// prototype, fixtures register a stand-in per provider and action; the
// store applies the result through core semantics only (record the Event,
// wake the Wait it matches, resume a parked attempt). Live, this is the
// provider's Invoke behind POST …/actions/{provider}/{action}.

import type { Disposition, TranscriptDraft, Work } from '../domain/types'

export interface ActionResult {
  /** The provider's short, secret-free message to the caller */
  message: string
  /** The Event the action produces (accepted under core.action) */
  event: { type: string; summary: string }
  /** The Event matches the Work's Wait: the parked attempt resumes */
  resumes?: { tail: TranscriptDraft[]; then: Omit<Disposition, 'since'> }
}

export type ActionSim = (input: string, work?: Work) => ActionResult

const sims = new Map<string, ActionSim>()

/** Register by action id; `provider` may be '*' to match any provider. */
export function registerActionSim(provider: string, action: string, sim: ActionSim) {
  sims.set(provider + '\n' + action, sim)
}

export function simFor(provider: string, action: string): ActionSim | undefined {
  return sims.get(provider + '\n' + action) ?? sims.get('*\n' + action)
}
