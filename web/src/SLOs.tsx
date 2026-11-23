import { useEffect, useRef, useState } from 'react'
import { api, fmt, href, quote, useFetch, useTime, type Meta, type SLO, type SLOState, type Status } from './api'

/** SLO state reuses the health vocabulary: glyph + label, never color alone. */
const STATE: Record<SLOState, { status: Status; glyph: string; label: string }> = {
  'ok': { status: 'healthy', glyph: '✓', label: 'Within budget' },
  'slow-burn': { status: 'degraded', glyph: '▲', label: 'Burning slowly' },
  'fast-burn': { status: 'critical', glyph: '✕', label: 'Burning fast' },
  'exhausted': { status: 'critical', glyph: '✕', label: 'Budget spent' },
  'no-data': { status: 'silent', glyph: '○', label: 'No data' },
}

/** 99.5 stays 99.5, 97.98223 becomes 97.98: enough digits to compare with a target of several nines. */
const pct = (v: number | null | undefined) => (v == null ? '–' : (v >= 99.995 ? v.toFixed(3) : v.toFixed(2)).replace(/\.?0+$/, '') + '%')
const burn = (v: number) => (v >= 10 ? v.toFixed(0) : v.toFixed(1)) + '×'
const budgetText = (v: number | null | undefined) =>
  v == null ? '–' : v >= 0 ? `${v >= 10 ? v.toFixed(0) : v.toFixed(1)}% left` : `overspent by ${Math.abs(v) >= 10 ? Math.abs(v).toFixed(0) : Math.abs(v).toFixed(1)}%`
const objective = (s: SLO) =>
  s.kind === 'latency' ? `${pct(s.target)} of requests faster than ${fmt(s.thresholdMs, 'ms')}` : `${pct(s.target)} of requests succeed`

/** How full the budget is. The fill is a quantity, not a status: the badge beside it carries the state. */
function Meter({ value }: { value: number | null | undefined }) {
  const w = Math.max(0, Math.min(100, value ?? 0))
  return <span className="meter" role="img" aria-label={budgetText(value)}><i style={{ width: `${w}%` }} /></span>
}

const M = { l: 44, r: 10, t: 8, b: 22 }
const FLOOR = -100 // the chart stops at "spent twice over"; the tooltip still gives the real figure

