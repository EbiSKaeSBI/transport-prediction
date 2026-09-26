import { nearestSegment, type RouteGeom } from './geo'
import type { Risk, VehicleState } from './types'

// Риск до подключения модели — задокументированное правило-фолбэк
// (ADR 0003, docs/dashboard.md): predicted = cur_dev_s официальной точки;
// порог инцидента 120 с взят из docs/architecture.md §4.1. Когда CatBoost
// (этап 3) подключится, gateway начнёт присылать predicted_delay_s и P(late),
// и функция переключится на них без изменения интерфейса.
// Этап 5, live-лента: gateway шлёт классификацию risk (в ней уже учтены
// predicted_dev_s И p_late, пороги те же 60/120 + 0.3/0.6, см.
// backend/internal/gateway/incidents.go). Если кадр пришёл с нею — цвет
// берется оттуда; replay-кадры поля risk не несут и живут на правиле-фолбэке.
export const YELLOW_THRESHOLD_S = 60
export const RED_THRESHOLD_S = 120
export const STALE_AFTER_S = 180

export function vehicleRisk(v: VehicleState, clockTs: number): Risk {
  if (clockTs - v.ts > STALE_AFTER_S) return 'stale'
  const f = v.lastFrame
  if (f?.risk) return f.risk
  const dev = f?.cur_dev_s
  if (dev == null) return 'unknown'
  if (dev >= RED_THRESHOLD_S) return 'red'
  if (dev >= YELLOW_THRESHOLD_S) return 'yellow'
  return 'green'
}

export const RISK_COLORS: Record<Risk, string> = {
  green: '#2ecc71',
  yellow: '#f1c40f',
  red: '#e74c3c',
  stale: '#7f8c8d',
  unknown: '#5dade2',
}

// --- участки маршрутов с цветом риска (docs/architecture.md §6) -----------

/**
 * Допуск привязки: ТС красит звено полилинии только стоя на нём фактически.
 * 150 м — тот же порядок, что у порога остановочного детектора в gateway
 * (dist<80 м) плюс запас на сжатие полилинии в плане; дальше — считаем,
 * что машина не на этом маршруте, и не красим ничего.
 */
export const ON_ROUTE_TOLERANCE_M = 150

// старшинство для слияния: худший риск двух машин на одном звене побеждает
const RISK_PRECEDENCE: Risk[] = ['red', 'yellow', 'stale', 'unknown', 'green']

export function mergeRisk(a: Risk, b: Risk): Risk {
  return RISK_PRECEDENCE.indexOf(a) <= RISK_PRECEDENCE.indexOf(b) ? a : b
}

/**
 * riskSegmentsFC — звенья маршрутов, на которых едут ТС, с их риском.
 * Раскраска клиентская и намеренно «фактическая»: сегмент красится риском
 * машины, проецируемой на ближайшую полилинию (live-полилиния = остановки
 * плана в порядке следования, см. routesFromPlan). Пустой FC — не ошибка:
 * без маршрутов или без машин карта остаётся с линиями плана.
 */
export function riskSegmentsFC(
  geoms: RouteGeom[], vehicles: Iterable<VehicleState>, clock: number,
): {
  type: 'FeatureCollection'
  features: { type: 'Feature'; properties: { kind: string; risk: Risk }; geometry: { type: 'LineString'; coordinates: [number, number][] } }[]
} {
  const rows = new Map<string, { risk: Risk; seg: [number, number][] }>()
  for (const v of vehicles) {
    if (!Number.isFinite(v.lon) || !Number.isFinite(v.lat)) continue
    let best: { g: RouteGeom; idx: number; dist_m: number } | null = null
    for (const g of geoms) {
      // маршрут ТС известен (live: tr_id плана) — не перекрашиваем чужой
      if (g.tr_id !== 0 && v.tr_id !== 0 && g.tr_id !== v.tr_id) continue
      const p = nearestSegment(g.coords, v.lon, v.lat)
      if (p.dist_m <= ON_ROUTE_TOLERANCE_M && (best === null || p.dist_m < best.dist_m)) {
        best = { g, ...p }
      }
    }
    if (!best) continue
    const key = `${best.g.route}:${best.idx}`
    const risk = vehicleRisk(v, clock)
    const row = rows.get(key)
    if (row) row.risk = mergeRisk(row.risk, risk)
    else rows.set(key, { risk, seg: [best.g.coords[best.idx], best.g.coords[best.idx + 1]] })
  }
  return {
    type: 'FeatureCollection',
    features: Array.from(rows.values(), (r) => ({
      type: 'Feature' as const,
      // ключ звена не дублируем: слой красится только полем risk
      properties: { kind: 'seg', risk: r.risk },
      geometry: { type: 'LineString' as const, coordinates: r.seg },
    })),
  }
}
