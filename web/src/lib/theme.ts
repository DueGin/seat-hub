import { useSyncExternalStore } from 'react'

// Theme preference: follow the OS, or pin light/dark. index.html applies the
// same logic before first paint (keep THEME_KEY and the fallback in sync with
// the inline script there) so the page never flashes the wrong theme.
export type ThemePreference = 'system' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'

export const THEME_KEY = 'portal.theme'
const META_COLORS: Record<ResolvedTheme, string> = { dark: '#141210', light: '#f6f3ee' }
const media = typeof window !== 'undefined' ? window.matchMedia('(prefers-color-scheme: light)') : null
const listeners = new Set<() => void>()

function readPreference(): ThemePreference {
  try {
    const stored = localStorage.getItem(THEME_KEY)
    if (stored === 'light' || stored === 'dark' || stored === 'system') return stored
  } catch { /* storage may be blocked; fall back to the system theme */ }
  return 'system'
}

function resolve(preference: ThemePreference): ResolvedTheme {
  if (preference !== 'system') return preference
  return media?.matches ? 'light' : 'dark'
}

let preference = readPreference()
let snapshot = { preference, resolved: resolve(preference) }

function apply() {
  snapshot = { preference, resolved: resolve(preference) }
  const root = document.documentElement
  root.dataset.theme = snapshot.resolved
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', META_COLORS[snapshot.resolved])
  listeners.forEach((listener) => listener())
}

media?.addEventListener('change', () => { if (preference === 'system') apply() })
// Another tab changed the preference: follow it.
if (typeof window !== 'undefined') window.addEventListener('storage', (event) => { if (event.key === THEME_KEY) { preference = readPreference(); apply() } })

export function setThemePreference(next: ThemePreference) {
  preference = next
  try { if (next === 'system') localStorage.removeItem(THEME_KEY); else localStorage.setItem(THEME_KEY, next) } catch { /* not persisted */ }
  apply()
}

function subscribe(listener: () => void) { listeners.add(listener); return () => { listeners.delete(listener) } }
const getSnapshot = () => snapshot

export function useTheme() {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

// Make sure the attribute matches even if the inline script did not run.
if (typeof document !== 'undefined') apply()
