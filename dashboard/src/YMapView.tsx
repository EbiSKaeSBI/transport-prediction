// Яндекс-движок карты (ymaps3): тёмная схема с кастомизацией + наши данные
// поверх через YMapFeature/YMapMarker. Данные считает тот же пайплайн, что и
// MapLibre-версия (geo.ts/risk.ts) — слои просто перекраиваются в императивные
// сущности. Включается только при валидном VITE_YANDEX_MAPS_API_KEY, иначе
// App остаётся на офлайн-MapLibre (MapView.tsx).
import { useEffect, useRef } from 'react'
import type { Store } from './store'
import type { RouteFC, RouteGeom } from './geo'
import { pointsBounds, routeGeometries, routesBounds, mergeBounds } from './geo'
import { RISK_COLORS, riskSegmentsFC, traveledRoutesFC, vehicleRisk } from './risk'
import type { Risk, VehicleState } from './types'
import type { YEntity, YMaps3 } from './ymaps'

interface Props {
  api: YMaps3
  store: Store
  routes: RouteFC | null
  selected: number | null
  onSelect: (trId: number | null) => void
}

// Датасет и план-графики — Москва: камера до приезда данных смотрит на город.
const MOSCOW_CENTER: [number, number] = [37.6173, 55.7558]

// Тёмная кастомизация по тегам из документации customization: подложку
// приглушаем, чтобы цвета риска остались самым ярким объектом на карте;
// подписи организаций и адресов прячем — их роль на себя берут панели.
const DARK_CUSTOMIZATION = [
  { tags: { all: ['land'] }, stylers: { color: '#14181e' } },
  { tags: { all: ['building'] }, stylers: { color: '#232b36' } },
  { tags: { all: ['road_surface'] }, stylers: { color: '#2d3744' } },
  { tags: { all: ['water'] }, stylers: { color: '#0e2233' } },
  { tags: { any: ['vegetation', 'park'] }, stylers: { color: '#15221a' } },
  { tags: { any: ['poi', 'address'] }, elements: 'label', stylers: { visibility: 'off' } },
]

const STOP_HALF_DEG = 1e-5 // «точки» остановок рисуем вырожденными звеньями

type Lines = [number, number][][]

function riskColor(risk: Risk): string {
  return RISK_COLORS[risk]
}

