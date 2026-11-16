import { StrictMode, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { TimeCtx, href, useRoute } from './api'
import Health from './Health'
import Explore from './Explore'
import TraceView from './TraceView'
import { DashboardList, DashboardView } from './Dashboards'
import './styles.css'

const RANGES = ['15m', '1h', '6h', '24h']

function App() {
  const route = useRoute()
  const [range, setRange] = useState(() => localStorage.getItem('range') ?? '1h')
  const [tick, setTick] = useState(0)
  const [live, setLive] = useState(true)
  useEffect(() => {
    if (!live) return
    const t = setInterval(() => { if (!document.hidden) setTick(n => n + 1) }, 10_000)
    return () => clearInterval(t)
  }, [live])
  useEffect(() => localStorage.setItem('range', range), [range])

  const seg = route.path.split('/').filter(Boolean)
  const section = seg[0] ?? 'health'
  let page
  if (section === 'explore') page = <Explore route={route} />
  else if (section === 'trace' && seg[1]) page = <TraceView key={seg[1]} id={seg[1]} route={route} />
  else if (section === 'dashboards' && seg[1]) page = <DashboardView key={seg[1]} id={seg[1]} edit={route.params.has('edit')} />
  else if (section === 'dashboards') page = <DashboardList />
  else page = <Health />

  const nav = [['health', '/', 'Health'], ['explore', '/explore', 'Explore'], ['dashboards', '/dashboards', 'Dashboards']]
  return (
    <TimeCtx.Provider value={{ range, tick }}>
      <header className="top">
        <a className="brand" href={href('/')}><svg viewBox="0 0 32 32" width="20" height="20" aria-hidden><circle cx="16" cy="16" r="13" fill="none" stroke="currentColor" strokeWidth="3" /><circle cx="16" cy="16" r="5" fill="currentColor" /></svg>Telepathy</a>
        <nav>
          {nav.map(([k, p, label]) => <a key={k} href={href(p)} className={section === k || (k === 'explore' && section === 'trace') ? 'on' : ''}>{label}</a>)}
        </nav>
        <span className="spacer" />
        <div className="seg" role="group" aria-label="Time range">
          {RANGES.map(r => <button key={r} className={r === range ? 'on' : ''} onClick={() => setRange(r)}>{r}</button>)}
        </div>
        <button className={`btn live${live ? ' on' : ''}`} onClick={() => setLive(!live)} title={live ? 'Refreshing every 10s — click to pause' : 'Paused — click to resume'}>
          <i />{live ? 'Live' : 'Paused'}
        </button>
      </header>
      <main>{page}</main>
    </TimeCtx.Provider>
  )
}

createRoot(document.getElementById('root')!).render(<StrictMode><App /></StrictMode>)
