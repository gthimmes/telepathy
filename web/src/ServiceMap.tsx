import { api, fmt, href, useFetch, useTime, type MapEdge, type MapNode, type Route, type Status } from './api'
import { STATUS } from './Health'

const NODE_W = 190, NODE_H = 54, COL_GAP = 150, ROW_GAP = 40, PAD = 30
const STATUS_VAR: Record<Status, string> = { healthy: 'var(--good)', degraded: 'var(--warning)', critical: 'var(--critical)', silent: 'var(--base)' }

/** Left-to-right layers: a service sits one column right of its furthest caller. */
function layout(nodes: MapNode[], edges: MapEdge[]) {
  const layer = new Map(nodes.map(n => [n.service, 0]))
  for (let pass = 0; pass < nodes.length; pass++) { // bounded, so call cycles cannot loop forever
    let moved = false
    for (const e of edges) {
      const want = (layer.get(e.from) ?? 0) + 1
      if (e.from !== e.to && (layer.get(e.to) ?? 0) < want && want < nodes.length) { layer.set(e.to, want); moved = true }
    }
    if (!moved) break
  }
  const cols: MapNode[][] = []
  for (const n of nodes) (cols[layer.get(n.service)!] ??= []).push(n)
  const tallest = Math.max(1, ...cols.map(c => c?.length ?? 0))
  const height = PAD * 2 + tallest * NODE_H + (tallest - 1) * ROW_GAP
  const pos = new Map<string, { x: number; y: number }>()
  cols.forEach((col, i) => {
    const colH = col.length * NODE_H + (col.length - 1) * ROW_GAP
    col.forEach((n, j) => pos.set(n.service, { x: PAD + i * (NODE_W + COL_GAP), y: (height - colH) / 2 + j * (NODE_H + ROW_GAP) }))
  })
  /** Free horizontal lanes in a column: above, between and below its nodes. */
  const lanes = (i: number) => {
    const ys = (cols[i] ?? []).map(n => pos.get(n.service)!.y).sort((a, b) => a - b)
    if (!ys.length) return [height / 2]
    return [ys[0] - ROW_GAP / 2, ...ys.slice(1).map(y => y - ROW_GAP / 2), ys[ys.length - 1] + NODE_H + ROW_GAP / 2]
  }
  return { pos, layer, lanes, width: PAD * 2 + cols.length * NODE_W + (cols.length - 1) * COL_GAP, height }
}

/** An edge that skips columns is routed through a free lane so it never runs behind a node. */
function edgePath(a: { x: number; y: number }, b: { x: number; y: number }, la: number, lb: number, lanes: (i: number) => number[]) {
  const back = b.x <= a.x // a call back up the chain: join the facing sides
  const x1 = back ? a.x : a.x + NODE_W, x2 = back ? b.x + NODE_W : b.x
  const y1 = a.y + NODE_H / 2, y2 = b.y + NODE_H / 2
  const bend = (back ? -1 : 1) * Math.max(40, Math.min(COL_GAP, Math.abs(x2 - x1)) / 2)
  if (back || lb - la < 2) {
    return { d: `M${x1} ${y1} C${x1 + bend} ${y1} ${x2 - bend} ${y2} ${x2} ${y2}`, lx: (x1 + x2) / 2, ly: (y1 + y2) / 2 }
  }
  // Pick the lane, usable in every skipped column, that is closest to a straight line.
  const mid = (y1 + y2) / 2
  let lane = mid, best = Infinity
  for (const y of lanes(la + 1)) {
    let clear = true
    for (let i = la + 2; i < lb; i++) clear = clear && lanes(i).some(v => Math.abs(v - y) < 1)
    if (clear && Math.abs(y - mid) < best) { best = Math.abs(y - mid); lane = y }
  }
  const xa = x1 + COL_GAP, xb = x2 - COL_GAP // the span of the skipped columns
  return {
    d: `M${x1} ${y1} C${x1 + bend} ${y1} ${xa - bend} ${lane} ${xa} ${lane} L${xb} ${lane} C${xb + bend} ${lane} ${x2 - bend} ${y2} ${x2} ${y2}`,
    lx: (xa + xb) / 2, ly: lane,
  }
}

