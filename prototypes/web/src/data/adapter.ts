// The boundary between presentation and data. Views only ever see the view
// models in domain/types; an adapter produces them. Two exist:
//   fixture — the constructed world in fixtures/, fully interactive
//   live    — read-only GETs against the local daemon through the dev
//             server's bridge (vite.config.ts), mapped to the same models

import type { Work, ConfigSnapshot, DaemonHealth, TranscriptItem } from '../domain/types'
import { buildWorld, transcripts } from './fixtures/scenarios'
import { configFixture } from './fixtures/config'
import { ago } from './fixtures/build'
import { loadLive, liveTranscript } from './live'
import './fixtures/effects'

export interface Snapshot {
  works: Work[]
  config: ConfigSnapshot
  health: DaemonHealth
}

export interface ConduitAdapter {
  readonly mode: 'fixture' | 'live'
  load(): Promise<Snapshot>
  transcript(work: Work, turnId: string): Promise<TranscriptItem[]>
}

export type Condition = 'normal' | 'slow' | 'unreachable' | 'fresh'

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

export function fixtureAdapter(condition: Condition): ConduitAdapter {
  const latency = condition === 'slow' ? 2400 : 380
  return {
    mode: 'fixture',
    async load() {
      await sleep(latency)
      if (condition === 'unreachable') {
        throw new Error('dial unix ~/.local/state/open-conduit/management.sock: connect: no such file or directory')
      }
      if (condition === 'fresh') {
        return {
          works: [],
          config: { ...configFixture, generation: 0, scopes: [], connections: [], appliedAt: '', fileMatchesGeneration: false },
          health: { state: 'ready', version: '1.5.0', generation: 0, agentSlots: { used: 0, total: 2 }, jobSlots: { used: 0, total: 1 }, tunnel: 'none', lastReconcile: '' },
        }
      }
      return {
        works: buildWorld(),
        config: structuredClone(configFixture),
        health: { state: 'ready', version: '1.5.0', generation: 3, agentSlots: { used: 2, total: 2 }, jobSlots: { used: 0, total: 1 }, tunnel: 'cloudflared', lastReconcile: ago(3.2) },
      }
    },
    async transcript(_work, turnId) {
      await sleep(condition === 'slow' ? 1400 : 160)
      return transcripts[turnId] ?? []
    },
  }
}

export function liveAdapter(): ConduitAdapter {
  return { mode: 'live', load: loadLive, transcript: liveTranscript }
}
