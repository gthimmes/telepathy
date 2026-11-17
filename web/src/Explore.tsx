import { useEffect, useState } from 'react'
import { api, clock, fmt, href, quote, useFetch, useTime, type Finding, type Route } from './api'
import { TimeChart } from './chart'

/** Span-only fields mean nothing to logs; drop them when reusing the filter. */
const forLogs = (f: string) => f.split(/\s+/).filter(t => !/^-?(status|duration|kind|name|span_id|parent_id)[:<>!]/.test(t)).join(' ')

export default function Explore({ route }: { route: Route }) {
  const { range, tick } = useTime()
  const filter = route.params.get('filter') ?? ''
  const tab = route.params.get('tab') ?? 'explain'
  const group = route.params.get('group') ?? 'service'
  const [draft, setDraft] = useState(filter)
  useEffect(() => setDraft(filter), [filter])

  const go = (p: Record<string, string>) => {
    const next = { filter, tab, group, ...p }
    window.location.hash = href('/explore', next)
  }
  const groupBy = group === 'none' ? [] : [group]
  const base = { signal: 'spans' as const, filter, groupBy, range, limit: 8 }
  // "Requests" means entry spans, so rate and errors are not inflated by internal spans.
  const reqFilter = /(^|\s)-?kind[:!]/.test(filter) ? filter : `${filter} kind:server`.trim()
  const rate = useFetch(() => api.query({ ...base, filter: reqFilter, agg: 'rate' }), [filter, group, range, tick])
  const errs = useFetch(() => api.query({ ...base, filter: reqFilter, agg: 'error_rate' }), [filter, group, range, tick])
  const p95 = useFetch(() => api.query({ ...base, filter: reqFilter, agg: 'p95' }), [filter, group, range, tick])
  const events = useFetch(() => api.events(range), [range, tick])
  const err = rate.error || errs.error || p95.error

  return (
    <>
      <div className="page-head"><h1>Explore</h1></div>
      <form className="filterbar" onSubmit={e => { e.preventDefault(); go({ filter: draft.trim() }) }}>
        <input value={draft} onChange={e => setDraft(e.target.value)} spellCheck={false}
          placeholder='service:checkout location:eu-central status:error duration>500ms http.route:/orders' />
        <label>split by
          <select value={group} onChange={e => go({ group: e.target.value })}>
            {['none', 'service', 'location', 'name', 'service.version', 'http.response.status_code'].map(g => <option key={g}>{g}</option>)}
          </select>
        </label>
        <button className="btn primary">Run</button>
      </form>
      {err && <div className="error">{err}</div>}

      <div className="grid3">
        <div className="card"><h3>Requests <span className="muted">per second</span></h3><TimeChart data={rate.data} events={events.data?.events} /></div>
        <div className="card"><h3>Error rate <span className="muted">%</span></h3><TimeChart data={errs.data} events={events.data?.events} /></div>
        <div className="card"><h3>Latency <span className="muted">p95</span></h3><TimeChart data={p95.data} events={events.data?.events} /></div>
      </div>

      <div className="tabs" role="tablist">
        {[['explain', 'Explain'], ['traces', 'Traces'], ['logs', 'Logs']].map(([k, label]) => (
          <button key={k} role="tab" aria-selected={tab === k} className={tab === k ? 'on' : ''} onClick={() => go({ tab: k })}>{label}</button>
        ))}
      </div>
      {tab === 'explain' && <Explain filter={filter} onPick={f => go({ filter: `${filter} ${f.key}:${quote(f.value)}`.trim() })} />}
      {tab === 'traces' && <Traces filter={filter} />}
      {tab === 'logs' && <Logs filter={forLogs(filter)} />}
    </>
  )
}

