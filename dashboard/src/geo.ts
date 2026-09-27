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

// Палитра только из светлых тонов: линии рисуются поверх тёмного офлайн-фона
// (#101418) с line-opacity 0.8, и тёмные цвета (#000075, #800000 из старого
// набора) на нём исчезают — маршрут остаётся пунктиром точек останов, а
// прижатая к линии машина выглядит «сошедшей с путей».
const ROUTE_PALETTE = [
  '#e6194b', '#3cb44b', '#4363d8', '#f58231', '#911eb4', '#46f0f0',
  '#f032e6', '#bcf60c', '#fabebe', '#00ced1', '#e6beff', '#ff6f61',
  '#7b68ee',
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
 * PLAN_WINDOW_PAST_S / PLAN_WINDOW_FUTURE_S — отображаемое окно плана:
 * −20 мин назад (окно фактических наблюдений генератора плана) и +15 мин
 * вперёд (горизонт прогноза по ТЗ). live-план синтезируется из записи, и
 * всё, что дальше головы записи, — экстраполяция по касательной: она
 * перерисовывается на каждом replan и каждый раз иначе. Резать её глазам —
 * не прятать ошибку, а показывать маршрутом только то, что ещё можно
 * назвать маршрутом: проеханный след и горизонт предсказания.
 */
export const PLAN_WINDOW_PAST_S = 20 * 60
export const PLAN_WINDOW_FUTURE_S = 15 * 60

/** Остановки плана, чьё время в окне [now−PAST, now+FUTURE]. Окно полупустое — оставляет как есть. */
export function trimPlanWindow(stops: PlanStop[], nowMs = Date.now()): PlanStop[] {
  const from = nowMs / 1000 - PLAN_WINDOW_PAST_S
  const to = nowMs / 1000 + PLAN_WINDOW_FUTURE_S
  const inw = stops.filter(s => {
    const t = Date.parse(s.time_begin) / 1000
    return Number.isFinite(t) && t >= from && t <= to
  })
  return inw.length >= 2 ? inw : stops
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
    // Линия имеет смысл от двух точек; одна остановка — только точка.
    if (stops.length >= 2) {
      features.push({
        type: 'Feature',
        properties: { kind: 'route', tr_id: first.tr_id, route, stops: stops.length },
        geometry: { type: 'LineString', coordinates: stops.map(s => [s.lon, s.lat]) },
      })
    }
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
 * SNAP_MAX_DEVIATION_M — предел прижимания точки к маршруту. Зеркалит
 * mapmatch.DefaultSearchRadiusM (backend/internal/mapmatch/mapmatch.go):
 * дальше 400 м начинается осмысленное «вне маршрута» (депо, другой участок —
 * 12 % точек validate лежат за ним), и на карте это надо видеть, а не прятать
 * за проекцией. Расхождение порога с бэкендом означало бы, что дашборд
 * прижимает к плану то, что признаки честно помечают как off-route.
 */
export const SNAP_MAX_DEVIATION_M = 400

export interface SnapResult {
  lon: number
  lat: number
  /** true — точка прижата к полилинии; false — лежала за порогом или полилинии нет. */
  snapped: boolean
}

/**
 * Дисплейная позиция машины: сырые координаты прижаты к ближайшему звену
 * полилинии её собственного маршрута (tr_id), если расстояние не больше
 * порога. Прижимание — только отрисовка: store и API сохраняют телеметрию
 * как есть. Без него GPS-шум (медиана 8–32 м от хорды остановок, см. док
 * пакета mapmatch) рисует зигзаг рядом с планом, и зрителю это читается как
 * «транспорт сошёл с маршрута» — ровно то впечатление, которое демо не
 * должно производить.
 */
export function snapVehicle(
  v: { tr_id: number; lon: number; lat: number },
  geoms: RouteGeom[],
  maxDevM = SNAP_MAX_DEVIATION_M,
): SnapResult {
  if (!Number.isFinite(v.lon) || !Number.isFinite(v.lat)) {
    return { lon: v.lon, lat: v.lat, snapped: false }
  }
  let best: { d: number; lon: number; lat: number } | null = null
  for (const g of geoms) {
    if (g.tr_id !== v.tr_id || g.coords.length < 2) continue
    const cos = Math.cos((v.lat * Math.PI) / 180)
    const px = v.lon * M_PER_DEG * cos
    const py = v.lat * M_PER_DEG
    for (let i = 0; i + 1 < g.coords.length; i++) {
      const ax = g.coords[i][0] * M_PER_DEG * cos, ay = g.coords[i][1] * M_PER_DEG
      const bx = g.coords[i + 1][0] * M_PER_DEG * cos, by = g.coords[i + 1][1] * M_PER_DEG
      const dx = bx - ax, dy = by - ay
      const l2 = dx * dx + dy * dy
      const t = l2 > 0 ? Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / l2)) : 0
      const qx = ax + t * dx, qy = ay + t * dy
      const d = Math.hypot(qx - px, qy - py)
      if (!best || d < best.d) best = { d, lon: qx / (M_PER_DEG * cos), lat: qy / M_PER_DEG }
    }
  }
  if (!best || best.d > maxDevM) return { lon: v.lon, lat: v.lat, snapped: false }
  return { lon: best.lon, lat: best.lat, snapped: true }
}