export default function YMapView({ api, store, routes, selected, onSelect }: Props) {
  const holder = useRef<HTMLDivElement | null>(null)
  const selectedRef = useRef<number | null>(selected)
  const onSelectRef = useRef(onSelect)
  useEffect(() => { selectedRef.current = selected }, [selected])
  useEffect(() => { onSelectRef.current = onSelect }, [onSelect])

  useEffect(() => {
    const el = holder.current
    if (!el) return
    const { YMap, YMapDefaultSchemeLayer, YMapDefaultFeaturesLayer, YMapFeature, YMapMarker } = api

    const map = new YMap(el, { location: { center: MOSCOW_CENTER, zoom: 11 }, theme: 'dark' })
    map.addChild(new YMapDefaultSchemeLayer({ customization: DARK_CUSTOMIZATION }))
    // слой фич: без него YMapFeature не рендерится (пример в документации)
    map.addChild(new YMapDefaultFeaturesLayer())
    // dev-only хук для браузерных проверок (вырезается из прод-сборки)
    if (import.meta.env.DEV) {
      (window as unknown as { __ndtpYMap?: unknown }).__ndtpYMap = { map, api }
    }

    const entities = new Map<string, YEntity>()
    const removeKey = (key: string) => {
      const e = entities.get(key)
      if (!e) return
      entities.delete(key)
      map.removeChild(e)
      e.destroy()
    }
    const upsertLine = (key: string, color: string, lines: Lines, width: number, opacity: number, zIndex: number) => {
      if (!lines.length) return removeKey(key)
      const props = {
        geometry: { type: 'MultiLineString', coordinates: lines },
        style: { stroke: [{ width, color, opacity }], interactive: false, zIndex },
      }
      const old = entities.get(key)
      if (old) old.update(props)
      else {
        const e = new YMapFeature(props)
        entities.set(key, e)
        map.addChild(e)
      }
    }

    // план: линии группами по цвету, остановки — одно MultiLineString из
    // вырожденных звеньев (точечный рендер YMapFeature по стилю не калиброван,
    // линия заданной толщины — гарантированная «точка»)
    const geoms: RouteGeom[] = routeGeometries(routes)
    const drawPlan = () => {
      const planLines = new Map<string, Lines>()
      const stopDots: Lines = []
      for (const f of routes?.features ?? []) {
        const p = f.properties as unknown as Record<string, unknown> | undefined
        if (p?.kind === 'route') {
          const color = String(p.color ?? '#7a8699')
          ;(planLines.get(color) ?? planLines.set(color, []).get(color)!).push(f.geometry.coordinates as Lines[0])
        } else if (p?.kind === 'stop') {
          const [lon, lat] = f.geometry.coordinates as [number, number]
          stopDots.push([[lon - STOP_HALF_DEG, lat], [lon + STOP_HALF_DEG, lat]])
        }
      }
      for (const key of [...entities.keys()]) {
        if (key.startsWith('plan:') || key === 'stops') removeKey(key)
      }
      for (const [color, lines] of planLines) upsertLine(`plan:${color}`, color, lines, 2, 0.8, 8)
      if (stopDots.length) upsertLine('stops', '#5d6a75', stopDots, 5, 1, 10)
    }
    drawPlan()

    // машины: DOM-маркеры (их единицы), клик по элементу — как в MapLibre-версии
    const cars = new Map<number, { marker: YEntity; div: HTMLDivElement }>()
    const drawCar = (v: VehicleState) => {
      let rec = cars.get(v.tr_id)
      if (!rec) {
        const div = document.createElement('div')
        div.style.cssText = 'width:14px;height:14px;border-radius:50%;border:2px solid #0b0e11;'
          + 'cursor:pointer;transform:translate(-50%,-50%)'
        div.addEventListener('click', () => onSelectRef.current?.(v.tr_id))
        const marker = new YMapMarker({ coordinates: [v.lon, v.lat], element: div })
        rec = { marker, div }
        cars.set(v.tr_id, rec)
        map.addChild(marker)
      }
      rec.marker.update({ coordinates: [v.lon, v.lat] })
      const risk = vehicleRisk(v, store.clock)
      rec.div.style.background = riskColor(risk)
      rec.div.style.boxShadow = v.tr_id === selectedRef.current
        ? '0 0 0 3px rgba(255,255,255,0.9)' : 'none'
    }

    // хвост пройденного и горящие звенья: группы по риску машины — при
    // опасности весь хвост борта перекрашивается целиком (риск.ts)
    const drawRisk = () => {
      const vehicles = [...store.vehicles.values()]
      const groups = {
        'traveled:': traveledRoutesFC(geoms, vehicles, store.clock),
        'seg:': riskSegmentsFC(geoms, vehicles, store.clock),
      }
      const live = new Set<string>()
      for (const [prefix, fc] of Object.entries(groups)) {
        const byRisk = new Map<Risk, Lines>()
        for (const f of fc.features) {
          const risk = (f.properties as { risk: Risk }).risk
          ;(byRisk.get(risk) ?? byRisk.set(risk, []).get(risk)!).push(f.geometry.coordinates as Lines[0])
        }
        const width = prefix === 'seg:' ? 5 : 3.5
        const opacity = prefix === 'seg:' ? 0.75 : 0.85
        const zIndex = prefix === 'seg:' ? 12 : 11
        for (const [risk, lines] of byRisk) {
          const key = prefix + risk
          live.add(key)
          upsertLine(key, riskColor(risk), lines, width, opacity, zIndex)
        }
        for (const key of [...entities.keys()]) {
          if (key.startsWith(prefix) && !live.has(key)) removeKey(key)
        }
      }
    }

    // рамка — один раз, когда есть и план, и позиции (та же логика, что в
    // MapView: план без машин игнорируем, дальше рулит оператор)
    let fitted = false
    const fitOnce = () => {
      if (fitted) return
      const pts = pointsBounds(store.vehicles.values())
      const plan = routesBounds(routes)
      if (plan && !pts) return
      const b = mergeBounds(plan, pts)
      if (!b) return
      fitted = true
      map.update({ location: { bounds: b } })
    }

    let raf = 0
    const draw = () => {
      drawRisk()
      const seen = new Set<number>()
      for (const v of store.vehicles.values()) {
        seen.add(v.tr_id)
        drawCar(v)
      }
      for (const [id, rec] of cars) {
        if (!seen.has(id)) {
          cars.delete(id)
          map.removeChild(rec.marker)
          rec.marker.destroy()
        }
      }
      fitOnce()
    }
    const redraw = () => {
      if (raf) return
      raf = requestAnimationFrame(() => { raf = 0; draw() })
    }
    redrawRef.current = redraw
    const unsub = store.subscribe(redraw)
    draw()

    return () => {
      unsub()
      if (redrawRef.current === redraw) redrawRef.current = () => {}
      if (raf) cancelAnimationFrame(raf)
      if (import.meta.env.DEV) {
        const w = window as unknown as { __ndtpYMap?: unknown }
        if (w.__ndtpYMap && (w.__ndtpYMap as { map: YEntity }).map === map) delete w.__ndtpYMap
      }
      for (const [, rec] of cars) rec.marker.destroy()
      cars.clear()
      for (const [, e] of entities) e.destroy()
      entities.clear()
      map.destroy()
    }
  }, [api, store, routes])

  // смена выделения на паузе реплея: событий нет — перерисовать вручную
  const redrawRef = useRef<() => void>(() => {})
  useEffect(() => { redrawRef.current() }, [selected])

  return <div ref={holder} className="map-holder" />
}
