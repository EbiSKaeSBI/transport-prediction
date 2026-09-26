// Контракт потока дашборда (docs/dashboard.md). Одно и то же событие приходит
// из replay-файла demo/stream.ndjson и по WebSocket от gateway (этап 4):
// клиент не различает источники после разбора.

export interface VehicleEvent {
  type: 'vehicle'
  ts: number
  tr_id: number
  lon: number
  lat: number
  speed: number
  heading: number
}

export interface FrameEvent {
  type: 'frame'
  ts: number
  sample_id: string
  tr_id: number
  target_stop_id: number
  horizon_s: number
  ambiguous: boolean
  /** Подсказка организаторов; есть только у официальных точек. */
  cur_dev_s: number | null
  official: boolean
  values: Record<string, number>
  /**
   * Классификация риска от gateway (wire-событие vehicle_update, этап 5):
   * учтена и предсказанная задержка, и p_late. В replay-потоке поля нет —
   * риск остаётся на правиле-фолбэке cur_dev_s (ADR 0003).
   */
  risk?: Risk
  /** Вероятность опоздания от модели; приходит только live-лентой. */
  p_late?: number
  /**
   * Источник оценки: 'ml' или 'baseline'. Число в values.predicted_dev_s
   * при baseline — это измеренное отставание, а не прогноз, и панель
   * обязана называть его accordingly (см. isFallback в panels.tsx).
   */
  source?: string
  /** Версия модели, выдавшая кадр; показывается рядом с источником. */
  model_version?: string
  /** Причина от модели; у fallback-кадров её нет. */
  reason?: string
  /**
   * Признаки контракта, которых в кадре не оказалось: модель получила null и
   * импутировала их. Показывать обязательно — иначе прогноз по 7 пустым
   * признакам выглядит так же уверенно, как по полному вектору.
   */
  missing?: string[]
}

export interface IncidentEvent {
  type: 'incident'
  ts: number
  id: string
  tr_id: number
  target_stop_id: number
  horizon_s: number
  predicted_delay_s: number
  cur_dev_s: number
  reason: string
  /** Вероятность опоздания из карточки gateway (решето риска на карте). */
  p_late?: number
  /** Плановая остановка перед целью — участок, на котором копится опоздание. */
  prev_stop_id?: number
  source: string
  /**
   * Инцидент закрыт на gateway: риск ушёл из красной зоны либо машина уехала
   * на следующую цель. Панель убирает карточку — иначе одна опоздывающая ТС
   * оставляет в рельсе по карточке на каждую пройденную остановку.
   */
  resolved?: boolean
}

export interface ModelEvent {
  type: 'model'
  ts: number
  version: string
  model_version: string
  trained_at: string | null
  mae_validate_s: number | null
  mae_test_s: number | null
  note?: string
  features?: string[]
  /** Паспорт P(late)-классификатора (v4); null — голова не подключена. */
  late?: LatePassport | null
}

export interface LatePassport {
  version: string
  threshold_s: number
  auc_holdout: number | null
  positive_rate_holdout: number | null
}

export interface MetaEvent {
  type: 'meta'
  ts: number
  day: string
  window: [number, number]
  vehicles: number
  frames: number
  incidents: number
  routes: number
}

export type StreamEvent =
  | VehicleEvent | FrameEvent | IncidentEvent | ModelEvent | MetaEvent

export type Risk = 'green' | 'yellow' | 'red' | 'stale' | 'unknown'

export interface VehicleState {
  tr_id: number
  lon: number
  lat: number
  speed: number
  heading: number
  ts: number
  lastFrame: FrameEvent | null
}
