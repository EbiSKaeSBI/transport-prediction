import { useEffect, useMemo, useState } from 'react'
import { Store } from './store'
import { detectSource } from './source'
import type { DataSource } from './source'
import MapView from './MapView'
import type { RouteFC } from './geo'
import { paintRoutes } from './geo'
import CanvasMap from './CanvasMap'
import { Clock, IncidentRail, MetricsPanel, ModelPanel, VehicleCard } from './panels'
import './App.css'

function hasWebGL(): boolean {
  try {
    const c = document.createElement('canvas')
    return !!c.getContext('webgl2')
  } catch {
    return false
  }
}

const RATES = [1, 10, 60, 300] as const

export default function App() {
  const store = useMemo(() => new Store(), [])
  const [routes, setRoutes] = useState<RouteFC | null>(null)
  const [stopNames, setStopNames] = useState<Map<number, string>>(new Map())
  const [source, setSource] = useState<DataSource | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [rate, setRate] = useState<number>(60)
  const [paused, setPaused] = useState(false)
  const [selected, setSelected] = useState<number | null>(null)
  const [live] = useState(() => new URLSearchParams(window.location.search).has('ws'))
  const [webgl] = useState(hasWebGL)

  useEffect(() => {
    let mounted = true
    fetch('/demo/routes.geojson')
      .then(r => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<RouteFC>
      })
      .then(fc => {
        if (!mounted) return
        const names = new Map<number, string>()
        for (const f of fc.features) {
          if (f.properties.kind === 'stop' && f.properties.name) {
            names.set(Number(f.properties.stop_id), String(f.properties.name))
          }
        }
        setStopNames(names)
        setRoutes(paintRoutes(fc))
      })
      .catch(() => {
        if (mounted) setError('Нет demo/routes.geojson — сгенерируйте поток: python3 scripts/make_dashboard_demo.py')
      })
    return () => { mounted = false }
  }, [])

  useEffect(() => {
    let mounted = true
    let src: DataSource | null = null
    detectSource(store).then(async s => {
      if (!mounted) { s.stop(); return }
      src = s
      try {
        await s.start()
      } catch {
        setError('Не удалось подключить источник потока')
      }
      if (mounted) setSource(s)
    })
    return () => { mounted = false; src?.stop() }
  }, [store])

  useEffect(() => { source?.setRate?.(rate) }, [source, rate])
  useEffect(() => { source?.setPaused?.(paused) }, [source, paused])

  return (
    <div className="app">
      <header>
        <div className="brand">
          <b>Предиктор отклонений</b>
          <span className="muted">наземный транспорт · горизонт 10–15 мин</span>
        </div>
        <div className="controls">
          <Clock store={store} />
          <span className={`badge ${live ? 'live' : 'demo'}`}>{live ? 'live WS' : 'demo-реплей'}</span>
          {!live && (
            <>
              <select value={rate} onChange={e => setRate(Number(e.target.value))} aria-label="скорость потока">
                {RATES.map(r => <option key={r} value={r}>×{r}</option>)}
              </select>
              <button onClick={() => setPaused(p => !p)}>{paused ? '▶' : '⏸'}</button>
            </>
          )}
          {error && <span className="error">{error}</span>}
        </div>
      </header>
      <main>
        <div className="map-col">
          {!webgl && <div className="warnbar">GPU/WebGL недоступен — включён canvas-фолбэк (требование офлайн-демо).</div>}
          {webgl
            ? <MapView store={store} routes={routes} selected={selected} onSelect={setSelected} />
            : <CanvasMap store={store} routes={routes} selected={selected} onSelect={setSelected} />}
          <div className="legend">
            <span><i style={{ background: '#2ecc71' }} /> ≤ 60 с</span>
            <span><i style={{ background: '#f1c40f' }} /> 60–120 с</span>
            <span><i style={{ background: '#e74c3c' }} /> ≥ 120 с (инцидент)</span>
            <span><i style={{ background: '#7f8c8d' }} /> нет свежей телеметрии</span>
            <span><i style={{ background: '#5dade2' }} /> прогноз не сформирован</span>
          </div>
        </div>
        <aside>
          <IncidentRail store={store} stopNames={stopNames} live={live} />
          <VehicleCard store={store} trId={selected} />
          <MetricsPanel store={store} />
          <ModelPanel store={store} />
        </aside>
      </main>
    </div>
  )
}
