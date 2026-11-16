import { ago, api, fmt, href, useFetch, useTime, type Cell, type Status } from './api'

export const STATUS: Record<Status, { glyph: string; label: string }> = {
  healthy: { glyph: '✓', label: 'Healthy' },
  degraded: { glyph: '▲', label: 'Degraded' },
  critical: { glyph: '✕', label: 'Critical' },
  silent: { glyph: '○', label: 'Silent' },
}

/** Status is never color alone: glyph + label travel with it. */
export function StatusBadge({ status, bare }: { status: Status; bare?: boolean }) {
  return (
    <span className={`status ${status}`} title={STATUS[status].label}>
      <i>{STATUS[status].glyph}</i>{!bare && STATUS[status].label}
    </span>
  )
}

const scope = (c: { service?: string; location?: string }) =>
  [c.service && `service:${c.service}`, c.location && `location:${c.location}`].filter(Boolean).join(' ')

export default function Health() {
  const { tick } = useTime()
  const { data, error } = useFetch(() => api.health(), [tick])
  const events = useFetch(() => api.events('6h'), [tick])
  if (error && !data) return <div className="error">{error}</div>
  if (!data) return <div className="empty">Loading…</div>
  if (!data.cells.length) return <Onboarding />

  const cell = new Map(data.cells.map(c => [c.service + '\0' + c.location, c]))
  const order: Status[] = ['critical', 'degraded', 'silent', 'healthy']
  const attention = data.cells
    .filter(c => c.status !== 'healthy')
    .sort((a, b) => order.indexOf(a.status) - order.indexOf(b.status))

  return (
    <>
      <div className="page-head">
        <h1>Health</h1>
        <div className="summary">
          {order.filter(s => data.summary[s] > 0).map(s => (
            <span key={s}><StatusBadge status={s} bare /> <b>{data.summary[s]}</b> {STATUS[s].label.toLowerCase()}</span>
          ))}
        </div>
        <span className="muted">last {data.windowSec / 60} min vs. each cell's own baseline</span>
      </div>

      <div className="card">
        <div className="matrix-wrap">
          <table className="matrix">
            <thead>
              <tr>
                <th>Application</th>
                {data.locations.map(l => (
                  <th key={l.name}><a href={href('/explore', { filter: scope({ location: l.name }) })}><StatusBadge status={l.status} bare /> {l.name}</a></th>
                ))}
              </tr>
            </thead>
            <tbody>
              {data.services.map(s => (
                <tr key={s.name}>
                  <th><a href={href('/explore', { filter: scope({ service: s.name }) })}><StatusBadge status={s.status} bare /> {s.name}</a></th>
                  {data.locations.map(l => {
                    const c = cell.get(s.name + '\0' + l.name)
                    return <td key={l.name}>{c ? <HealthCell c={c} /> : <span className="muted">–</span>}</td>
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      <div className="cols">
        <div className="card">
          <h2>Needs attention</h2>
          {attention.length === 0 && <p className="muted">Everything is within its normal range.</p>}
          {attention.map(c => (
            <div key={c.service + c.location} className="attn">
              <StatusBadge status={c.status} />
              <div>
                <b>{c.service}</b> <span className="muted">in</span> <b>{c.location}</b>
                {c.reasons.map(r => <div key={r} className="reason">{r}</div>)}
              </div>
              <a className="btn" href={href('/explore', { filter: scope(c), tab: 'explain' })}>Explain</a>
            </div>
          ))}
        </div>
        <div className="card">
          <h2>Recent changes</h2>
          {events.data?.events.length === 0 && <p className="muted">No deploys or config changes recorded in the last 6 hours.</p>}
          {events.data?.events.slice(0, 8).map(e => (
            <div key={e.id} className="change">
              <span className="kind">{e.kind}</span>
              <span><b>{e.title}</b>{e.location && <span className="muted"> · {e.location}</span>}</span>
              <span className="muted">{ago(e.tsNs / 1e6)}</span>
            </div>
          ))}
        </div>
      </div>
    </>
  )
}

function HealthCell({ c }: { c: Cell }) {
  return (
    <a className={`cell ${c.status}`} href={href('/explore', { filter: scope(c) })}
      title={c.reasons.join('\n') || 'Within normal range'}>
      <StatusBadge status={c.status} />
      <span className="cell-nums">
        <span>{fmt(c.rate, '/s')}</span>
        <span>{fmt(c.errorRate, '%')} err</span>
        <span>p95 {fmt(c.p95, 'ms')}</span>
      </span>
    </a>
  )
}

function Onboarding() {
  const origin = window.location.origin
  return (
    <div className="card onboarding">
      <h1>Waiting for telemetry</h1>
      <p>Telepathy speaks OpenTelemetry. Point any OTel SDK or Collector at this server — no proprietary agent.</p>
      <pre>{`OTEL_EXPORTER_OTLP_ENDPOINT=${origin}
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_SERVICE_NAME=my-service
OTEL_RESOURCE_ATTRIBUTES=telepathy.location=store-042,deployment.environment.name=production`}</pre>
      <p>Or generate a simulated system to explore: <code>telepathy demo</code></p>
    </div>
  )
}
