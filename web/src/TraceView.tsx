import { useMemo, useState } from 'react'
import { api, clock, fmt, href, useFetch, type Route, type Span } from './api'
import { slotColor } from './chart'
import { LogRows } from './Explore'

interface Row { span: Span; depth: number }

function flatten(spans: Span[]): Row[] {
  const ids = new Set(spans.map(s => s.spanId))
  const kids = new Map<string, Span[]>()
  const roots: Span[] = []
  for (const s of spans) {
    if (s.parentId && ids.has(s.parentId)) kids.set(s.parentId, [...(kids.get(s.parentId) ?? []), s])
    else roots.push(s)
  }
  const out: Row[] = []
  const walk = (s: Span, depth: number) => {
    out.push({ span: s, depth })
    ;(kids.get(s.spanId) ?? []).sort((a, b) => a.tsNs - b.tsNs).forEach(k => walk(k, depth + 1))
  }
  roots.sort((a, b) => a.tsNs - b.tsNs).forEach(r => walk(r, 0))
  return out
}

export default function TraceView({ id, route }: { id: string; route: Route }) {
  const { data, error } = useFetch(() => api.trace(id), [id])
  const [sel, setSel] = useState<string | null>(route.params.get('span'))
  const rows = useMemo(() => (data ? flatten(data.spans) : []), [data])
  if (error) return <div className="error">{error}</div>
  if (!data) return <div className="empty">Loading…</div>

  const t0 = Math.min(...data.spans.map(s => s.tsNs))
  const t1 = Math.max(...data.spans.map(s => s.tsNs + s.durationNs))
  const total = Math.max(1, t1 - t0)
  const services = [...new Set(rows.map(r => r.span.service))]
  const color = (svc: string) => slotColor(services.indexOf(svc))
  const root = rows[0].span
  const errors = data.spans.filter(s => s.status === 2).length
  const picked = data.spans.find(s => s.spanId === sel)

  return (
    <>
      <div className="page-head">
        <h1>{root.service}: {root.name}</h1>
        <span className="muted mono">{id}</span>
      </div>
      <div className="summary">
        <span><b>{fmt(total / 1e6, 'ms')}</b> total</span>
        <span><b>{data.spans.length}</b> spans</span>
        <span><b>{services.length}</b> services</span>
        <span>{errors > 0 ? <span className="status critical"><i>✕</i>{errors} failed</span> : <span className="status healthy"><i>✓</i>no errors</span>}</span>
        <span className="muted">{root.location} · {new Date(t0 / 1e6).toLocaleString()}</span>
        <a href={href('/explore', { filter: `service:${root.service} location:${root.location}` })}>Explore this service →</a>
      </div>

      <div className="card">
        <div className="legend flat">{services.map(s => <span key={s}><i style={{ background: color(s) }} />{s}</span>)}</div>
        <div className="waterfall">
          {rows.map(({ span: s, depth }) => (
            <div key={s.spanId} className={`wrow${sel === s.spanId ? ' on' : ''}`} onClick={() => setSel(sel === s.spanId ? null : s.spanId)}>
              <div className="wname" style={{ paddingLeft: 8 + depth * 14 }} title={`${s.service}: ${s.name}`}>
                {s.status === 2 && <span className="status critical"><i>✕</i></span>}
                <b>{s.service}</b> {s.name}
              </div>
              <div className="wtrack">
                <i className={s.status === 2 ? 'err' : ''} style={{
                  left: `${((s.tsNs - t0) / total) * 100}%`,
                  width: `max(3px, ${(s.durationNs / total) * 100}%)`,
                  background: color(s.service),
                }} />
              </div>
              <div className="wdur mono">{fmt(s.durationNs / 1e6, 'ms')}</div>
            </div>
          ))}
        </div>
      </div>

      {picked && (
        <div className="card">
          <h2>{picked.service}: {picked.name}</h2>
          {picked.statusMsg && <div className="error">{picked.statusMsg}</div>}
          <table className="kv">
            <tbody>
              {Object.entries({ kind: picked.kind, location: picked.location, start: clock(picked.tsNs / 1e6, true), duration: fmt(picked.durationNs / 1e6, 'ms'), 'span id': picked.spanId, ...picked.attrs })
                .map(([k, v]) => <tr key={k}><th>{k}</th><td className="mono">{typeof v === 'object' ? JSON.stringify(v) : String(v)}</td></tr>)}
            </tbody>
          </table>
        </div>
      )}

      {data.logs.length > 0 && (
        <div className="card"><h2>Logs in this trace</h2><LogRows logs={data.logs.map(l => ({ ...l, traceId: undefined }))} /></div>
      )}
    </>
  )
}
