import { useSyncExternalStore } from 'react'

export interface Route {
  view: 'runs' | 'work' | 'attention' | 'config' | 'setup'
  id?: string
  params: URLSearchParams
}

function parse(): Route {
  const raw = location.hash.replace(/^#\/?/, '')
  const [path, query = ''] = raw.split('?')
  const [view, id] = path.split('/')
  const params = new URLSearchParams(query)
  switch (view) {
    case 'work':
      return { view: 'work', id, params }
    case 'attention':
      return { view: 'attention', params }
    case 'config':
      return { view: 'config', id, params }
    case 'setup':
      return { view: 'setup', params }
    default:
      return { view: 'runs', params }
  }
}

let current = parse()
let currentHash = location.hash
const subs = new Set<() => void>()
window.addEventListener('hashchange', () => {
  if (location.hash === currentHash) return
  currentHash = location.hash
  current = parse()
  subs.forEach((s) => s())
})

export function useRoute(): Route {
  return useSyncExternalStore(
    (l) => {
      subs.add(l)
      return () => subs.delete(l)
    },
    () => current,
  )
}

export function go(hash: string) {
  location.hash = hash
}

/** Update the query of the current view without adding history entries. */
export function setParams(p: Record<string, string | undefined>) {
  const params = new URLSearchParams(current.params)
  for (const [k, v] of Object.entries(p)) {
    if (v === undefined || v === '') params.delete(k)
    else params.set(k, v)
  }
  const path = location.hash.replace(/^#\/?/, '').split('?')[0]
  const q = params.toString()
  const hash = '#/' + path + (q ? '?' + q : '')
  history.replaceState(null, '', hash)
  currentHash = location.hash
  current = parse()
  subs.forEach((s) => s())
}

export const workHref = (id: string, turn?: number) => `#/work/${id.replace(/^work\//, '').slice(0, 10)}${turn ? `?turn=${turn}` : ''}`
