import { useEffect, useMemo, useRef, useState } from 'react'
import { clock, fmt, type Ev, type QueryResult, type Series } from './api'

/** Categorical slots, assigned in fixed order. A 9th series folds to neutral. */
const SLOTS = 8
export const slotColor = (i: number) => (i < SLOTS ? `var(--s${i + 1})` : 'var(--muted)')
export const seriesLabel = (s: Series) => Object.values(s.group).map(v => v || '(none)').join(' · ') || 'all'

/**
 * Colors follow the entity, not its rank or the chart it appears in: the first time a value of a
 * dimension is seen it takes the next slot, and keeps it on every chart and across refreshes.
 */
const slots = new Map<string, Map<string, number>>()
export function seriesColor(s: Series): string {
  const dim = Object.keys(s.group).join(',')
  if (!slots.has(dim)) slots.set(dim, new Map())
  const m = slots.get(dim)!
  const label = seriesLabel(s)
  if (!m.has(label)) m.set(label, m.size)
  return slotColor(m.get(label)!)
}

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null)
  const [w, setW] = useState(0)
  useEffect(() => {
    if (!ref.current) return
    const ro = new ResizeObserver(e => setW(Math.floor(e[0].contentRect.width)))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  return [ref, w] as const
}

function niceTicks(max: number, n = 4): number[] {
  if (max <= 0) return [0, 1]
  const raw = max / n
  const mag = Math.pow(10, Math.floor(Math.log10(raw)))
  const step = [1, 2, 2.5, 5, 10].map(m => m * mag).find(s => s >= raw)!
  const out: number[] = []
  for (let v = 0; v < max + step * 0.999; v += step) out.push(+v.toPrecision(12))
  return out
}

/** Drop partially-filled buckets at either edge so the line never ends in a fake dip. */
function trim(d: QueryResult) {
  let a = 0, b = d.timestamps.length
  if (b > 2 && d.timestamps[0] < d.from) a = 1
  if (b - a > 2 && d.timestamps[b - 1] + d.step * 1000 > d.to) b -= 1
  return { ts: d.timestamps.slice(a, b), series: d.series.map(s => ({ ...s, values: s.values.slice(a, b) })) }
}

const M = { l: 46, r: 10, t: 10, b: 22 }

export function TimeChart({ data, height = 170, events }: { data?: QueryResult; height?: number; events?: Ev[] }) {
  const [ref, width] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)
  const t = useMemo(() => (data ? trim(data) : null), [data])

  const body = (() => {
    if (!data || !t) return <div className="empty" style={{ height }}>Loading…</div>
    if (!t.series.length || !t.ts.length) return <div className="empty" style={{ height }}>No data in this window</div>
    if (width < 80) return <div style={{ height }} />
    const iw = width - M.l - M.r, ih = height - M.t - M.b
    const max = Math.max(0, ...t.series.flatMap(s => s.values.map(v => v ?? 0)))
    const ticks = niceTicks(max)
    const top = ticks[ticks.length - 1]
    const x0 = t.ts[0], x1 = t.ts[t.ts.length - 1]
    const x = (ms: number) => M.l + (x1 === x0 ? iw / 2 : ((ms - x0) / (x1 - x0)) * iw)
    const y = (v: number) => M.t + ih - (v / top) * ih
    const path = (s: Series) => {
      let d = '', pen = false
      s.values.forEach((v, i) => {
        if (v == null) { pen = false; return }
        d += `${pen ? 'L' : 'M'}${x(t.ts[i]).toFixed(1)} ${y(v).toFixed(1)}`
        pen = true
      })
      return d
    }
    const nx = Math.max(2, Math.min(6, Math.floor(iw / 90)))
    const xt = Array.from({ length: nx + 1 }, (_, i) => x0 + ((x1 - x0) * i) / nx)
    const marks = (events ?? []).map(e => ({ e, ms: e.tsNs / 1e6 })).filter(m => m.ms >= x0 && m.ms <= x1)
    const onMove = (ev: React.MouseEvent<SVGSVGElement>) => {
      const px = ev.clientX - ev.currentTarget.getBoundingClientRect().left
      const frac = Math.min(1, Math.max(0, (px - M.l) / iw))
      setHover(Math.round(frac * (t.ts.length - 1)))
    }
    const hx = hover != null && hover < t.ts.length ? x(t.ts[hover]) : null
    const rows = hover == null ? [] : t.series
      .map(s => ({ label: seriesLabel(s), v: s.values[hover], c: seriesColor(s) }))
      .filter(r => r.v != null).sort((a, b) => b.v! - a.v!)
    return (
      <>
        <svg width={width} height={height} onMouseMove={onMove} onMouseLeave={() => setHover(null)} role="img">
          {ticks.map(v => (
            <g key={v}>
              <line x1={M.l} x2={width - M.r} y1={y(v)} y2={y(v)} className={v === 0 ? 'axis' : 'grid'} />
              <text x={M.l - 6} y={y(v) + 3.5} textAnchor="end" className="tick">{fmt(v, data.unit === 'ms' || data.unit === '%' ? data.unit : '')}</text>
            </g>
          ))}
          {xt.map((ms, i) => (
            <text key={i} x={x(ms)} y={height - 6} textAnchor={i === 0 ? 'start' : i === nx ? 'end' : 'middle'} className="tick">{clock(ms)}</text>
          ))}
          {marks.map(({ e, ms }) => (
            <g key={e.id} className="mark">
              <line x1={x(ms)} x2={x(ms)} y1={M.t} y2={M.t + ih} />
              <path d={`M${x(ms)} ${M.t - 5}l5 5l-5 5l-5 -5z`} />
              <rect x={x(ms) - 7} y={M.t - 6} width={14} height={ih + 6} fill="transparent">
                <title>{`${e.title} — ${clock(ms, true)}${e.location ? ' · ' + e.location : ''}`}</title>
              </rect>
            </g>
          ))}
          {t.series.map(s => (
            <path key={seriesLabel(s)} d={path(s)} fill="none" stroke={seriesColor(s)} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
          ))}
          {hx != null && <line x1={hx} x2={hx} y1={M.t} y2={M.t + ih} className="cross" />}
          {hx != null && t.series.map(s => {
            const v = s.values[hover!]
            return v == null ? null : <circle key={seriesLabel(s)} cx={hx} cy={y(v)} r={4} fill={seriesColor(s)} stroke="var(--surface)" strokeWidth={2} />
          })}
        </svg>
        {hx != null && rows.length > 0 && (
          <div className="tip" style={hx > width / 2 ? { right: width - hx + 12 } : { left: hx + 12 }}>
            <div className="tip-time">{clock(t.ts[hover!], true)}</div>
            {rows.map(r => (
              <div key={r.label} className="tip-row">
                <i style={{ background: r.c }} /><span>{r.label}</span><b>{fmt(r.v, data.unit)}</b>
              </div>
            ))}
          </div>
        )}
        {t.series.length > 1 && (
          <div className="legend">
            {t.series.map(s => <span key={seriesLabel(s)}><i style={{ background: seriesColor(s) }} />{seriesLabel(s)}</span>)}
          </div>
        )}
      </>
    )
  })()
  return <div className="chart" ref={ref}>{body}</div>
}

/** A single headline number — used when the question is "how much", not "when". */
export function Stat({ data }: { data?: QueryResult }) {
  if (!data) return <div className="empty">Loading…</div>
  const total = data.series.reduce((a, s) => a + s.total, 0)
  return <div className="stat">{data.series.length ? fmt(total, data.unit) : '–'}</div>
}

/** Ranked horizontal bars: one measure, one hue, sorted by magnitude. */
export function TopList({ data }: { data?: QueryResult }) {
  if (!data) return <div className="empty">Loading…</div>
  if (!data.series.length) return <div className="empty">No data in this window</div>
  const max = Math.max(...data.series.map(s => s.total), 1e-9)
  return (
    <div className="toplist">
      {data.series.map(s => (
        <div key={seriesLabel(s)} className="toprow" title={`${seriesLabel(s)}: ${fmt(s.total, data.unit)}`}>
          <span className="toplabel">{seriesLabel(s)}</span>
          <span className="topbar"><i style={{ width: `${Math.max(1, (s.total / max) * 100)}%` }} /></span>
          <b>{fmt(s.total, data.unit)}</b>
        </div>
      ))}
    </div>
  )
}
