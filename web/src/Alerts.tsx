import { useState } from 'react'
import { ago, api, clock, fmt, howLong, href, useFetch, useTime, type Alert, type Channel, type Meta, type Monitor, type Query } from './api'
import { StatusBadge } from './Health'

const explainLink = (a: Alert) => href('/explore', { filter: a.details.scope ?? '', tab: 'explain' })

function AlertRow({ a }: { a: Alert }) {
  const d = a.details
  return (
    <div className="alert">
      <StatusBadge status={a.severity} />
      <div>
        <h3>{a.title}</h3>
        <div className="reason">{a.summary}</div>
        <div className="muted">Firing for {howLong(Date.now() - a.startedMs)} · since {clock(a.startedMs)} · {a.monitorName}</div>
        <div className="evidence">
          {d.causedBy && <div><b>Downstream of {d.causedBy}</b> <span className="muted">— a symptom, so no notification was sent for it</span></div>}
          {d.impacted && d.impacted.length > 0 && <div><b>Root of this incident.</b> Also affected: {d.impacted.join(', ')}</div>}
          {d.triggers?.map(t => <div key={t}><b>Likely trigger:</b> ◆ {t}</div>)}
          {d.findings && d.findings.length > 0 && <div className="muted">What is different about the affected requests:</div>}
          {d.findings?.map(f => (
            <div key={f.key + f.value} className="mono">
              {f.key} <b>= {f.value}</b> — {fmt(f.selPct, '%')} of affected vs {fmt(f.basePct, '%')} of the rest
            </div>
          ))}
          {a.notifyError && <div className="warn">Notification failed — {a.notifyError}</div>}
        </div>
      </div>
      <a className="btn" href={explainLink(a)}>Explain</a>
    </div>
  )
}

const AGGS = ['count', 'rate', 'error_rate', 'avg', 'sum', 'min', 'max', 'p50', 'p90', 'p95', 'p99']

function describe(m: Monitor): string {
  if (m.kind === 'health') {
    const s = m.spec
    const where = [s.service && `service ${s.service}`, s.location && `location ${s.location}`].filter(Boolean).join(', ')
    return `Alerts when any application × location becomes ${(s.statuses ?? ['critical', 'degraded', 'silent']).join(', ')}${where ? ` (${where})` : ''}. No thresholds: each cell is compared with its own baseline.`
  }
  const q = m.spec.query
  if (!q) return ''
  const what = q.signal === 'metrics' ? q.metric : q.signal
  return `${q.agg} of ${what}${q.filter ? ` [${q.filter}]` : ''}${q.groupBy?.length ? ` by ${q.groupBy.join(', ')}` : ''} ${m.spec.op} ${m.spec.threshold} over ${m.spec.window ?? '5m'}`
}

