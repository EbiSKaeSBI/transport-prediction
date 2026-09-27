import { useEffect, useRef, useState } from 'react'
// первым импортом — модуль с URL воркера: без него карта не поднимется ни в
// dev, ни в собранном dist (см. ./maplibregl.ts)
import './maplibregl'
import { AttributionControl, Map, NavigationControl } from 'maplibre-gl'
import type { GeoJSONSource, ExpressionSpecification, StyleSpecification } from 'maplibre-gl'
import 'maplibre-gl/dist/maplibre-gl.css'
import YMapView from './YMapView'
import { loadYmaps, ymapsApiKey, type YMaps3 } from './ymaps'
import type { Store } from './store'
import type { RouteFC, RouteGeom, Bounds } from './geo'
import { routesBounds, routeGeometries, mergeBounds, pointsBounds, graticuleFC, niceStep } from './geo'
import { RISK_COLORS, vehicleRisk, riskSegmentsFC, traveledRoutesFC } from './risk'
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

const EMPTY: RouteFC = { type: 'FeatureCollection', features: [] }

/**
 * Офлайн-стиль по умолчанию: только фон, ноль сетевых запросов за тайлами —
 * демо обязано работать без сети (docs/architecture.md §6).
 */
const OFFLINE_STYLE: StyleSpecification = {
  version: 8,
  sources: {},
  layers: [{ id: 'bg', type: 'background', paint: { 'background-color': '#101418' } }],
}

// Датасет и план-графики — Москва (37.6173, 55.7558): до прихода плана и
// телеметрии камера показывает город, а не «нулевой остров» у экватора.
const MOSCOW_CENTER: [number, number] = [37.6173, 55.7558]

// Настоящая подложка по умолчанию: растровые тайлы CARTO dark — совпадают с
// тёмной темой дашборда, данные © OpenStreetMap. Свой style.json (со своими
// тайлами и подписями) по-прежнему приоритетнее: VITE_MAP_STYLE ниже.
const CARTO_TILES = ['a', 'b', 'c', 'd'].map(
  (s) => `https://${s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png`,
)

function moscowBasemap(): StyleSpecification {
  return {
    version: 8,
    sources: {
      'ndtp-basemap': {
        type: 'raster',
        tiles: CARTO_TILES,
        tileSize: 256,
        maxzoom: 20,
        attribution: '© OpenStreetMap contributors © CARTO',
      },
    },
    layers: [
      // фон под тайлами: где тайл ещё не доехал — цвет дашборда, а не белый
      { id: 'bg', type: 'background', paint: { 'background-color': '#101418' } },
      { id: 'ndtp-basemap-tiles', type: 'raster', source: 'ndtp-basemap' },
    ],
  }
}

// Проба «есть ли сеть вообще» — один тайл над Москвой (z=11): недоступен —
// остаёмся на офлайн-стиле с сеткой координат.
const MOSCOW_TILE_PROBE = 'https://a.basemaps.cartocdn.com/dark_all/11/1238/640.png'

/**
 * VITE_MAP_STYLE — необязательная настоящая подложка: style.json с тайлами и
 * подписями. Пусто, недоступно или не отдаёт 200 — остаёмся на офлайн-стиле,
 * чтобы демо не падало в пустоту вместе с сетью. Проба одним запросом до
 * подстановки стиля: у maplibre нет асинхронного фолбэка, а setStyle на
 * несуществующий URL оставляет карту навсегда пустой.
 */
async function probeOnlineStyle(): Promise<string | StyleSpecification | null> {
  const url = import.meta.env.VITE_MAP_STYLE
  if (url) {
    try {
      const res = await fetch(url)
      return res.ok ? url : null
    } catch {
      return null
    }
  }
  // Явного стиля нет — проба одного тайла решает «есть ли сеть вообще»:
  // недоступен — остаёмся на офлайн-сетке (docs/architecture.md §6).
  try {
    const res = await fetch(MOSCOW_TILE_PROBE)
    return res.ok ? moscowBasemap() : null
  } catch {
    return null
  }
}

// Идентификаторы с префиксом: подложка извне может принести свои слои с
// любыми именами, и install() не должен сносить чужие.
const SOURCES = ['ndtp-grid', 'ndtp-routes', 'ndtp-vehicles', 'ndtp-risk-segs', 'ndtp-traveled'] as const
const LAYERS = [
  'ndtp-grid-line', 'ndtp-routes-line', 'ndtp-traveled-line', 'ndtp-risk-seg-line',
  'ndtp-stops-dot', 'ndtp-vehicles-halo', 'ndtp-vehicles-dot',
] as const

interface Props {
  store: Store
  routes: RouteFC | null
  selected: number | null
  onSelect: (trId: number | null) => void
}

