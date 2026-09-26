// Адаптер wire-формата gateway → контракт потока (docs/dashboard.md, этап 5).
//
// Gateway пишет в /ws/stream события envelope {type, at, data} с типами
// `vehicle_update | incident | metrics` (backend/internal/gateway/hub.go),
// а дашборд consumed контракт `meta | vehicle | frame | incident | model`
// (src/types.ts). Gateway переписывать под контракт нельзя — он покрыт
// Go-тестами и стабилен; поэтому перевод делает клиент.
//
// Решения по маппингу (детали — в docs/dashboard.md, раздел «Live-адаптер»):
//  - `at` — time.Time из Go, RFC3339 с НАСТОЯЩИМ смещением зоны (+03:00 или
//    «Z» только когда процесс живёт в UTC). Date.parse даёт истинный unix
//    epoch — та же ось, что у replay-генератора (dt.replace(tzinfo=MSK)
//    .timestamp()). Никаких ручных сдвигов: ловушка «Go дописывает Z» здесь
//    не грозит, на ленте сериализуется time.Time, а не наивная строка.
//  - frame.cur_dev_s в live = null: подсказка организаторов по определению
//    есть только в офлайн-данных. Риск берётся из классификации gateway
//    (поле risk, учитывает и predicted_dev_s, и p_late) — см. src/risk.ts.
//  - чего физически нет на проводе (values признаков, horizon у инцидента,
//    reason, meta) — не выдумывается: панель кадров показывает только то,
//    что пришло, горизонт инцидента подтягивается из последнего прогноза
//    той же машины, поля без данных остаются null/0 и честно помечены.

import type {
  FrameEvent, IncidentEvent, ModelEvent, Risk, StreamEvent, VehicleEvent,
} from './types'

/** Конверт ленты gateway: один JSON на WebSocket-сообщение. */
interface WireEnvelope {
  type: string
  at: string
  data: unknown
}

interface WirePrediction {
  sample_id?: string
  target_stop_id?: number
  horizon_s?: number
  delta_s?: number
  predicted_dev_s?: number
  p_late?: number
  source?: string
  stale?: boolean
  model_version?: string
  risk?: string
  as_of?: string
}

interface WireVehicle {
  unit_id?: number
  tr_id?: number
  has_tr_id?: boolean
  last_seen?: string | null
  staleness_s?: number
  stale?: boolean
  speed_kmh?: number
  lat?: number
  lon?: number
  location_valid?: boolean
  course_deg?: number
  satellites?: number
  points_in_window?: number
  prediction?: WirePrediction | null
  risk?: string
  open_incident_id?: string
}

interface WireIncident {
  id?: string
  unit_id?: number
  tr_id?: number
  target_stop_id?: number
  status?: string
  risk?: string
  opened_at?: string
  updated_at?: string
  acked_at?: string | null
  predicted_dev_s?: number
  p_late?: number
  stale?: boolean
  source?: string
}

interface WireIncidentData {
  incident?: WireIncident | null
  incidents?: WireIncident[]
  stats?: unknown
}

export interface WireDecodeResult {
  /** события контракта — их несёт в Store источник */
  events: StreamEvent[]
  /** инциденты, которые gateway уже считает подтверждёнными */
  acked: string[]
  /** инциденты, закрытые на gateway (панель их не убирает — см. docs) */
  resolved: string[]
}

const WIRE_RISKS: Record<string, Risk> = {
  green: 'green',
  yellow: 'yellow',
  red: 'red',
}