function MonitorForm({ meta, onSaved }: { meta?: Meta; onSaved: () => void }) {
  const blank = { name: '', op: '>', threshold: '', window: '5m', severity: 'critical', split: '' }
  const [f, setF] = useState(blank)
  const [q, setQ] = useState<Query>({ signal: 'spans', agg: 'error_rate', filter: '' })
  const [err, setErr] = useState('')
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await api.saveMonitor({
        name: f.name, kind: 'query', enabled: true,
        spec: { query: { ...q, groupBy: f.split ? [f.split.trim()] : [] }, op: f.op, threshold: Number(f.threshold), window: f.window, severity: f.severity },
      })
      setF(blank); setErr(''); onSaved()
    } catch (e) { setErr(String((e as Error).message)) }
  }
  return (
    <form onSubmit={submit}>
      {err && <div className="error">{err}</div>}
      <div className="inline-form">
        <label className="grow">Name<input required value={f.name} onChange={e => setF({ ...f, name: e.target.value })} placeholder="Checkout error rate" /></label>
        <label>Signal<select value={q.signal} onChange={e => { const s = e.target.value as Query['signal']; setQ({ ...q, signal: s, agg: s === 'metrics' ? 'avg' : s === 'logs' ? 'count' : 'error_rate', metric: s === 'metrics' ? meta?.metrics[0] : undefined }) }}>
          <option value="spans">Traces</option><option value="metrics">Metrics</option><option value="logs">Logs</option></select></label>
        {q.signal === 'metrics' && <label>Metric<select value={q.metric ?? ''} onChange={e => setQ({ ...q, metric: e.target.value })}>{meta?.metrics.map(m => <option key={m}>{m}</option>)}</select></label>}
        <label>Aggregate<select value={q.agg} onChange={e => setQ({ ...q, agg: e.target.value })}>
          {AGGS.filter(a => q.signal === 'logs' ? a === 'count' || a === 'rate' : q.signal === 'metrics' ? a !== 'error_rate' : true).map(a => <option key={a}>{a}</option>)}</select></label>
        <label className="grow">Filter<input value={q.filter} onChange={e => setQ({ ...q, filter: e.target.value })} placeholder="service:checkout kind:server" spellCheck={false} /></label>
        <label>Alert per<input value={f.split} onChange={e => setF({ ...f, split: e.target.value })} placeholder="location" spellCheck={false} size={12} /></label>
        <label>Is<select value={f.op} onChange={e => setF({ ...f, op: e.target.value })}>{['>', '>=', '<', '<='].map(o => <option key={o}>{o}</option>)}</select></label>
        <label>Threshold<input required type="number" step="any" value={f.threshold} onChange={e => setF({ ...f, threshold: e.target.value })} size={8} /></label>
        <label>Over<select value={f.window} onChange={e => setF({ ...f, window: e.target.value })}>{['1m', '5m', '15m', '1h'].map(o => <option key={o}>{o}</option>)}</select></label>
        <label>Severity<select value={f.severity} onChange={e => setF({ ...f, severity: e.target.value })}><option>critical</option><option>degraded</option></select></label>
        <button className="btn primary">Add monitor</button>
      </div>
      <p className="muted">Durations are in milliseconds, error rates in percent. The query is the same one the explorer and dashboards use.</p>
    </form>
  )
}

function Monitors({ meta }: { meta?: Meta }) {
  const [n, setN] = useState(0)
  const { data, error } = useFetch(() => api.monitors(), [n])
  const [err, setErr] = useState('')
  const act = async (fn: () => Promise<unknown>) => { try { await fn(); setErr('') } catch (e) { setErr(String((e as Error).message)) } setN(x => x + 1) }
  return (
    <div className="card">
      <h2>Monitors</h2>
      {(error || err) && <div className="error">{error || err}</div>}
      {data?.monitors.map(m => (
        <div key={m.id} className="list-row">
          <span className="kind">{m.kind}</span>
          <div className="grow"><b>{m.name}</b><div className="reason">{describe(m)}</div></div>
          <label className="switch"><input type="checkbox" checked={m.enabled} onChange={e => act(() => api.saveMonitor({ ...m, enabled: e.target.checked }))} />Enabled</label>
          {m.kind === 'query' && <button className="btn danger" onClick={() => act(() => api.deleteMonitor(m.id))}>Remove</button>}
        </div>
      ))}
      <details>
        <summary>Add a query monitor</summary>
        <MonitorForm meta={meta} onSaved={() => setN(x => x + 1)} />
      </details>
    </div>
  )
}

const HINT: Record<Channel['kind'], string> = {
  slack: 'https://hooks.slack.com/services/…',
  webhook: 'https://example.com/hooks/telepathy',
  pagerduty: 'Events API v2 integration key',
}