export default function ServiceMap({ route }: { route: Route }) {
  const { range, tick } = useTime()
  const location = route.params.get('location') ?? ''
  const { data, error } = useFetch(() => api.serviceMap(range, location), [range, location, tick])
  const meta = useFetch(() => api.meta(), [])
  const scope = (service: string) => [`service:${service}`, location && `location:${location}`].filter(Boolean).join(' ')

  const head = (
    <div className="page-head">
      <h1>Service map</h1>
      <span className="muted">derived from traces over the last {range} — nothing to declare</span>
      <span className="spacer" />
      <label className="var">location
        <select value={location} onChange={e => (window.location.hash = href('/map', e.target.value ? { location: e.target.value } : undefined))}>
          <option value="">All</option>
          {meta.data?.locations.map(l => <option key={l}>{l}</option>)}
        </select>
      </label>
    </div>
  )
  if (error && !data) return <>{head}<div className="error">{error}</div></>
  if (!data) return <>{head}<div className="empty">Loading…</div></>
  if (!data.nodes.length) return <>{head}<div className="card"><p className="muted">No traces in this window{location && ` for ${location}`}.</p></div></>

  const { pos, layer, lanes, width, height } = layout(data.nodes, data.edges)
  const maxRate = Math.max(...data.edges.map(e => e.rate), 0.001)
  return (
    <>
      {head}
      <div className="card smap">
        <svg viewBox={`0 0 ${width} ${height}`} style={{ maxWidth: width * 1.15 }} role="img" aria-label="Service dependency map">
          <defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="13" markerHeight="13" orient="auto-start-reverse" markerUnits="userSpaceOnUse"><path d="M0 0 10 5 0 10z" className="arrow" /></marker></defs>
          {data.edges.map(e => {
            const a = pos.get(e.from), b = pos.get(e.to)
            if (!a || !b || e.from === e.to) return null
            const r = edgePath(a, b, layer.get(e.from) ?? 0, layer.get(e.to) ?? 0, lanes)
            const bad = e.errorRate >= 5
            return (
              <a key={e.from + '>' + e.to} className={`edge${bad ? ' bad' : ''}`}
                href={href('/explore', { filter: `${scope(e.to)} kind:server`, tab: bad ? 'explain' : 'traces' })}>
                <title>{`${e.from} → ${e.to}\n${fmt(e.rate, '/s')} · ${fmt(e.errorRate, '%')} errors · p95 ${fmt(e.p95, 'ms')}`}</title>
                <path d={r.d} markerEnd="url(#arrow)" strokeWidth={1.5 + 3 * Math.sqrt(e.rate / maxRate)} />
                <text x={r.lx} y={r.ly - 7} textAnchor="middle">
                  {fmt(e.rate, '/s')}{e.errorRate >= 1 ? ` · ${bad ? '✕ ' : ''}${fmt(e.errorRate, '%')} err` : ''}
                </text>
              </a>
            )
          })}
          {data.nodes.map(n => {
            const p = pos.get(n.service)!
            return (
              <a key={n.service} className={`node ${n.status}`} href={href('/explore', { filter: scope(n.service) })}>
                <title>{`${n.service} — ${STATUS[n.status].label}`}</title>
                <rect className="box" x={p.x} y={p.y} width={NODE_W} height={NODE_H} rx={7} />
                <circle cx={p.x + 20} cy={p.y + 20} r={8.5} fill={STATUS_VAR[n.status]} />
                <text className="glyph" x={p.x + 20} y={p.y + 23.5}>{STATUS[n.status].glyph}</text>
                <text x={p.x + 36} y={p.y + 24.5}>{n.service}</text>
                <text className="sub" x={p.x + 12} y={p.y + 43}>
                  {n.rate ? `${fmt(n.rate, '/s')} · ${fmt(n.errorRate, '%')} err · p95 ${fmt(n.p95, 'ms')}` : 'no inbound requests'}
                </text>
              </a>
            )
          })}
        </svg>
        <div className="legend">
          <span>Arrows point from caller to callee; thicker means more traffic.</span>
          <span>✕ marks edges with ≥ 5% errors.</span>
          <span>Click a service or an edge to explore it.</span>
          {data.sampled && <span>Edge figures cover the most recent 100,000 calls in this window.</span>}
        </div>
      </div>
    </>
  )
}
