import type { Risk, VehicleState } from './types'

// Риск до подключения модели — задокументированное правило-фолбэк
// (ADR 0003, docs/dashboard.md): predicted = cur_dev_s официальной точки;
// порог инцидента 120 с взят из docs/architecture.md §4.1. Когда CatBoost
// (этап 3) подключится, gateway начнёт присылать predicted_delay_s и P(late),
// и функция переключится на них без изменения интерфейса.
// Этап 5, live-лента: gateway шлёт классификацию risk (в ней уже учтены
// predicted_dev_s И p_late, пороги те же 60/120 + 0.3/0.6, см.
// backend/internal/gateway/incidents.go). Если кадр пришёл с нею — цвет
// берется оттуда; replay-кадры поля risk не несут и живут на правиле-фолбэке.
export const YELLOW_THRESHOLD_S = 60
export const RED_THRESHOLD_S = 120
export const STALE_AFTER_S = 180

export function vehicleRisk(v: VehicleState, clockTs: number): Risk {
  if (clockTs - v.ts > STALE_AFTER_S) return 'stale'
  const f = v.lastFrame
  if (f?.risk) return f.risk
  const dev = f?.cur_dev_s
  if (dev == null) return 'unknown'
  if (dev >= RED_THRESHOLD_S) return 'red'
  if (dev >= YELLOW_THRESHOLD_S) return 'yellow'
  return 'green'
}

export const RISK_COLORS: Record<Risk, string> = {
  green: '#2ecc71',
  yellow: '#f1c40f',
  red: '#e74c3c',
  stale: '#7f8c8d',
  unknown: '#5dade2',
}