function MapLibreView({ store, routes, selected, onSelect }: Props) {
  const holder = useRef<HTMLDivElement | null>(null)
  const selectedRef = useRef<number | null>(selected)
  const onSelectRef = useRef(onSelect)
  useEffect(() => { selectedRef.current = selected }, [selected])
  useEffect(() => { onSelectRef.current = onSelect }, [onSelect])

  useEffect(() => {
    if (!holder.current) return
    const map = new Map({
      container: holder.current,
      attributionControl: false,
      style: OFFLINE_STYLE,
      center: MOSCOW_CENTER,
      zoom: 11,
    })
    map.addControl(new NavigationControl({ showCompass: false }), 'top-left')
    // dev-only хук: браузерные проверки и отладка читают состояние слоёв
    // (const в прод-сборке вырезается вместе с условием import.meta.env.DEV)
    if (import.meta.env.DEV) (window as unknown as { __ndtpMap?: Map }).__ndtpMap = map

    let geoms: RouteGeom[] = []
    let disposed = false
    // Настоящая подложка (город, улицы, подписи) активна — сетка координат
    // тогда лишняя: она была офлайн-заменителем карты, а не декорацией поверх.
    let baseOnline = false

    // Рамка — один раз, когда известны и план, и позиция ТС. План без машин
    // игнорируем: план демо-фида лежит в 8 км от телеметрии, рамка только по
    // нему уводит машину за край и карта выглядит пустой. Дальше оператор
    // рулит зумом сам.
    let fitted = false
    const fitOnce = () => {
      if (fitted) return
      const pts = pointsBounds(store.vehicles.values())
      const plan = routesBounds(routes)
      if (plan && !pts) return
      const b = mergeBounds(plan, pts)
      if (!b) return
      fitted = true
      map.fitBounds(b, { padding: 40, maxZoom: 14 })
      updateGrid()
    }

    // Сетка координат пересчитывается под текущий кадр: при зуме оператора
    // шаг меняется, иначе либо лишние линии, либо пустота.
    const updateGrid = () => {
      const src = map.getSource('ndtp-grid') as GeoJSONSource | undefined
      if (!src) return
      const b = map.getBounds()
      if (!b) return
      const box: Bounds = [[b.getWest(), b.getSouth()], [b.getEast(), b.getNorth()]]
      const midLat = (box[0][1] + box[1][1]) / 2
      const span = Math.max(
        box[1][0] - box[0][0],
        (box[1][1] - box[0][1]) * Math.cos((midLat * Math.PI) / 180),
      )
      src.setData(graticuleFC(box, niceStep(span)) as never)
    }

    const install = () => {
      geoms = routeGeometries(routes)
      for (const id of LAYERS) if (map.getLayer(id)) map.removeLayer(id)
      for (const id of SOURCES) if (map.getSource(id)) map.removeSource(id)
      map.addSource('ndtp-routes', { type: 'geojson', data: (routes ?? EMPTY) as never })
      map.addSource('ndtp-vehicles', { type: 'geojson', data: vehiclesFC([], 0, null) as never })
      map.addSource('ndtp-risk-segs', { type: 'geojson', data: riskSegmentsFC(geoms, [], 0) as never })
      map.addSource('ndtp-traveled', { type: 'geojson', data: traveledRoutesFC([], [], 0) as never })
      // сетка — под линиями плана: это фон, а не данные; с реальной подложкой
      // её не рисуем вовсе
      if (!baseOnline) {
        map.addSource('ndtp-grid', { type: 'geojson', data: EMPTY as never })
        map.addLayer({
          id: 'ndtp-grid-line', type: 'line', source: 'ndtp-grid',
          paint: { 'line-color': '#1b242c', 'line-width': 1 },
        } as never)
      }
      map.addLayer({
        id: 'ndtp-routes-line', type: 'line', source: 'ndtp-routes',
        filter: ['==', ['get', 'kind'], 'route'],
        paint: { 'line-color': ['get', 'color'] as never, 'line-width': 2, 'line-opacity': 0.8 },
      } as never)
      // пройденная часть маршрута: тон вдоль плана за машиной, в цвете её
      // риска — при опасности хвост желтеет/краснеет целиком (риск.ts)
      map.addLayer({
        id: 'ndtp-traveled-line', type: 'line', source: 'ndtp-traveled',
        layout: { 'line-cap': 'round', 'line-join': 'round' } as never,
        paint: {
          'line-color': riskColorExpr as never,
          'line-width': 3.5, 'line-opacity': 0.85,
        },
      } as never)
      // участки риска поверх линий плана, под точками остановок: диспетчеру
      // важно, какой участок маршрута горит, а не только где машина.
      // line-cap — свойство layout, не paint: в paint оно невалидно, и MapLibre
      // отказывается добавлять весь слой (ошибка при install, до setData).
      map.addLayer({
        id: 'ndtp-risk-seg-line', type: 'line', source: 'ndtp-risk-segs',
        layout: { 'line-cap': 'round' } as never,
        paint: { 'line-color': riskColorExpr as never, 'line-width': 5, 'line-opacity': 0.75 },
      } as never)
      map.addLayer({
        id: 'ndtp-stops-dot', type: 'circle', source: 'ndtp-routes',
        filter: ['==', ['get', 'kind'], 'stop'],
        paint: { 'circle-radius': 2, 'circle-color': '#5d6a75' },
      })
      map.addLayer({
        id: 'ndtp-vehicles-halo', type: 'circle', source: 'ndtp-vehicles',
        paint: {
          'circle-radius': ['case', ['==', ['get', 'selected'], 1], 11, 0] as never,
          'circle-color': 'rgba(0,0,0,0)',
          'circle-stroke-color': '#ffffff', 'circle-stroke-width': 2,
        },
      })
      map.addLayer({
        id: 'ndtp-vehicles-dot', type: 'circle', source: 'ndtp-vehicles',
        paint: {
          'circle-radius': 6, 'circle-color': riskColorExpr as never,
          'circle-stroke-color': '#0b0e11', 'circle-stroke-width': 1.5,
        },
      })
      updateGrid()
    }

    // style.load, а не только load: подстановка внешней подложки перезагружает
    // стиль и сносит наши слои — install() их возвращает.
    map.on('style.load', install)
    map.on('moveend', updateGrid)

    map.on('click', 'ndtp-vehicles-dot', (e) => {
      const f = e.features?.[0]
      if (f) {
        const props = f.properties as unknown as Record<string, unknown>
        onSelectRef.current(Number(props.tr_id))
      }
    })
    map.on('mousemove', 'ndtp-vehicles-dot', () => {
      map.getCanvas().style.cursor = 'pointer'
    })
    map.on('mouseleave', 'ndtp-vehicles-dot', () => {
      map.getCanvas().style.cursor = ''
    })

    let raf = 0
    const redraw = () => {
      if (raf) return
      raf = requestAnimationFrame(() => {
        raf = 0
        const src = map.getSource('ndtp-vehicles') as GeoJSONSource | undefined
        src?.setData(vehiclesFC(store.vehicles.values(), store.clock, selectedRef.current) as never)
        const segs = map.getSource('ndtp-risk-segs') as GeoJSONSource | undefined
        segs?.setData(riskSegmentsFC(geoms, store.vehicles.values(), store.clock) as never)
        const traveled = map.getSource('ndtp-traveled') as GeoJSONSource | undefined
        traveled?.setData(traveledRoutesFC(geoms, store.vehicles.values(), store.clock) as never)
        fitOnce() // позиция могла приехать позже, чем стиль прогрузился
      })
    }
    redrawRef.current = redraw
    const unsub = store.subscribe(redraw)
    redraw()

    void probeOnlineStyle().then((style) => {
      if (!style || disposed) return
      // сначала флаг: style.load сработает синхронно из setStyle, install()
      // обязан увидеть онлайн-подложку и не рисовать сетку
      baseOnline = true
      map.setStyle(style)
      // атрибуция нужна только настоящей подложке (требование OSM/CARTO);
      // на офлайн-сетке показывать нечего
      map.addControl(new AttributionControl({ compact: true }), 'bottom-right')
    })

    return () => {
      disposed = true
      if (raf) cancelAnimationFrame(raf)
      if (redrawRef.current === redraw) redrawRef.current = () => {}
      unsub()
      if (import.meta.env.DEV && (window as unknown as { __ndtpMap?: Map }).__ndtpMap === map) {
        delete (window as unknown as { __ndtpMap?: Map }).__ndtpMap
      }
      map.remove()
    }
  }, [store, routes])

  // смена выделения на паузе реплея: событий нет — перерисовать вручную
  const redrawRef = useRef<() => void>(() => {})
  useEffect(() => { redrawRef.current() }, [selected])

  return <div ref={holder} className="map-holder" />
}