function Explain({ filter, onPick }: { filter: string; onPick: (f: Finding) => void }) {
  const { range, tick } = useTime()
  const [mode, setMode] = useState('errors')
  const { data, error } = useFetch(() => api.explain(filter, mode, range), [filter, mode, range, tick])
  const what = mode === 'errors' ? 'failing' : 'slowest 5% of'
  const related = data?.changes.filter(c => c.related) ?? []
  return (
    <div className="card">
      <div className="row">
        <h2>What is different about the {what} spans?</h2>
        <div className="seg">
          <button className={mode === 'errors' ? 'on' : ''} onClick={() => setMode('errors')}>Errors</button>
          <button className={mode === 'slow' ? 'on' : ''} onClick={() => setMode('slow')}>Slow</button>
        </div>
      </div>
      {error && <div className="error">{error}</div>}
      {data && (
        <>
          <p className="muted">
            Compared {data.selectionCount.toLocaleString()} {what} spans against {data.baselineCount.toLocaleString()} others in this scope,
            across every attribute. Click a finding to narrow the filter.
          </p>
          {data.selectionCount === 0 && <p>No {mode === 'errors' ? 'errors' : 'spans'} in this scope and window — nothing to explain.</p>}
          {related.length > 0 && data.findings.length > 0 && (
            <div className="trigger">
              <b>Likely trigger</b>
              {related.map(c => (
                <div key={c.id}>◆ {c.title}{c.location && ` in ${c.location}`} <span className="muted">at {clock(c.tsNs / 1e6, true)}</span></div>
              ))}
            </div>
          )}
          {data.findings.length > 0 && (
            <div className="legend flat"><span><i style={{ background: 'var(--s1)' }} />{what} spans</span><span><i style={{ background: 'var(--base)' }} />everything else</span></div>
          )}
          <div className="findings">
            {data.findings.map(f => (
              <button key={f.key + f.value} className="finding" onClick={() => onPick(f)}
                title={`${f.selCount} of ${data.selectionCount} selected vs ${f.baseCount} of ${data.baselineCount} baseline`}>
                <span className="fkey">{f.key} <b>= {f.value}</b></span>
                <span className="fbars">
                  <span className="fbar"><i style={{ width: `${f.selPct}%`, background: 'var(--s1)' }} /></span>
                  <span className="fbar"><i style={{ width: `${f.basePct}%`, background: 'var(--base)' }} /></span>
                </span>
                <span className="fnum"><b>{fmt(f.selPct, '%')}</b><span className="muted">{fmt(f.basePct, '%')}</span></span>
              </button>
            ))}
          </div>
          {data.selectionCount > 0 && data.findings.length === 0 && (
            <p>No single attribute stands out — the {what} spans look like everything else.</p>
          )}
        </>
      )}
    </div>
  )
}

function Traces({ filter }: { filter: string }) {
  const { range, tick } = useTime()
  const [order, setOrder] = useState('recent')
  const { data, error } = useFetch(() => api.spans(filter, range, order), [filter, range, order, tick])
  return (
    <div className="card">
      <div className="row">
        <h2>Matching spans</h2>
        <div className="seg">
          <button className={order === 'recent' ? 'on' : ''} onClick={() => setOrder('recent')}>Most recent</button>
          <button className={order === 'slow' ? 'on' : ''} onClick={() => setOrder('slow')}>Slowest</button>
        </div>
      </div>
      {error && <div className="error">{error}</div>}
      <table className="rows">
        <thead><tr><th>Time</th><th>Service</th><th>Operation</th><th>Location</th><th className="num">Duration</th><th>Status</th></tr></thead>
        <tbody>
          {data?.spans.map(s => (
            <tr key={s.traceId + s.spanId} onClick={() => (window.location.hash = href(`/trace/${s.traceId}`, { span: s.spanId }))}>
              <td className="mono">{clock(s.tsNs / 1e6, true)}</td>
              <td>{s.service}</td><td>{s.name}</td><td>{s.location}</td>
              <td className="num mono">{fmt(s.durationNs / 1e6, 'ms')}</td>
              <td>{s.status === 2 ? <span className="status critical"><i>✕</i>Error</span> : <span className="muted">OK</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {data?.spans.length === 0 && <p className="muted">No spans match.</p>}
    </div>
  )
}

export function LogRows({ logs }: { logs: { tsNs: number; severity: string; service: string; location: string; body: string; traceId?: string }[] }) {
  return (
    <div className="logs">
      {logs.map((l, i) => (
        <div key={i} className="log">
          <span className="mono muted">{clock(l.tsNs / 1e6, true)}</span>
          <span className={`sev sev-${l.severity.toLowerCase()}`}>{l.severity}</span>
          <span className="muted">{l.service} · {l.location}</span>
          <span className="body">{l.body}</span>
          {l.traceId && <a href={href(`/trace/${l.traceId}`)}>trace →</a>}
        </div>
      ))}
    </div>
  )
}

function Logs({ filter }: { filter: string }) {
  const { range, tick } = useTime()
  const { data, error } = useFetch(() => api.logs(filter, range), [filter, range, tick])
  return (
    <div className="card">
      <h2>Logs {filter && <span className="muted mono">{filter}</span>}</h2>
      {error && <div className="error">{error}</div>}
      {data && <LogRows logs={data.logs} />}
      {data?.logs.length === 0 && <p className="muted">No logs match.</p>}
    </div>
  )
}
