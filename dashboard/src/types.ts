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
  source: string
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
