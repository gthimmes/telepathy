import { createContext, useContext, useEffect, useRef, useState } from 'react'

export interface Cell {
  service: string; location: string; status: Status; requests: number
  rate: number; errorRate: number; p95: number
  baseRate: number; baseErrorRate: number; baseP95: number; reasons: string[]
}
export type Status = 'healthy' | 'degraded' | 'critical' | 'silent'
export interface Rollup { name: string; status: Status }
export interface HealthReport {
  generatedMs: number; windowSec: number
  services: Rollup[]; locations: Rollup[]; cells: Cell[]; summary: Record<Status, number>
}
export interface Query {
  signal: 'spans' | 'logs' | 'metrics'; metric?: string; filter?: string
  agg?: string; field?: string; groupBy?: string[]; range?: string; limit?: number
}
export interface Series { group: Record<string, string>; values: (number | null)[]; total: number }
export interface QueryResult { from: number; to: number; step: number; unit: string; timestamps: number[]; series: Series[] }
export interface Span {
  traceId: string; spanId: string; parentId: string; service: string; location: string; env: string
  name: string; kind: string; tsNs: number; durationNs: number; status: number; statusMsg?: string
  attrs: Record<string, unknown>
}
export interface Log {
  tsNs: number; service: string; location: string; severity: string; body: string
  traceId?: string; spanId?: string; attrs: Record<string, unknown>
}
export interface Ev {
  id: number; tsNs: number; kind: string; service: string; location: string; title: string
  attrs: Record<string, unknown>; related?: boolean
}
export interface Finding { key: string; value: string; selPct: number; basePct: number; selCount: number; baseCount: number; score: number }
export interface ExplainResult { selection: string; selectionCount: number; baselineCount: number; findings: Finding[]; changes: Ev[] }
export interface Trace { traceId: string; spans: Span[]; logs: Log[] }
export interface Meta { services: string[]; locations: string[]; metrics: string[] }
export interface Widget { id: string; title: string; type: 'timeseries' | 'stat' | 'toplist'; w: number; query: Query }
export interface Dashboard { id: string; title: string; spec: { widgets: Widget[] }; updatedMs: number; provisioned: boolean }

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch('/api/v1' + path, {
    method, headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  if (res.status === 204) return undefined as T
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || res.statusText)
  return data as T
}
const qs = (p: Record<string, string | number | undefined>) =>
  '?' + Object.entries(p).filter(([, v]) => v !== undefined && v !== '').map(([k, v]) => `${k}=${encodeURIComponent(v!)}`).join('&')

export const api = {
  health: () => call<HealthReport>('GET', '/health'),
  meta: () => call<Meta>('GET', '/meta'),
  query: (q: Query) => call<QueryResult>('POST', '/query', q),
  explain: (scope: string, mode: string, range: string) => call<ExplainResult>('POST', '/explain', { scope, mode, range }),
  spans: (filter: string, range: string, order: string) => call<{ spans: Span[] }>('GET', '/spans' + qs({ filter, range, order, limit: 100 })),
  logs: (filter: string, range: string) => call<{ logs: Log[] }>('GET', '/logs' + qs({ filter, range, limit: 200 })),
  trace: (id: string) => call<Trace>('GET', '/traces/' + id),
  events: (range: string) => call<{ events: Ev[] }>('GET', '/events' + qs({ range })),
  dashboards: () => call<{ dashboards: Dashboard[] }>('GET', '/dashboards'),
  dashboard: (id: string) => call<Dashboard>('GET', '/dashboards/' + id),
  saveDashboard: (d: Partial<Dashboard>) => call<Dashboard>(d.id ? 'PUT' : 'POST', '/dashboards' + (d.id ? '/' + d.id : ''), d),
  deleteDashboard: (id: string) => call<void>('DELETE', '/dashboards/' + id),
}

/** Global time range + refresh tick shared by every page. */
export const TimeCtx = createContext<{ range: string; tick: number }>({ range: '1h', tick: 0 })
export const useTime = () => useContext(TimeCtx)

/** Fetch on mount and whenever deps change; keeps the previous data while refreshing. */
export function useFetch<T>(fn: () => Promise<T>, deps: unknown[]) {
  const [state, set] = useState<{ data?: T; error?: string; loading: boolean }>({ loading: true })
  const seq = useRef(0)
  useEffect(() => {
    const id = ++seq.current
    fn().then(
      data => { if (id === seq.current) set({ data, loading: false }) },
      e => { if (id === seq.current) set(s => ({ data: s.data, error: String(e.message || e), loading: false })) },
    )
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return state
}

// ---- hash router ----
export interface Route { path: string; params: URLSearchParams }
const parse = (): Route => {
  const h = window.location.hash.replace(/^#/, '') || '/'
  const i = h.indexOf('?')
  return { path: i < 0 ? h : h.slice(0, i), params: new URLSearchParams(i < 0 ? '' : h.slice(i + 1)) }
}
export function useRoute(): Route {
  const [r, set] = useState(parse)
  useEffect(() => {
    const on = () => set(parse())
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return r
}
export const href = (path: string, params?: Record<string, string>) =>
  '#' + path + (params ? '?' + new URLSearchParams(params).toString() : '')

// ---- formatting ----
export function fmt(v: number | null | undefined, unit = ''): string {
  if (v == null || !isFinite(v)) return '–'
  if (unit === 'ms') return v >= 1000 ? (v / 1000).toFixed(v >= 10000 ? 1 : 2) + ' s' : (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ms'
  if (unit === '%') return (v >= 10 ? v.toFixed(0) : v.toFixed(1)) + '%'
  const a = Math.abs(v)
  const n = a >= 1e9 ? (v / 1e9).toFixed(1) + 'B' : a >= 1e6 ? (v / 1e6).toFixed(1) + 'M' : a >= 1e4 ? (v / 1e3).toFixed(1) + 'k'
    : a >= 100 ? v.toFixed(0) : a >= 10 ? v.toFixed(1) : a === 0 ? '0' : v.toFixed(2)
  return n + (unit === '/s' ? '/s' : unit && unit !== '1' ? ' ' + unit : '')
}
export const clock = (ms: number, secs = false) =>
  new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: secs ? '2-digit' : undefined })
export function ago(ms: number): string {
  const s = Math.max(0, (Date.now() - ms) / 1000)
  return s < 90 ? `${Math.round(s)}s ago` : s < 5400 ? `${Math.round(s / 60)}m ago` : `${Math.round(s / 3600)}h ago`
}