function Channels() {
  const [n, setN] = useState(0)
  const { data, error } = useFetch(() => api.channels(), [n])
  const [c, setC] = useState<{ kind: Channel['kind']; name: string; url: string }>({ kind: 'slack', name: '', url: '' })
  const [msg, setMsg] = useState<{ id: string; text: string; ok: boolean }>()
  const [err, setErr] = useState('')
  const add = async (e: React.FormEvent) => {
    e.preventDefault()
    try { await api.addChannel(c); setC({ ...c, name: '', url: '' }); setErr(''); setN(x => x + 1) }
    catch (e) { setErr(String((e as Error).message)) }
  }
  const test = async (id: string) => {
    setMsg({ id, text: 'Sending…', ok: true })
    try { await api.testChannel(id); setMsg({ id, text: 'Test notification sent', ok: true }) }
    catch (e) { setMsg({ id, text: 'Failed: ' + (e as Error).message, ok: false }) }
  }
  return (
    <div className="card">
      <h2>Notification channels</h2>
      {(error || err) && <div className="error">{error || err}</div>}
      {data?.channels.length === 0 && <p className="muted">No channels yet. Alerts are recorded here either way; add a channel to be notified.</p>}
      {data?.channels.map(ch => (
        <div key={ch.id} className="list-row">
          <span className="kind">{ch.kind}</span>
          <div className="grow"><b>{ch.name}</b> <span className="muted mono">{ch.url}</span>
            {msg?.id === ch.id && <div className={msg.ok ? 'reason' : 'warn'}>{msg.text}</div>}</div>
          <button className="btn" onClick={() => test(ch.id)}>Send test</button>
          <button className="btn danger" onClick={async () => { await api.deleteChannel(ch.id); setN(x => x + 1) }}>Remove</button>
        </div>
      ))}
      <form className="inline-form" onSubmit={add}>
        <label>Type<select value={c.kind} onChange={e => setC({ ...c, kind: e.target.value as Channel['kind'] })}>
          <option value="slack">Slack</option><option value="pagerduty">PagerDuty</option><option value="webhook">Webhook</option></select></label>
        <label>Name<input value={c.name} onChange={e => setC({ ...c, name: e.target.value })} placeholder="#oncall" /></label>
        <label className="grow">{c.kind === 'pagerduty' ? 'Routing key' : 'URL'}
          <input required type="password" autoComplete="off" value={c.url} onChange={e => setC({ ...c, url: e.target.value })} placeholder={HINT[c.kind]} /></label>
        <button className="btn primary">Add channel</button>
      </form>
      <p className="muted">Every alert goes to every channel. Notifications include what changed and what is different about the affected requests.</p>
    </div>
  )
}

export default function Alerts() {
  const { tick } = useTime()
  const { data, error } = useFetch(() => api.alerts(), [tick])
  const meta = useFetch(() => api.meta(), [])
  return (
    <>
      <div className="page-head"><h1>Alerts</h1>{data && <span className="muted">{data.firing.length} firing</span>}</div>
      {error && <div className="error">{error}</div>}
      <div className="card">
        <h2>Firing now</h2>
        {data?.firing.length === 0 && <p className="muted">Nothing is firing.</p>}
        {data && [...data.firing].sort((a, b) => Number(!!a.details.causedBy) - Number(!!b.details.causedBy)).map(a => <AlertRow key={a.id} a={a} />)}
      </div>
      <div className="cols">
        <Monitors meta={meta.data} />
        <Channels />
      </div>
      <div className="card">
        <h2>Recently resolved</h2>
        {data?.recent.length === 0 && <p className="muted">No resolved alerts yet.</p>}
        {data && data.recent.length > 0 && (
          <table className="rows">
            <thead><tr><th>Alert</th><th>Severity</th><th>Started</th><th className="num">Lasted</th><th>Resolved</th></tr></thead>
            <tbody>
              {data.recent.map(a => (
                <tr key={a.id} onClick={() => (window.location.hash = explainLink(a))}>
                  <td>{a.title}</td><td><StatusBadge status={a.severity} /></td>
                  <td className="mono">{new Date(a.startedMs).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</td>
                  <td className="num mono">{howLong(a.resolvedMs - a.startedMs)}</td>
                  <td className="muted">{ago(a.resolvedMs)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  )
}
