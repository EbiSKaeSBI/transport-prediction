import { useEffect, useRef } from 'react'
import type { Store } from './store'
import type { RouteFC } from './geo'
import { routeColor, routeGeometries, routesBounds, mergeBounds, pointsBounds, graticuleFC, niceStep } from './geo'
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

    // Рамка — по плану и по позициям ТС одновременно, один раз. План без машин
    // не берём: демо-фид кладёт телеметрию в 8 км от плана, и рамка только по
    // нему оставляет все точки вне холста (тот же грабли, что в MapView).
    let bounds: { minX: number; minY: number; maxX: number; maxY: number } | null = null
    const ensureBounds = () => {
      if (bounds) return
      const plan = routesBounds(routes)
      const pts = pointsBounds(store.vehicles.values())
      if (plan && !pts) return
      const merged = mergeBounds(plan, pts)
      if (!merged) return
      const padX = (merged[1][0] - merged[0][0]) * 0.05
      const padY = (merged[1][1] - merged[0][1]) * 0.05
      bounds = {
        minX: merged[0][0] - padX, maxX: merged[1][0] + padX,
        minY: merged[0][1] - padY, maxY: merged[1][1] + padY,
      }
    }

    const project = (lon: number, lat: number): [number, number] => {
      if (!bounds) return [w / 2, h / 2]
      const x = (lon - bounds.minX) / (bounds.maxX - bounds.minX) * w
      const y = (1 - (lat - bounds.minY) / (bounds.maxY - bounds.minY)) * h
      return [x, y]
    }

    const draw = () => {
      ensureBounds()
      ctx.fillStyle = '#101418'
      ctx.fillRect(0, 0, w, h)
      if (bounds) drawGrid(ctx, bounds, project, w, h)
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
        // стрелка направления и бирка — та же семантика, что в MapLibre-версии
        // (heading 0 = север, поворот по часовой; canvas y-ось вниз — совпадает)
        ctx.save()
        ctx.translate(x, y)
        ctx.rotate(((Number.isFinite(v.heading) ? v.heading : 0) * Math.PI) / 180)
        ctx.fillStyle = RISK_COLORS[risk]
        ctx.beginPath(); ctx.moveTo(0, -13); ctx.lineTo(4.5, -5); ctx.lineTo(-4.5, -5); ctx.closePath(); ctx.fill()
        ctx.restore()
        ctx.fillStyle = RISK_COLORS[risk]
        ctx.beginPath(); ctx.arc(x, y, 4.5, 0, Math.PI * 2); ctx.fill()
        ctx.fillStyle = '#cfd8e3'
        ctx.font = '10px ui-monospace, monospace'
        ctx.fillText(`ТС ${v.tr_id} · ${Math.round(v.speed)} км/ч`, x + 9, y - 4)
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

type Bounds = { minX: number; minY: number; maxX: number; maxY: number }
type Project = (lon: number, lat: number) => [number, number]

/**
 * Сетка координат с подписями — офлайн-аналог подложки. В canvas подписи
 * бесплатны (у maplibre для текста нужен сетевой glyphs-сервер), поэтому
 * фолбэк даёт даже больше контекста, чем основная карта.
 */
function drawGrid(
  ctx: CanvasRenderingContext2D,
  b: Bounds,
  project: Project,
  w: number,
  h: number,
) {
  const midLat = (b.minY + b.maxY) / 2
  const span = Math.max(b.maxX - b.minX, (b.maxY - b.minY) * Math.cos((midLat * Math.PI) / 180))
  const step = niceStep(span)
  const digits = step < 0.01 ? 3 : step < 0.1 ? 2 : step < 1 ? 1 : 0
  const grid = graticuleFC([[b.minX, b.minY], [b.maxX, b.maxY]], step)
  ctx.strokeStyle = '#1b242c'
  ctx.lineWidth = 1
  ctx.beginPath()
  for (const f of grid.features) {
    const c = f.geometry.coordinates as [number, number][]
    c.forEach(([lon, lat], i) => {
      const [x, y] = project(lon, lat)
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y)
    })
  }
  ctx.stroke()
  ctx.fillStyle = '#3c4a55'
  ctx.font = '10px ui-monospace, monospace'
  for (const f of grid.features) {
    const c = f.geometry.coordinates as [number, number][]
    const vertical = c[0][0] === c[1][0]
    const [x, y] = project(...c[0])
    const label = (vertical ? c[0][0] : c[0][1]).toFixed(digits) + '°'
    if (x < 0 || x > w || y < 0 || y > h) continue
    ctx.fillText(label, vertical ? x + 3 : 4, vertical ? 11 : y - 3)
  }
}
