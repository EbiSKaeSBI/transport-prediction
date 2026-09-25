import { useSyncExternalStore } from 'react'
import * as echarts from 'echarts'
import { useEffect, useRef } from 'react'
import type { Store } from './store'
import type { FrameEvent } from './types'
import { formatClock } from './time'

function useStoreRev(store: Store): number {
  return useSyncExternalStore(store.subscribe, store.getSnapshot)
}

export function Clock({ store }: { store: Store }) {
  useStoreRev(store)
  return <span className="clock">{formatClock(store.clock)}</span>
}

export function IncidentRail({ store, stopNames, live }: {
  store: Store
  stopNames: Map<number, string>
  live: boolean
}) {
  useStoreRev(store)
  const items = Array.from(store.incidents.values()).sort((a, b) => b.ts - a.ts)
  return (
    <section className="panel">
      <h2>Инциденты <span className="muted">{items.filter(i => !i.acked).length} откр.</span></h2>
      {items.length === 0 && <p className="muted">Нет прогнозов ≥ 120 с на горизонте 10–15 мин.</p>}
      <ul className="incidents">
        {items.map(i => (
          <li key={i.id} className={i.acked ? 'acked' : ''}>
            <div className="row1">
              <b>+{Math.round(i.predicted_delay_s)} с</b>
              <span className="muted">опоздание · ТС {i.tr_id}</span>
            </div>
            <div className="row2">{stopNames.get(i.target_stop_id) ?? `остановка ${i.target_stop_id}`}</div>
            <div className="row3 muted">
              {formatClock(i.ts)} · горизонт {(i.horizon_s / 60).toFixed(1)} мин · {i.reason}
            </div>
            <div className="row3 muted">модель: {i.source}</div>
            {!i.acked && (
              <button onClick={() => {
                if (live) fetch(`/api/v1/incidents/${encodeURIComponent(i.id)}/ack`, { method: 'POST' }).catch(() => {})
                store.ack(i.id)
              }}>Подтвердить</button>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

const FEATURE_LABELS: Record<string, string> = {
  cur_dev_s: 'отставание сейчас, с',
  horizon_s: 'горизонт, с',
  dwell_current_s: 'простой сейчас, с',
  dwell_last_s: 'последняя стоянка, с',
  speed_current: 'скорость сейчас, км/ч',
  speed_seg_avg: 'средняя на сегменте, км/ч',
  distance_to_target_m: 'до цели, м',
  stops_remaining: 'остановок до цели',
  drift_last3_slope: 'дрейф последних 3, с/ост.',
  consecutive_late_stops: 'подряд с опозданием',
  headway_s: 'интервал за ТС, с',
  slack_s: 'запас графика, с',
  staleness_s: 'свежесть данных, с',
  points_in_window: 'точек в окне',
  lag_s: 'лаг приёма, с',
}

export function VehicleCard({ store, trId }: { store: Store; trId: number | null }) {
  useStoreRev(store)
  if (trId == null) return null
  const v = store.vehicles.get(trId)
  if (!v) return null
  const f: FrameEvent | null = v.lastFrame
  return (
    <section className="panel">
      <h2>ТС {v.tr_id}</h2>
      <table className="kv">
        <tbody>
          <tr><td>позиция</td><td>{v.lon.toFixed(5)}, {v.lat.toFixed(5)}</td></tr>
          <tr><td>скорость</td><td>{v.speed.toFixed(1)} км/ч</td></tr>
          <tr><td>последняя телеметрия</td><td>{formatClock(v.ts)}</td></tr>
        </tbody>
      </table>
      {f ? (
        <table className="kv">
          <tbody>
            {Object.entries(FEATURE_LABELS).map(([key, label]) => {
              const val = key === 'cur_dev_s' ? f.cur_dev_s : f.values[key]
              if (val == null) return null
              return <tr key={key}><td>{label}</td><td>{typeof val === 'number' ? Math.round(val * 10) / 10 : val}</td></tr>
            })}
            {f.ambiguous && <tr><td>цель</td><td>две кандидатуры (ничья в плане)</td></tr>}
          </tbody>
        </table>
      ) : <p className="muted">Прогнозных кадров по этому ТС ещё не было.</p>}
    </section>
  )
}

export function MetricsPanel({ store }: { store: Store }) {
  useStoreRev(store)
  const holder = useRef<HTMLDivElement | null>(null)
  const chart = useRef<echarts.ECharts | null>(null)
  useEffect(() => {
    if (!holder.current) return
    chart.current = echarts.init(holder.current)
    return () => { chart.current?.dispose(); chart.current = null }
  }, [])
  useEffect(() => {
    const buckets = store.rate
    chart.current?.setOption({
      animation: false,
      grid: { left: 34, right: 8, top: 8, bottom: 18 },
      xAxis: { type: 'category', show: false, data: buckets.map(b => b.t) },
      yAxis: { type: 'value', axisLabel: { color: '#8b98a5', fontSize: 10 }, splitLine: { lineStyle: { color: '#1d242c' } } },
      series: [{ type: 'bar', data: buckets.map(b => b.n), itemStyle: { color: '#3b82f6' }, barWidth: '70%' }],
    })
  })
  return (
    <section className="panel">
      <h2>Поток</h2>
      <div className="metrics-grid">
        <div><b>{store.eventsPerSecond().toFixed(1)}</b><span>событий/с</span></div>
        <div><b>{store.activeVehicles()}/{store.vehicles.size}</b><span>активных ТС</span></div>
        <div><b>{store.framesTotal}</b><span>кадров</span></div>
        <div><b>{store.openIncidents()}</b><span>откр. инцидентов</span></div>
      </div>
      <div ref={holder} className="chart" />
      <p className="muted tiny">p50/p95/p99 инференса появятся вместе с ML-сервисом (этап 3) — сейчас прогноз считает правило-фолбэк.</p>
    </section>
  )
}

export function ModelPanel({ store }: { store: Store }) {
  useStoreRev(store)
  const m = store.model
  return (
    <section className="panel">
      <h2>Модель</h2>
      {m ? (
        <table className="kv">
          <tbody>
            <tr><td>версия</td><td>{m.version}</td></tr>
            <tr><td>MAE validate (оракул)</td><td>{m.mae_validate_s != null ? `${m.mae_validate_s} с` : '—'}</td></tr>
            <tr><td>MAE test</td><td>{m.mae_test_s != null ? `${m.mae_test_s} с` : '—'}</td></tr>
            <tr><td>обучена</td><td>{m.trained_at ?? '—'}</td></tr>
          </tbody>
        </table>
      ) : <p className="muted">Событие model ещё не приходило.</p>}
      {m?.note && <p className="muted tiny">{m.note}</p>}
    </section>
  )
}
