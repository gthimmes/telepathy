import { useEffect, useState } from 'react'
import { api, href, quote, useFetch, useTime, type Dashboard, type Meta, type Query, type Route, type Widget } from './api'
import { Stat, TimeChart, TopList } from './chart'

export function DashboardList() {
  const { data, error } = useFetch(() => api.dashboards(), [])
  const create = async () => {
    const d = await api.saveDashboard({ title: 'Untitled dashboard', spec: { widgets: [] } })
    window.location.hash = href(`/dashboards/${d.id}`, { edit: '1' })
  }
  return (
    <>
      <div className="page-head"><h1>Dashboards</h1><button className="btn primary" onClick={create}>New dashboard</button></div>
      {error && <div className="error">{error}</div>}
      <div className="card">
        {data?.dashboards.length === 0 && <p className="muted">No dashboards yet. Create one, or drop a JSON file in the <code>dashboards/</code> directory.</p>}
        {data?.dashboards.map(d => (
          <a key={d.id} className="dash-link" href={href(`/dashboards/${d.id}`)}>
            <b>{d.title}</b>
            <span className="muted">{d.spec.widgets?.length ?? 0} widgets{d.provisioned && ' · provisioned from file'}</span>
          </a>
        ))}
      </div>
    </>
  )
}

/** Dashboard variables narrow every widget, except ones that already pin that dimension. */
type Vars = Record<string, string>
const pins = (filter: string, key: string) => new RegExp(`(^|\\s)-?${key}[:<>!=]`).test(filter)
function withVars(q: Query, vars: Vars): Query {
  let filter = q.filter ?? ''
  for (const [k, v] of Object.entries(vars)) if (v && !pins(filter, k)) filter = `${filter} ${k}:${quote(v)}`.trim()
  return { ...q, filter }
}

function WidgetBody({ w, vars }: { w: Widget; vars: Vars }) {
  const { range, tick } = useTime()
  const query = withVars(w.query, vars)
  const key = JSON.stringify(query)
  const { data, error } = useFetch(() => api.query({ ...query, range, limit: 8 }), [key, range, tick])
  if (error) return <div className="error">{error}</div>
  if (w.type === 'stat') return <Stat data={data} />
  if (w.type === 'toplist') return <TopList data={data} />
  return <TimeChart data={data} />
}

const AGGS = ['count', 'rate', 'error_rate', 'avg', 'sum', 'min', 'max', 'p50', 'p90', 'p95', 'p99']
const blank = (): Widget => ({ id: Math.random().toString(36).slice(2, 10), title: '', type: 'timeseries', w: 6, query: { signal: 'spans', agg: 'rate', filter: '', groupBy: [] } })

function WidgetForm({ meta, onAdd }: { meta?: Meta; onAdd: (w: Widget) => void }) {
  const [w, setW] = useState<Widget>(blank)
  const q = (p: Partial<Query>) => setW({ ...w, query: { ...w.query, ...p } })
  const sig = w.query.signal
  return (
    <form className="card wform" onSubmit={e => { e.preventDefault(); onAdd({ ...w, title: w.title || `${w.query.agg} of ${sig === 'metrics' ? w.query.metric : sig}` }); setW(blank()) }}>
      <h2>Add widget</h2>
      <div className="fields">
        <label>Title<input value={w.title} onChange={e => setW({ ...w, title: e.target.value })} placeholder="Checkout latency" /></label>
        <label>Type<select value={w.type} onChange={e => setW({ ...w, type: e.target.value as Widget['type'] })}>
          <option value="timeseries">Time series</option><option value="stat">Single number</option><option value="toplist">Top list</option></select></label>
        <label>Signal<select value={sig} onChange={e => q({ signal: e.target.value as Query['signal'], agg: e.target.value === 'metrics' ? 'avg' : 'rate', metric: e.target.value === 'metrics' ? meta?.metrics[0] : undefined })}>
          <option value="spans">Traces</option><option value="metrics">Metrics</option><option value="logs">Logs</option></select></label>
        {sig === 'metrics' && <label>Metric<select value={w.query.metric ?? ''} onChange={e => q({ metric: e.target.value })}>
          {meta?.metrics.map(m => <option key={m}>{m}</option>)}</select></label>}
        <label>Aggregate<select value={w.query.agg} onChange={e => q({ agg: e.target.value })}>
          {AGGS.filter(a => sig === 'logs' ? a === 'count' || a === 'rate' : sig === 'metrics' ? a !== 'error_rate' : true).map(a => <option key={a}>{a}</option>)}</select></label>
        <label className="wide">Filter<input value={w.query.filter} onChange={e => q({ filter: e.target.value })} placeholder="service:checkout location:us-east" spellCheck={false} /></label>
        <label>Split by<input value={w.query.groupBy?.[0] ?? ''} onChange={e => q({ groupBy: e.target.value ? [e.target.value.trim()] : [] })} placeholder="service, location, any attribute" list="dims" spellCheck={false} /></label>
        <label>Width<select value={w.w} onChange={e => setW({ ...w, w: +e.target.value })}>
          <option value={4}>One third</option><option value={6}>Half</option><option value={12}>Full</option></select></label>
      </div>
      <datalist id="dims">{['service', 'location', 'name', 'service.version', 'http.route', 'http.response.status_code'].map(d => <option key={d} value={d} />)}</datalist>
      <button className="btn primary">Add widget</button>
    </form>
  )
}