/**
 * Точка входа карты: при валидном VITE_YANDEX_MAPS_API_KEY — Яндекс JS API
 * (тёмная схема с кастомизацией, §docs customization), иначе — офлайн-движок
 * MapLibre. Загрузка Яндекса асинхронная: битый ключ, отсутствие сети или
 * неподтверждённый реферер не роняют дашборд — тихо откатываемся на MapLibre
 * (одно warning в консоль) и живём с подложкой/сеткой как раньше.
 */
export default function MapView(props: Props) {
  const [engine, setEngine] = useState<'pending' | 'yandex' | 'libre'>(
    () => (ymapsApiKey() ? 'pending' : 'libre'),
  )
  const [api, setApi] = useState<YMaps3 | null>(null)
  useEffect(() => {
    if (engine !== 'pending') return
    let alive = true
    loadYmaps().then(
      (a) => { if (alive) { setApi(a); setEngine('yandex') } },
      (err: Error) => {
        if (!alive) return
        console.warn(`[ndtp] ${err.message}; карта на офлайн-движке MapLibre`)
        setEngine('libre')
      },
    )
    return () => { alive = false }
  }, [engine])
  if (engine === 'yandex' && api) return <YMapView api={api} {...props} />
  if (engine === 'pending') return <div className="map-holder" />
  return <MapLibreView {...props} />
}
