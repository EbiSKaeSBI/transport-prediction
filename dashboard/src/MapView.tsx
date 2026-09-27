import { useEffect, useRef } from 'react'
// первым импортом — модуль с URL воркера: без него карта не поднимется ни в
// dev, ни в собранном dist (см. ./maplibregl.ts)
import './maplibregl'
import { AttributionControl, Map, Marker, NavigationControl } from 'maplibre-gl'
import type { GeoJSONSource, ExpressionSpecification, StyleSpecification } from 'maplibre-gl'
import 'maplibre-gl/dist/maplibre-gl.css'
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
        speed: v.speed,
        heading: v.heading,
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

/**
 * Единственная внешняя подложка, которую дашборд знает: свой style.json через
 * VITE_MAP_STYLE. Пусто (по умолчанию), недоступно или не отдаёт 200 —
 * остаёмся на офлайн-фоне: тёмный background + сетка координат
 * (docs/architecture.md §6: демо работает без сети и без сторонних тайловых
 * сервисов). Проба одним запросом до подстановки стиля: у maplibre нет
 * асинхронного фолбэка, а setStyle на несуществующий URL оставляет карту
 * навсегда пустой.
 */
async function probeOnlineStyle(): Promise<string | null> {
  const url = import.meta.env.VITE_MAP_STYLE
  if (!url) return null
  try {
    const res = await fetch(url)
    return res.ok ? url : null
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
  /**
   * Движок карты жив, но бесполезен: стиль так и не загрузился (заблокирован
   * Web Worker maplibre — типично для жёсткой CSP или урезанных браузеров).
   * Без этого сигнала дашборд стоит с чёрным холстом: ни маршрутной сети, ни
   * машин — MapBoundary ловит только исключения, а зависший воркер не бросает
   * ничего. App переключается на CanvasMap.
   */
  onEngineFail?: (reason: string) => void
}

export default function MapView({ store, routes, selected, onSelect, onEngineFail }: Props) {
  const holder = useRef<HTMLDivElement | null>(null)
  const selectedRef = useRef<number | null>(selected)
  const onSelectRef = useRef(onSelect)
  const onEngineFailRef = useRef(onEngineFail)
  useEffect(() => { selectedRef.current = selected }, [selected])
  useEffect(() => { onSelectRef.current = onSelect }, [onSelect])
  useEffect(() => { onEngineFailRef.current = onEngineFail }, [onEngineFail])

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

    // Watchdog движка: maplibre разбирает стиль в Web Worker. Если воркер
    // заблокирован (жёсткая CSP, урезанный браузер), стиль не приходит НИКОГДА
    // — без исключения и без события error: висит чёрный холст без маршрутной
    // сети и машин. Через 6 с считаем движок мёртвым и отдаём карту
    // canvas-фолбэку, который рисует то же самое без воркеров и без GPU.
    let styleOk = false
    map.on('load', () => { styleOk = true })
    const watchdog = window.setTimeout(() => {
      if (!disposed && !styleOk && !map.isStyleLoaded()) {
        onEngineFailRef.current?.('MapLibre: стиль карты не загрузился (воркер карты не отвечает)')
      }
    }, 6000)
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
    // Два момента, из-за которых сетка раньше «пропадала при перемещении»:
    // 1) генерировать нужно не впритык по viewport, а с запасом ~1.5× по
    //    обеим осям: при drag мы выезжаем за старый прямоугольник раньше,
    //    чем приходит moveend, и за краем линий уже нет;
    // 2) обновляться надо на 'move' (во время жеста), а не только на
    //    'moveend' — иначе середина перетаскивания всегда показывает пустоту.
    // Обновление дешёвое: 10–30 коротких линий на кадр, коалесится maplibre.
    const updateGrid = () => {
      const src = map.getSource('ndtp-grid') as GeoJSONSource | undefined
      if (!src) return
      const b = map.getBounds()
      if (!b) return
      const [[west, south], [east, north]] = [
        [b.getWest(), b.getSouth()], [b.getEast(), b.getNorth()],
      ] as Bounds
      const midLat = (south + north) / 2
      // шаг считаем по видимому экрану (не по раздутому прямоугольнику),
      // иначе сетка на pad'е стала бы вдвое реже; раздутым box'ом только
      // покрываем область генерации
      const span = Math.max(
        east - west,
        (north - south) * Math.cos((midLat * Math.PI) / 180),
      )
      const padX = (east - west) * 0.6
      const padY = (north - south) * 0.6
      const box: Bounds = [[west - padX, south - padY], [east + padX, north + padY]]
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
    // 'move', а не 'moveend': сетка догоняет камеру во время жеста, а не
    // после — иначе всё время перетаскивания за старым прямоугольником
    // линии уже не рисуются (см. комментарий в updateGrid)
    map.on('move', updateGrid)

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

    // Текущее положение ТС поверх маршрутной сети: стрелка направления
    // движения + бирка «№ · скорость». DOM-маркеры, а не symbol-слой: для
    // текста в слоях maplibre нужен сетевой glyphs-сервер, а демо обязано
    // работают офлайн. pointer-events: none в CSS — клик и hover остаются на
    // circle-слое точек под маркером (он же рисует риск-цвет и кольцо
    // выделения). Map тут занят классом maplibre — глобальный берём явно.
    const heads = new globalThis.Map<number, { m: Marker; dir: HTMLDivElement; tag: HTMLSpanElement }>()
    const drawHeads = (vehicles: Iterable<VehicleState>) => {
      const seen = new globalThis.Set<number>()
      for (const v of vehicles) {
        seen.add(v.tr_id)
        let rec = heads.get(v.tr_id)
        if (!rec) {
          const el = document.createElement('div')
          el.className = 'veh-head'
          const dir = document.createElement('div')
          dir.className = 'veh-dir'
          const tag = document.createElement('span')
          tag.className = 'veh-tag'
          el.append(dir, tag)
          const m = new Marker({ element: el, anchor: 'center' }).addTo(map)
          rec = { m, dir, tag }
          heads.set(v.tr_id, rec)
        }
        rec.m.setLngLat([v.lon, v.lat])
        rec.dir.style.transform = `rotate(${Number.isFinite(v.heading) ? v.heading : 0}deg)`
        rec.dir.style.borderBottomColor = RISK_COLORS[vehicleRisk(v, store.clock)]
        rec.tag.textContent = `ТС ${v.tr_id} · ${Math.round(v.speed)} км/ч`
        rec.tag.classList.toggle('sel', v.tr_id === selectedRef.current)
      }
      for (const [id, rec] of heads) {
        if (!seen.has(id)) { heads.delete(id); rec.m.remove() }
      }
    }

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
        // маркеры живут в DOM и не зависят от стиля — обновляем всегда:
        // так стрелки и бирки видны даже пока внешняя подложка догружается
        drawHeads(store.vehicles.values())
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
      // атрибуция нужна только внешней подложке (требование её тайлового
      // сервиса); на офлайн-сетке показывать нечего
      map.addControl(new AttributionControl({ compact: true }), 'bottom-right')
    })

    return () => {
      disposed = true
      window.clearTimeout(watchdog)
      if (raf) cancelAnimationFrame(raf)
      for (const [, rec] of heads) rec.m.remove()
      heads.clear()
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