export function DashboardView({ id, edit, route }: { id: string; edit: boolean; route: Route }) {
  const loaded = useFetch(() => api.dashboard(id), [id])
  const meta = useFetch(() => api.meta(), [])
  const [d, setD] = useState<Dashboard>()
  const [dirty, setDirty] = useState(false)
  const [err, setErr] = useState('')
  const [confirmDelete, setConfirmDelete] = useState(false)
  const vars: Vars = { service: route.params.get('service') ?? '', location: route.params.get('location') ?? '' }
  const setVar = (k: string, v: string) => {
    const p: Record<string, string> = {}
    for (const [key, val] of Object.entries({ ...vars, [k]: v })) if (val) p[key] = val
    if (edit) p.edit = '1'
    window.location.hash = href(`/dashboards/${id}`, p)
  }
  useEffect(() => { if (loaded.data) { setD(loaded.data); setDirty(false) } }, [loaded.data])
  if (loaded.error) return <div className="error">{loaded.error}</div>
  if (!d) return <div className="empty">Loading…</div>

  const change = (p: Partial<Dashboard>) => { setD({ ...d, ...p }); setDirty(true) }
  const widgets = d.spec.widgets ?? []
  const setWidgets = (ws: Widget[]) => change({ spec: { ...d.spec, widgets: ws } })
  const save = async () => {
    try { setD(await api.saveDashboard(d)); setDirty(false); setErr(''); window.location.hash = href(`/dashboards/${id}`) }
    catch (e) { setErr(String((e as Error).message)) }
  }
  const remove = async () => {
    if (!confirmDelete) { setConfirmDelete(true); return }
    await api.deleteDashboard(id)
    window.location.hash = href('/dashboards')
  }
  const move = (i: number, by: number) => {
    const ws = [...widgets]; const [w] = ws.splice(i, 1); ws.splice(i + by, 0, w); setWidgets(ws)
  }

  return (
    <>
      <div className="page-head">
        {edit ? <input className="title-input" value={d.title} onChange={e => change({ title: e.target.value })} aria-label="Dashboard title" /> : <h1>{d.title}</h1>}
        {d.provisioned && <span className="kind">provisioned</span>}
        <span className="spacer" />
        {(['service', 'location'] as const).map(k => {
          const known = (k === 'service' ? meta.data?.services : meta.data?.locations) ?? []
          return (
            <label key={k} className="var">{k}
              <select value={vars[k]} onChange={e => setVar(k, e.target.value)}>
                <option value="">All</option>
                {vars[k] && !known.includes(vars[k]) && <option>{vars[k]}</option>}
                {known.map(v => <option key={v}>{v}</option>)}
              </select>
            </label>
          )
        })}
        {edit ? (
          <>
            <button className="btn danger" onClick={remove} onBlur={() => setConfirmDelete(false)}>{confirmDelete ? 'Really delete?' : 'Delete'}</button>
            <a className="btn" href={href(`/dashboards/${id}`)} onClick={() => { setD(loaded.data); setDirty(false) }}>Cancel</a>
            <button className="btn primary" onClick={save} disabled={!dirty}>Save</button>
          </>
        ) : <a className="btn" href={href(`/dashboards/${id}`, { edit: '1' })}>Edit</a>}
      </div>
      {err && <div className="error">{err}</div>}
      {edit && d.provisioned && <p className="muted">This dashboard is provisioned from a file. Saving here detaches it, so the file no longer overwrites your changes.</p>}
      {widgets.length === 0 && !edit && <div className="card"><p className="muted">This dashboard is empty. Choose Edit to add widgets.</p></div>}

      <div className="dash-grid">
        {widgets.map((w, i) => (
          <div key={w.id} className="card" style={{ gridColumn: `span ${w.w}` }}>
            <div className="row">
              <h3>{w.title}</h3>
              {edit && (
                <span className="wtools">
                  <button title="Move earlier" disabled={i === 0} onClick={() => move(i, -1)}>←</button>
                  <button title="Move later" disabled={i === widgets.length - 1} onClick={() => move(i, 1)}>→</button>
                  <button title="Remove widget" onClick={() => setWidgets(widgets.filter(x => x.id !== w.id))}>✕</button>
                </span>
              )}
            </div>
            <WidgetBody w={w} vars={vars} />
          </div>
        ))}
      </div>

      {edit && <WidgetForm meta={meta.data} onAdd={w => setWidgets([...widgets, w])} />}
      {edit && (
        <details className="card">
          <summary>Dashboard as code</summary>
          <p className="muted">Save this as <code>dashboards/{id}.json</code> in your repo to version it and provision it on startup.</p>
          <pre>{JSON.stringify({ title: d.title, spec: d.spec }, null, 2)}</pre>
        </details>
      )}
    </>
  )
}