function wireTime(at: string | null | undefined, fallback: number): number {
  // секунды той же оси, что и replay (epoch стенных часов); Date.parse
  // разбирает RFC3339 с зоной корректно и для «+03:00», и для «Z».
  if (!at) return fallback
  const ms = Date.parse(at)
  return Number.isNaN(ms) ? fallback : ms / 1000
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

/**
 * WireAdapter — состояние на соединение: помнит горизонт последнего прогноза
 * по машине (у карточки инцидента своего горизонта нет) и какую версию модели
 * уже показал в панели «Модель», чтобы не слать model-событие каждый тик.
 */
export class WireAdapter {
  private horizons = new Map<number, number>()
  private modelKey = ''

  /** Разбирает одно WebSocket-сообщение (может содержать несколько JSON-строк). */
  decodeMessage(raw: string, now = Date.now() / 1000): WireDecodeResult {
    const out: WireDecodeResult = { events: [], acked: [], resolved: [] }
    for (const line of String(raw).split('\n')) {
      const s = line.trim()
      if (!s) continue
      let env: WireEnvelope
      try {
        env = JSON.parse(s) as WireEnvelope
      } catch {
        continue // сообщение не нашего формата — поток жив
      }
      this.decodeEnvelope(env, now, out)
    }
    return out
  }

  private decodeEnvelope(env: WireEnvelope, now: number, out: WireDecodeResult): void {
    if (!env || typeof env.type !== 'string') return
    const ts = wireTime(env.at, now)
    switch (env.type) {
      case 'vehicle_update':
        this.onVehicle(env.data, ts, out)
        break
      case 'incident':
        this.onIncident(env.data, ts, out)
        break
      case 'metrics':
        // gateway кладёт в metrics срез снимка: массив таких же конвертов.
        // Рекурсия раскрыто описана в docs/dashboard.md.
        if (Array.isArray(env.data)) {
          for (const inner of env.data) {
            if (isObject(inner)) this.decodeEnvelope(inner as unknown as WireEnvelope, now, out)
          }
        }
        break
      default:
        // неизвестный тип ленты: игнорируем, как и replay с битой строкой
        break
    }
  }

  private onVehicle(data: unknown, ts: number, out: WireDecodeResult): void {
    if (!isObject(data)) return
    // лента носит карточку одной машины, а снимок при подключении —
    // {vehicles: [...], count}: обе формы разворачиваются в один список.
    const cards: WireVehicle[] = Array.isArray(data.vehicles)
      ? data.vehicles as WireVehicle[]
      : [data as WireVehicle]
    for (const card of cards) this.vehicleCard(card, ts, out)
  }

  private vehicleCard(card: WireVehicle, ts: number, out: WireDecodeResult): void {
    const tr = card.tr_id ?? 0
    // Без привязки tr_id машина на проводе бесхозная: Store и карта живут
    // по tr_id — пропускаем, выдумывать номер из unit_id нечем.
    if (!card.has_tr_id || !tr) return
    // Координаты достоверны — иначе метку показывать негде (panel обязан её
    // прятать, см. handlers.go: location_valid), и frame к такой машине
    // Store не прицепит: кадр висял бы на несуществующей точке.
    if (!card.location_valid) return

    // Ось времени точки — last_seen (в накопителе это время ПРИЁМА пакета,
    // т.е. живые стенные часы gateway), с честным запасом до `at` сообщения.
    const vts = wireTime(card.last_seen, ts)
    const vehicle: VehicleEvent = {
      type: 'vehicle',
      ts: vts,
      tr_id: tr,
      lon: card.lon ?? 0,
      lat: card.lat ?? 0,
      speed: card.speed_kmh ?? 0,
      heading: card.course_deg ?? 0,
    }
    out.events.push(vehicle)

    const p = card.prediction
    if (!p || !p.sample_id) return
    this.horizons.set(tr, p.horizon_s ?? 0)
    const risk = WIRE_RISKS[card.risk ?? p.risk ?? ''] ?? null
    const frame: FrameEvent = {
      type: 'frame',
      // кадр привязан к моменту прогноза, не к моменту рассылки
      ts: wireTime(p.as_of, vts),
      sample_id: p.sample_id,
      tr_id: tr,
      target_stop_id: p.target_stop_id ?? 0,
      horizon_s: p.horizon_s ?? 0,
      // ambiguous на проводе не сериализуется; кадр live — не ничья
      ambiguous: false,
      // подсказки организаторов в онлайне нет — риска это не ломает:
      // классификация gateway идёт полем risk (см. src/risk.ts)
      cur_dev_s: null,
      official: false,
      values: {
        predicted_dev_s: p.predicted_dev_s ?? 0,
        delta_s: p.delta_s ?? 0,
        p_late: p.p_late ?? 0,
        horizon_s: p.horizon_s ?? 0,
        speed_current: card.speed_kmh ?? 0,
        staleness_s: card.staleness_s ?? 0,
        points_in_window: card.points_in_window ?? 0,
      },
      risk: risk ?? undefined,
      p_late: p.p_late,
    }
    out.events.push(frame)
    this.modelFromPrediction(p, vts, out)
  }

  /**
   * Панель «Модель»: на wire нет ни MAE, ни trained_at — есть version из
   * source/model_version каждого прогноза. Событие уходит ОДИН раз на
   * изменение (источник, версия) — не поллинг и не спам на каждый тик.
   */
  private modelFromPrediction(p: WirePrediction, ts: number, out: WireDecodeResult): void {
    const source = p.source ?? 'unknown'
    const mv = p.model_version ?? ''
    const key = `${source}|${mv}`
    if (key === this.modelKey) return
    this.modelKey = key
    const model: ModelEvent = {
      type: 'model',
      ts,
      version: source === 'ml' && mv ? `модель ${mv}` : `источник: ${source}`,
      model_version: mv || source,
      trained_at: null,
      mae_validate_s: null,
      mae_test_s: null,
      note: 'Live-лента gateway: версия и источник прогноза из карточек; MAE модели лентой не передаётся.',
    }
    out.events.push(model)
  }

  private onIncident(data: unknown, ts: number, out: WireDecodeResult): void {
    if (!isObject(data)) return
    const d = data as WireIncidentData
    const list: WireIncident[] = []
    if (d.incident) list.push(d.incident)
    if (Array.isArray(d.incidents)) list.push(...d.incidents)
    for (const inc of list) this.incidentCard(inc, ts, out)
  }

  private incidentCard(inc: WireIncident, ts: number, out: WireDecodeResult): void {
    if (!inc.id) return
    // закрытый инцидент лентой — в Store нет события «закрыть»; карточка
    // остаётся до ack оператором (совпадает с поведением replay-демо)
    if (inc.status === 'resolved') {
      out.resolved.push(inc.id)
      return
    }
    const tr = inc.tr_id ?? 0
    if (!tr) return
    const delay = inc.predicted_dev_s ?? 0
    const pLate = inc.p_late ?? 0
    const event: IncidentEvent = {
      type: 'incident',
      ts: wireTime(inc.updated_at ?? inc.opened_at, ts),
      id: inc.id,
      tr_id: tr,
      target_stop_id: inc.target_stop_id ?? 0,
      // у карточки инцидента на проводе горизонта нет: берём горизонт
      // последнего прогноза той же машины (Observe шлёт сначала карточку
      // машины, потом инцидент — порядок в ленте гарантирует свежесть);
      // до первого прогноза по машине — 0, панель покажет «0.0 мин» честно
      horizon_s: this.horizons.get(tr) ?? 0,
      predicted_delay_s: delay,
      // предсказанное отставание — то, на чём стоит инцидент; cur_dev_s в
      // контрактном смысле (подсказка точек) в онлайне отсутствует
      cur_dev_s: delay,
      reason: `риск ${inc.risk ?? '?'}, прогноз ${Math.round(delay)} с, p_late ${pLate.toFixed(2)}`,
      source: `gateway:${inc.source ?? '?'}`,
    }
    out.events.push(event)
    if (inc.status === 'acked') out.acked.push(inc.id)
  }
}