/** Error budget remaining over the window. One series, so no legend: the card title names it. */
function BudgetChart({ ts, values, height = 150 }: { ts: number[]; values: number[]; height?: number }) {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(0)
  const [hover, setHover] = useState<number | null>(null)
  useEffect(() => {
    if (!ref.current) return
    const ro = new ResizeObserver(e => setWidth(Math.floor(e[0].contentRect.width)))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  let body = <div style={{ height }} />
  if (ts.length < 2) body = <div className="empty" style={{ height }}>The budget history appears after two hours of data</div>
  else if (width >= 80) {
    const iw = width - M.l - M.r, ih = height - M.t - M.b
    const lo = Math.max(FLOOR, Math.min(0, ...values))
    const ticks = lo < -50 ? [-100, -50, 0, 50, 100] : lo < 0 ? [-50, 0, 50, 100] : [0, 25, 50, 75, 100]
    const y0 = ticks[0]
    const x = (i: number) => M.l + (i / (ts.length - 1)) * iw
    const y = (v: number) => M.t + ih - ((Math.max(y0, v) - y0) / (100 - y0)) * ih
    const d = values.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join('')
    const days = (ts[ts.length - 1] - ts[0]) / 86400_000
    const label = (ms: number) => days > 2
      ? new Date(ms).toLocaleDateString([], { month: 'short', day: 'numeric' })
      : new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    const nx = Math.max(2, Math.min(5, Math.floor(iw / 110)))
    const onMove = (ev: React.MouseEvent<SVGSVGElement>) => {
      const px = ev.clientX - ev.currentTarget.getBoundingClientRect().left
      setHover(Math.round(Math.min(1, Math.max(0, (px - M.l) / iw)) * (ts.length - 1)))
    }
    const hx = hover != null && hover < ts.length ? x(hover) : null
    body = (
      <>
        <svg width={width} height={height} onMouseMove={onMove} onMouseLeave={() => setHover(null)} role="img" aria-label="Error budget remaining over time">
          {ticks.map(v => (
            <g key={v}>
              <line x1={M.l} x2={width - M.r} y1={y(v)} y2={y(v)} className={v === 0 ? 'axis' : 'grid'} />
              <text x={M.l - 6} y={y(v) + 3.5} textAnchor="end" className="tick">{v}%</text>
            </g>
          ))}
          {Array.from({ length: nx + 1 }, (_, i) => Math.round((i / nx) * (ts.length - 1))).map((idx, i) => (
            <text key={i} x={x(idx)} y={height - 6} textAnchor={i === 0 ? 'start' : i === nx ? 'end' : 'middle'} className="tick">{label(ts[idx])}</text>
          ))}
          <path d={d} fill="none" stroke="var(--s1)" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
          {hx != null && <line x1={hx} x2={hx} y1={M.t} y2={M.t + ih} className="cross" />}
          {hx != null && <circle cx={hx} cy={y(values[hover!])} r={4} fill="var(--s1)" stroke="var(--surface)" strokeWidth={2} />}
        </svg>
        {hx != null && (
          <div className="tip" style={hx > width / 2 ? { right: width - hx + 12 } : { left: hx + 12 }}>
            <div className="tip-time">{new Date(ts[hover!]).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</div>
            <div className="tip-row"><i style={{ background: 'var(--s1)' }} /><span>Budget</span><b>{budgetText(values[hover!])}</b></div>
          </div>
        )}
      </>
    )
  }
  return <div className="chart" ref={ref}>{body}</div>
}

function SLOCard({ s, onChange }: { s: SLO; onChange: () => void }) {
  const [confirm, setConfirm] = useState(false)
  const st = STATE[s.state]
  const explain = (location?: string) => href('/explore', {
    filter: `service:${quote(s.service)}${location ? ` location:${quote(location)}` : ''}`, tab: 'explain',
  })
  return (
    <div className="card slo">
      <div className="slo-head">
        <span className={`status ${st.status}`}><i>{st.glyph}</i>{st.label}</span>
        <div className="grow">
          <h2>{s.name}</h2>
          <div className="muted">{s.service}{s.location ? ` in ${s.location}` : ''} · {objective(s)} · rolling {s.windowDays} days</div>
        </div>
        <a className="btn" href={explain()}>Explain</a>
        {confirm
          ? <><button className="btn danger" onClick={async () => { await api.deleteSLO(s.id); onChange() }}>Confirm remove</button><button className="btn" onClick={() => setConfirm(false)}>Cancel</button></>
          : <button className="btn danger" onClick={() => setConfirm(true)}>Remove</button>}
      </div>
      {s.state === 'no-data' ? <p className="muted">No requests from {s.service} in this window yet.</p> : (
        <>
          <div className="slo-stats">
            <div><span className="muted">Measured</span><b>{pct(s.sli)}</b><span className="muted">target {pct(s.target)}</span></div>
            <div><span className="muted">Error budget</span><b>{budgetText(s.budgetLeft)}</b><Meter value={s.budgetLeft} /></div>
            <div title="1× spends the budget exactly over the window. 14.4× over an hour or 6× over six hours raises an alert.">
              <span className="muted">Burn rate</span><b>{burn(s.burn1h)}</b><span className="muted">last hour · {burn(s.burn6h)} over 6 h</span></div>
            <div><span className="muted">Requests</span><b>{fmt(s.total)}</b><span className="muted">{fmt(s.total - s.good)} outside the objective</span></div>
          </div>
          <div className="cols">
            <div>
              <h3>Error budget remaining</h3>
              <BudgetChart ts={s.timestamps} values={s.budget} />
            </div>
            <div>
              <h3>By location</h3>
              <table className="rows">
                <thead><tr><th>Location</th><th className="num">Measured</th><th>Its share of budget</th><th className="num">Burn, 1 h</th></tr></thead>
                <tbody>
                  {s.locations.map(l => (
                    <tr key={l.location} onClick={() => (window.location.hash = explain(l.location))}>
                      <td>{l.location}</td>
                      <td className="num mono">{pct(l.sli)}</td>
                      <td><span className="meter-cell"><Meter value={l.budgetLeft} /><span className="mono">{budgetText(l.budgetLeft)}</span></span></td>
                      <td className="num mono">{burn(l.burn1h)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}
    </div>
  )
}

function SLOForm({ meta, onSaved }: { meta?: Meta; onSaved: () => void }) {
  const blank = { name: '', service: '', location: '', kind: 'availability' as SLO['kind'], thresholdMs: '300', target: '99.9', windowDays: '30' }
  const [f, setF] = useState(blank)
  const [err, setErr] = useState('')
  const set = (k: keyof typeof blank) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.value })
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    try {
      await api.saveSLO({ name: f.name, service: f.service || meta?.services[0] || '', location: f.location, kind: f.kind,
        thresholdMs: Number(f.thresholdMs), target: Number(f.target), windowDays: Number(f.windowDays) })
      setF(blank); setErr(''); onSaved()
    } catch (e) { setErr(String((e as Error).message)) }
  }
  return (
    <form onSubmit={submit}>
      {err && <div className="error">{err}</div>}
      <div className="inline-form">
        <label className="grow">Name<input required value={f.name} onChange={set('name')} placeholder="Checkout availability" /></label>
        <label>Service<select value={f.service || meta?.services[0] || ''} onChange={set('service')}>{meta?.services.map(s => <option key={s}>{s}</option>)}</select></label>
        <label>Locations<input value={f.location} onChange={set('location')} placeholder="all, or eu-*" spellCheck={false} size={12} /></label>
        <label>Objective<select value={f.kind} onChange={set('kind')}><option value="availability">Requests succeed</option><option value="latency">Requests are fast</option></select></label>
        {f.kind === 'latency' && <label>Faster than (ms)<input required type="number" min="1" step="any" value={f.thresholdMs} onChange={set('thresholdMs')} size={7} /></label>}
        <label>Target %<input required type="number" min="50" max="99.999" step="any" value={f.target} onChange={set('target')} size={7} /></label>
        <label>Window<select value={f.windowDays} onChange={set('windowDays')}>{['7', '14', '28', '30', '90'].map(d => <option key={d} value={d}>{d} days</option>)}</select></label>
        <button className="btn primary">Add SLO</button>
      </div>
    </form>
  )
}

export default function SLOs() {
  const { tick } = useTime()
  const [n, setN] = useState(0)
  const { data, error } = useFetch(() => api.slos(), [tick, n])
  const meta = useFetch(() => api.meta(), [])
  const order: SLOState[] = ['fast-burn', 'exhausted', 'slow-burn', 'ok', 'no-data']
  const slos = data ? [...data.slos].sort((a, b) => order.indexOf(a.state) - order.indexOf(b.state)) : []
  return (
    <>
      <div className="page-head">
        <h1>SLOs</h1>
        {data && <span className="muted">{slos.filter(s => s.state === 'ok').length} of {slos.length} within budget</span>}
      </div>
      {error && <div className="error">{error}</div>}
      {data && slos.length === 0 && (
        <div className="card"><p className="muted">No objectives yet. An SLO states how reliable a service must be — for example “99.9% of checkout requests succeed over 30 days” — and Telepathy tracks how much of the allowed failure is left, in total and per location.</p></div>
      )}
      {slos.map(s => <SLOCard key={s.id} s={s} onChange={() => setN(x => x + 1)} />)}
      <div className="card">
        <details open={data?.slos.length === 0}>
          <summary>Add an SLO</summary>
          <SLOForm meta={meta.data} onSaved={() => setN(x => x + 1)} />
          <p className="muted">A request is a server or consumer span. An alert fires when the budget is being spent 14.4× too fast over an hour, or 6× over six hours; the “SLO burn rate” monitor on the Alerts page controls it.</p>
        </details>
      </div>
    </>
  )
}
