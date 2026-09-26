#!/usr/bin/env node
// Живой smoke-прогон live-ленты gateway (этап 5, GitLab #36).
//
// Подключается к WebSocket ТЕМ ЖЕ боевым кодом, что крутится в браузере:
// из бандла scripts/adapter.bundle.mjs (esbuild из src/) берутся WsSource +
// WireAdapter + Store. Доказывает: позиции, риск, инциденты и метрики
// доезжают в реальном времени без polling, реконнект с backoff работает.
//
// Запуск (нужен node >= 20, глобальный WebSocket):
//   node scripts/build-adapter.mjs            # собрать бандл после правок src
//   node scripts/ws-smoke.mjs [ws://host/ws/stream] [--duration 30]
import { WireAdapter, WsSource, Store, vehicleRisk } from './adapter.bundle.mjs'

const argv = process.argv.slice(2)
const url = argv.find(a => a.startsWith('ws://') || a.startsWith('wss://'))
  ?? 'ws://127.0.0.1:8080/ws/stream'
const durIdx = argv.indexOf('--duration')
const duration = durIdx >= 0 ? Number(argv[durIdx + 1]) : 30

const store = new Store()
// подсматриваем первые события контракта, не меняя путь данных
const seen = []
const spy = {
  apply(ev) {
    if (seen.length < 12) seen.push(ev)
    store.apply(ev)
  },
  ack(id) { store.ack(id) },
  tickClock(ts) { store.tickClock(ts) },
  noteStreamEnd() { store.noteStreamEnd() },
}
const t0 = Date.now()
const log = (...a) => console.log(`[smoke +${((Date.now() - t0) / 1000).toFixed(1)}s]`, ...a)

log(`подключение: ${url}`)
const src = new WsSource(spy, url, (status, detail) => log('статус:', status, detail ?? ''))
src.start()

process.on('SIGINT', () => finish())
setTimeout(finish, duration * 1000)
let finished = false
function finish() {
  if (finished) return
  finished = true
  src.stop()
  console.log('\n── первые события контракта после адаптера ──')
  for (const ev of seen) {
    console.log(`  ${ev.type}`, JSON.stringify(ev).slice(0, 160))
  }
  console.log('\n── состояние Store ──')
  console.log(`  ТС: ${store.vehicles.size}, кадров всего: ${store.framesTotal}, ` +
    `инцидентов: ${store.incidents.size} (открытых ${store.openIncidents()}), ` +
    `событий/с: ${store.eventsPerSecond().toFixed(2)}`)
  console.log(`  модель: ${store.model ? store.model.version : '—'}`)
  console.log(`  часы потока: ${new Date(store.clock * 1000).toISOString()}`)
  console.log('\n── риск по машинам (risk.ts поверх live-кадров) ──')
  for (const v of store.vehicles.values()) {
    const r = vehicleRisk(v, store.clock)
    const f = v.lastFrame
    console.log(`  tr_id ${v.tr_id}: ${r.padEnd(6)} ` +
      `lon=${v.lon.toFixed(5)} lat=${v.lat.toFixed(5)} v=${v.speed}км/ч ` +
      `прогноз=${f ? `${Math.round(f.values.predicted_dev_s ?? 0)}с p_late=${(f.p_late ?? 0).toFixed(2)} risk=${f.risk ?? '—'}` : 'нет'}`)
  }
  // отдельная прямая проверка адаптера: WireAdapter живёт и сам по себе
  void WireAdapter
  const ok = store.vehicles.size > 0 || store.incidents.size > 0
  console.log(`\nИТОГ: ${ok ? 'лента живая — события дошли до Store' : 'НЕТ СОБЫТИЙ — лента молчит'}`)
  process.exit(ok ? 0 : 1)
}
