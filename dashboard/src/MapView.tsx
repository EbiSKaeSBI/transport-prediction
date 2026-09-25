import { useEffect, useRef } from 'react'
import { Map as MLMap, NavigationControl } from 'maplibre-gl'
import type { GeoJSONSource, ExpressionSpecification } from 'maplibre-gl'
import 'maplibre-gl/dist/maplibre-gl.css'
import type { Store } from './store'
import type { RouteFC } from './geo'
import { routesBounds } from './geo'
import { RISK_COLORS, vehicleRisk } from './risk'
import type { VehicleState } from './types'

function vehiclesFC(vehicles: Iterable<VehicleState>, clock: number, selected: number | null) {
  return {
    type: 'FeatureCollection' as const,
    features: Array.from(vehicles, (v) => ({
      type: 'Feature' as const,
      properties: {
        tr_id: v.tr_id,
        risk: vehicleRisk(v, clock),
        selected: v.tr_id === selected ? 1 : 0,
      },
      geometry: { type: 'Point' as const, coordinates: [v.lon, v.lat] },
    })),
  }
}

const riskColorExpr: ExpressionSpecification = [
  'match', ['get', 'risk'],
  'green', RISK_COLORS.green,
  'yellow', RISK_COLORS.yellow,
  'red', RISK_COLORS.red,
  'stale', RISK_COLORS.stale,
  RISK_COLORS.unknown,
]

interface Props {
  store: Store
  routes: RouteFC | null
  selected: number | null
  onSelect: (trId: number | null) => void
}

export default function MapView({ store, routes, selected, onSelect }: Props) {
  const holder = useRef<HTMLDivElement | null>(null)
  const selectedRef = useRef<number | null>(selected)
  const onSelectRef = useRef(onSelect)
  useEffect(() => { selectedRef.current = selected }, [selected])
  useEffect(() => { onSelectRef.current = onSelect }, [onSelect])

  useEffect(() => {
    if (!holder.current) return
    const map = new MLMap({
      container: holder.current,
      attributionControl: false,
      // вендоренный минимальный стиль: фон + наши слои, ноль сетевых
      // запросов к тайлам — демо работает офлайн (docs/architecture.md §6)
      style: {
        version: 8,
        sources: {},
        layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#101418' } }],
      },
    })
    map.addControl(new NavigationControl({ showCompass: false }), 'top-left')

    map.on('load', () => {
      map.addSource('routes', {
        type: 'geojson',
        data: (routes ?? { type: 'FeatureCollection', features: [] }) as never,
      })
      map.addSource('vehicles', { type: 'geojson', data: vehiclesFC([], 0, null) as never })
      map.addLayer({
        id: 'routes-line', type: 'line', source: 'routes',
        filter: ['==', ['get', 'kind'], 'route'],
        paint: { 'line-color': ['get', 'color'] as never, 'line-width': 2, 'line-opacity': 0.8 },
      } as never)
      map.addLayer({
        id: 'stops-dot', type: 'circle', source: 'routes',
        filter: ['==', ['get', 'kind'], 'stop'],
        paint: { 'circle-radius': 2, 'circle-color': '#5d6a75' },
      })
      map.addLayer({
        id: 'vehicles-halo', type: 'circle', source: 'vehicles',
        paint: {
          'circle-radius': ['case', ['==', ['get', 'selected'], 1], 11, 0] as never,
          'circle-color': 'rgba(0,0,0,0)',
          'circle-stroke-color': '#ffffff', 'circle-stroke-width': 2,
        },
      })
      map.addLayer({
        id: 'vehicles-dot', type: 'circle', source: 'vehicles',
        paint: {
          'circle-radius': 6, 'circle-color': riskColorExpr as never,
          'circle-stroke-color': '#0b0e11', 'circle-stroke-width': 1.5,
        },
      })
      const bounds = routesBounds(routes)
      if (bounds) map.fitBounds(bounds, { padding: 40 })
    })

    map.on('click', 'vehicles-dot', (e) => {
      const f = e.features?.[0]
      if (f) {
        const props = f.properties as unknown as Record<string, unknown>
        onSelectRef.current(Number(props.tr_id))
      }
    })
    map.on('mousemove', 'vehicles-dot', () => {
      map.getCanvas().style.cursor = 'pointer'
    })
    map.on('mouseleave', 'vehicles-dot', () => {
      map.getCanvas().style.cursor = ''
    })

    let raf = 0
    const redraw = () => {
      if (raf) return
      raf = requestAnimationFrame(() => {
        raf = 0
        const src = map.getSource('vehicles') as GeoJSONSource | undefined
        src?.setData(vehiclesFC(store.vehicles.values(), store.clock, selectedRef.current) as never)
      })
    }
    redrawRef.current = redraw
    const unsub = store.subscribe(redraw)
    redraw()

    return () => {
      if (raf) cancelAnimationFrame(raf)
      if (redrawRef.current === redraw) redrawRef.current = () => {}
      unsub()
      map.remove()
    }
  }, [store, routes])

  // смена выделения на паузе реплея: событий нет — перерисовать вручную
  const redrawRef = useRef<() => void>(() => {})
  useEffect(() => { redrawRef.current() }, [selected])

  return <div ref={holder} className="map-holder" />
}
