// Гео-утилиты вне React-компонентов (требование react-refresh: файлы
// компонентов экспортят только компоненты).

export interface RouteFC {
  type: 'FeatureCollection'
  features: {
    type: 'Feature'
    properties: Record<string, unknown>
    geometry: { type: string; coordinates: unknown }
  }[]
}

const ROUTE_PALETTE = [
  '#e6194b', '#3cb44b', '#4363d8', '#f58231', '#911eb4', '#46f0f0',
  '#f032e6', '#bcf60c', '#fabebe', '#008080', '#e6beff', '#800000', '#000075',
]

export function routeColor(route: string): string {
  let h = 0
  for (const ch of route) h = (h * 31 + ch.charCodeAt(0)) >>> 0
  return ROUTE_PALETTE[h % ROUTE_PALETTE.length]
}

/** докрашивает линии маршрутов палитрой — один раз при загрузке geojson */
export function paintRoutes(fc: RouteFC): RouteFC {
  for (const f of fc.features) {
    if (f.properties.kind === 'route') {
      f.properties.color = routeColor(String(f.properties.route ?? ''))
    }
  }
  return fc
}

/** Одна строка плана-графика из GET /api/v1/routes/{tr}/stops (schedule.Stop в Go). */
export interface PlanStop {
  action_id: number
  tr_id: number
  time_begin: string
  lon: number
  lat: number
  address: string
}

/**
 * routesFromPlan — live-карта: список остановок каждого TRID от gateway
 * превращается в тот же RouteFC, что replay строит из routes.geojson
 * (polyline на машину + точки остановок). Панели о источнике не знают.
 * Остановки внутри tr уже отсортированы по времени (индекс в schedule).
 */
export function routesFromPlan(lists: PlanStop[][]): RouteFC {
  const features: RouteFC['features'] = []
  const seen = new Set<number>()
  for (const stops of lists) {
    const first = stops[0]
    if (!first) continue
    const route = String(first.tr_id)
    features.push({
      type: 'Feature',
      properties: { kind: 'route', tr_id: first.tr_id, route, stops: stops.length },
      geometry: { type: 'LineString', coordinates: stops.map(s => [s.lon, s.lat]) },
    })
    for (const s of stops) {
      // action_id — он же target_stop_id в прогнозе: панель инцидентов
      // ищет имя остановки по этому ключу, как и в replay по stop_id
      if (!s.action_id || seen.has(s.action_id)) continue
      seen.add(s.action_id)
      features.push({
        type: 'Feature',
        properties: { kind: 'stop', stop_id: s.action_id, route, name: s.address },
        geometry: { type: 'Point', coordinates: [s.lon, s.lat] },
      })
    }
  }
  return { type: 'FeatureCollection', features }
}

export function routesBounds(fc: RouteFC | null): [[number, number], [number, number]] | null {
  if (!fc) return null
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
  const eat = (c: unknown) => {
    if (Array.isArray(c)) {
      if (typeof c[0] === 'number' && typeof c[1] === 'number') {
        minX = Math.min(minX, c[0]); maxX = Math.max(maxX, c[0])
        minY = Math.min(minY, c[1]); maxY = Math.max(maxY, c[1])
      } else for (const p of c) eat(p)
    }
  }
  for (const f of fc.features) eat(f.geometry.coordinates)
  return Number.isFinite(minX) ? [[minX, minY], [maxX, maxY]] : null
}

/** Полилиния маршрута для нарезки сегментов риска. */
export interface RouteGeom {
  tr_id: number
  route: string
  coords: [number, number][]
}

export function routeGeometries(fc: RouteFC | null): RouteGeom[] {
  const out: RouteGeom[] = []
  for (const f of fc?.features ?? []) {
    if (f.properties.kind !== 'route' || f.geometry.type !== 'LineString') continue
    const coords = f.geometry.coordinates as [number, number][]
    if (coords.length >= 2) {
      out.push({ tr_id: Number(f.properties.tr_id ?? 0), route: String(f.properties.route ?? ''), coords })
    }
  }
  return out
}

const M_PER_DEG = 111320

/**
 * Ближайшее звено полилинии к точке: индекс и расстояние в метрах.
 * Плоская аппроксимация (градусы широты/долготы × масштаб) — городскому
 * масштабу точности «на каком сегменте едет ТС» хватает с запасом; для
 * длин маршрутов она не используется.
 */
export function nearestSegment(
  coords: [number, number][], lon: number, lat: number,
): { idx: number; dist_m: number } {
  const cos = Math.cos((lat * Math.PI) / 180)
  const px = lon * M_PER_DEG * cos
  const py = lat * M_PER_DEG
  let best = { idx: 0, dist_m: Infinity }
  for (let i = 0; i + 1 < coords.length; i++) {
    const ax = coords[i][0] * M_PER_DEG * cos, ay = coords[i][1] * M_PER_DEG
    const bx = coords[i + 1][0] * M_PER_DEG * cos, by = coords[i + 1][1] * M_PER_DEG
    const dx = bx - ax, dy = by - ay
    const l2 = dx * dx + dy * dy
    // t прижимаем к отрезку: за пределами звена расстояние считаем до конца
    const t = l2 > 0 ? Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / l2)) : 0
    const d = Math.hypot(ax + t * dx - px, ay + t * dy - py)
    if (d < best.dist_m) best = { idx: i, dist_m: d }
  }
  return best
}
