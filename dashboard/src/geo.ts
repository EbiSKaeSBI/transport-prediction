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

/** Границы в порядке maplibre: [[запад, юг], [восток, север]]. */
export type Bounds = [[number, number], [number, number]]

/** Прямоугольник, вмещающий оба источника; null, если оба пусты. */
export function mergeBounds(a: Bounds | null, b: Bounds | null): Bounds | null {
  if (!a) return b
  if (!b) return a
  return [
    [Math.min(a[0][0], b[0][0]), Math.min(a[0][1], b[0][1])],
    [Math.max(a[1][0], b[1][0]), Math.max(a[1][1], b[1][1])],
  ]
}

/** Границы точек машин; с запасом, чтобы маркер не липнул к краю холста. */
export function pointsBounds(
  pts: Iterable<{ lon: number; lat: number }>,
  eps = 0.002,
): Bounds | null {
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
  for (const p of pts) {
    if (!Number.isFinite(p.lon) || !Number.isFinite(p.lat)) continue
    minX = Math.min(minX, p.lon); maxX = Math.max(maxX, p.lon)
    minY = Math.min(minY, p.lat); maxY = Math.max(maxY, p.lat)
  }
  if (!Number.isFinite(minX)) return null
  return [[minX - eps, minY - eps], [maxX + eps, maxY + eps]]
}

/**
 * «Круглый» шаг сетки, дающий примерно target линий на сторону: 0.05, 0.1,
 * 0.25, 0.5, 1, 2, 5 … Вместо произвольных долей градуса подписи сетки
 * читаются глазами, а не выглядят как шум.
 */
export function niceStep(span: number, target = 6): number {
  if (!(span > 0) || !Number.isFinite(span)) return 0.1
  const raw = span / target
  const mag = 10 ** Math.floor(Math.log10(raw))
  const norm = raw / mag
  return (norm >= 5 ? 10 : norm >= 2 ? 5 : norm >= 1 ? 2 : 1) * mag
}

/**
 * Сетка координат по границам — офлайн-аналог подложки. Без тайлов демо
 * выглядит как карта, а не как пустой фон (docs/architecture.md §6: сети нет
 * и быть не должно, а ощущение масштаба и ориентации нужно).
 */
export function graticuleFC(b: Bounds | null, step: number): RouteFC {
  const empty: RouteFC = { type: 'FeatureCollection', features: [] }
  if (!b || !(step > 0) || !Number.isFinite(step)) return empty
  const [[w, s], [e, n]] = b
  const lines: RouteFC['features'] = []
  const seg = (a: [number, number], c: [number, number]) => {
    if (lines.length >= 200) return
    lines.push({
      type: 'Feature',
      properties: { kind: 'grid' },
      geometry: { type: 'LineString', coordinates: [a, c] },
    })
  }
  for (let lon = Math.ceil(w / step) * step; lon <= e; lon += step) {
    seg([lon, s], [lon, n])
  }
  for (let lat = Math.ceil(s / step) * step; lat <= n; lat += step) {
    seg([w, lat], [e, lat])
  }
  return { type: 'FeatureCollection', features: lines }
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

/**
 * Пройденная часть полилинии: от начала маршрута до проекции точки ТС на
 * ближайшее звено. Метрика плоская, как в nearestSegment, а она линейна по
 * градусам (cos широты — константа в пределах маршрута), поэтому параметр
 * t из метрического проекта переносит точку на отрезке в градусы без пересчёта.
 * null — точка дальше звена, чем tol_m: машина не на этом маршруте, и
 * «пройденного» по нему показывать нельзя.
 */
export function traveledPath(
  coords: [number, number][], lon: number, lat: number, tol_m: number,
): [number, number][] | null {
  if (coords.length < 2) return null
  const { idx, dist_m } = nearestSegment(coords, lon, lat)
  if (dist_m > tol_m) return null
  const [ax, ay] = coords[idx]
  const [bx, by] = coords[idx + 1]
  const cos = Math.cos((lat * Math.PI) / 180)
  const m = M_PER_DEG * cos
  const px = lon * m, py = lat * M_PER_DEG
  const axm = ax * m, aym = ay * M_PER_DEG
  const dx = bx * m - axm, dy = by * M_PER_DEG - aym
  const l2 = dx * dx + dy * dy
  const t = l2 > 0 ? Math.max(0, Math.min(1, ((px - axm) * dx + (py - aym) * dy) / l2)) : 0
  const proj: [number, number] = [ax + (bx - ax) * t, ay + (by - ay) * t]
  return [...coords.slice(0, idx + 1), proj]
}
