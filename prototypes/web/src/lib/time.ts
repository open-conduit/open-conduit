import { useEffect, useState } from 'react'
import type { Turn, Work } from '../domain/types'

/** Re-render on a shared clock so durations of live turns advance. */
const subs = new Set<(n: number) => void>()
let clock = Date.now()
setInterval(() => {
  clock = Date.now()
  subs.forEach((f) => f(clock))
}, 1000)
export function useNow(): number {
  const [n, setN] = useState(clock)
  useEffect(() => {
    subs.add(setN)
    return () => void subs.delete(setN)
  }, [])
  return n
}

export function dur(ms: number, precise = false): string {
  if (ms < 0) ms = 0
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return precise && m < 10 ? `${m}m ${s % 60}s` : `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return m % 60 ? `${h}h ${m % 60}m` : `${h}h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d}d ${h % 24}h` : `${d}d`
}

export function since(iso: string, now = Date.now()): string {
  if (!iso) return '—'
  const ms = now - new Date(iso).getTime()
  if (ms < 45_000) return 'just now'
  return dur(ms) + ' ago'
}

export function until(iso: string, now = Date.now()): string {
  return 'in ' + dur(new Date(iso).getTime() - now)
}

export function clockTime(iso: string): string {
  const d = new Date(iso)
  const today = new Date()
  const sameDay = d.toDateString() === today.toDateString()
  const time = d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })
  if (sameDay) return time
  const yest = new Date(today)
  yest.setDate(today.getDate() - 1)
  if (d.toDateString() === yest.toDateString()) return `Yesterday ${time}`
  return d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short' }) + ' ' + time
}

export function stamp(iso: string): string {
  return new Date(iso).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function turnActive(t: Turn, now = Date.now()): number {
  return t.segments.reduce((a, s) => a + ((s.end ? new Date(s.end).getTime() : now) - new Date(s.start).getTime()), 0)
}

export function workActive(w: Work, now = Date.now()): number {
  return w.turns.reduce((a, t) => a + turnActive(t, now), 0)
}

export function workElapsed(w: Work, now = Date.now()): number {
  const end = w.disposition.group === 'closed' ? new Date(w.disposition.since).getTime() : now
  return end - new Date(w.createdAt).getTime()
}

export const money = (n: number) => (n < 0.01 ? '<$0.01' : '$' + n.toFixed(2))
export const kTok = (n: number) => (n >= 1_000_000 ? (n / 1_000_000).toFixed(1) + 'M' : n >= 1000 ? Math.round(n / 1000) + 'k' : String(n))
