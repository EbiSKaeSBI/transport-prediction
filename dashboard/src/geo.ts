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
