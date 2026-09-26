import type { StreamEvent } from './types'
import { Store } from './store'
import { WireAdapter } from './wire'

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
 * WsSource — живой поток gateway (этап 4/5): JSON-конверты
 * {type: vehicle_update|incident|metrics, at, data} по /ws/stream.
 * WireAdapter переводит их в события контракта — Store и панели не знают
 * о различии форматов. Реконнект с экспонениальным backoff, как положено
 * по §4.7. Класс не трогает window/document: тот же код исполняется в
 * браузере и в node-проверке (dashboard/scripts/ws-smoke.mjs).
 */
export class WsSource implements DataSource {
  readonly mode = 'ws'
  private ws: WsLike | null = null
  private closed = false
  private retry = 0
  private timer: ReturnType<typeof setTimeout> | null = null

  private store: Store
  private url: string
  private wire = new WireAdapter()
  private status: (s: 'open' | 'closed' | 'retry', detail?: string) => void

  constructor(
    store: Store,
    url: string,
    onStatus: (s: 'open' | 'closed' | 'retry', detail?: string) => void = () => {},
  ) {
    this.store = store
    this.url = url
    this.status = onStatus
  }

  async start(): Promise<void> {
    this.closed = false
    this.open()
  }

  stop(): void {
    this.closed = true
    if (this.timer != null) clearTimeout(this.timer)
    this.timer = null
    this.ws?.close()
    this.ws = null
  }

  private open(): void {
    if (this.closed) return
    const Ctor = globalThis.WebSocket
    if (!Ctor) {
      this.status('closed', 'нет WebSocket в окружении')
      return
    }
    const ws = new Ctor(this.url) as unknown as WsLike
    this.ws = ws
    ws.onopen = () => {
      this.retry = 0
      this.status('open')
    }
    ws.onmessage = (msg) => {
      const { events, acked } = this.wire.decodeMessage(String(msg.data))
      for (const ev of events) this.store.apply(ev)
      // gateway уже подтверждённый инцидент: ack без сброса локального состояния
      // (Store.apply повторный id игнорирует, ack идемпотентен)
      for (const id of acked) this.store.ack(id)
    }
    ws.onerror = () => {
      // перед onclose: подробности ошибки в браузере не отдаются, журнал — в статусе
    }
    ws.onclose = (ev?: { code?: number; reason?: string }) => {
      if (this.closed) return
      const delay = Math.min(30000, 500 * 2 ** this.retry++)
      this.status('retry', `через ${delay} мс (попытка ${this.retry + 1}, код ${ev?.code ?? '?'})`)
      this.timer = setTimeout(() => { this.timer = null; this.open() }, delay)
    }
  }
}

// Минимальный контракт WebSocket-объекта: браузерный и node-ный (undici)
// WebSocket подходят, типами обязывает сам окружение, здесь — только поле
// обработчиков, которые использует класс.
interface WsLike {
  onopen: (() => void) | null
  onmessage: ((msg: { data: unknown }) => void) | null
  onerror: (() => void) | null
  onclose: ((ev?: { code?: number; reason?: string }) => void) | null
  close(): void
}

export async function detectSource(store: Store): Promise<DataSource> {
  const params = new URLSearchParams(window.location.search)
  const wsUrl = params.get('ws')
  if (wsUrl) return new WsSource(store, wsUrl)
  // Live — режим по умолчанию: демо показывает себя как на площадке, а не
  // как прокрутку записи. URL считается от текущего хоста: в dev vite
  // проксирует /ws на gateway :8080, в проде сам gateway отдаёт фронт.
  // Replay остаётся явным: ?replay=1 или свой ?stream=…ndjson.
  if (params.get('replay') != null || params.get('stream') != null) {
    return new ReplaySource(store, params.get('stream') ?? '/demo/stream.ndjson')
  }
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return new WsSource(store, `${proto}//${window.location.host}/ws/stream`)
}
