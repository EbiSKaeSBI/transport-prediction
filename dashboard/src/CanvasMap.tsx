import { useEffect, useRef } from 'react'
import type { Store } from './store'
import type { RouteFC } from './geo'
import { routeColor, routeGeometries } from './geo'
import { RISK_COLORS, vehicleRisk, riskSegmentsFC } from './risk'

interface Props {
  store: Store
  routes: RouteFC | null
  selected: number | null
  onSelect: (trId: number | null) => void
}

/**
 * CanvasMap — фолбэк без WebGL (docs/architecture.md §6, риск §9.3):
 * равносторонняя проекция по boundbox маршрутов, линии, остановки, ТС.
 * Медленнее MapLibre, но гарантированно рисует там, где браузеру
 * не дали GPU — демо не должно падать в чёрный экран.
 */
export default function CanvasMap({ store, routes, selected, onSelect }: Props) {
  const ref = useRef<HTMLCanvasElement | null>(null)
  const selectedRef = useRef<number | null>(selected)
  const onSelectRef = useRef(onSelect)
  const redrawRef = useRef<() => void>(() => {})
  useEffect(() => {
    selectedRef.current = selected
    redrawRef.current() // кольцо выделения на паузе реплея
  }, [selected])
  useEffect(() => { onSelectRef.current = onSelect }, [onSelect])

  useEffect(() => {
    const canvas = ref.current
    if (!canvas) return
    const ctx = canvas.getContext('2d')
    if (!ctx) return

    let w = 0, h = 0
    const resize = () => {
      const r = canvas.getBoundingClientRect()
      w = r.width; h = r.height
      const dpr = window.devicePixelRatio || 1
      canvas.width = w * dpr; canvas.height = h * dpr
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    }
    resize()
    window.addEventListener('resize', resize)

    let bounds = (() => {
      let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
      const eat = (c: unknown) => {
        if (Array.isArray(c)) {
          if (typeof c[0] === 'number' && typeof c[1] === 'number') {
            minX = Math.min(minX, c[0]); maxX = Math.max(maxX, c[0])
            minY = Math.min(minY, c[1]); maxY = Math.max(maxY, c[1])
          } else for (const p of c) eat(p)
        }
      }
      for (const f of routes?.features ?? []) eat(f.geometry.coordinates)
      if (!Number.isFinite(minX)) return null
      const padX = (maxX - minX) * 0.05, padY = (maxY - minY) * 0.05
      return { minX: minX - padX, maxX: maxX + padX, minY: minY - padY, maxY: maxY + padY }
    })()

    // live без плана (serve без --plan): рамка по первым точкам машин,
    // иначе все точки лепятся в центр холста и карта выглядит пустой
    const fitByVehicles = () => {
      if (bounds || routes) return
      let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
      for (const v of store.vehicles.values()) {
        minX = Math.min(minX, v.lon); maxX = Math.max(maxX, v.lon)
        minY = Math.min(minY, v.lat); maxY = Math.max(maxY, v.lat)
      }
      if (!Number.isFinite(minX)) return
      const eps = 0.002
      bounds = { minX: minX - eps, maxX: maxX + eps, minY: minY - eps, maxY: maxY + eps }
    }

    const project = (lon: number, lat: number): [number, number] => {
      if (!bounds) return [w / 2, h / 2]
      const x = (lon - bounds.minX) / (bounds.maxX - bounds.minX) * w
      const y = (1 - (lat - bounds.minY) / (bounds.maxY - bounds.minY)) * h
      return [x, y]
    }

    const draw = () => {
      fitByVehicles()
      ctx.fillStyle = '#101418'
      ctx.fillRect(0, 0, w, h)
      for (const f of routes?.features ?? []) {
        const [x0, y0] = project(...firstCoord(f.geometry.coordinates))
        if (f.properties.kind === 'route') {
          ctx.strokeStyle = String(f.properties.color ?? routeColor(String(f.properties.route)))
          ctx.lineWidth = 1.5; ctx.globalAlpha = 0.8
          ctx.beginPath()
          const coords = f.geometry.coordinates as [number, number][]
          coords.forEach(([lon, lat], i) => {
            const [x, y] = project(lon, lat)
            if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y)
          })
          ctx.stroke(); ctx.globalAlpha = 1
        } else if (f.properties.kind === 'stop' && (x0 + y0) !== 0) {
          ctx.fillStyle = '#5d6a75'
          ctx.beginPath(); ctx.arc(x0, y0, 1.3, 0, Math.PI * 2); ctx.fill()
        }
      }
      // участки маршрутов с риском ТС — тот же riskSegmentsFC, что в MapView:
      // фолбэк не должен врать иначе, чем основная карта
      for (const seg of riskSegmentsFC(routeGeometries(routes), store.vehicles.values(), store.clock).features) {
        const [a, b] = seg.geometry.coordinates
        const [x1, y1] = project(a[0], a[1])
        const [x2, y2] = project(b[0], b[1])
        ctx.strokeStyle = RISK_COLORS[seg.properties.risk]
        ctx.globalAlpha = 0.75; ctx.lineWidth = 4; ctx.lineCap = 'round'
        ctx.beginPath(); ctx.moveTo(x1, y1); ctx.lineTo(x2, y2); ctx.stroke()
        ctx.globalAlpha = 1
      }
      for (const v of store.vehicles.values()) {
        const [x, y] = project(v.lon, v.lat)
        const risk = vehicleRisk(v, store.clock)
        if (selectedRef.current === v.tr_id) {
          ctx.strokeStyle = '#fff'; ctx.lineWidth = 2
          ctx.beginPath(); ctx.arc(x, y, 9, 0, Math.PI * 2); ctx.stroke()
        }
        ctx.fillStyle = RISK_COLORS[risk]
        ctx.beginPath(); ctx.arc(x, y, 4.5, 0, Math.PI * 2); ctx.fill()
      }
    }

    const onClick = (e: MouseEvent) => {
      const r = canvas.getBoundingClientRect()
      const px = e.clientX - r.left, py = e.clientY - r.top
      let best: number | null = null, bestD = 14
      for (const v of store.vehicles.values()) {
        const [x, y] = project(v.lon, v.lat)
        const d = Math.hypot(x - px, y - py)
        if (d < bestD) { bestD = d; best = v.tr_id }
      }
      onSelectRef.current(best)
    }
    canvas.addEventListener('click', onClick)

    draw()
    let raf = 0 // коалесинг: пачка событий за тик реплея = одна перерисовка
    const scheduled = () => {
      if (raf) return
      raf = window.requestAnimationFrame(() => { raf = 0; draw() })
    }
    redrawRef.current = scheduled
    const unsub = store.subscribe(scheduled)
    return () => {
      if (raf) window.cancelAnimationFrame(raf)
      if (redrawRef.current === scheduled) redrawRef.current = () => {}
      window.removeEventListener('resize', resize)
      canvas.removeEventListener('click', onClick)
      unsub()
    }
  }, [store, routes])

  return <div className="map-holder canvas-mode"><canvas ref={ref} /><span className="canvas-badge">canvas-фолбэк (нет WebGL)</span></div>
}

function firstCoord(c: unknown): [number, number] {
  if (Array.isArray(c)) {
    if (typeof c[0] === 'number') return [c[0] as number, c[1] as number]
    return firstCoord(c[0])
  }
  return [0, 0]
}
