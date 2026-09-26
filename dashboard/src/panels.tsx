import * as echarts from 'echarts'
import { useEffect, useRef, useState } from 'react'
import type { Store } from './store'
import type { FrameEvent } from './types'
import { apiBase } from './source'
import { formatClock } from './time'
import { RISK_COLORS, vehicleRisk } from './risk'
import { useStoreRev } from './useStoreRev'

/**
 * baseline — не модель, а повтор измеренного отставания (predictor/baseline.go).
 * Число в карточке реальное, но называть его «прогнозом» нельзя: панель
 * обязана показывать, что оценка копится из факта, а не предсказана.
 */
function isFallback(i: { source?: string }): boolean {
  // источник приходит и как 'baseline' (кадр прогноза), и как
  // 'gateway:baseline' (карточка инцидента) — правило одно на оба
  return (i.source ?? '').endsWith('baseline')
}

export function Clock({ store }: { store: Store }) {
  useStoreRev(store)
  return <span className="clock mono">{formatClock(store.clock)}</span>
}

export function StreamEndedBadge({ store }: { store: Store }) {
  useStoreRev(store)
  return store.streamEnded ? <span className="badge ended">поток завершён</span> : null
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
              <span className="muted">{isFallback(i) ? 'отставание сейчас' : 'опоздание'} · ТС {i.tr_id}</span>
            </div>
            <div className="row2">
              {i.prev_stop_id != null && (
                <>{stopNames.get(i.prev_stop_id) ?? `остановка ${i.prev_stop_id}`}{' → '}</>
              )}
              {stopNames.get(i.target_stop_id) ?? `остановка ${i.target_stop_id}`}
            </div>
            <div className="row3 muted">
              {formatClock(i.ts)} · горизонт {(i.horizon_s / 60).toFixed(1)} мин
              {i.p_late != null ? ` · P(опозд.) ${(i.p_late * 100).toFixed(0)}%` : ' · P(опозд.) —'} · {i.reason}
            </div>
            <div className="row3 muted">
              {isFallback(i) ? 'оценка: правило-эталон, модель не подключена' : `модель: ${i.source}`}
            </div>
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

export function VehicleList({ store, selected, onSelect }: {
  store: Store
  selected: number | null
  onSelect: (trId: number) => void
}) {
  useStoreRev(store)
  const rows = [...store.vehicles.values()].sort((a, b) => a.tr_id - b.tr_id)
  if (rows.length <= 1) return null
  return (
    <section className="panel">
      <h2>Транспорт ({rows.length})</h2>
      {/* Список нужен при нескольких машинах: карточка ниже показывает одну
          выбранную, а выбрать машину можно только кликом по точке на карте —
          без списка правая колонка остаётся пустой, пока диспетчер не угадает
          пиксель. Строка = измерение сейчас и прогноз у цели рядом, риск
          показан тем же цветом, что и точка на карте. */}
      <table className="kv">
        <tbody>
          {rows.map((v) => {
            const f = v.lastFrame
            const risk = vehicleRisk(v, store.clock)
            const hasPrediction = f != null && (f.source != null || f.values.predicted_dev_s != null)
            return (
              <tr
                key={v.tr_id}
                className={`list-row${v.tr_id === selected ? ' row-selected' : ''}`}
                onClick={() => onSelect(v.tr_id)}
              >
                <td>
                  <i className="dot" style={{ background: RISK_COLORS[risk] }} /> ТС {v.tr_id}
                </td>
                <td>
                  {f?.values.cur_dev_s != null ? `${Math.round(f.values.cur_dev_s)} с` : '—'}
                </td>
                <td>
                  {hasPrediction && f?.values.predicted_dev_s != null
                    ? `${Math.round(f.values.predicted_dev_s)} с`
                    : '—'}
                </td>
                <td>{f?.p_late != null ? `${(f.p_late * 100).toFixed(0)}%` : '—'}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <div className="hint">измеренное сейчас · прогноз у цели · P(опозд.)</div>
    </section>
  )
}

export function VehicleCard({ store, trId }: { store: Store; trId: number | null }) {
  useStoreRev(store)
  if (trId == null) return null
  const v = store.vehicles.get(trId)
  if (!v) return null
  const f: FrameEvent | null = v.lastFrame
  // Офлайн-кадр из demo/stream.ndjson несёт только признаки: там нет ни
  // прогноза, ни источника, и рисовать «прогноз отставания 0 с» значило бы
  // выдумать оценку. Блок прогноза — только там, где он реально пришёл.
  const hasPrediction = f != null && (f.source != null || f.values.predicted_dev_s != null)
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
        <>
          {hasPrediction && (
            <table className="kv">
              <tbody>
                {/* Измеренное отставание в этом блоке не повторяется: оно уже
                    есть в телеметрии ниже, рядом со скоростью и свежестью
                    данных. Здесь только прогноз — иначе одно и то же число
                    читается дважды и рушит смысл разделения. */}
                {isFallback(f!) ? (
                  /* baseline по построению повторяет измеренное: называть его
                     «прогнозом» нельзя, поэтому строки прогноза нет. */
                  <tr>
                    <td>прогноз</td>
                    <td>baseline — копия измеренного</td>
                  </tr>
                ) : (
                  <tr>
                    <td>прогноз отставания, с</td>
                    <td>{Math.round(f.values.predicted_dev_s ?? 0)}</td>
                  </tr>
                )}
                <tr>
                  <td>источник</td>
                  <td>
                    {isFallback(f!)
                      ? 'baseline — модель не подключена'
                      : f.source
                        ? `модель ${f.model_version ?? f.source}`
                        : 'не указан (replay-кадр)'}
                  </td>
                </tr>
                <tr>
                  <td>P(опозд.)</td>
                  {/* нет вероятности — это «не оценивали», а не 0% */}
                  <td>{f.p_late != null ? `${(f.p_late * 100).toFixed(0)}%` : '—'}</td>
                </tr>
                {f.reason && <tr><td>причина</td><td>{f.reason}</td></tr>}
                {f.missing != null && f.missing.length > 0 && (
                  <tr>
                    <td>не хватает признаков</td>
                    <td>{f.missing.length} — {f.missing.join(', ')}</td>
                  </tr>
                )}
              </tbody>
            </table>
          )}
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
        </>
      ) : <p className="muted">Прогнозных кадров по этому ТС ещё не было.</p>}
    </section>
  )
}

/** Снимок GET /api/v1/metrics/latency (контракт зафиксирован Go-тестом
 *  dashboard_api_test.go; пустые окна в ответе отсутствуют, а не нулевые). */
interface Quantiles {
  count: number; total: number
  p50_s: number; p95_s: number; p99_s: number; max_s: number
}
interface LatencySnapshot {
  uptime_s: number
  prediction_latency?: Quantiles
  inference_latency?: Quantiles
  queue?: {
    depth: number; submitted: number; predicted: number; dropped: number
    batches: number; batched_frames: number
    batch_size?: Quantiles
  }
  throughput?: { predictions_total: number; predictions_per_s: number; inference_per_s: number }
  stream?: { clients: number; sent_total: number; dropped_total: number }
  vehicles_known?: number
}

/** Квантили в миллисекундах: доли секунды в секундах на панели читаются
 *  как нули, а «23 мс» и «410 мс» различаются глазом сразу. */
function fmtQ(q: Quantiles | undefined): string {
  if (!q || q.total === 0) return '—'
  const ms = (v: number) => Math.round(v * 1000)
  return `${ms(q.p50_s)} / ${ms(q.p95_s)} / ${ms(q.p99_s)} мс`
}

export function MetricsPanel({ store, live }: { store: Store; live: boolean }) {
  useStoreRev(store)
  const holder = useRef<HTMLDivElement | null>(null)
  const chart = useRef<echarts.ECharts | null>(null)
  const [snap, setSnap] = useState<LatencySnapshot | null>(null)
  useEffect(() => {
    if (!holder.current) return
    chart.current = echarts.init(holder.current)
    return () => { chart.current?.dispose(); chart.current = null }
  }, [])
  // Опрос раз в 5 с — не polling основной ленты (позиции и инциденты идут
  // по WS), а снимок агрегатов: latency-окно гейтвея обновляется само по
  // тикам, и дергать его чаще смысла нет.
  useEffect(() => {
    if (!live) return
    let mounted = true
    const load = () => fetch(`${apiBase()}/api/v1/metrics/latency`)
      .then(r => (r.ok ? r.json() : null))
      .then(d => { if (mounted && d) setSnap(d as LatencySnapshot) })
      .catch(() => { /* gateway ещё не поднялся или baseline — панель остаётся честной */ })
    load()
    const timer = setInterval(load, 5000)
    return () => { mounted = false; clearInterval(timer) }
  }, [live])
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
      {live ? (
        <table className="kv">
          <tbody>
            <tr><td>инференция p50/p95/p99</td><td>{fmtQ(snap?.inference_latency)}</td></tr>
            <tr><td>прогноз целиком p50/p95/p99</td><td>{fmtQ(snap?.prediction_latency)}</td></tr>
            <tr><td>очередь · сброшено</td>
              <td>{snap?.queue ? `${snap.queue.depth} / ${snap.queue.dropped}` : '—'}</td></tr>
            <tr><td>прогнозов в секунду</td>
              <td>{snap?.throughput ? snap.throughput.predictions_per_s.toFixed(3) : '—'}</td></tr>
            <tr><td>лента · отправлено / подписчиков</td>
              <td>{snap?.stream ? `${snap.stream.sent_total} / ${snap.stream.clients}` : '—'}</td></tr>
          </tbody>
        </table>
      ) : (
        <p className="muted tiny">
          В replay прогноз считает правило-фолбэк; p50/p95/p99 инференции,
          очередь и пропускная способность — в live-режиме
          (GET /api/v1/metrics/latency).
        </p>
      )}
    </section>
  )
}

export function ModelPanel({ store }: { store: Store }) {
  useStoreRev(store)
  const m = store.model
  // Числа из паспорта — сырые float из CatBoost; диспетчеру хватит десятих
  // секунды, а «68.27979460888994» читается как баг.
  const mae = (v: number | null | undefined) => (v != null ? `${v.toFixed(1)} с` : '—')
  const date = (iso: string | null | undefined) => {
    if (!iso) return '—'
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? iso
      : d.toLocaleString('ru-RU', { dateStyle: 'medium', timeStyle: 'short' })
  }
  return (
    <section className="panel">
      <h2>Модель</h2>
      {m ? (
        <table className="kv">
          <tbody>
            <tr><td>версия</td><td>{m.version}</td></tr>
            <tr><td>MAE validate (оракул)</td><td>{mae(m.mae_validate_s)}</td></tr>
            <tr><td>MAE test</td><td>{mae(m.mae_test_s)}</td></tr>
            <tr>
              <td>P(опозд.)</td>
              <td>{m.late
                ? `${m.late.version}: AUC ${m.late.auc_holdout?.toFixed(3) ?? '—'} (порог ${m.late.threshold_s} с)`
                : 'голова не подключена'}</td>
            </tr>
            <tr><td>обучена</td><td>{date(m.trained_at)}</td></tr>
          </tbody>
        </table>
      ) : <p className="muted">Событие model ещё не приходило.</p>}
      {m?.note && <p className="muted tiny">{m.note}</p>}
    </section>
  )
}
