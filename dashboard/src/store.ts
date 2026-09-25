import type {
  FrameEvent, IncidentEvent, MetaEvent, ModelEvent, StreamEvent, VehicleState,
} from './types'
import { vehicleRisk } from './risk'

export interface IncidentState extends IncidentEvent {
  acked: boolean
}

interface RateBucket { t: number; n: number }

export class Store {
  vehicles = new Map<number, VehicleState>()
  incidents = new Map<string, IncidentState>()
  model: ModelEvent | null = null
  meta: MetaEvent | null = null
  clock = 0
  framesTotal = 0
  /** реплей доиграл до конца: часы не гоним, показываем бейдж */
  streamEnded = false
  /** скользящая частота событий в секунду (по 1-секундным бакетам) */
  rate: RateBucket[] = []

  private rev = 0
  private listeners = new Set<() => void>()

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }
  getSnapshot = (): number => this.rev

  apply(ev: StreamEvent): void {
    switch (ev.type) {
      case 'vehicle': {
        const prev = this.vehicles.get(ev.tr_id)
        if (!prev || ev.ts >= prev.ts) {
          this.vehicles.set(ev.tr_id, {
            tr_id: ev.tr_id, lon: ev.lon, lat: ev.lat,
            speed: ev.speed, heading: ev.heading, ts: ev.ts,
            lastFrame: prev?.lastFrame ?? null,
          })
        }
        break
      }
      case 'frame': {
        const f = ev as FrameEvent
        const v = this.vehicles.get(f.tr_id)
        if (v && (!v.lastFrame || f.ts >= v.lastFrame.ts)) {
          this.framesTotal++ // только реально применённые кадры
          this.vehicles.set(v.tr_id, { ...v, lastFrame: f })
        }
        break
      }
      case 'incident': {
        // at-least-once доставка (live WS): повтор того же id не должен
        // сбрасывать ack уже показанного инцидента
        if (!this.incidents.has(ev.id)) this.incidents.set(ev.id, { ...ev, acked: false })
        break
      }
      case 'model': { this.model = ev as ModelEvent; break }
      case 'meta': { this.meta = ev as MetaEvent; break }
    }
    if (ev.ts > this.clock) this.clock = ev.ts
    let sec = Math.floor(ev.ts)
    const last = this.rate[this.rate.length - 1]
    if (last) {
      if (sec < last.t) sec = last.t // неупорядоченные live-события не ломают окно
      if (sec === last.t) last.n++
    }
    if (!last || sec !== last.t) {
      this.rate.push({ t: sec, n: 1 })
      if (this.rate.length > 120) this.rate.shift()
    }
    this.rev++
    for (const fn of this.listeners) fn()
  }

  /** часы потока могут опережать последнее событие: stale-детекция живёт тут */
  tickClock(ts: number): void {
    if (this.streamEnded || ts <= this.clock) return
    this.clock = ts
    this.rev++
    for (const fn of this.listeners) fn()
  }

  noteStreamEnd(): void {
    if (this.streamEnded) return
    this.streamEnded = true
    this.rev++
    for (const fn of this.listeners) fn()
  }

  ack(id: string): void {
    const inc = this.incidents.get(id)
    if (!inc) return
    this.incidents.set(id, { ...inc, acked: true })
    this.rev++
    for (const fn of this.listeners) fn()
  }

  eventsPerSecond(): number {
    const n = this.rate.length
    if (!n) return 0
    const window = this.rate.slice(-10)
    const total = window.reduce((s, b) => s + b.n, 0)
    // делим на фактическую длительность окна, а не на число непустых секунд:
    // на разреженном потоке (×1) честные доли события в секунду
    const span = window[window.length - 1].t - window[0].t + 1
    return total / span
  }

  activeVehicles(): number {
    let k = 0
    for (const v of this.vehicles.values()) {
      if (vehicleRisk(v, this.clock) !== 'stale') k++
    }
    return k
  }

  openIncidents(): number {
    let k = 0
    for (const i of this.incidents.values()) if (!i.acked) k++
    return k
  }
}
