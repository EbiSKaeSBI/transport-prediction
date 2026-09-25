import type { StreamEvent } from './types'
import { Store } from './store'

export interface DataSource {
  start(): Promise<void>
  stop(): void
  /** только для replay: множитель скорости потока */
  setRate?(rate: number): void
  setPaused?(paused: boolean): void
  mode: 'replay' | 'ws'
}

async function parseNdjson(url: string): Promise<StreamEvent[]> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`)
  const text = await res.text()
  const events: StreamEvent[] = []
  for (const line of text.split('\n')) {
    const s = line.trim()
    if (!s) continue
    try {
      events.push(JSON.parse(s) as StreamEvent)
    } catch {
      // битая строка не должна ронять демо
    }
  }
  return events
}

/**
 * ReplaySource проигрывает demo/stream.ndjson ускоренно: один «аппаратный»
 * тик выдаёт порцию событий до текущего момента потока. События идут в тот
 * же Store, что и живые из WebSocket, — карта и панели не знают о источнике.
 */
export class ReplaySource implements DataSource {
  readonly mode = 'replay'
  private events: StreamEvent[] = []
  private idx = 0
  private wall0 = 0
  private stream0 = 0
  private rate = 60
  private paused = false
  private timer: number | null = null
  private stopped = false

  private store: Store
  private url: string

  constructor(store: Store, url = '/demo/stream.ndjson') {
    this.store = store
    this.url = url
  }

  async start(): Promise<void> {
    this.stopped = false
    const events = await parseNdjson(this.url)
    if (this.stopped) return // stop() пришёл, пока шёл fetch — не взводим таймер
    this.events = events
    this.events.sort((a, b) => a.ts - b.ts)
    this.stream0 = this.events[0]?.ts ?? 0
    this.idx = 0
    this.wall0 = performance.now()
    this.timer = window.setInterval(() => this.tick(), 50)
    this.tick()
  }

  stop(): void {
    this.stopped = true
    if (this.timer != null) window.clearInterval(this.timer)
    this.timer = null
  }

  setRate(rate: number): void {
    const anchorWall = performance.now()
    const advance = (anchorWall - this.wall0) / 1000 * this.rate
    this.wall0 = anchorWall - advance * 1000 / rate
    this.rate = rate
  }

  setPaused(paused: boolean): void {
    if (paused === this.paused) return
    this.paused = paused
    if (!paused) {
      // возобновление: переносим якорь, пропущенное стенд-время не догоняем
      this.wall0 = performance.now() - (this.lastStreamTs() - this.stream0) * 1000 / this.rate
    }
  }

  private lastStreamTs(): number {
    return this.idx > 0 ? this.events[this.idx - 1].ts : this.stream0
  }

  private tick(): void {
    if (this.paused) return
    const targetTs = this.stream0 + (performance.now() - this.wall0) / 1000 * this.rate
    let applied = 0
    while (this.idx < this.events.length && this.events[this.idx].ts <= targetTs) {
      this.store.apply(this.events[this.idx])
      this.idx++
      if (++applied >= 2000) break // защита от длинного синхронного фриза
    }
    if (this.idx >= this.events.length) {
      // поток доигран: фиксируем часы (иначе ТС «постареют» в серое навсегда)
      this.stop()
      this.store.noteStreamEnd()
      return
    }
    this.store.tickClock(targetTs)
  }
}

/**
 * WsSource — живой поток gateway (этап 4): сообщения NDJSON по /ws/stream.
 * Реконнект с экспоненциальным backoff, как и положено по §4.7.
 */
export class WsSource implements DataSource {
  readonly mode = 'ws'
  private ws: WebSocket | null = null
  private closed = false
  private retry = 0

  private store: Store
  private url: string

  constructor(store: Store, url: string) {
    this.store = store
    this.url = url
  }

  async start(): Promise<void> {
    this.closed = false
    this.open()
  }

  stop(): void {
    this.closed = true
    this.ws?.close()
    this.ws = null
  }

  private open(): void {
    if (this.closed) return
    this.ws = new WebSocket(this.url)
    this.ws.onopen = () => { this.retry = 0 }
    this.ws.onmessage = (msg: MessageEvent<string>) => {
      for (const line of String(msg.data).split('\n')) {
        const s = line.trim()
        if (!s) continue
        try {
          this.store.apply(JSON.parse(s) as StreamEvent)
        } catch {
          // сообщение не нашего контракта — игнорируем, поток жив
        }
      }
    }
    this.ws.onclose = () => {
      if (this.closed) return
      const delay = Math.min(30000, 500 * 2 ** this.retry++)
      window.setTimeout(() => this.open(), delay)
    }
  }
}

export async function detectSource(store: Store): Promise<DataSource> {
  const params = new URLSearchParams(window.location.search)
  const wsUrl = params.get('ws')
  if (wsUrl) return new WsSource(store, wsUrl)
  return new ReplaySource(store, params.get('stream') ?? '/demo/stream.ndjson')
}
