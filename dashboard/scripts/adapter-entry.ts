// Точка сборки esbuild-бандла для node-проверок live-адаптера.
// Браузерное приложение эти файлы не импортирует — только скрипты:
//   scripts/ws-smoke.mjs   (живой WS-прогон через тот же WsSource)
//   scripts/adapter.test.mjs (unit-тесты адаптера, node:test)
export { WireAdapter } from '../src/wire'
export { WsSource } from '../src/source'
export { Store } from '../src/store'
export { vehicleRisk, RISK_COLORS } from '../src/risk'
