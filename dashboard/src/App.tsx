import { useEffect, useMemo, useState } from 'react'
import { Store } from './store'
import { apiBase, detectSource } from './source'
import type { DataSource } from './source'
import MapView from './MapView'
import MapBoundary from './MapBoundary'
import type { PlanStop, RouteFC } from './geo'
import { paintRoutes, routesFromPlan } from './geo'
import CanvasMap from './CanvasMap'
import {
  Clock,
  IncidentRail,
  MetricsPanel,
  ModelPanel,
  StreamEndedBadge,
  VehicleCard,
  VehicleList,
} from './panels'
import { useStoreRev } from './useStoreRev'
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
  const [picked, setPicked] = useState<number | null>(null)
  const [live] = useState(() => {
    // То же правило, что в detectSource: live по умолчанию, replay только
    // явно (?replay или ?stream=…). Свой ?ws= тоже live.
    const p = new URLSearchParams(window.location.search)
    return p.has('ws') || (p.get('replay') == null && p.get('stream') == null)
  })
  const [webgl] = useState(hasWebGL)
  // hasWebGL() отвечает только «есть ли WebGL». Реальная карта может упасть
  // позже (воркер maplibre, стиль), и тогда без границы ошибок React
  // размонтирует весь дашборд. Здесь падение карты не стоит панелей.
  const [mapFailed, setMapFailed] = useState(false)
  const canvasFallback = !webgl || mapFailed

  useEffect(() => {
    let mounted = true
    const takeNames = (fc: RouteFC) => {
      const names = new Map<number, string>()
      for (const f of fc.features) {
        if (f.properties.kind === 'stop' && f.properties.name) {
          names.set(Number(f.properties.stop_id), String(f.properties.name))
        }
      }
      return names
    }
    if (live) {
      // Live: маршруты — план-график самого gateway (GET /api/v1/routes +
      // /routes/{tr}/stops). Geojson-файла здесь нет и не будет: источник
      // истины — тот же schedule, по которому считаются прогнозы.
      const base = apiBase()
      fetch(`${base}/api/v1/routes`)
        .then(async r => {
          if (!r.ok) throw new Error(`HTTP ${r.status}`)
          const data = await r.json() as { routes?: { tr_id: number }[] }
          const lists = await Promise.all((data.routes ?? []).map(rt =>
            fetch(`${base}/api/v1/routes/${rt.tr_id}/stops`)
              .then(s => (s.ok ? s.json() : { stops: [] }))
              .then(d => (d.stops ?? []) as PlanStop[])))
          if (!mounted) return
          const fc = paintRoutes(routesFromPlan(lists))
          setStopNames(takeNames(fc))
          setRoutes(fc)
        })
        .catch(() => {
          // не fatal: машины видны и без плана (fitBy по траекториям в MapView)
          if (mounted) setError('План-график с gateway не пришёл — карта без маршрутов (serve без --plan?)')
        })
      return () => { mounted = false }
    }
    fetch('/demo/routes.geojson')
      .then(r => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<RouteFC>
      })
      .then(fc => {
        if (!mounted) return
        setStopNames(takeNames(fc))
        setRoutes(paintRoutes(fc))
      })
      .catch(() => {
        if (mounted) setError('Нет demo/routes.geojson — сгенерируйте поток: python3 scripts/make_dashboard_demo.py')
      })
    return () => { mounted = false }
  }, [live])

  useEffect(() => {
    let mounted = true
    let src: DataSource | null = null
    detectSource(store).then(async s => {
      if (!mounted) { s.stop(); return }
      src = s
      try {
        await s.start()
      } catch {
        if (mounted) setError('Не удалось подключить источник потока')
      }
      if (!mounted) { s.stop(); return } // unmount прилетел во время start()
      setSource(s)
    })
    return () => { mounted = false; src?.stop() }
  }, [store])

  useEffect(() => { source?.setRate?.(rate) }, [source, rate])
  useEffect(() => { source?.setPaused?.(paused) }, [source, paused])

  // Карточка ТС показывается для выбранной машины, а выбрать её на карте можно
  // только кликом по точке в 6 пикселей. Пока машину не выбрали, показываем
  // первую по tr_id: иначе правая колонка пустая — при одной машине это просто
  // потерянные телеметрия и прогноз, при нескольких диспетчер вообще не видит
  // машин, пока не угадает пиксель на карте. Значение выводится из стора, а не
  // копится в effect: так не нужен лишний рендер, и снятие выбора кликом по
  // другой ТС работает как раньше. Подписка нужна ради самого рендера: значение
  // ревизии не используется, перерисовку инициирует useSyncExternalStore.
  useStoreRev(store)
  const firstTrId = [...store.vehicles.keys()].sort((a, b) => a - b)[0] ?? null
  const selected = picked ?? firstTrId

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
          {!live && <StreamEndedBadge store={store} />}
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
          {webgl && mapFailed && <div className="warnbar">MapLibre не инициализировался (см. консоль) — включён canvas-фолбэк: карта в сетке координат, панели живут.</div>}
          {canvasFallback
            ? <CanvasMap store={store} routes={routes} selected={selected} onSelect={setPicked} />
            : (
              <MapBoundary onFailure={() => setMapFailed(true)}>
                <MapView store={store} routes={routes} selected={selected} onSelect={setPicked} />
              </MapBoundary>
            )}
          <div className="legend">
            {/* подписи — про прогноз у цели: риск и цвет считаются по нему,
                а измеренное отклонение «сейчас» может быть другим и намеренно
                показывается отдельной строкой в карточке ТС */}
            <span className="legend-hint">цвет — прогноз отставания у цели (точка машины, пройденный участок и звено маршрута):</span>
            <span><i style={{ background: '#2ecc71' }} /> &lt; 60 с</span>
            <span><i style={{ background: '#f1c40f' }} /> 60–120 с</span>
            <span><i style={{ background: '#e74c3c' }} /> ≥ 120 с (инцидент)</span>
            <span><i style={{ background: '#7f8c8d' }} /> нет свежей телеметрии</span>
            <span><i style={{ background: '#5dade2' }} /> прогноз не сформирован</span>
          </div>
        </div>
        <aside>
          <IncidentRail store={store} stopNames={stopNames} live={live} />
          <VehicleList store={store} selected={selected} onSelect={setPicked} />
          <VehicleCard store={store} trId={selected} />
          <MetricsPanel store={store} live={live} />
          <ModelPanel store={store} />
        </aside>
      </main>
    </div>
  )
}
